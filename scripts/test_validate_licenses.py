#!/usr/bin/env python3
"""Failure-oriented unit tests for the release license validator."""
from __future__ import annotations

import hashlib
import importlib.util
import io
import json
import os
import runpy
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


MODULE_NAME = "rkc_validate_licenses"
VALIDATOR = Path(__file__).with_name("validate-licenses.py").absolute()
SPEC = importlib.util.spec_from_file_location(
    MODULE_NAME, VALIDATOR
)
assert SPEC and SPEC.loader
LICENSES = importlib.util.module_from_spec(SPEC)
sys.modules[MODULE_NAME] = LICENSES
SPEC.loader.exec_module(LICENSES)

ATTRIBUTION_FOOTER = LICENSES.ATTRIBUTION_FOOTER + "\n"


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
            "MIT License\nPermission is hereby granted\n"
            "THE SOFTWARE IS PROVIDED\n",
        )
        self.write(
            "NOTICE",
            "Repository Knowledge Compiler (RKC)\n"
            "Copyright (c) 2026 NeuroForgeIO and RKC contributors\n",
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
            "RKC-owned source code MIT License NeuroForgeIO commercial products\n"
            "Go runtime and standard library BSD-3-Clause LICENSES/Go.txt\n"
            "modernc.org/sqlite v1.54.0 modernc.org/libc v1.74.1\n"
            "gopkg.in/yaml.v3 v3.0.1\n"
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
        self.write("LICENSES/empty-directory/.keep", "temporary")
        (LICENSES.ROOT / "LICENSES/empty-directory/.keep").unlink()
        LICENSES.validate_root_documents()
        self.assertFalse(LICENSES.ERRORS, LICENSES.ERRORS)
        self.write("LICENSES/Extra.txt", "terms")
        LICENSES.validate_root_documents()
        self.assertTrue(
            any("LICENSES/Extra.txt" in error for error in LICENSES.ERRORS),
            LICENSES.ERRORS,
        )

    def test_root_documents_reject_missing_license_directory(self) -> None:
        self.root_fixture()
        (LICENSES.ROOT / "LICENSES/Go.txt").unlink()
        (LICENSES.ROOT / "LICENSES").rmdir()

        LICENSES.validate_root_documents()

        self.assertTrue(
            any("license-file notice closure" in error for error in LICENSES.ERRORS),
            LICENSES.ERRORS,
        )

    def test_markdown_attribution_requires_footer_and_excludes_boundaries(
        self,
    ) -> None:
        self.write("README.md", "# RKC\n\n" + ATTRIBUTION_FOOTER)
        self.write("docs/guide.md", "# Guide\n\n" + ATTRIBUTION_FOOTER)
        self.write("THIRD_PARTY_NOTICES.md", "mixed-source inventory\n")
        self.write("LICENSES/vendor/LICENSE.md", "upstream terms\n")
        self.write("third_party/vendor/NOTICE.md", "upstream notice\n")
        self.write("vendor/upstream/README.md", "vendored documentation\n")
        self.write("dist/generated/README.md", "generated output\n")
        self.write(".rkc-output/docs/README.md", "generated output\n")

        LICENSES.validate_markdown_attribution()

        self.assertTrue(LICENSES.CHECKS[-1]["ok"], LICENSES.CHECKS[-1])
        self.write("docs/missing.md", "# Missing\n")
        LICENSES.validate_markdown_attribution()
        self.assertFalse(LICENSES.CHECKS[-1]["ok"])
        self.assertIn("docs/missing.md", str(LICENSES.CHECKS[-1]["detail"]))

    def test_markdown_attribution_rejects_empty_inventory_and_nonregular_file(
        self,
    ) -> None:
        LICENSES.validate_markdown_attribution()
        self.assertFalse(LICENSES.CHECKS[-1]["ok"])
        self.assertIn("no first-party Markdown", LICENSES.CHECKS[-1]["detail"])

        self.write("README.md", "# RKC\n\n" + ATTRIBUTION_FOOTER)
        if hasattr(os, "symlink"):
            (LICENSES.ROOT / "docs").mkdir()
            (LICENSES.ROOT / "docs" / "linked.md").symlink_to(
                LICENSES.ROOT / "README.md"
            )
            LICENSES.validate_markdown_attribution()
            self.assertFalse(LICENSES.CHECKS[-1]["ok"])
            self.assertIn("must be a regular file", LICENSES.CHECKS[-1]["detail"])

    def test_markdown_attribution_rejects_oversize_and_non_utf8_text(self) -> None:
        readme = self.write("README.md", "# RKC\n\n" + ATTRIBUTION_FOOTER)
        original_lstat = os.lstat

        def oversized_lstat(path: str | bytes | os.PathLike[str]) -> mock.Mock:
            info = original_lstat(path)
            return mock.Mock(st_mode=info.st_mode, st_size=8 * 1024 * 1024 + 1)

        with mock.patch.object(
            LICENSES.os, "lstat", side_effect=oversized_lstat
        ):
            LICENSES.validate_markdown_attribution()
        self.assertFalse(LICENSES.CHECKS[-1]["ok"])
        self.assertIn("exceeds 8388608 bytes", LICENSES.CHECKS[-1]["detail"])

        readme.write_bytes(b"\xff")
        LICENSES.validate_markdown_attribution()
        self.assertFalse(LICENSES.CHECKS[-1]["ok"])
        self.assertIn("cannot read UTF-8 text", LICENSES.CHECKS[-1]["detail"])

    def test_attribution_language_rejects_extra_mandatory_credit(self) -> None:
        surface = Path("surface.md")
        with mock.patch.object(
            LICENSES, "ATTRIBUTION_LANGUAGE_FILES", (surface,)
        ):
            for wording in (
                "MIT requires retention of its copyright and permission notice. "
                "NeuroForgeIO credit is requested, not an additional condition.\n",
                "NeuroForgeIO requests that redistributions retain NOTICE and "
                "credit RKC contributors.\n",
                "NOTICE retention and NeuroForgeIO credit are requested; neither "
                "is a license condition.\n",
                "Users are not required to credit NeuroForgeIO.\n",
                "No user must credit NeuroForgeIO.\n",
                "RKC is MIT-licensed with optional attribution.\n",
                "Attribution to NeuroForgeIO is voluntary.\n",
                "Users do not have to credit NeuroForgeIO.\n",
                "Users do not need to credit NeuroForgeIO.\n",
                "Users are not required to retain NOTICE.\n",
                "No user must retain NOTICE.\n",
                "RKC is MIT-licensed with attribution, which is not required.\n",
                "Crediting NeuroForgeIO is not necessary.\n",
                "Acknowledging NeuroForgeIO is entirely discretionary.\n",
            ):
                self.write(str(surface), wording)
                LICENSES.validate_attribution_language()
                self.assertTrue(LICENSES.CHECKS[-1]["ok"], wording)

            for wording in (
                "MIT-licensed open source with attribution.\n",
                "RKC is MIT-licensed with simple attribution.\n",
                "Retain NOTICE when redistributing.\n",
                "Retain NOTICE and credit NeuroForgeIO in redistributions.\n",
                "Users must provide attribution to NeuroForgeIO.\n",
                "Attribution to NeuroForgeIO is mandatory.\n",
                "NeuroForgeIO credit is required.\n",
                "The MIT license requires NeuroForgeIO credit.\n",
                "Commercial use is permitted provided that users credit "
                "NeuroForgeIO.\n",
                "Redistributors need to credit NeuroForgeIO.\n",
                "Redistributors have to credit NeuroForgeIO.\n",
                "Attribution to NeuroForgeIO is obligatory.\n",
                "Users are obliged to credit NeuroForgeIO.\n",
                "Users are obligated to credit NeuroForgeIO.\n",
                "Commercial use requires crediting NeuroForgeIO.\n",
                "Users must acknowledge NeuroForgeIO.\n",
                "Attribution to NeuroForgeIO is compulsory.\n",
                "NeuroForgeIO credit is a prerequisite.\n",
                "Use is prohibited unless NeuroForgeIO is credited.\n",
                "Commercial use is allowed only if you credit NeuroForgeIO.\n",
                "Commercial products must display the NeuroForgeIO name.\n",
                "Redistribution is permitted only after naming NeuroForgeIO.\n",
                "Users are obliged to mention NeuroForgeIO.\n",
                "NeuroForgeIO credit is requested. Retain NOTICE and credit "
                "NeuroForgeIO.\n",
                "Attribution to NeuroForgeIO is requested, but users must credit "
                "NeuroForgeIO.\n",
            ):
                self.write(str(surface), wording)
                LICENSES.validate_attribution_language()
                self.assertFalse(LICENSES.CHECKS[-1]["ok"], wording)

    def test_attribution_language_scans_first_party_markdown_only(self) -> None:
        with mock.patch.object(LICENSES, "ATTRIBUTION_LANGUAGE_FILES", ()):
            self.write(
                "THIRD_PARTY_NOTICES.md",
                "Upstream users must retain NOTICE and provide attribution.\n",
            )
            LICENSES.validate_attribution_language()
            self.assertTrue(LICENSES.CHECKS[-1]["ok"])

            self.write(
                "docs/public.md",
                "RKC is MIT-licensed with simple attribution.\n",
            )
            LICENSES.validate_attribution_language()
            self.assertFalse(LICENSES.CHECKS[-1]["ok"])
            self.assertIn("docs/public.md:1", LICENSES.CHECKS[-1]["detail"])

    def test_declared_metadata_happy_and_invalid_paths(self) -> None:
        self.write(
            "api/openapi.yaml",
            "license:\n  name: MIT\n  identifier: MIT\n",
        )
        self.write(
            "plugins/official/plugin.json",
            json.dumps({"plugin": {"id": "rkc.official", "license": "MIT"}}),
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

    def test_declared_metadata_rejects_missing_and_malformed_model_lock(self) -> None:
        self.write(
            "api/openapi.yaml",
            "license:\n  name: MIT\n  identifier: MIT\n",
        )

        LICENSES.validate_declared_metadata()
        self.assertIn(
            "optional model/runtime license metadata",
            str(LICENSES.CHECKS[-1]["name"]),
        )
        self.assertFalse(LICENSES.CHECKS[-1]["ok"])

        LICENSES.ERRORS.clear()
        LICENSES.CHECKS.clear()
        self.write(
            "models/models.lock.json",
            json.dumps({"llama_cpp": None, "assets": []}),
        )
        LICENSES.validate_declared_metadata()
        self.assertFalse(LICENSES.CHECKS[-1]["ok"])
        self.assertIn("invalid model lock", str(LICENSES.CHECKS[-1]["detail"]))

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

    def test_dependency_boundary_rejects_module_and_license_collection_drift(
        self,
    ) -> None:
        expected, roots = self.dependency_fixture()
        go_mod_path = LICENSES.ROOT / "go.mod"
        self.write(
            "go.mod",
            go_mod_path.read_text(encoding="utf-8").replace(
                "module fixture.test/rkc", "module fixture.test/wrong", 1
            ),
        )
        lock_path = LICENSES.ROOT / "third_party/go-modules.lock.json"
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        first = lock["modules"][0]
        first["version"] = "v9.9.9"
        first["licenses"] = {"not": "an array"}
        duplicate = dict(first)
        unknown = dict(first)
        unknown["path"] = "unknown.test/module"
        lock["modules"].insert(1, duplicate)
        lock["modules"].append(unknown)
        del lock["modules"][2]["licenses"][0]["sha256"]
        self.write("third_party/go-modules.lock.json", json.dumps(lock))

        result = self.validate_dependency_fixture(expected, roots)

        detail = str(result["detail"])
        for marker in (
            "go.mod metadata drift",
            "version drift",
            "licenses must be an array",
            "duplicates module",
            "keys differ",
            "contains unknown module",
        ):
            self.assertIn(marker, detail)

    def test_dependency_boundary_rejects_invalid_internal_policy_metadata(
        self,
    ) -> None:
        expected, roots = self.dependency_fixture()
        expected["example.test/lib"]["licenses"] = []
        expected["example.test/lib"]["module_sum"] = 7

        result = self.validate_dependency_fixture(expected, roots)

        detail = str(result["detail"])
        self.assertIn("internal expected-license metadata invalid", detail)
        self.assertIn("internal expected-module metadata invalid", detail)

    def test_dependency_boundary_rejects_hash_notice_sum_and_license_root_drift(
        self,
    ) -> None:
        expected, roots = self.dependency_fixture()
        lock_path = LICENSES.ROOT / "third_party/go-modules.lock.json"
        lock = json.loads(lock_path.read_text(encoding="utf-8"))
        lock["modules"][0]["licenses"][0]["sha256"] = "invalid"
        self.write("third_party/go-modules.lock.json", json.dumps(lock))

        notice_path = LICENSES.ROOT / "THIRD_PARTY_NOTICES.md"
        self.write(
            "THIRD_PARTY_NOTICES.md",
            notice_path.read_text(encoding="utf-8").replace(
                "third_party/go-modules.lock.json", "lock inventory", 1
            ),
        )
        license_root = LICENSES.ROOT / "LICENSES/go-modules"
        license_root.rename(LICENSES.ROOT / "LICENSES/go-modules-away")

        go_sum_path = LICENSES.ROOT / "go.sum"
        lines = go_sum_path.read_text(encoding="utf-8").splitlines()
        lines = [
            line
            for line in lines
            if not line.startswith("example.test/lib v1.2.3 h1:")
        ]
        lines[0] = lines[0].rsplit(" ", 1)[0] + " invalid"
        self.write("go.sum", "\n".join(sorted(lines)) + "\n")

        result = self.validate_dependency_fixture(expected, roots)

        detail = str(result["detail"])
        for marker in (
            "license hash metadata drift",
            "notice omits third_party/go-modules.lock.json",
            "tracked Go module license set differs from lock",
            "go.sum is missing entries",
            "go.sum has invalid checksum",
        ):
            self.assertIn(marker, detail)

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
            LICENSES, "validate_markdown_attribution", good_check
        ), mock.patch.object(
            LICENSES, "validate_attribution_language", good_check
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
        ), mock.patch.object(
            LICENSES, "validate_markdown_attribution"
        ), mock.patch.object(
            LICENSES, "validate_attribution_language"
        ), mock.patch.object(LICENSES, "validate_declared_metadata"), mock.patch.object(
            LICENSES, "validate_dependency_boundary"
        ), mock.patch.object(
            LICENSES, "validate_tracked_artifacts"
        ), mock.patch(
            "sys.stdout", new=io.StringIO()
        ):
            self.assertEqual(LICENSES.main(), 1)

    def test_script_entrypoint_returns_machine_readable_failure(self) -> None:
        self.root_fixture()
        original_resolve = Path.resolve
        synthetic_script = LICENSES.ROOT / "scripts/validate-licenses.py"

        def controlled_resolve(
            path: Path, *args: object, **kwargs: object
        ) -> Path:
            if path.absolute() == VALIDATOR:
                return synthetic_script
            return original_resolve(path, *args, **kwargs)

        stdout = io.StringIO()
        with mock.patch.object(Path, "resolve", controlled_resolve), mock.patch(
            "sys.stdout", new=stdout
        ):
            with self.assertRaises(SystemExit) as raised:
                runpy.run_path(str(VALIDATOR), run_name="__main__")

        self.assertEqual(raised.exception.code, 1)
        report = json.loads(stdout.getvalue())
        self.assertFalse(report["ok"])
        self.assertTrue(report["errors"])


if __name__ == "__main__":
    unittest.main()
