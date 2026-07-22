#!/usr/bin/env python3
"""Unit tests for the immutable model asset downloader."""
from __future__ import annotations

import hashlib
import io
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
import urllib.error
import urllib.request
from copy import deepcopy
from pathlib import Path
from unittest import mock

import model_assets


class FakeResponse:
    def __init__(
        self,
        payload: bytes,
        url: str,
        *,
        content_length: int | None = None,
        content_encoding: str | None = None,
        status: int = 200,
    ) -> None:
        self.stream = io.BytesIO(payload)
        self.url = url
        self.status = status
        self.headers: dict[str, str] = {}
        if content_length is not None:
            self.headers["Content-Length"] = str(content_length)
        if content_encoding is not None:
            self.headers["Content-Encoding"] = content_encoding

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        self.stream.close()

    def read(self, size: int = -1) -> bytes:
        return self.stream.read(size)

    def getcode(self) -> int:
        return self.status

    def geturl(self) -> str:
        return self.url


class FakeOpener:
    def __init__(self, response: FakeResponse) -> None:
        self.response = response
        self.requests = []

    def open(self, request, timeout: int):  # type: ignore[no-untyped-def]
        self.requests.append((request, timeout))
        return self.response


def fixture_asset(payload: bytes) -> model_assets.Asset:
    revision = "1" * 40
    return model_assets.Asset(
        asset_id="fixture",
        kind="generation-model",
        status="unqualified",
        default_eligible=False,
        repository="https://fixtures.example/repository",
        revision=revision,
        filename="fixture.gguf",
        url=f"https://fixtures.example/resolve/{revision}/fixture.gguf",
        allowed_hosts=("fixtures.example", ".content.example"),
        sha256=hashlib.sha256(payload).hexdigest(),
        size_bytes=len(payload),
        license_spdx="Apache-2.0",
        license_url="https://fixtures.example/LICENSE",
        redistribution="not-bundled-download-on-demand",
        quantization="Q4_K_M",
        native_context_tokens=32768,
        qualification_spec="models/qualification/fixture.json",
        extraction_root=None,
    )


class ModelAssetTests(unittest.TestCase):
    def test_priority_match_ignores_wrapper_ancestor_only(self) -> None:
        ancestors = {100, 101}
        wrapper = b"/bin/bash -lc echo erais; run child"
        self.assertFalse(
            model_assets._matches_priority_process(100, wrapper, b"bash", ancestors)
        )
        self.assertTrue(
            model_assets._matches_priority_process(
                101,
                b"/usr/bin/torchrun train.py",
                b"torchrun",
                ancestors,
            )
        )
        self.assertTrue(
            model_assets._matches_priority_process(
                202,
                b"python /home/lloyd/erais/train.py",
                b"python3",
                ancestors,
            )
        )

    def test_fetch_hashes_and_publishes_without_replacement(self) -> None:
        payload = (b"verified-model-bytes\n" * 257) + b"end"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"
            opener = FakeOpener(
                FakeResponse(payload, asset.url, content_length=len(payload))
            )
            path = model_assets.fetch_asset(
                asset,
                cache,
                require_guard=False,
                opener=opener,
                priority_check=lambda: None,
            )
            self.assertEqual(path.read_bytes(), payload)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertFalse(
                any(item.name.endswith(".part") for item in cache.iterdir())
            )
            self.assertEqual(len(opener.requests), 1)

            reused = model_assets.fetch_asset(
                asset,
                cache,
                require_guard=False,
                opener=FakeOpener(FakeResponse(b"wrong", asset.url)),
                priority_check=lambda: None,
            )
            self.assertEqual(reused, path)
            self.assertEqual(path.read_bytes(), payload)

    def test_hash_mismatch_is_never_published(self) -> None:
        payload = b"expected"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"
            response = FakeResponse(b"tampered", asset.url, content_length=len(payload))
            with self.assertRaises(model_assets.IntegrityError):
                model_assets.fetch_asset(
                    asset,
                    cache,
                    require_guard=False,
                    opener=FakeOpener(response),
                    priority_check=lambda: None,
                )
            self.assertFalse((cache / asset.filename).exists())
            self.assertEqual(list(cache.iterdir()), [])

    def test_disk_gate_runs_before_download_temporary_creation(self) -> None:
        payload = b"expected"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"
            opener = FakeOpener(FakeResponse(payload, asset.url))
            with mock.patch.object(
                model_assets,
                "assert_disk_headroom",
                side_effect=model_assets.AssetError("insufficient disk headroom"),
            ), self.assertRaisesRegex(model_assets.AssetError, "disk headroom"):
                model_assets.fetch_asset(
                    asset,
                    cache,
                    require_guard=False,
                    opener=opener,
                    priority_check=lambda: None,
                )
            self.assertEqual(opener.requests, [])
            self.assertEqual(list(cache.iterdir()), [])

    def test_size_header_mismatch_is_rejected_before_publication(self) -> None:
        payload = b"expected"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"
            response = FakeResponse(payload, asset.url, content_length=len(payload) + 1)
            with self.assertRaisesRegex(
                model_assets.IntegrityError, "size header mismatch"
            ):
                model_assets.fetch_asset(
                    asset,
                    cache,
                    require_guard=False,
                    opener=FakeOpener(response),
                    priority_check=lambda: None,
                )
            self.assertEqual(list(cache.iterdir()), [])

    @unittest.skipUnless(hasattr(os, "symlink"), "symlink support required")
    def test_existing_symlink_is_rejected_and_not_replaced(self) -> None:
        payload = b"expected"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cache = root / "cache"
            cache.mkdir(mode=0o700)
            outside = root / "outside"
            outside.write_bytes(payload)
            (cache / asset.filename).symlink_to(outside)
            with self.assertRaises(model_assets.IntegrityError):
                model_assets.fetch_asset(
                    asset,
                    cache,
                    require_guard=False,
                    opener=FakeOpener(FakeResponse(payload, asset.url)),
                    priority_check=lambda: None,
                )
            self.assertTrue((cache / asset.filename).is_symlink())
            self.assertEqual(outside.read_bytes(), payload)

    def test_off_policy_redirect_host_is_rejected(self) -> None:
        asset = fixture_asset(b"expected")
        with self.assertRaisesRegex(model_assets.IntegrityError, "unapproved host"):
            model_assets._validate_fetch_url(
                "https://attacker.example/payload.gguf", asset
            )

    def test_https_downgrade_is_rejected(self) -> None:
        asset = fixture_asset(b"expected")
        with self.assertRaisesRegex(
            model_assets.IntegrityError, "credential-free HTTPS"
        ):
            model_assets._validate_fetch_url(
                "http://fixtures.example/payload.gguf", asset
            )

    def test_lock_access_and_strict_json_fail_closed(self) -> None:
        lock = model_assets.load_lock()
        self.assertEqual(lock.asset(lock.assets[0].asset_id), lock.assets[0])
        with self.assertRaisesRegex(model_assets.LockError, "uniquely"):
            lock.asset("absent")
        duplicate = model_assets.ModelLock(
            path=lock.path,
            digest=lock.digest,
            document=lock.document,
            assets=(lock.assets[0], lock.assets[0]),
        )
        with self.assertRaisesRegex(model_assets.LockError, "uniquely"):
            duplicate.asset(lock.assets[0].asset_id)
        with self.assertRaisesRegex(model_assets.LockError, "duplicate"):
            model_assets._strict_object([("x", 1), ("x", 2)])

    def test_regular_lock_reader_rejects_link_size_and_non_regular(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            path = root / "lock"
            path.write_bytes(b"value")
            self.assertEqual(model_assets._read_regular(path, 10), b"value")
            with self.assertRaisesRegex(model_assets.LockError, "exceeds"):
                model_assets._read_regular(path, 1)
            with self.assertRaisesRegex(model_assets.LockError, "regular"):
                model_assets._read_regular(root, 10)
            if hasattr(os, "symlink"):
                link = root / "link"
                link.symlink_to(path)
                with self.assertRaises(model_assets.LockError):
                    model_assets._read_regular(link, 10)

    def test_scalar_url_host_and_cmake_validators(self) -> None:
        self.assertEqual(model_assets._string("x", "x"), "x")
        self.assertTrue(model_assets._boolean(True, "x"))
        self.assertEqual(model_assets._integer(3, "x"), 3)
        self.assertIsNone(model_assets._optional_string(None, "x"))
        self.assertIsNone(model_assets._optional_integer(None, "x"))
        for function, value in (
            (model_assets._string, 1),
            (model_assets._boolean, 1),
            (model_assets._integer, True),
        ):
            with self.subTest(function=function.__name__), self.assertRaises(
                model_assets.LockError
            ):
                function(value, "x")
        for url in (
            "http://example.test/path",
            "https://user@example.test/path",
            "https://127.0.0.1/path",
            "https://example.test/path#fragment",
        ):
            with self.subTest(url=url), self.assertRaises(model_assets.LockError):
                model_assets._https_url(url, "url")
        self.assertTrue(model_assets._host_allowed("exact.example", ("exact.example",)))
        self.assertTrue(
            model_assets._host_allowed("a.content.example", (".content.example",))
        )
        self.assertFalse(
            model_assets._host_allowed("content.example", (".content.example",))
        )
        self.assertEqual(
            model_assets._validate_hosts(["EXAMPLE.test", ".content.test"], "hosts"),
            ("example.test", ".content.test"),
        )
        for hosts in ([], ["bad host"], ["same.test", "same.test"]):
            with self.subTest(hosts=hosts), self.assertRaises(model_assets.LockError):
                model_assets._validate_hosts(hosts, "hosts")

        llama = deepcopy(model_assets.load_lock().document["llama_cpp"])
        model_assets._validate_cmake(llama)
        for key, value in (
            ("minimum_version", "3"),
            ("generator", "Visual Studio"),
            ("build_type", "Debug"),
            ("parallel_jobs", 3),
            ("targets", ["llama-cli"]),
            ("common_options", ["unsafe"]),
        ):
            changed = deepcopy(llama)
            changed["cmake"][key] = value
            with self.subTest(key=key), self.assertRaises(model_assets.LockError):
                model_assets._validate_cmake(changed)

    def test_asset_parser_rejects_policy_mutations(self) -> None:
        lock = model_assets.load_lock()
        generation = deepcopy(lock.document["assets"][1])
        parsed = model_assets._parse_asset(generation, 1)
        self.assertEqual(parsed.kind, "generation-model")
        mutations = (
            ("id", "BAD ID"),
            ("kind", "other"),
            ("status", "runtime-pinned"),
            ("filename", "../model.gguf"),
            ("revision", "short"),
            ("sha256", "A" * 64),
            ("size_bytes", 0),
            ("allowed_hosts", ["other.example"]),
            ("license_spdx", "MIT"),
            ("redistribution", "bundled"),
            ("quantization", None),
            ("native_context_tokens", 0),
            ("qualification_spec", "../escape.json"),
            ("extraction_root", "unexpected"),
            ("default_eligible", True),
        )
        for key, value in mutations:
            changed = deepcopy(generation)
            changed[key] = value
            with self.subTest(key=key), self.assertRaises(model_assets.LockError):
                model_assets._parse_asset(changed, 1)
        with self.assertRaises(model_assets.LockError):
            model_assets._parse_asset([], 0)

        source = deepcopy(lock.document["assets"][0])
        for key, value in (
            ("status", "unqualified"),
            ("quantization", "Q4"),
            ("extraction_root", None),
        ):
            changed = deepcopy(source)
            changed[key] = value
            with self.subTest(source_key=key), self.assertRaises(
                model_assets.LockError
            ):
                model_assets._parse_asset(changed, 0)

    def test_load_lock_rejects_duplicate_json_and_invalid_defaults(self) -> None:
        lock = model_assets.load_lock()
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "lock.json"
            path.write_text('{"x":1,"x":2}', encoding="utf-8")
            with self.assertRaisesRegex(model_assets.LockError, "duplicate"):
                model_assets.load_lock(path)
            document = deepcopy(lock.document)
            document["default_generation_model"] = lock.assets[1].asset_id
            path.write_text(json.dumps(document), encoding="utf-8")
            with self.assertRaisesRegex(model_assets.LockError, "qualified"):
                model_assets.load_lock(path)
            document = deepcopy(lock.document)
            document["llama_cpp"]["source_asset_id"] = "missing"
            path.write_text(json.dumps(document), encoding="utf-8")
            with self.assertRaisesRegex(model_assets.LockError, "pinned commit"):
                model_assets.load_lock(path)

    @mock.patch.object(model_assets.subprocess, "run")
    def test_non_linux_priority_fallback_and_assertion(self, run: mock.Mock) -> None:
        run.return_value = subprocess.CompletedProcess(
            [], 0, stdout=b"123 python /work/erais/train.py\n", stderr=b""
        )
        with mock.patch.object(model_assets.sys, "platform", "darwin"):
            matches = model_assets.active_priority_processes()
        self.assertEqual(matches[0][0], 123)
        with mock.patch.object(
            model_assets, "active_priority_processes", return_value=matches
        ):
            with self.assertRaisesRegex(model_assets.PriorityBlocked, "priority"):
                model_assets.assert_priority_available()
        run.side_effect = FileNotFoundError("pgrep")
        with mock.patch.object(
            model_assets.sys, "platform", "darwin"
        ), self.assertRaises(model_assets.PriorityBlocked):
            model_assets.active_priority_processes()

    def test_linux_priority_inventory_and_ancestor_walk(self) -> None:
        real_path = Path
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            proc = root / "proc"
            proc.mkdir()
            (proc / "101").mkdir()
            (proc / "101/cmdline").write_bytes(b"python\0/work/erais/train.py\0")
            (proc / "101/comm").write_bytes(b"python3\n")
            (proc / "102").mkdir()
            (proc / "not-a-pid").mkdir()

            def mapped(value: object = ".") -> Path:
                name = str(value)
                if name == "/proc":
                    return proc
                if name.startswith("/proc/"):
                    return proc / name.removeprefix("/proc/")
                return real_path(value)

            with mock.patch.object(
                model_assets, "Path", side_effect=mapped
            ), mock.patch.object(model_assets, "_ancestor_pids", return_value={999}):
                matches = model_assets.active_priority_processes()
            self.assertEqual(matches, [(101, "python /work/erais/train.py ")])

            pid = os.getpid()
            own = proc / str(pid)
            own.mkdir(exist_ok=True)
            (own / "stat").write_text(f"{pid} (python) S 50 0 0\n", encoding="utf-8")
            parent = proc / "50"
            parent.mkdir()
            (parent / "stat").write_text("malformed", encoding="utf-8")
            with mock.patch.object(model_assets, "Path", side_effect=mapped):
                ancestors = model_assets._ancestor_pids()
            self.assertIn(pid, ancestors)
            self.assertIn(50, ancestors)

    def test_linux_resource_guard_full_contract_and_mismatches(self) -> None:
        real_path = Path
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            sysfs = root / "sysfs"
            cgroup = sysfs / "user.slice" / "rkc-low-fixture.scope"
            cgroup.mkdir(parents=True)
            files = {
                "cpu.weight": "1",
                "cpu.max": "100000 100000",
                "memory.high": str(2 * 1024 * 1024 * 1024),
                "memory.max": str(2560 * 1024 * 1024),
                "memory.swap.max": str(256 * 1024 * 1024),
                "pids.max": "128",
                "io.weight": "default 1",
            }
            for name, value in files.items():
                (cgroup / name).write_text(value + "\n", encoding="ascii")
            self_cgroup = root / "self.cgroup"
            self_cgroup.write_text(
                "0::/user.slice/rkc-low-fixture.scope\n", encoding="ascii"
            )
            oom = root / "oom_score_adj"
            oom.write_text("750\n", encoding="ascii")

            def mapped(value: object = ".") -> Path:
                name = str(value)
                if name == "/proc/self/cgroup":
                    return self_cgroup
                if name == "/sys/fs/cgroup":
                    return sysfs
                if name == "/proc/self/oom_score_adj":
                    return oom
                return real_path(value)

            ionice = subprocess.CompletedProcess([], 0, stdout="idle\n", stderr="")
            with mock.patch.object(
                model_assets, "Path", side_effect=mapped
            ), mock.patch.object(
                model_assets.os, "getpriority", return_value=19
            ), mock.patch.object(
                model_assets.subprocess, "run", return_value=ionice
            ):
                model_assets.assert_resource_guard()
                (cgroup / "cpu.weight").write_text("2\n", encoding="ascii")
                with self.assertRaisesRegex(model_assets.AssetError, "CPUWeight"):
                    model_assets.assert_resource_guard()
                (cgroup / "cpu.weight").write_text("1\n", encoding="ascii")
                (cgroup / "memory.high").write_text("1\n", encoding="ascii")
                with self.assertRaisesRegex(model_assets.AssetError, "memory.high"):
                    model_assets.assert_resource_guard()
                (cgroup / "memory.high").write_text(
                    files["memory.high"] + "\n", encoding="ascii"
                )
                (cgroup / "io.weight").write_text("default 100\n", encoding="ascii")
                with self.assertRaisesRegex(model_assets.AssetError, "IOWeight"):
                    model_assets.assert_resource_guard()
            (cgroup / "io.weight").write_text("default 1\n", encoding="ascii")
            with mock.patch.object(
                model_assets, "Path", side_effect=mapped
            ), mock.patch.object(
                model_assets.os, "getpriority", return_value=0
            ), mock.patch.object(
                model_assets.subprocess, "run", return_value=ionice
            ), self.assertRaisesRegex(
                model_assets.AssetError, "nice level"
            ):
                model_assets.assert_resource_guard()
            with mock.patch.object(
                model_assets, "Path", side_effect=mapped
            ), mock.patch.object(
                model_assets.os, "getpriority", return_value=19
            ), mock.patch.object(
                model_assets.subprocess,
                "run",
                return_value=subprocess.CompletedProcess(
                    [], 1, stdout="", stderr="bad"
                ),
            ), self.assertRaisesRegex(
                model_assets.AssetError, "scheduling"
            ):
                model_assets.assert_resource_guard()

    def test_redirect_write_and_fsync_edge_failures(self) -> None:
        asset = fixture_asset(b"x")
        handler = model_assets._StrictRedirectHandler(asset)
        request = urllib.request.Request(asset.url)
        request.redirect_dict = {asset.url: 5}  # type: ignore[attr-defined]
        with self.assertRaisesRegex(model_assets.IntegrityError, "five redirects"):
            handler.redirect_request(request, None, 302, "found", {}, asset.url)
        self.assertIsNotNone(model_assets._default_opener(asset))
        with mock.patch.object(
            model_assets.os, "write", return_value=0
        ), self.assertRaisesRegex(OSError, "short write"):
            model_assets._write_all(1, b"x")
        for error_number, should_raise in ((22, False), (95, False), (5, True)):
            error = OSError(error_number, "fsync")
            with mock.patch.object(model_assets.os, "fsync", side_effect=error):
                if should_raise:
                    with self.assertRaises(OSError):
                        model_assets._fsync_directory(1)
                else:
                    model_assets._fsync_directory(1)

    def test_fetch_handles_concurrent_identical_publication(self) -> None:
        payload = b"concurrent"
        asset = fixture_asset(payload)
        real_link = os.link
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"

            def racing_link(src, dst, **kwargs):  # type: ignore[no-untyped-def]
                real_link(src, dst, **kwargs)
                raise FileExistsError(dst)

            with mock.patch.object(
                model_assets, "assert_resource_guard"
            ), mock.patch.object(model_assets.os, "link", side_effect=racing_link):
                path = model_assets.fetch_asset(
                    asset,
                    cache,
                    opener=FakeOpener(FakeResponse(payload, asset.url)),
                    priority_check=lambda: None,
                )
            self.assertEqual(path.read_bytes(), payload)

    def test_download_cleanup_refuses_replaced_temporary_inode(self) -> None:
        asset = fixture_asset(b"expected")
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"

            def replace_temporary(*_args, **_kwargs):  # type: ignore[no-untyped-def]
                path = next(cache.glob(".download-*.part"))
                path.unlink()
                path.write_bytes(b"replacement")
                raise model_assets.AssetError("interrupted")

            with mock.patch.object(
                model_assets, "_download_to_descriptor", side_effect=replace_temporary
            ), self.assertRaisesRegex(model_assets.IntegrityError, "inode changed"):
                model_assets.fetch_asset(
                    asset,
                    cache,
                    require_guard=False,
                    priority_check=lambda: None,
                )
            leftovers = list(cache.glob(".download-*.part"))
            self.assertEqual(len(leftovers), 1)
            self.assertEqual(leftovers[0].read_bytes(), b"replacement")

    def test_resource_guard_and_cache_root_fail_closed(self) -> None:
        with mock.patch.object(
            model_assets.sys, "platform", "darwin"
        ), self.assertRaisesRegex(model_assets.AssetError, "Linux"):
            model_assets.assert_resource_guard()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cache = root / "cache"
            opened, descriptor, info = model_assets._open_cache_root(cache)
            self.assertEqual(opened, cache)
            self.assertTrue(stat.S_ISDIR(info.st_mode))
            os.close(descriptor)
            os.chmod(cache, 0o777)
            with self.assertRaisesRegex(model_assets.AssetError, "writable"):
                model_assets._open_cache_root(cache)
            os.chmod(cache, 0o700)
            if hasattr(os, "symlink"):
                link = root / "link"
                link.symlink_to(cache, target_is_directory=True)
                with self.assertRaises(model_assets.AssetError):
                    model_assets._open_cache_root(link)

    def test_cached_asset_verification_reports_absence_size_and_hash(self) -> None:
        payload = b"verified"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"
            cache.mkdir(mode=0o700)
            with self.assertRaises(model_assets.MissingAsset):
                model_assets.verify_cached_asset(
                    asset, cache, priority_check=lambda: None
                )
            target = cache / asset.filename
            target.write_bytes(b"wrong-size")
            with self.assertRaisesRegex(model_assets.IntegrityError, "size mismatch"):
                model_assets.verify_cached_asset(
                    asset, cache, priority_check=lambda: None
                )
            target.write_bytes(b"tampered")
            self.assertEqual(len(b"tampered"), asset.size_bytes)
            with self.assertRaisesRegex(model_assets.IntegrityError, "SHA-256"):
                model_assets.verify_cached_asset(
                    asset, cache, priority_check=lambda: None
                )
            target.write_bytes(payload)
            self.assertEqual(
                model_assets.verify_cached_asset(
                    asset, cache, priority_check=lambda: None
                ),
                target,
            )

    def test_cached_hash_rechecks_priority_and_disk_headroom_fails_closed(self) -> None:
        payload = b"verified"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"
            cache.mkdir(mode=0o700)
            (cache / asset.filename).write_bytes(payload)
            checks = 0

            def priority() -> None:
                nonlocal checks
                checks += 1

            with mock.patch.object(model_assets, "PRIORITY_RECHECK_BYTES", 1):
                self.assertEqual(
                    model_assets.verify_cached_asset(
                        asset,
                        cache,
                        priority_check=priority,
                    ),
                    cache / asset.filename,
                )
            self.assertGreaterEqual(checks, 3)

            statvfs = mock.Mock(f_frsize=4096, f_bsize=4096, f_bavail=1)
            with mock.patch.object(
                model_assets.os, "statvfs", return_value=statvfs
            ), self.assertRaisesRegex(model_assets.AssetError, "disk headroom"):
                model_assets.assert_disk_headroom(cache, 1, "fixture")
            with self.assertRaisesRegex(model_assets.AssetError, "negative"):
                model_assets._required_free_bytes(-1)

    def test_download_rejects_http_contract_violations(self) -> None:
        payload = b"expected"
        asset = fixture_asset(payload)
        cases = (
            (FakeResponse(payload, asset.url, status=503), model_assets.AssetError),
            (
                FakeResponse(payload, asset.url, content_encoding="gzip"),
                model_assets.IntegrityError,
            ),
            (FakeResponse(payload, asset.url, content_length=None), None),
            (FakeResponse(payload[:-1], asset.url), model_assets.IntegrityError),
            (FakeResponse(payload + b"x", asset.url), model_assets.IntegrityError),
        )
        for response, error in cases:
            with self.subTest(
                status=response.status, size=len(response.stream.getvalue())
            ):
                with tempfile.TemporaryFile() as handle:
                    if error is None:
                        model_assets._download_to_descriptor(
                            asset, handle.fileno(), FakeOpener(response), lambda: None
                        )
                    else:
                        with self.assertRaises(error):
                            model_assets._download_to_descriptor(
                                asset,
                                handle.fileno(),
                                FakeOpener(response),
                                lambda: None,
                            )
        bad_length = FakeResponse(payload, asset.url)
        bad_length.headers["Content-Length"] = "NaN"
        with tempfile.TemporaryFile() as handle, self.assertRaisesRegex(
            model_assets.IntegrityError, "integer"
        ):
            model_assets._download_to_descriptor(
                asset, handle.fileno(), FakeOpener(bad_length), lambda: None
            )

        class BrokenOpener:
            def open(self, *_args, **_kwargs):  # type: ignore[no-untyped-def]
                raise urllib.error.URLError("offline")

        with tempfile.TemporaryFile() as handle, self.assertRaises(
            model_assets.AssetError
        ):
            model_assets._download_to_descriptor(
                asset, handle.fileno(), BrokenOpener(), lambda: None
            )

    def test_main_commands_and_error_statuses_are_mocked(self) -> None:
        lock = model_assets.load_lock()
        asset = lock.assets[1]
        with mock.patch.object(
            model_assets, "load_lock", return_value=lock
        ), mock.patch("sys.stdout", new=io.StringIO()) as output:
            self.assertEqual(model_assets.main(["list"]), 0)
            self.assertEqual(len(json.loads(output.getvalue())), len(lock.assets))
        with mock.patch.object(
            model_assets, "load_lock", return_value=lock
        ), mock.patch("sys.stdout", new=io.StringIO()):
            self.assertEqual(model_assets.main(["validate-lock"]), 0)
        with mock.patch.object(
            model_assets, "load_lock", return_value=lock
        ), mock.patch.object(
            model_assets, "verify_cached_asset", return_value=Path("/cache/model")
        ), mock.patch.object(
            model_assets, "assert_priority_available"
        ) as priority, mock.patch.object(
            model_assets, "assert_resource_guard"
        ) as guard, mock.patch(
            "sys.stdout", new=io.StringIO()
        ):
            self.assertEqual(
                model_assets.main(
                    ["verify", "--asset", asset.asset_id, "--cache-root", "/cache"]
                ),
                0,
            )
            priority.assert_called()
            guard.assert_called_once()
        with mock.patch.object(
            model_assets, "load_lock", return_value=lock
        ), mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(
                model_assets.main(
                    ["fetch", "--asset", asset.asset_id, "--cache-root", "/cache"]
                ),
                1,
            )
        with mock.patch.object(
            model_assets, "load_lock", return_value=lock
        ), mock.patch.object(
            model_assets, "fetch_asset", return_value=Path("/cache/model")
        ), mock.patch(
            "sys.stdout", new=io.StringIO()
        ):
            self.assertEqual(
                model_assets.main(
                    [
                        "fetch",
                        "--asset",
                        asset.asset_id,
                        "--cache-root",
                        "/cache",
                        "--accept-license",
                        asset.license_spdx,
                    ]
                ),
                0,
            )
        with mock.patch.object(
            model_assets, "load_lock", side_effect=model_assets.PriorityBlocked("busy")
        ), mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(model_assets.main(["list"]), 75)

    def test_lock_rejects_structural_and_binding_policy_drift(self) -> None:
        lock = model_assets.load_lock()
        documents: list[tuple[str, object, str]] = []

        def changed(label: str, message: str) -> dict[str, object]:
            document = deepcopy(lock.document)
            documents.append((label, document, message))
            return document

        documents.append(("root", [], "root must be an object"))
        changed("missing-key", "keys differ").pop("assets")
        changed("schema", "checked-in schema")["$schema"] = "elsewhere.json"
        changed("version", "unsupported")["schema_version"] = "2.0.0"
        changed("llama-type", "must be an object")["llama_cpp"] = []
        changed("repository", "reviewed upstream")["llama_cpp"][
            "repository"
        ] = "https://example.test/llama.cpp"
        changed("tag", "tag is invalid")["llama_cpp"]["tag"] = "release-1"
        changed("commit", "full lowercase")["llama_cpp"]["commit"] = "A" * 40
        changed("license", "upstream MIT")["llama_cpp"]["license_spdx"] = "Apache-2.0"
        changed("asset-count", "between three and 32")["assets"] = []
        duplicate = changed("duplicate-assets", "duplicate asset identifiers")
        duplicate["assets"].append(deepcopy(duplicate["assets"][0]))
        wrong_tag = changed("source-tag", "pinned release tag")
        wrong_tag["assets"][0]["filename"] = "llama-source.tar.gz"
        changed("default-id", "valid asset identifier")["default_generation_model"] = 17
        wrong_kind = changed("default-kind", "qualified, default-eligible")
        wrong_kind["assets"][1]["status"] = "qualified"
        wrong_kind["assets"][1]["default_eligible"] = True
        wrong_kind["default_embedding_model"] = wrong_kind["assets"][1]["id"]

        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "models.lock.json"
            for label, document, message in documents:
                with self.subTest(label=label):
                    path.write_text(json.dumps(document), encoding="utf-8")
                    with self.assertRaisesRegex(model_assets.LockError, message):
                        model_assets.load_lock(path)

    def test_cmake_rejects_nested_collection_policy_drift(self) -> None:
        base = deepcopy(model_assets.load_lock().document["llama_cpp"])
        cases: list[tuple[str, object, str]] = []

        def changed(label: str, message: str) -> dict[str, object]:
            llama = deepcopy(base)
            cases.append((label, llama, message))
            return llama

        changed("cmake-type", "must be an object")["cmake"] = None
        changed("cmake-keys", "keys differ")["cmake"]["unexpected"] = []
        changed("targets-type", "one to eight targets")["cmake"]["targets"] = None
        duplicate_targets = changed("targets-duplicate", "contains duplicates")
        duplicate_targets["cmake"]["targets"].append("llama-cli")
        changed("common-empty", "one to 64 options")["cmake"]["common_options"] = []
        duplicate_options = changed("common-duplicate", "duplicate options")
        option = duplicate_options["cmake"]["common_options"][0]
        duplicate_options["cmake"]["common_options"] = [option, option]
        changed("profiles-type", "must be an object")["cmake"]["profiles"] = []
        changed("profiles-keys", "keys differ")["cmake"]["profiles"]["gpu"] = []
        changed("portable-empty", "one to 64 options")["cmake"]["profiles"][
            "portable"
        ] = []
        changed("native-invalid", "invalid CMake")["cmake"]["profiles"]["native"] = [
            "-Dunsafe value=true"
        ]

        for label, llama, message in cases:
            with self.subTest(label=label), self.assertRaisesRegex(
                model_assets.LockError, message
            ):
                model_assets._validate_cmake(llama)

    def test_asset_parser_rejects_nested_metadata_policy_edges(self) -> None:
        lock = model_assets.load_lock()
        generation = deepcopy(lock.document["assets"][1])
        cases: list[tuple[str, dict[str, object], str]] = []

        def changed(label: str, message: str) -> dict[str, object]:
            asset = deepcopy(generation)
            cases.append((label, asset, message))
            return asset

        changed("missing-key", "keys differ").pop("repository")
        changed("revision-binding", "embed the immutable revision")[
            "url"
        ] = "https://huggingface.co/example/model.gguf"
        changed("hosts-type", "between one and eight host rules")[
            "allowed_hosts"
        ] = "huggingface.co"
        changed("host-entry-type", "must be a string")["allowed_hosts"] = [17]
        changed("quantization", "quantization is invalid")["quantization"] = "bad value"
        changed("context-bool", "must be an integer")["native_context_tokens"] = True
        changed("context-bound", "supported bound")["native_context_tokens"] = (
            1024 * 1024 + 1
        )
        changed("qualification-absolute", "escapes")[
            "qualification_spec"
        ] = "/models/qualification/model.json"
        changed("qualification-directory", "escapes")[
            "qualification_spec"
        ] = "docs/qualification/model.json"
        changed("qualification-suffix", "escapes")[
            "qualification_spec"
        ] = "models/qualification/model.yaml"
        changed("extraction-root", "portable directory")["extraction_root"] = "../src"
        changed("default-type", "must be a boolean")["default_eligible"] = 1
        changed("repository-ip", "DNS hostname")[
            "repository"
        ] = "https://127.0.0.1/model"
        changed("license-url", "without credentials")[
            "license_url"
        ] = "https://user@example.test/LICENSE"

        for label, asset, message in cases:
            with self.subTest(label=label), self.assertRaisesRegex(
                model_assets.LockError, message
            ):
                model_assets._parse_asset(asset, 1)

        source = deepcopy(lock.document["assets"][0])
        for label, key, value in (
            ("context", "native_context_tokens", 1),
            ("qualification", "qualification_spec", "models/qualification/source.json"),
        ):
            with self.subTest(source_metadata=label):
                changed_source = deepcopy(source)
                changed_source[key] = value
                with self.assertRaisesRegex(
                    model_assets.LockError, "model-only metadata"
                ):
                    model_assets._parse_asset(changed_source, 0)

    def test_resource_guard_rejects_missing_and_incomplete_contracts(self) -> None:
        with mock.patch.object(
            model_assets.Path, "read_text", return_value="1:name:/legacy\n"
        ), self.assertRaisesRegex(model_assets.AssetError, "no unified cgroup"):
            model_assets.assert_resource_guard()
        with mock.patch.object(
            model_assets.Path,
            "read_text",
            return_value="0::/user.slice/not-rkc.scope\n",
        ), self.assertRaisesRegex(model_assets.AssetError, "outside an RKC"):
            model_assets.assert_resource_guard()

        with tempfile.TemporaryDirectory() as temporary:
            missing = Path(temporary)
            with self.assertRaisesRegex(model_assets.AssetError, "is unavailable"):
                model_assets._read_cgroup_value(missing, "cpu.weight")

            root = Path(temporary)
            sysfs = root / "sysfs"
            cgroup = sysfs / "rkc-low-fixture.service"
            cgroup.mkdir(parents=True)
            values = {
                "cpu.weight": "1",
                "cpu.max": "max 100000",
                "memory.high": str(2 * 1024 * 1024 * 1024),
                "memory.max": str(2560 * 1024 * 1024),
                "memory.swap.max": str(256 * 1024 * 1024),
                "pids.max": "128",
            }
            for name, value in values.items():
                (cgroup / name).write_text(value + "\n", encoding="ascii")
            self_cgroup = root / "self.cgroup"
            self_cgroup.write_text("0::/rkc-low-fixture.service\n", encoding="ascii")
            oom = root / "oom_score_adj"
            oom.write_text("750\n", encoding="ascii")
            real_path = Path

            def mapped(value: object = ".") -> Path:
                name = str(value)
                if name == "/proc/self/cgroup":
                    return self_cgroup
                if name == "/sys/fs/cgroup":
                    return sysfs
                if name == "/proc/self/oom_score_adj":
                    return oom
                return real_path(value)

            ionice = subprocess.CompletedProcess([], 0, stdout="idle\n", stderr="")
            with mock.patch.object(
                model_assets, "Path", side_effect=mapped
            ), mock.patch.object(
                model_assets.os, "getpriority", return_value=19
            ), mock.patch.object(
                model_assets.subprocess, "run", return_value=ionice
            ), self.assertRaisesRegex(
                model_assets.AssetError, "exceeds one CPU"
            ):
                model_assets.assert_resource_guard()
            (cgroup / "cpu.max").write_text("100000 100000\n", encoding="ascii")
            oom.write_text("0\n", encoding="ascii")
            with mock.patch.object(
                model_assets, "Path", side_effect=mapped
            ), mock.patch.object(
                model_assets.os, "getpriority", return_value=19
            ), mock.patch.object(
                model_assets.subprocess, "run", return_value=ionice
            ), self.assertRaisesRegex(
                model_assets.AssetError, "OOM score"
            ):
                model_assets.assert_resource_guard()
            oom.write_text("750\n", encoding="ascii")
            with mock.patch.object(
                model_assets, "Path", side_effect=mapped
            ), mock.patch.object(
                model_assets.os, "getpriority", return_value=19
            ), mock.patch.object(
                model_assets.subprocess, "run", side_effect=FileNotFoundError("ionice")
            ), self.assertRaisesRegex(
                model_assets.AssetError, "cannot inspect"
            ):
                model_assets.assert_resource_guard()

    def test_cache_and_verification_identity_failures_are_closed(self) -> None:
        payload = b"verified"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(model_assets.AssetError, "does not exist"):
                model_assets._assert_no_symlink_components(root / "missing" / "child")
            component = root / "component"
            component.write_bytes(b"not a directory")
            with self.assertRaisesRegex(
                model_assets.AssetError, "not a real directory"
            ):
                model_assets._assert_no_symlink_components(component / "child")
            with self.assertRaisesRegex(model_assets.AssetError, "real directory"):
                model_assets._open_cache_root(component)

            cache = root / "cache"
            cache.mkdir(mode=0o700)
            if hasattr(os, "getuid"):
                with mock.patch.object(
                    model_assets.os, "getuid", return_value=os.getuid() + 1
                ), self.assertRaisesRegex(model_assets.AssetError, "not owned"):
                    model_assets._open_cache_root(cache)
            with mock.patch.object(
                model_assets.os, "open", side_effect=OSError("denied")
            ), self.assertRaisesRegex(model_assets.AssetError, "open cache root"):
                model_assets._open_cache_root(cache)
            with mock.patch.object(
                model_assets, "_same_inode", return_value=False
            ), self.assertRaisesRegex(
                model_assets.AssetError, "changed while it was opened"
            ):
                model_assets._open_cache_root(cache)

            target = cache / asset.filename
            target.write_bytes(payload)
            with mock.patch.object(
                model_assets,
                "_same_inode",
                side_effect=[True, True, False],
            ), self.assertRaisesRegex(model_assets.IntegrityError, "pathname changed"):
                model_assets.verify_cached_asset(
                    asset, cache, priority_check=lambda: None
                )
            with mock.patch.object(
                model_assets,
                "_same_inode",
                side_effect=[True, True, True, False],
            ), self.assertRaisesRegex(
                model_assets.IntegrityError, "root pathname changed"
            ):
                model_assets.verify_cached_asset(
                    asset, cache, priority_check=lambda: None
                )

            descriptor = os.open(cache, os.O_RDONLY)
            try:
                with self.assertRaisesRegex(
                    model_assets.IntegrityError, "not a regular file"
                ):
                    model_assets._verify_open_asset(
                        descriptor, asset, priority_check=lambda: None
                    )
                enough = mock.Mock(f_frsize=4096, f_bsize=4096, f_bavail=2**30)
                with mock.patch.object(
                    model_assets.os, "fstatvfs", return_value=enough
                ):
                    model_assets.assert_disk_headroom(descriptor, 1, "fixture")
                with mock.patch.object(
                    model_assets.os, "fstatvfs", side_effect=OSError("unavailable")
                ), self.assertRaisesRegex(model_assets.AssetError, "cannot inspect"):
                    model_assets.assert_disk_headroom(descriptor, 1, "fixture")
            finally:
                os.close(descriptor)

    def test_exact_temporary_cleanup_and_publication_fail_closed(self) -> None:
        payload = b"expected"
        asset = fixture_asset(payload)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root_fd = os.open(root, os.O_RDONLY)
            try:
                temporary_name = "temporary.part"
                temporary_path = root / temporary_name
                temporary_path.write_bytes(payload)
                expected = temporary_path.stat()
                model_assets._unlink_exact_temporary(root_fd, temporary_name, expected)
                self.assertFalse(temporary_path.exists())
                model_assets._unlink_exact_temporary(root_fd, temporary_name, expected)

                changed_name = "changed.part"
                changed_path = root / changed_name
                changed_path.write_bytes(payload)
                changed_expected = changed_path.stat()
                changed_path.unlink()
                changed_path.mkdir()
                with self.assertRaisesRegex(
                    model_assets.IntegrityError, "inode changed"
                ):
                    model_assets._unlink_exact_temporary(
                        root_fd, changed_name, changed_expected
                    )
            finally:
                os.close(root_fd)

        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"
            with mock.patch.object(
                model_assets.os, "link", side_effect=OSError("publication failed")
            ), self.assertRaisesRegex(OSError, "publication failed"):
                model_assets.fetch_asset(
                    asset,
                    cache,
                    require_guard=False,
                    opener=FakeOpener(FakeResponse(payload, asset.url)),
                    priority_check=lambda: None,
                )
            self.assertEqual(list(cache.glob(".download-*.part")), [])
            self.assertFalse((cache / asset.filename).exists())

        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary) / "cache"

            def corrupting_race(_source, destination, **_kwargs):  # type: ignore[no-untyped-def]
                (cache / destination).write_bytes(b"tampered")
                raise FileExistsError(destination)

            with mock.patch.object(
                model_assets.os, "link", side_effect=corrupting_race
            ), self.assertRaisesRegex(model_assets.IntegrityError, "SHA-256"):
                model_assets.fetch_asset(
                    asset,
                    cache,
                    require_guard=False,
                    opener=FakeOpener(FakeResponse(payload, asset.url)),
                    priority_check=lambda: None,
                )
            self.assertEqual(list(cache.glob(".download-*.part")), [])
            self.assertEqual((cache / asset.filename).read_bytes(), b"tampered")


if __name__ == "__main__":
    unittest.main()
