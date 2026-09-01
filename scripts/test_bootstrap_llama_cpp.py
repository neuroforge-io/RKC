#!/usr/bin/env python3
"""Unit tests for the checksum-pinned llama.cpp runtime receipt."""
from __future__ import annotations

import hashlib
import io
import json
import os
import runpy
import shutil
import subprocess
import sys
import stat
import tarfile
import tempfile
import unittest
from contextlib import ExitStack
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
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
            regular = SimpleNamespace(st_mode=stat.S_IFREG | 0o600, st_size=0, st_dev=1, st_ino=2)
            with mock.patch.object(bootstrap_llama_cpp.os, "open", return_value=41), mock.patch.object(
                bootstrap_llama_cpp.os, "fstat", return_value=regular
            ), mock.patch.object(
                bootstrap_llama_cpp.os, "read", side_effect=[b"too-long", b""]
            ), mock.patch.object(bootstrap_llama_cpp.os, "close"):
                with self.assertRaisesRegex(model_assets.IntegrityError, "exceeds"):
                    bootstrap_llama_cpp._sha256_file(
                        file, maximum_bytes=2, priority_check=lambda: None
                    )
            changed_inode = SimpleNamespace(
                st_mode=regular.st_mode,
                st_size=3,
                st_dev=1,
                st_ino=99,
            )
            with mock.patch.object(bootstrap_llama_cpp.os, "open", return_value=42), mock.patch.object(
                bootstrap_llama_cpp.os,
                "fstat",
                side_effect=[regular, changed_inode],
            ), mock.patch.object(
                bootstrap_llama_cpp.os, "read", side_effect=[b"abc", b""]
            ), mock.patch.object(bootstrap_llama_cpp.os, "close"):
                with self.assertRaisesRegex(model_assets.IntegrityError, "changed"):
                    bootstrap_llama_cpp._sha256_file(
                        file, maximum_bytes=10, priority_check=lambda: None
                    )
            same_inode_different_size = SimpleNamespace(
                st_mode=regular.st_mode,
                st_size=4,
                st_dev=1,
                st_ino=2,
            )
            pathname = SimpleNamespace(st_dev=1, st_ino=2)
            with mock.patch.object(bootstrap_llama_cpp.os, "open", return_value=43), mock.patch.object(
                bootstrap_llama_cpp.os,
                "fstat",
                side_effect=[regular, same_inode_different_size],
            ), mock.patch.object(
                bootstrap_llama_cpp.os, "lstat", return_value=pathname
            ), mock.patch.object(
                bootstrap_llama_cpp.os, "read", side_effect=[b"abc", b""]
            ), mock.patch.object(bootstrap_llama_cpp.os, "close"):
                with self.assertRaisesRegex(model_assets.IntegrityError, "size changed"):
                    bootstrap_llama_cpp._sha256_file(
                        file, maximum_bytes=10, priority_check=lambda: None
                    )
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
            with mock.patch.object(bootstrap_llama_cpp.os, "getuid", return_value=os.getuid() + 1):
                with self.assertRaisesRegex(model_assets.AssetError, "owned"):
                    bootstrap_llama_cpp._private_directory(nested)

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

    def test_extract_source_covers_directories_stream_boundaries_and_priority(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)

            def write_archive(path: Path, members: list[tuple[str, str, bytes | None]]) -> None:
                with tarfile.open(path, "w:gz") as archive:
                    for name, kind, payload in members:
                        member = tarfile.TarInfo(name)
                        if kind == "dir":
                            member.type = tarfile.DIRTYPE
                            archive.addfile(member)
                        else:
                            data = payload or b""
                            member.size = len(data)
                            member.mode = 0o644
                            archive.addfile(member, io.BytesIO(data))

            tree_members = [
                ("llama-root", "dir", None),
                ("llama-root/docs", "dir", None),
                ("llama-root/CMakeLists.txt", "file", b"cmake\n"),
                ("llama-root/LICENSE", "file", b"MIT\n"),
                ("llama-root/ggml/CMakeLists.txt", "file", b"ggml\n"),
                ("llama-root/docs/readme.md", "file", b"docs\n"),
            ]
            archive = root / "tree.tar.gz"
            write_archive(archive, tree_members)
            destination = root / "tree"
            destination.mkdir()
            checks: list[str] = []
            with mock.patch.object(bootstrap_llama_cpp, "PRIORITY_RECHECK_BYTES", 1):
                bootstrap_llama_cpp._extract_source(
                    archive, destination, "llama-root", priority_check=lambda: checks.append("check")
                )
            self.assertEqual((destination / "docs/readme.md").read_bytes(), b"docs\n")
            self.assertGreaterEqual(len(checks), 2)

            total_limited = root / "total-limited"
            total_limited.mkdir()
            with mock.patch.object(bootstrap_llama_cpp, "MAX_ARCHIVE_TOTAL_BYTES", 1), self.assertRaisesRegex(
                model_assets.IntegrityError, "expands"
            ):
                bootstrap_llama_cpp._extract_source(archive, total_limited, "llama-root")

            stream_none = root / "stream-none"
            stream_none.mkdir()
            with mock.patch.object(tarfile.TarFile, "extractfile", return_value=None), self.assertRaisesRegex(
                model_assets.IntegrityError, "cannot read"
            ):
                bootstrap_llama_cpp._extract_source(archive, stream_none, "llama-root")

            truncated = root / "truncated"
            truncated.mkdir()
            with mock.patch.object(tarfile.TarFile, "extractfile", return_value=io.BytesIO()), self.assertRaisesRegex(
                model_assets.IntegrityError, "truncated"
            ):
                bootstrap_llama_cpp._extract_source(archive, truncated, "llama-root")

            oversized = root / "oversized"
            oversized.mkdir()
            with mock.patch.object(
                tarfile.TarFile, "extractfile", return_value=io.BytesIO(b"x" * 100)
            ), self.assertRaisesRegex(model_assets.IntegrityError, "oversized"):
                bootstrap_llama_cpp._extract_source(archive, oversized, "llama-root")

            short_write = root / "short-write"
            short_write.mkdir()
            with mock.patch.object(bootstrap_llama_cpp.os, "write", return_value=0), self.assertRaisesRegex(
                OSError, "short write"
            ):
                bootstrap_llama_cpp._extract_source(archive, short_write, "llama-root")

            nonregular = root / "nonregular-required.tar.gz"
            write_archive(
                nonregular,
                [
                    ("llama-root", "dir", None),
                    ("llama-root/CMakeLists.txt", "file", b"cmake\n"),
                    ("llama-root/LICENSE", "dir", None),
                    ("llama-root/ggml", "dir", None),
                    ("llama-root/ggml/CMakeLists.txt", "file", b"ggml\n"),
                ],
            )
            nonregular_out = root / "nonregular-required"
            nonregular_out.mkdir()
            with self.assertRaisesRegex(model_assets.IntegrityError, "missing regular LICENSE"):
                bootstrap_llama_cpp._extract_source(nonregular, nonregular_out, "llama-root")

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

    def test_build_group_windows_termination_and_lookup_races(self) -> None:
        class WindowsProcess:
            pid = 321

            def __init__(self, *, lookup_race: bool) -> None:
                self.lookup_race = lookup_race
                self.wait_calls = 0
                self.terminated = False
                self.killed = False

            def poll(self) -> None:
                return None

            def terminate(self) -> None:
                self.terminated = True
                if self.lookup_race:
                    raise ProcessLookupError()

            def kill(self) -> None:
                self.killed = True
                if self.lookup_race:
                    raise ProcessLookupError()

            def wait(self, timeout: float | None = None) -> int:
                self.wait_calls += 1
                if self.wait_calls == 1:
                    raise subprocess.TimeoutExpired("cmake", timeout)
                return 0

        normal = WindowsProcess(lookup_race=False)
        with mock.patch.object(bootstrap_llama_cpp.os, "name", "nt"):
            bootstrap_llama_cpp._terminate_process_group(normal)  # type: ignore[arg-type]
        self.assertTrue(normal.terminated)
        self.assertTrue(normal.killed)
        self.assertEqual(normal.wait_calls, 2)

        lookup_race = WindowsProcess(lookup_race=True)
        with mock.patch.object(bootstrap_llama_cpp.os, "name", "nt"):
            bootstrap_llama_cpp._terminate_process_group(lookup_race)  # type: ignore[arg-type]
        self.assertTrue(lookup_race.terminated)
        self.assertTrue(lookup_race.killed)
        self.assertEqual(lookup_race.wait_calls, 2)

    def test_run_uses_default_priority_check(self) -> None:
        class CompleteProcess:
            pid = 12

            def poll(self) -> int:
                return 0

            def wait(self, timeout: float | None = None) -> int:
                return 0

        with mock.patch.object(
            bootstrap_llama_cpp.subprocess, "Popen", return_value=CompleteProcess()
        ), mock.patch.object(
            bootstrap_llama_cpp, "assert_priority_available"
        ) as priority:
            bootstrap_llama_cpp._run(["cmake"], {}, 1)
        self.assertGreaterEqual(priority.call_count, 2)

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
            regular = root / "regular"
            regular.write_bytes(b"x")
            with self.assertRaisesRegex(model_assets.IntegrityError, "real directory"):
                bootstrap_llama_cpp._staging_identity(regular)
            owned = Path(tempfile.mkdtemp(prefix=".owned.building-", dir=root))
            with mock.patch.object(bootstrap_llama_cpp.os, "getuid", return_value=os.getuid() + 1):
                with self.assertRaisesRegex(model_assets.IntegrityError, "owned"):
                    bootstrap_llama_cpp._staging_identity(owned)

    def test_staging_cleanup_handles_rename_identity_and_remove_failures(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            staging = Path(tempfile.mkdtemp(prefix=".runtime.building-", dir=root))
            identity = bootstrap_llama_cpp._staging_identity(staging)
            with mock.patch.object(
                bootstrap_llama_cpp.os, "rename", side_effect=OSError("rename blocked")
            ), mock.patch.object(bootstrap_llama_cpp.os, "rmdir", wraps=os.rmdir) as rmdir:
                with self.assertRaisesRegex(OSError, "rename blocked"):
                    bootstrap_llama_cpp._cleanup_owned_staging(staging, identity)
                self.assertEqual(rmdir.call_count, 1)
            self.assertTrue(staging.exists())

            retained = Path(tempfile.mkdtemp(prefix=".runtime.building-", dir=root))
            retained_identity = bootstrap_llama_cpp._staging_identity(retained)
            identities = iter((retained_identity, (retained_identity[0], retained_identity[1] + 1)))
            with mock.patch.object(
                bootstrap_llama_cpp, "_staging_identity", side_effect=lambda _path: next(identities)
            ):
                with self.assertRaisesRegex(model_assets.AssetError, "during quarantine"):
                    bootstrap_llama_cpp._cleanup_owned_staging(retained, retained_identity)
            for path in root.glob("*.failed-*"):
                if path.is_dir():
                    shutil.rmtree(path)

            remove_failure = Path(tempfile.mkdtemp(prefix=".runtime.building-", dir=root))
            remove_identity = bootstrap_llama_cpp._staging_identity(remove_failure)
            with mock.patch.object(
                bootstrap_llama_cpp.shutil, "rmtree", side_effect=OSError("remove blocked")
            ):
                with self.assertRaisesRegex(model_assets.AssetError, "cannot remove"):
                    bootstrap_llama_cpp._cleanup_owned_staging(remove_failure, remove_identity)
            for path in root.glob("*.failed-*"):
                if path.is_dir():
                    shutil.rmtree(path)

    def test_atomic_json_is_no_replace_and_runtime_name_is_pinned(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "receipt.json"
            bootstrap_llama_cpp._atomic_json(path, {"ok": True})
            self.assertEqual(json.loads(path.read_text(encoding="utf-8")), {"ok": True})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                bootstrap_llama_cpp._atomic_json(path, {"ok": False})
            with mock.patch.object(bootstrap_llama_cpp.os, "write", return_value=0), self.assertRaisesRegex(
                OSError, "short write"
            ):
                bootstrap_llama_cpp._atomic_json(Path(temporary) / "short.json", {"ok": True})
        name = bootstrap_llama_cpp._runtime_name(lock, "portable")
        self.assertTrue(name.endswith("-portable"))
        self.assertIn(lock.digest[:12], name)

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

            malformed_shape = json.loads(json.dumps(original))
            malformed_shape.pop("runtime")
            receipt_path.write_text(json.dumps(malformed_shape), encoding="utf-8")
            with self.assertRaisesRegex(model_assets.IntegrityError, "shape"):
                bootstrap_llama_cpp._verify_existing_runtime(root, lock, "portable")

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

            receipt_path.write_bytes(b"x" * (1024 * 1024 + 1))
            with self.assertRaisesRegex(model_assets.IntegrityError, "oversized"):
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

            license_path.write_bytes(b"")
            with self.assertRaisesRegex(model_assets.IntegrityError, "empty"):
                bootstrap_llama_cpp._runtime_license_record(root, lock)

            llama = dict(lock.document["llama_cpp"])  # type: ignore[arg-type]
            llama["license_spdx"] = "Apache-2.0"
            mismatched_lock = SimpleNamespace(
                document={"llama_cpp": llama},
                asset=lambda _asset_id: lock.asset(str(llama["source_asset_id"])),
            )
            with self.assertRaisesRegex(model_assets.IntegrityError, "metadata"):
                bootstrap_llama_cpp._runtime_license_record(root, mismatched_lock)  # type: ignore[arg-type]

    def test_build_runtime_happy_path_is_fully_mocked(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "archive.tar.gz"
            archive.write_bytes(b"locked")
            runtime_root = root / "runtime"
            build_environments: list[dict[str, str]] = []

            def fake_run(
                command: list[str],
                environment: dict[str, str],
                _timeout: float,
            ) -> None:
                build_environments.append(environment)
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
            self.assertEqual(len(build_environments), 2)
            for environment in build_environments:
                ceiling = Path(environment["GIT_CEILING_DIRECTORIES"])
                self.assertEqual(ceiling.name, "source")
                self.assertEqual(ceiling.parent.parent, runtime_root)
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

    def test_build_runtime_rejects_old_cmake_and_missing_extraction_root(self) -> None:
        lock = model_assets.load_lock()
        source_asset_id = str(lock.document["llama_cpp"]["source_asset_id"])  # type: ignore[index]
        assets = tuple(
            replace(asset, extraction_root=None)
            if asset.asset_id == source_asset_id
            else asset
            for asset in lock.assets
        )
        missing_root_lock = replace(lock, assets=assets)
        for current_lock, version, marker in (
            (lock, (3, 13, "cmake 3.13"), "required"),
            (missing_root_lock, (3, 31, "cmake 3.31"), "extraction root"),
        ):
            with tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive = root / "archive.tar.gz"
                archive.write_bytes(b"locked")
                runtime_root = root / "runtime"
                with mock.patch.object(bootstrap_llama_cpp, "assert_priority_available"), mock.patch.object(
                    bootstrap_llama_cpp, "assert_resource_guard"
                ), mock.patch.object(bootstrap_llama_cpp, "fetch_asset", return_value=archive), mock.patch.object(
                    bootstrap_llama_cpp, "verify_cached_asset", return_value=archive
                ), mock.patch.object(
                    bootstrap_llama_cpp, "_cmake_version", return_value=version
                ), mock.patch.object(bootstrap_llama_cpp, "assert_disk_headroom"):
                    with self.subTest(marker=marker), self.assertRaisesRegex(
                        model_assets.AssetError if marker == "required" else model_assets.IntegrityError,
                        marker,
                    ):
                        bootstrap_llama_cpp.build_runtime(
                            current_lock,
                            root / "downloads",
                            runtime_root,
                            "portable",
                            "cmake",
                        )

    def test_build_runtime_rejects_nonregular_and_nonexecutable_outputs(self) -> None:
        lock = model_assets.load_lock()
        for output_kind in ("directory", "non-executable"):
            with tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                archive = root / "archive.tar.gz"
                archive.write_bytes(b"locked")
                runtime_root = root / "runtime"

                def fake_extract(
                    _archive: Path, destination: Path, _extraction_root: str
                ) -> None:
                    (destination / "LICENSE").write_bytes(b"fixture MIT license\n")

                def fake_run(
                    command: list[str], _environment: dict[str, str], _timeout: float
                ) -> None:
                    if "--build" not in command:
                        return
                    build = Path(command[command.index("--build") + 1])
                    targets = command[command.index("--target") + 1 :]
                    for target in targets:
                        binary = build / "bin" / target
                        binary.parent.mkdir(parents=True, exist_ok=True)
                        if output_kind == "directory":
                            binary.mkdir()
                        else:
                            binary.write_bytes(b"not executable\n")
                            os.chmod(binary, 0o600)

                with mock.patch.object(bootstrap_llama_cpp, "assert_priority_available"), mock.patch.object(
                    bootstrap_llama_cpp, "assert_resource_guard"
                ), mock.patch.object(bootstrap_llama_cpp, "fetch_asset", return_value=archive), mock.patch.object(
                    bootstrap_llama_cpp, "verify_cached_asset", return_value=archive
                ), mock.patch.object(
                    bootstrap_llama_cpp, "_extract_source", side_effect=fake_extract
                ), mock.patch.object(
                    bootstrap_llama_cpp, "_cmake_version", return_value=(3, 31, "cmake 3.31")
                ), mock.patch.object(bootstrap_llama_cpp, "assert_disk_headroom"), mock.patch.object(
                    bootstrap_llama_cpp, "_run", side_effect=fake_run
                ):
                    marker = "regular" if output_kind == "directory" else "executable"
                    with self.subTest(output_kind=output_kind), self.assertRaisesRegex(
                        model_assets.IntegrityError, marker
                    ):
                        bootstrap_llama_cpp.build_runtime(
                            lock,
                            root / "downloads",
                            runtime_root,
                            "portable",
                            "cmake",
                        )

    def test_build_runtime_handles_concurrent_publish_and_postpublish_verify(self) -> None:
        lock = model_assets.load_lock()

        def execute(
            *, concurrent: bool
        ) -> tuple[Path, Path, BaseException]:
            temporary = tempfile.TemporaryDirectory()
            self.addCleanup(temporary.cleanup)
            root = Path(temporary.name)
            archive = root / "archive.tar.gz"
            archive.write_bytes(b"locked")
            runtime_root = root / "runtime"
            final = runtime_root / bootstrap_llama_cpp._runtime_name(lock, "portable")

            def fake_extract(
                _archive: Path, destination: Path, _extraction_root: str
            ) -> None:
                (destination / "LICENSE").write_bytes(b"fixture MIT license\n")

            def fake_run(
                command: list[str], _environment: dict[str, str], _timeout: float
            ) -> None:
                if "--build" not in command:
                    return
                build = Path(command[command.index("--build") + 1])
                for target in command[command.index("--target") + 1 :]:
                    binary = build / "bin" / target
                    binary.parent.mkdir(parents=True, exist_ok=True)
                    binary.write_bytes((target + "\n").encode())
                    os.chmod(binary, 0o700)

            real_rename = bootstrap_llama_cpp.os.rename
            rename_calls = 0

            def rename(source: str | os.PathLike[str], destination: str | os.PathLike[str]) -> None:
                nonlocal rename_calls
                rename_calls += 1
                if concurrent and rename_calls == 1:
                    raise FileExistsError("published concurrently")
                real_rename(source, destination)

            patches = [
                mock.patch.object(bootstrap_llama_cpp, "assert_priority_available"),
                mock.patch.object(bootstrap_llama_cpp, "assert_resource_guard"),
                mock.patch.object(bootstrap_llama_cpp, "fetch_asset", return_value=archive),
                mock.patch.object(bootstrap_llama_cpp, "verify_cached_asset", return_value=archive),
                mock.patch.object(bootstrap_llama_cpp, "_extract_source", side_effect=fake_extract),
                mock.patch.object(bootstrap_llama_cpp, "_cmake_version", return_value=(3, 31, "cmake 3.31")),
                mock.patch.object(bootstrap_llama_cpp, "assert_disk_headroom"),
                mock.patch.object(bootstrap_llama_cpp, "_run", side_effect=fake_run),
                mock.patch.object(bootstrap_llama_cpp.os, "rename", side_effect=rename),
            ]
            if concurrent:
                patches.append(mock.patch.object(bootstrap_llama_cpp, "_verify_existing_runtime", return_value={}))
            else:
                patches.append(
                    mock.patch.object(
                        bootstrap_llama_cpp,
                        "_verify_existing_runtime",
                        side_effect=model_assets.IntegrityError("post-publish verify failed"),
                    )
                )
            with ExitStack() as stack:
                for patcher in patches:
                    stack.enter_context(patcher)
                try:
                    bootstrap_llama_cpp.build_runtime(
                        lock, root / "downloads", runtime_root, "portable", "cmake"
                    )
                except BaseException as exc:
                    return root, final, exc
            raise AssertionError("mocked build unexpectedly succeeded")

        root, final, concurrent_error = execute(concurrent=True)
        self.assertIsInstance(concurrent_error, model_assets.AssetError)
        self.assertRegex(str(concurrent_error), "concurrent identical")
        self.assertEqual(list((root / "runtime").glob("*.building-*")), [])
        self.assertEqual(list((root / "runtime").glob("*.failed-*")), [])

        root, final, postpublish_error = execute(concurrent=False)
        self.assertIsInstance(postpublish_error, model_assets.IntegrityError)
        self.assertRegex(str(postpublish_error), "post-publish")
        self.assertTrue(final.is_dir())

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

    def test_script_guard_exposes_help_without_starting_a_build(self) -> None:
        with mock.patch.object(sys, "argv", ["bootstrap_llama_cpp.py", "--help"]):
            with self.assertRaises(SystemExit) as raised:
                runpy.run_path(
                    str(Path(bootstrap_llama_cpp.__file__).resolve()),
                    run_name="__main__",
                )
        self.assertEqual(raised.exception.code, 0)


if __name__ == "__main__":
    unittest.main()
