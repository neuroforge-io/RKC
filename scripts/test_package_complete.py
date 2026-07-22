#!/usr/bin/env python3
"""Unit tests for deterministic, fail-closed complete-package assembly."""
from __future__ import annotations

import importlib.util
import io
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path, PurePosixPath
from unittest import mock


MODULE_NAME = "rkc_package_complete"
SPEC = importlib.util.spec_from_file_location(
    MODULE_NAME, Path(__file__).with_name("package-complete.py")
)
assert SPEC and SPEC.loader
PACKAGE = importlib.util.module_from_spec(SPEC)
sys.modules[MODULE_NAME] = PACKAGE
SPEC.loader.exec_module(PACKAGE)


def tree_entry(mode: str, path: str, object_type: str = "blob") -> bytes:
    return f"{mode} {object_type} {'0' * 40}\t{path}".encode() + b"\0"


class CompletePackageTests(unittest.TestCase):
    def setUp(self) -> None:
        self.original_root = PACKAGE.ROOT
        self.original_dist = PACKAGE.DIST
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-package-test-")
        PACKAGE.ROOT = Path(self.temporary.name)
        PACKAGE.DIST = PACKAGE.ROOT / "dist"
        PACKAGE.DIST.mkdir(mode=0o755)
        self.identity = PACKAGE.SourceIdentity("a" * 40, "b" * 40, "7")

    def tearDown(self) -> None:
        PACKAGE.ROOT = self.original_root
        PACKAGE.DIST = self.original_dist
        self.temporary.cleanup()

    def write(
        self, relative: str, content: bytes = b"fixture\n", mode: int = 0o644
    ) -> Path:
        path = PACKAGE.ROOT / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content)
        os.chmod(path, mode)
        return path

    def write_module_lock(self, root: Path) -> Path:
        license_relative = "LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE"
        license_path = root / license_relative
        lock = root.joinpath(*PACKAGE.GO_MODULE_LOCK.parts)
        lock.parent.mkdir(parents=True, exist_ok=True)
        lock.write_text(
            json.dumps(
                {
                    "schema_version": "1.0",
                    "go": {"directive": "1.25.0", "toolchain": "go1.26.5"},
                    "root_requirements": [
                        {"path": "modernc.org/sqlite", "version": "v1.54.0"}
                    ],
                    "modules": [
                        {
                            "path": "modernc.org/sqlite",
                            "version": "v1.54.0",
                            "direct": True,
                            "module_sum": "h1:module",
                            "go_mod_sum": "h1:mod",
                            "source_url": "https://modernc.org/sqlite",
                            "license_spdx": "BSD-3-Clause",
                            "licenses": [
                                {
                                    "source_path": "LICENSE",
                                    "path": license_relative,
                                    "sha256": PACKAGE.sha256(license_path),
                                }
                            ],
                            "notice_path": "THIRD_PARTY_NOTICES.md",
                        }
                    ],
                },
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
        return lock

    def write_release_evidence(
        self, root: Path, identity: object | None = None
    ) -> Path:
        identity = self.identity if identity is None else identity
        logs = root / "logs"
        logs.mkdir(parents=True, exist_ok=True)
        rows: list[str] = []
        steps: list[dict[str, object]] = []
        for index, name in enumerate(PACKAGE.RELEASE_STEPS):
            log = logs / f"{name}.log"
            log.write_text(f"{name} passed\n", encoding="utf-8")
            rows.append(f"{name}\tpassed\t{index}")
            steps.append(
                {
                    "name": name,
                    "status": "passed",
                    "duration_seconds": index,
                    "log_sha256": PACKAGE.sha256(log),
                }
            )
        (root / "steps.tsv").write_text("\n".join(rows) + "\n", encoding="utf-8")
        (root / "summary.json").write_text(
            json.dumps(
                {
                    "schema_version": "2.0",
                    "ok": True,
                    "source": {
                        "commit": identity.commit,
                        "tree": identity.tree,
                        "commit_time_unix": identity.commit_time_unix,
                    },
                    "elapsed_seconds": 99,
                    "steps": steps,
                },
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
        return root

    def write_benchmark_evidence(self, root: Path) -> Path:
        root.mkdir(parents=True, exist_ok=True)
        (root / "result.json").write_text(
            json.dumps(
                {
                    "schema_version": "1.0",
                    "profile": "fixture",
                    "elapsed_seconds": 1.0,
                    "maximum_rss_kib": 1,
                    "coverage": {},
                },
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
        (root / "time.txt").write_text("elapsed fixture\n", encoding="utf-8")
        (root / "scan.stdout").write_text("scan fixture\n", encoding="utf-8")
        return root

    def test_safe_paths_and_prohibited_names(self) -> None:
        self.assertEqual(
            PACKAGE.safe_relative_path("a/b.txt", "fixture"), PurePosixPath("a/b.txt")
        )
        for value in ("", "/abs", "../escape", "a/../b", "a\\b", "a\x01b"):
            with self.subTest(value=value), self.assertRaises(PACKAGE.PackageError):
                PACKAGE.safe_relative_path(value, "fixture")
        self.assertTrue(PACKAGE.prohibited_name(PurePosixPath("MODEL.safetensors")))
        self.assertTrue(PACKAGE.prohibited_name(PurePosixPath("model-00001.bin")))
        self.assertFalse(PACKAGE.prohibited_name(PurePosixPath("model.json")))

    @mock.patch.object(PACKAGE.subprocess, "run")
    def test_tracked_files_parses_modes_and_rejects_commit_tree_hazards(
        self, run: mock.Mock
    ) -> None:
        run.return_value = subprocess.CompletedProcess(
            [],
            0,
            stdout=tree_entry("100755", "scripts/tool.sh")
            + tree_entry("100644", "README.md"),
            stderr=b"",
        )
        files = PACKAGE.tracked_files(self.identity.commit)
        self.assertEqual(
            [str(item.path) for item in files], ["README.md", "scripts/tool.sh"]
        )
        self.assertTrue(files[1].executable)

        cases = (
            (tree_entry("100644", "conflict", "tree"), "unsupported"),
            (tree_entry("120000", "link"), "symlinks"),
            (tree_entry("160000", "module", "commit"), "submodules"),
            (tree_entry("100600", "mode"), "unsupported"),
            (tree_entry("100644", "dist/output"), "generated"),
            (tree_entry("100644", "same") * 2, "duplicate"),
            (b"malformed\0", "unportable"),
            (b"", "no source"),
        )
        for payload, marker in cases:
            with self.subTest(marker=marker):
                run.return_value = subprocess.CompletedProcess(
                    [], 0, stdout=payload, stderr=b""
                )
                with self.assertRaisesRegex(PACKAGE.PackageError, marker):
                    PACKAGE.tracked_files(self.identity.commit)
        run.return_value = subprocess.CompletedProcess(
            [], 7, stdout=b"", stderr=b"failure"
        )
        with self.assertRaisesRegex(PACKAGE.PackageError, "failure"):
            PACKAGE.tracked_files(self.identity.commit)

    @mock.patch.object(PACKAGE.subprocess, "run")
    def test_clean_source_requires_both_git_diff_checks(self, run: mock.Mock) -> None:
        run.side_effect = [
            subprocess.CompletedProcess([], 0),
            subprocess.CompletedProcess([], 0),
        ]
        PACKAGE.require_clean_tracked_source()
        self.assertEqual(run.call_count, 2)
        for status, marker in ((1, "dirty"), (2, "cannot verify")):
            run.reset_mock()
            run.side_effect = None
            run.return_value = subprocess.CompletedProcess([], status)
            with self.assertRaisesRegex(PACKAGE.PackageError, marker):
                PACKAGE.require_clean_tracked_source()

    @mock.patch.object(PACKAGE.subprocess, "run")
    def test_git_text_and_source_identity_fail_closed(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(
            [], 0, stdout="canonical-value\n", stderr=""
        )
        self.assertEqual(PACKAGE.git_text(["fixture"], "fixture"), "canonical-value")

        for result, marker in (
            (
                subprocess.CompletedProcess([], 2, stdout="", stderr="fatal fixture\n"),
                "fatal fixture",
            ),
            (
                subprocess.CompletedProcess([], 2, stdout="", stderr=""),
                "Git query failed",
            ),
            (
                subprocess.CompletedProcess([], 0, stdout="\n", stderr=""),
                "canonical line",
            ),
            (
                subprocess.CompletedProcess([], 0, stdout="one\ntwo\n", stderr=""),
                "canonical line",
            ),
        ):
            with self.subTest(marker=marker):
                run.return_value = result
                with self.assertRaisesRegex(PACKAGE.PackageError, marker):
                    PACKAGE.git_text(["fixture"], "fixture")

        commit, tree = "a" * 40, "b" * 40
        with mock.patch.object(PACKAGE, "git_text", side_effect=[commit, tree, "7"]):
            self.assertEqual(
                PACKAGE.source_identity(),
                PACKAGE.SourceIdentity(commit, tree, "7"),
            )
        for values, marker in (
            (("z" * 40, tree, "7"), "commit"),
            ((commit, "b" * 39, "7"), "tree"),
            ((commit, tree, "-1"), "timestamp"),
        ):
            with self.subTest(marker=marker), mock.patch.object(
                PACKAGE, "git_text", side_effect=values
            ), self.assertRaisesRegex(PACKAGE.PackageError, marker):
                PACKAGE.source_identity()

    def test_copy_commit_blob_materialization_and_integrity_failures(self) -> None:
        object_id = "c" * 40

        def materializer(content: bytes, status: int = 0, stderr: bytes = b""):
            def run(
                _arguments: object, **kwargs: object
            ) -> subprocess.CompletedProcess[bytes]:
                output = kwargs["stdout"]
                output.write(content)
                return subprocess.CompletedProcess([], status, stderr=stderr)

            return run

        for executable in (False, True):
            content = b"immutable tracked content\n"
            item = PACKAGE.TrackedFile(
                PurePosixPath(f"tool-{executable}"), executable, object_id
            )
            target = PACKAGE.ROOT / f"materialized-{executable}"
            with mock.patch.object(
                PACKAGE,
                "git_text",
                side_effect=[str(len(content)), object_id],
            ), mock.patch.object(
                PACKAGE.subprocess,
                "run",
                side_effect=materializer(content),
            ):
                self.assertEqual(PACKAGE.copy_commit_blob(item, target), len(content))
            self.assertEqual(target.read_bytes(), content)
            self.assertEqual(
                target.stat().st_mode & 0o777,
                0o755 if executable else 0o644,
            )

        item = PACKAGE.TrackedFile(PurePosixPath("fixture"), False, object_id)
        with mock.patch.object(
            PACKAGE, "git_text", return_value="not-a-size"
        ), self.assertRaisesRegex(PACKAGE.PackageError, "size is invalid"):
            PACKAGE.copy_commit_blob(item, PACKAGE.ROOT / "invalid-size")
        with mock.patch.object(
            PACKAGE, "git_text", return_value="2"
        ), mock.patch.object(
            PACKAGE, "MAX_ARTIFACT_FILE_BYTES", 1
        ), self.assertRaisesRegex(
            PACKAGE.PackageError, "per-file"
        ):
            PACKAGE.copy_commit_blob(item, PACKAGE.ROOT / "oversized")

        target = PACKAGE.ROOT / "timeout"
        with mock.patch.object(
            PACKAGE, "git_text", return_value="4"
        ), mock.patch.object(
            PACKAGE.subprocess,
            "run",
            side_effect=subprocess.TimeoutExpired(["git"], 60),
        ), self.assertRaisesRegex(
            PACKAGE.PackageError, "cannot materialize"
        ):
            PACKAGE.copy_commit_blob(item, target)
        self.assertFalse(target.exists())

        target = PACKAGE.ROOT / "git-failure"
        with mock.patch.object(
            PACKAGE, "git_text", return_value="7"
        ), mock.patch.object(
            PACKAGE.subprocess,
            "run",
            side_effect=materializer(b"payload", 2, b"object missing"),
        ), self.assertRaisesRegex(
            PACKAGE.PackageError, "object missing"
        ):
            PACKAGE.copy_commit_blob(item, target)
        self.assertFalse(target.exists())

        target = PACKAGE.ROOT / "short-object"
        with mock.patch.object(
            PACKAGE, "git_text", return_value="99"
        ), mock.patch.object(
            PACKAGE.subprocess, "run", side_effect=materializer(b"short")
        ), self.assertRaisesRegex(
            PACKAGE.PackageError, "materialization failed"
        ):
            PACKAGE.copy_commit_blob(item, target)

        content = b"ordinary content"
        target = PACKAGE.ROOT / "wrong-identity"
        with mock.patch.object(
            PACKAGE, "git_text", side_effect=[str(len(content)), "d" * 40]
        ), mock.patch.object(
            PACKAGE.subprocess, "run", side_effect=materializer(content)
        ), self.assertRaisesRegex(
            PACKAGE.PackageError, "identity differs"
        ):
            PACKAGE.copy_commit_blob(item, target)

        for content, marker in ((b"GGUFmodel", "model"), (b"\x7fELFbinary", "native")):
            target = PACKAGE.ROOT / f"prohibited-{marker}"
            with self.subTest(marker=marker), mock.patch.object(
                PACKAGE,
                "git_text",
                side_effect=[str(len(content)), object_id],
            ), mock.patch.object(
                PACKAGE.subprocess,
                "run",
                side_effect=materializer(content),
            ), self.assertRaisesRegex(
                PACKAGE.PackageError, marker
            ):
                PACKAGE.copy_commit_blob(item, target)
            self.assertFalse(target.exists())

    def test_strict_json_demo_and_required_file_boundaries(self) -> None:
        document = self.write("documents/valid.json", b'{"value":1}\n')
        self.assertEqual(PACKAGE.load_strict_json(document, "fixture"), {"value": 1})
        with self.assertRaisesRegex(PACKAGE.PackageError, "is missing"):
            PACKAGE.load_strict_json(PACKAGE.ROOT / "missing.json", "fixture")
        with self.assertRaisesRegex(PACKAGE.PackageError, "regular file"):
            PACKAGE.load_strict_json(PACKAGE.ROOT, "fixture")
        with self.assertRaisesRegex(PACKAGE.PackageError, "exceeds"):
            PACKAGE.load_strict_json(document, "fixture", maximum=1)
        document.write_bytes(b'{"duplicate":1,"duplicate":2}\n')
        with self.assertRaisesRegex(PACKAGE.PackageError, "duplicate JSON key"):
            PACKAGE.load_strict_json(document, "fixture")
        document.write_bytes(b"\xff")
        with self.assertRaisesRegex(PACKAGE.PackageError, "strict UTF-8 JSON"):
            PACKAGE.load_strict_json(document, "fixture")

        output = PACKAGE.ROOT / "documents/output.json"
        PACKAGE.write_json_file(output, {"b": 2, "a": 1})
        self.assertEqual(
            json.loads(output.read_text(encoding="utf-8")), {"a": 1, "b": 2}
        )
        with self.assertRaisesRegex(PACKAGE.PackageError, "duplicate staged"):
            PACKAGE.write_json_file(output, {})

        demo = PACKAGE.ROOT / "demo-fixture"
        demo.mkdir()
        bundle_path = demo / "bundle.json"
        coverage_path = demo / "coverage.json"
        valid_bundle = {
            "snapshot": {
                "id": "snapshot-one",
                # Canonical GitInfo JSON omits its zero-valued clean marker.
                "git": {"commit": self.identity.commit},
            }
        }

        def write_demo(bundle: object, coverage: object) -> None:
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            coverage_path.write_text(json.dumps(coverage), encoding="utf-8")

        write_demo(valid_bundle, {"snapshot_id": "snapshot-one"})
        PACKAGE.validate_demo_outputs(demo, self.identity)
        explicit_clean_bundle = {
            "snapshot": {
                "id": "snapshot-one",
                "git": {"commit": self.identity.commit, "dirty": False},
            }
        }
        write_demo(explicit_clean_bundle, {"snapshot_id": "snapshot-one"})
        PACKAGE.validate_demo_outputs(demo, self.identity)
        for bundle, coverage, marker in (
            ([], {}, "JSON objects"),
            ({}, {}, "snapshot is missing"),
            ({"snapshot": {"id": "one"}}, {"snapshot_id": "one"}, "immutable source"),
            (
                {
                    "snapshot": {
                        "id": "snapshot-one",
                        "git": {"commit": self.identity.commit, "dirty": True},
                    }
                },
                {"snapshot_id": "snapshot-one"},
                "immutable source",
            ),
            (
                {
                    "snapshot": {
                        "id": "snapshot-one",
                        "git": {"commit": self.identity.commit, "dirty": "false"},
                    }
                },
                {"snapshot_id": "snapshot-one"},
                "immutable source",
            ),
            (
                {
                    "snapshot": {
                        "id": "snapshot-one",
                        "git": {
                            "commit": self.identity.commit,
                            "unavailable": True,
                        },
                    }
                },
                {"snapshot_id": "snapshot-one"},
                "immutable source",
            ),
            (
                {
                    "snapshot": {
                        "id": "snapshot-one",
                        "git": {
                            "commit": self.identity.commit,
                            "working_tree_digest": "sha256:unexpected",
                        },
                    }
                },
                {"snapshot_id": "snapshot-one"},
                "immutable source",
            ),
            (valid_bundle, {"snapshot_id": "other"}, "not bound"),
        ):
            with self.subTest(marker=marker):
                write_demo(bundle, coverage)
                with self.assertRaisesRegex(PACKAGE.PackageError, marker):
                    PACKAGE.validate_demo_outputs(demo, self.identity)

        source = PACKAGE.ROOT / "selected-source"
        target = PACKAGE.ROOT / "selected-target"
        source.mkdir()
        with self.assertRaisesRegex(
            PACKAGE.PackageError, "required fixture is missing"
        ):
            PACKAGE.copy_required_files(source, target, ("required.json",), "fixture")
        (source / "required.json").write_text("{}\n", encoding="utf-8")
        PACKAGE.copy_required_files(source, target, ("required.json",), "fixture")
        self.assertTrue((target / "required.json").is_file())
        (source / "weight.gguf").write_bytes(b"GGUF")
        with self.assertRaisesRegex(PACKAGE.PackageError, "prohibited"):
            PACKAGE.copy_selected_files(source, target, ("weight.gguf",), "fixture")

        self.assertRegex(
            PACKAGE.package_verification_code(["a" * 40, "b" * 40]),
            r"^[0-9a-f]{40}$",
        )
        for digests in ([], ["short"], ["z" * 40]):
            with self.subTest(digests=digests), self.assertRaisesRegex(
                PACKAGE.PackageError, "verification input"
            ):
                PACKAGE.package_verification_code(digests)

    def test_symlink_component_and_tree_inventory_are_rejected(self) -> None:
        root = PACKAGE.ROOT / "tree"
        (root / "real").mkdir(parents=True)
        self.write("tree/real/file.txt", b"ok")
        self.assertEqual(
            PACKAGE.tree_files(root, "tree")[0][0], PurePosixPath("real/file.txt")
        )
        PACKAGE.assert_no_symlink_components(root, PurePosixPath("real/file.txt"))
        with self.assertRaisesRegex(PACKAGE.PackageError, "disappeared"):
            PACKAGE.assert_no_symlink_components(root, PurePosixPath("missing/file"))
        ordinary = self.write("tree/not-dir", b"x")
        with self.assertRaisesRegex(PACKAGE.PackageError, "not a directory"):
            PACKAGE.assert_no_symlink_components(root, PurePosixPath("not-dir/file"))
        if hasattr(os, "symlink"):
            link = root / "link"
            link.symlink_to(root / "real", target_is_directory=True)
            with self.assertRaisesRegex(PACKAGE.PackageError, "symlink"):
                PACKAGE.assert_no_symlink_components(root, PurePosixPath("link/file"))
            with self.assertRaises(PACKAGE.PackageError):
                PACKAGE.tree_files(root, "tree")
            link.unlink()
            file_link = root / "linked-file"
            file_link.symlink_to(ordinary)
            with self.assertRaises(PACKAGE.PackageError):
                PACKAGE.tree_files(root, "tree")
        with self.assertRaisesRegex(PACKAGE.PackageError, "missing"):
            PACKAGE.tree_files(PACKAGE.ROOT / "absent", "fixture")
        with self.assertRaises(PACKAGE.PackageError):
            PACKAGE.tree_files(ordinary, "fixture")

    def test_copy_regular_file_enforces_content_and_mode(self) -> None:
        source = self.write("source.txt", b"payload")
        target = PACKAGE.ROOT / "stage/nested/target"
        self.assertEqual(
            PACKAGE.copy_regular_file(source, target, mode=0o755, label="fixture"),
            len(b"payload"),
        )
        self.assertEqual(target.read_bytes(), b"payload")
        self.assertEqual(target.stat().st_mode & 0o777, 0o755)
        with self.assertRaisesRegex(PACKAGE.PackageError, "duplicate"):
            PACKAGE.copy_regular_file(source, target, mode=0o644, label="fixture")
        for content, marker in ((b"GGUFdata", "model"), (b"\x7fELFdata", "compiled")):
            source.write_bytes(content)
            with self.assertRaisesRegex(PACKAGE.PackageError, marker):
                PACKAGE.copy_regular_file(
                    source,
                    PACKAGE.ROOT / f"stage/{marker}",
                    mode=0o644,
                    label="fixture",
                )
        self.assertEqual(
            PACKAGE.copy_regular_file(
                source,
                PACKAGE.ROOT / "stage/allowed-elf",
                mode=0o755,
                label="binary",
                reject_native=False,
            ),
            len(source.read_bytes()),
        )
        with self.assertRaisesRegex(PACKAGE.PackageError, "missing"):
            PACKAGE.copy_regular_file(
                PACKAGE.ROOT / "missing",
                PACKAGE.ROOT / "stage/no",
                mode=0o644,
                label="x",
            )
        with self.assertRaisesRegex(PACKAGE.PackageError, "regular"):
            PACKAGE.copy_regular_file(
                PACKAGE.ROOT, PACKAGE.ROOT / "stage/dir", mode=0o644, label="x"
            )
        if hasattr(os, "symlink"):
            link = PACKAGE.ROOT / "source-link"
            link.symlink_to(source)
            with self.assertRaisesRegex(PACKAGE.PackageError, "symlink"):
                PACKAGE.copy_regular_file(
                    link, PACKAGE.ROOT / "stage/link", mode=0o644, label="x"
                )

    def test_copy_tracked_source_requires_license_closure(self) -> None:
        object_id = "c" * 40
        tracked = [
            PACKAGE.TrackedFile(PurePosixPath("LICENSE"), False, object_id),
            PACKAGE.TrackedFile(PurePosixPath("NOTICE"), False, object_id),
            PACKAGE.TrackedFile(
                PurePosixPath("THIRD_PARTY_NOTICES.md"), False, object_id
            ),
            PACKAGE.TrackedFile(PurePosixPath("LICENSES/Go.txt"), False, object_id),
            PACKAGE.TrackedFile(
                PurePosixPath("LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE"),
                False,
                object_id,
            ),
            PACKAGE.TrackedFile(PACKAGE.GO_MODULE_LOCK, False, object_id),
            PACKAGE.TrackedFile(PurePosixPath("tool.sh"), True, object_id),
        ]
        for item in tracked:
            if item.path == PACKAGE.GO_MODULE_LOCK:
                continue
            self.write(str(item.path), b"terms\n", 0o755 if item.executable else 0o644)
        self.write_module_lock(PACKAGE.ROOT)
        target = PACKAGE.ROOT / "stage-source"

        def copy_fixture(item: object, destination: Path) -> int:
            tracked_item = item
            source = PACKAGE.ROOT.joinpath(*tracked_item.path.parts)
            return PACKAGE.copy_regular_file(
                source,
                destination,
                mode=0o755 if tracked_item.executable else 0o644,
                label="fixture commit blob",
            )

        with mock.patch.object(
            PACKAGE, "tracked_files", return_value=tracked
        ), mock.patch.object(PACKAGE, "copy_commit_blob", side_effect=copy_fixture):
            PACKAGE.copy_tracked_source(target, self.identity)
        self.assertEqual((target / "tool.sh").stat().st_mode & 0o777, 0o755)
        with mock.patch.object(
            PACKAGE,
            "tracked_files",
            return_value=[
                PACKAGE.TrackedFile(PurePosixPath("weight.gguf"), False, object_id)
            ],
        ), self.assertRaisesRegex(PACKAGE.PackageError, "prohibited"):
            PACKAGE.copy_tracked_source(PACKAGE.ROOT / "other", self.identity)
        with mock.patch.object(
            PACKAGE, "tracked_files", return_value=[]
        ), self.assertRaisesRegex(PACKAGE.PackageError, "required license"):
            PACKAGE.copy_tracked_source(PACKAGE.ROOT / "empty", self.identity)

    def release_binary_fixture(self, source: Path) -> None:
        for name in PACKAGE.TOP_LEVEL_LICENSE_FILES:
            self.write(name, ("terms " + name).encode())
        self.write("LICENSES/Go.txt", b"Go terms")
        self.write(
            "LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE",
            b"SQLite terms",
        )
        self.write_module_lock(PACKAGE.ROOT)
        for relative in PACKAGE.EXPECTED_BINARIES:
            path = source.joinpath(*relative.parts)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b"\x7fELFfixture")
        for relative in PACKAGE.EXPECTED_BINARY_SBOMS:
            path = source.joinpath(*relative.parts)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(
                json.dumps(
                    {
                        "spdxVersion": "SPDX-2.3",
                        "dataLicense": "CC0-1.0",
                        "packages": [{"name": relative.stem}],
                    }
                ),
                encoding="utf-8",
            )
        for relative in PACKAGE.binary_notice_files():
            path = source.joinpath(*relative.parts)
            path.parent.mkdir(parents=True, exist_ok=True)
            canonical_parts = (
                relative.parts[1:]
                if relative.parts[0].startswith("linux-")
                else relative.parts
            )
            canonical = PACKAGE.ROOT.joinpath(*canonical_parts)
            path.write_bytes(canonical.read_bytes())

    def write_staged_distribution_components(self, stage: Path) -> None:
        dependency = {
            "SPDXID": "SPDXRef-Module-0001",
            "name": "example.test/module",
            "versionInfo": "v1.0.0",
            "downloadLocation": "https://example.test/module.zip",
            "filesAnalyzed": False,
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": "BSD-3-Clause",
            "copyrightText": "NOASSERTION",
            "externalRefs": [
                {
                    "referenceCategory": "PACKAGE-MANAGER",
                    "referenceType": "purl",
                    "referenceLocator": "pkg:golang/example.test/module@v1.0.0",
                }
            ],
        }
        for relative in PACKAGE.EXPECTED_BINARIES:
            binary = stage / "artifacts" / "binaries" / relative
            binary.parent.mkdir(parents=True, exist_ok=True)
            binary.write_bytes(b"\x7fELFdistribution fixture")
            sbom = binary.with_name(binary.name + ".spdx.json")
            sbom.write_text(
                json.dumps(
                    {
                        "spdxVersion": "SPDX-2.3",
                        "packages": [
                            {"SPDXID": "SPDXRef-Package-RKC"},
                            dependency,
                        ],
                    },
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )

    @mock.patch.object(PACKAGE, "validate_binary_sbom")
    def test_release_binary_bundle_is_exact_and_notices_match(
        self, validate_sbom: mock.Mock
    ) -> None:
        source = PACKAGE.ROOT / "bundle"
        self.release_binary_fixture(source)
        target = PACKAGE.ROOT / "staged-bundle"
        module_lock = PACKAGE.ROOT.joinpath(*PACKAGE.GO_MODULE_LOCK.parts)
        PACKAGE.copy_release_binaries(
            source, target, module_lock, PACKAGE.ROOT, self.identity
        )
        self.assertEqual(
            len(PACKAGE.tree_files(target, "staged")),
            len(PACKAGE.expected_binary_bundle()),
        )
        self.assertEqual(validate_sbom.call_count, len(PACKAGE.EXPECTED_BINARY_SBOMS))
        for relative in PACKAGE.EXPECTED_BINARY_SBOMS:
            binary_name = relative.name[: -len(".spdx.json")]
            validate_sbom.assert_any_call(
                target.joinpath(*relative.parts),
                target / relative.parent / binary_name,
                module_lock,
                PACKAGE.ROOT,
                self.identity,
                "linux",
                relative.parts[0].split("-", 1)[1],
            )
        nested_notice = source / (
            "linux-amd64/LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE"
        )
        nested_content = nested_notice.read_bytes()
        nested_notice.unlink()
        with self.assertRaisesRegex(PACKAGE.PackageError, "missing=.*sqlite"):
            PACKAGE.copy_release_binaries(
                source,
                PACKAGE.ROOT / "missing-license",
                module_lock,
                PACKAGE.ROOT,
                self.identity,
            )
        nested_notice.write_bytes(nested_content)
        (source / "unexpected").write_text("x", encoding="utf-8")
        with self.assertRaisesRegex(PACKAGE.PackageError, "differs"):
            PACKAGE.copy_release_binaries(
                source,
                PACKAGE.ROOT / "nope",
                module_lock,
                PACKAGE.ROOT,
                self.identity,
            )
        (source / "unexpected").unlink()
        binary = source.joinpath(*next(iter(PACKAGE.EXPECTED_BINARIES)).parts)
        binary.write_bytes(b"NOTELF")
        with self.assertRaisesRegex(PACKAGE.PackageError, "not an ELF"):
            PACKAGE.copy_release_binaries(
                source,
                PACKAGE.ROOT / "nope2",
                module_lock,
                PACKAGE.ROOT,
                self.identity,
            )

    @mock.patch.object(PACKAGE.subprocess, "run")
    def test_binary_sbom_verification_delegates_to_strict_generator(
        self, run: mock.Mock
    ) -> None:
        sbom = self.write("bundle/linux-amd64/rkc.spdx.json", b"{}\n")
        binary = self.write("bundle/linux-amd64/rkc", b"\x7fELFfixture", 0o755)
        self.write("VERSION", b"0.3.0-reference\n")
        module_lock = self.write("third_party/go-modules.lock.json", b"{}\n")
        run.return_value = subprocess.CompletedProcess([], 0, stdout="", stderr="")
        PACKAGE.validate_binary_sbom(
            sbom,
            binary,
            module_lock,
            PACKAGE.ROOT,
            self.identity,
            "linux",
            "amd64",
        )
        command = run.call_args.args[0]
        self.assertIn("--verify-document", command)
        self.assertIn(str(sbom), command)
        self.assertIn(str(binary), command)
        self.assertIn("--source-commit", command)
        self.assertIn("--goarch", command)
        self.assertIn("amd64", command)

        run.return_value = subprocess.CompletedProcess([], 2, stdout="", stderr="drift")
        with self.assertRaisesRegex(PACKAGE.PackageError, "does not bind.*drift"):
            PACKAGE.validate_binary_sbom(
                sbom,
                binary,
                module_lock,
                PACKAGE.ROOT,
                self.identity,
                "linux",
                "amd64",
            )
        run.side_effect = subprocess.TimeoutExpired(command, 30)
        with self.assertRaisesRegex(PACKAGE.PackageError, "cannot verify"):
            PACKAGE.validate_binary_sbom(
                sbom,
                binary,
                module_lock,
                PACKAGE.ROOT,
                self.identity,
                "linux",
                "amd64",
            )

    def test_generated_tree_allowlist_and_total_boundary(self) -> None:
        source = PACKAGE.ROOT / "data"
        self.write("data/one.json", b"one")
        self.write("data/two.txt", b"two")
        target = PACKAGE.ROOT / "data-stage"
        PACKAGE.copy_data_tree(source, target, "data")
        self.assertEqual((target / "one.json").read_bytes(), b"one")
        selected = PACKAGE.ROOT / "selected"
        PACKAGE.copy_selected_files(
            source, selected, ("one.json", "absent"), "selected"
        )
        self.assertTrue((selected / "one.json").is_file())
        self.write("data/model.gguf", b"GGUF")
        with self.assertRaisesRegex(PACKAGE.PackageError, "prohibited"):
            PACKAGE.copy_data_tree(source, PACKAGE.ROOT / "bad-stage", "data")
        (source / "model.gguf").unlink()
        with mock.patch.object(
            PACKAGE, "MAX_ARTIFACT_TOTAL_BYTES", 2
        ), self.assertRaisesRegex(PACKAGE.PackageError, "total"):
            PACKAGE.copy_data_tree(source, PACKAGE.ROOT / "large-stage", "data")

    def test_release_evidence_is_exact_bound_and_normalized(self) -> None:
        evidence = self.write_release_evidence(PACKAGE.ROOT / "validation")
        benchmark = self.write_benchmark_evidence(PACKAGE.ROOT / "benchmark")
        receipt = PACKAGE.validate_release_evidence(evidence, benchmark, self.identity)
        self.assertEqual(receipt["status"], "passed")
        self.assertEqual(receipt["raw_evidence_root"], "evidence")
        self.assertNotIn("elapsed_seconds", receipt)
        self.assertNotIn("log_sha256", receipt["steps"][0])
        raw_files = receipt["raw_evidence_files"]
        self.assertEqual(raw_files[0]["path"], "validation/summary.json")
        self.assertEqual(raw_files[1]["path"], "validation/steps.tsv")
        self.assertEqual(raw_files[-3]["path"], "benchmark/result.json")
        self.assertEqual(raw_files[-2]["path"], "benchmark/time.txt")
        self.assertEqual(raw_files[-1]["path"], "benchmark/scan.stdout")
        self.assertEqual(
            len(raw_files),
            len(PACKAGE.RELEASE_STEPS) + 5,
        )
        self.assertRegex(receipt["raw_evidence_manifest_sha256"], r"^[0-9a-f]{64}$")
        self.assertTrue(
            all(
                isinstance(item["size_bytes"], int)
                and item["size_bytes"] >= 0
                and len(item["sha256"]) == 64
                for item in raw_files
            )
        )

        unexpected = evidence / "logs" / "stale.log"
        unexpected.write_text("stale\n", encoding="utf-8")
        with self.assertRaisesRegex(PACKAGE.PackageError, "inventory differs"):
            PACKAGE.validate_release_evidence(evidence, benchmark, self.identity)
        unexpected.unlink()

        log = evidence / "logs" / f"{PACKAGE.RELEASE_STEPS[0]}.log"
        original = log.read_bytes()
        log.write_bytes(b"tampered\n")
        with self.assertRaisesRegex(PACKAGE.PackageError, "digest differs"):
            PACKAGE.validate_release_evidence(evidence, benchmark, self.identity)
        log.write_bytes(original)

        other = PACKAGE.SourceIdentity("c" * 40, self.identity.tree, "7")
        with self.assertRaisesRegex(PACKAGE.PackageError, "source identity"):
            PACKAGE.validate_release_evidence(evidence, benchmark, other)

        (benchmark / "time.txt").write_text("changed\n", encoding="utf-8")
        changed = PACKAGE.validate_release_evidence(evidence, benchmark, self.identity)
        self.assertNotEqual(
            receipt["raw_evidence_manifest_sha256"],
            changed["raw_evidence_manifest_sha256"],
        )
        (benchmark / "unexpected").write_text("unexpected\n", encoding="utf-8")
        with self.assertRaisesRegex(
            PACKAGE.PackageError, "benchmark evidence inventory"
        ):
            PACKAGE.validate_release_evidence(evidence, benchmark, self.identity)

    def test_go_module_lock_closes_nested_license_inventory(self) -> None:
        for name in PACKAGE.TOP_LEVEL_LICENSE_FILES:
            self.write(name, (name + "\n").encode())
        self.write("LICENSES/Go.txt", b"Go terms\n")
        module_license = self.write(
            "LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE",
            b"SQLite terms\n",
        )
        module_lock = self.write_module_lock(PACKAGE.ROOT)
        PACKAGE.validate_go_module_lock(PACKAGE.ROOT)

        original = module_license.read_bytes()
        module_license.write_bytes(b"tampered\n")
        with self.assertRaisesRegex(PACKAGE.PackageError, "digest differs"):
            PACKAGE.validate_go_module_lock(PACKAGE.ROOT)
        module_license.write_bytes(original)

        unlocked = self.write(
            "LICENSES/go-modules/example.invalid/unlocked@v1.0.0/LICENSE",
            b"unlocked\n",
        )
        with self.assertRaisesRegex(PACKAGE.PackageError, "unlocked"):
            PACKAGE.validate_go_module_lock(PACKAGE.ROOT)
        unlocked.unlink()

        module_lock.write_text(
            '{"schema_version":"1.0","schema_version":"1.0"}\n',
            encoding="utf-8",
        )
        with self.assertRaisesRegex(PACKAGE.PackageError, "duplicate JSON key"):
            PACKAGE.validate_go_module_lock(PACKAGE.ROOT)

    def test_license_readme_manifest_and_zip_are_deterministic(self) -> None:
        stage = PACKAGE.ROOT / "stage"
        stage.mkdir()
        source = stage / "source"
        for name in PACKAGE.TOP_LEVEL_LICENSE_FILES:
            path = source / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(name, encoding="utf-8")
        (source / "LICENSES").mkdir()
        (source / "LICENSES/Go.txt").write_text("go", encoding="utf-8")
        sqlite_license = (
            source / "LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE"
        )
        sqlite_license.parent.mkdir(parents=True)
        sqlite_license.write_text("sqlite", encoding="utf-8")
        module_lock = self.write_module_lock(source)
        (source / "VERSION").write_text("1.2.3\n", encoding="utf-8")
        self.write_staged_distribution_components(stage)
        PACKAGE.copy_license_material(source, stage)
        PACKAGE.write_source_receipt(stage, self.identity)
        PACKAGE.write_readme(stage, self.identity)
        PACKAGE.write_distribution_sbom(stage, self.identity)
        PACKAGE.stage_manifest(stage, self.identity)
        PACKAGE.write_stage_checksums(stage)
        distribution_sbom = json.loads(
            (stage / "SBOM.spdx.json").read_text(encoding="utf-8")
        )
        distribution = distribution_sbom["packages"][0]
        self.assertTrue(distribution["filesAnalyzed"])
        self.assertEqual(distribution["licenseConcluded"], "NOASSERTION")
        self.assertEqual(
            distribution["packageVerificationCode"][
                "packageVerificationCodeExcludedFiles"
            ],
            list(PACKAGE.DISTRIBUTION_SBOM_EXCLUSIONS),
        )
        sbom_names = {item["fileName"] for item in distribution_sbom["files"]}
        self.assertTrue(
            all(name not in sbom_names for name in PACKAGE.DISTRIBUTION_SBOM_EXCLUSIONS)
        )
        self.assertEqual(
            len(
                [
                    item
                    for item in distribution_sbom["packages"]
                    if item["SPDXID"].startswith("SPDXRef-Binary-")
                ]
            ),
            len(PACKAGE.EXPECTED_BINARIES),
        )
        manifest = json.loads((stage / "MANIFEST.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["version"], "1.2.3")
        self.assertEqual(manifest["go_module_lock"], "third_party/go-modules.lock.json")
        self.assertGreater(manifest["payload_files"], 0)
        manifest_files = {item["path"]: item for item in manifest["files"]}
        self.assertEqual(
            manifest_files["SBOM.spdx.json"]["sha256"],
            PACKAGE.sha256(stage / "SBOM.spdx.json"),
        )
        checksums = (stage / "SHA256SUMS.txt").read_text(encoding="utf-8")
        self.assertIn("  SBOM.spdx.json\n", checksums)
        self.assertIn("  MANIFEST.json\n", checksums)
        self.assertNotIn("  SHA256SUMS.txt\n", checksums)
        self.assertEqual(
            (stage / "third_party/go-modules.lock.json").read_bytes(),
            module_lock.read_bytes(),
        )
        output = PACKAGE.DIST / "complete.zip"
        second = PACKAGE.DIST / "complete-second.zip"
        PACKAGE.write_zip(stage, output, False)
        PACKAGE.write_zip(stage, second, False)
        self.assertEqual(output.read_bytes(), second.read_bytes())
        with zipfile.ZipFile(output) as archive:
            names = archive.namelist()
            self.assertTrue(all(name.startswith(PACKAGE.TOP + "/") for name in names))
            self.assertEqual(archive.getinfo(names[0]).date_time, (1980, 1, 1, 0, 0, 0))
            self.assertTrue(
                all(
                    item.compress_type == zipfile.ZIP_STORED
                    for item in archive.infolist()
                )
            )
        with self.assertRaisesRegex(PACKAGE.PackageError, "appeared"):
            PACKAGE.write_zip(stage, output, False)
        PACKAGE.write_zip(stage, output, True)

        module_lock.write_text("[]\n", encoding="utf-8")
        with self.assertRaisesRegex(PACKAGE.PackageError, "schema_version 1.0"):
            PACKAGE.copy_license_material(source, PACKAGE.ROOT / "bad-lock-stage")

    def test_prepare_output_enforces_dist_and_safe_replacement(self) -> None:
        candidate = PACKAGE.prepare_output("dist/nested/result.zip", False)
        self.assertEqual(candidate, PACKAGE.DIST / "nested/result.zip")
        for value in ("outside.zip", "dist/not-zip", "dist/demo/output.zip"):
            with self.subTest(value=value), self.assertRaises(PACKAGE.PackageError):
                PACKAGE.prepare_output(value, False)
        candidate.write_text("old", encoding="utf-8")
        with self.assertRaisesRegex(PACKAGE.PackageError, "already exists"):
            PACKAGE.prepare_output(str(candidate), False)
        self.assertEqual(PACKAGE.prepare_output(str(candidate), True), candidate)
        if hasattr(os, "symlink"):
            linked_parent = PACKAGE.DIST / "linked"
            linked_parent.symlink_to(PACKAGE.ROOT, target_is_directory=True)
            with self.assertRaises(PACKAGE.PackageError):
                PACKAGE.prepare_output("dist/linked/file.zip", False)
            linked_output = PACKAGE.DIST / "linked-output.zip"
            linked_output.symlink_to(candidate)
            with self.assertRaises(PACKAGE.PackageError):
                PACKAGE.prepare_output(str(linked_output), True)
        original = PACKAGE.DIST
        PACKAGE.DIST = PACKAGE.ROOT / "missing-dist"
        try:
            with self.assertRaisesRegex(PACKAGE.PackageError, "dist does not exist"):
                PACKAGE.prepare_output("missing-dist/a.zip", False)
        finally:
            PACKAGE.DIST = original

    def test_release_shell_uses_isolated_caches_and_atomic_generations(self) -> None:
        scripts = Path(__file__).parent
        reproducible = (scripts / "reproducible-complete-package.sh").read_text(
            encoding="utf-8"
        )
        verifier = (scripts / "verify-release.sh").read_text(encoding="utf-8")
        demo = (scripts / "generate-demo.sh").read_text(encoding="utf-8")
        binaries = (scripts / "build-release-binaries.sh").read_text(encoding="utf-8")
        self.assertIn('export GOCACHE="$lane_go_cache"', reproducible)
        self.assertIn('export GOMODCACHE="$lane_module_cache"', reproducible)
        self.assertIn("export GOFLAGS='-p=1 -modcacherw'", demo)
        self.assertIn("export GOFLAGS='-p=1 -modcacherw'", binaries)
        self.assertIn("-mod=readonly", demo)
        self.assertIn("-mod=readonly", binaries)
        self.assertIn("go mod verify", binaries)
        self.assertIn('--destination "$ROOT/dist/release"', reproducible)
        self.assertIn('mv "$WORK/evidence" "$RELEASE_STAGE/evidence"', reproducible)
        self.assertIn('--destination "$ROOT/dist/evidence"', verifier)
        self.assertIn("prior evidence is unchanged", verifier)

    def test_build_package_orchestrates_only_expected_inputs(self) -> None:
        for relative in PACKAGE.REQUIRED_INPUTS:
            (PACKAGE.DIST / relative).mkdir(parents=True)
        output = PACKAGE.DIST / "result.zip"
        calls: list[str] = []
        with mock.patch.object(
            PACKAGE, "source_identity", return_value=self.identity
        ), mock.patch.object(
            PACKAGE,
            "require_clean_tracked_source",
            side_effect=lambda: calls.append("clean"),
        ), mock.patch.object(
            PACKAGE,
            "copy_tracked_source",
            side_effect=lambda _path, _identity: calls.append("source"),
        ), mock.patch.object(
            PACKAGE, "copy_license_material"
        ), mock.patch.object(
            PACKAGE, "copy_release_binaries"
        ), mock.patch.object(
            PACKAGE, "validate_demo_outputs"
        ), mock.patch.object(
            PACKAGE, "copy_required_files"
        ), mock.patch.object(
            PACKAGE, "validate_release_evidence", return_value={}
        ), mock.patch.object(
            PACKAGE, "write_json_file"
        ), mock.patch.object(
            PACKAGE, "write_source_receipt"
        ), mock.patch.object(
            PACKAGE, "write_readme"
        ), mock.patch.object(
            PACKAGE, "write_distribution_sbom"
        ), mock.patch.object(
            PACKAGE, "stage_manifest"
        ), mock.patch.object(
            PACKAGE, "write_stage_checksums"
        ), mock.patch.object(
            PACKAGE, "write_zip", side_effect=lambda *_args: calls.append("zip")
        ):
            PACKAGE.build_package(output, False)
        self.assertEqual(calls, ["clean", "source", "zip"])
        (PACKAGE.DIST / "demo").rmdir()
        with mock.patch.object(
            PACKAGE, "source_identity", return_value=self.identity
        ), mock.patch.object(
            PACKAGE, "require_clean_tracked_source"
        ), self.assertRaisesRegex(
            PACKAGE.PackageError, "missing release"
        ):
            PACKAGE.build_package(output, False)
        (PACKAGE.DIST / "demo").symlink_to(PACKAGE.ROOT, target_is_directory=True)
        with mock.patch.object(
            PACKAGE, "source_identity", return_value=self.identity
        ), mock.patch.object(
            PACKAGE, "require_clean_tracked_source"
        ), self.assertRaisesRegex(
            PACKAGE.PackageError, "not a real"
        ):
            PACKAGE.build_package(output, False)

    def test_main_reports_success_and_translates_package_error(self) -> None:
        output = PACKAGE.DIST / "result.zip"
        output.write_bytes(b"zip")
        argv = ["package-complete.py", "--output", str(output), "--force"]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            PACKAGE, "prepare_output", return_value=output
        ), mock.patch.object(PACKAGE, "build_package"), mock.patch(
            "sys.stdout", new=io.StringIO()
        ) as stdout:
            PACKAGE.main()
            self.assertEqual(json.loads(stdout.getvalue())["size_bytes"], 3)
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            PACKAGE, "prepare_output", side_effect=PACKAGE.PackageError("no")
        ), self.assertRaisesRegex(SystemExit, "package error"):
            PACKAGE.main()


if __name__ == "__main__":
    unittest.main()
