from __future__ import annotations

import argparse
import importlib.metadata
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

try:
    from scripts import verify_python_environment as ENVIRONMENT
    from scripts.verify_python_environment import (
        PythonEnvironmentError,
        read_lock,
        verify_environment,
    )
except ModuleNotFoundError as exc:
    if exc.name != "scripts":
        raise
    import verify_python_environment as ENVIRONMENT
    from verify_python_environment import (
        PythonEnvironmentError,
        read_lock,
        verify_environment,
    )


class VerifyPythonEnvironmentTests(unittest.TestCase):
    def write_lock(self, content: str) -> Path:
        temporary = tempfile.NamedTemporaryFile("w", encoding="utf-8", delete=False)
        self.addCleanup(Path(temporary.name).unlink, missing_ok=True)
        with temporary:
            temporary.write(content)
        return Path(temporary.name)

    def test_required_versions_pass(self) -> None:
        lock = self.write_lock("# tools\nAlpha==1.2.3\nbeta_pkg==4.5+cpu\n")
        versions = {"Alpha": "1.2.3", "beta_pkg": "4.5+cpu"}
        receipt = verify_environment(
            lock,
            version_lookup=versions.__getitem__,
            python_version=(3, 11, 9),
        )
        self.assertTrue(receipt["ok"])
        self.assertEqual(len(receipt["requirements"]), 2)

    def test_malformed_empty_and_duplicate_locks_fail(self) -> None:
        for content, message in (
            ("\n# only comments\n", "empty"),
            ("alpha>=1\n", "exact"),
            ("alpha==1\nAlpha==1\n", "duplicate"),
        ):
            with self.subTest(content=content):
                with self.assertRaisesRegex(PythonEnvironmentError, message):
                    read_lock(self.write_lock(content))

    def test_version_mismatch_missing_package_and_old_python_fail(self) -> None:
        lock = self.write_lock("alpha==1\nbeta==2\n")

        def lookup(name: str) -> str:
            if name == "alpha":
                return "0"
            raise importlib.metadata.PackageNotFoundError(name)

        with self.assertRaisesRegex(PythonEnvironmentError, "alpha==0.*beta is missing"):
            verify_environment(lock, version_lookup=lookup, python_version=(3, 11, 0))
        with self.assertRaisesRegex(PythonEnvironmentError, "3.11"):
            verify_environment(lock, version_lookup=lookup, python_version=(3, 10, 14))

    def test_lock_and_distribution_inspection_errors_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory(prefix="rkc-missing-lock-") as temporary:
            missing = Path(temporary) / "requirements.lock"
            with self.assertRaisesRegex(PythonEnvironmentError, "cannot read"):
                read_lock(missing)

        invalid_utf8 = self.write_lock("temporary")
        invalid_utf8.write_bytes(b"\xff")
        with self.assertRaisesRegex(PythonEnvironmentError, "cannot read"):
            read_lock(invalid_utf8)

        lock = self.write_lock("alpha==1\nbeta==2\n")

        def lookup(name: str) -> str:
            if name == "alpha":
                raise OSError("metadata unavailable")
            raise ValueError("metadata invalid")

        with self.assertRaisesRegex(
            PythonEnvironmentError,
            "alpha cannot be inspected: metadata unavailable.*"
            "beta cannot be inspected: metadata invalid",
        ):
            verify_environment(lock, version_lookup=lookup, python_version=(3, 11, 0))

    def test_main_emits_receipt_and_translates_verification_failure(self) -> None:
        arguments = argparse.Namespace(requirements=Path("requirements.lock"))
        with mock.patch.object(
            sys,
            "argv",
            ["verify_python_environment.py", "--requirements", "custom.lock"],
        ):
            self.assertEqual(
                ENVIRONMENT.parse_args().requirements, Path("custom.lock")
            )
        receipt = {
            "ok": True,
            "python": "3.11.0",
            "executable": sys.executable,
            "requirements": [],
        }
        with mock.patch.object(
            ENVIRONMENT, "parse_args", return_value=arguments
        ), mock.patch.object(
            ENVIRONMENT, "verify_environment", return_value=receipt
        ) as verify, mock.patch(
            "sys.stdout", new=io.StringIO()
        ) as stdout:
            ENVIRONMENT.main()
        verify.assert_called_once_with(arguments.requirements)
        self.assertEqual(json.loads(stdout.getvalue()), receipt)

        with mock.patch.object(
            ENVIRONMENT, "parse_args", return_value=arguments
        ), mock.patch.object(
            ENVIRONMENT,
            "verify_environment",
            side_effect=PythonEnvironmentError("lock drift"),
        ), self.assertRaisesRegex(
            SystemExit, "verification failed: lock drift"
        ):
            ENVIRONMENT.main()


if __name__ == "__main__":
    unittest.main()
