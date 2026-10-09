#!/usr/bin/env python3
"""Static and syntax contract tests for the repository shell workflows.

The workflows intentionally perform release, resource-guard, and smoke
operations that are not safe to execute from a unit-test discovery run. These
checks still prove that every checked-in entry point has a strict shell mode,
is executable by an available interpreter, and remains parseable. The guarded
release/CI jobs provide the execution evidence for the destructive paths.
"""
from __future__ import annotations

import ast
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SHELL_WORKFLOWS = (
    "install.sh",
    "scripts/benchmark-reference.sh",
    "scripts/build-release-binaries.sh",
    "scripts/check-portable-builds.sh",
    "scripts/generate-demo.sh",
    "scripts/install-package.sh",
    "scripts/install-release.sh",
    "scripts/reproducibility.sh",
    "scripts/reproducible-complete-package.sh",
    "scripts/self-catalogue.sh",
    "scripts/smoke-api.sh",
    "scripts/smoke-git-acquisition.sh",
    "scripts/smoke-mcp.sh",
    "scripts/smoke-reference.sh",
    "scripts/test_install.sh",
    "scripts/validate-dco.sh",
    "scripts/verify-release.sh",
    "scripts/verify-resource-guard.sh",
    "scripts/with-rkc-limits.sh",
)


def supervisor_fixture_commands() -> dict[str, str]:
    """Keep flag/privacy fixtures isolated from the real service manager.

    Lifecycle tests exercise the real watchdog separately. These fixtures
    only acknowledge its owner handshake and explicitly report no fake unit.
    """
    return {
        "choom": '#!/bin/sh\nshift 3\nexec "$@"\n',
        "ionice": '#!/bin/sh\nshift 2\nexec "$@"\n',
        "nice": '#!/bin/sh\nshift 2\nexec "$@"\n',
        "setsid": (
            "#!/usr/bin/python3\n"
            "import sys, time\nfrom pathlib import Path\n"
            "state = Path(sys.argv[4])\n"
            "(state / 'ready').touch()\n"
            "while not (state / 'closing').exists(): time.sleep(0.01)\n"
            "(state / 'done').touch()\n"
        ),
        "systemctl": (
            "#!/bin/sh\n"
            "case \"$*\" in *LoadState*) printf 'not-found\\n' ;; "
            "*ActiveState*) printf 'inactive\\n' ;; esac\n"
        ),
        "awk": "#!/bin/sh\nexit 0\n",
    }


def fixture_executable(path: Path, body: str) -> None:
    if path.name == "readlink":
        body = body.replace(
            "#!/bin/sh\n",
            '#!/bin/sh\nif [ "${1-}" = -f ]; then exec /usr/bin/readlink "$@"; fi\n',
            1,
        )
    path.write_text(body, encoding="utf-8")
    path.chmod(0o700)


class ShellWorkflowTests(unittest.TestCase):
    def test_release_demo_scans_the_exact_git_root(self) -> None:
        text = (ROOT / "scripts/generate-demo.sh").read_text(encoding="utf-8")
        self.assertIn(
            'set -- "$WORK/rkc" scan --no-python --out "$OUT" --force', text
        )
        self.assertIn('if [ "$name" != examples ]; then', text)
        self.assertIn('set -- "$@" --exclude "$name"', text)
        self.assertIn('set -- "$@" "$SOURCE"', text)
        self.assertIn("run_and_publish demo-scan.txt scan_demo_fixture", text)
        self.assertIn('--repo-root "$SOURCE"', text)
        self.assertNotIn(
            'scan --no-python --out "$OUT" --force "$SOURCE/examples"', text
        )
        self.assertNotIn('--repo-root "$SOURCE/examples"', text)

    def test_reference_benchmark_uses_private_clean_scan_state(self) -> None:
        text = (ROOT / "scripts/benchmark-reference.sh").read_text(encoding="utf-8")
        self.assertIn("--no-cache", text)
        self.assertIn('--runs-dir "$WORK/runs"', text)

    def test_release_benchmark_precedes_race_and_receipt_order_matches(self) -> None:
        text = (ROOT / "scripts/verify-release.sh").read_text(encoding="utf-8")
        inventory = re.search(r"^EXPECTED_STEPS='([^']+)'$", text, re.MULTILINE)
        self.assertIsNotNone(inventory)
        assert inventory is not None
        expected = tuple(inventory.group(1).split())
        executed = tuple(re.findall(r"^run_step ([a-z-]+) ", text, re.MULTILINE))
        self.assertEqual(len(expected), 18)
        self.assertEqual(len(set(expected)), 18)
        self.assertEqual(executed, expected)
        self.assertEqual(executed[-2:], ("benchmark", "race"))

        package = ast.parse(
            (ROOT / "scripts/package-complete.py").read_text(encoding="utf-8")
        )
        release_steps = [
            node.value
            for node in package.body
            if isinstance(node, ast.Assign)
            and any(
                isinstance(target, ast.Name) and target.id == "RELEASE_STEPS"
                for target in node.targets
            )
        ]
        self.assertEqual(len(release_steps), 1)
        self.assertEqual(ast.literal_eval(release_steps[0]), expected)

    def test_workflows_exist_have_strict_mode_and_parse(self) -> None:
        discovered = {"install.sh"} | {
            path.relative_to(ROOT).as_posix() for path in (ROOT / "scripts").glob("*.sh")
        }
        self.assertEqual(set(SHELL_WORKFLOWS), discovered)
        for relative in SHELL_WORKFLOWS:
            with self.subTest(relative=relative):
                path = ROOT / relative
                self.assertTrue(path.is_file(), relative)
                text = path.read_text(encoding="utf-8")
                self.assertRegex(
                    text.splitlines()[0], r"^#!/(?:usr/bin/env )?(?:bin/)?(?:ba)?sh\s*$"
                )
                self.assertRegex(text, re.compile(r"^set -e(?:u|uo pipefail)?$", re.MULTILINE))

                interpreter = "bash" if "bash" in text.splitlines()[0] else "sh"
                if shutil.which(interpreter) is None:
                    self.skipTest(f"{interpreter} is unavailable on this platform")
                result = subprocess.run(
                    [interpreter, "-n", str(path)],
                    cwd=ROOT,
                    check=False,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                )
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_resource_guard_never_reports_priority_process_argv(self) -> None:
        sentinel = "SUPER_SECRET_PRIORITY_ARGV_SENTINEL"
        with tempfile.TemporaryDirectory() as temporary:
            binary_dir = Path(temporary)
            scripts = {
                "pgrep": (
                    "#!/bin/sh\n"
                    "[ \"$1\" = -f ] || exit 98\n"
                    "case \"$2\" in\n"
                    "  *python*) printf '%s\\n' 888888 ;;\n"
                    "  *) printf '%s\\n' 999999 'NOT_A_PID /private/project/erais/train.py "
                    f"--token {sentinel}' '123x' ;;\n"
                    "esac\n"
                ),
                "ps": "#!/bin/sh\nprintf '1\\n'\n",
                "readlink": (
                    "#!/bin/sh\n"
                    f"[ \"$1\" = /proc/888888/cwd ] && printf '%s\\n' '/private/{sentinel}/erais'\n"
                ),
            }
            for name in ("systemd-run", "ionice", "nice", "choom"):
                scripts[name] = "#!/bin/sh\nexit 0\n"
            scripts.update(supervisor_fixture_commands())
            for name, body in scripts.items():
                fixture_executable(binary_dir / name, body)
            environment = os.environ.copy()
            environment["PATH"] = os.pathsep.join(
                (str(binary_dir), "/usr/bin", "/bin")
            )
            environment["RKC_HIGHER_PRIORITY_MARKERS"] = "erais"

            def run_guard() -> subprocess.CompletedProcess[str]:
                return subprocess.run(
                    ["/bin/sh", str(ROOT / "scripts/with-rkc-limits.sh"), "true"],
                    cwd=ROOT,
                    env=environment,
                    check=False,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    timeout=10,
                )

            refusal = os.environ.copy()
            refusal["PATH"] = environment["PATH"]
            refusal["RKC_HIGHER_PRIORITY_POLICY"] = "refuse"
            refusal["RKC_HIGHER_PRIORITY_MARKERS"] = "erais"
            refused = subprocess.run(
                ["/bin/sh", str(ROOT / "scripts/with-rkc-limits.sh"), "true"],
                cwd=ROOT,
                env=refusal,
                check=False,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                timeout=10,
            )
            self.assertEqual(refused.returncode, 75, refused.stderr)
            self.assertIn("pid=999999 class=erais", refused.stderr)
            self.assertIn("pid=888888 class=erais", refused.stderr)

            yielded = run_guard()
            self.assertEqual(yielded.returncode, 0, yielded.stderr)
            self.assertIn("yield policy", yielded.stderr)
            self.assertIn("pid=999999 class=erais", yielded.stderr)
            self.assertIn("pid=888888 class=erais", yielded.stderr)

            custom = refusal.copy()
            custom["RKC_HIGHER_PRIORITY_MARKERS"] = "critical_train"
            custom_result = subprocess.run(
                ["/bin/sh", str(ROOT / "scripts/with-rkc-limits.sh"), "true"],
                cwd=ROOT,
                env=custom,
                check=False,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                timeout=10,
            )
            self.assertEqual(custom_result.returncode, 75, custom_result.stderr)
            self.assertIn("pid=999999 class=critical_train", custom_result.stderr)
            self.assertNotIn("class=erais", custom_result.stderr)
        self.assertNotIn(sentinel, refused.stderr)
        self.assertNotIn("/private/project", refused.stderr)
        self.assertNotIn("pid=NOT_A_PID", refused.stderr)
        self.assertNotIn(sentinel, yielded.stderr)
        self.assertNotIn("/private/project", yielded.stderr)
        self.assertNotIn("pid=NOT_A_PID", yielded.stderr)

    def test_resource_guard_rejects_invalid_marker_configuration_privately(
        self,
    ) -> None:
        sentinel = "SUPER_SECRET_MARKER_CONFIGURATION"
        invalid_values = (
            "critical_train,",
            "critical_train,critical_train",
            "CriticalTrain",
            "critical-train",
            "_critical",
            "a" * 33,
            "a" * 256,
            "a," * 16 + "a",
            sentinel,
        )
        for value in invalid_values:
            with self.subTest(value_length=len(value)):
                environment = os.environ.copy()
                environment["RKC_HIGHER_PRIORITY_MARKERS"] = value
                result = subprocess.run(
                    [
                        "/bin/sh",
                        str(ROOT / "scripts/with-rkc-limits.sh"),
                        "true",
                    ],
                    cwd=ROOT,
                    env=environment,
                    check=False,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    timeout=10,
                )
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("RKC_HIGHER_PRIORITY_MARKERS", result.stderr)
                self.assertNotIn(value, result.stderr)
                self.assertNotIn(sentinel, result.stderr)

    def test_resource_guard_rejects_invalid_host_memory_reserve_privately(
        self,
    ) -> None:
        sentinel = "SUPER_SECRET_HOST_MEMORY_RESERVE"
        for value in ("-1", "+1", "1.5", "65537", "0001", "000001", "9" * 64, sentinel):
            with self.subTest(value_length=len(value)):
                environment = os.environ.copy()
                environment["RKC_HOST_AVAILABLE_MEMORY_MIN_MIB"] = value
                result = subprocess.run(
                    ["/bin/sh", str(ROOT / "scripts/with-rkc-limits.sh"), "true"],
                    cwd=ROOT,
                    env=environment,
                    check=False,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    timeout=10,
                )
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("host reserve=0..65536", result.stderr)
                self.assertNotIn(value, result.stderr)
                self.assertNotIn(sentinel, result.stderr)

    def test_resource_guard_cpu_quota_is_bounded_and_private(self) -> None:
        sentinel = "PRIVATE_CPU_QUOTA_SENTINEL"
        for value in ("0", "101", "-1", "+25", "25.5", "025", "9" * 64, sentinel):
            with self.subTest(value_length=len(value)):
                environment = os.environ.copy()
                environment["RKC_CPU_QUOTA_PERCENT"] = value
                result = subprocess.run(
                    ["/bin/sh", str(ROOT / "scripts/with-rkc-limits.sh"), "true"],
                    cwd=ROOT, env=environment, check=False,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                    text=True, timeout=10,
                )
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("between 1 and 100", result.stderr)
                self.assertNotIn(sentinel, result.stderr)

        with tempfile.TemporaryDirectory() as temporary:
            binary_dir = Path(temporary)
            scripts = {
                "pgrep": "#!/bin/sh\nexit 1\n",
                "ps": "#!/bin/sh\nprintf '1\\n'\n",
                "readlink": "#!/bin/sh\nexit 1\n",
                "systemd-run": "#!/bin/sh\nprintf '%s\\n' \"$@\"\n",
                "ionice": "#!/bin/sh\nexit 0\n",
                "nice": "#!/bin/sh\nexit 0\n",
                "choom": "#!/bin/sh\nexit 0\n",
            }
            scripts.update(supervisor_fixture_commands())
            for name, body in scripts.items():
                fixture_executable(binary_dir / name, body)
            for mode in ("scope", "service"):
                for quota in ("1", "25", "100", ""):
                    with self.subTest(mode=mode, quota=quota):
                        environment = os.environ.copy()
                        environment.update({
                            "PATH": os.pathsep.join((str(binary_dir), "/usr/bin", "/bin")),
                            "RKC_RESOURCE_GUARD_MODE": mode,
                            "RKC_CPU_QUOTA_PERCENT": quota,
                            "XDG_RUNTIME_DIR": temporary,
                            "DBUS_SESSION_BUS_ADDRESS": "unix:path=/tmp/fixture-bus",
                        })
                        result = subprocess.run(
                            ["/bin/sh", str(ROOT / "scripts/with-rkc-limits.sh"), "true"],
                            cwd=ROOT, env=environment, check=False,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, timeout=10,
                        )
                        self.assertEqual(result.returncode, 0, result.stderr)
                        expected = quota or "100"
                        self.assertIn(f"CPUQuota={expected}%", result.stdout.splitlines())
                        if mode == "service":
                            self.assertIn(f"--setenv=RKC_CPU_QUOTA_PERCENT={expected}", result.stdout.splitlines())

    def test_resource_guard_propagates_priority_contract_to_service(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            binary_dir = Path(temporary)
            scripts = {
                "pgrep": "#!/bin/sh\nexit 1\n",
                "ps": "#!/bin/sh\nprintf '1\\n'\n",
                "readlink": "#!/bin/sh\nexit 1\n",
                "systemd-run": (
                    "#!/bin/sh\n"
                    "case \" $* \" in "
                    "*' --setenv=RKC_HIGHER_PRIORITY_POLICY=yield '*) ;; "
                    "*) exit 91 ;; esac\n"
                    "case \" $* \" in "
                    "*' --setenv=RKC_HIGHER_PRIORITY_MARKERS="
                    "critical_train,batch2 '*) ;; *) exit 92 ;; esac\n"
                    "case \" $* \" in "
                    "*' --setenv=RKC_HIGHER_PRIORITY_LOAD_MAX=0.25 '*) ;; "
                    "*) exit 93 ;; esac\n"
                    "case \" $* \" in "
                    "*' --setenv=RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=1536 '*) ;; "
                    "*) exit 94 ;; esac\n"
                    "exit 0\n"
                ),
            }
            for name in ("ionice", "nice", "choom"):
                scripts[name] = "#!/bin/sh\nexit 0\n"
            scripts.update(supervisor_fixture_commands())
            for name, body in scripts.items():
                fixture_executable(binary_dir / name, body)
            environment = os.environ.copy()
            environment.update(
                {
                    "PATH": os.pathsep.join((str(binary_dir), "/usr/bin", "/bin")),
                    "RKC_RESOURCE_GUARD_MODE": "service",
                    "RKC_HIGHER_PRIORITY_POLICY": "yield",
                    "RKC_HIGHER_PRIORITY_MARKERS": "critical_train,batch2",
                    "RKC_HIGHER_PRIORITY_LOAD_MAX": "0.25",
                    "RKC_HOST_AVAILABLE_MEMORY_MIN_MIB": "1536",
                    "XDG_RUNTIME_DIR": temporary,
                    "DBUS_SESSION_BUS_ADDRESS": "unix:path=/tmp/fixture-bus",
                }
            )
            result = subprocess.run(
                ["/bin/sh", str(ROOT / "scripts/with-rkc-limits.sh"), "true"],
                cwd=ROOT,
                env=environment,
                check=False,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                timeout=10,
            )
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
