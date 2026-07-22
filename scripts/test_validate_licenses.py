#!/usr/bin/env python3
"""Failure-oriented unit tests for the release license validator."""
from __future__ import annotations

import hashlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


MODULE_NAME = "rkc_validate_licenses"
SPEC = importlib.util.spec_from_file_location(
    MODULE_NAME, Path(__file__).with_name("validate-licenses.py")
)
assert SPEC and SPEC.loader
LICENSES = importlib.util.module_from_spec(SPEC)
sys.modules[MODULE_NAME] = LICENSES
SPEC.loader.exec_module(LICENSES)


def index_entry(mode: str, path: str, stage: str = "0") -> bytes:
    return f"{mode} {'0' * 40} {stage}\t{path}".encode("utf-8") + b"\0"


class LicenseValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.original_root = LICENSES.ROOT
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-license-test-")
        LICENSES.ROOT = Path(self.temporary.name)
        LICENSES.ERRORS.clear()
        LICENSES.CHECKS.clear()

    def tearDown(self) -> None:
        LICENSES.ROOT = self.original_root
        LICENSES.ERRORS.clear()
        LICENSES.CHECKS.clear()
        self.temporary.cleanup()

    def write(self, relative: str, value: str | bytes) -> Path:
        path = LICENSES.ROOT / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        if isinstance(value, bytes):
            path.write_bytes(value)
        else:
            path.write_text(value, encoding="utf-8")
        return path

    def root_fixture(self) -> None:
        self.write(
            "LICENSE",
            "Apache License\nVersion 2.0, January 2004\nEND OF TERMS AND CONDITIONS\n",
        )
        self.write(
            "NOTICE",
            "Repository Knowledge Compiler (RKC)\nCopyright 2026 RKC contributors\n",
        )
        self.write(
            "LICENSES/Go.txt",
            "SPDX-License-Identifier: BSD-3-Clause\n"
            "Copyright 2009 The Go Authors.\n"
            "Redistributions in binary form must reproduce\n"
            "Neither the name of Google LLC\n"
            "Additional IP Rights Grant (Patents)\n"
            "Google hereby grants to You a perpetual\n",
        )
        self.write(
            "THIRD_PARTY_NOTICES.md",
            "RKC-owned source code Apache-2.0\n"
            "Go runtime and standard library BSD-3-Clause LICENSES/Go.txt\n"
            "modernc.org/sqlite v1.54.0 modernc.org/libc v1.74.1\n"
            "third_party/go-modules.lock.json LICENSES/go-modules/\n"
            "do not bundle model weights\nllama.cpp is MIT licensed\n"
            "Qwen3.5-2B Qwen3-Embedding-0.6B models/models.lock.json\n",
        )
        self.write("go.sum", "fixture.test/module v1.0.0 h1:fixture\n")
        self.write("third_party/go-modules.lock.json", "{}\n")

    def dependency_fixture(
        self,
    ) -> tuple[dict[str, dict[str, object]], dict[str, str]]:
        """Write a complete two-module dependency-governance fixture."""
        license_values = {
            "example.test/lib": {"LICENSE": "fixture library license\n"},
            "example.test/sqlite": {
                "LICENSE": "fixture sqlite driver license\n",
                "SQLITE-LICENSE": "fixture sqlite public-domain notice\n",
            },
        }
        expected_modules: dict[str, dict[str, object]] = {
            "example.test/lib": {
                "version": "v1.2.3",
                "module_sum": "h1:GzkhY7T5VNhEkwH0PVJgjz+fX1rhBrR7pRT3mDkpeCY=",
                "go_mod_sum": "h1:Mu1zIs6XwVuF/gI1OepvI0qD18qycQx+mFykh5fBlto=",
                "license_spdx": "BSD-3-Clause",
                "licenses": {},
            },
            "example.test/sqlite": {
                "version": "v4.5.6",
                "module_sum": "h1:JCxR4qwkJvOaqAoYcgDoO25Nc+ROg6EJ2LfBVzdrgog=",
                "go_mod_sum": "h1:4ntCLuNmnH8+GNqjka1wNg7KJd5/Hi5FYp8K+XQ7GZw=",
                "license_spdx": "BSD-3-Clause",
                "licenses": {},
            },
        }
        for module_path, source_files in license_values.items():
            expected = expected_modules[module_path]
            version = str(expected["version"])
            license_hashes: dict[str, str] = {}
            for source_path, value in source_files.items():
                relative = f"LICENSES/go-modules/{module_path}@{version}/{source_path}"
                self.write(relative, value)
                license_hashes[source_path] = hashlib.sha256(
                    value.encode("utf-8")
                ).hexdigest()
            expected["licenses"] = license_hashes

        roots = {"example.test/sqlite": "v4.5.6"}
        modules: list[dict[str, object]] = []
        notice_lines = ["third_party/go-modules.lock.json"]
        sum_lines: list[str] = []
        for module_path, expected in sorted(expected_modules.items()):
            version = str(expected["version"])
            licenses = [
                {
                    "source_path": source_path,
                    "path": (
                        f"LICENSES/go-modules/{module_path}@{version}/{source_path}"
                    ),
                    "sha256": digest,
                }
                for source_path, digest in sorted(dict(expected["licenses"]).items())
            ]
            modules.append(
                {
                    "path": module_path,
                    "version": version,
                    "direct": module_path in roots,
                    "module_sum": expected["module_sum"],
                    "go_mod_sum": expected["go_mod_sum"],
                    "source_url": (
                        f"https://proxy.golang.org/{module_path}/@v/{version}.zip"
                    ),
                    "license_spdx": expected["license_spdx"],
                    "licenses": licenses,
                    "notice_path": "THIRD_PARTY_NOTICES.md",
                }
            )
            notice_lines.append(f"{module_path} {version}")
            notice_lines.extend(str(item["path"]) for item in licenses)
            sum_lines.extend(
                (
                    f"{module_path} {version} {expected['module_sum']}",
                    f"{module_path} {version}/go.mod {expected['go_mod_sum']}",
                )
            )

        lock = {
            "schema_version": "1.0",
            "go": {"directive": "1.25.0", "toolchain": "go1.26.5"},
            "root_requirements": [
                {"path": path, "version": version}
                for path, version in sorted(roots.items())
            ],
            "modules": modules,
        }
        self.write(
            "go.mod",
            "module fixture.test/rkc\n\n"
            "go 1.25.0\n\n"
            "toolchain go1.26.5\n\n"
            "require (\n"
            "\texample.test/lib v1.2.3 // indirect\n"
            "\texample.test/sqlite v4.5.6\n"
            ")\n",
        )
        self.write("go.sum", "\n".join(sorted(sum_lines)) + "\n")
        self.write(
            "third_party/go-modules.lock.json",
            json.dumps(lock, indent=2) + "\n",
        )
        self.write("THIRD_PARTY_NOTICES.md", "\n".join(notice_lines) + "\n")
        return expected_modules, roots

    def validate_dependency_fixture(
        self,
        expected_modules: dict[str, dict[str, object]],
        roots: dict[str, str],
        compatibility_sums: dict[tuple[str, str], str] | None = None,
    ) -> dict[str, object]:
        """Run dependency validation with the fixture's pinned policy."""
        LICENSES.ERRORS.clear()
        LICENSES.CHECKS.clear()
        with mock.patch.multiple(
            LICENSES,
            EXPECTED_MODULE_PATH="fixture.test/rkc",
            EXPECTED_GO_DIRECTIVE="1.25.0",
            EXPECTED_TOOLCHAIN="go1.26.5",
            EXPECTED_ROOT_REQUIREMENTS=roots,
            EXPECTED_EXPLICIT_REQUIREMENTS={
                "example.test/lib": "v1.2.3",
                "example.test/sqlite": "v4.5.6",
            },
            EXPECTED_GO_MOD_COMPATIBILITY_SUMS=(compatibility_sums or {}),
            EXPECTED_MODULES=expected_modules,
        ):
            LICENSES.validate_dependency_boundary()
        return LICENSES.CHECKS[-1]

    def test_read_regular_enforces_type_size_and_utf8(self) -> None:
        self.assertIsNone(LICENSES.read_regular(Path("missing")))
        regular = self.write("regular", "hello")
        self.assertEqual(LICENSES.read_regular(Path("regular")), "hello")
        self.assertIsNone(LICENSES.read_regular(Path("regular"), maximum_bytes=1))
        regular.write_bytes(b"\xff")
        self.assertIsNone(LICENSES.read_regular(Path("regular")))
        if hasattr(os, "symlink"):
            self.write("target", "safe")
            (LICENSES.ROOT / "link").symlink_to(LICENSES.ROOT / "target")
            self.assertIsNone(LICENSES.read_regular(Path("link")))

    def test_require_markers_records_missing_and_accepts_none(self) -> None:
        LICENSES.require_markers("none", None, ("x",))
        self.assertEqual(LICENSES.CHECKS, [])
        LICENSES.require_markers("marker", "alpha", ("alpha", "beta"))
        self.assertIn("beta", LICENSES.ERRORS[-1])
        LICENSES.require_markers("complete", "alpha beta", ("alpha", "beta"))
        self.assertTrue(LICENSES.CHECKS[-1]["ok"])

    def test_strict_go_parsers_and_json_shape_helpers_reject_ambiguity(self) -> None:
        failures: list[str] = []
        self.assertIsNone(
            LICENSES.require_exact_keys(
                [], frozenset({"required"}), "fixture", failures
            )
        )
        self.assertIsNone(
            LICENSES.require_exact_keys(
                {"unknown": True}, frozenset({"required"}), "fixture", failures
            )
        )
        self.assertEqual(
            LICENSES.require_exact_keys(
                {"required": True}, frozenset({"required"}), "fixture", failures
            ),
            {"required": True},
        )
        self.assertIn("must be an object", failures[0])
        self.assertIn("keys differ", failures[1])
        with self.assertRaisesRegex(ValueError, "duplicate JSON key"):
            LICENSES.reject_duplicate_keys([("key", 1), ("key", 2)])

        metadata, requirements, failures = LICENSES.parse_go_mod(
            "module fixture.test/one\n"
            "module fixture.test/two\n"
            "go 1.25.0\n"
            "toolchain go1.26.5\n"
            "require example.test/inline v1.0.0\n"
            "require example.test/inline v2.0.0\n"
            "replace example.test/inline => ../local\n"
            "require (\n"
            "malformed\n"
            "example.test/block v1.0.0\n"
            "example.test/block v2.0.0\n"
        )
        self.assertEqual(metadata["module"], "fixture.test/two")
        self.assertEqual(requirements["example.test/inline"], "v2.0.0")
        self.assertEqual(requirements["example.test/block"], "v2.0.0")
        for marker in (
            "duplicate module directive",
            "duplicate requirement example.test/inline",
            "prohibited or invalid directive",
            "invalid require entry",
            "duplicate requirement example.test/block",
            "unterminated require block",
        ):
            self.assertTrue(any(marker in failure for failure in failures), failures)

        entries, failures = LICENSES.parse_go_sum(
            "z.test/module v1.0.0 h1:fixture\n"
            "malformed\n"
            "a.test/module v1.0.0 h1:first\n"
            "a.test/module v1.0.0 h1:second\n"
        )
        self.assertEqual(entries[("a.test/module", "v1.0.0")], "h1:second")
        self.assertTrue(any("not sorted" in failure for failure in failures))
        self.assertTrue(any("expected three fields" in failure for failure in failures))
        self.assertTrue(
            any("duplicate a.test/module" in failure for failure in failures)
        )

    def test_root_documents_happy_path_and_notice_closure(self) -> None:
        self.root_fixture()
        LICENSES.validate_root_documents()
        self.assertFalse(LICENSES.ERRORS, LICENSES.ERRORS)
        self.write("LICENSES/Extra.txt", "terms")
        LICENSES.validate_root_documents()
        self.assertTrue(
            any("LICENSES/Extra.txt" in error for error in LICENSES.ERRORS),
            LICENSES.ERRORS,
        )

    def test_declared_metadata_happy_and_invalid_paths(self) -> None:
        self.write(
            "api/openapi.yaml",
            "license:\n  name: Apache-2.0\n  identifier: Apache-2.0\n",
        )
        self.write(
            "plugins/official/plugin.json",
            json.dumps({"plugin": {"id": "rkc.official", "license": "Apache-2.0"}}),
        )
        self.write(
            "plugins/external/plugin.json",
            json.dumps({"plugin": {"id": "vendor.plugin", "license": "MIT"}}),
        )
        self.write(
            "models/models.lock.json",
            json.dumps(
                {
                    "llama_cpp": {"license_spdx": "MIT"},
                    "assets": [
                        {
                            "id": "source",
                            "kind": "source-archive",
                            "license_spdx": "MIT",
                        },
                        {
                            "id": "model",
                            "kind": "generation-model",
                            "license_spdx": "Apache-2.0",
                        },
                    ],
                }
            ),
        )
        LICENSES.validate_declared_metadata()
        self.assertFalse(LICENSES.ERRORS, LICENSES.ERRORS)

        LICENSES.ERRORS.clear()
        self.write(
            "plugins/official/plugin.json",
            json.dumps({"plugin": {"id": "rkc.official", "license": "MIT OR GPL"}}),
        )
        self.write("plugins/broken/plugin.json", "{")
        self.write(
            "models/models.lock.json",
            json.dumps(
                {
                    "llama_cpp": {"license_spdx": "Apache-2.0"},
                    "assets": [
                        {
                            "id": "source",
                            "kind": "source-archive",
                            "license_spdx": "Apache-2.0",
                        },
                        {
                            "id": "model",
                            "kind": "embedding-model",
                            "license_spdx": "MIT",
                        },
                    ],
                }
            ),
        )
        LICENSES.validate_declared_metadata()
        self.assertGreaterEqual(len(LICENSES.ERRORS), 2)

    def test_dependency_boundary_accepts_exact_reviewed_closure(self) -> None:
        expected, roots = self.dependency_fixture()
        result = self.validate_dependency_fixture(expected, roots)
        self.assertTrue(result["ok"], result["detail"])

    def test_dependency_boundary_rejects_unknown_missing_and_version_drift(
        self,
    ) -> None:
        expected, roots = self.dependency_fixture()
        self.write(
            "go.mod",
            (LICENSES.ROOT / "go.mod").read_text(encoding="utf-8")
            + "require unknown.test/module v1.0.0\n",
        )
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("unknown module", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        lock_path = LICENSES.ROOT / "third_party/go-modules.lock.json"
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        lock["modules"].pop(0)
        self.write("third_party/go-modules.lock.json", json.dumps(lock))
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("missing governed modules", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        self.write(
            "go.mod",
            "module fixture.test/rkc\n"
            "go 1.25.0\n"
            "toolchain go1.26.5\n"
            "require example.test/sqlite v9.9.9\n",
        )
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("version drift", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        self.write(
            "go.mod",
            "module fixture.test/rkc\n"
            "go 1.25.0\n"
            "toolchain go1.26.5\n"
            "require example.test/sqlite v4.5.6\n",
        )
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("missing explicit requirement", str(result["detail"]))

    def test_dependency_boundary_rejects_go_sum_drift_and_absence(self) -> None:
        expected, roots = self.dependency_fixture()
        go_sum = (LICENSES.ROOT / "go.sum").read_text(encoding="utf-8")
        self.write("go.sum", go_sum.replace("h1:Gzkh", "h1:Azkh", 1))
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("checksum drift", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        (LICENSES.ROOT / "go.sum").unlink()
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("missing or unreadable", str(result["detail"]))

    def test_dependency_boundary_go_mod_compatibility_sum_is_exact(self) -> None:
        expected, roots = self.dependency_fixture()
        key = ("example.test/legacy", "v0.1.0/go.mod")
        digest = "h1:oPkhp1MJrh7nUepCBck5+mAzfO9JrbApNNgaTdGDITg="
        path = LICENSES.ROOT / "go.sum"
        lines = path.read_text(encoding="utf-8").splitlines()
        lines.append(f"{key[0]} {key[1]} {digest}")
        path.write_text("\n".join(sorted(lines)) + "\n", encoding="utf-8")

        result = self.validate_dependency_fixture(
            expected, roots, compatibility_sums={key: digest}
        )
        self.assertTrue(result["ok"], result)

        path.write_text(
            path.read_text(encoding="utf-8").replace(digest, "h1:" + "A" * 43 + "="),
            encoding="utf-8",
        )
        result = self.validate_dependency_fixture(
            expected, roots, compatibility_sums={key: digest}
        )
        self.assertFalse(result["ok"])
        self.assertIn("checksum drift", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        path = LICENSES.ROOT / "go.sum"
        lines = path.read_text(encoding="utf-8").splitlines()
        lines.append(f"{key[0]} {key[1]} {digest}")
        path.write_text("\n".join(sorted(lines)) + "\n", encoding="utf-8")
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("unknown module entry", str(result["detail"]))

    def test_dependency_boundary_rejects_license_hash_path_and_absence(self) -> None:
        expected, roots = self.dependency_fixture()
        license_path = (
            LICENSES.ROOT / "LICENSES/go-modules/example.test/lib@v1.2.3/LICENSE"
        )
        license_path.write_text("tampered\n", encoding="utf-8")
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("license file hash drift", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        lock_path = LICENSES.ROOT / "third_party/go-modules.lock.json"
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        lock["modules"][0]["licenses"][0]["path"] += ".moved"
        self.write("third_party/go-modules.lock.json", json.dumps(lock))
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("license path drift", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        license_path.unlink()
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("missing governed license file", str(result["detail"]))

    def test_dependency_boundary_rejects_missing_notice_and_ambiguous_lock(
        self,
    ) -> None:
        expected, roots = self.dependency_fixture()
        self.write("THIRD_PARTY_NOTICES.md", "third_party/go-modules.lock.json\n")
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("notice omits", str(result["detail"]))

        expected, roots = self.dependency_fixture()
        lock = (LICENSES.ROOT / "third_party/go-modules.lock.json").read_text(
            encoding="utf-8"
        )
        self.write(
            "third_party/go-modules.lock.json",
            lock.replace(
                '"schema_version": "1.0",',
                '"schema_version": "1.0",\n  "schema_version": "1.0",',
                1,
            ),
        )
        result = self.validate_dependency_fixture(expected, roots)
        self.assertFalse(result["ok"])
        self.assertIn("duplicate JSON key", str(result["detail"]))

    def test_dependency_boundary_rejects_lock_schema_and_entry_shapes(self) -> None:
        expected, roots = self.dependency_fixture()
        lock_path = LICENSES.ROOT / "third_party/go-modules.lock.json"
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        lock["schema_version"] = "2.0"
        lock["go"] = {"directive": "9.9.9", "toolchain": "go9.9.9"}
        lock["root_requirements"] = []
        lock["modules"] = {"not": "an array"}
        self.write("third_party/go-modules.lock.json", json.dumps(lock))
        result = self.validate_dependency_fixture(expected, roots)
        detail = str(result["detail"])
        for marker in (
            "schema_version must be 1.0",
            "toolchain metadata drift",
            "root requirements drift",
            "modules must be an array",
            "missing governed modules",
        ):
            self.assertIn(marker, detail)

        expected, roots = self.dependency_fixture()
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        lock["modules"].insert(0, {"path": "missing-fields"})
        lock["modules"].append(
            {
                "path": 7,
                "version": None,
                "direct": False,
                "module_sum": "invalid",
                "go_mod_sum": "invalid",
                "source_url": "invalid",
                "license_spdx": "invalid",
                "licenses": [],
                "notice_path": "invalid",
            }
        )
        self.write("third_party/go-modules.lock.json", json.dumps(lock))
        result = self.validate_dependency_fixture(expected, roots)
        self.assertIn("keys differ", str(result["detail"]))
        self.assertIn("identity is invalid", str(result["detail"]))

    def test_dependency_boundary_rejects_license_entry_policy_drift(self) -> None:
        expected, roots = self.dependency_fixture()
        lock_path = LICENSES.ROOT / "third_party/go-modules.lock.json"
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        module = lock["modules"][0]
        original = dict(module["licenses"][0])
        module["direct"] = True
        module["module_sum"] = "invalid"
        module["go_mod_sum"] = "h1:" + "A" * 43 + "="
        module["source_url"] = "https://invalid.example/module.zip"
        module["license_spdx"] = "GPL-3.0-only"
        module["notice_path"] = "OTHER.md"
        module["licenses"] = [
            {"source_path": 7, "path": None, "sha256": False},
            {
                "source_path": "../LICENSE",
                "path": original["path"],
                "sha256": original["sha256"],
            },
            {
                "source_path": "UNKNOWN",
                "path": original["path"],
                "sha256": original["sha256"],
            },
            original,
            original,
        ]
        self.write("third_party/go-modules.lock.json", json.dumps(lock))
        result = self.validate_dependency_fixture(expected, roots)
        detail = str(result["detail"])
        for marker in (
            "direct flag drift",
            "invalid module_sum",
            "module_sum drift",
            "go_mod_sum drift",
            "source URL drift",
            "SPDX drift",
            "notice path drift",
            "license fields invalid",
            "unsafe upstream license path",
            "unknown upstream license",
            "duplicates example.test/lib/LICENSE",
            "duplicate tracked license path",
        ):
            self.assertIn(marker, detail)

    @mock.patch.object(LICENSES.subprocess, "run")
    def test_tracked_artifact_policy_accepts_regular_and_reports_all_failures(
        self, run: mock.Mock
    ) -> None:
        run.return_value = subprocess.CompletedProcess(
            [], 0, stdout=index_entry("100644", "README.md"), stderr=b""
        )
        LICENSES.validate_tracked_artifacts()
        self.assertTrue(LICENSES.CHECKS[-1]["ok"])

        malformed = b"broken\0" + b"100644 " + b"0" * 40 + b" 0\t\xff\0"
        run.return_value = subprocess.CompletedProcess(
            [],
            0,
            stdout=(
                index_entry("100644", "conflict", "2")
                + index_entry("120000", "link")
                + index_entry("160000", "module")
                + index_entry("100644", "weights.gguf")
                + malformed
            ),
            stderr=b"",
        )
        LICENSES.validate_tracked_artifacts()
        detail = str(LICENSES.CHECKS[-1]["detail"])
        for marker in (
            "unmerged",
            "symlink",
            "submodule",
            "model/native",
            "unportable",
        ):
            self.assertIn(marker, detail)

        run.return_value = subprocess.CompletedProcess(
            [], 9, stdout=b"", stderr=b"git failed"
        )
        LICENSES.validate_tracked_artifacts()
        self.assertIn("git failed", str(LICENSES.CHECKS[-1]["detail"]))

    def test_main_returns_machine_readable_status(self) -> None:
        def good_check() -> None:
            LICENSES.record("fixture", True, "ok")

        with mock.patch.object(
            LICENSES, "validate_root_documents", good_check
        ), mock.patch.object(
            LICENSES, "validate_declared_metadata", good_check
        ), mock.patch.object(
            LICENSES, "validate_dependency_boundary", good_check
        ), mock.patch.object(
            LICENSES, "validate_tracked_artifacts", good_check
        ), mock.patch(
            "sys.stdout", new=io.StringIO()
        ) as output:
            self.assertEqual(LICENSES.main(), 0)
            self.assertTrue(json.loads(output.getvalue())["ok"])

        LICENSES.ERRORS.clear()
        LICENSES.CHECKS.clear()
        with mock.patch.object(
            LICENSES,
            "validate_root_documents",
            side_effect=lambda: LICENSES.record("fixture", False, "bad"),
        ), mock.patch.object(LICENSES, "validate_declared_metadata"), mock.patch.object(
            LICENSES, "validate_dependency_boundary"
        ), mock.patch.object(
            LICENSES, "validate_tracked_artifacts"
        ), mock.patch(
            "sys.stdout", new=io.StringIO()
        ):
            self.assertEqual(LICENSES.main(), 1)


if __name__ == "__main__":
    unittest.main()
