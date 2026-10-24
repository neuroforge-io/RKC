#!/usr/bin/env python3
# Copyright 2026 NeuroForgeIO. Licensed under the Apache License, Version 2.0.
"""Bounded fixtures for profile safety and parity; no host scans or services."""
from __future__ import annotations

import argparse
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest import mock
from typing import Callable

if sys.platform != "linux":
    raise unittest.SkipTest("bounded resource profiling requires Linux cgroup v2")

NAME = "rkc_bounded_profile"
SPEC = importlib.util.spec_from_file_location(
    NAME, Path(__file__).with_name("profile-resource-workload.py")
)
assert SPEC and SPEC.loader
PROFILE = importlib.util.module_from_spec(SPEC)
sys.modules[NAME] = PROFILE
SPEC.loader.exec_module(PROFILE)


def psi(some: float = 0, full: float = 0) -> bytes:
    """Produce complete kernel-shaped pressure records."""
    return (f"some avg10={some} avg60=0 avg300=0 total=10\n"
            f"full avg10={full} avg60=0 avg300=0 total=2\n").encode()


class ProfileWorkloadTests(unittest.TestCase):
    """Prove bounded refusal and matched-run behavior without real workloads."""

    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-profile-test-")
        self.root = Path(self.temporary.name)
        self.source = self.root / "input"
        self.source.mkdir()
        (self.source / "notes.txt").write_bytes(b"Orchid source evidence\n")
        self.binary = self.root / "rkc"
        self.binary.write_bytes(b"inert binary fixture")
        self.lock = self.root / "execution.lock"
        self.lock.write_bytes(b"existing lock contents")
        self.output = self.root / "receipt.json"

    def tearDown(self) -> None:
        for current, _, _ in os.walk(self.root):
            Path(current).chmod(0o700)
        self.temporary.cleanup()

    def test_profiles_are_coherent_and_allow_future_desktop_sizes(self) -> None:
        self.assertEqual(PROFILE.Profile.parse("A:512:640:256").go, 256)
        self.assertEqual(PROFILE.Profile.parse("B:7168:8192:6144").maximum, 8192)
        for value in ("A:512:640:700", "A:640:512:256", "A:32:64:32",
                      "A:16384:16385:64", "A:512:640:bad", "../A:512:640:256"):
            with self.subTest(value=value), self.assertRaises(
                argparse.ArgumentTypeError
            ):
                PROFILE.Profile.parse(value)

    def test_profile_environment_keeps_controls_without_ambient_credentials(self) -> None:
        source = {"HOME": "/private/home", "PATH": "/usr/bin:/bin",
                  "XDG_RUNTIME_DIR": "/private/runtime", "GOMAXPROCS": "1",
                  "GOMEMLIMIT": "256MiB", "RKC_RESOURCE_PROFILE": "default",
                  "RKC_CPU_QUOTA_PERCENT": "25", "RKC_MEMORY_MAX_MIB": "640"}
        unwanted = ("OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY",
                    "GH_TOKEN", "GITHUB_TOKEN", "RKC_ARBITRARY_SECRET",
                    "PYTHONPATH", "PYTHONSTARTUP", "COVERAGE_PROCESS_START")
        for name in unwanted:
            source[name] = "synthetic ambient fixture value"
        clean = PROFILE.profile_environment(source)
        self.assertEqual(clean["GOMEMLIMIT"], "256MiB")
        self.assertEqual(clean["RKC_MEMORY_MAX_MIB"], "640")
        self.assertEqual(clean["XDG_RUNTIME_DIR"], "/private/runtime")
        self.assertTrue(all(name not in clean for name in unwanted))

    def test_coverage_counters_reject_bool_nonfinite_and_negative_values(self) -> None:
        valid = {"inventory_accounting_ratio": 1,
                 "high_confidence_secret_findings": 0, "artifacts_inventoried": 1,
                 "nodes_total": 3, "symbols_total": 0}
        PROFILE.validate_profile_coverage(valid)
        PROFILE.validate_profile_coverage({**valid, "inventory_accounting_ratio": 1.0})
        invalid = [("inventory_accounting_ratio", value)
                   for value in (True, False, "1", float("nan"), float("inf"), .5)]
        invalid += [(name, value)
                    for name in ("high_confidence_secret_findings",
                                 "artifacts_inventoried", "nodes_total", "symbols_total")
                    for value in (False, True, -1, 1.0)]
        invalid.append(("high_confidence_secret_findings", 1))
        for name, value in invalid:
            with self.subTest(name=name, value=value), self.assertRaises(
                PROFILE.ProfileError
            ):
                PROFILE.validate_profile_coverage({**valid, name: value})

    def test_existing_lock_is_exclusive_and_never_modified(self) -> None:
        self.lock.chmod(0o664)
        with PROFILE.execution_lock(self.lock):
            with self.assertRaises(PROFILE.ProfileError):
                with PROFILE.execution_lock(self.lock):
                    self.fail("busy lock acquired")
        self.assertEqual(self.lock.read_bytes(), b"existing lock contents")
        self.assertEqual(self.lock.stat().st_mode & 0o777, 0o664)
        with self.assertRaises(FileNotFoundError):
            with PROFILE.execution_lock(self.root / "absent"):
                self.fail("absent lock created")

    def test_owned_leaf_can_be_opened_through_search_only_ancestors(self) -> None:
        ancestor = self.root / "search-only"
        leaf = ancestor / "private"
        leaf.mkdir(mode=0o700, parents=True)
        lock = leaf / "existing.lock"
        lock.write_bytes(b"caller-owned coordination inode")
        original = lock.stat()
        ancestor.chmod(0o111)
        try:
            with PROFILE.execution_lock(lock) as descriptor:
                opened = os.fstat(descriptor)
                self.assertEqual((opened.st_dev, opened.st_ino),
                                 (original.st_dev, original.st_ino))
            self.assertEqual(lock.read_bytes(), b"caller-owned coordination inode")
            self.assertEqual(PROFILE.fixture_roster(leaf)[0]["path"], "existing.lock")
        finally:
            ancestor.chmod(0o700)

    def test_lock_replacement_symlinks_and_unowned_leaves_are_refused(self) -> None:
        linked = self.root / "linked.lock"
        linked.symlink_to(self.lock)
        with self.assertRaises(OSError):
            with PROFILE.execution_lock(linked):
                self.fail("symlink lock acquired")
        with mock.patch.object(PROFILE.os, "getuid", return_value=os.getuid() + 1):
            with self.assertRaises(PROFILE.ProfileError):
                with PROFILE.execution_lock(self.lock):
                    self.fail("foreign lock acquired")
        with PROFILE.execution_lock(self.lock) as descriptor:
            self.lock.rename(self.root / "previous.lock")
            self.lock.write_bytes(b"replacement")
            with self.assertRaises(PROFILE.ProfileError):
                PROFILE.prove_lock(self.lock, descriptor)

    def test_world_writable_and_special_locks_are_refused(self) -> None:
        self.lock.chmod(0o666)
        with self.assertRaises(PROFILE.ProfileError):
            with PROFILE.execution_lock(self.lock):
                self.fail("world-writable lock acquired")
        fifo = self.root / "fifo.lock"
        os.mkfifo(fifo)
        with self.assertRaises(PROFILE.ProfileError):
            with PROFILE.execution_lock(fifo):
                self.fail("special lock acquired")

    def test_watchdog_lease_outlives_wrapper_and_harness_descriptors(self) -> None:
        wrapper = self.root / "fake-wrapper.py"
        ready, release, done = (
            self.root / name for name in ("ready", "release", "done")
        )
        wrapper.write_text(
            "import os,sys,time\nfrom pathlib import Path\n"
            "if os.fork(): sys.exit(0)\n"
            "os.setsid()\nnull=os.open(os.devnull,os.O_RDWR)\n"
            "for fd in (0,1,2): os.dup2(null,fd)\n"
            "os.close(null)\nos.fstat(int(sys.argv[4]))\n"
            "ready,release,done=map(Path,sys.argv[1:4])\nready.touch()\n"
            "deadline=time.monotonic()+4\n"
            "while not release.exists() and time.monotonic()<deadline:\n"
            " time.sleep(.01)\n"
            "done.touch()\n"
        )
        try:
            with PROFILE.execution_lock(self.lock) as descriptor:
                with PROFILE.inherited_lease(descriptor) as lease:
                    self.assertGreaterEqual(lease, 10)
                    result = PROFILE.run_bounded([
                        sys.executable, "-I", "-S", str(wrapper), str(ready),
                        str(release), str(done), str(lease),
                    ], 2, dict(os.environ), lease_fd=lease)
                    self.assertEqual(result["exit_code"], 0)
                    deadline = time.monotonic() + 2
                    while not ready.exists() and time.monotonic() < deadline:
                        time.sleep(.01)
                    self.assertTrue(ready.exists())
            with self.assertRaises(PROFILE.ProfileError):
                with PROFILE.execution_lock(self.lock):
                    self.fail("detached cleanup lost its lock lease")
            release.touch()
            deadline = time.monotonic() + 2
            while time.monotonic() < deadline:
                try:
                    with PROFILE.execution_lock(self.lock):
                        self.assertTrue(done.exists())
                        break
                except PROFILE.ProfileError:
                    time.sleep(.01)
            else:
                self.fail("cleanup did not release its inherited lease")
        finally:
            release.touch()

    def test_owner_birth_pin_rejects_death_reuse_and_lock_replacement(self) -> None:
        info = self.lock.stat()
        job = {"owner": 1234, "owner_birth": 42, "lock_fd": 10,
               "lock_path": str(self.lock), "lock_identity": [info.st_dev, info.st_ino]}
        actual_stat = os.stat

        def owner_stat(path, *arguments, **keywords):
            if path == "/proc/1234/fd/10":
                return info
            return actual_stat(path, *arguments, **keywords)

        with mock.patch.object(PROFILE.os, "stat", side_effect=owner_stat):
            with mock.patch.object(PROFILE, "birth_ticks", return_value=42):
                PROFILE.prove_owner(job)
            with mock.patch.object(PROFILE, "birth_ticks", return_value=43):
                with self.assertRaisesRegex(PROFILE.ProfileError, "birth"):
                    PROFILE.prove_owner(job)
            with mock.patch.object(
                PROFILE, "birth_ticks", side_effect=FileNotFoundError
            ):
                with self.assertRaisesRegex(PROFILE.ProfileError, "unavailable"):
                    PROFILE.prove_owner(job)
            self.lock.rename(self.root / "previous.lock")
            self.lock.write_bytes(b"new coordination inode")
            with mock.patch.object(PROFILE, "birth_ticks", return_value=42):
                with self.assertRaisesRegex(PROFILE.ProfileError, "inode"):
                    PROFILE.prove_owner(job)

    def test_birth_parser_handles_comm_delimiters_and_dead_owners(self) -> None:
        tail = b"R " + b"0 " * 18 + b"42"
        with mock.patch.object(
            PROFILE, "read_small", return_value=b"1 (odd ) name) " + tail
        ):
            self.assertEqual(PROFILE.birth_ticks(1), 42)
        for data in (b"1 no comm", b"1 (short) R 0",
                     b"1 (dead) " + tail.replace(b"R", b"Z")):
            with mock.patch.object(PROFILE, "read_small", return_value=data), \
                    self.assertRaises(PROFILE.ProfileError):
                PROFILE.birth_ticks(1)

    def test_frozen_input_is_byte_exact_and_independent(self) -> None:
        frozen = self.root / "frozen"
        frozen.mkdir()
        initial = PROFILE.fixture_roster(self.source, frozen)
        self.assertEqual(initial, PROFILE.fixture_roster(frozen))
        (self.source / "notes.txt").write_bytes(b"changed original\n")
        self.assertEqual(initial, PROFILE.fixture_roster(frozen))
        self.assertNotEqual(initial, PROFILE.fixture_roster(self.source))
        self.assertEqual((frozen / "notes.txt").stat().st_mode & 0o777, 0o400)

    def test_input_symlinks_bytes_and_path_limits_fail_before_scanning(self) -> None:
        linked = self.source / "linked.txt"
        linked.symlink_to(self.binary)
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.fixture_roster(self.source)
        linked.unlink()
        (self.source / "large.txt").write_bytes(b"x" * (PROFILE.MAX_INPUT + 1))
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.fixture_roster(self.source)
        (self.source / "large.txt").unlink()
        for index in range(PROFILE.MAX_PATHS):
            (self.source / f"empty{index}").touch()
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.fixture_roster(self.source)

    def test_empty_nested_and_replaced_source_paths(self) -> None:
        empty = self.root / "empty"
        empty.mkdir()
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.fixture_roster(empty)
        nested = self.source / "nested"
        nested.mkdir()
        (nested / "records.ndjson").write_bytes(b'{"topic":"Orchid"}\n')
        self.assertEqual(len(PROFILE.fixture_roster(self.source)), 2)
        actual_open = os.open
        replaced = False

        def replacing_open(path, flags, *arguments, **keywords):
            nonlocal replaced
            if path == "notes.txt" and not replaced:
                replaced = True
                (self.source / "notes.txt").rename(self.source / "previous")
                (self.source / "notes.txt").write_bytes(b"replacement\n")
            return actual_open(path, flags, *arguments, **keywords)

        with mock.patch.object(PROFILE.os, "open", side_effect=replacing_open):
            with self.assertRaisesRegex(PROFILE.ProfileError, "changed"):
                PROFILE.fixture_roster(self.source)

    def test_digest_bounds_and_read_size_limit(self) -> None:
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.digest_file(self.binary, 2)
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.digest_file(self.source, 100)
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.read_small(self.binary, 2)
        self.assertEqual(PROFILE.read_small(self.binary), self.binary.read_bytes())

    def test_pressure_parser_rejects_unknown_or_nonfinite_observations(self) -> None:
        self.assertEqual(PROFILE.parse_psi(psi())["some"]["total"], 10)
        for data in (b"", psi(float("nan")), psi(101), psi() + psi(),
                     b"some avg10=0 avg60=0 avg300=0 total=0\n",
                     psi().replace(b"avg60=0", b"avg10=0")):
            with self.subTest(data=data), self.assertRaises(
                (ValueError, PROFILE.ProfileError)
            ):
                PROFILE.parse_psi(data)

    def test_current_cgroup_and_counter_parsers_are_fail_closed(self) -> None:
        for record in (b"0::/\n", b"0::/user.slice/rkc-low-fixture.scope\n"):
            with mock.patch.object(PROFILE, "read_small", return_value=record):
                self.assertTrue(
                    str(PROFILE.current_cgroup()).startswith("/sys/fs/cgroup")
                )
        for record in (b"", b"0::relative\n", b"0::/../escape\n", b"0::/\n0::/other\n"):
            with mock.patch.object(PROFILE, "read_small", return_value=record), \
                    self.assertRaises(PROFILE.ProfileError):
                PROFILE.current_cgroup()
        for data in (b"", b"high -1\n", b"high 1\nhigh 2\n", b"high 1 extra\n"):
            with self.assertRaises(PROFILE.ProfileError):
                PROFILE.counters(data)

    def test_observer_requires_real_cpu_and_memory_denominators(self) -> None:
        group = self.root / "kernel"
        group.mkdir()
        files = {
            "cpu.stat": (b"usage_usec 12\nnr_periods 1\n"
                         b"nr_throttled 0\nthrottled_usec 0\n"),
            "memory.events": b"high 0\nmax 0\noom 0\noom_kill 0\n",
            "memory.current": b"128\n", "memory.peak": b"256\n",
            "memory.pressure": psi(), "cpu.pressure": psi(),
        }
        for name, data in files.items():
            (group / name).write_bytes(data)
        self.assertEqual(PROFILE.observe(group)["memory_peak"], 256)
        for name in ("cpu.stat", "memory.events"):
            (group / name).write_bytes(b"unrelated 0\n")
            with self.assertRaises(PROFILE.ProfileError):
                PROFILE.observe(group)
            (group / name).write_bytes(files[name])

    def admission_reader(self, global_full: float = 0, local_full: float = 0,
                         available: int = 22000) -> Callable[..., bytes]:
        """Supply only private fixtures to the kernel-observation boundary."""
        def read(path: Path, maximum: int = PROFILE.MAX_CONTROL) -> bytes:
            if path == Path("/proc/meminfo"):
                return (f"MemTotal: 32000000 kB\nMemAvailable: {available * 1024} kB\n"
                        "HugePages_Total: 0\n").encode()
            if path == Path("/proc/pressure/memory"):
                return psi(full=global_full)
            if path.name == "memory.pressure":
                return psi(full=local_full)
            if path == Path("/proc/pressure/cpu"):
                return psi()
            raise AssertionError("unexpected host observation")
        return read

    def test_global_and_cgroup_pressure_defeat_apparently_free_ram(self) -> None:
        for global_full, local_full, available in ((30, 0, 22000), (0, 2, 22000),
                                                    (0, 0, 4200)):
            reader = self.admission_reader(global_full, local_full, available)
            with mock.patch.object(PROFILE, "read_small", side_effect=reader), \
                    self.assertRaises(PROFILE.ProfileError):
                PROFILE.admission(Path("/fixture"), 4096, 640, 10, 1)
        with mock.patch.object(
            PROFILE, "read_small", side_effect=self.admission_reader()
        ):
            observed = PROFILE.admission(Path("/fixture"), 4096, 640, 10, 1)
        self.assertEqual(observed["available_bytes"], 22000 * PROFILE.MIB)

    def test_exact_selected_cpu_and_memory_controls_are_required(self) -> None:
        profile = PROFILE.Profile("A", 512, 640, 256)
        controls = {"cpu.max": b"25000 100000", "cpu.weight": b"1",
                    "memory.high": str(512 * PROFILE.MIB).encode(),
                    "memory.max": str(640 * PROFILE.MIB).encode(),
                    "memory.swap.max": b"0"}
        with mock.patch.dict(os.environ, {"GOMEMLIMIT": "256MiB", "GOMAXPROCS": "1"}):
            with mock.patch.object(
                PROFILE, "read_small", side_effect=lambda path: controls[path.name]
            ):
                PROFILE.prove_controls(Path("/fixture"), profile, 25)
                for value in (b"100000 100000", b"max 100000", b"25000 0"):
                    controls["cpu.max"] = value
                    with self.assertRaises(PROFILE.ProfileError):
                        PROFILE.prove_controls(Path("/fixture"), profile, 25)
                controls["cpu.max"] = b"25000 100000"
                controls["memory.swap.max"] = b"1"
                with self.assertRaises(PROFILE.ProfileError):
                    PROFILE.prove_controls(Path("/fixture"), profile, 25)
                controls["memory.swap.max"] = b"0"
                with mock.patch.dict(os.environ, {"GOMEMLIMIT": "384MiB"}):
                    with self.assertRaises(PROFILE.ProfileError):
                        PROFILE.prove_controls(Path("/fixture"), profile, 25)

    def test_scan_arguments_cannot_call_models_workers_or_git_helpers(self) -> None:
        job = {"binary": "/private/rkc", "source": "/private/input",
               "output": "/private/atlas", "runs": "/private/runs"}
        command = PROFILE.scan_arguments(job)
        for flag in ("--no-python", "--no-git-metadata", "--no-cache", "--json"):
            self.assertIn(flag, command)
        self.assertEqual(command[command.index("--stage-workers") + 1], "1")
        self.assertEqual(command[command.index("--stage-memory-mib") + 1], "512")
        self.assertEqual(command[-1], job["source"])
        self.assertNotIn("--no-plugins", command)

    def test_collector_retains_native_counters_hashes_and_bounded_throughput(
        self, negative_psi: bool = False,
    ) -> None:
        output, result = self.root / "atlas", self.root / "collected.json"
        lock_info = self.lock.stat()
        job = {"owner": 1, "lock_fd": 4, "owner_birth": 42,
               "lock_identity": [lock_info.st_dev, lock_info.st_ino],
               "lock_path": str(self.lock),
               "profile": {"name": "A", "high": 512, "maximum": 640, "go": 256},
               "cpu": 25, "reserve": 4096, "some": 10, "full": 1, "seconds": 45,
               "source": str(self.source), "output": str(output),
               "binary": str(self.binary), "runs": str(self.root / "runs"),
               "receipt": str(result), "input": PROFILE.fixture_roster(self.source)}
        job_path = self.root / "job.json"
        PROFILE.atomic_receipt(job_path, job)
        actual_stat = os.stat

        def observe_stat(path, *arguments, **keywords):
            if path == "/proc/1/fd/4":
                return lock_info
            return actual_stat(path, *arguments, **keywords)

        def fake_scan(arguments: list[str], seconds: float, environment: dict,
                      monitor=None) -> dict:
            monitor()
            output.mkdir()
            (output / "search").mkdir()
            for name in ("bundle.json", "search/index.json"):
                (output / name).write_bytes(b"{}\n")
            (output / "coverage.json").write_text(json.dumps({
                "inventory_accounting_ratio": 1, "high_confidence_secret_findings": 0,
                "deterministic_output_digest": "a" * 64,
                "artifacts_inventoried": 1, "nodes_total": 3, "symbols_total": 1,
            }))
            return {"exit_code": 0, "wall_seconds": 0.5}

        def observation(usage: int, high: int) -> dict:
            observed = {"cpu": {"usage_usec": usage}, "events": {"high": high},
                        "memory_peak": 12345,
                        "memory_psi": PROFILE.parse_psi(psi()),
                        "cpu_psi": PROFILE.parse_psi(psi())}
            if negative_psi and usage == 250:
                observed["memory_psi"]["full"]["total"] = 1
            return observed

        with mock.patch.object(PROFILE.os, "stat", side_effect=observe_stat), \
                mock.patch.object(PROFILE, "birth_ticks", return_value=42), \
                mock.patch.object(PROFILE, "current_cgroup", return_value=self.root), \
                mock.patch.object(PROFILE, "prove_controls", return_value={}), \
                mock.patch.object(PROFILE, "admission", return_value={}), \
                mock.patch.object(PROFILE, "observe", side_effect=[
                    observation(100, 0), observation(250, 2),
                ]), mock.patch.object(PROFILE, "run_bounded", side_effect=fake_scan), \
                mock.patch.object(PROFILE.resource, "getrusage",
                                  return_value=SimpleNamespace(ru_maxrss=1234)):
            status = PROFILE.collect(job_path)
        collected = json.loads(result.read_bytes())
        if negative_psi:
            self.assertEqual(status, 1)
            self.assertEqual(collected["status"], "failed")
            self.assertIn("PSI counters decreased", collected["reason"])
            return
        self.assertEqual(status, 0)
        self.assertEqual(collected["cpu_delta"], {"usage_usec": 150})
        self.assertEqual(collected["memory_events_delta"], {"high": 2})
        self.assertEqual(collected["maximum_child_rss_kib"], 1234)
        self.assertEqual(collected["input_bytes_per_second"],
                         len((self.source / "notes.txt").read_bytes()) / 0.5)
        self.assertEqual(len(collected["canonical_hashes"]), 3)

    def test_collector_rejects_regressing_psi_instead_of_qualifying_the_run(
        self,
    ) -> None:
        self.test_collector_retains_native_counters_hashes_and_bounded_throughput(
            negative_psi=True,
        )

    def test_termination_reaps_only_owned_process_group(self) -> None:
        process = mock.Mock(pid=12345)
        process.poll.side_effect = [None, None]
        process.wait.side_effect = [subprocess.TimeoutExpired("fixture", 2), 0]
        with mock.patch.object(PROFILE.os, "killpg") as kill:
            PROFILE.stop_process(process)
        self.assertEqual(kill.call_args_list, [
            mock.call(12345, signal.SIGTERM), mock.call(12345, signal.SIGKILL),
        ])

    def test_output_capture_hashes_values_without_retaining_them(self) -> None:
        result = PROFILE.run_bounded(
            [sys.executable, "-I", "-S", "-c", "print('private fixture marker')"],
            2, dict(os.environ),
        )
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["output"]["stdout"]["bytes"], 23)
        self.assertNotIn("private fixture marker", json.dumps(result))

    def test_deadline_terminates_an_inert_sleeping_child(self) -> None:
        with self.assertRaisesRegex(PROFILE.ProfileError, "deadline"):
            PROFILE.run_bounded(
                [sys.executable, "-I", "-S", "-c", "import time; time.sleep(10)"],
                0.05, dict(os.environ),
            )

    def test_output_overflow_and_pressure_refusal_stop_owned_children(self) -> None:
        with self.assertRaisesRegex(PROFILE.ProfileError, "capture bound"):
            PROFILE.run_bounded([
                sys.executable, "-I", "-S", "-c",
                "import sys;sys.stdout.write('x' * (2 * 1024 * 1024 + 1))",
            ], 2, dict(os.environ))
        with self.assertRaisesRegex(PROFILE.ProfileError, "fixture pressure"):
            PROFILE.run_bounded([
                sys.executable, "-I", "-S", "-c", "import time;time.sleep(10)",
            ], 2, dict(os.environ), monitor=mock.Mock(
                side_effect=PROFILE.ProfileError("fixture pressure")
            ))

    def test_cleanup_disappearance_and_unreaped_child_are_reported(self) -> None:
        process = mock.Mock(pid=12345)
        process.poll.return_value = None
        with mock.patch.object(PROFILE.os, "killpg", side_effect=ProcessLookupError):
            PROFILE.stop_process(process)
        process.wait.side_effect = subprocess.TimeoutExpired("fixture", 2)
        with mock.patch.object(PROFILE.os, "killpg"), \
                self.assertRaisesRegex(PROFILE.ProfileError, "cleanup"):
            PROFILE.stop_process(process)

    def test_receipt_is_private_atomic_and_never_overwrites_a_prior_result(
        self,
    ) -> None:
        PROFILE.atomic_receipt(self.output, {"status": "passed"})
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o600)
        before = self.output.read_bytes()
        with self.assertRaises(FileExistsError):
            PROFILE.atomic_receipt(self.output, {"status": "failed"})
        self.assertEqual(before, self.output.read_bytes())

    def test_atomic_collision_and_size_refusal_preserve_prior_inodes(self) -> None:
        collision = self.root / (".rkc-profile-" + "a" * 32)
        collision.write_bytes(b"unrelated existing temporary")
        with mock.patch.object(PROFILE.secrets, "token_hex", return_value="a" * 32):
            with self.assertRaises(FileExistsError):
                PROFILE.atomic_receipt(self.output, {"status": "passed"})
        self.assertEqual(collision.read_bytes(), b"unrelated existing temporary")
        with self.assertRaises(PROFILE.ProfileError):
            PROFILE.atomic_receipt(self.output, {"oversized": "x" * PROFILE.MAX_OUTPUT})
        self.assertFalse(self.output.exists())

    def run_pair(self, mismatch: bool = False) -> tuple[int, dict]:
        """Exercise orchestration with fake collectors, never invoking a binary."""
        inputs = []

        def fake_run(arguments: list[str], seconds: float, environment: dict,
                     monitor=None, *, lease_fd: int | None = None) -> dict:
            self.assertEqual(environment["RKC_RESOURCE_PROFILE"], "default")
            self.assertGreaterEqual(lease_fd, 10)
            self.assertEqual(os.fstat(lease_fd).st_ino, self.lock.stat().st_ino)
            monitor()
            job = json.loads(Path(arguments[-1]).read_bytes())
            inputs.append(job["source"])
            with self.assertRaises(PROFILE.ProfileError):
                with PROFILE.execution_lock(self.lock):
                    self.fail("observation lost the shared execution lock")
            self.assertEqual(PROFILE.fixture_roster(Path(job["source"])), job["input"])
            digest = "b" * 64 if mismatch and len(inputs) == 2 else "a" * 64
            PROFILE.atomic_receipt(Path(job["receipt"]), {
                "status": "passed", "canonical_hashes": {"bundle.json": digest},
                "deterministic_digest": digest,
            })
            return {"exit_code": 0, "wall_seconds": 0.01}

        arguments = ["profile", "--binary", str(self.binary),
                     "--source", str(self.source),
                     "--execution-lock", str(self.lock), "--output", str(self.output),
                     "--host-reserve-mib", "4096"]
        with mock.patch.object(sys, "argv", arguments), \
                mock.patch.object(PROFILE, "admission", return_value={}), \
                mock.patch.object(PROFILE, "current_cgroup",
                                  return_value=Path("/fixture")), \
                mock.patch.object(PROFILE, "birth_ticks", return_value=42), \
                mock.patch.object(PROFILE, "run_bounded", side_effect=fake_run):
            status = PROFILE.main()
        self.assertEqual(len(inputs), 2)
        self.assertEqual(inputs[0], inputs[1])
        self.assertEqual(self.lock.read_bytes(), b"existing lock contents")
        return status, json.loads(self.output.read_bytes())

    def test_matching_profiles_retain_same_input_and_output_proof(self) -> None:
        status, receipt = self.run_pair()
        self.assertEqual(status, 0)
        self.assertEqual(receipt["status"], "passed")
        self.assertEqual(len(receipt["observations"]), 2)

    def test_different_outputs_fail_the_pair_without_hiding_observations(self) -> None:
        status, receipt = self.run_pair(mismatch=True)
        self.assertEqual(status, 1)
        self.assertEqual(receipt["status"], "failed")
        self.assertEqual(len(receipt["observations"]), 2)

    def test_main_refuses_pressure_before_hashing_binary_or_starting_scan(self) -> None:
        arguments = ["profile", "--binary", str(self.binary),
                     "--source", str(self.source), "--execution-lock", str(self.lock),
                     "--output", str(self.output), "--host-reserve-mib", "4096"]
        refusal = PROFILE.ProfileError("fixture pressure", {"full_avg10": 30})
        with mock.patch.object(sys, "argv", arguments), \
                mock.patch.object(PROFILE, "current_cgroup", return_value=self.root), \
                mock.patch.object(PROFILE, "admission", side_effect=refusal), \
                mock.patch.object(PROFILE, "digest_file") as digest, \
                mock.patch.object(PROFILE, "run_bounded") as run, \
                mock.patch.object(sys, "stderr", io.StringIO()):
            self.assertEqual(PROFILE.main(), 1)
        digest.assert_not_called()
        run.assert_not_called()
        receipt = json.loads(self.output.read_bytes())
        self.assertEqual(receipt["refusal_observation"], {"full_avg10": 30})

    def test_cli_rejects_incoherent_pairs_and_desktop_reserve(self) -> None:
        base = ["profile", "--binary", str(self.binary), "--source", str(self.source),
                "--execution-lock", str(self.lock), "--output", str(self.output)]
        for options in (["--host-reserve-mib", "0"],
                        ["--host-reserve-mib", "4096", "--profile", "A:512:640:256"],
                        ["--host-reserve-mib", "64", "--resource-profile", "desktop"],
                        ["--host-reserve-mib", "4096",
                         "--max-memory-full-avg10", "nan"]):
            with mock.patch.object(sys, "argv", base + options), \
                    mock.patch.object(sys, "stderr", io.StringIO()), \
                    self.assertRaises(SystemExit):
                PROFILE.main()

    def test_unsupported_platform_refuses_without_source_or_process_access(
        self,
    ) -> None:
        with mock.patch.object(sys, "platform", "win32"), \
                mock.patch.object(sys, "stderr", io.StringIO()), \
                mock.patch.object(PROFILE, "execution_lock") as lock:
            self.assertEqual(PROFILE.main(), 2)
        lock.assert_not_called()


if __name__ == "__main__":
    unittest.main()
