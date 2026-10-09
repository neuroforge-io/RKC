#!/usr/bin/env python3
"""Exercise real guard supervision with tiny commands and isolated fake units.

No service manager or cgroup is contacted. Only fixture-owned process groups
are created, observed, and cleaned up by the fake command endpoints.
"""
from __future__ import annotations

import json
import os
import signal
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GUARD = ROOT / "scripts/with-rkc-limits.sh"


class GuardFixture:
    def __init__(self, directory: Path) -> None:
        self.root = directory
        self.bin = directory / "bin"
        self.units = directory / "units"
        self.runtime = directory / "runtime"
        for path in (self.bin, self.units, self.runtime):
            path.mkdir()
        self.memory = directory / "meminfo"
        self.set_memory(4096)
        self.config = directory / "config.json"
        self.config.write_text("{}", encoding="utf-8")
        self.processes: list[subprocess.Popen[str]] = []
        self.environment = os.environ.copy()
        # Temporary service emulators and one-line fixture payloads are not
        # production coverage targets. Avoid coverage's Python startup work
        # inside the guard's strict controller deadlines. Instrumentation of
        # this test process and all other product subprocesses is unchanged.
        for key in ("COVERAGE_PROCESS_CONFIG", "COVERAGE_PROCESS_START"):
            self.environment.pop(key, None)
        self.environment.update({
            "PATH": f"{self.bin}:/usr/bin:/bin",
            "XDG_RUNTIME_DIR": str(self.runtime),
            "DBUS_SESSION_BUS_ADDRESS": "unix:path=/fixture/no-real-manager",
            "RKC_HIGHER_PRIORITY_MARKERS": "fixture_training",
            "RKC_HIGHER_PRIORITY_POLICY": "yield",
            "RKC_CPU_QUOTA_PERCENT": "25",
            "RKC_MEMORY_HIGH_MIB": "512",
            "RKC_MEMORY_MAX_MIB": "640",
            "RKC_MEMORY_SWAP_MAX_MIB": "0",
            "RKC_GO_MEMORY_LIMIT_MIB": "384",
            "RKC_HOST_AVAILABLE_MEMORY_MIN_MIB": "1024",
        })
        self.write("pgrep", "#!/bin/sh\nexit 1\n")
        self.write("ps", "#!/bin/sh\nprintf '1\\n'\n")
        self.write("readlink", "#!/bin/sh\nexec /usr/bin/readlink \"$@\"\n")
        self.write("awk", (
            "#!/usr/bin/python3\nimport os, sys\n"
            f"memory = {str(self.memory)!r}\n"
            "args = sys.argv[1:]\n"
            "assert args[-1] == '/proc/meminfo'\n"
            "os.execv('/usr/bin/awk', ['awk', *args[:-1], memory])\n"
        ))
        for name, offset in (("choom", 4), ("ionice", 3), ("nice", 3)):
            self.write(name, (
                "#!/usr/bin/python3\nimport os, sys\n"
                f"args = sys.argv[{offset}:]\n"
                "os.execvpe(args[0], args, os.environ)\n"
            ))
        prefix = (
            "#!/usr/bin/python3\n"
            "import json, os, signal, subprocess, sys, time\n"
            "from pathlib import Path\n"
            f"root = Path({str(directory)!r})\n"
            "units = root / 'units'\n"
            "def save(path, value):\n"
            "    temporary = path.with_name(path.name + '.' + str(os.getpid()))\n"
            "    temporary.write_text(json.dumps(value))\n"
            "    temporary.replace(path)\n"
        )
        self.write("systemd-run", prefix + r'''
args = sys.argv[1:]
environment = os.environ.copy()
unit = description = ''
index = 0
while index < len(args):
    value = args[index]
    if value == '--unit':
        unit = args[index + 1]; index += 2; continue
    if value == '--property':
        prop = args[index + 1]
        if prop.startswith('Description='): description = prop.partition('=')[2]
        index += 2; continue
    if value.startswith('--setenv='):
        key, _, content = value[len('--setenv='):].partition('=')
        environment[key] = content
        index += 1; continue
    if value == '--':
        index += 1; break
    if value.startswith('--'):
        index += 1; continue
    break
command = args[index:]
assert unit.startswith('rkc-low-') and description.startswith('rkc-guard-')
save(root / 'invocation.json', {'args': args, 'unit': unit, 'description': description})
configuration = json.loads((root / 'config.json').read_text())
if configuration.get('queued_start'):
    # The fixture parent owns and reaps the queued agent even when this fake
    # systemd-run launcher is killed before the queued request is dispatched.
    save(root / 'queued.json', {
        'unit': unit, 'description': description,
        'command': command, 'environment': environment,
    })
    (root / 'queued').touch()
    time.sleep(60)
    sys.exit(0)
path = units / unit
process = subprocess.Popen(command, env=environment, start_new_session=True)
if configuration.get('wrong_description'): description = 'unrelated-existing-unit'
save(path, {'description': description, 'pid': process.pid, 'active': 'active'})
(root / 'started').touch()
status = process.wait()
if configuration.get('collect'):
    path.unlink(missing_ok=True)
else:
    # Keep the fake unit active until explicit cleanup, like a unit retaining
    # background descendants after its main command has returned.
    state = json.loads(path.read_text())
    state['command_status'] = status
    save(path, state)
sys.exit(status if status >= 0 else 128 - status)
''')
        self.write("unit-agent", prefix + r'''
queued = json.loads(Path(sys.argv[1]).read_text())
unit, description = queued['unit'], queued['description']
command, environment = queued['command'], queued['environment']
owner_state = Path(command[command.index('--rkc-internal-payload') + 1])
deadline = time.monotonic() + 10
while owner_state.exists():
    if time.monotonic() >= deadline: sys.exit(92)
    time.sleep(0.02)
# Publish only after cleanup acknowledgement, deterministically exercising a
# queued manager request that arrives after the original owner state is gone.
process = subprocess.Popen(command, env=environment, start_new_session=True)
path = units / unit
save(path, {'description': description, 'pid': process.pid, 'active': 'active'})
(root / 'late-created').touch()
status = process.wait(timeout=5)
save(root / 'late-child-exited.json', {'unit': unit, 'status': status})
# Hold the transitional manager record until the test acknowledges it. This
# makes observing active before terminal publication intentional, not a race.
deadline = time.monotonic() + 10
while not (root / 'allow-terminal-publication').exists():
    if time.monotonic() >= deadline: sys.exit(93)
    time.sleep(0.02)
save(path, {'description': description, 'pid': process.pid, 'active': 'inactive'})
save(root / 'late-finished.json', {'unit': unit, 'status': status})
''')
        self.write("systemctl", prefix + r'''
args = sys.argv[1:]
unit = args[-1]
path = units / unit
try: state = json.loads(path.read_text())
except FileNotFoundError: state = None
if 'show' in args:
    prop = next((value.partition('=')[2] for value in args if value.startswith('--property=')), '')
    if prop == 'LoadState': print('loaded' if state else 'not-found')
    elif prop == 'Description': print(state['description'] if state else '')
    elif prop == 'ActiveState': print(state['active'] if state else '')
    else: sys.exit(90)
    sys.exit(0)
if 'stop' in args or 'kill' in args:
    with (root / 'cleanup.jsonl').open('a') as output:
        output.write(json.dumps({'args': args, 'unit': unit}) + '\n')
    if state:
        try: os.killpg(state['pid'], signal.SIGKILL)
        except ProcessLookupError: pass
        state['active'] = 'inactive'
        save(path, state)
    sys.exit(0)
sys.exit(91)
''')

    def write(self, name: str, body: str) -> None:
        executable = self.bin / name
        executable.write_text(body, encoding="utf-8")
        executable.chmod(0o700)

    def configure(self, **values: object) -> None:
        self.config.write_text(json.dumps(values), encoding="utf-8")

    def set_memory(self, available_mib: int) -> None:
        self.memory.write_text(
            f"MemTotal:       8388608 kB\nMemAvailable:   {available_mib * 1024} kB\n",
            encoding="utf-8",
        )

    def start(
        self, command: list[str], *, mode: str = "scope", umask: int = -1,
    ) -> subprocess.Popen[str]:
        environment = self.environment.copy()
        environment["RKC_RESOURCE_GUARD_MODE"] = mode
        process = subprocess.Popen(
            ["/bin/sh", str(GUARD), *command], cwd=ROOT, env=environment,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, start_new_session=True, umask=umask,
        )
        self.processes.append(process)
        return process

    def wait_file(self, name: str, timeout: float = 8) -> Path:
        target = self.root / name
        deadline = time.monotonic() + timeout
        while not target.exists():
            if time.monotonic() >= deadline:
                raise AssertionError(f"fixture did not create {name}")
            time.sleep(0.02)
        return target

    def start_queued_agent(self) -> subprocess.Popen[str]:
        process = subprocess.Popen(
            [str(self.bin / "unit-agent"), str(self.root / "queued.json")],
            env=self.environment, stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            text=True, start_new_session=True,
        )
        self.processes.append(process)
        return process

    def cleanup_records(self) -> list[dict[str, object]]:
        path = self.root / "cleanup.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def unit_records(self) -> list[Path]:
        # A killed fake endpoint may leave a staging file. Only its atomically
        # published scope/service record carries authority over a group.
        return [path for path in self.units.iterdir() if path.suffix in (".scope", ".service")]

    def assert_private_state_removed(self, test: unittest.TestCase) -> None:
        deadline = time.monotonic() + 8
        while list(self.runtime.glob("rkc-guard.*")) and time.monotonic() < deadline:
            time.sleep(0.02)
        test.assertEqual(list(self.runtime.glob("rkc-guard.*")), [])

    def cleanup(self) -> None:
        # Fixture groups only; never discover or target unrelated processes.
        for process in self.processes:
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            try:
                process.communicate(timeout=1)
            except subprocess.TimeoutExpired:
                pass
        for path in self.unit_records():
            try:
                state = json.loads(path.read_text())
                os.killpg(state["pid"], signal.SIGKILL)
            except (ProcessLookupError, FileNotFoundError):
                pass


class ResourceGuardLifecycleTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.fixture = GuardFixture(Path(self.temporary.name))
        self.addCleanup(self.temporary.cleanup)
        self.addCleanup(self.fixture.cleanup)

    def assert_quiescent(self) -> None:
        self.fixture.assert_private_state_removed(self)
        for path in self.fixture.unit_records():
            state = json.loads(path.read_text())
            self.assertEqual(state["active"], "inactive")
            try:
                process_stat = Path(f"/proc/{state['pid']}/stat").read_text()
            except FileNotFoundError:
                continue
            self.assertIn(process_stat.rsplit(") ", 1)[1].split()[0], ("Z", "X"))

    def test_admission_refuses_low_unreadable_and_invalid_meminfo_privately(self) -> None:
        sentinel = "PRIVATE_MEMORY_CONTENT_SENTINEL"
        cases = (
            "MemTotal: 8388608 kB\nMemAvailable: 1048575 kB\n",
            "MemTotal: 8388608 kB\n",
            "MemTotal: 8388608 kB\nMemAvailable: 4194304 MB\n",
            "MemTotal: 8388608 kB\nMemAvailable: 4194304 kB\nMemAvailable: 4194304 kB\n",
            "MemTotal: 8388608 kB\nMemAvailable: 99999999999999999 kB\n",
            "MemTotal: 8388608 kB\nMemAvailable: 9999999 kB\n",
            f"MemTotal: {sentinel} kB\nMemAvailable: 4194304 kB\n",
            None,
        )
        for content in cases:
            with self.subTest(content=content):
                if content is None:
                    self.fixture.memory.unlink(missing_ok=True)
                else:
                    self.fixture.memory.write_text(content, encoding="utf-8")
                process = self.fixture.start(["true"])
                _, error = process.communicate(timeout=5)
                self.assertEqual(process.returncode, 75, error)
                self.assertIn("refusing to start", error)
                self.assertNotIn(sentinel, error)
                self.assertNotIn(str(self.fixture.root), error)
                self.assertFalse((self.fixture.root / "invocation.json").exists())
                self.assertEqual(list(self.fixture.runtime.iterdir()), [])

    def test_zero_reserve_does_not_require_host_meminfo_and_collected_exit_is_clean(self) -> None:
        self.fixture.environment["RKC_HOST_AVAILABLE_MEMORY_MIN_MIB"] = "0"
        self.fixture.memory.unlink()
        self.fixture.configure(collect=True)
        process = self.fixture.start(["true"])
        _, error = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 0, error)
        self.assertEqual(self.fixture.cleanup_records(), [])
        self.assert_quiescent()

    def test_argument_bytes_stdin_and_selected_limits_survive_both_modes(self) -> None:
        arguments = ["", "with space", "line\nbreak", "$HOME", "$(false)", "évidence"]
        script = (
            "import json, os, sys; "
            "print(json.dumps({'args': sys.argv[1:], 'input': sys.stdin.read(), "
            "'threads': os.environ['GOMAXPROCS'], 'memory': os.environ['GOMEMLIMIT'], "
            "'coverage_bootstrap': any(key in os.environ for key in "
            "('COVERAGE_PROCESS_CONFIG', 'COVERAGE_PROCESS_START'))}))"
        )
        for mode in ("scope", "service"):
            with self.subTest(mode=mode):
                process = self.fixture.start(["/usr/bin/python3", "-c", script, *arguments], mode=mode)
                output, error = process.communicate(input="interactive input\n", timeout=12)
                self.assertEqual(process.returncode, 0, error)
                receipt = json.loads(output)
                self.assertEqual(receipt, {
                    "args": arguments, "input": "interactive input\n",
                    "threads": "1", "memory": "384MiB", "coverage_bootstrap": False,
                })
                invocation = json.loads((self.fixture.root / "invocation.json").read_text())
                for selected in ("CPUQuota=25%", "MemoryHigh=512M", "MemoryMax=640M", "MemorySwapMax=0M"):
                    self.assertIn(selected, invocation["args"])
                self.assertIn("--expand-environment=no", invocation["args"])
                self.assert_quiescent()

    def test_normal_nonzero_exit_cleans_background_descendants_and_preserves_status(self) -> None:
        descendant = self.fixture.root / "descendant"
        process = self.fixture.start([
            "/bin/sh", "-c", 'sleep 60 & printf "%s\\n" "$!" > "$1"; exit 28', "fixture", str(descendant),
        ])
        _, error = process.communicate(timeout=12)
        self.assertEqual(process.returncode, 28, error)
        self.assertTrue(descendant.exists())
        self.assertGreater(len(self.fixture.cleanup_records()), 0)
        self.assert_quiescent()
        child_pid = int(descendant.read_text())
        stat = Path(f"/proc/{child_pid}/stat")
        if stat.exists():
            self.assertIn(stat.read_text().rsplit(") ", 1)[1].split()[0], ("Z", "X"))

    def test_payload_preserves_caller_umask_while_supervisor_receipts_stay_private(self) -> None:
        for mode in ("scope", "service"):
            with self.subTest(mode=mode):
                output_file = self.fixture.root / f"{mode}-output"
                output_dir = self.fixture.root / f"{mode}-directory"
                mask_file = self.fixture.root / f"{mode}-mask"
                process = self.fixture.start([
                    "/bin/sh", "-c",
                    'touch "$1"; mkdir "$2"; umask > "$3"; sleep 60',
                    "fixture", str(output_file), str(output_dir), str(mask_file),
                ], mode=mode, umask=0o022)
                self.fixture.wait_file(mask_file.name)
                self.assertEqual(int(mask_file.read_text().strip(), 8), 0o022)
                self.assertEqual(output_file.stat().st_mode & 0o777, 0o644)
                self.assertEqual(output_dir.stat().st_mode & 0o777, 0o755)
                states = list(self.fixture.runtime.glob("rkc-guard.*"))
                self.assertEqual(len(states), 1)
                self.assertEqual(states[0].stat().st_mode & 0o777, 0o700)
                for name in ("owner", "launcher", "ready"):
                    self.assertEqual((states[0] / name).stat().st_mode & 0o777, 0o600)
                invocation = json.loads((self.fixture.root / "invocation.json").read_text())
                masks = [value.partition("=")[2] for value in invocation["args"] if value.startswith("UMask=")]
                if mode == "service":
                    self.assertEqual(len(masks), 1)
                    self.assertEqual(int(masks[0], 8), 0o022)
                else:
                    self.assertEqual(masks, [])
                process.send_signal(signal.SIGTERM)
                _, error = process.communicate(timeout=12)
                self.assertEqual(process.returncode, 143, error)
                self.assert_quiescent()

    def test_running_low_or_unreadable_memory_cancels_arbitrary_command(self) -> None:
        for missing in (False, True):
            with self.subTest(unreadable=missing):
                self.fixture.set_memory(4096)
                (self.fixture.root / "started").unlink(missing_ok=True)
                process = self.fixture.start(["/bin/sleep", "60"])
                self.fixture.wait_file("started")
                if missing:
                    self.fixture.memory.unlink()
                else:
                    self.fixture.set_memory(512)
                _, error = process.communicate(timeout=12)
                self.assertEqual(process.returncode, 75, error)
                self.assertIn("cancelling workload", error)
                self.assertNotIn(str(self.fixture.root), error)
                self.assert_quiescent()

    def test_term_interrupt_preserves_status_and_cleans_owned_unit(self) -> None:
        process = self.fixture.start(["/bin/sleep", "60"], mode="service")
        self.fixture.wait_file("started")
        process.send_signal(signal.SIGTERM)
        _, error = process.communicate(timeout=12)
        self.assertEqual(process.returncode, 143, error)
        self.assert_quiescent()

    def test_detached_watchdog_cleans_after_owner_or_entire_launcher_group_is_killed(self) -> None:
        for entire_group in (False, True):
            with self.subTest(entire_group=entire_group):
                (self.fixture.root / "started").unlink(missing_ok=True)
                process = self.fixture.start(["/bin/sleep", "60"])
                self.fixture.wait_file("started")
                if entire_group:
                    os.killpg(process.pid, signal.SIGKILL)
                else:
                    process.kill()
                process.communicate(timeout=12)
                self.assertEqual(process.returncode, -signal.SIGKILL)
                self.assert_quiescent()

    def test_late_queued_unit_cannot_start_work_after_interrupted_creation(self) -> None:
        self.fixture.configure(queued_start=True)
        forbidden = self.fixture.root / "work-started-too-late"
        process = self.fixture.start(["/usr/bin/touch", str(forbidden)])
        self.fixture.wait_file("queued")
        agent = self.fixture.start_queued_agent()
        process.kill()
        process.communicate(timeout=12)
        self.fixture.wait_file("late-created")
        exited = json.loads(self.fixture.wait_file("late-child-exited.json").read_text())
        self.assertEqual(exited["status"], 75)
        transitional = json.loads((self.fixture.units / exited["unit"]).read_text())
        self.assertEqual(transitional["active"], "active")
        (self.fixture.root / "allow-terminal-publication").touch()
        finished = json.loads(self.fixture.wait_file("late-finished.json").read_text())
        self.assertEqual(finished, exited)
        self.assertEqual(agent.wait(timeout=5), 0)
        self.assertFalse(forbidden.exists())
        self.assert_quiescent()

    def test_existing_unit_with_wrong_description_is_never_stopped(self) -> None:
        self.fixture.configure(wrong_description=True)
        process = self.fixture.start(["true"])
        _, error = process.communicate(timeout=12)
        self.assertEqual(process.returncode, 1, error)
        self.assertIn("could not verify owned workload cleanup", error)
        self.assertNotIn(str(self.fixture.root), error)
        self.assertEqual(self.fixture.cleanup_records(), [])

    def test_internal_entry_rejects_arbitrary_existing_units(self) -> None:
        state = self.fixture.runtime / "rkc-guard.ABCDEFGHIJKLMNOP"
        state.mkdir(mode=0o700)
        owner = os.getpid()
        birth = Path(f"/proc/{owner}/stat").read_text().rsplit(") ", 1)[1].split()[19]
        # Even a shaped private directory cannot authorize an arbitrary name.
        (state / "owner").write_text(f"{owner} {birth} scope unrelated.service 0 0022\n")
        for entry in ("--rkc-internal-watchdog", "--rkc-internal-launcher", "--rkc-internal-payload"):
            result = subprocess.run(
                ["/bin/sh", str(GUARD), entry, str(state), str(owner), birth, "scope", "true"],
                env=self.fixture.environment, capture_output=True, text=True, timeout=3,
            )
            self.assertEqual(result.returncode, 75, result.stderr)
        self.assertEqual(self.fixture.cleanup_records(), [])


if __name__ == "__main__":
    unittest.main()
