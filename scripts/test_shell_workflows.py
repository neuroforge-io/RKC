#!/usr/bin/env python3
"""Static and syntax contract tests for the repository shell workflows.

The workflows intentionally perform release, resource-guard, and smoke
operations that are not safe to execute from a unit-test discovery run. These
checks still prove that every checked-in entry point has a strict shell mode,
is executable by an available interpreter, and remains parseable. The guarded
release/CI jobs provide the execution evidence for the destructive paths.
"""
from __future__ import annotations

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


class ShellWorkflowTests(unittest.TestCase):
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
            for name, body in scripts.items():
                path = binary_dir / name
                path.write_text(body, encoding="utf-8")
                path.chmod(0o700)
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
                    "exit 0\n"
                ),
            }
            for name in ("ionice", "nice", "choom"):
                scripts[name] = "#!/bin/sh\nexit 0\n"
            for name, body in scripts.items():
                path = binary_dir / name
                path.write_text(body, encoding="utf-8")
                path.chmod(0o700)
            environment = os.environ.copy()
            environment.update(
                {
                    "PATH": os.pathsep.join((str(binary_dir), "/usr/bin", "/bin")),
                    "RKC_RESOURCE_GUARD_MODE": "service",
                    "RKC_HIGHER_PRIORITY_POLICY": "yield",
                    "RKC_HIGHER_PRIORITY_MARKERS": "critical_train,batch2",
                    "RKC_HIGHER_PRIORITY_LOAD_MAX": "0.25",
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
