from __future__ import annotations

import importlib.metadata
import tempfile
import unittest
from pathlib import Path

try:
    from scripts.verify_python_environment import (
        PythonEnvironmentError,
        read_lock,
        verify_environment,
    )
except ModuleNotFoundError as exc:
    if exc.name != "scripts":
        raise
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


if __name__ == "__main__":
    unittest.main()
