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


def index_entry(mode: str, path: str, stage: str = "0") -> bytes:
    return f"{mode} {'0' * 40} {stage}\t{path}".encode() + b"\0"


class CompletePackageTests(unittest.TestCase):
    def setUp(self) -> None:
        self.original_root = PACKAGE.ROOT
        self.original_dist = PACKAGE.DIST
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-package-test-")
        PACKAGE.ROOT = Path(self.temporary.name)
        PACKAGE.DIST = PACKAGE.ROOT / "dist"
        PACKAGE.DIST.mkdir(mode=0o755)

    def tearDown(self) -> None:
        PACKAGE.ROOT = self.original_root
        PACKAGE.DIST = self.original_dist
        self.temporary.cleanup()

    def write(self, relative: str, content: bytes = b"fixture\n", mode: int = 0o644) -> Path:
        path = PACKAGE.ROOT / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content)
        os.chmod(path, mode)
        return path

    def test_safe_paths_and_prohibited_names(self) -> None:
        self.assertEqual(PACKAGE.safe_relative_path("a/b.txt", "fixture"), PurePosixPath("a/b.txt"))
        for value in ("", "/abs", "../escape", "a/../b", "a\\b", "a\x01b"):
            with self.subTest(value=value), self.assertRaises(PACKAGE.PackageError):
                PACKAGE.safe_relative_path(value, "fixture")
        self.assertTrue(PACKAGE.prohibited_name(PurePosixPath("MODEL.safetensors")))
        self.assertTrue(PACKAGE.prohibited_name(PurePosixPath("model-00001.bin")))
        self.assertFalse(PACKAGE.prohibited_name(PurePosixPath("model.json")))

    @mock.patch.object(PACKAGE.subprocess, "run")
    def test_tracked_files_parses_modes_and_rejects_index_hazards(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(
            [],
            0,
            stdout=index_entry("100755", "scripts/tool.sh") + index_entry("100644", "README.md"),
            stderr=b"",
        )
        files = PACKAGE.tracked_files()
        self.assertEqual([str(item.path) for item in files], ["README.md", "scripts/tool.sh"])
        self.assertTrue(files[1].executable)

        cases = (
            (index_entry("100644", "conflict", "2"), "unmerged"),
            (index_entry("120000", "link"), "symlinks"),
            (index_entry("160000", "module"), "submodules"),
            (index_entry("100600", "mode"), "unsupported"),
            (index_entry("100644", "dist/output"), "generated"),
            (index_entry("100644", "same") * 2, "duplicate"),
            (b"malformed\0", "unportable"),
            (b"", "no source"),
        )
        for payload, marker in cases:
            with self.subTest(marker=marker):
                run.return_value = subprocess.CompletedProcess([], 0, stdout=payload, stderr=b"")
                with self.assertRaisesRegex(PACKAGE.PackageError, marker):
                    PACKAGE.tracked_files()
        run.return_value = subprocess.CompletedProcess([], 7, stdout=b"", stderr=b"failure")
        with self.assertRaisesRegex(PACKAGE.PackageError, "failure"):
            PACKAGE.tracked_files()

    @mock.patch.object(PACKAGE.subprocess, "run")
    def test_clean_source_requires_both_git_diff_checks(self, run: mock.Mock) -> None:
        run.side_effect = [subprocess.CompletedProcess([], 0), subprocess.CompletedProcess([], 0)]
        PACKAGE.require_clean_tracked_source()
        self.assertEqual(run.call_count, 2)
        for status, marker in ((1, "dirty"), (2, "cannot verify")):
            run.reset_mock()
            run.side_effect = None
            run.return_value = subprocess.CompletedProcess([], status)
            with self.assertRaisesRegex(PACKAGE.PackageError, marker):
                PACKAGE.require_clean_tracked_source()

    def test_symlink_component_and_tree_inventory_are_rejected(self) -> None:
        root = PACKAGE.ROOT / "tree"
        (root / "real").mkdir(parents=True)
        self.write("tree/real/file.txt", b"ok")
        self.assertEqual(PACKAGE.tree_files(root, "tree")[0][0], PurePosixPath("real/file.txt"))
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
                    source, PACKAGE.ROOT / f"stage/{marker}", mode=0o644, label="fixture"
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
                PACKAGE.ROOT / "missing", PACKAGE.ROOT / "stage/no", mode=0o644, label="x"
            )
        with self.assertRaisesRegex(PACKAGE.PackageError, "regular"):
            PACKAGE.copy_regular_file(
                PACKAGE.ROOT, PACKAGE.ROOT / "stage/dir", mode=0o644, label="x"
            )
        if hasattr(os, "symlink"):
            link = PACKAGE.ROOT / "source-link"
            link.symlink_to(source)
            with self.assertRaisesRegex(PACKAGE.PackageError, "symlink"):
                PACKAGE.copy_regular_file(link, PACKAGE.ROOT / "stage/link", mode=0o644, label="x")

    def test_copy_tracked_source_requires_license_closure(self) -> None:
        tracked = [
            PACKAGE.TrackedFile(PurePosixPath("LICENSE"), False),
            PACKAGE.TrackedFile(PurePosixPath("NOTICE"), False),
            PACKAGE.TrackedFile(PurePosixPath("THIRD_PARTY_NOTICES.md"), False),
            PACKAGE.TrackedFile(PurePosixPath("LICENSES/Go.txt"), False),
            PACKAGE.TrackedFile(PurePosixPath("tool.sh"), True),
        ]
        for item in tracked:
            self.write(str(item.path), b"terms\n", 0o755 if item.executable else 0o644)
        target = PACKAGE.ROOT / "stage-source"
        with mock.patch.object(PACKAGE, "tracked_files", return_value=tracked):
            PACKAGE.copy_tracked_source(target)
        self.assertEqual((target / "tool.sh").stat().st_mode & 0o777, 0o755)
        with mock.patch.object(
            PACKAGE,
            "tracked_files",
            return_value=[PACKAGE.TrackedFile(PurePosixPath("weight.gguf"), False)],
        ), self.assertRaisesRegex(PACKAGE.PackageError, "prohibited"):
            PACKAGE.copy_tracked_source(PACKAGE.ROOT / "other")
        with mock.patch.object(PACKAGE, "tracked_files", return_value=[]), self.assertRaisesRegex(
            PACKAGE.PackageError, "required license"
        ):
            PACKAGE.copy_tracked_source(PACKAGE.ROOT / "empty")

    def release_binary_fixture(self, source: Path) -> None:
        for relative in PACKAGE.EXPECTED_BINARIES:
            path = source.joinpath(*relative.parts)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b"\x7fELFfixture")
        for relative in PACKAGE.BINARY_NOTICE_FILES:
            path = source.joinpath(*relative.parts)
            path.parent.mkdir(parents=True, exist_ok=True)
            canonical_parts = relative.parts[1:] if relative.parts[0].startswith("linux-") else relative.parts
            canonical = PACKAGE.ROOT.joinpath(*canonical_parts)
            canonical.parent.mkdir(parents=True, exist_ok=True)
            canonical.write_bytes(("terms " + "/".join(canonical_parts)).encode())
            path.write_bytes(canonical.read_bytes())

    def test_release_binary_bundle_is_exact_and_notices_match(self) -> None:
        source = PACKAGE.ROOT / "bundle"
        self.release_binary_fixture(source)
        target = PACKAGE.ROOT / "staged-bundle"
        PACKAGE.copy_release_binaries(source, target)
        self.assertEqual(len(PACKAGE.tree_files(target, "staged")), len(PACKAGE.EXPECTED_BINARY_BUNDLE))
        (source / "unexpected").write_text("x", encoding="utf-8")
        with self.assertRaisesRegex(PACKAGE.PackageError, "differs"):
            PACKAGE.copy_release_binaries(source, PACKAGE.ROOT / "nope")
        (source / "unexpected").unlink()
        binary = source.joinpath(*next(iter(PACKAGE.EXPECTED_BINARIES)).parts)
        binary.write_bytes(b"NOTELF")
        with self.assertRaisesRegex(PACKAGE.PackageError, "not an ELF"):
            PACKAGE.copy_release_binaries(source, PACKAGE.ROOT / "nope2")

    def test_generated_tree_allowlist_and_total_boundary(self) -> None:
        source = PACKAGE.ROOT / "data"
        self.write("data/one.json", b"one")
        self.write("data/two.txt", b"two")
        target = PACKAGE.ROOT / "data-stage"
        PACKAGE.copy_data_tree(source, target, "data")
        self.assertEqual((target / "one.json").read_bytes(), b"one")
        selected = PACKAGE.ROOT / "selected"
        PACKAGE.copy_selected_files(source, selected, ("one.json", "absent"), "selected")
        self.assertTrue((selected / "one.json").is_file())
        self.write("data/model.gguf", b"GGUF")
        with self.assertRaisesRegex(PACKAGE.PackageError, "prohibited"):
            PACKAGE.copy_data_tree(source, PACKAGE.ROOT / "bad-stage", "data")
        (source / "model.gguf").unlink()
        with mock.patch.object(PACKAGE, "MAX_ARTIFACT_TOTAL_BYTES", 2), self.assertRaisesRegex(
            PACKAGE.PackageError, "total"
        ):
            PACKAGE.copy_data_tree(source, PACKAGE.ROOT / "large-stage", "data")

    def test_license_readme_manifest_and_zip_are_deterministic(self) -> None:
        source = PACKAGE.ROOT / "source-tree"
        for name in PACKAGE.TOP_LEVEL_LICENSE_FILES:
            path = source / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(name, encoding="utf-8")
        (source / "LICENSES").mkdir()
        (source / "LICENSES/Go.txt").write_text("go", encoding="utf-8")
        self.write("VERSION", b"1.2.3\n")
        stage = PACKAGE.ROOT / "stage"
        stage.mkdir()
        PACKAGE.copy_license_material(source, stage)
        PACKAGE.write_readme(stage)
        PACKAGE.stage_manifest(stage)
        manifest = json.loads((stage / "MANIFEST.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["version"], "1.2.3")
        self.assertGreater(manifest["payload_files"], 0)
        output = PACKAGE.DIST / "complete.zip"
        PACKAGE.write_zip(stage, output, False)
        with zipfile.ZipFile(output) as archive:
            names = archive.namelist()
            self.assertTrue(all(name.startswith(PACKAGE.TOP + "/") for name in names))
            self.assertEqual(archive.getinfo(names[0]).date_time, (1980, 1, 1, 0, 0, 0))
        with self.assertRaisesRegex(PACKAGE.PackageError, "appeared"):
            PACKAGE.write_zip(stage, output, False)
        PACKAGE.write_zip(stage, output, True)

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

    def test_build_package_orchestrates_only_expected_inputs(self) -> None:
        for relative in PACKAGE.REQUIRED_INPUTS:
            (PACKAGE.DIST / relative).mkdir(parents=True)
        output = PACKAGE.DIST / "result.zip"
        calls: list[str] = []
        with mock.patch.object(PACKAGE, "require_clean_tracked_source", side_effect=lambda: calls.append("clean")), mock.patch.object(
            PACKAGE, "copy_tracked_source", side_effect=lambda _path: calls.append("source")
        ), mock.patch.object(PACKAGE, "copy_license_material"), mock.patch.object(
            PACKAGE, "copy_release_binaries"
        ), mock.patch.object(PACKAGE, "copy_data_tree"), mock.patch.object(
            PACKAGE, "copy_selected_files"
        ), mock.patch.object(PACKAGE, "write_readme"), mock.patch.object(
            PACKAGE, "stage_manifest"
        ), mock.patch.object(PACKAGE, "write_zip", side_effect=lambda *_args: calls.append("zip")):
            PACKAGE.build_package(output, False)
        self.assertEqual(calls, ["clean", "source", "zip"])
        (PACKAGE.DIST / "demo").rmdir()
        with mock.patch.object(PACKAGE, "require_clean_tracked_source"), self.assertRaisesRegex(
            PACKAGE.PackageError, "missing release"
        ):
            PACKAGE.build_package(output, False)
        (PACKAGE.DIST / "demo").symlink_to(PACKAGE.ROOT, target_is_directory=True)
        with mock.patch.object(PACKAGE, "require_clean_tracked_source"), self.assertRaisesRegex(
            PACKAGE.PackageError, "not a real"
        ):
            PACKAGE.build_package(output, False)

    def test_main_reports_success_and_translates_package_error(self) -> None:
        output = PACKAGE.DIST / "result.zip"
        output.write_bytes(b"zip")
        argv = ["package-complete.py", "--output", str(output), "--force"]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            PACKAGE, "prepare_output", return_value=output
        ), mock.patch.object(PACKAGE, "build_package"), mock.patch("sys.stdout", new=io.StringIO()) as stdout:
            PACKAGE.main()
            self.assertEqual(json.loads(stdout.getvalue())["size_bytes"], 3)
        with mock.patch.object(sys, "argv", argv), mock.patch.object(
            PACKAGE, "prepare_output", side_effect=PACKAGE.PackageError("no")
        ), self.assertRaisesRegex(SystemExit, "package error"):
            PACKAGE.main()


if __name__ == "__main__":
    unittest.main()
