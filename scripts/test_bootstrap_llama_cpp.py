#!/usr/bin/env python3
"""Unit tests for the checksum-pinned llama.cpp runtime receipt."""
from __future__ import annotations

import hashlib
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import bootstrap_llama_cpp
import model_assets

RUNTIME_RECEIPT_FIXTURE = (
    Path(__file__).resolve().parents[1]
    / "models"
    / "runtime-receipt-v1.1.fixture.json"
)


class RuntimeReceiptTests(unittest.TestCase):
    def setUp(self) -> None:
        """Keep unit fixtures hermetic while production defaults stay fail-closed."""
        priority = mock.patch.object(bootstrap_llama_cpp, "assert_priority_available")
        priority.start()
        self.addCleanup(priority.stop)

    def _runtime_fixture(
        self,
        root: Path,
        lock: model_assets.ModelLock,
        *,
        omit: str | None = None,
    ) -> None:
        binary_directory = root / "build" / "bin"
        binary_directory.mkdir(parents=True)
        llama = lock.document["llama_cpp"]  # type: ignore[assignment]
        cmake = llama["cmake"]  # type: ignore[index]
        targets = cmake["targets"]  # type: ignore[index]
        source_asset = lock.asset(str(llama["source_asset_id"]))  # type: ignore[index]
        license_payload = b"llama.cpp fixture MIT license\n"
        license_path = root / bootstrap_llama_cpp.RUNTIME_LICENSE_RELATIVE
        license_path.parent.mkdir(parents=True)
        license_path.write_bytes(license_payload)
        receipt = json.loads(
            RUNTIME_RECEIPT_FIXTURE.read_bytes(),
            object_pairs_hook=bootstrap_llama_cpp._strict_json_object,
            parse_constant=bootstrap_llama_cpp._reject_json_constant,
        )
        fixture_binaries = {
            Path(str(entry["path"])).name: entry for entry in receipt["binaries"]
        }
        binaries = []
        for target in targets:
            if target == omit:
                continue
            relative = f"build/bin/{target}"
            payload = f"fixture {target}\n".encode("ascii")
            (root / relative).write_bytes(payload)
            binary = dict(fixture_binaries[target])
            binary.update(
                {
                    "path": relative,
                    "sha256": hashlib.sha256(payload).hexdigest(),
                    "size_bytes": len(payload),
                }
            )
            binaries.append(binary)
        receipt.update(
            {
                "schema_version": bootstrap_llama_cpp.RUNTIME_RECEIPT_SCHEMA_VERSION,
                "runtime": "llama.cpp",
                "tag": llama["tag"],  # type: ignore[index]
                "commit": llama["commit"],  # type: ignore[index]
                "source_sha256": source_asset.sha256,
                "source_size_bytes": source_asset.size_bytes,
                "lock_sha256": lock.digest,
                "profile": "portable",
                "license": {
                    "path": bootstrap_llama_cpp.RUNTIME_LICENSE_RELATIVE.as_posix(),
                    "sha256": hashlib.sha256(license_payload).hexdigest(),
                    "size_bytes": len(license_payload),
                    "license_spdx": llama["license_spdx"],  # type: ignore[index]
                    "license_url": llama["license_url"],  # type: ignore[index]
                },
                "binaries": binaries,
            }
        )
        (root / bootstrap_llama_cpp.RECEIPT_NAME).write_text(
            json.dumps(receipt), encoding="utf-8"
        )

    def test_exact_locked_binary_inventory_passes(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._runtime_fixture(root, lock)
            bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

    def test_missing_embedding_binary_fails_closed(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._runtime_fixture(root, lock, omit="llama-embedding")
            with self.assertRaisesRegex(model_assets.IntegrityError, "missing=.*embedding"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

    def test_type_helpers_and_safe_archive_member_contract(self) -> None:
        self.assertEqual(bootstrap_llama_cpp._mapping({"x": 1}, "x"), {"x": 1})
        self.assertEqual(bootstrap_llama_cpp._string_list(["x"], "x"), ["x"])
        with self.assertRaises(model_assets.AssetError):
            bootstrap_llama_cpp._mapping([], "x")
        with self.assertRaises(model_assets.AssetError):
            bootstrap_llama_cpp._string_list([1], "x")
        self.assertIsNone(bootstrap_llama_cpp._safe_relative_member("root", "root"))
        self.assertEqual(
            bootstrap_llama_cpp._safe_relative_member("root/src/file", "root"),
            Path("src/file"),
        )
        for value in ("/root/file", "root/../file", "other/file", "root\\file"):
            with self.subTest(value=value), self.assertRaises(model_assets.IntegrityError):
                bootstrap_llama_cpp._safe_relative_member(value, "root")

    def test_hash_and_private_directory_reject_links_and_oversize(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            file = root / "file"
            file.write_bytes(b"payload")
            checks = 0

            def priority() -> None:
                nonlocal checks
                checks += 1

            with mock.patch.object(bootstrap_llama_cpp, "PRIORITY_RECHECK_BYTES", 1):
                digest, size = bootstrap_llama_cpp._sha256_file(
                    file, priority_check=priority
                )
            self.assertEqual(digest, hashlib.sha256(b"payload").hexdigest())
            self.assertEqual(size, 7)
            self.assertGreaterEqual(checks, 2)
            with self.assertRaises(model_assets.IntegrityError):
                bootstrap_llama_cpp._sha256_file(file, maximum_bytes=1)
            nested = bootstrap_llama_cpp._private_directory(root / "a" / "b")
            self.assertTrue(nested.is_dir())
            self.assertEqual(nested.stat().st_mode & 0o777, 0o700)
            unsafe = root / "unsafe"
            unsafe.mkdir(mode=0o777)
            os.chmod(unsafe, 0o777)
            with self.assertRaisesRegex(model_assets.AssetError, "writable"):
                bootstrap_llama_cpp._private_directory(unsafe)
            if hasattr(os, "symlink"):
                link = root / "link"
                link.symlink_to(nested, target_is_directory=True)
                with self.assertRaisesRegex(model_assets.AssetError, "real directory"):
                    bootstrap_llama_cpp._private_directory(link)

    def make_archive(self, path: Path, *, special: bool = False) -> None:
        with tarfile.open(path, "w:gz") as archive:
            root = tarfile.TarInfo("llama-root")
            root.type = tarfile.DIRTYPE
            archive.addfile(root)
            for name, payload, mode in (
                ("CMakeLists.txt", b"project(llama)\n", 0o644),
                ("LICENSE", b"MIT\n", 0o644),
                ("ggml/CMakeLists.txt", b"project(ggml)\n", 0o644),
                ("tools/run.sh", b"#!/bin/sh\n", 0o755),
            ):
                member = tarfile.TarInfo(f"llama-root/{name}")
                member.size = len(payload)
                member.mode = mode
                archive.addfile(member, io.BytesIO(payload))
            if special:
                link = tarfile.TarInfo("llama-root/link")
                link.type = tarfile.SYMTYPE
                link.linkname = "/etc/passwd"
                archive.addfile(link)

    def test_extract_source_accepts_regular_tree_and_rejects_special_member(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "source.tar.gz"
            self.make_archive(archive)
            destination = root / "source"
            destination.mkdir()
            bootstrap_llama_cpp._extract_source(archive, destination, "llama-root")
            self.assertEqual((destination / "LICENSE").read_bytes(), b"MIT\n")
            self.assertEqual((destination / "tools/run.sh").stat().st_mode & 0o777, 0o700)
            special = root / "special.tar.gz"
            self.make_archive(special, special=True)
            other = root / "other"
            other.mkdir()
            with self.assertRaisesRegex(model_assets.IntegrityError, "special"):
                bootstrap_llama_cpp._extract_source(special, other, "llama-root")
            missing = root / "missing.tar.gz"
            with tarfile.open(missing, "w:gz") as tar:
                directory = tarfile.TarInfo("llama-root")
                directory.type = tarfile.DIRTYPE
                tar.addfile(directory)
            absent = root / "absent"
            absent.mkdir()
            with self.assertRaises((model_assets.IntegrityError, FileNotFoundError)):
                bootstrap_llama_cpp._extract_source(missing, absent, "llama-root")
            limited = root / "limited"
            limited.mkdir()
            with mock.patch.object(bootstrap_llama_cpp, "MAX_ARCHIVE_MEMBERS", 0), self.assertRaisesRegex(
                model_assets.IntegrityError, "members"
            ):
                bootstrap_llama_cpp._extract_source(archive, limited, "llama-root")
            too_large = root / "too-large"
            too_large.mkdir()
            with mock.patch.object(
                bootstrap_llama_cpp, "MAX_ARCHIVE_FILE_BYTES", 1
            ), self.assertRaisesRegex(model_assets.IntegrityError, "member exceeds"):
                bootstrap_llama_cpp._extract_source(archive, too_large, "llama-root")

            bad_root = root / "bad-root.tar.gz"
            with tarfile.open(bad_root, "w:gz") as tar:
                member = tarfile.TarInfo("llama-root")
                member.size = 1
                tar.addfile(member, io.BytesIO(b"x"))
            bad_root_out = root / "bad-root"
            bad_root_out.mkdir()
            with self.assertRaisesRegex(model_assets.IntegrityError, "root is not"):
                bootstrap_llama_cpp._extract_source(
                    bad_root, bad_root_out, "llama-root"
                )

            duplicate = root / "duplicate.tar.gz"
            with tarfile.open(duplicate, "w:gz") as tar:
                directory = tarfile.TarInfo("llama-root")
                directory.type = tarfile.DIRTYPE
                tar.addfile(directory)
                for _ in range(2):
                    member = tarfile.TarInfo("llama-root/same")
                    member.size = 1
                    tar.addfile(member, io.BytesIO(b"x"))
            duplicate_out = root / "duplicate"
            duplicate_out.mkdir()
            with self.assertRaisesRegex(model_assets.IntegrityError, "repeats"):
                bootstrap_llama_cpp._extract_source(
                    duplicate, duplicate_out, "llama-root"
                )

    @mock.patch.object(bootstrap_llama_cpp.subprocess, "run")
    @mock.patch.object(bootstrap_llama_cpp, "assert_priority_available")
    def test_cmake_version_and_build_command_failures(
        self, priority: mock.Mock, run: mock.Mock
    ) -> None:
        run.return_value = subprocess.CompletedProcess(
            ["cmake"], 0, stdout="cmake version 3.31.4\n", stderr=""
        )
        self.assertEqual(bootstrap_llama_cpp._cmake_version("cmake")[:2], (3, 31))
        run.return_value = subprocess.CompletedProcess(["cmake"], 2, stdout="", stderr="bad")
        with self.assertRaisesRegex(model_assets.AssetError, "failed"):
            bootstrap_llama_cpp._cmake_version("cmake")
        run.return_value = subprocess.CompletedProcess(["cmake"], 0, stdout="unknown\n", stderr="")
        with self.assertRaisesRegex(model_assets.AssetError, "parse"):
            bootstrap_llama_cpp._cmake_version("cmake")
        self.assertTrue(priority.called)

    def test_build_group_is_bounded_reaped_and_priority_preemptible(self) -> None:
        class Process:
            def __init__(self, statuses: list[int | None]) -> None:
                self.pid = 456
                self.statuses = statuses
                self.returncode: int | None = None
                self.wait_calls = 0

            def poll(self) -> int | None:
                if self.statuses:
                    self.returncode = self.statuses.pop(0)
                return self.returncode

            def wait(self, timeout: float | None = None) -> int:
                self.wait_calls += 1
                self.returncode = 0 if self.returncode is None else self.returncode
                return self.returncode

        process = Process([None, 0])
        with mock.patch.object(
            bootstrap_llama_cpp.subprocess, "Popen", return_value=process
        ) as popen, mock.patch.object(bootstrap_llama_cpp.time, "sleep"):
            bootstrap_llama_cpp._run(["cmake"], {}, 10, priority_check=lambda: None)
        self.assertTrue(popen.call_args.kwargs["start_new_session"])
        self.assertGreaterEqual(process.wait_calls, 1)

        failed = Process([9])
        with mock.patch.object(
            bootstrap_llama_cpp.subprocess, "Popen", return_value=failed
        ), self.assertRaisesRegex(model_assets.AssetError, "status 9"):
            bootstrap_llama_cpp._run(["cmake"], {}, 10, priority_check=lambda: None)

        blocked = Process([None, None])
        checks = iter((None, model_assets.PriorityBlocked("busy")))

        def priority() -> None:
            outcome = next(checks)
            if outcome is not None:
                raise outcome

        with mock.patch.object(
            bootstrap_llama_cpp.subprocess, "Popen", return_value=blocked
        ), mock.patch.object(bootstrap_llama_cpp.os, "killpg") as killpg, mock.patch.object(
            bootstrap_llama_cpp.time, "sleep"
        ), self.assertRaises(model_assets.PriorityBlocked):
            bootstrap_llama_cpp._run(["cmake"], {}, 10, priority_check=priority)
        killpg.assert_called_once_with(blocked.pid, bootstrap_llama_cpp.signal.SIGTERM)
        self.assertGreaterEqual(blocked.wait_calls, 1)

        timed_out = Process([None, None])
        with mock.patch.object(
            bootstrap_llama_cpp.subprocess, "Popen", return_value=timed_out
        ), mock.patch.object(bootstrap_llama_cpp.os, "killpg") as timeout_kill, mock.patch.object(
            bootstrap_llama_cpp.time, "monotonic", side_effect=[0.0, 2.0]
        ), self.assertRaisesRegex(model_assets.AssetError, "exceeded"):
            bootstrap_llama_cpp._run(
                ["cmake"], {}, 1, priority_check=lambda: None
            )
        timeout_kill.assert_called_once_with(
            timed_out.pid, bootstrap_llama_cpp.signal.SIGTERM
        )

    def test_build_group_force_kill_start_failure_and_invalid_timeout(self) -> None:
        class StubbornProcess:
            pid = 789

            def __init__(self) -> None:
                self.wait_calls = 0

            def poll(self) -> None:
                return None

            def wait(self, timeout: float | None = None) -> int:
                self.wait_calls += 1
                if self.wait_calls == 1:
                    raise subprocess.TimeoutExpired("cmake", timeout)
                return 0

        process = StubbornProcess()
        with mock.patch.object(bootstrap_llama_cpp.os, "killpg") as killpg:
            bootstrap_llama_cpp._terminate_process_group(process)  # type: ignore[arg-type]
        self.assertEqual(
            killpg.call_args_list,
            [
                mock.call(process.pid, bootstrap_llama_cpp.signal.SIGTERM),
                mock.call(process.pid, bootstrap_llama_cpp.signal.SIGKILL),
            ],
        )
        self.assertEqual(process.wait_calls, 2)

        with self.assertRaisesRegex(model_assets.AssetError, "positive"):
            bootstrap_llama_cpp._run(["cmake"], {}, 0, priority_check=lambda: None)
        with mock.patch.object(
            bootstrap_llama_cpp.subprocess,
            "Popen",
            side_effect=OSError("unavailable"),
        ), self.assertRaisesRegex(model_assets.AssetError, "cannot start"):
            bootstrap_llama_cpp._run(
                ["cmake"], {}, 1, priority_check=lambda: None
            )

    def test_staging_cleanup_is_exact_inode_and_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            staging = Path(tempfile.mkdtemp(prefix=".runtime.building-", dir=root))
            identity = bootstrap_llama_cpp._staging_identity(staging)
            (staging / "payload").write_bytes(b"x")
            bootstrap_llama_cpp._cleanup_owned_staging(staging, identity)
            self.assertFalse(staging.exists())

            replaced = Path(tempfile.mkdtemp(prefix=".runtime.building-", dir=root))
            expected = bootstrap_llama_cpp._staging_identity(replaced)
            moved = replaced.with_name(replaced.name + ".original")
            replaced.rename(moved)
            replaced.mkdir(mode=0o700)
            with self.assertRaisesRegex(model_assets.AssetError, "inode changed"):
                bootstrap_llama_cpp._cleanup_owned_staging(replaced, expected)
            self.assertTrue(replaced.is_dir())

            ordinary = root / "ordinary"
            ordinary.mkdir(mode=0o700)
            with self.assertRaisesRegex(model_assets.AssetError, "non-building"):
                bootstrap_llama_cpp._cleanup_owned_staging(
                    ordinary, bootstrap_llama_cpp._staging_identity(ordinary)
                )
            missing = root / ".missing.building-fixture"
            with self.assertRaisesRegex(model_assets.AssetError, "disappeared"):
                bootstrap_llama_cpp._cleanup_owned_staging(missing, (1, 2))

            public = Path(tempfile.mkdtemp(prefix=".public.building-", dir=root))
            os.chmod(public, 0o755)
            with self.assertRaisesRegex(model_assets.IntegrityError, "not private"):
                bootstrap_llama_cpp._staging_identity(public)

    def test_atomic_json_is_no_replace_and_runtime_name_is_pinned(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "receipt.json"
            bootstrap_llama_cpp._atomic_json(path, {"ok": True})
            self.assertEqual(json.loads(path.read_text(encoding="utf-8")), {"ok": True})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                bootstrap_llama_cpp._atomic_json(path, {"ok": False})
        self.assertTrue(bootstrap_llama_cpp._runtime_name(lock, "portable").endswith("-portable"))

    def test_runtime_receipt_rejects_malformed_metadata_and_mutation(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._runtime_fixture(root, lock)
            receipt_path = root / bootstrap_llama_cpp.RECEIPT_NAME
            receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
            receipt["profile"] = "native"
            receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "profile"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")
            receipt["profile"] = "portable"
            receipt["binaries"][0]["sha256"] = "0" * 64
            receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "no longer matches"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")
            receipt_path.write_text("[]", encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "not an object"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")
            receipt_path.write_text("{", encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "invalid"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

    def test_runtime_receipt_rejects_binary_inventory_shape_paths_and_duplicates(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._runtime_fixture(root, lock)
            receipt_path = root / bootstrap_llama_cpp.RECEIPT_NAME
            original = json.loads(receipt_path.read_text(encoding="utf-8"))
            variants = (
                ({**original, "binaries": []}, "no binary"),
                ({**original, "binaries": [{"path": "bad"}]}, "malformed"),
                (
                    {
                        **original,
                        "binaries": [
                            {
                                "path": "../../escape",
                                "sha256": "0" * 64,
                                "size_bytes": 1,
                            }
                        ],
                    },
                    "unsafe",
                ),
                (
                    {**original, "binaries": [original["binaries"][0]] * 2},
                    "repeats",
                ),
            )
            for receipt, marker in variants:
                receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
                with self.subTest(marker=marker), self.assertRaisesRegex(
                    model_assets.IntegrityError, marker
                ):
                    bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

    def test_runtime_receipt_matches_go_required_shape_and_strict_json(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._runtime_fixture(root, lock)
            receipt_path = root / bootstrap_llama_cpp.RECEIPT_NAME
            original = json.loads(receipt_path.read_text(encoding="utf-8"))
            self.assertEqual(
                set(original), set(bootstrap_llama_cpp.RUNTIME_RECEIPT_KEYS)
            )
            variants = (
                ("source_size_bytes", True, "source_size_bytes"),
                ("cmake", "", "cmake"),
                ("configure_argv", [], "configure_argv"),
                ("configure_argv", ["cmake", 1], "configure_argv"),
                ("build_argv", [], "build_argv"),
                ("platform", " ", "platform"),
                ("machine", None, "machine"),
                ("python", "", "python"),
            )
            for key, value, marker in variants:
                receipt = json.loads(json.dumps(original))
                receipt[key] = value
                receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
                with self.subTest(key=key, value=value), self.assertRaisesRegex(
                    model_assets.IntegrityError, marker
                ):
                    bootstrap_llama_cpp._verify_existing_runtime(
                        root, lock, "portable"
                    )

            receipt = json.loads(json.dumps(original))
            receipt["binaries"][0]["size_bytes"] = True
            receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "binary"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

            receipt = json.loads(json.dumps(original))
            receipt["license"]["size_bytes"] = True
            receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "license"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

            raw = json.dumps(original)
            duplicate = raw.replace(
                '"schema_version":',
                '"schema_version":"invalid","schema_version":',
                1,
            )
            receipt_path.write_text(duplicate, encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "repeats JSON key"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

            receipt_path.write_text(
                raw.replace(
                    f'"source_size_bytes": {original["source_size_bytes"]}',
                    '"source_size_bytes": NaN',
                    1,
                ),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(model_assets.IntegrityError, "JSON constant"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

            receipt_path.write_bytes(b"\xff")
            with self.assertRaisesRegex(model_assets.IntegrityError, "invalid"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

            receipt_path.write_text(
                raw.replace(
                    '"path": "source/LICENSE"',
                    '"path":"invalid","path": "source/LICENSE"',
                    1,
                ),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(model_assets.IntegrityError, "repeats JSON key"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

    def test_runtime_receipt_binds_retained_upstream_mit_license(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self._runtime_fixture(root, lock)
            receipt_path = root / bootstrap_llama_cpp.RECEIPT_NAME
            original = json.loads(receipt_path.read_text(encoding="utf-8"))
            license_path = root / bootstrap_llama_cpp.RUNTIME_LICENSE_RELATIVE
            payload = license_path.read_bytes()

            license_path.unlink()
            with self.assertRaisesRegex(model_assets.IntegrityError, "license"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

            license_path.write_bytes(payload + b"tampered")
            with self.assertRaisesRegex(model_assets.IntegrityError, "license"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

            license_path.write_bytes(payload)
            for key, value in (
                ("path", "LICENSE"),
                ("license_spdx", "Apache-2.0"),
                ("license_url", "https://example.invalid/LICENSE"),
            ):
                receipt = json.loads(json.dumps(original))
                receipt["license"][key] = value
                receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
                with self.subTest(key=key), self.assertRaisesRegex(
                    model_assets.IntegrityError, "license"
                ):
                    bootstrap_llama_cpp._verify_existing_runtime(
                        root, lock, "portable"
                    )

    def test_build_runtime_happy_path_is_fully_mocked(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "archive.tar.gz"
            archive.write_bytes(b"locked")
            runtime_root = root / "runtime"

            def fake_run(
                command: list[str],
                _environment: dict[str, str],
                _timeout: float,
            ) -> None:
                if "--build" not in command:
                    return
                build = Path(command[command.index("--build") + 1])
                targets = command[command.index("--target") + 1 :]
                for target in targets:
                    binary = build / "bin" / target
                    binary.parent.mkdir(parents=True, exist_ok=True)
                    binary.write_bytes((target + "\n").encode())
                    os.chmod(binary, 0o700)

            def fake_extract(
                _archive: Path,
                destination: Path,
                _extraction_root: str,
            ) -> None:
                (destination / "LICENSE").write_bytes(b"fixture MIT license\n")

            with mock.patch.object(bootstrap_llama_cpp, "assert_priority_available"), mock.patch.object(
                bootstrap_llama_cpp, "assert_resource_guard"
            ), mock.patch.object(bootstrap_llama_cpp, "fetch_asset", return_value=archive), mock.patch.object(
                bootstrap_llama_cpp, "verify_cached_asset", return_value=archive
            ), mock.patch.object(
                bootstrap_llama_cpp, "_extract_source", side_effect=fake_extract
            ), mock.patch.object(
                bootstrap_llama_cpp, "_cmake_version", return_value=(3, 31, "cmake 3.31")
            ), mock.patch.object(
                bootstrap_llama_cpp, "assert_disk_headroom"
            ) as disk_gate, mock.patch.object(
                bootstrap_llama_cpp, "_run", side_effect=fake_run
            ):
                runtime = bootstrap_llama_cpp.build_runtime(
                    lock, root / "downloads", runtime_root, "portable", "cmake"
                )
                self.assertTrue((runtime / bootstrap_llama_cpp.RECEIPT_NAME).is_file())
                reused = bootstrap_llama_cpp.build_runtime(
                    lock, root / "downloads", runtime_root, "portable", "cmake"
                )
                self.assertEqual(reused, runtime)
            disk_gate.assert_called_once_with(
                runtime_root,
                bootstrap_llama_cpp.RUNTIME_STAGING_WRITE_BYTES,
                "llama.cpp runtime staging",
            )

    def test_build_failure_quarantines_and_removes_owned_staging(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "archive.tar.gz"
            archive.write_bytes(b"locked")
            runtime_root = root / "runtime"
            with mock.patch.object(
                bootstrap_llama_cpp, "assert_priority_available"
            ), mock.patch.object(
                bootstrap_llama_cpp, "assert_resource_guard"
            ), mock.patch.object(
                bootstrap_llama_cpp, "fetch_asset", return_value=archive
            ), mock.patch.object(
                bootstrap_llama_cpp, "verify_cached_asset", return_value=archive
            ), mock.patch.object(
                bootstrap_llama_cpp,
                "_cmake_version",
                return_value=(3, 31, "cmake 3.31"),
            ), mock.patch.object(
                bootstrap_llama_cpp, "assert_disk_headroom"
            ), mock.patch.object(
                bootstrap_llama_cpp,
                "_extract_source",
                side_effect=model_assets.IntegrityError("bad archive"),
            ), self.assertRaisesRegex(model_assets.IntegrityError, "bad archive"):
                bootstrap_llama_cpp.build_runtime(
                    lock,
                    root / "downloads",
                    runtime_root,
                    "portable",
                    "cmake",
                )
            self.assertEqual(list(runtime_root.glob("*.building-*")), [])
            self.assertEqual(list(runtime_root.glob("*.failed-*")), [])

    def test_main_maps_success_priority_and_integrity_failures(self) -> None:
        lock = model_assets.load_lock()
        runtime = Path("/fixture/runtime")
        with mock.patch.object(bootstrap_llama_cpp, "load_lock", return_value=lock), mock.patch.object(
            bootstrap_llama_cpp, "build_runtime", return_value=runtime
        ), mock.patch("sys.stdout", new=io.StringIO()) as output:
            self.assertEqual(bootstrap_llama_cpp.main([]), 0)
            self.assertTrue(json.loads(output.getvalue())["ok"])
        with mock.patch.object(
            bootstrap_llama_cpp, "load_lock", side_effect=model_assets.PriorityBlocked("busy")
        ), mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(bootstrap_llama_cpp.main([]), 75)
        with mock.patch.object(
            bootstrap_llama_cpp, "load_lock", side_effect=model_assets.LockError("bad")
        ), mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(bootstrap_llama_cpp.main([]), 1)


if __name__ == "__main__":
    unittest.main()
