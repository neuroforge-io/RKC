#!/usr/bin/env python3
"""Offline contract validation for the RKC reference release."""
from __future__ import annotations

import json
import re
import sqlite3
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ERRORS: list[str] = []
CHECKS: list[dict[str, object]] = []


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
            registry = registry.with_resource(schema.get("$id", (ROOT / "schemas" / path_name).resolve().as_uri()), resource)
            registry = registry.with_resource((ROOT / "schemas" / path_name).resolve().as_uri(), resource)
            record(f"schema syntax {path_name}", True)
        except Exception as exc:
            record(f"schema syntax {path_name}", False, str(exc))

    def validate(instance, schema_name: str, label: str) -> None:
        try:
            validator = Draft202012Validator(schemas[schema_name], registry=registry)
            errors = sorted(validator.iter_errors(instance), key=lambda err: list(err.absolute_path))
            if errors:
                detail = "; ".join(f"/{'/'.join(map(str, err.absolute_path))}: {err.message}" for err in errors[:20])
                record(label, False, detail)
            else:
                record(label, True)
        except Exception as exc:
            record(label, False, str(exc))

    config = load_json(ROOT / "config" / "rkc.example.json")
    if config is not None:
        validate(config, "config.schema.json", "example configuration")

    for path in sorted((ROOT / "plugins").glob("*/plugin.json")):
        instance = load_json(path)
        if instance is not None:
            validate(instance, "plugin-manifest.schema.json", f"plugin manifest {path.parent.name}")

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
        "artifacts": [], "nodes": [], "edges": [], "evidence": [], "diagnostics": []
    }
    validate(minimal_bundle, "rkc-bundle.schema.json", "minimal canonical bundle")
    minimal_patch = {
        "protocol_version": "1.0", "schema_version": "0.2.0",
        "snapshot_id": "rkc:snapshot:test",
        "producer": {"plugin_id": "rkc.fixture", "version": "1.0.0"},
        "fragment": {}
    }
    validate(minimal_patch, "graph-patch.schema.json", "minimal GraphPatch")

    smoke_bundle = ROOT / ".rkc-smoke" / "bundle.json"
    if smoke_bundle.exists():
        instance = load_json(smoke_bundle)
        if instance is not None:
            validate(instance, "rkc-bundle.schema.json", "smoke canonical bundle")

if yaml is not None:
    for name in ("openapi.yaml", "openapi-service-future.yaml"):
        path = ROOT / "api" / name
        try:
            document = yaml.safe_load(path.read_text(encoding="utf-8"))
            ok = isinstance(document, dict) and str(document.get("openapi", "")).startswith("3.") and isinstance(document.get("paths"), dict)
            record(f"OpenAPI parse {name}", ok, "missing openapi/paths" if not ok else "")
        except Exception as exc:
            record(f"OpenAPI parse {name}", False, str(exc))

    try:
        implemented = yaml.safe_load((ROOT / "api" / "openapi.yaml").read_text(encoding="utf-8"))
        documented_paths = set(implemented["paths"])
        source = (ROOT / "internal" / "server" / "server.go").read_text(encoding="utf-8")
        coded_paths = set(re.findall(r'mux\.HandleFunc\("GET ([^"{]+(?:\{[^}]+\})?)"', source))
        # Go path variables and OpenAPI variables use the same spelling in this project.
        record("implemented OpenAPI route parity", documented_paths == coded_paths,
               f"only documented={sorted(documented_paths-coded_paths)}, only code={sorted(coded_paths-documented_paths)}")
    except Exception as exc:
        record("implemented OpenAPI route parity", False, str(exc))

try:
    connection = sqlite3.connect(":memory:")
    connection.executescript((ROOT / "storage" / "sqlite" / "schema.sql").read_text(encoding="utf-8"))
    version = connection.execute("SELECT value FROM schema_meta WHERE key='schema_version'").fetchone()[0]
    record("SQLite DDL", version == "0.2.0", f"schema_version={version}")
    connection.close()
except Exception as exc:
    record("SQLite DDL", False, str(exc))

try:
    wit = (ROOT / "plugins" / "plugin.wit").read_text(encoding="utf-8")
    record("WIT package revision", "package rkc:extractor@0.2.0;" in wit)
except Exception as exc:
    record("WIT package revision", False, str(exc))

try:
    lock = load_json(ROOT / "plugins" / "plugins.lock.json")
    valid = bool(lock and lock.get("schema_version") == "1.0" and isinstance(lock.get("plugins"), list))
    record("plugin lockfile shape", valid)
except Exception as exc:
    record("plugin lockfile shape", False, str(exc))

result = {"schema_version": "1.0", "ok": not ERRORS, "checks": CHECKS, "errors": ERRORS}
print(json.dumps(result, indent=2, sort_keys=True))
if ERRORS:
    sys.exit(1)
