"""Bounded local fixtures for the synthetic CLI/MCP receipt driver.

These tests validate the driver and its rejection paths. Fake CLI results and
mock model replies are not evidence of compiler or real-model quality.
"""

from __future__ import annotations

import copy
import hashlib
import io
import json
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

try:
    from scripts import synthetic_model_workflow as workflow
except ModuleNotFoundError as error:
    if error.name != "scripts":
        raise
    import synthetic_model_workflow as workflow


ROOT = Path(__file__).resolve().parents[1]


def bridge_fixture() -> dict:
    return json.loads((ROOT / "fixtures" / "sinter_context_bridge.json").read_text())


def fixture_stage(atlas: Path) -> tuple[dict, dict]:
    fixture = bridge_fixture()
    name = "before" if "v1" in atlas.name else "after"
    return fixture["packets"][name], fixture["checks"]["document_source_references"][name]


def model_reply(handler_type, payload: dict, *, path="/v1/chat/completions", auth=None,
                content_length=None):
    """Run the actual HTTP handler against in-memory request and response IO."""
    handler = handler_type.__new__(handler_type)
    raw = json.dumps(payload).encode()
    handler.path = path
    handler.headers = {"Content-Length": str(len(raw) if content_length is None else content_length)}
    if auth is not None:
        handler.headers["Authorization"] = auth
    handler.rfile = io.BytesIO(raw)
    handler.wfile = io.BytesIO()
    handler.send_error = mock.Mock()
    handler.send_response = mock.Mock()
    handler.send_header = mock.Mock()
    handler.end_headers = mock.Mock()
    handler.do_POST()
    return handler, json.loads(handler.wfile.getvalue()) if handler.wfile.getvalue() else None


def structured_request(excerpts=None) -> dict:
    packet = {"snapshot_id": "fictional-snapshot", "source_excerpts": excerpts or []}
    prompt = "Task\nBEGIN_UNTRUSTED_REPOSITORY_DATA\n" + json.dumps(packet) + "\nEND_UNTRUSTED_REPOSITORY_DATA"
    return {"model": "fictional-mock", "messages": [{"role": "user", "content": prompt}]}


def native_request(candidates=None) -> dict:
    packet = {"candidates": candidates or [{"text": "Lantern lending period is 14 days.",
                                          "evidence_id": "evidence-1"}]}
    return {"model": "fictional-mock", "messages": [{"role": "user", "content":
            "RKC_EXTRACTIVE_PROTOCOL 1\n" + json.dumps(packet)}], "max_tokens": 128,
            "stream": False, "n": 1}


class MockModelTests(unittest.TestCase):
    def setUp(self):
        workflow.FictionalModel.calls = []
        workflow.FictionalNativeModel.calls = []

    def test_structured_model_quotes_one_admitted_sentence_and_records_exact_prompt(self):
        payload = structured_request([
            {"text": "No lending rule here.", "evidence_id": "irrelevant"},
            {"text": "Lantern lending period is 14 days.", "evidence_id": "canonical"},
            {"text": "Lantern lending period is 99 days.", "evidence_id": "later"},
        ])
        handler, reply = model_reply(workflow.FictionalModel, payload)
        handler.log_message("discarded", "request")
        handler.send_response.assert_called_once_with(200)
        claim = json.loads(reply["choices"][0]["message"]["content"])["claims"][0]
        self.assertEqual(claim["text"], "Lantern lending period is 14 days.")
        self.assertEqual(claim["evidence_ids"], ["canonical"])
        audit = workflow.FictionalModel.calls[0]
        prompt = payload["messages"][0]["content"]
        self.assertEqual(audit["prompt_digest"], "sha256:" + hashlib.sha256(prompt.encode()).hexdigest())
        self.assertEqual(audit["prompt_bytes"], len(prompt.encode()))
        self.assertEqual(audit["snapshot_id"], "fictional-snapshot")

    def test_structured_model_reports_missing_rule_without_inventing_a_claim(self):
        _, reply = model_reply(workflow.FictionalModel, structured_request())
        content = json.loads(reply["choices"][0]["message"]["content"])
        self.assertEqual(content["claims"], [])
        self.assertEqual(content["unresolved_questions"], ["Missing lending period."])
        self.assertIsNone(workflow.FictionalModel.calls[0]["claim"])

    def test_both_mock_profiles_reject_authentication_wrong_routes_and_unbounded_bodies(self):
        for handler_type in (workflow.FictionalModel, workflow.FictionalNativeModel):
            for changes, status in (({"auth": "must-not-be-read"}, 400),
                                    ({"path": "/other"}, 400),
                                    ({"content_length": 0}, 413),
                                    ({"content_length": 512 * 1024 + 1}, 413)):
                with self.subTest(profile=handler_type.__name__, changes=changes):
                    handler, reply = model_reply(handler_type, {}, **changes)
                    handler.send_error.assert_called_once_with(status)
                    self.assertIsNone(reply)
        self.assertEqual(workflow.FictionalModel.calls, [])
        self.assertEqual(workflow.FictionalNativeModel.calls, [])

    def test_native_model_admits_only_the_restricted_plaintext_contract(self):
        handler, reply = model_reply(workflow.FictionalNativeModel, native_request())
        handler.send_response.assert_called_once_with(200)
        self.assertEqual(reply["choices"][0]["message"]["content"], "Lantern lending period is 14 days.")
        self.assertEqual(workflow.FictionalNativeModel.calls[0]["evidence_id"], "evidence-1")
        self.assertEqual(workflow.FictionalNativeModel.calls[0]["request_keys"],
                         ["max_tokens", "messages", "model", "n", "stream"])
        for change in ("schema", "message_count", "role", "output", "stream", "n",
                       "protocol", "prompt_size", "missing_candidate", "ambiguous_candidate"):
            payload = native_request()
            if change == "schema":
                payload["temperature"] = 0
            elif change == "message_count":
                payload["messages"] = []
            elif change == "role":
                payload["messages"][0]["role"] = "system"
            elif change == "output":
                payload["max_tokens"] = 129
            elif change == "stream":
                payload["stream"] = True
            elif change == "n":
                payload["n"] = 2
            elif change == "protocol":
                payload["messages"][0]["content"] = "{}​"
            elif change == "prompt_size":
                payload["messages"][0]["content"] += "x" * 2049
            elif change == "missing_candidate":
                payload = native_request([{"text": "Lantern opens on Tuesday.", "evidence_id": "other"}])
            else:
                payload = native_request([{"text": "Lantern lending period is 14 days.", "evidence_id": "a"},
                                          {"text": "Lantern lending period is 21 days.", "evidence_id": "b"}])
            with self.subTest(change=change):
                handler, reply = model_reply(workflow.FictionalNativeModel, payload)
                handler.send_error.assert_called_once_with(400)
                self.assertIsNone(reply)


class CLIAndDocumentTests(unittest.TestCase):
    def test_run_preserves_argument_boundaries_timeouts_and_nonzero_failures(self):
        with mock.patch.object(workflow.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout="receipt", stderr="")) as command, \
                mock.patch.object(workflow.time, "monotonic", side_effect=[10, 10.125]):
            output, elapsed = workflow.run(Path("/fake/rkc"), ["query", "$(touch nothing)"], Path("/fake/workspace"))
        self.assertEqual((output, elapsed), ("receipt", 125))
        self.assertEqual(command.call_args.args[0], ["/fake/rkc", "query", "$(touch nothing)"])
        self.assertEqual(command.call_args.kwargs["timeout"], 120)
        self.assertNotIn("shell", command.call_args.kwargs)
        with mock.patch.object(workflow.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout="", stderr="rejected")), \
                self.assertRaisesRegex(RuntimeError, "scan failed: rejected"):
            workflow.run(Path("/fake/rkc"), ["scan"], Path("/fake"))

    def test_stdio_evidence_requires_a_successful_matching_rpc_response(self):
        record = {"id": "evidence-1", "source": {"path": "handbook.md"}}
        replies = [{"jsonrpc": "2.0", "id": 1, "result": {}},
                   {"jsonrpc": "2.0", "id": 2, "result": {"structuredContent": record}}]
        result = SimpleNamespace(stdout="\n".join(json.dumps(row) for row in replies))
        with mock.patch.object(workflow.subprocess, "run", return_value=result) as command:
            self.assertEqual(workflow.mcp_evidence(Path("/fake/mcp"), Path("/fake/atlas"), "evidence-1"), record)
        self.assertEqual(command.call_args.kwargs["timeout"], 30)
        self.assertTrue(command.call_args.kwargs["check"])
        requests = [json.loads(line) for line in command.call_args.kwargs["input"].splitlines()]
        self.assertEqual(requests[1]["params"]["arguments"], {"evidence_id": "evidence-1"})
        for rejected in ({"id": 2, "error": {"message": "unknown"}},
                         {"id": 2, "result": {"isError": True}}):
            with mock.patch.object(workflow.subprocess, "run", return_value=SimpleNamespace(stdout=json.dumps(rejected))), \
                    self.assertRaises(RuntimeError):
                workflow.mcp_evidence(Path("/fake/mcp"), Path("/fake/atlas"), "evidence-1")

    def test_document_evidence_requires_complete_ranges_sorted_ids_and_matching_owner(self):
        packet, references = fixture_stage(Path("atlas-v1"))
        records = references["resolved_evidence"]
        with mock.patch.object(workflow, "mcp_evidence", side_effect=lambda binary, atlas, identity: records[identity]):
            result = workflow.document_references(packet, Path("/fake/mcp"), Path("atlas-v1"))
            self.assertEqual(result["resolved_evidence"], records)
        for change in ("document_count", "range", "evidence_ids", "owner"):
            value = copy.deepcopy(packet)
            current = copy.deepcopy(records)
            document = value["items"][0]
            if change == "document_count":
                value["items"] = []
            elif change == "range":
                document["source"]["anchor"] = "not-full-file"
            elif change == "evidence_ids":
                document["evidence_ids"] = list(reversed(document["evidence_ids"]))
            else:
                next(iter(current.values()))["source"]["artifact_id"] = "wrong-owner"
            with self.subTest(change=change), \
                    mock.patch.object(workflow, "mcp_evidence", side_effect=lambda binary, atlas, identity: current[identity]), \
                    self.assertRaises(RuntimeError):
                workflow.document_references(value, Path("/fake/mcp"), Path("atlas-v1"))

    def test_relocated_import_preserves_identity_rejects_tampering_and_restores_source(self):
        packet, _ = fixture_stage(Path("atlas-v1"))
        expected = packet["items"][0]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "fictional-source"
            source.mkdir()
            (source / "handbook.md").write_text(workflow.fictional_handbook(14))

            def command(binary, args, cwd):
                if args[0] == "snapshots":
                    exported = Path(args[args.index("--out") + 1])
                    exported.mkdir()
                    (exported / "bundle.json").write_text("{}")
                    return "", 1
                self.assertFalse(source.exists(), "import silently depended on live source")
                return json.dumps(packet), 1

            def reject_tampered(args, **kwargs):
                imported = Path(args[args.index("--dir") + 1])
                self.assertEqual((imported / "bundle.json").read_bytes(), b"{}\n")
                self.assertTrue(source.exists(), "source was not restored before rejection check")
                return SimpleNamespace(returncode=1)

            with mock.patch.object(workflow, "run", side_effect=command), \
                    mock.patch.object(workflow.subprocess, "run", side_effect=reject_tampered):
                result = workflow.export_import_context(Path("/fake/rkc"), root, packet["snapshot_id"], expected, 1)
            self.assertTrue(result["tampered_export_rejected"])
            self.assertEqual(result["document"], expected)
            self.assertTrue(source.exists())
            with mock.patch.object(workflow, "run", side_effect=command), \
                    mock.patch.object(workflow.subprocess, "run", return_value=SimpleNamespace(returncode=0)), \
                    self.assertRaisesRegex(RuntimeError, "Tampered export was admitted"):
                workflow.export_import_context(Path("/fake/rkc"), root, packet["snapshot_id"], expected, 2)
            with mock.patch.object(workflow, "run", side_effect=[
                    command(Path("/fake/rkc"), ["snapshots", "--out", str(root / "export-v3")], root),
                    RuntimeError("interrupted read")]), self.assertRaisesRegex(RuntimeError, "interrupted read"):
                workflow.export_import_context(Path("/fake/rkc"), root, packet["snapshot_id"], expected, 3)
            self.assertTrue(source.exists(), "failed import did not restore owned source")


class FakeProcess:
    def __init__(self, startup, stuck=False):
        self.stderr = io.StringIO(startup)
        self.stuck = stuck
        self.terminated = False
        self.killed = False

    def terminate(self):
        self.terminated = True

    def kill(self):
        self.killed = True

    def wait(self, timeout):
        if self.stuck and not self.killed:
            raise subprocess.TimeoutExpired("fake-mcp", timeout)
        return 0


class FakeSelector:
    def __init__(self, ready=True):
        self.ready = ready

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False

    def register(self, handle, events):
        self.handle = handle

    def select(self, timeout):
        return [(self.handle, 1)] if self.ready else []


class HTTPMCPTests(unittest.TestCase):
    def invoke(self, mode="success", *, stuck=False):
        packet, references = fixture_stage(Path("atlas-v1"))
        evidence_id = sorted(references["resolved_evidence"])[0]
        evidence = references["resolved_evidence"][evidence_id]
        endpoint = "http://203.0.113.1:12345/mcp" if mode == "remote" else "http://127.0.0.1:12345/mcp"
        startup = f"rkc-mcp: listening on {endpoint} (snapshot {packet['snapshot_id']}; credential-free local only)\n"
        process = FakeProcess("unexpected startup\n" if mode == "startup" else startup, stuck)
        requests = []
        closed = []

        class Connection:
            def __init__(self, host, port, timeout):
                if (host, port, timeout) != ("127.0.0.1", 12345, 10):
                    raise AssertionError("MCP request escaped the selected loopback fixture")

            def request(self, method, path, body, headers):
                if method != "POST" or path != "/mcp" or headers["MCP-Protocol-Version"] != "2025-11-25":
                    raise AssertionError("unexpected MCP HTTP contract")
                self.message = json.loads(body)
                requests.append(self.message)

            def getresponse(self):
                message = self.message
                method = message["method"]
                if method == "notifications/initialized":
                    status, body = (400, b"invalid") if mode == "notification" else (202, b"")
                else:
                    if method == "initialize":
                        result = {"protocolVersion": "wrong" if mode == "version" else "2025-11-25"}
                    elif method == "tools/list":
                        result = {"tools": [{"name": "rkc.context", "annotations": {"readOnlyHint": mode != "writable"}}]}
                    else:
                        name = message["params"]["name"]
                        value = {"hits": [{"path": "handbook.md"}]} if name == "rkc.search" else \
                            packet if name == "rkc.context" else evidence
                        result = {"structuredContent": value, "isError": mode == "tool_error"}
                    response = {"error": {"message": "rejected"}} if mode == "protocol_error" else {"result": result}
                    status = 500 if mode == "status" else 200
                    body = json.dumps(response).encode()
                    if mode == "oversized":
                        body = b"x" * (4 * 1024 * 1024 + 1)
                return SimpleNamespace(status=status, read=lambda limit: body)

            def close(self):
                closed.append(True)

        with mock.patch.object(workflow.subprocess, "Popen", return_value=process) as launch, \
                mock.patch.object(workflow.selectors, "DefaultSelector", return_value=FakeSelector(mode != "timeout")), \
                mock.patch.object(workflow.http.client, "HTTPConnection", Connection):
            if mode != "success":
                with self.assertRaises(RuntimeError):
                    workflow.mcp_http_workflow(Path("/fake/mcp"), Path("/fake/atlas"), evidence_id, 14)
                result = None
            else:
                result = workflow.mcp_http_workflow(Path("/fake/mcp"), Path("/fake/atlas"), evidence_id, 14)
        self.assertTrue(process.terminated)
        self.assertTrue(process.stderr.closed)
        self.assertEqual(len(closed), len(requests))
        self.assertEqual(launch.call_args.args[0][-2:], ["--transport", "http"])
        return result, process, requests

    def test_http_workflow_checks_transport_notification_readonly_tools_and_snapshot(self):
        result, process, requests = self.invoke(stuck=True)
        self.assertTrue(process.killed)
        self.assertEqual(result["initialized_notification"], {"http_status": 202})
        self.assertEqual(result["context"]["snapshot_id"], result["snapshot_id"])
        self.assertEqual([message["method"] for message in requests[:3]],
                         ["initialize", "notifications/initialized", "tools/list"])
        self.assertEqual([message["params"]["name"] for message in requests[3:]],
                         ["rkc.search", "rkc.context", "rkc.get_evidence"])

    def test_http_startup_protocol_bounds_and_failure_cleanup_are_enforced(self):
        for mode in ("timeout", "startup", "remote", "version", "notification", "writable",
                     "protocol_error", "status", "oversized", "tool_error"):
            with self.subTest(mode=mode):
                self.invoke(mode)


class WorkflowReceiptTests(unittest.TestCase):
    def exercise_profile(self, profile, failure=None):
        fixture = bridge_fixture()
        handler = workflow.FictionalNativeModel if profile == "neuroforge-native-extractive" else workflow.FictionalModel
        server = SimpleNamespace(server_port=12345, serve_forever=mock.Mock(), shutdown=mock.Mock(), server_close=mock.Mock())
        worker = SimpleNamespace(start=mock.Mock(), join=mock.Mock())
        commands = []

        def command(binary, args, root):
            commands.append(list(args))
            action = args[0]
            if action == "scan":
                self.assertTrue({"--no-python", "--no-git-metadata", "--no-cache"}.issubset(args))
                Path(args[args.index("--out") + 1]).mkdir()
                return "", 1
            if action == "snapshots":
                exported = Path(args[args.index("--out") + 1])
                exported.mkdir()
                (exported / "bundle.json").write_text("{}")
                return "", 1
            if action == "diff":
                return json.dumps({"summary": {"artifacts_modified": 0 if failure == "diff" else 1},
                                   "from_snapshot": fixture["packets"]["before"]["snapshot_id"],
                                   "to_snapshot": fixture["packets"]["after"]["snapshot_id"]}), 1
            atlas = Path(args[args.index("--dir") + 1])
            packet, references = fixture_stage(atlas)
            days = 14 if "v1" in atlas.name else 21
            if action == "query":
                return json.dumps({"hits": [] if failure == "query" else [{"document": {"path": "handbook.md"}}]}), 1
            if action == "context":
                if "import" in atlas.name:
                    self.assertFalse((root / "fictional-source").exists())
                if "--format" in args:
                    return f"handbook.md\nLantern lending period is {days} days.\n", 1
                return json.dumps(packet), 1
            if action != "answer":
                raise AssertionError("unexpected fake command")
            self.assertEqual(args[args.index("--endpoint") + 1], "http://127.0.0.1:12345/v1/chat/completions")
            if args[-1] == "zorblaxquux":
                return json.dumps({"status": "abstained", "claims": []}), 1
            evidence_id = sorted(references["resolved_evidence"])[0]
            sentence = f"Lantern lending period is {days} days."
            payload = native_request([{"text": sentence, "evidence_id": evidence_id}]) if profile == "neuroforge-native-extractive" else \
                structured_request([{"text": sentence, "evidence_id": evidence_id}])
            if profile == "structured-claims":
                prompt = payload["messages"][0]["content"]
                payload["messages"][0]["content"] = prompt.replace("fictional-snapshot", packet["snapshot_id"])
            _, reply = model_reply(handler, payload)
            self.assertIsNotNone(reply)
            return json.dumps({"status": "answered", "claims": [{"text": sentence}],
                               "citations": [{"evidence_id": evidence_id}],
                               "provenance": {"prompt_digest": "wrong" if failure == "audit" else handler.calls[-1]["prompt_digest"]}}), 1

        def evidence(binary, atlas, identity):
            return fixture_stage(atlas)[1]["resolved_evidence"][identity]

        def http_receipt(binary, atlas, identity, days):
            return {"context": fixture_stage(atlas)[0], "evidence": evidence(binary, atlas, identity)}

        with mock.patch.object(workflow, "HTTPServer", return_value=server) as launch, \
                mock.patch.object(workflow.threading, "Thread", return_value=worker), \
                mock.patch.object(workflow, "run", side_effect=command), \
                mock.patch.object(workflow, "mcp_evidence", side_effect=evidence), \
                mock.patch.object(workflow, "mcp_http_workflow", side_effect=http_receipt), \
                mock.patch.object(workflow.subprocess, "run", return_value=SimpleNamespace(returncode=1)):
            if failure:
                with self.assertRaises(RuntimeError):
                    workflow.exercise(Path("/fake/rkc"), Path("/fake/mcp"), profile)
                receipt = None
            else:
                receipt = workflow.exercise(Path("/fake/rkc"), Path("/fake/mcp"), profile)
        self.assertEqual(launch.call_args.args[0], ("127.0.0.1", 0))
        self.assertIs(launch.call_args.args[1], handler)
        server.shutdown.assert_called_once()
        server.server_close.assert_called_once()
        worker.join.assert_called_once_with(timeout=5)
        return receipt, commands

    def test_both_endpoint_profiles_create_reproducible_fictional_receipts_and_abstain(self):
        for profile in ("structured-claims", "neuroforge-native-extractive"):
            with self.subTest(profile=profile):
                receipt, commands = self.exercise_profile(profile)
                self.assertEqual(receipt["status"], "passed")
                self.assertEqual(receipt["remote_requests"], 0)
                self.assertEqual(receipt["api_cost"], 0)
                self.assertTrue(receipt["old_snapshot_retained"])
                self.assertEqual(len(receipt["model_requests"]), 2)
                self.assertEqual(receipt["stages"][0]["source"], workflow.fictional_handbook(14))
                self.assertEqual(receipt["stages"][1]["source"], workflow.fictional_handbook(21))
                self.assertEqual(receipt["old_snapshot_context_after_update"], receipt["stages"][0]["context"])
                self.assertTrue(all(stage["missing_evidence_model_calls"] == 0 for stage in receipt["stages"]))
                answer_commands = [args for args in commands if args[0] == "answer"]
                self.assertEqual(len(answer_commands), 4)
                if profile == "neuroforge-native-extractive":
                    self.assertTrue(all("--max-output" in args and "128" in args for args in answer_commands))

    def test_failed_query_prompt_audit_or_diff_cannot_produce_a_passing_receipt(self):
        for failure in ("query", "audit", "diff"):
            with self.subTest(failure=failure):
                self.exercise_profile("structured-claims", failure)

    def test_main_resolves_explicit_binary_paths_and_prints_the_selected_profile(self):
        with tempfile.TemporaryDirectory() as directory:
            rkc, mcp = Path(directory) / "rkc", Path(directory) / "rkc-mcp"
            rkc.touch()
            mcp.touch()
            output = io.StringIO()
            with mock.patch.object(sys, "argv", ["workflow", "--rkc", str(rkc), "--rkc-mcp", str(mcp),
                                                "--endpoint-profile", "neuroforge-native-extractive"]), \
                    mock.patch.object(workflow, "exercise", return_value={"status": "passed"}) as execute, \
                    redirect_stdout(output):
                workflow.main()
            self.assertEqual(json.loads(output.getvalue()), {"status": "passed"})
            execute.assert_called_once_with(rkc.resolve(), mcp.resolve(), "neuroforge-native-extractive")
            with mock.patch.object(sys, "argv", ["workflow", "--rkc", str(rkc / "missing"), "--rkc-mcp", str(mcp)]), \
                    self.assertRaises(OSError):
                workflow.main()


if __name__ == "__main__":
    unittest.main()
