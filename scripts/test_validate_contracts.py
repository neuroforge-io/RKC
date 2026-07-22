#!/usr/bin/env python3
"""Hermetic coverage and dependency checks for validate-contracts.py."""
from __future__ import annotations

import hashlib
import importlib.metadata
import io
import json
import runpy
import shutil
import tempfile
import unittest
from contextlib import contextmanager, redirect_stdout
from pathlib import Path
from typing import Callable, Iterator


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "validate-contracts.py"


@contextmanager
def sqlite_contract_fixture() -> Iterator[Path]:
    with tempfile.TemporaryDirectory(prefix=".sqlite-contract-test-", dir=ROOT) as name:
        root = Path(name)
        (root / "storage").mkdir()
        shutil.copytree(ROOT / "storage" / "sqlite", root / "storage" / "sqlite")
        yield root


def rewrite_manifest(root: Path, mutation: Callable[[dict[str, object]], None]) -> str:
    path = root / "storage" / "sqlite" / "migrations" / "manifest.json"
    document = json.loads(path.read_text(encoding="utf-8"))
    mutation(document)
    path.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
    return hashlib.sha256(path.read_bytes()).hexdigest()


class ValidateContractsTests(unittest.TestCase):
    def validator_namespace(self) -> dict[str, object]:
        output = io.StringIO()
        with redirect_stdout(output):
            return runpy.run_path(str(SCRIPT), run_name="rkc_migration_contract_test")

    def test_locked_development_dependencies_are_active(self) -> None:
        expected: dict[str, str] = {}
        for raw in (
            (ROOT / "requirements-dev.txt").read_text(encoding="utf-8").splitlines()
        ):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            name, separator, version = line.partition("==")
            self.assertEqual(
                separator, "==", f"dependency is not exactly locked: {line}"
            )
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
        with tempfile.TemporaryDirectory(
            prefix=".contract-test-", dir=root
        ) as temporary:
            invalid = Path(temporary) / "invalid.json"
            invalid.write_text("{", encoding="utf-8")
            self.assertIsNone(namespace["load_json"](invalid))
        self.assertTrue(any("invalid JSON" in item for item in errors))

    def test_sqlite_migrations_match_the_consolidated_schema(self) -> None:
        namespace = self.validator_namespace()
        validate = namespace["validate_sqlite_migrations"]
        detail = validate()  # type: ignore[operator]
        self.assertEqual(detail["migration_count"], 2)
        self.assertEqual(detail["database_schema_version"], "0.2.0")
        self.assertRegex(detail["manifest_sha256"], r"^[0-9a-f]{64}$")
        self.assertRegex(detail["catalog_sha256"], r"^[0-9a-f]{64}$")

    def test_sqlite_migrations_fail_closed_on_file_and_manifest_drift(self) -> None:
        namespace = self.validator_namespace()
        validate = namespace["validate_sqlite_migrations"]
        error = namespace["MigrationContractError"]

        with sqlite_contract_fixture() as root:
            migration = root / "storage" / "sqlite" / "migrations" / "0001_initial.sql"
            migration.write_text(
                migration.read_text(encoding="utf-8") + "-- unauthorized edit\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(error, "migration digest mismatch"):
                validate(root)  # type: ignore[operator]

        with sqlite_contract_fixture() as root:
            manifest = root / "storage" / "sqlite" / "migrations" / "manifest.json"
            manifest.write_text(
                manifest.read_text(encoding="utf-8").replace(
                    '"schema_version": "1.0"', '"schema_version": "9.0"'
                ),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(error, "manifest digest mismatch"):
                validate(root)  # type: ignore[operator]

        with sqlite_contract_fixture() as root:
            extra = root / "storage" / "sqlite" / "migrations" / "untracked.sql"
            extra.write_text("SELECT 1;\n", encoding="utf-8")
            with self.assertRaisesRegex(error, "directory entries drifted"):
                validate(root)  # type: ignore[operator]

    def test_sqlite_migrations_fail_closed_on_order_and_version_drift(self) -> None:
        namespace = self.validator_namespace()
        validate = namespace["validate_sqlite_migrations"]
        error = namespace["MigrationContractError"]

        with sqlite_contract_fixture() as root:
            expected = rewrite_manifest(
                root,
                lambda document: document["migrations"].reverse(),  # type: ignore[union-attr]
            )
            with self.assertRaisesRegex(error, "contiguous and ordered"):
                validate(root, expected)  # type: ignore[operator]

        with sqlite_contract_fixture() as root:

            def drift_target(document: dict[str, object]) -> None:
                migrations = document["migrations"]
                migrations[0]["target_schema_version"] = "0.1.1"  # type: ignore[index]

            expected = rewrite_manifest(root, drift_target)
            with self.assertRaisesRegex(error, "recorded schema version"):
                validate(root, expected)  # type: ignore[operator]

        with sqlite_contract_fixture() as root:

            def drift_final(document: dict[str, object]) -> None:
                document["database_schema_version"] = "0.3.0"

            expected = rewrite_manifest(root, drift_final)
            with self.assertRaisesRegex(error, "final migration target"):
                validate(root, expected)  # type: ignore[operator]

        with sqlite_contract_fixture() as root:

            def reverse_targets(document: dict[str, object]) -> None:
                migrations = document["migrations"]
                migrations[1]["target_schema_version"] = "0.0.9"  # type: ignore[index]
                document["database_schema_version"] = "0.0.9"

            expected = rewrite_manifest(root, reverse_targets)
            with self.assertRaisesRegex(error, "not forward-only"):
                validate(root, expected)  # type: ignore[operator]

    def test_sqlite_migrations_reject_malformed_contracts(self) -> None:
        namespace = self.validator_namespace()
        validate = namespace["validate_sqlite_migrations"]
        error = namespace["MigrationContractError"]

        def assert_manifest_failure(
            mutation: Callable[[dict[str, object]], None], message: str
        ) -> None:
            with sqlite_contract_fixture() as root:
                expected = rewrite_manifest(root, mutation)
                with self.assertRaisesRegex(error, message):
                    validate(root, expected)  # type: ignore[operator]

        assert_manifest_failure(
            lambda document: document.__setitem__("unexpected", True),
            "manifest keys drifted",
        )
        assert_manifest_failure(
            lambda document: document.__setitem__("schema_version", "2.0"),
            "schema_version must be 1.0",
        )
        assert_manifest_failure(
            lambda document: document.__setitem__("migrations", []),
            "must contain migrations",
        )

        def invalid_migration_keys(document: dict[str, object]) -> None:
            migrations = document["migrations"]
            migrations[0]["unexpected"] = True  # type: ignore[index]

        assert_manifest_failure(invalid_migration_keys, "keys drifted")

        def invalid_name(document: dict[str, object]) -> None:
            migrations = document["migrations"]
            migrations[0]["name"] = "Initial migration"  # type: ignore[index]

        assert_manifest_failure(invalid_name, "invalid name")

        def invalid_minimum(document: dict[str, object]) -> None:
            migrations = document["migrations"]
            migrations[0]["minimum_rkc"] = "latest"  # type: ignore[index]

        assert_manifest_failure(invalid_minimum, "invalid minimum_rkc")

        def invalid_digest(document: dict[str, object]) -> None:
            migrations = document["migrations"]
            migrations[0]["sha256"] = "ABC"  # type: ignore[index]

        assert_manifest_failure(invalid_digest, "invalid sha256")

    def test_sqlite_migrations_reject_sql_and_consolidated_schema_drift(self) -> None:
        namespace = self.validator_namespace()
        validate = namespace["validate_sqlite_migrations"]
        error = namespace["MigrationContractError"]

        with sqlite_contract_fixture() as root:
            path = root / "storage" / "sqlite" / "migrations" / "0001_initial.sql"
            path.write_bytes(path.read_bytes().replace(b"\n", b"\r\n"))
            digest = hashlib.sha256(path.read_bytes()).hexdigest()

            def replace_digest(document: dict[str, object]) -> None:
                migrations = document["migrations"]
                migrations[0]["sha256"] = digest  # type: ignore[index]

            expected = rewrite_manifest(root, replace_digest)
            with self.assertRaisesRegex(error, "not canonical UTF-8/LF"):
                validate(root, expected)  # type: ignore[operator]

        with sqlite_contract_fixture() as root:
            schema = root / "storage" / "sqlite" / "schema.sql"
            schema.write_text(
                schema.read_text(encoding="utf-8")
                + "\nCREATE TABLE unauthorized_drift(value TEXT) STRICT;\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(error, "schema drifted from migrations"):
                validate(root)  # type: ignore[operator]

        with sqlite_contract_fixture() as root:
            path = (
                root
                / "storage"
                / "sqlite"
                / "migrations"
                / "0002_claims_conflicts_paths.sql"
            )
            path.write_text("THIS IS NOT SQL;\n", encoding="utf-8")
            digest = hashlib.sha256(path.read_bytes()).hexdigest()

            def replace_digest(document: dict[str, object]) -> None:
                migrations = document["migrations"]
                migrations[1]["sha256"] = digest  # type: ignore[index]

            expected = rewrite_manifest(root, replace_digest)
            with self.assertRaisesRegex(error, "SQLite migration execution failed"):
                validate(root, expected)  # type: ignore[operator]


if __name__ == "__main__":
    unittest.main()
