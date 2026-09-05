#!/usr/bin/env python3
"""Hermetic portable release and bootstrap boundary tests."""
from __future__ import annotations

import hashlib
import importlib.util
import io
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest
import zipfile
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("rkc_package_portable", ROOT / "scripts/package-portable.py")
assert SPEC and SPEC.loader
PACKAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PACKAGE)


class PortableReleaseTests(unittest.TestCase):
    def setUp(self) -> None:
        self.work = tempfile.TemporaryDirectory(prefix="rkc-portable-test-")
        self.addCleanup(self.work.cleanup)
        # macOS commonly exposes its temporary root through /var -> /private/var.
        # Fixtures that expect admission use the real path; linked-path rejection
        # is exercised separately below.
        self.root = Path(self.work.name).resolve()
        self.binaries = self.root / "dist/portable-binaries"
        self.source = {"version": "0.4.0", "commit": "a" * 40, "tree": "b" * 40, "commit_time_unix": 1700000000}
        for name in ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md", "VERSION", "LICENSES/Go.txt", "LICENSES/go-modules/example@v1/LICENSE", "third_party/go-modules.lock.json", "models/models.lock.json", "models/qualification/rkc-local-model-v1.json", "schemas/model-lock.schema.json", "schemas/model-qualification.schema.json", "scripts/install-release.sh", "scripts/install-release.ps1"):
            self.write(name, b"0.4.0\n" if name == "VERSION" else b"fixture\n")
        for platform in PACKAGE.PLATFORMS:
            suffix = ".exe" if platform.startswith("windows-") else ""
            for name in ("rkc" + suffix, "rkc-mcp" + suffix, "rkc.spdx.json", "rkc-mcp.spdx.json"):
                self.write(f"dist/portable-binaries/{platform}/{name}", (platform + ":" + name).encode())

    def write(self, relative: str, data: bytes) -> Path:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        return path

    def test_payload_provenance_and_deterministic_archives(self) -> None:
        for platform in PACKAGE.PLATFORMS:
            files = PACKAGE.payload(self.root, self.binaries, platform, self.source)
            manifest = json.loads(files[PACKAGE.MANIFEST][0])
            self.assertEqual(manifest["source"], self.source)
            self.assertEqual(manifest["platform"], platform)
            self.assertEqual(manifest["excluded_generated_files"], [PACKAGE.MANIFEST])
            self.assertEqual({entry["path"] for entry in manifest["files"]}, set(files) - {PACKAGE.MANIFEST})
            for entry in manifest["files"]:
                body, mode = files[entry["path"]]
                self.assertEqual(entry["sha256"], hashlib.sha256(body).hexdigest())
                self.assertEqual(entry["bytes"], len(body))
                self.assertEqual(entry["mode"], format(mode, "04o"))
            first, second = self.root / "first.zip", self.root / "second.zip"
            PACKAGE.write_archive(first, files, self.source["commit_time_unix"])
            PACKAGE.write_archive(second, files, self.source["commit_time_unix"])
            self.assertEqual(first.read_bytes(), second.read_bytes())
            with zipfile.ZipFile(first) as archive:
                self.assertEqual(archive.namelist(), sorted(files))
                self.assertTrue(all(stat.S_ISREG(entry.external_attr >> 16) for entry in archive.infolist()))
            first.unlink(); second.unlink()
        with self.assertRaisesRegex(PACKAGE.PackageError, "unsupported"):
            PACKAGE.payload(self.root, self.binaries, "linux-riscv64", self.source)

    def test_input_boundaries_reject_links_directories_and_oversize(self) -> None:
        linked = self.root / "linked"
        linked.symlink_to(self.root / "VERSION")
        for path in (linked, self.root / "LICENSES"):
            with self.assertRaises(PACKAGE.PackageError):
                PACKAGE.regular_bytes(path)
        with patch.object(PACKAGE, "MAX_FILE_BYTES", 3):
            with self.assertRaisesRegex(PACKAGE.PackageError, "too large"):
                PACKAGE.regular_bytes(self.root / "VERSION")
        with patch.object(PACKAGE, "MAX_PACKAGE_BYTES", 3):
            with self.assertRaisesRegex(PACKAGE.PackageError, "bound"):
                PACKAGE.payload(self.root, self.binaries, "linux-amd64", self.source)
        shutil.rmtree(self.root / "LICENSES")
        with self.assertRaisesRegex(PACKAGE.PackageError, "LICENSES"):
            PACKAGE.payload(self.root, self.binaries, "linux-amd64", self.source)

    def test_source_identity_and_version_validation(self) -> None:
        with patch.object(PACKAGE, "require_clean_worktree") as clean, patch.object(PACKAGE.subprocess, "check_output", side_effect=["a" * 40, "b" * 40, "1700000000"]):
            self.assertEqual(PACKAGE.source_identity(self.root), self.source)
            clean.assert_called_once()
        self.write("VERSION", b"bad/path")
        with patch.object(PACKAGE, "require_clean_worktree"), self.assertRaisesRegex(PACKAGE.PackageError, "VERSION"):
            PACKAGE.source_identity(self.root)

    def test_binary_verification_covers_every_exact_source_target(self) -> None:
        with patch.object(PACKAGE.subprocess, "run") as run:
            PACKAGE.verify_binary_receipts(self.root, self.binaries, self.source)
        self.assertEqual(run.call_count, 12)
        for call in run.call_args_list:
            args = call.args[0]
            self.assertEqual(args[args.index("--source-commit") + 1], self.source["commit"])
            self.assertIn("--verify-document", args)
            self.assertTrue(call.kwargs["check"])
            self.assertEqual(call.kwargs["timeout"], 120)

    def test_generation_publishes_only_after_verification(self) -> None:
        output = self.root / "dist/portable-release"
        with patch.object(PACKAGE, "source_identity", return_value=self.source), patch.object(PACKAGE, "verify_binary_receipts") as verify:
            PACKAGE.assemble(self.root, self.binaries, output)
            self.assertEqual(verify.call_count, 1)
            self.assertNotEqual(verify.call_args.args[1], self.binaries)
            self.assertTrue(str(verify.call_args.args[1]).startswith(str(self.root / "dist/.rkc-portable-")))
            with self.assertRaisesRegex(PACKAGE.PackageError, "absent"):
                PACKAGE.assemble(self.root, self.binaries, output)
        lines = (output / "SHA256SUMS.txt").read_text().splitlines()
        self.assertEqual(len(lines), 8)
        for line in lines:
            digest, name = line.split("  ")
            self.assertEqual(digest, hashlib.sha256((output / name).read_bytes()).hexdigest())
        shutil.rmtree(output)
        changed = dict(self.source, commit="c" * 40)
        with patch.object(PACKAGE, "source_identity", side_effect=[self.source, changed]), patch.object(PACKAGE, "verify_binary_receipts"), self.assertRaisesRegex(PACKAGE.PackageError, "changed"):
            PACKAGE.assemble(self.root, self.binaries, output)
        self.assertFalse(output.exists())
        with patch.object(PACKAGE, "source_identity", return_value=self.source), patch.object(PACKAGE, "verify_binary_receipts", side_effect=ValueError("bad receipt")), self.assertRaisesRegex(ValueError, "bad receipt"):
            PACKAGE.assemble(self.root, self.binaries, output)
        self.assertFalse(output.exists())

    def installer(self, files: dict[str, tuple[bytes, int]] | None = None, system: str = "Linux", machine: str = "x86_64", extra: list[str] | None = None, online: bool = False) -> subprocess.CompletedProcess[str]:
        archive = self.root / "download.zip"
        archive.unlink(missing_ok=True)
        platform = ("darwin" if system == "Darwin" else "linux") + "-" + ("arm64" if machine in {"arm64", "aarch64"} else "amd64")
        PACKAGE.write_archive(archive, files if files is not None else PACKAGE.payload(self.root, self.binaries, platform, self.source), 1700000000)
        receipt = self.write("SHA256SUMS.txt", f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  rkc-{platform}.zip\n".encode())
        fake = self.root / "fake-bin"
        fake.mkdir(exist_ok=True)
        uname = self.write("fake-bin/uname", b'#!/bin/sh\ncase "$1" in -s) echo "$TEST_SYSTEM";; -m) echo "$TEST_MACHINE";; esac\n')
        uname.chmod(0o755)
        curl = self.write("fake-bin/curl", b'#!/bin/sh\nprintf "%s\\n" "$@" >> "$TEST_CURL_LOG"\nwhile [ "$#" -gt 0 ]; do case "$1" in --output) output=$2; shift 2;; *) address=$1; shift;; esac; done\ncase "$address" in *SHA256SUMS.txt) cp "$TEST_RECEIPT" "$output";; *.zip) cp "$TEST_ARCHIVE" "$output";; *) exit 1;; esac\n')
        curl.chmod(0o755)
        env = dict(os.environ, PATH=str(fake) + os.pathsep + os.environ["PATH"], TEST_SYSTEM=system, TEST_MACHINE=machine, TEST_RECEIPT=str(receipt), TEST_ARCHIVE=str(archive), TEST_CURL_LOG=str(self.root / "curl.log"))
        args = ["sh", str(ROOT / "scripts/install-release.sh"), "--prefix", str(self.root / "install with 'quote")]
        if not online:
            args.extend(["--archive", str(archive), "--checksums", str(receipt)])
        return subprocess.run(args + (extra or []), env=env, capture_output=True, text=True, timeout=30)

    @unittest.skipUnless(shutil.which("unzip") and shutil.which("sha256sum"), "POSIX unzip and SHA256 tools required")
    def test_posix_installer_hosts_receipts_and_preserved_licenses(self) -> None:
        for system, machine in (("Linux", "x86_64"), ("Linux", "aarch64"), ("Darwin", "x86_64"), ("Darwin", "arm64")):
            result = self.installer(system=system, machine=machine)
            self.assertEqual(result.returncode, 0, result.stderr)
            prefix = self.root / "install with 'quote"
            platform = ("darwin" if system == "Darwin" else "linux") + "-" + ("arm64" if machine in {"arm64", "aarch64"} else "amd64")
            self.assertEqual((prefix / "bin/rkc").read_bytes(), (platform + ":rkc").encode())
            self.assertTrue(os.access(prefix / "bin/rkc", os.X_OK))
            self.assertTrue((prefix / "share/doc/rkc/LICENSES/go-modules/example@v1/LICENSE").is_file())
            self.assertIn(" gui", result.stdout)
        result = self.installer(online=True, extra=["--version", "v0.4.0"])
        self.assertEqual(result.returncode, 0, result.stderr)
        transport = (self.root / "curl.log").read_text()
        self.assertIn("https://github.com/neuroforge-io/RKC/releases/download/v0.4.0/rkc-linux-amd64.zip", transport)
        self.assertIn("--proto-redir\n=https", transport)
        self.assertIn("--max-filesize\n134217728", transport)

    @unittest.skipUnless(shutil.which("unzip") and shutil.which("sha256sum"), "POSIX unzip and SHA256 tools required")
    def test_posix_installer_rejects_hostile_archives_and_destinations(self) -> None:
        normal = PACKAGE.payload(self.root, self.binaries, "linux-amd64", self.source)
        for name, files in (
            ("traversal", dict(normal, **{"share/doc/rkc/../../escape": (b"bad", 0o644)})),
            ("unexpected", dict(normal, **{"etc/profile": (b"bad", 0o644)})),
            ("missing", {name: item for name, item in normal.items() if name != "bin/rkc"}),
            ("symlink", dict(normal, **{"share/doc/rkc/link": (b"/tmp", stat.S_IFLNK | 0o777)})),
        ):
            with self.subTest(name=name):
                result = self.installer(files)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "install with 'quote/bin/rkc").exists())
        prefix = self.root / "install with 'quote"
        prefix.mkdir(exist_ok=True)
        (prefix / "bin").symlink_to(self.root, target_is_directory=True)
        result = self.installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("symlink", result.stderr)
        self.assertFalse((self.root / "rkc").exists())

    def test_posix_installer_rejects_invalid_options_and_platforms(self) -> None:
        for options in (["--prefix", "/"], ["--version", "../bad"], ["--unknown"], ["--version"]):
            self.assertNotEqual(self.installer(extra=options).returncode, 0)
        self.assertNotEqual(self.installer(system="FreeBSD").returncode, 0)
        self.assertNotEqual(self.installer(machine="riscv64").returncode, 0)
        result = subprocess.run(["sh", str(ROOT / "scripts/install-release.sh"), "--help"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0)
        self.assertIn("never disables guards", result.stdout)

    @unittest.skipUnless(shutil.which("unzip") and shutil.which("sha256sum"), "POSIX unzip and SHA256 tools required")
    def test_posix_installer_rejects_existing_linked_ancestors_and_root_aliases(self) -> None:
        real = self.root / "real-parent"
        real.mkdir()
        linked = self.root / "linked-parent"
        linked.symlink_to(real, target_is_directory=True)
        prefix = linked / "existing-prefix"
        prefix.mkdir()
        result = self.installer(extra=["--prefix", str(prefix)])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("symlink", result.stderr)
        self.assertFalse((real / "existing-prefix/bin").exists())
        result = self.installer(extra=["--prefix", "/."])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("filesystem root", result.stderr)

    def test_installer_checksum_failures_precede_installation(self) -> None:
        missing = self.write("missing-sums.txt", b"0" * 64 + b"  another-platform.zip\n")
        invalid = self.write("invalid-sums.txt", b"0" * 64 + b"  rkc-linux-amd64.zip\n")
        duplicate = self.write("duplicate-sums.txt", invalid.read_bytes() * 2)
        for receipt in (missing, invalid, duplicate):
            result = self.installer(extra=["--checksums", str(receipt)])
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((self.root / "install with 'quote/bin/rkc").exists())

    def test_command_line_and_missing_dist_fail_cleanly(self) -> None:
        with patch.object(PACKAGE, "ROOT", self.root), patch.object(PACKAGE, "source_identity", return_value=self.source), patch.object(PACKAGE, "verify_binary_receipts"), patch.object(PACKAGE.sys, "argv", ["package-portable.py"]):
            with redirect_stdout(io.StringIO()) as output:
                self.assertEqual(PACKAGE.main(), 0)
            self.assertIn("six verified downloads", output.getvalue())
            with redirect_stderr(io.StringIO()) as errors:
                self.assertEqual(PACKAGE.main(), 1)
            self.assertIn("absent", errors.getvalue())
        shutil.rmtree(self.root / "dist")
        with patch.object(PACKAGE, "source_identity", return_value=self.source), self.assertRaisesRegex(PACKAGE.PackageError, "dist must"):
            PACKAGE.assemble(self.root, self.binaries, self.root / "dist/portable-release")


if __name__ == "__main__":
    unittest.main()
