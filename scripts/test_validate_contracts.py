#!/usr/bin/env python3
"""Hermetic coverage and dependency checks for validate-contracts.py."""
from __future__ import annotations

import importlib.metadata
import io
import json
import runpy
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "validate-contracts.py"


class ValidateContractsTests(unittest.TestCase):
    def test_locked_development_dependencies_are_active(self) -> None:
        expected: dict[str, str] = {}
        for raw in (ROOT / "requirements-dev.txt").read_text(encoding="utf-8").splitlines():
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            name, separator, version = line.partition("==")
            self.assertEqual(separator, "==", f"dependency is not exactly locked: {line}")
            expected[name] = version
        self.assertTrue(expected)
        observed = {name: importlib.metadata.version(name) for name in expected}
        self.assertEqual(observed, expected)

    def test_checked_in_contracts_pass_and_diagnostics_are_structured(self) -> None:
        output = io.StringIO()
        try:
            with redirect_stdout(output):
                namespace = runpy.run_path(
                    str(SCRIPT), run_name="rkc_validate_contracts_test"
                )
        except SystemExit as exc:
            payload = json.loads(output.getvalue())
            self.fail(f"validate-contracts exited {exc.code}: {payload['errors']}")
        payload = json.loads(output.getvalue())
        self.assertTrue(payload["ok"], payload["errors"])
        self.assertGreater(len(payload["checks"]), 20)

        errors = namespace["ERRORS"]
        checks = namespace["CHECKS"]
        errors.clear()
        checks.clear()
        namespace["record"]("fixture", False, "expected failure")
        self.assertEqual(errors, ["fixture: expected failure"])
        self.assertEqual(checks[0]["ok"], False)

        root = namespace["ROOT"]
        with tempfile.TemporaryDirectory(prefix=".contract-test-", dir=root) as temporary:
            invalid = Path(temporary) / "invalid.json"
            invalid.write_text("{", encoding="utf-8")
            self.assertIsNone(namespace["load_json"](invalid))
        self.assertTrue(any("invalid JSON" in item for item in errors))


if __name__ == "__main__":
    unittest.main()
