#!/usr/bin/env python3
"""Offline contract validation for the RKC reference release."""
from __future__ import annotations

import hashlib
import json
import re
import sqlite3
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ERRORS: list[str] = []
CHECKS: list[dict[str, object]] = []
SAFE_DEFAULT_EXCLUSIONS = frozenset(
    {
        ".cache",
        ".coverage",
        ".git",
        ".mypy_cache",
        ".pytest_cache",
        ".rkc",
        ".rkc-coverage",
        ".rkc-downloads",
        ".rkc-models",
        ".rkc-runtime",
        ".rkc-state",
        ".rkc.rkc-derived",
        ".ruff_cache",
        ".venv",
        "__pycache__",
        "bin",
        "coverage",
        "coverage.out",
        "coverage.xml",
        "dist",
        "htmlcov",
        "venv",
    }
)
SELF_BENCHMARK_EXCLUSIONS = SAFE_DEFAULT_EXCLUSIONS | frozenset(
    {
        ".rkc-smoke",
        ".rkc-smoke.rkc-derived",
        ".rkc-state-smoke",
        "plugins/python-ast/__pycache__",
        "scripts/__pycache__",
    }
)
SQLITE_MIGRATION_MANIFEST_SHA256 = (
    "d5bbcec6025773be9f8dfe23da4b43e3a0dbf1092fdfc6b51ad0911e7910ffb4"
)
SQLITE_MIGRATION_MANIFEST_KEYS = frozenset(
    {"schema_version", "database_schema_version", "migrations"}
)
SQLITE_MIGRATION_KEYS = frozenset(
    {
        "version",
        "name",
        "target_schema_version",
        "sha256",
        "minimum_rkc",
    }
)
SQLITE_MIGRATION_NAME = re.compile(r"[a-z][a-z0-9]*(?:_[a-z0-9]+)*")
SQLITE_SCHEMA_VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
RKC_VERSION = re.compile(
    r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?"
)


class MigrationContractError(RuntimeError):
    """A checked-in SQLite migration contract is unsafe or inconsistent."""


def migration_require(condition: bool, detail: str) -> None:
    if not condition:
        raise MigrationContractError(detail)


def strict_json_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise MigrationContractError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def schema_version_tuple(value: object, label: str) -> tuple[int, int, int]:
    migration_require(isinstance(value, str), f"{label} must be a string")
    match = SQLITE_SCHEMA_VERSION.fullmatch(value)
    migration_require(match is not None, f"{label} is not a canonical version: {value}")
    assert match is not None
    return tuple(int(part) for part in match.groups())


def sqlite_catalog(connection: sqlite3.Connection) -> list[tuple[str, str, str, str]]:
    return connection.execute(
        """
        SELECT type, name, tbl_name, sql
        FROM sqlite_schema
        WHERE name NOT LIKE 'sqlite_%'
        ORDER BY type, name, tbl_name, sql
        """
    ).fetchall()


def validate_sqlite_migrations(
    root: Path = ROOT,
    expected_manifest_sha256: str = SQLITE_MIGRATION_MANIFEST_SHA256,
) -> dict[str, object]:
    """Validate immutable ordered migrations against their consolidated schema."""
    migration_root = root / "storage" / "sqlite" / "migrations"
    manifest_path = migration_root / "manifest.json"
    migration_require(
        migration_root.is_dir() and not migration_root.is_symlink(),
        "migration root must be a real directory",
    )
    migration_require(
        manifest_path.is_file() and not manifest_path.is_symlink(),
        "migration manifest must be a real file",
    )

    manifest_bytes = manifest_path.read_bytes()
    manifest_sha256 = hashlib.sha256(manifest_bytes).hexdigest()
    migration_require(
        manifest_sha256 == expected_manifest_sha256,
        "migration manifest digest mismatch: "
        f"expected {expected_manifest_sha256}, observed {manifest_sha256}",
    )
    try:
        manifest = json.loads(
            manifest_bytes.decode("utf-8"), object_pairs_hook=strict_json_object
        )
    except MigrationContractError:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise MigrationContractError(
            f"migration manifest is not strict UTF-8 JSON: {exc}"
        ) from exc

    migration_require(
        isinstance(manifest, dict), "migration manifest must be an object"
    )
    migration_require(
        set(manifest) == SQLITE_MIGRATION_MANIFEST_KEYS,
        "migration manifest keys drifted: "
        f"expected {sorted(SQLITE_MIGRATION_MANIFEST_KEYS)}, "
        f"observed {sorted(manifest)}",
    )
    migration_require(
        manifest["schema_version"] == "1.0",
        "migration manifest schema_version must be 1.0",
    )
    final_schema_version = schema_version_tuple(
        manifest["database_schema_version"], "database_schema_version"
    )
    migrations = manifest["migrations"]
    migration_require(
        isinstance(migrations, list) and bool(migrations),
        "migration manifest must contain migrations",
    )

    planned: list[tuple[Path, str, str]] = []
    expected_entries = {"manifest.json"}
    prior_schema_version: tuple[int, int, int] | None = None
    for position, migration in enumerate(migrations, start=1):
        migration_require(
            isinstance(migration, dict), f"migration {position} must be an object"
        )
        migration_require(
            set(migration) == SQLITE_MIGRATION_KEYS,
            f"migration {position} keys drifted",
        )
        version = migration["version"]
        migration_require(
            type(version) is int and version == position,
            f"migration versions must be contiguous and ordered; expected {position}, "
            f"observed {version}",
        )
        name = migration["name"]
        migration_require(
            isinstance(name, str) and SQLITE_MIGRATION_NAME.fullmatch(name) is not None,
            f"migration {position} has an invalid name: {name}",
        )
        target = migration["target_schema_version"]
        target_tuple = schema_version_tuple(
            target, f"migration {position} target_schema_version"
        )
        migration_require(
            prior_schema_version is None or target_tuple > prior_schema_version,
            f"migration {position} target_schema_version is not forward-only",
        )
        prior_schema_version = target_tuple
        minimum_rkc = migration["minimum_rkc"]
        migration_require(
            isinstance(minimum_rkc, str)
            and RKC_VERSION.fullmatch(minimum_rkc) is not None,
            f"migration {position} has an invalid minimum_rkc: {minimum_rkc}",
        )
        digest = migration["sha256"]
        migration_require(
            isinstance(digest, str)
            and re.fullmatch(r"[0-9a-f]{64}", digest) is not None,
            f"migration {position} has an invalid sha256",
        )
        filename = f"{version:04d}_{name}.sql"
        expected_entries.add(filename)
        planned.append((migration_root / filename, digest, target))

    migration_require(
        prior_schema_version == final_schema_version,
        "database_schema_version does not match the final migration target",
    )
    observed_entries = {path.name for path in migration_root.iterdir()}
    migration_require(
        observed_entries == expected_entries,
        "migration directory entries drifted: "
        f"expected {sorted(expected_entries)}, observed {sorted(observed_entries)}",
    )

    payloads: list[tuple[str, str]] = []
    for path, expected_sha256, target in planned:
        migration_require(
            path.is_file() and not path.is_symlink(),
            f"migration must be a real file: {path.name}",
        )
        payload = path.read_bytes()
        observed_sha256 = hashlib.sha256(payload).hexdigest()
        migration_require(
            observed_sha256 == expected_sha256,
            f"migration digest mismatch for {path.name}: "
            f"expected {expected_sha256}, observed {observed_sha256}",
        )
        try:
            sql = payload.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise MigrationContractError(
                f"migration is not UTF-8: {path.name}: {exc}"
            ) from exc
        migration_require(
            sql.endswith("\n") and "\r" not in sql and not sql.startswith("\ufeff"),
            f"migration text is not canonical UTF-8/LF: {path.name}",
        )
        payloads.append((sql, target))

    migrated = sqlite3.connect(":memory:")
    consolidated = sqlite3.connect(":memory:")
    try:
        try:
            for position, (sql, target) in enumerate(payloads, start=1):
                migrated.executescript(sql)
                migration_require(
                    not migrated.in_transaction,
                    f"migration {position} did not close its transaction",
                )
                row = migrated.execute(
                    "SELECT value FROM schema_meta WHERE key = 'schema_version'"
                ).fetchone()
                migration_require(
                    row == (target,),
                    f"migration {position} recorded schema version {row}, expected {target}",
                )
        except sqlite3.Error as exc:
            raise MigrationContractError(
                f"SQLite migration execution failed: {exc}"
            ) from exc

        integrity = migrated.execute("PRAGMA integrity_check").fetchone()
        migration_require(
            integrity == ("ok",), f"migration integrity check failed: {integrity}"
        )
        foreign_key_failures = migrated.execute("PRAGMA foreign_key_check").fetchall()
        migration_require(
            not foreign_key_failures,
            f"migration foreign-key check failed: {foreign_key_failures[:10]}",
        )

        consolidated.executescript(
            (root / "storage" / "sqlite" / "schema.sql").read_text(encoding="utf-8")
        )
        consolidated_version = consolidated.execute(
            "SELECT value FROM schema_meta WHERE key = 'schema_version'"
        ).fetchone()
        migration_require(
            consolidated_version == (manifest["database_schema_version"],),
            "consolidated schema version drifted from the migration manifest",
        )
        migrated_catalog = sqlite_catalog(migrated)
        consolidated_catalog = sqlite_catalog(consolidated)
        migrated_digest = hashlib.sha256(
            json.dumps(migrated_catalog, separators=(",", ":")).encode("utf-8")
        ).hexdigest()
        consolidated_digest = hashlib.sha256(
            json.dumps(consolidated_catalog, separators=(",", ":")).encode("utf-8")
        ).hexdigest()
        migration_require(
            migrated_catalog == consolidated_catalog,
            "consolidated SQLite schema drifted from migrations: "
            f"migration catalog {migrated_digest}, consolidated catalog {consolidated_digest}",
        )
    finally:
        migrated.close()
        consolidated.close()

    return {
        "manifest_sha256": manifest_sha256,
        "migration_count": len(planned),
        "database_schema_version": manifest["database_schema_version"],
        "catalog_sha256": migrated_digest,
    }


def record(name: str, ok: bool, detail: str = "") -> None:
    CHECKS.append({"name": name, "ok": ok, "detail": detail})
    if not ok:
        ERRORS.append(f"{name}: {detail}")


def load_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:  # pragma: no cover - release diagnostic
        record(str(path.relative_to(ROOT)), False, f"invalid JSON: {exc}")
        return None


try:
    import yaml  # type: ignore
except Exception as exc:  # pragma: no cover
    yaml = None
    record("PyYAML availability", False, str(exc))

try:
    from jsonschema import Draft202012Validator
    from referencing import Registry, Resource
except Exception as exc:  # pragma: no cover
    Draft202012Validator = None
    Registry = Resource = None
    record("jsonschema availability", False, str(exc))

schemas: dict[str, dict] = {}
for path in sorted((ROOT / "schemas").glob("*.json")):
    document = load_json(path)
    if document is not None:
        schemas[path.name] = document

if Draft202012Validator is not None:
    registry = Registry()
    for path_name, schema in schemas.items():
        try:
            Draft202012Validator.check_schema(schema)
            resource = Resource.from_contents(schema)
            registry = registry.with_resource(
                schema.get("$id", (ROOT / "schemas" / path_name).resolve().as_uri()),
                resource,
            )
            registry = registry.with_resource(
                (ROOT / "schemas" / path_name).resolve().as_uri(), resource
            )
            record(f"schema syntax {path_name}", True)
        except Exception as exc:
            record(f"schema syntax {path_name}", False, str(exc))

    def validate(instance, schema_name: str, label: str) -> None:
        try:
            validator = Draft202012Validator(schemas[schema_name], registry=registry)
            errors = sorted(
                validator.iter_errors(instance), key=lambda err: list(err.absolute_path)
            )
            if errors:
                detail = "; ".join(
                    f"/{'/'.join(map(str, err.absolute_path))}: {err.message}"
                    for err in errors[:20]
                )
                record(label, False, detail)
            else:
                record(label, True)
        except Exception as exc:
            record(label, False, str(exc))

    config = load_json(ROOT / "config" / "rkc.example.json")
    if config is not None:
        validate(config, "config.schema.json", "example configuration")
        inventory = config.get("inventory", {})
        exclusions = inventory.get("exclude", []) if isinstance(inventory, dict) else []
        exclusion_set = (
            {value for value in exclusions if isinstance(value, str)}
            if isinstance(exclusions, list)
            else set()
        )
        missing = sorted(SAFE_DEFAULT_EXCLUSIONS - exclusion_set)
        fake_globs = sorted(
            value
            for value in exclusion_set
            if isinstance(value, str) and any(character in value for character in "*?[")
        )
        record(
            "example explicit safe exclusions",
            not missing and not fake_globs,
            f"missing={missing}, unsupported_glob_paths={fake_globs}",
        )
        schema_inventory = schemas["config.schema.json"]["properties"]["inventory"]
        record(
            "Git-ignore toggle is not advertised",
            "include_git_ignored" not in inventory
            and "include_git_ignored" not in schema_inventory.get("properties", {})
            and "include_git_ignored" not in schema_inventory.get("required", []),
            "inventory.include_git_ignored is not implemented",
        )

    model_lock = load_json(ROOT / "models" / "models.lock.json")
    if model_lock is not None:
        validate(model_lock, "model-lock.schema.json", "model supply-chain lock")

    for path in sorted((ROOT / "models" / "qualification").glob("*.json")):
        qualification = load_json(path)
        if qualification is not None:
            validate(
                qualification,
                "model-qualification.schema.json",
                f"model qualification {path.name}",
            )

    benchmark_source = (ROOT / "scripts" / "benchmark-reference.sh").read_text(
        encoding="utf-8"
    )
    benchmark_exclusions = set(
        re.findall(r"--exclude[ \t]+([^\s\\]+)", benchmark_source)
    )
    benchmark_missing = sorted(SELF_BENCHMARK_EXCLUSIONS - benchmark_exclusions)
    benchmark_fake_globs = sorted(
        value
        for value in benchmark_exclusions
        if any(character in value for character in "*?[")
    )
    record(
        "self-benchmark explicit safe exclusions",
        not benchmark_missing and not benchmark_fake_globs,
        f"missing={benchmark_missing}, unsupported_glob_paths={benchmark_fake_globs}",
    )

    for path in sorted((ROOT / "plugins").glob("*/plugin.json")):
        instance = load_json(path)
        if instance is not None:
            validate(
                instance,
                "plugin-manifest.schema.json",
                f"plugin manifest {path.parent.name}",
            )

    minimal_bundle = {
        "snapshot": {
            "schema_version": "0.2.0",
            "id": "rkc:snapshot:test",
            "created_at": "2026-07-21T00:00:00Z",
            "status": "committed",
            "root_name": "fixture",
            "root_path": "/fixture",
            "content_digest": "0" * 64,
            "git": {"unavailable": True},
            "tool": {"name": "rkc", "version": "0.3.0-reference"},
        },
        "artifacts": [],
        "nodes": [],
        "edges": [],
        "evidence": [],
        "diagnostics": [],
    }
    validate(minimal_bundle, "rkc-bundle.schema.json", "minimal canonical bundle")
    minimal_patch = {
        "protocol_version": "1.0",
        "schema_version": "0.2.0",
        "snapshot_id": "rkc:snapshot:test",
        "producer": {"plugin_id": "rkc.fixture", "version": "1.0.0"},
        "fragment": {},
    }
    validate(minimal_patch, "graph-patch.schema.json", "minimal GraphPatch")

if yaml is not None:
    for name in ("openapi.yaml", "openapi-service-future.yaml"):
        path = ROOT / "api" / name
        try:
            document = yaml.safe_load(path.read_text(encoding="utf-8"))
            ok = (
                isinstance(document, dict)
                and str(document.get("openapi", "")).startswith("3.")
                and isinstance(document.get("paths"), dict)
            )
            record(
                f"OpenAPI parse {name}", ok, "missing openapi/paths" if not ok else ""
            )
        except Exception as exc:
            record(f"OpenAPI parse {name}", False, str(exc))

    try:
        implemented = yaml.safe_load(
            (ROOT / "api" / "openapi.yaml").read_text(encoding="utf-8")
        )
        documented_paths = set(implemented["paths"])
        source = (ROOT / "internal" / "server" / "server.go").read_text(
            encoding="utf-8"
        )
        coded_paths = set(
            re.findall(r'mux\.HandleFunc\("GET ([^"{]+(?:\{[^}]+\})?)"', source)
        )
        # Go path variables and OpenAPI variables use the same spelling in this project.
        record(
            "implemented OpenAPI route parity",
            documented_paths == coded_paths,
            f"only documented={sorted(documented_paths-coded_paths)}, only code={sorted(coded_paths-documented_paths)}",
        )
    except Exception as exc:
        record("implemented OpenAPI route parity", False, str(exc))

try:
    connection = sqlite3.connect(":memory:")
    connection.executescript(
        (ROOT / "storage" / "sqlite" / "schema.sql").read_text(encoding="utf-8")
    )
    version = connection.execute(
        "SELECT value FROM schema_meta WHERE key='schema_version'"
    ).fetchone()[0]
    record("SQLite DDL", version == "0.2.0", f"schema_version={version}")
    connection.close()
except Exception as exc:
    record("SQLite DDL", False, str(exc))

try:
    migration_detail = validate_sqlite_migrations()
    record(
        "SQLite immutable migrations",
        True,
        json.dumps(migration_detail, sort_keys=True, separators=(",", ":")),
    )
except Exception as exc:
    record("SQLite immutable migrations", False, str(exc))

try:
    wit = (ROOT / "plugins" / "plugin.wit").read_text(encoding="utf-8")
    record("WIT package revision", "package rkc:extractor@0.2.0;" in wit)
except Exception as exc:
    record("WIT package revision", False, str(exc))

try:
    lock = load_json(ROOT / "plugins" / "plugins.lock.json")
    valid = bool(
        lock
        and lock.get("schema_version") == "1.0"
        and isinstance(lock.get("plugins"), list)
    )
    record("plugin lockfile shape", valid)
except Exception as exc:
    record("plugin lockfile shape", False, str(exc))

result = {"schema_version": "1.0", "ok": not ERRORS, "checks": CHECKS, "errors": ERRORS}
print(json.dumps(result, indent=2, sort_keys=True))
if ERRORS:
    sys.exit(1)
