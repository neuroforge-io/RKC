#!/usr/bin/env python3
"""Failure-oriented tests for deterministic Go binary SPDX generation."""
from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

MODULE_NAME = "rkc_generate_go_sbom"
SPEC = importlib.util.spec_from_file_location(
    MODULE_NAME, Path(__file__).with_name("generate-go-sbom.py")
)
assert SPEC and SPEC.loader
SBOM = importlib.util.module_from_spec(SPEC)
sys.modules[MODULE_NAME] = SBOM
SPEC.loader.exec_module(SBOM)


class GenerateGoSBOMTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-sbom-test-")
        self.root = Path(self.temporary.name)
        (self.root / "third_party").mkdir()
        (self.root / "LICENSES" / "go-modules").mkdir(parents=True)
        (self.root / "THIRD_PARTY_NOTICES.md").write_text(
            "modernc.org/libc v1.74.1 BSD-3-Clause\n"
            "modernc.org/sqlite v1.54.0 BSD-3-Clause\n",
            encoding="utf-8",
        )
        self.binary = self.root / "rkc"
        self.binary.write_bytes(b"RKC-binary-fixture")
        self.source_commit = "a" * 40
        self.source_tree = "b" * 40
        self.source_date_epoch = "7"
        self.commit_time = "1970-01-01T00:00:07Z"
        self.licenses: dict[str, tuple[str, str]] = {}
        for name, content in (
            ("libc", "libc license\n"),
            ("sqlite", "sqlite license\n"),
        ):
            relative = f"LICENSES/go-modules/{name}.txt"
            payload = content.encode()
            (self.root / relative).write_bytes(payload)
            self.licenses[name] = (relative, hashlib.sha256(payload).hexdigest())
        sqlite_public = b"SQLite public domain dedication\n"
        self.sqlite_public_path = "LICENSES/go-modules/sqlite-public-domain.txt"
        (self.root / self.sqlite_public_path).write_bytes(sqlite_public)
        self.sqlite_public_digest = hashlib.sha256(sqlite_public).hexdigest()
        self.lock = self.root / "third_party" / "go-modules.lock.json"
        self.write_lock()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def module(
        self,
        path: str,
        version: str,
        module_sum: str,
        name: str,
        direct: bool,
    ) -> dict[str, object]:
        license_path, license_digest = self.licenses[name]
        licenses = [
            {
                "source_path": "LICENSE",
                "path": license_path,
                "sha256": license_digest,
            }
        ]
        expression = "BSD-3-Clause"
        if name == "sqlite":
            expression = "BSD-3-Clause AND LicenseRef-SQLite-Public-Domain"
            licenses.append(
                {
                    "source_path": "SQLITE-LICENSE",
                    "path": self.sqlite_public_path,
                    "sha256": self.sqlite_public_digest,
                }
            )
        return {
            "path": path,
            "version": version,
            "direct": direct,
            "module_sum": module_sum,
            "go_mod_sum": f"h1:{name}-go-mod",
            "source_url": f"https://example.test/{name}",
            "license_spdx": expression,
            "licenses": licenses,
            "notice_path": "THIRD_PARTY_NOTICES.md",
        }

    def lock_document(self) -> dict[str, object]:
        return {
            "schema_version": "1.0",
            "go": {"directive": "1.25.0", "toolchain": "go1.26.5"},
            "root_requirements": [
                {"path": "modernc.org/sqlite", "version": "v1.54.0"}
            ],
            "modules": [
                self.module(
                    "modernc.org/libc", "v1.74.1", "h1:libc", "libc", False
                ),
                self.module(
                    "modernc.org/sqlite",
                    "v1.54.0",
                    "h1:sqlite",
                    "sqlite",
                    True,
                ),
            ],
        }

    def write_lock(self, document: dict[str, object] | None = None) -> None:
        self.lock.write_text(
            json.dumps(document or self.lock_document(), indent=2) + "\n",
            encoding="utf-8",
        )

    def build_info(
        self,
        *,
        cgo: str = "0",
        main: str = SBOM.PROJECT_MODULE,
        command_path: str | None = None,
        toolchain: str = "go1.26.5",
        goos: str = "linux",
        goarch: str = "amd64",
        target_tuning: str | None = None,
        goexperiment: str | None = None,
        gofips140: str | None = None,
        source_commit: str | None = None,
        modified: str = "false",
        dependencies: list[dict[str, object]] | None = None,
    ) -> dict[str, object]:
        settings = [
            {"Key": "CGO_ENABLED", "Value": cgo},
            {"Key": "GOOS", "Value": goos},
            {"Key": "GOARCH", "Value": goarch},
            {
                "Key": "GOAMD64" if goarch == "amd64" else "GOARM64",
                "Value": target_tuning
                or ("v1" if goarch == "amd64" else "v8.0"),
            },
            {"Key": "-trimpath", "Value": "true"},
            {"Key": "vcs", "Value": "git"},
            {
                "Key": "vcs.revision",
                "Value": source_commit or self.source_commit,
            },
            {"Key": "vcs.modified", "Value": modified},
            {"Key": "vcs.time", "Value": self.commit_time},
        ]
        if goexperiment is not None:
            settings.append({"Key": "GOEXPERIMENT", "Value": goexperiment})
        if gofips140 is not None:
            settings.append({"Key": "GOFIPS140", "Value": gofips140})
        return {
            "GoVersion": toolchain,
            "Path": command_path or SBOM.PROJECT_MODULE + "/cmd/rkc",
            "Main": {"Path": main, "Version": "(devel)", "Sum": ""},
            "Deps": dependencies
            if dependencies is not None
            else [
                {
                    "Path": "modernc.org/sqlite",
                    "Version": "v1.54.0",
                    "Sum": "h1:sqlite",
                },
                {
                    "Path": "modernc.org/libc",
                    "Version": "v1.74.1",
                    "Sum": "h1:libc",
                },
            ],
            "Settings": settings,
        }

    def identity_arguments(self) -> dict[str, str]:
        return {
            "source_commit": self.source_commit,
            "source_tree": self.source_tree,
            "goos": "linux",
            "goarch": "amd64",
            "source_date_epoch": self.source_date_epoch,
        }

    def completed(self, document: dict[str, object], status: int = 0) -> subprocess.CompletedProcess[str]:
        return subprocess.CompletedProcess(
            ["go", "version"],
            status,
            stdout=json.dumps(document),
            stderr="failure" if status else "",
        )

    @mock.patch.object(SBOM.subprocess, "run")
    def test_generate_is_deterministic_and_binds_binary_modules(self, run: mock.Mock) -> None:
        run.side_effect = [
            self.completed(self.build_info()),
            self.completed(self.build_info(gofips140="off")),
        ]
        with mock.patch.dict(os.environ, {"SOURCE_DATE_EPOCH": "1"}, clear=False):
            first = SBOM.generate(
                self.binary,
                self.lock,
                "0.3.0-reference",
                self.root,
                **self.identity_arguments(),
            )
            second = SBOM.generate(
                self.binary,
                self.lock,
                "0.3.0-reference",
                self.root,
                **self.identity_arguments(),
            )
        self.assertEqual(first, second)
        self.assertEqual(first["spdxVersion"], "SPDX-2.3")
        self.assertEqual(first["creationInfo"]["created"], self.commit_time)
        self.assertEqual(
            [item["name"] for item in first["packages"]],
            ["Repository Knowledge Compiler", "modernc.org/libc", "modernc.org/sqlite"],
        )
        digest = hashlib.sha256(self.binary.read_bytes()).hexdigest()
        self.assertEqual(first["files"][0]["checksums"][0]["checksumValue"], digest)
        self.assertEqual(first["files"][0]["licenseConcluded"], "NOASSERTION")
        self.assertEqual(first["files"][0]["licenseInfoInFiles"], ["NOASSERTION"])
        self.assertFalse(first["packages"][0]["filesAnalyzed"])
        self.assertEqual(first["packages"][0]["licenseConcluded"], "NOASSERTION")
        self.assertEqual(first["packages"][0]["licenseDeclared"], "Apache-2.0")
        self.assertIn(digest, first["documentNamespace"])
        self.assertIn(self.source_tree[:16], first["documentNamespace"])
        self.assertEqual(first["packages"][1]["licenseConcluded"], "NOASSERTION")
        self.assertEqual(
            first["packages"][1]["externalRefs"][0]["referenceLocator"],
            "pkg:golang/modernc.org/libc@v1.74.1",
        )
        self.assertIn("GOAMD64=v1", first["packages"][0]["comment"])
        self.assertIn("GOEXPERIMENT=default", first["comment"])
        self.assertIn("GOFIPS140=off", first["comment"])
        extracted = first["hasExtractedLicensingInfos"]
        self.assertEqual(extracted[0]["licenseId"], "LicenseRef-SQLite-Public-Domain")
        self.assertIn("public domain", extracted[0]["extractedText"].lower())
        self.assertEqual(run.call_count, 2)

    @mock.patch.object(SBOM.subprocess, "run")
    def test_binary_metadata_failures_are_closed(self, run: mock.Mock) -> None:
        cases = (
            (self.build_info(cgo="1"), "CGO_ENABLED=0"),
            (self.build_info(main="example.invalid/main"), "main module"),
            (
                self.build_info(command_path=SBOM.PROJECT_MODULE + "/cmd/rkc-mcp"),
                "command path",
            ),
            (self.build_info(toolchain="go1.26.4"), "toolchain"),
            (self.build_info(goarch="arm64"), "GOARCH"),
            (self.build_info(target_tuning="v3"), "GOAMD64"),
            (self.build_info(goexperiment="boringcrypto"), "GOEXPERIMENT"),
            (self.build_info(gofips140="latest"), "GOFIPS140=off"),
            (self.build_info(source_commit="c" * 40), "source commit"),
            (self.build_info(modified="true"), "dirty"),
            (
                self.build_info(
                    dependencies=[
                        {"Path": "unknown.test/module", "Version": "v1.0.0", "Sum": "h1:x"}
                    ]
                ),
                "unaudited module",
            ),
            (
                self.build_info(
                    dependencies=[
                        {
                            "Path": "modernc.org/sqlite",
                            "Version": "v1.53.0",
                            "Sum": "h1:sqlite",
                        }
                    ]
                ),
                "version drifted",
            ),
            (
                self.build_info(
                    dependencies=[
                        {
                            "Path": "modernc.org/sqlite",
                            "Version": "v1.54.0",
                            "Sum": "h1:wrong",
                        }
                    ]
                ),
                "checksum drifted",
            ),
            (
                self.build_info(
                    dependencies=[
                        {
                            "Path": "modernc.org/sqlite",
                            "Version": "v1.54.0",
                            "Sum": "h1:sqlite",
                            "Replace": {"Path": "local"},
                        }
                    ]
                ),
                "replacement",
            ),
        )
        for document, marker in cases:
            with self.subTest(marker=marker):
                run.return_value = self.completed(document)
                with self.assertRaisesRegex(SBOM.SBOMError, marker):
                    SBOM.generate(
                        self.binary,
                        self.lock,
                        "0.3.0",
                        self.root,
                        **self.identity_arguments(),
                    )

        run.return_value = self.completed({}, 1)
        with self.assertRaisesRegex(SBOM.SBOMError, "cannot inspect"):
            SBOM.generate(
                self.binary,
                self.lock,
                "0.3.0",
                self.root,
                **self.identity_arguments(),
            )
        run.return_value = subprocess.CompletedProcess([], 0, stdout="{", stderr="")
        with self.assertRaisesRegex(SBOM.SBOMError, "invalid JSON"):
            SBOM.generate(
                self.binary,
                self.lock,
                "0.3.0",
                self.root,
                **self.identity_arguments(),
            )

    def test_lock_rejects_drift_and_unsafe_license_inputs(self) -> None:
        locked, digest, go_metadata = SBOM.load_lock(self.lock, self.root)
        self.assertEqual(sorted(locked), ["modernc.org/libc", "modernc.org/sqlite"])
        self.assertRegex(digest, r"^[0-9a-f]{64}$")
        self.assertEqual(go_metadata["toolchain"], "go1.26.5")

        document = self.lock_document()
        document["extra"] = True
        self.write_lock(document)
        with self.assertRaisesRegex(SBOM.SBOMError, "keys drifted"):
            SBOM.load_lock(self.lock, self.root)

        document = self.lock_document()
        modules = document["modules"]
        modules.reverse()
        self.write_lock(document)
        with self.assertRaisesRegex(SBOM.SBOMError, "path-sorted"):
            SBOM.load_lock(self.lock, self.root)

        document = self.lock_document()
        document["modules"][0]["licenses"][0]["sha256"] = "0" * 64
        self.write_lock(document)
        with self.assertRaisesRegex(SBOM.SBOMError, "digest drifted"):
            SBOM.load_lock(self.lock, self.root)

        self.write_lock()
        license_path = self.root / self.licenses["libc"][0]
        license_path.unlink()
        license_path.symlink_to(self.root / self.licenses["sqlite"][0])
        with self.assertRaisesRegex(SBOM.SBOMError, "regular file"):
            SBOM.load_lock(self.lock, self.root)

    def test_dependency_inventory_rejects_duplicate_and_malformed_metadata(self) -> None:
        locked, _, _ = SBOM.load_lock(self.lock, self.root)
        duplicate = self.build_info()["Deps"]
        duplicate.append(dict(duplicate[0]))
        with self.assertRaisesRegex(SBOM.SBOMError, "duplicate"):
            SBOM.dependency_inventory({"Deps": duplicate}, locked)
        with self.assertRaisesRegex(SBOM.SBOMError, "malformed"):
            SBOM.dependency_inventory({"Deps": [None]}, locked)
        with self.assertRaisesRegex(SBOM.SBOMError, "metadata"):
            SBOM.dependency_inventory({"Deps": {}}, locked)

    def test_canonical_go_purl_preserves_namespace_segments(self) -> None:
        self.assertEqual(
            SBOM.canonical_go_purl(
                "github.com/example/module", "v1.2.3+incompatible"
            ),
            "pkg:golang/github.com/example/module@v1.2.3%2Bincompatible",
        )
        with self.assertRaisesRegex(SBOM.SBOMError, "module path"):
            SBOM.canonical_go_purl("example.com//module", "v1.0.0")

    def test_atomic_output_and_source_date_validation(self) -> None:
        output = self.root / "rkc.spdx.json"
        document = {"spdxVersion": "SPDX-2.3"}
        SBOM.write_document(document, output, False)
        self.assertEqual(json.loads(output.read_text(encoding="utf-8")), document)
        self.assertEqual(output.stat().st_mode & 0o777, 0o644)
        with self.assertRaisesRegex(SBOM.SBOMError, "pass --force"):
            SBOM.write_document(document, output, False)
        SBOM.write_document({"changed": True}, output, True)
        self.assertEqual(json.loads(output.read_text(encoding="utf-8")), {"changed": True})
        with mock.patch.dict(os.environ, {"SOURCE_DATE_EPOCH": "bad"}, clear=False):
            with self.assertRaisesRegex(SBOM.SBOMError, "non-negative"):
                SBOM.source_date()
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(SBOM.source_date(), "1970-01-01T00:00:00Z")

        directory_output = self.root / "directory-output"
        directory_output.mkdir()
        with self.assertRaisesRegex(SBOM.SBOMError, "regular file"):
            SBOM.write_document(document, directory_output, True)
        with self.assertRaisesRegex(SBOM.SBOMError, "parent is missing"):
            SBOM.write_document(document, self.root / "missing" / "output", False)

    @mock.patch.object(SBOM.subprocess, "run")
    def test_verify_document_recomputes_every_binary_binding(self, run: mock.Mock) -> None:
        run.return_value = self.completed(self.build_info())
        with mock.patch.dict(os.environ, {"SOURCE_DATE_EPOCH": "7"}, clear=False):
            document = SBOM.generate(
                self.binary,
                self.lock,
                "0.3.0",
                self.root,
                **self.identity_arguments(),
            )
        output = self.root / "rkc.spdx.json"
        SBOM.write_document(document, output, False)
        SBOM.verify_document(
            output,
            self.binary,
            self.lock,
            "0.3.0",
            self.root,
            **self.identity_arguments(),
        )

        document["files"][0]["checksums"][0]["checksumValue"] = "0" * 64
        SBOM.write_document(document, output, True)
        with self.assertRaisesRegex(SBOM.SBOMError, "does not exactly bind"):
            SBOM.verify_document(
                output,
                self.binary,
                self.lock,
                "0.3.0",
                self.root,
                **self.identity_arguments(),
            )

        document["creationInfo"]["created"] = "1970-01-01T00:00:08Z"
        SBOM.write_document(document, output, True)
        with self.assertRaisesRegex(SBOM.SBOMError, "does not exactly bind"):
            SBOM.verify_document(
                output,
                self.binary,
                self.lock,
                "0.3.0",
                self.root,
                **self.identity_arguments(),
            )

    def test_regular_input_guards(self) -> None:
        digest = SBOM.sha256_file(self.binary, "fixture")
        self.assertEqual(digest, hashlib.sha256(self.binary.read_bytes()).hexdigest())
        with self.assertRaisesRegex(SBOM.SBOMError, "missing"):
            SBOM.sha256_file(self.root / "missing", "fixture")
        with self.assertRaisesRegex(SBOM.SBOMError, "regular"):
            SBOM.sha256_file(self.root, "fixture")
        if hasattr(os, "symlink"):
            link = self.root / "binary-link"
            link.symlink_to(self.binary)
            with self.assertRaisesRegex(SBOM.SBOMError, "regular"):
                SBOM.sha256_file(link, "fixture")


if __name__ == "__main__":
    unittest.main()
