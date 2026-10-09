"""Offline checks of the recorded source bridge and its fail-closed verifier."""

from __future__ import annotations

import copy
import hashlib
import io
import json
import runpy
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import ModuleType
from unittest import mock

try:
    from scripts import synthetic_model_workflow as workflow
    from scripts import verify_sinter_context_contract as verifier
except ModuleNotFoundError as error:
    if error.name != "scripts":
        raise
    import synthetic_model_workflow as workflow
    import verify_sinter_context_contract as verifier


ROOT = Path(__file__).resolve().parents[1]
BRIDGE = ROOT / "fixtures" / "sinter_context_bridge.json"


def recorded_receipt() -> dict:
    """Reconstruct producer input from the checked-in fictional bridge only."""
    bridge = json.loads(BRIDGE.read_text(encoding="utf-8"))
    stages = []
    for name, days in (("before", 14), ("after", 21)):
        packet = bridge["packets"][name]
        document = next(row for row in packet["items"] if row["object_type"] == "document")
        references = bridge["checks"]["document_source_references"][name]
        stages.append({
            "context": packet,
            "source": workflow.fictional_handbook(days),
            "document_references": {
                "item": document,
                "resolved_evidence": references["resolved_evidence"],
            },
            "export_import": references["export_import"],
        })
    return {
        "schema": "rkc-synthetic-model-workflow/v1",
        "status": "passed",
        "data": "wholly fictional Lantern library",
        "remote_requests": 0,
        "stages": stages,
        "old_snapshot_retained": True,
        "old_snapshot_context_after_update": bridge["packets"]["old_retained"],
    }


def pure_consumer() -> ModuleType:
    """A controlled consumer that enforces identity without any external IO."""
    consumer = ModuleType("fictional_sinter_consumer")

    def validate(packet):
        try:
            verifier.validate_packet_identity(packet)
        except RuntimeError as error:
            raise ValueError(str(error)) from error
        return {"items": [{"id": row["citation_id"], "text": row["text"]}
                          for row in packet["items"]]}

    def context(packet, question):
        if question != verifier.QUESTION:
            raise AssertionError("unexpected consumer question")
        validate(packet)
        return copy.deepcopy(packet)

    consumer.validate = validate
    consumer.context = context
    return consumer


class ReceiptInputTests(unittest.TestCase):
    def test_exact_bounded_fictional_receipt_and_raw_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            raw = json.dumps(recorded_receipt(), ensure_ascii=False).encode()
            path.write_bytes(raw)
            receipt, digest = verifier.load_receipt(path)
            self.assertEqual(digest, hashlib.sha256(raw).hexdigest())
            self.assertEqual(receipt, recorded_receipt())

    def test_receipts_with_wrong_authority_or_bounds_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            with self.assertRaisesRegex(RuntimeError, "Choose the generated"):
                verifier.load_receipt(path)
            path.write_bytes(b" " * (verifier.MAX_RECEIPT_BYTES + 1))
            with self.assertRaisesRegex(RuntimeError, "bounded input"):
                verifier.load_receipt(path)
            path.write_text("[]")
            with self.assertRaisesRegex(RuntimeError, "must be an object"):
                verifier.load_receipt(path)
            path.write_text("{")
            with self.assertRaises(json.JSONDecodeError):
                verifier.load_receipt(path)
            for key, value in (("schema", "other"), ("data", "customer data"),
                               ("status", "failed"), ("remote_requests", 1)):
                with self.subTest(key=key):
                    receipt = recorded_receipt()
                    receipt[key] = value
                    path.write_text(json.dumps(receipt))
                    with self.assertRaisesRegex(RuntimeError, "local-only Lantern"):
                        verifier.load_receipt(path)

    def test_consumer_requires_exact_source_hash_and_import_origin(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            with self.assertRaisesRegex(RuntimeError, "sinter/atlas.py"):
                verifier.load_consumer(source)
            atlas = source / "sinter" / "atlas.py"
            atlas.parent.mkdir()
            atlas.write_text("# Pure, authored test consumer.\n")
            with self.assertRaisesRegex(RuntimeError, "source changed"):
                verifier.load_consumer(source)
            digest = hashlib.sha256(atlas.read_bytes()).hexdigest()
            consumer = pure_consumer()
            consumer.__file__ = str(atlas)
            with mock.patch.object(verifier, "AUDITED_ATLAS_SHA256", digest), \
                    mock.patch.object(verifier.importlib, "import_module", return_value=consumer) as importer, \
                    mock.patch.object(sys, "path", list(sys.path)), \
                    mock.patch.object(sys, "dont_write_bytecode", False), \
                    mock.patch.dict(verifier.os.environ, {}, clear=False):
                loaded, actual_digest = verifier.load_consumer(source)
                self.assertIs(loaded, consumer)
                self.assertEqual(actual_digest, digest)
                self.assertEqual(sys.path[0], str(source))
                self.assertTrue(sys.dont_write_bytecode)
                self.assertEqual(verifier.os.environ["PYTHONDONTWRITEBYTECODE"], "1")
                importer.assert_called_once_with("sinter.atlas")
                consumer.__file__ = str(source / "elsewhere.py")
                with self.assertRaisesRegex(RuntimeError, "outside the explicitly selected"):
                    verifier.load_consumer(source)


class BridgeIdentityTests(unittest.TestCase):
    def test_recorded_packets_are_preserved_and_updates_rebind_citations(self):
        receipt = recorded_receipt()
        original = copy.deepcopy(receipt)
        result = verifier.verify(receipt, None)
        self.assertEqual(receipt, original)
        self.assertIsNone(result["consumer_results"])
        self.assertEqual(result["packets"]["before"], original["stages"][0]["context"])
        self.assertEqual(result["packets"]["old_retained"], result["packets"]["before"])
        self.assertTrue(result["checks"]["snapshot_and_citations_changed"])
        self.assertIsNone(result["checks"]["document_source_references"]["before"]
                          ["source_dropped_by_audited_consumer"])
        for errors in result["checks"]["corrupt_packets_rejected"].values():
            self.assertIn("fixture_identity_validation", errors)

    def test_controlled_consumer_checks_both_operations_and_records_dropped_sources(self):
        receipt = recorded_receipt()
        result = verifier.verify(receipt, pure_consumer())
        self.assertEqual(result["consumer_results"]["before"], receipt["stages"][0]["context"])
        for errors in result["checks"]["corrupt_packets_rejected"].values():
            self.assertEqual(set(errors), {"validate", "context"})
        self.assertEqual(len(result["checks"]["document_source_references"]["before"]
                             ["source_dropped_by_audited_consumer"]), 2)

    def test_identity_validator_never_admits_wrong_schema_or_corrupt_citations(self):
        packet = recorded_receipt()["stages"][0]["context"]
        for change in ("schema", "citation", "snapshot"):
            value = copy.deepcopy(packet)
            if change == "schema":
                value["schema_version"] = "other"
            elif change == "citation":
                value["items"][0]["citation_id"] = "0" * 64
            else:
                value["snapshot_id"] = "other"
            with self.subTest(change=change):
                self.assertIn("fixture_identity_validation", verifier.reject_identity(value))
        with self.assertRaisesRegex(RuntimeError, "admitted a corrupt packet"):
            verifier.reject_identity(packet)

    def test_corruption_must_be_rejected_by_validate_and_context(self):
        for accepted in ("validate", "context"):
            consumer = pure_consumer()
            setattr(consumer, accepted, lambda *args: {})
            packet = recorded_receipt()["stages"][0]["context"]
            packet["items"][0]["citation_id"] = "bad"
            with self.subTest(accepted=accepted), \
                    self.assertRaisesRegex(RuntimeError, f"Sinter {accepted} admitted"):
                verifier.reject_corruption(consumer, packet)

    def test_receipt_incompleteness_stale_retention_and_consumer_mutation_fail(self):
        receipt = recorded_receipt()
        for change in ("stages", "retention", "same_snapshot", "stale_text"):
            value = copy.deepcopy(receipt)
            if change == "stages":
                value["stages"] = []
            elif change == "retention":
                value["old_snapshot_retained"] = False
            elif change == "same_snapshot":
                value["stages"][1] = copy.deepcopy(value["stages"][0])
            else:
                value["stages"][1]["context"]["items"][0]["text"] = verifier.BEFORE_RULE
            with self.subTest(change=change), self.assertRaises(RuntimeError):
                verifier.verify(value, None)
        consumer = pure_consumer()
        original_validate = consumer.validate

        def mutate(packet):
            view = original_validate(packet)
            packet["unexpected_mutation"] = True
            return view

        consumer.validate = mutate
        with self.assertRaisesRegex(RuntimeError, "mutated its imported context"):
            verifier.verify(receipt, consumer)

    def test_document_ranges_owner_digests_and_tamper_rejection_are_required(self):
        receipt = recorded_receipt()
        for change in ("document", "artifact", "references", "range", "ids", "digest", "tamper"):
            stage = copy.deepcopy(receipt["stages"][0])
            packet = stage["context"]
            document = next(row for row in packet["items"] if row["object_type"] == "document")
            refs = stage["document_references"]
            if change in ("document", "artifact"):
                packet["items"] = [row for row in packet["items"] if row["object_type"] != change]
            elif change == "references":
                refs["item"] = {}
            elif change == "range":
                document["source"]["anchor"] = "not-a-full-file"
            elif change == "ids":
                document["evidence_ids"] = []
            elif change == "digest":
                next(iter(refs["resolved_evidence"].values()))["input_digest"] = "bad"
            else:
                stage["export_import"]["tampered_export_rejected"] = False
            with self.subTest(change=change), self.assertRaises(RuntimeError):
                verifier.inspect_document_references(packet, stage, None)


class BridgeCommandTests(unittest.TestCase):
    def test_main_writes_a_complete_fixture_and_truthful_producer_only_report(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            receipt = root / "receipt.json"
            receipt.write_text(json.dumps(recorded_receipt()))
            output = root / "nested" / "bridge.json"
            stream = io.StringIO()
            with mock.patch.object(sys, "argv", ["verify", "--receipt", str(receipt),
                                                "--fixture-out", str(output)]), redirect_stdout(stream):
                self.assertEqual(verifier.main(), 0)
            report = json.loads(stream.getvalue())
            fixture = json.loads(output.read_text())
            self.assertEqual(report["consumer_calls"], [])
            self.assertIsNone(report["sinter_atlas_sha256"])
            self.assertEqual(report["checks"], fixture["checks"])
            self.assertEqual(fixture["packets"]["before"], recorded_receipt()["stages"][0]["context"])
            with mock.patch.object(sys, "argv", ["verify", "--receipt", str(receipt),
                                                "--sinter-source", str(root)]), \
                    mock.patch.object(verifier, "load_consumer", return_value=(pure_consumer(), "pinned-test-digest")), \
                    redirect_stdout(io.StringIO()) as consumer_output:
                self.assertEqual(verifier.main(), 0)
            self.assertEqual(json.loads(consumer_output.getvalue())["consumer_calls"],
                             ["sinter.atlas.validate", "sinter.atlas.context"])

    def test_entrypoint_reports_invalid_receipts_with_nonzero_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "missing.json"
            errors = io.StringIO()
            with mock.patch.object(sys, "argv", ["verify", "--receipt", str(path)]), \
                    redirect_stderr(errors), self.assertRaises(SystemExit) as exit_status:
                runpy.run_path(str(ROOT / "scripts" / "verify_sinter_context_contract.py"), run_name="__main__")
            self.assertEqual(exit_status.exception.code, 1)
            self.assertIn("Sinter context contract verification failed", errors.getvalue())


if __name__ == "__main__":
    unittest.main()
