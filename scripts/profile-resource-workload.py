#!/usr/bin/env python3
# Copyright 2026 NeuroForgeIO. Licensed under the Apache License, Version 2.0.
"""Linux-only comparison of guarded profiles on a frozen, bounded local fixture.

Requires an existing caller-owned execution lock and receipt parent directory.
Never builds a binary, fetches data, invokes models, or scans the checkout.
Current wrapper policy remains authoritative even for larger requested profiles.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
from dataclasses import asdict, dataclass
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import secrets
import selectors
import signal
import stat
import subprocess
import sys
import tempfile
import time
from typing import Any, Callable, Iterator, Mapping

if sys.platform == "linux":
    import fcntl
    import resource
else:
    fcntl = resource = None

# JSON receipt envelopes are heterogeneous; numeric kernel parsers stay typed.
Record = dict[str, Any]

MIB = 1024 * 1024
MAX_INPUT = MIB
MAX_PATHS = 128
MAX_CONTROL = 65536
MAX_OUTPUT = 2 * MIB
SCRIPT = Path(__file__).resolve()
WRAPPER = SCRIPT.with_name("with-rkc-limits.sh")
PROFILE_ENVIRONMENT = (
    "HOME", "PATH", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL",
    "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
    "RKC_RESOURCE_PROFILE", "RKC_CPU_QUOTA_PERCENT", "RKC_MEMORY_HIGH_MIB",
    "RKC_MEMORY_MAX_MIB", "RKC_MEMORY_SWAP_MAX_MIB", "RKC_GO_MEMORY_LIMIT_MIB",
    "RKC_HOST_AVAILABLE_MEMORY_MIN_MIB", "RKC_REQUIRE_IO_CONTROLLER",
    "RKC_HIGHER_PRIORITY_POLICY", "RKC_HIGHER_PRIORITY_MARKERS",
    "RKC_HIGHER_PRIORITY_LOAD_MAX", "GOMAXPROCS", "GOMEMLIMIT", "OMP_NUM_THREADS",
    "OPENBLAS_NUM_THREADS", "MKL_NUM_THREADS", "NUMEXPR_NUM_THREADS",
    "CMAKE_BUILD_PARALLEL_LEVEL", "CARGO_BUILD_JOBS",
)


class ProfileError(RuntimeError):
    """A bounded observation was refused or could not be proved complete."""

    def __init__(self, message: str, observation: Record | None = None) -> None:
        super().__init__(message)
        self.observation = observation or {}


def profile_environment(source: Mapping[str, str]) -> dict[str, str]:
    """Retain transport paths and guard/runtime controls, omitting provider keys."""
    return {name: source[name] for name in PROFILE_ENVIRONMENT if name in source}


def validate_profile_coverage(coverage: Record) -> None:
    """Validate profile counters; these checks are not the full RKC quality gate."""
    ratio = coverage["inventory_accounting_ratio"]
    valid_ratio = ((type(ratio) is int and ratio == 1) or
                   (type(ratio) is float and math.isfinite(ratio) and ratio == 1))
    names = ("high_confidence_secret_findings", "artifacts_inventoried",
             "nodes_total", "symbols_total")
    if not valid_ratio or any(type(coverage[name]) is not int or coverage[name] < 0
                              for name in names):
        raise ProfileError("profile coverage counters are malformed")
    if coverage["high_confidence_secret_findings"] != 0:
        raise ProfileError("profile inventory or secret parity gates failed")


@dataclass(frozen=True)
class Profile:
    """One named soft/hard/Go memory envelope, measured in MiB."""

    name: str
    high: int
    maximum: int
    go: int

    @classmethod
    def parse(cls, text: str) -> Profile:
        """Parse NAME:HIGH:MAX:GO without relaxing the real guard policy."""
        parts = text.split(":")
        if len(parts) != 4 or not parts[0].isascii() or not parts[0].isalnum():
            raise argparse.ArgumentTypeError("profile must be NAME:HIGH:MAX:GO")
        try:
            high, maximum, go = (int(value) for value in parts[1:])
        except ValueError as exc:
            raise argparse.ArgumentTypeError("profile sizes must be integers") from exc
        if not 64 <= go <= high <= maximum <= 16384:
            raise argparse.ArgumentTypeError("require 64 <= Go <= high <= max <= 16384")
        return cls(parts[0], high, maximum, go)


def read_small(path: Path, maximum: int = MAX_CONTROL) -> bytes:
    """Read at most the declared bound, failing on overflow."""
    with path.open("rb") as handle:
        data = handle.read(maximum + 1)
    if len(data) > maximum:
        raise ProfileError("bounded input overflow")
    return data


def owned_open(path: Path, flags: int) -> int:
    """Open an owned leaf through directory descriptors without symlinks."""
    absolute = Path(os.path.abspath(path))
    traversal = os.O_PATH | os.O_DIRECTORY | os.O_NOFOLLOW
    directory = os.open("/", traversal)
    try:
        for part in absolute.parts[1:-1]:
            child = os.open(part, traversal, dir_fd=directory)
            os.close(directory)
            directory = child
        descriptor = os.open(absolute.name, flags | os.O_NOFOLLOW | os.O_NONBLOCK,
                             dir_fd=directory)
        if os.fstat(descriptor).st_uid != os.getuid():
            os.close(descriptor)
            raise ProfileError("input leaf is not owned by the caller")
        return descriptor
    finally:
        os.close(directory)


def identity(info: os.stat_result) -> tuple[int, int, int, int]:
    """Compare content identity without treating a read's atime update as mutation."""
    return info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns


def prove_lock(path: Path, descriptor: int) -> None:
    """Refuse a replaced lock pathname while retaining the originally locked inode."""
    observed = owned_open(path, os.O_RDONLY)
    try:
        if identity(os.fstat(observed)) != identity(os.fstat(descriptor)):
            raise ProfileError("execution lock pathname changed")
    finally:
        os.close(observed)


@contextmanager
def inherited_lease(descriptor: int) -> Iterator[int]:
    """Let the existing watchdog retain this same flock through cleanup."""
    lease = fcntl.fcntl(descriptor, fcntl.F_DUPFD_CLOEXEC, 10)
    try:
        yield lease
    finally:
        os.close(lease)


def birth_ticks(pid: int) -> int:
    """Read the kernel start-time identity, ignoring arbitrary comm bytes."""
    fields = read_small(Path(f"/proc/{pid}/stat")).rsplit(b") ", 1)
    tail = fields[-1].split()
    if (len(fields) != 2 or len(tail) < 20 or tail[0] in (b"Z", b"X") or
            not tail[19].isdigit()):
        raise ProfileError("profile owner identity is unavailable")
    return int(tail[19])


def prove_owner(job: Record) -> None:
    """Pin owner birth, its open lock inode and the current coordination pathname."""
    try:
        if birth_ticks(job["owner"]) != job["owner_birth"]:
            raise ProfileError("profile owner birth identity changed")
        owner = os.stat(f"/proc/{job['owner']}/fd/{job['lock_fd']}")
        descriptor = owned_open(Path(job["lock_path"]), os.O_RDONLY)
        try:
            current = os.fstat(descriptor)
            if ([owner.st_dev, owner.st_ino] != job["lock_identity"] or
                    [current.st_dev, current.st_ino] != job["lock_identity"]):
                raise ProfileError("profile owner or coordination inode changed")
        finally:
            os.close(descriptor)
    except OSError as exc:
        raise ProfileError("profile owner or its lock is unavailable") from exc


@contextmanager
def execution_lock(path: Path) -> Iterator[int]:
    """Hold an existing regular lock; never create, truncate or replace it."""
    descriptor = owned_open(path, os.O_RDWR | os.O_CLOEXEC)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o002:
            raise ProfileError("execution lock is not an owner-controlled regular file")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise ProfileError("execution lock is busy") from exc
        prove_lock(path, descriptor)
        yield descriptor
    finally:
        os.close(descriptor)


def fixture_roster(source: Path, destination: Path | None = None) -> list[Record]:
    """Hash or freeze at most 128 owned paths and 1 MiB; reject special files."""
    roster: list[Record] = []
    total = paths = 0

    def visit(directory: int, relative: Path) -> None:
        nonlocal total, paths
        with os.scandir(directory) as entries:
            names = []
            for entry in entries:
                paths += 1
                if paths > MAX_PATHS:
                    raise ProfileError("fixture exceeds 128 encountered paths")
                names.append(entry.name)
        for name in sorted(names):
            info = os.stat(name, dir_fd=directory, follow_symlinks=False)
            if info.st_uid != os.getuid() or not (
                stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode)
            ):
                raise ProfileError("fixture contains an unowned or special path")
            child = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                            dir_fd=directory)
            try:
                before = os.fstat(child)
                if (info.st_dev, info.st_ino) != (before.st_dev, before.st_ino):
                    raise ProfileError("fixture path changed during capture")
                path = relative / name
                if stat.S_ISDIR(before.st_mode):
                    if destination is not None:
                        (destination / path).mkdir(mode=0o700)
                    visit(child, path)
                else:
                    with os.fdopen(os.dup(child), "rb") as handle:
                        data = handle.read(MAX_INPUT - total + 1)
                    total += len(data)
                    if (total > MAX_INPUT or
                            identity(before) != identity(os.fstat(child))):
                        raise ProfileError("fixture exceeded its bound or changed")
                    roster.append({"path": path.as_posix(), "bytes": len(data),
                                   "sha256": hashlib.sha256(data).hexdigest()})
                    if destination is not None:
                        (destination / path).write_bytes(data)
                        (destination / path).chmod(0o400)
            finally:
                os.close(child)

    descriptor = owned_open(source, os.O_RDONLY | os.O_DIRECTORY)
    try:
        visit(descriptor, Path())
    finally:
        os.close(descriptor)
    if not roster:
        raise ProfileError("fixture contains no regular files")
    if destination is not None:
        for current, directories, _ in os.walk(destination, topdown=False):
            for name in directories:
                (Path(current) / name).chmod(0o500)
        destination.chmod(0o500)
    return sorted(roster, key=lambda item: item["path"])


def parse_psi(data: bytes) -> dict[str, dict[str, float | int]]:
    """Parse kernel PSI records strictly, including finite percentage bounds."""
    result = {}
    for line in data.decode("ascii").splitlines():
        fields = line.split()
        if not fields or fields[0] not in {"some", "full"} or fields[0] in result:
            raise ProfileError("malformed PSI record")
        values = dict(field.split("=", 1) for field in fields[1:])
        if len(fields) != 5 or set(values) != {"avg10", "avg60", "avg300", "total"}:
            raise ProfileError("malformed PSI fields")
        parsed = {key: float(values[key]) for key in ("avg10", "avg60", "avg300")}
        if any(not math.isfinite(value) or not 0 <= value <= 100
               for value in parsed.values()) or not values["total"].isdigit():
            raise ProfileError("malformed PSI values")
        result[fields[0]] = {**parsed, "total": int(values["total"])}
    if set(result) != {"some", "full"}:
        raise ProfileError("PSI observation is incomplete")
    return result


def current_cgroup() -> Path:
    """Resolve only the process's unified kernel cgroup, including root namespaces."""
    records = read_small(Path("/proc/self/cgroup")).decode("ascii").splitlines()
    groups = [line[3:] for line in records if line.startswith("0::")]
    if len(groups) != 1 or not groups[0].startswith("/"):
        raise ProfileError("unified cgroup membership is unavailable")
    if ".." in PurePosixPath(groups[0]).parts:
        raise ProfileError("unified cgroup path is unsafe")
    return Path("/sys/fs/cgroup") / groups[0].lstrip("/")


def counters(data: bytes) -> dict[str, int]:
    """Parse nonnegative, unique kernel counter pairs."""
    result = {}
    for line in data.decode("ascii").splitlines():
        fields = line.split()
        if len(fields) != 2 or fields[0] in result or not fields[1].isdigit():
            raise ProfileError("malformed kernel counters")
        result[fields[0]] = int(fields[1])
    if not result:
        raise ProfileError("kernel counters are absent")
    return result


def observe(group: Path) -> Record:
    """Capture bounded counters and pressure while the fresh unit still exists."""
    cpu = counters(read_small(group / "cpu.stat"))
    events = counters(read_small(group / "memory.events"))
    if not {"usage_usec", "nr_periods", "nr_throttled", "throttled_usec"} <= cpu.keys():
        raise ProfileError("required CPU accounting counters are absent")
    if not {"high", "max", "oom", "oom_kill"} <= events.keys():
        raise ProfileError("required memory event counters are absent")
    return {
        "cpu": cpu,
        "events": events,
        "memory_current": int(read_small(group / "memory.current")),
        "memory_peak": int(read_small(group / "memory.peak")),
        "memory_psi": parse_psi(read_small(group / "memory.pressure")),
        "cpu_psi": parse_psi(read_small(group / "cpu.pressure")),
    }


def admission(group: Path, reserve: int, allowance: int, some: float,
              full: float) -> Record:
    """Refuse stale/unknown/high pressure or insufficient physical headroom."""
    memory = counters(read_small(Path("/proc/meminfo")).replace(b" kB", b""))
    available = memory.get("MemAvailable:", -1) * 1024
    psi = {"global": parse_psi(read_small(Path("/proc/pressure/memory"))),
           "cgroup": parse_psi(read_small(group / "memory.pressure"))}
    observed = {"available_bytes": available, "memory_psi": psi,
                "global_cpu_psi": parse_psi(read_small(Path("/proc/pressure/cpu")))}
    if (available > memory.get("MemTotal:", -1) * 1024 or
            available < (reserve + allowance) * MIB):
        raise ProfileError("host reserve or profile headroom is unavailable", observed)
    for value in psi.values():
        if value["some"]["avg10"] > some or value["full"]["avg10"] > full:
            raise ProfileError("memory pressure exceeds admission thresholds", observed)
    return observed


def prove_controls(group: Path, profile: Profile, cpu: int) -> dict[str, str]:
    """Require selected controls exactly, rather than trusting environment labels."""
    controls = {name: read_small(group / name).decode("ascii").strip()
                for name in ("cpu.max", "cpu.weight", "memory.high",
                             "memory.max", "memory.swap.max")}
    quota = controls["cpu.max"].split()
    if (len(quota) != 2 or not all(value.isdigit() for value in quota)
            or int(quota[1]) <= 0 or int(quota[0]) * 100 != int(quota[1]) * cpu):
        raise ProfileError("selected CPU quota is unproven")
    expected = {"cpu.weight": "1", "memory.high": str(profile.high * MIB),
                "memory.max": str(profile.maximum * MIB), "memory.swap.max": "0"}
    if any(controls[key] != value for key, value in expected.items()):
        raise ProfileError("selected memory or CPU-weight controls are unproven")
    if (os.environ.get("GOMEMLIMIT") != f"{profile.go}MiB" or
            os.environ.get("GOMAXPROCS") != "1"):
        raise ProfileError("selected Go memory limit is unproven")
    return controls


def stop_process(process: subprocess.Popen[bytes]) -> None:
    """Reap only this owned session, allowing TERM cleanup before KILL."""
    for action in (signal.SIGTERM, signal.SIGKILL):
        if process.poll() is not None:
            return
        try:
            os.killpg(process.pid, action)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=2)
            return
        except subprocess.TimeoutExpired:
            continue
    raise ProfileError("owned command cleanup could not be confirmed")


def run_bounded(arguments: list[str], seconds: float, environment: dict[str, str],
                monitor: Callable[[], object] | None = None, *,
                lease_fd: int | None = None) -> Record:
    """Drain bounded output without retaining source values; stop on interruption."""
    started = time.monotonic()
    process = subprocess.Popen(arguments, env=environment, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, start_new_session=True,
                               pass_fds=() if lease_fd is None else (lease_fd,))
    assert process.stdout is not None and process.stderr is not None
    hashes = {name: hashlib.sha256() for name in ("stdout", "stderr")}
    sizes = {name: 0 for name in hashes}
    try:
        with selectors.DefaultSelector() as selector:
            streams = (("stdout", process.stdout), ("stderr", process.stderr))
            for name, stream in streams:
                selector.register(stream, selectors.EVENT_READ, name)
            while selector.get_map() or process.poll() is None:
                if time.monotonic() - started >= seconds:
                    raise ProfileError("observation exceeded its wall-clock deadline")
                if monitor is not None:
                    monitor()
                for key, _ in selector.select(timeout=0.25):
                    data = os.read(key.fileobj.fileno(), 65536)
                    if not data:
                        selector.unregister(key.fileobj)
                    hashes[key.data].update(data)
                    sizes[key.data] += len(data)
                    if sizes[key.data] > MAX_OUTPUT:
                        raise ProfileError("command output exceeded the capture bound")
        return {"exit_code": process.wait(), "wall_seconds": time.monotonic() - started,
                "output": {name: {"bytes": sizes[name], "sha256": value.hexdigest()}
                           for name, value in hashes.items()}}
    finally:
        try:
            stop_process(process)
        finally:
            for stream in (process.stdout, process.stderr):
                if stream is not None:
                    stream.close()


def atomic_receipt(path: Path, document: Record) -> None:
    """Publish a private complete JSON inode without replacing an existing receipt."""
    parent = owned_open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    temporary = ""
    try:
        encoded = (json.dumps(document, sort_keys=True, indent=2) + "\n").encode()
        if len(encoded) > MAX_OUTPUT:
            raise ProfileError("receipt exceeds its size limit")
        candidate = ".rkc-profile-" + secrets.token_hex(16)
        descriptor = os.open(candidate, os.O_WRONLY | os.O_CREAT | os.O_EXCL,
                             0o600, dir_fd=parent)
        temporary = candidate
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        os.link(temporary, path.name, src_dir_fd=parent, dst_dir_fd=parent,
                follow_symlinks=False)
        os.fsync(parent)
    finally:
        if temporary:
            os.unlink(temporary, dir_fd=parent)
        os.close(parent)


def digest_file(path: Path, maximum: int) -> str:
    """Hash a bounded regular file without following links."""
    descriptor = owned_open(path, os.O_RDONLY)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_size > maximum:
            raise ProfileError("digest input is not a bounded regular file")
        digest = hashlib.sha256()
        copied = 0
        with os.fdopen(os.dup(descriptor), "rb") as handle:
            for chunk in iter(lambda: handle.read(MIB), b""):
                copied += len(chunk)
                if copied > maximum:
                    raise ProfileError("digest input grew beyond its bound")
                digest.update(chunk)
        if identity(info) != identity(os.fstat(descriptor)):
            raise ProfileError("digest input changed while reading")
        return digest.hexdigest()
    finally:
        os.close(descriptor)


def scan_arguments(job: Record) -> list[str]:
    """Keep both arms on identical native, offline, fresh-scan capabilities."""
    return [job["binary"], "scan", "--no-python", "--no-git-metadata",
            "--no-cache", "--json", "--fail-on-errors", "--stage-workers", "1",
            "--stage-memory-mib", "512", "--max-files", str(MAX_PATHS),
            "--max-file-bytes", str(MAX_INPUT), "--max-text-bytes", str(MAX_INPUT),
            "--max-repository-bytes", str(MAX_INPUT), "--no-static-site",
            "--no-integrations", "--no-jsonl-graph", "--out", job["output"],
            "--runs-dir", job["runs"], job["source"]]


def collect(job_path: Path) -> int:
    """Collect inside the guarded unit while the outer process retains its lock."""
    descriptor = owned_open(job_path, os.O_RDONLY)
    with os.fdopen(descriptor, "rb") as handle:
        if stat.S_IMODE(os.fstat(handle.fileno()).st_mode) != 0o600:
            raise ProfileError("collector job is not private")
        encoded = handle.read(MAX_CONTROL + 1)
        if len(encoded) > MAX_CONTROL:
            raise ProfileError("collector job exceeds its bound")
        job = json.loads(encoded)
    prove_owner(job)
    profile = Profile(**job["profile"])
    receipt = {"profile": asdict(profile), "status": "failed"}
    try:
        group = current_cgroup()
        receipt["controls"] = prove_controls(group, profile, job["cpu"])
        receipt["before"] = observe(group)
        receipt["admission"] = admission(group, job["reserve"], profile.maximum,
                                          job["some"], job["full"])
        source, output = Path(job["source"]), Path(job["output"])
        if fixture_roster(source) != job["input"]:
            raise ProfileError("frozen input changed before the observation")
        def monitor() -> Record:
            prove_owner(job)
            return admission(group, job["reserve"], 0, job["some"], job["full"])
        result = run_bounded(
            scan_arguments(job), job["seconds"], profile_environment(os.environ),
            monitor,
        )
        receipt["command"], receipt["after"] = result, observe(group)
        receipt["completion_admission"] = monitor()
        if result["exit_code"] != 0 or fixture_roster(source) != job["input"]:
            raise ProfileError("scan failed or changed its frozen input")
        coverage = json.loads(read_small(output / "coverage.json"))
        validate_profile_coverage(coverage)
        digest = coverage["deterministic_output_digest"]
        if (not isinstance(digest, str) or len(digest) != 64 or
                any(value not in "0123456789abcdef" for value in digest)):
            raise ProfileError("canonical deterministic digest is malformed")
        receipt.update({"status": "passed", "canonical_hashes": {
            name: digest_file(output / name, 256 * MIB)
            for name in ("bundle.json", "coverage.json", "search/index.json")},
            "maximum_child_rss_kib": resource.getrusage(
                resource.RUSAGE_CHILDREN).ru_maxrss,
            "deterministic_digest": digest,
            "artifacts": coverage["artifacts_inventoried"],
            "nodes": coverage["nodes_total"],
            "symbols": coverage["symbols_total"], "input_bytes_per_second":
            sum(item["bytes"] for item in job["input"]) / result["wall_seconds"]})
        for label, key in (("cpu_delta", "cpu"), ("memory_events_delta", "events")):
            receipt[label] = {name: value - receipt["before"][key].get(name, 0)
                              for name, value in receipt["after"][key].items()}
            if any(value < 0 for value in receipt[label].values()):
                raise ProfileError("kernel counters decreased during observation")
        receipt["psi_total_delta_usec"] = {
            scope: {kind: receipt["after"][scope][kind]["total"] -
                    receipt["before"][scope][kind]["total"]
                    for kind in ("some", "full")}
            for scope in ("memory_psi", "cpu_psi")
        }
        if any(value < 0 for scope in receipt["psi_total_delta_usec"].values()
               for value in scope.values()):
            raise ProfileError("PSI counters decreased during observation")
    except (OSError, ValueError, KeyError, TypeError,
            ProfileError, KeyboardInterrupt) as exc:
        receipt["status"] = "failed"
        receipt["error_class"] = type(exc).__name__
        if isinstance(exc, ProfileError):
            receipt.update({"reason": str(exc), "refusal_observation": exc.observation})
    atomic_receipt(Path(job["receipt"]), receipt)
    return 0 if receipt["status"] == "passed" else 1


def main() -> int:
    """Acquire admission, compare two profiles, and atomically retain their receipt."""
    if sys.platform != "linux":
        print("bounded resource profiling requires Linux cgroup v2", file=sys.stderr)
        return 2
    if len(sys.argv) == 3 and sys.argv[1] == "--internal-collect":
        return collect(Path(sys.argv[2]))
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("binary", "source", "execution-lock", "output"):
        parser.add_argument(f"--{name}", required=True, type=Path)
    parser.add_argument("--profile", action="append", type=Profile.parse)
    parser.add_argument("--resource-profile", choices=("default", "desktop"),
                        default="default")
    parser.add_argument("--cpu-percent", type=int, choices=range(1, 101), default=25)
    parser.add_argument("--host-reserve-mib", required=True, type=int)
    parser.add_argument("--seconds", type=int, choices=range(5, 46), default=45)
    parser.add_argument("--max-memory-some-avg10", type=float, default=10.0)
    parser.add_argument("--max-memory-full-avg10", type=float, default=1.0)
    args = parser.parse_args()
    profiles = args.profile or [
        Profile("A", 512, 640, 256), Profile("B", 512, 640, 384),
    ]
    if len(profiles) != 2 or profiles[0].name == profiles[1].name:
        parser.error("exactly two distinctly named profiles are required")
    if not 64 <= args.host_reserve_mib <= 65536:
        parser.error("host reserve must be 64..65536 MiB")
    if args.resource_profile == "desktop" and args.host_reserve_mib < 4096:
        parser.error("desktop profile requires a host reserve of at least 4096 MiB")
    if any(not math.isfinite(value) or not 0 <= value < 100 for value in
           (args.max_memory_some_avg10, args.max_memory_full_avg10)):
        parser.error("memory PSI thresholds must be finite percentages below 100")
    if os.path.lexists(args.output):
        parser.error("receipt destination must not already exist")
    receipt = {"schema_version": "rkc.bounded-resource-profile.v1", "status": "failed",
               "resource_profile": args.resource_profile,
               "cpu_percent": args.cpu_percent,
               "host_reserve_mib": args.host_reserve_mib,
               "psi_thresholds": {"some_avg10": args.max_memory_some_avg10,
                                  "full_avg10": args.max_memory_full_avg10},
               "observations": []}
    try:
        with execution_lock(args.execution_lock) as lock:
            receipt["initial_admission"] = admission(
                current_cgroup(), args.host_reserve_mib,
                max(profile.maximum for profile in profiles),
                args.max_memory_some_avg10, args.max_memory_full_avg10,
            )
            receipt["binary_sha256"] = digest_file(args.binary, 128 * MIB)
            with tempfile.TemporaryDirectory(prefix="rkc-profile-") as temporary:
                work = Path(temporary)
                source = work / "fixture"
                source.mkdir(mode=0o700)
                try:
                    roster = fixture_roster(args.source, source)
                    receipt["input"] = roster
                    for index, profile in enumerate(profiles):
                        prove_lock(args.execution_lock, lock)
                        admission(
                            current_cgroup(), args.host_reserve_mib, profile.maximum,
                            args.max_memory_some_avg10, args.max_memory_full_avg10,
                        )
                        job_path = work / f"job{index}.json"
                        result_path = work / f"result{index}.json"
                        info = os.fstat(lock)
                        job = {"owner": os.getpid(), "lock_fd": lock,
                               "owner_birth": birth_ticks(os.getpid()),
                               "lock_path": str(args.execution_lock.absolute()),
                               "lock_identity": [info.st_dev, info.st_ino],
                               "profile": asdict(profile), "cpu": args.cpu_percent,
                               "reserve": args.host_reserve_mib,
                               "seconds": args.seconds,
                               "some": args.max_memory_some_avg10,
                               "full": args.max_memory_full_avg10,
                               "source": str(source), "input": roster,
                               "binary": str(args.binary.absolute()),
                               "output": str(work / f"atlas{index}"),
                               "runs": str(work / f"runs{index}"),
                               "receipt": str(result_path)}
                        atomic_receipt(job_path, job)
                        environment = profile_environment(os.environ)
                        environment.update({
                            "RKC_RESOURCE_PROFILE": args.resource_profile,
                            "RKC_CPU_QUOTA_PERCENT": str(args.cpu_percent),
                            "RKC_MEMORY_HIGH_MIB": str(profile.high),
                            "RKC_MEMORY_MAX_MIB": str(profile.maximum),
                            "RKC_MEMORY_SWAP_MAX_MIB": "0",
                            "RKC_GO_MEMORY_LIMIT_MIB": str(profile.go),
                            "RKC_HOST_AVAILABLE_MEMORY_MIN_MIB":
                            str(args.host_reserve_mib)})
                        with inherited_lease(lock) as lease:
                            launched = run_bounded(
                                ["/bin/sh", str(WRAPPER), sys.executable, "-I", "-S",
                                 str(SCRIPT), "--internal-collect", str(job_path)],
                                args.seconds + 10, environment,
                                monitor=lambda: prove_lock(args.execution_lock, lock),
                                lease_fd=lease,
                            )
                        result = (json.loads(read_small(result_path, MAX_OUTPUT))
                                  if result_path.exists() else {
                                      "profile": asdict(profile), "status": "failed"})
                        result["guarded_total"] = launched
                        receipt["observations"].append(result)
                        if launched["exit_code"] != 0 or result["status"] != "passed":
                            raise ProfileError("guarded observation refused or failed")
                    first, second = receipt["observations"]
                    if (first["canonical_hashes"] != second["canonical_hashes"] or
                            first["deterministic_digest"] !=
                            second["deterministic_digest"]):
                        raise ProfileError("matched canonical outputs differ")
                    if digest_file(args.binary, 128 * MIB) != receipt["binary_sha256"]:
                        raise ProfileError("binary changed during the profile pair")
                    prove_lock(args.execution_lock, lock)
                    receipt["status"] = "passed"
                except (OSError, ValueError, KeyError, TypeError,
                        ProfileError, KeyboardInterrupt) as exc:
                    receipt["error_class"] = type(exc).__name__
                    if isinstance(exc, ProfileError):
                        receipt.update({"reason": str(exc),
                                        "refusal_observation": exc.observation})
                finally:
                    for current, _, _ in os.walk(source):
                        Path(current).chmod(0o700)
            atomic_receipt(args.output, receipt)
        return 0 if receipt["status"] == "passed" else 1
    except (OSError, ValueError, KeyError, TypeError,
            ProfileError, KeyboardInterrupt) as exc:
        receipt["error_class"] = type(exc).__name__
        if isinstance(exc, ProfileError):
            receipt.update({"reason": str(exc), "refusal_observation": exc.observation})
        atomic_receipt(args.output, receipt)
        print("bounded profile failed; inspect the private receipt", file=sys.stderr)
        return 1


def interrupted(_signal: int, _frame: object) -> None:
    """Route TERM through the same owned-child cleanup path as keyboard interruption."""
    raise KeyboardInterrupt


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    sys.exit(main())
