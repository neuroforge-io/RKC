#!/usr/bin/env python3
"""Failure-oriented unit tests for the release license validator."""
from __future__ import annotations

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
            "do not bundle model weights\nllama.cpp is MIT licensed\n"
            "Qwen3.5-2B Qwen3-Embedding-0.6B models/models.lock.json\n",
        )

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
                        {"id": "source", "kind": "source-archive", "license_spdx": "MIT"},
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
                        {"id": "model", "kind": "embedding-model", "license_spdx": "MIT"},
                    ],
                }
            ),
        )
        LICENSES.validate_declared_metadata()
        self.assertGreaterEqual(len(LICENSES.ERRORS), 2)

    def test_dependency_boundary_detects_require_directives(self) -> None:
        self.write("go.mod", "module fixture\n\ngo 1.26\n")
        LICENSES.validate_dependency_boundary()
        self.assertTrue(LICENSES.CHECKS[-1]["ok"])
        self.write("go.mod", "module fixture\nrequire example.test/module v1.0.0 // comment\n")
        LICENSES.validate_dependency_boundary()
        self.assertFalse(LICENSES.CHECKS[-1]["ok"])
        (LICENSES.ROOT / "go.mod").unlink()
        count = len(LICENSES.CHECKS)
        LICENSES.validate_dependency_boundary()
        self.assertEqual(len(LICENSES.CHECKS), count + 1)  # missing-file record only

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
        for marker in ("unmerged", "symlink", "submodule", "model/native", "unportable"):
            self.assertIn(marker, detail)

        run.return_value = subprocess.CompletedProcess([], 9, stdout=b"", stderr=b"git failed")
        LICENSES.validate_tracked_artifacts()
        self.assertIn("git failed", str(LICENSES.CHECKS[-1]["detail"]))

    def test_main_returns_machine_readable_status(self) -> None:
        def good_check() -> None:
            LICENSES.record("fixture", True, "ok")

        with mock.patch.object(LICENSES, "validate_root_documents", good_check), mock.patch.object(
            LICENSES, "validate_declared_metadata", good_check
        ), mock.patch.object(LICENSES, "validate_dependency_boundary", good_check), mock.patch.object(
            LICENSES, "validate_tracked_artifacts", good_check
        ), mock.patch("sys.stdout", new=io.StringIO()) as output:
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
        ), mock.patch.object(LICENSES, "validate_tracked_artifacts"), mock.patch(
            "sys.stdout", new=io.StringIO()
        ):
            self.assertEqual(LICENSES.main(), 1)


if __name__ == "__main__":
    unittest.main()
