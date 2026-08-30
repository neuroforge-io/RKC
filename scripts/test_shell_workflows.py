#!/usr/bin/env python3
"""Static and syntax contract tests for the repository shell workflows.

The workflows intentionally perform release, resource-guard, and smoke
operations that are not safe to execute from a unit-test discovery run. These
checks still prove that every checked-in entry point has a strict shell mode,
is executable by an available interpreter, and remains parseable. The guarded
release/CI jobs provide the execution evidence for the destructive paths.
"""
from __future__ import annotations

import re
import shutil
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SHELL_WORKFLOWS = (
    "install.sh",
    "scripts/benchmark-reference.sh",
    "scripts/build-release-binaries.sh",
    "scripts/check-portable-builds.sh",
    "scripts/generate-demo.sh",
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


if __name__ == "__main__":
    unittest.main()
