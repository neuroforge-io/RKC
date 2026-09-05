"""Exact-source release qualification and credential-safe transport regressions."""
from __future__ import annotations

import http.client
import io
import json
import os
import runpy
import tempfile
import unittest
import urllib.error
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest.mock import patch

try:
    from scripts import verify_main_ci as gate
except ModuleNotFoundError as exc:
    if exc.name != "scripts":
        raise
    import verify_main_ci as gate

REPOSITORY = "neuroforge-io/RKC"
COMMIT = "a" * 40
CREDENTIAL = "unit-test-only"


def run(number=20, attempt=1, **changes):
    identifier = 1000 + number
    record = {
        "id": identifier, "run_number": number, "run_attempt": attempt,
        "workflow_id": 19, "head_sha": COMMIT, "head_branch": "main", "event": "push",
        "path": ".github/workflows/ci.yml", "status": "completed", "conclusion": "success",
        "repository": {"id": 23, "full_name": REPOSITORY},
        "head_repository": {"id": 23, "full_name": REPOSITORY},
        "html_url": f"https://github.com/{REPOSITORY}/actions/runs/{identifier}",
    }
    record.update(changes)
    return record


def collection(*rows):
    return {"total_count": len(rows), "workflow_runs": list(rows)}


class MainCIProofTests(unittest.TestCase):
    def verify(self, listing, current=None):
        with patch.object(gate, "get_json", side_effect=[listing, current]) as request:
            result = gate.verify(REPOSITORY, COMMIT, CREDENTIAL)
        return result, request.call_args_list

    def test_exact_main_push_and_refreshed_latest_attempt_are_recorded(self):
        current = run(attempt=3)
        current["repository"]["unrelated_metadata"] = "may change"
        proof, calls = self.verify(collection(run(20), run(19)), current)
        self.assertEqual(proof["schema_version"], "rkc-main-ci-proof/v1")
        for key, value in {"repository": REPOSITORY, "source_commit": COMMIT, "run_id": 1020,
                           "run_number": 20, "run_attempt": 3, "repository_id": 23, "workflow_id": 19,
                           "workflow_path": ".github/workflows/ci.yml", "event": "push", "head_branch": "main",
                           "status": "completed", "conclusion": "success", "html_url": current["html_url"]}.items():
            self.assertEqual(proof[key], value)
        self.assertIn("head_sha=" + COMMIT, calls[0].args[0])
        self.assertIn("branch=main&event=push", calls[0].args[0])
        self.assertNotIn("status=", calls[0].args[0])
        self.assertEqual(calls[1].args[0], f"/repos/{REPOSITORY}/actions/runs/1020")
        self.assertNotIn(CREDENTIAL, json.dumps(proof))

    def test_latest_failure_or_pending_attempt_cannot_reuse_an_older_success(self):
        for status, conclusion in (("completed", "failure"), ("in_progress", None),
                                   ("queued", None), ("completed", "cancelled"),
                                   ("completed", "skipped"), ("completed", "neutral")):
            with self.subTest(status=status, conclusion=conclusion):
                current = run(21, 2, status=status, conclusion=conclusion)
                with self.assertRaisesRegex(gate.VerificationError, "not completed successfully"):
                    self.verify(collection(run(21), run(20)), current)

    def test_response_order_does_not_select_an_older_run(self):
        proof, _ = self.verify(collection(run(19), run(22), run(20)), run(22))
        self.assertEqual(proof["run_number"], 22)

    def test_final_qualification_rechecks_a_previously_successful_attempt(self):
        pending = run(attempt=2, status="in_progress", conclusion=None)
        with patch.object(gate, "get_json", side_effect=[collection(run()), run(), collection(pending), pending]) as request:
            self.assertEqual(gate.verify(REPOSITORY, COMMIT, CREDENTIAL)["run_attempt"], 1)
            with self.assertRaisesRegex(gate.VerificationError, "not completed successfully"):
                gate.verify(REPOSITORY, COMMIT, CREDENTIAL)
            self.assertEqual(request.call_count, 4)

    def test_invalid_identity_fields_fail_closed_even_in_filtered_response(self):
        cases = [None, [], run(id=True), run(run_attempt=0), run(run_number=-1), run(workflow_id=2**63),
                 run(head_sha="b" * 40), run(head_branch="other"), run(event="workflow_dispatch"),
                 run(path=".github/workflows/other.yml"), run(repository=None), run(head_repository=[]),
                 run(repository={"id": 23, "full_name": "other/repo"}),
                 run(head_repository={"id": 23, "full_name": "fork/RKC"}),
                 run(repository={"id": False, "full_name": REPOSITORY}),
                 run(repository={"id": 1, "full_name": REPOSITORY}, head_repository={"id": True, "full_name": REPOSITORY}),
                 run(head_repository={"id": 24, "full_name": REPOSITORY}),
                 run(html_url="https://example.invalid/forged")]
        for record in cases:
            with self.subTest(record=record), self.assertRaises(gate.VerificationError):
                self.verify(collection(record), run())

    def test_direct_refresh_must_retain_source_identity_and_not_downgrade_attempt(self):
        for current in (run(19), run(workflow_id=17), run(attempt=1), run(head_sha="b" * 40),
                        run(repository={"id": 24, "full_name": REPOSITORY}, head_repository={"id": 24, "full_name": REPOSITORY})):
            with self.subTest(current=current), self.assertRaises(gate.VerificationError):
                self.verify(collection(run(attempt=2)), current)

    def test_collection_bounds_ambiguity_and_no_results_fail_closed(self):
        for listing in (None, [], {}, {"total_count": True, "workflow_runs": []},
                        {"total_count": 1, "workflow_runs": None}, collection(),
                        {"total_count": 101, "workflow_runs": [run()]},
                        {"total_count": 2, "workflow_runs": [run()]},
                        collection(run(), run()), collection(run(), run(id=999)),
                        collection(run(), run(21, workflow_id=22))):
            with self.subTest(listing=listing), self.assertRaises(gate.VerificationError):
                self.verify(listing, run())

    def test_configuration_rejected_before_network_access(self):
        for repository, commit, token in (("../RKC", COMMIT, CREDENTIAL), ("owner/repo/extra", COMMIT, CREDENTIAL),
                                         (REPOSITORY, "HEAD", CREDENTIAL), (REPOSITORY, COMMIT.upper(), CREDENTIAL),
                                         (REPOSITORY, COMMIT, ""), (REPOSITORY, COMMIT, "x" * 4097),
                                         (REPOSITORY, COMMIT, "has\nnewline"), (REPOSITORY, COMMIT, "nonascii-\u00e9")):
            with self.subTest(repository=repository, commit=commit), patch.object(gate, "get_json") as request:
                with self.assertRaises(gate.VerificationError):
                    gate.verify(repository, commit, token)
                request.assert_not_called()

    def test_main_writes_a_new_receipt_without_exposing_the_credential(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "proof.json"
            arguments = ["--repository", REPOSITORY, "--commit", COMMIT, "--output", str(output)]
            stdout, stderr = io.StringIO(), io.StringIO()
            with patch.dict(os.environ, {"RKC_GITHUB_TOKEN": CREDENTIAL}), \
                 patch.object(gate, "get_json", side_effect=[collection(run()), run()]), \
                 redirect_stdout(stdout), redirect_stderr(stderr):
                self.assertEqual(gate.main(arguments), 0)
            proof = json.loads(output.read_text())
            self.assertEqual(proof["source_commit"], COMMIT)
            self.assertIn(proof["html_url"], stdout.getvalue())
            self.assertNotIn(CREDENTIAL, output.read_text() + stdout.getvalue() + stderr.getvalue())
            with patch.object(gate, "verify", return_value=proof), redirect_stderr(stderr):
                self.assertEqual(gate.main(arguments), 1)
            self.assertEqual(json.loads(output.read_text()), proof)

    def test_main_failure_has_no_partial_receipt_or_credential_traceback(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "proof.json"
            stderr = io.StringIO()
            with patch.dict(os.environ, {"RKC_GITHUB_TOKEN": CREDENTIAL}), \
                 patch.object(gate.urllib.request, "build_opener") as opener, redirect_stderr(stderr):
                opener.return_value.open.side_effect = http.client.BadStatusLine(CREDENTIAL)
                self.assertEqual(gate.main(["--repository", REPOSITORY, "--commit", COMMIT, "--output", str(output)]), 1)
            self.assertFalse(output.exists())
            self.assertNotIn(CREDENTIAL, stderr.getvalue())

    def test_command_entry_point_fails_closed(self):
        stderr = io.StringIO()
        arguments = [str(Path(gate.__file__)), "--repository", REPOSITORY, "--commit", COMMIT, "--output", "unused"]
        with patch("sys.argv", arguments), patch.dict(os.environ, {"RKC_GITHUB_TOKEN": ""}), redirect_stderr(stderr):
            with self.assertRaises(SystemExit) as result:
                runpy.run_path(gate.__file__, run_name="__main__")
            self.assertEqual(result.exception.code, 1)


class MainCITransportTests(unittest.TestCase):
    def response(self, payload, status=200):
        opener = unittest.mock.MagicMock()
        response = opener.open.return_value.__enter__.return_value
        response.status = status
        response.read.return_value = payload
        return opener, response

    def test_bounded_https_request_and_no_redirect_handler(self):
        opener, response = self.response(b'{"ok":true}')
        with patch.object(gate.urllib.request, "build_opener", return_value=opener) as factory:
            self.assertEqual(gate.get_json("/repos/example/repo", CREDENTIAL), {"ok": True})
        self.assertIsInstance(factory.call_args.args[0], gate.NoRedirect)
        request = opener.open.call_args.args[0]
        self.assertEqual(request.full_url, "https://api.github.com/repos/example/repo")
        self.assertEqual(request.get_header("Authorization"), "Bearer " + CREDENTIAL)
        self.assertEqual(opener.open.call_args.kwargs["timeout"], 15)
        response.read.assert_called_once_with(gate.MAX_RESPONSE_BYTES + 1)
        self.assertIsNone(gate.NoRedirect().redirect_request(None, None, 302, "redirect", {}, "https://example.invalid"))

    def test_network_redirect_and_malformed_http_errors_are_sanitized(self):
        failures = [urllib.error.URLError(CREDENTIAL), urllib.error.HTTPError("https://api.github.com", 302, CREDENTIAL, {}, None),
                    TimeoutError(CREDENTIAL), OSError(CREDENTIAL), ValueError(CREDENTIAL),
                    http.client.BadStatusLine(CREDENTIAL), http.client.IncompleteRead(CREDENTIAL.encode())]
        for failure in failures:
            with self.subTest(failure=type(failure).__name__), patch.object(gate.urllib.request, "build_opener") as factory:
                factory.return_value.open.side_effect = failure
                with self.assertRaises(gate.VerificationError) as result:
                    gate.get_json("/repos/example/repo", CREDENTIAL)
                self.assertNotIn(CREDENTIAL, str(result.exception))

    def test_non_200_oversize_invalid_utf8_and_duplicate_fields_are_rejected(self):
        for payload, status in ((b'{}', 204), (b'x' * (gate.MAX_RESPONSE_BYTES + 1), 200),
                                (b'not JSON', 200), (b'\xff', 200), (b'{"a":1,"a":2}', 200)):
            with self.subTest(status=status, size=len(payload)):
                opener, _ = self.response(payload, status)
                with patch.object(gate.urllib.request, "build_opener", return_value=opener), self.assertRaises(gate.VerificationError):
                    gate.get_json("/repos/example/repo", CREDENTIAL)
        opener, _ = self.response(b'{}')
        with patch.object(gate.urllib.request, "build_opener", return_value=opener), \
             patch.object(gate.json, "loads", side_effect=RecursionError(CREDENTIAL)), self.assertRaises(gate.VerificationError):
            gate.get_json("/repos/example/repo", CREDENTIAL)


if __name__ == "__main__":
    unittest.main()
