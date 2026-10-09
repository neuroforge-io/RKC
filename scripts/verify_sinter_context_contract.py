#!/usr/bin/env python3
"""Verify fictional RKC source references and optionally the pinned consumer.

This does not contact a server, run a model, compile a collection, or inspect
paths carried inside a packet. RKC's CLI, REST and MCP context bodies share
Dataset.BuildContext; consuming a recorded body does not test HTTP GET or its
snapshot response header.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import importlib
import json
import os
from pathlib import Path
import sys
from types import ModuleType
from typing import Any

MAX_RECEIPT_BYTES = 2 * 1024 * 1024
QUESTION = "Lantern lending period"
BEFORE_RULE = "Lantern lending period is 14 days."
AFTER_RULE = "Lantern lending period is 21 days."
AUDITED_ATLAS_SHA256 = (
    "eb28014592eb190f1af5d7c3dbaf0d691aeaf3f2a5e6e2b4058cd03973da0879"
)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def load_receipt(path: Path) -> tuple[dict[str, Any], str]:
    require(path.is_file(), "Choose the generated fictional workflow receipt.")
    require(
        path.stat().st_size <= MAX_RECEIPT_BYTES,
        "Fictional workflow receipt exceeds the bounded input limit.",
    )
    raw = path.read_bytes()
    document = json.loads(raw)
    require(isinstance(document, dict), "Workflow receipt must be an object.")
    require(
        document.get("schema") == "rkc-synthetic-model-workflow/v1"
        and document.get("data") == "wholly fictional Lantern library"
        and document.get("status") == "passed"
        and document.get("remote_requests") == 0,
        "Use the passing, wholly fictional, local-only Lantern receipt.",
    )
    return document, hashlib.sha256(raw).hexdigest()


def load_consumer(source: Path) -> tuple[ModuleType, str]:
    source = source.resolve(strict=True)
    atlas_path = source / "sinter" / "atlas.py"
    require(atlas_path.is_file(), "Sinter source must contain sinter/atlas.py.")
    digest = hashlib.sha256(atlas_path.read_bytes()).hexdigest()
    require(
        digest == AUDITED_ATLAS_SHA256,
        "Sinter atlas source changed; review its pure consumer before rerunning.",
    )
    # Avoid writes to the independently owned Sinter checkout. The audited
    # imports define functions; validate/context do not invoke client settings,
    # credential readers, urllib, subprocess, model generation or atlas paths.
    os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
    sys.dont_write_bytecode = True
    sys.path.insert(0, str(source))
    consumer = importlib.import_module("sinter.atlas")
    require(
        Path(consumer.__file__).resolve() == atlas_path.resolve(),
        "Imported Sinter consumer is outside the explicitly selected source.",
    )
    return consumer, digest


def reject_corruption(consumer: ModuleType, packet: dict[str, Any]) -> dict[str, str]:
    errors: dict[str, str] = {}
    for name in ("validate", "context"):
        try:
            if name == "context":
                consumer.context(packet, QUESTION)
            else:
                consumer.validate(packet)
        except ValueError as error:
            errors[name] = str(error)
        else:
            raise RuntimeError(f"Sinter {name} admitted a corrupt citation packet.")
    return errors


def inspect_document_references(
    packet: dict[str, Any], stage: dict[str, Any], view: dict[str, Any] | None
) -> dict[str, Any]:
    documents = [row for row in packet["items"] if row["object_type"] == "document"]
    artifacts = [row for row in packet["items"] if row["object_type"] == "artifact"]
    require(bool(documents), "Fixture must contain a real document context hit.")
    require(bool(artifacts), "Fixture must contain a real artifact context hit.")
    references = stage["document_references"]
    require(
        len(documents) == 1 and documents[0] == references["item"],
        "Fixture document differs from the inspected MCP references.",
    )
    document = documents[0]
    source = document["source"]
    records = references["resolved_evidence"]
    require(
        source["path"] == "handbook.md"
        and source["start_line"] == 1
        and source["end_line"] == 12
        and "anchor" not in source,
        "Document lost its canonical full-file range.",
    )
    require(
        document["evidence_ids"] == sorted(records) and len(records) == 4,
        "Document must retain the document and three heading evidence IDs.",
    )
    digest = hashlib.sha256(stage["source"].encode()).hexdigest()
    require(
        all(
            record["id"] == identity
            and record["input_digest"] == digest
            and record["source"]["artifact_id"] == source["artifact_id"]
            and record["source"]["path"] == source["path"]
            and 1
            <= record["source"]["start_line"]
            <= record["source"]["end_line"]
            <= 12
            for identity, record in records.items()
        ),
        "Resolved document evidence lost its source/digest ownership.",
    )
    require(
        stage["export_import"]["tampered_export_rejected"] is True,
        "Relocated export must reject tampering.",
    )
    consumer_items = {row["id"]: row for row in view["items"]} if view else None
    return {
        "document_source": source,
        "document_evidence_ids": document["evidence_ids"],
        "resolved_evidence": records,
        "export_import": stage["export_import"],
        "source_dropped_by_audited_consumer": (
            [
                row["object_id"]
                for row in documents + artifacts
                if "source" not in consumer_items[row["citation_id"]]
            ]
            if consumer_items is not None
            else None
        ),
    }


def validate_packet_identity(packet: dict[str, Any]) -> None:
    """Check the existing context citation identity without importing Sinter."""
    require(packet["schema_version"] == "rkc-context/v1", "Wrong context schema.")
    for row in packet["items"]:
        identity = "\x00".join(
            (packet["snapshot_id"], row["object_type"], row["object_id"])
        )
        require(
            row["citation_id"] == hashlib.sha256(identity.encode()).hexdigest(),
            "A context citation does not match its snapshot and object identity.",
        )


def reject_identity(packet: dict[str, Any]) -> dict[str, str]:
    """Reject corrupt fixture identities; this is not a Sinter consumer test."""
    try:
        validate_packet_identity(packet)
    except RuntimeError as error:
        return {"fixture_identity_validation": str(error)}
    raise RuntimeError("Fixture identity validator admitted a corrupt packet.")


def verify(receipt: dict[str, Any], consumer: ModuleType | None) -> dict[str, Any]:
    stages = receipt.get("stages")
    require(
        isinstance(stages, list) and len(stages) == 2,
        "Receipt must contain the fictional before and after stages.",
    )
    before = copy.deepcopy(stages[0]["context"])
    after = copy.deepcopy(stages[1]["context"])
    retained = copy.deepcopy(receipt["old_snapshot_context_after_update"])
    require(
        receipt.get("old_snapshot_retained") is True and retained == before,
        "Receipt must retain the complete original context after the update.",
    )
    original_before = copy.deepcopy(before)
    original_after = copy.deepcopy(after)
    original_retained = copy.deepcopy(retained)
    for packet in (before, after, retained):
        validate_packet_identity(packet)
    before_view = consumer.validate(before) if consumer else None
    after_view = consumer.validate(after) if consumer else None
    before_context = consumer.context(before, QUESTION) if consumer else before
    after_context = consumer.context(after, QUESTION) if consumer else after
    retained_context = consumer.context(retained, QUESTION) if consumer else retained
    require(
        before == original_before
        and after == original_after
        and retained == original_retained,
        "Sinter consumer mutated its imported context packet.",
    )
    require(
        before["snapshot_id"] != after["snapshot_id"],
        "Fictional update did not change the snapshot identity.",
    )
    for context, wanted, unwanted in (
        (before_context, BEFORE_RULE, AFTER_RULE),
        (after_context, AFTER_RULE, BEFORE_RULE),
        (retained_context, BEFORE_RULE, AFTER_RULE),
    ):
        bodies = [row["text"] for row in context["items"]]
        require(
            any(wanted in body for body in bodies)
            and all(unwanted not in body for body in bodies),
            "Consumer selected stale or missing fictional lending text.",
        )
    before_citations = {row["citation_id"] for row in before["items"]}
    after_citations = {row["citation_id"] for row in after["items"]}
    require(
        before_citations.isdisjoint(after_citations),
        "Updated citations were not rebound to the new snapshot.",
    )
    swapped = copy.deepcopy(before)
    swapped["snapshot_id"] = after["snapshot_id"]
    corrupted = copy.deepcopy(before)
    corrupted["items"][0]["citation_id"] = "0" * 64
    rejected = {
        "snapshot_only_swap": (
            reject_corruption(consumer, swapped)
            if consumer
            else reject_identity(swapped)
        ),
        "citation_corruption": (
            reject_corruption(consumer, corrupted)
            if consumer
            else reject_identity(corrupted)
        ),
    }
    return {
        "packets": {"before": before, "after": after, "old_retained": retained},
        "consumer_results": (
            {
                "before": before_context,
                "after": after_context,
                "old_retained": retained_context,
            }
            if consumer
            else None
        ),
        "negative_packets": {
            "snapshot_only_swap": swapped,
            "citation_corruption": corrupted,
        },
        "checks": {
            "fictional_lending_days": [14, 21],
            "old_packet_unchanged": True,
            "old_snapshot_retained": True,
            "snapshot_and_citations_changed": True,
            "corrupt_packets_rejected": rejected,
            "document_source_references": {
                "before": inspect_document_references(before, stages[0], before_view),
                "after": inspect_document_references(after, stages[1], after_view),
            },
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--receipt", required=True, type=Path)
    parser.add_argument(
        "--sinter-source",
        type=Path,
        help="Optionally test the exact audited legacy consumer; omit during independently owned Sinter edits.",
    )
    parser.add_argument("--fixture-out", type=Path)
    args = parser.parse_args()
    receipt, receipt_digest = load_receipt(args.receipt)
    consumer, consumer_digest = (
        load_consumer(args.sinter_source) if args.sinter_source else (None, None)
    )
    result = verify(receipt, consumer)
    report = {
        "schema": "rkc-sinter-context-bridge/v1",
        "status": "passed",
        "data": "wholly fictional Lantern library",
        "receipt_sha256": receipt_digest,
        "sinter_atlas_sha256": consumer_digest,
        "consumer_calls": (
            ["sinter.atlas.validate", "sinter.atlas.context"] if consumer else []
        ),
        "boundaries": [
            "Recorded CLI context uses the same Dataset.BuildContext as REST/MCP.",
            "No HTTP GET, snapshot response header, or MCP transport was tested.",
            "No network, model, credential function, or compilation was invoked.",
            "Producer-only mode validates fixture citation identity; it does not test Sinter.",
            "Retained import does not automatically refresh after a source edit.",
        ],
        "source_references": {
            "sinter_scope": "Line references describe the hash-pinned legacy consumer, not independently changed Sinter code.",
            "sinter_consumer": {"file": "src/sinter/atlas.py", "line": 42},
            "sinter_citation_check": {"file": "src/sinter/atlas.py", "line": 60},
            "sinter_source_drop": {"file": "src/sinter/atlas.py", "line": 72},
            "rkc_shared_builder": {"file": "internal/server/context.go", "line": 83},
            "rkc_source_binding": {
                "file": "internal/server/context_document.go",
                "line": 19,
            },
            "rkc_rest_handler": {"file": "internal/server/server.go", "line": 800},
            "rkc_mcp_builder": {"file": "internal/mcpserver/server.go", "line": 256},
        },
    }
    result = {**report, **result}
    if args.fixture_out is not None:
        args.fixture_out.parent.mkdir(parents=True, exist_ok=True)
        args.fixture_out.write_text(
            json.dumps(result, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
    print(json.dumps({**report, "checks": result["checks"]}, indent=2))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (
        OSError,
        ValueError,
        KeyError,
        TypeError,
        ImportError,
        RuntimeError,
    ) as error:
        print(f"Sinter context contract verification failed: {error}", file=sys.stderr)
        raise SystemExit(1) from error
