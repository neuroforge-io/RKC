#!/usr/bin/env python3
"""Exercise the real guard probe against isolated cgroup/systemd fixtures.

No service is started, real cgroup is changed, or workload is launched. The
wrapper and verifier run unchanged except for redirecting their read-only
cgroup paths into a temporary fixture; fake commands emulate observed state.
"""
from __future__ import annotations

import os
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(
    sys.platform == "linux" and shutil.which("sh"), "Linux shell required"
)
class ResourceGuardVerifierTests(unittest.TestCase):
    def run_probe(
        self,
        selected: str | None,
        cpu_max: str,
        *,
        mode: str = "scope",
        environment_cpu: str | None = None,
        remove_cpu_environment: bool = False,
        controls: dict[str, str] | None = None,
        wrong_main_pid: bool = False,
    ) -> subprocess.CompletedProcess[str]:
        with tempfile.TemporaryDirectory(prefix="rkc-guard-probe-") as temporary:
            work = Path(temporary)
            scripts = work / "scripts"
            binaries = work / "bin"
            cgroup = work / "cgroup"
            template = work / "controls"
            unit_state = work / "units"
            runtime = work / "runtime"
            for directory in (scripts, binaries, cgroup, template, unit_state, runtime):
                directory.mkdir()
            (cgroup / "cgroup.controllers").write_text(
                "cpu memory pids io\n", encoding="ascii"
            )
            observed = {
                "cpu.weight": "1",
                "cpu.max": cpu_max,
                "io.weight": "default 1",
                "memory.high": str(4096 * 1024 * 1024),
                "memory.max": str(4608 * 1024 * 1024),
                "memory.swap.max": str(256 * 1024 * 1024),
                "pids.max": "128",
            }
            observed.update(controls or {})
            for name, value in observed.items():
                (template / name).write_text(value + "\n", encoding="ascii")
            proc_cgroup = work / "proc-self-cgroup"
            verifier = (ROOT / "scripts/verify-resource-guard.sh").read_text(
                encoding="utf-8"
            )
            verifier = verifier.replace("/sys/fs/cgroup", str(cgroup))
            verifier = verifier.replace("/proc/self/cgroup", str(proc_cgroup))
            (scripts / "verify-resource-guard.sh").write_text(
                verifier, encoding="utf-8"
            )
            shutil.copyfile(
                ROOT / "scripts/with-rkc-limits.sh", scripts / "with-rkc-limits.sh"
            )

            fake_commands = {
                "pgrep": "#!/bin/sh\nexit 1\n",
                "readlink": (
                    "#!/bin/sh\n[ \"${1:-}\" = -f ] || exit 1\n"
                    "exec /usr/bin/readlink \"$@\"\n"
                ),
                "nice": "#!/bin/sh\nshift 2\nexec \"$@\"\n",
                "choom": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
                "ionice": (
                    "#!/bin/sh\nif [ \"$1\" = -p ]; then printf 'idle\\n'; "
                    "else shift 2; exec \"$@\"; fi\n"
                ),
                "ps": (
                    "#!/bin/sh\nif [ \"${2:-}\" = ni= ]; then printf '19\\n'; "
                    "else printf '1\\n'; fi\n"
                ),
                "cat": (
                    "#!/bin/sh\n"
                    "case \"$1\" in /proc/*/oom_score_adj) printf '750\\n' ;;\n"
                    "*) exec /bin/cat \"$@\" ;; esac\n"
                ),
                "sleep": f"""#!/bin/sh
set -eu
# Controller fixtures use event-aware waits instead of the production cadence.
# Give the live fake watcher up to one second per readiness/closing check,
# preserving the owner's bounded startup allowance on a busy test host.
for state in {shlex.quote(str(runtime))}/rkc-guard.*; do
    [ -d "$state" ] || continue
    expected=
    if [ -f "$state/closing" ] && [ ! -f "$state/done" ]; then
        expected=done
    elif [ ! -f "$state/ready" ]; then
        expected=ready
    fi
    if [ -n "$expected" ]; then
        attempts=0
        while [ ! -f "$state/$expected" ] && [ "$attempts" -lt 100 ]; do
            /bin/sleep 0.01
            attempts=$((attempts + 1))
        done
        exit 0
    fi
done
/bin/sleep 0.01
""",
                "setsid": f"""#!{sys.executable}
import os
import sys
import time
from pathlib import Path

# Controller tests emulate only the lifecycle acknowledgement. Dedicated
# supervisor tests exercise the real detached watchdog and owner-death paths.
state = Path(sys.argv[4])
with open(os.devnull, "r+b") as detached:
    for descriptor in (0, 1, 2):
        os.dup2(detached.fileno(), descriptor)
(state / "ready").touch()
deadline = time.monotonic() + 10
while not (state / "closing").exists():
    if time.monotonic() >= deadline:
        sys.exit(94)
    time.sleep(0.01)
(state / "done").touch()
""",
                "systemctl": f"""#!{sys.executable}
import os
import sys
from pathlib import Path

arguments = sys.argv[1:]
if any(command in arguments for command in ("is-active", "kill", "stop")):
    sys.exit(0)
unit = arguments[-1]
if "--property=ControlGroup" in arguments:
    print("/fixture/" + unit)
elif "--property=MainPID" in arguments:
    print(os.environ["RKC_FIXTURE_MAIN_PID"])
elif "--property=OOMPolicy" in arguments:
    print("stop")
elif "--property=ActiveState" in arguments:
    print("inactive")
elif "--property=LoadState" in arguments:
    receipt = Path({str(unit_state)!r}) / (unit + ".description")
    print("loaded" if receipt.exists() else "not-found")
elif "--property=Description" in arguments:
    receipt = Path({str(unit_state)!r}) / (unit + ".description")
    print(receipt.read_text(encoding="utf-8") if receipt.exists() else "")
else:
    sys.exit(93)
""",
                "systemd-run": f"""#!{sys.executable}
import os
import shutil
import sys
from pathlib import Path

arguments = sys.argv[1:]
unit = arguments[arguments.index("--unit") + 1]
scope = "--scope" in arguments
environment = dict(os.environ) if scope else {{}}
for argument in arguments:
    if argument.startswith("--setenv="):
        key, value = argument[len("--setenv="):].split("=", 1)
        environment[key] = value
    if argument.startswith("Description="):
        (Path({str(unit_state)!r}) / (unit + ".description")).write_text(
            argument[len("Description="):], encoding="utf-8"
        )
environment["RKC_FIXTURE_MAIN_PID"] = (
    "wrong" if {wrong_main_pid!r} else str(os.getpid())
)
if {environment_cpu!r} is not None:
    environment["RKC_CPU_QUOTA_PERCENT"] = {environment_cpu!r}
if {remove_cpu_environment!r}:
    environment.pop("RKC_CPU_QUOTA_PERCENT", None)
control_group = Path({str(cgroup)!r}) / "fixture" / unit
shutil.copytree({str(template)!r}, control_group)
Path({str(proc_cgroup)!r}).write_text(
    "0::/fixture/" + unit + "\\n" if scope else "0::/\\n", encoding="ascii"
)
payload = arguments[arguments.index("choom"):]
os.execve({str(binaries / 'choom')!r}, payload, environment)
""",
            }
            for name, text in fake_commands.items():
                executable = binaries / name
                executable.write_text(text, encoding="utf-8")
                executable.chmod(0o700)
            environment = {
                key: value for key, value in os.environ.items()
                if not key.startswith("RKC_")
            }
            environment.update({
                "PATH": os.pathsep.join((str(binaries), "/usr/bin", "/bin")),
                "RKC_RESOURCE_GUARD_MODE": mode,
                "XDG_RUNTIME_DIR": str(runtime),
                "DBUS_SESSION_BUS_ADDRESS": "unix:path=" + str(runtime / "bus"),
            })
            if selected is not None:
                environment["RKC_CPU_QUOTA_PERCENT"] = selected
            # A bound Unix socket satisfies the existing read-only bus check;
            # no socket listener or systemd service is created.
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as bus:
                bus.bind(str(runtime / "bus"))
                return subprocess.run(
                    ["/bin/sh", str(scripts / "verify-resource-guard.sh")],
                    cwd=work, env=environment, check=False,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                    text=True, timeout=10,
                )

    def test_selected_and_default_cpu_caps_pass_in_both_launch_modes(self) -> None:
        for mode in ("scope", "service"):
            for selected, observed in (
                (None, "100000 100000"),
                ("", "100000 100000"),
                ("100", "100000 100000"),
                ("25", "25000 100000"),
                ("10", "10000 100000"),
                ("1", "1000 100000"),
                ("25", "50000 200000"),
            ):
                with self.subTest(mode=mode, selected=selected, observed=observed):
                    result = self.run_probe(selected, observed, mode=mode)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("verification: passed", result.stdout)

    def test_drifted_cpu_ratio_is_rejected_even_below_one_core(self) -> None:
        for selected, observed in (
            ("25", "100000 100000"),
            ("25", "26000 100000"),
            ("25", "24000 100000"),
            ("25", "25001 100000"),
            (None, "25000 100000"),
            ("1", "10000 100000"),
        ):
            with self.subTest(selected=selected, observed=observed):
                result = self.run_probe(selected, observed)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("does not match the selected profile", result.stderr)
                self.assertNotIn("verification: passed", result.stdout)

    def test_cpu_environment_cannot_drift_or_disappear(self) -> None:
        for mode in ("scope", "service"):
            for changed in ("100", "10", None):
                with self.subTest(mode=mode, changed=changed):
                    result = self.run_probe(
                        "25", "25000 100000", mode=mode,
                        environment_cpu=changed,
                        remove_cpu_environment=changed is None,
                    )
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertIn("CPU-quota profile did not survive", result.stderr)

    def test_unlimited_and_malformed_cpu_controls_fail_closed(self) -> None:
        for observed in (
            "max 100000", "", "25000", "25000 100000 extra",
            "25000 100000\n25000 100000",
            "0 100000", "25000 0", "-25000 100000", "25000 +100000", "25000.0 100000",
            "025000 100000", "25000 0100000", "nan 100000", "2500000 10000000",
            "9" * 64 + " 100000", "PRIVATE_CPU_CONTROL_SENTINEL 100000",
        ):
            with self.subTest(observed=observed):
                result = self.run_probe("25", observed)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("CPUQuota", result.stderr)
                self.assertNotIn("PRIVATE_CPU_CONTROL_SENTINEL", result.stderr)

    def test_cpu_verification_preserves_other_controller_checks(self) -> None:
        for control, value, failure in (
            ("cpu.weight", "2", "CPUWeight"),
            ("io.weight", "default 2", "IOWeight"),
            ("memory.high", "max", "MemoryHigh"),
            ("memory.max", "max", "MemoryMax"),
            ("memory.swap.max", "max", "MemorySwapMax"),
            ("pids.max", "max", "TasksMax"),
        ):
            with self.subTest(control=control):
                result = self.run_probe("25", "25000 100000", controls={control: value})
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn(failure, result.stderr)

    def test_service_namespace_still_requires_exact_probe_pid(self) -> None:
        result = self.run_probe(
            "25", "25000 100000", mode="service", wrong_main_pid=True
        )
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("MainPID does not match", result.stderr)


if __name__ == "__main__":
    unittest.main()
