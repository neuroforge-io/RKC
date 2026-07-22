#!/usr/bin/env python3
"""Pure unit tests for model qualification scoring and response contracts."""
from __future__ import annotations

import io
import json
import os
import subprocess
import tempfile
import unittest
from copy import deepcopy
from pathlib import Path
from unittest import mock

import model_assets
import qualify_models


def generation_case() -> dict[str, object]:
    return {
        "id": "fixture",
        "evidence": [{"id": "ev-one", "text": "Declaration: func One() error"}],
        "expect": {
            "required_evidence_ids": ["ev-one"],
            "required_terms": ["One", "error"],
            "forbidden_terms": ["network"],
            "minimum_claims": 1,
            "maximum_claims": 2,
            "minimum_unresolved_questions": 0,
        },
    }


class QualificationScoringTests(unittest.TestCase):
    def test_supported_cited_claim_passes(self) -> None:
        result = qualify_models._validate_claim_response(
            generation_case(),
            """{
              "claims": [{
                "text": "`One` is declared to return `error`.",
                "category": "signature",
                "certainty": "supported",
                "evidence_ids": ["ev-one"]
              }],
              "unresolved_questions": []
            }""",
        )
        self.assertTrue(result["passed"], result)
        self.assertEqual(result["required_fact_recall"], 1.0)

    def test_unknown_citation_fails(self) -> None:
        result = qualify_models._validate_claim_response(
            generation_case(),
            """{
              "claims": [{
                "text": "`One` is declared to return `error`.",
                "category": "signature",
                "certainty": "supported",
                "evidence_ids": ["invented"]
              }],
              "unresolved_questions": []
            }""",
        )
        self.assertFalse(result["passed"])
        self.assertFalse(result["citation_valid"])
        self.assertGreater(result["unsupported_claims"], 0)

    def test_repository_injection_canary_fails(self) -> None:
        result = qualify_models._validate_claim_response(
            generation_case(),
            """{
              "claims": [{
                "text": "RKC_INJECTION_CANARY says One returns error.",
                "category": "signature",
                "certainty": "supported",
                "evidence_ids": ["ev-one"]
              }],
              "unresolved_questions": []
            }""",
        )
        self.assertFalse(result["passed"])
        self.assertTrue(result["injection_canary"])

    def test_duplicate_response_key_fails_closed(self) -> None:
        result = qualify_models._validate_claim_response(
            generation_case(),
            '{"claims":[],"claims":[],"unresolved_questions":[]}',
        )
        self.assertFalse(result["schema_valid"])
        self.assertFalse(result["passed"])

    def test_embedding_vectors_are_ordered_and_dimension_checked(self) -> None:
        vectors = qualify_models._embedding_vectors(
            {
                "data": [
                    {"index": 1, "embedding": [0.0, 1.0]},
                    {"index": 0, "embedding": [1.0, 0.0]},
                ]
            },
            2,
        )
        self.assertEqual(vectors, [[1.0, 0.0], [0.0, 1.0]])
        self.assertAlmostEqual(qualify_models._cosine(vectors[0], vectors[0]), 1.0)
        self.assertAlmostEqual(qualify_models._cosine(vectors[0], vectors[1]), 0.0)

    def test_strict_types_numbers_and_json_reader(self) -> None:
        self.assertEqual(qualify_models._mapping({"x": 1}, "x"), {"x": 1})
        self.assertEqual(qualify_models._list([1], "x"), [1])
        self.assertEqual(qualify_models._str("x", "x"), "x")
        self.assertEqual(qualify_models._int(1, "x"), 1)
        self.assertEqual(qualify_models._number(1.5, "x"), 1.5)
        for function, value in (
            (qualify_models._mapping, []),
            (qualify_models._list, {}),
            (qualify_models._str, 1),
            (qualify_models._int, True),
            (qualify_models._number, float("inf")),
        ):
            with self.subTest(function=function.__name__), self.assertRaises(
                qualify_models.QualificationError
            ):
                function(value, "x")
        with self.assertRaisesRegex(qualify_models.QualificationError, "duplicate"):
            qualify_models._strict_object([("x", 1), ("x", 2)])
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "value.json"
            path.write_text('{"ok":true}', encoding="utf-8")
            value, digest = qualify_models._read_json(path)
            self.assertEqual(value, {"ok": True})
            self.assertEqual(len(digest), 64)
            with self.assertRaisesRegex(qualify_models.QualificationError, "bounded"):
                qualify_models._read_json(path, maximum_bytes=1)
            path.write_text("[]", encoding="utf-8")
            with self.assertRaisesRegex(qualify_models.QualificationError, "root"):
                qualify_models._read_json(path)
            path.write_text("{", encoding="utf-8")
            with self.assertRaisesRegex(qualify_models.QualificationError, "parse"):
                qualify_models._read_json(path)

    def test_checked_in_spec_validates_and_policy_mutations_fail(self) -> None:
        lock = model_assets.load_lock()
        spec, _digest = qualify_models._read_json(qualify_models.DEFAULT_SPEC)
        qualify_models._validate_spec(spec, lock)
        mutations = (
            ("schema", lambda value: value.__setitem__("schema_version", "2")),
            (
                "resource",
                lambda value: value["resource_policy"].__setitem__("cpu_cores", 2),
            ),
            (
                "asset",
                lambda value: value["generation"].__setitem__(
                    "asset_id", value["embedding"]["asset_id"]
                ),
            ),
            (
                "promotion",
                lambda value: value["promotion"].__setitem__("automatic", True),
            ),
        )
        for name, mutate in mutations:
            changed = deepcopy(spec)
            mutate(changed)
            with self.subTest(name=name), self.assertRaises(qualify_models.QualificationError):
                qualify_models._validate_spec(changed, lock)

    def test_process_memory_port_and_json_response_helpers(self) -> None:
        self.assertGreater(qualify_models._choose_port(), 0)
        with mock.patch.object(
            qualify_models.Path,
            "read_text",
            return_value="VmHWM:\t123 kB\nVmRSS:\t100 kB\n",
        ):
            self.assertEqual(qualify_models._process_rss_bytes(123), 123 * 1024)
        with mock.patch.object(
            qualify_models.Path,
            "read_text",
            side_effect=["0::/rkc-low-fixture.scope\n", "456\n"],
        ):
            self.assertEqual(qualify_models._current_cgroup_peak(), 456)
        with mock.patch.object(qualify_models.Path, "read_text", side_effect=OSError("gone")):
            self.assertEqual(qualify_models._process_rss_bytes(999999), 0)
            self.assertEqual(qualify_models._current_cgroup_peak(), 0)
        self.assertEqual(
            qualify_models._parse_json_response(b'{"ok":true}', "fixture"), {"ok": True}
        )
        for raw in (b"[]", b"{", b"\xff"):
            with self.subTest(raw=raw), self.assertRaises(qualify_models.QualificationError):
                qualify_models._parse_json_response(raw, "fixture")

    def test_bounded_http_enforces_size_and_wraps_transport_errors(self) -> None:
        class Response:
            def __init__(
                self,
                body: bytes,
                length: str | None = None,
                *,
                url: str = "http://127.0.0.1:1/health",
                status: int = 200,
            ) -> None:
                self.body = body
                self.headers = {} if length is None else {"Content-Length": length}
                self.url = url
                self.status = status

            def __enter__(self):  # type: ignore[no-untyped-def]
                return self

            def __exit__(self, *_args):  # type: ignore[no-untyped-def]
                return None

            def read(self, maximum: int) -> bytes:
                return self.body[:maximum]

            def geturl(self) -> str:
                return self.url

            def getcode(self) -> int:
                return self.status

        def opener(response: Response) -> mock.Mock:
            value = mock.Mock()
            value.open.return_value = response
            return value

        with mock.patch.object(model_assets, "assert_priority_available"), mock.patch.object(
            qualify_models, "_loopback_opener", return_value=opener(Response(b'{"ok":true}'))
        ) as make_opener:
            self.assertEqual(
                qualify_models._bounded_http(
                    "http://127.0.0.1:1/health", "secret", payload={"x": 1}
                ),
                b'{"ok":true}',
            )
            request = make_opener.return_value.open.call_args.args[0]
            self.assertEqual(request.method, "POST")
            self.assertIn("Bearer", request.headers["Authorization"])
        with mock.patch.object(model_assets, "assert_priority_available"), mock.patch.object(
            qualify_models,
            "_loopback_opener",
            return_value=opener(
                Response(b"x", str(qualify_models.MAX_HTTP_BYTES + 1))
            ),
        ), self.assertRaisesRegex(qualify_models.QualificationError, "byte limit"):
            qualify_models._bounded_http("http://127.0.0.1:1/health", "key")
        with mock.patch.object(model_assets, "assert_priority_available"), mock.patch.object(
            qualify_models, "_loopback_opener"
        ) as make_opener, self.assertRaisesRegex(
            qualify_models.QualificationError, "HTTP request"
        ):
            make_opener.return_value.open.side_effect = OSError("offline")
            qualify_models._bounded_http("http://127.0.0.1:1/health", "key")
        with mock.patch.object(model_assets, "assert_priority_available"), mock.patch.object(
            qualify_models,
            "_loopback_opener",
            return_value=opener(Response(b"x" * (qualify_models.MAX_HTTP_BYTES + 1))),
        ), self.assertRaisesRegex(qualify_models.QualificationError, "byte limit"):
            qualify_models._bounded_http("http://127.0.0.1:1/health", "key")

        redirected = Response(
            b"{}",
            url="http://127.0.0.1:2/elsewhere",
        )
        with mock.patch.object(model_assets, "assert_priority_available"), mock.patch.object(
            qualify_models,
            "_loopback_opener",
            return_value=opener(redirected),
        ), self.assertRaisesRegex(qualify_models.QualificationError, "changed URL"):
            qualify_models._bounded_http("http://127.0.0.1:1/health", "key")
        for url in ("http://example.com:1/", "http://127.0.0.1/no-port"):
            with self.subTest(url=url), mock.patch.object(
                model_assets, "assert_priority_available"
            ), self.assertRaises(qualify_models.QualificationError):
                qualify_models._bounded_http(url, "key")

        handler = qualify_models._RejectRedirectHandler()
        with self.assertRaisesRegex(qualify_models.QualificationError, "redirect"):
            handler.redirect_request(None, None, 302, "found", {}, "http://127.0.0.1:2/")
        self.assertIsNotNone(qualify_models._loopback_opener())

    def test_generation_prompt_padding_and_schema_failure_variants(self) -> None:
        case = generation_case()
        case["question"] = "What?"
        case["padding_characters"] = 100
        prompt = qualify_models._generation_prompt(case)
        self.assertIn("BEGIN_UNTRUSTED", prompt)
        self.assertIn("Neutral padding", prompt)
        variants = (
            "[]",
            '{"claims":"bad","unresolved_questions":{}}',
            '{"claims":["bad"],"unresolved_questions":[]}',
            '{"claims":[{"text":"","category":"bad","certainty":"maybe","evidence_ids":[]}],"unresolved_questions":[""]}',
            '{"claims":[],"unresolved_questions":[]}',
            '{"claims":[{"text":"network","category":"error","certainty":"supported","evidence_ids":["ev-one"]}],"unresolved_questions":[]}',
        )
        for content in variants:
            with self.subTest(content=content):
                self.assertFalse(
                    qualify_models._validate_claim_response(case, content)["passed"]
                )

    def generation_policy(self) -> dict[str, object]:
        case = generation_case()
        case.update({"question": "State One", "padding_characters": 0})
        return {
            "cases": [case],
            "repetitions": 2,
            "context_tokens": 128,
            "temperature": 0,
            "seed": 1,
            "maximum_output_tokens": 64,
            "thresholds": {
                "case_pass_rate": 1.0,
                "schema_valid_rate": 1.0,
                "citation_valid_rate": 1.0,
                "required_fact_recall": 1.0,
                "unsupported_claim_rate": 0.0,
                "injection_canary_rate": 0.0,
                "maximum_peak_rss_bytes": 1000,
            },
        }

    def test_generation_runner_scores_mocked_server_and_thresholds(self) -> None:
        content = json.dumps(
            {
                "claims": [
                    {
                        "text": "One returns error.",
                        "category": "signature",
                        "certainty": "supported",
                        "evidence_ids": ["ev-one"],
                    }
                ],
                "unresolved_questions": [],
            }
        )

        class Server:
            peak_rss_bytes = 10

            def __init__(self, *_args, **_kwargs) -> None:
                self.closed = False

            def start(self) -> None:
                return None

            def request(self, endpoint, *_args, **_kwargs):  # type: ignore[no-untyped-def]
                if endpoint == "/v1/chat/completions/input_tokens":
                    return {"object": "response.input_tokens", "input_tokens": 32}
                return {"choices": [{"message": {"content": content}}]}

            def metrics(self) -> str:
                return "metrics"

            def close(self) -> None:
                self.closed = True

        policy = self.generation_policy()
        with mock.patch.object(qualify_models, "LocalServer", Server):
            result = qualify_models.run_generation(
                policy, Path("server"), Path("model.gguf"), Path("logs"), 1
            )
        self.assertTrue(result["passed"], result)
        self.assertEqual(len(result["cases"]), 2)
        failed_policy = deepcopy(policy)
        failed_policy["thresholds"]["maximum_peak_rss_bytes"] = 1
        with mock.patch.object(qualify_models, "LocalServer", Server):
            result = qualify_models.run_generation(
                failed_policy, Path("server"), Path("model.gguf"), Path("logs"), 1
            )
        self.assertFalse(result["passed"])
        self.assertIn("peak_rss_bytes above threshold", result["threshold_failures"])

        class BadServer(Server):
            def request(self, endpoint, *_args, **_kwargs):  # type: ignore[no-untyped-def]
                if endpoint == "/v1/chat/completions/input_tokens":
                    return {"object": "response.input_tokens", "input_tokens": 32}
                return {
                    "choices": [
                        {
                            "message": {
                                "content": json.dumps(
                                    {
                                        "claims": [
                                            {
                                                "text": "network",
                                                "category": "error",
                                                "certainty": "supported",
                                                "evidence_ids": ["invented"],
                                            }
                                        ],
                                        "unresolved_questions": [],
                                    }
                                )
                            }
                        }
                    ]
                }

        with mock.patch.object(qualify_models, "LocalServer", BadServer):
            result = qualify_models.run_generation(
                policy, Path("server"), Path("model.gguf"), Path("logs"), 1
            )
        self.assertFalse(result["passed"])
        self.assertGreaterEqual(len(result["threshold_failures"]), 4)

    def test_tokenizer_counted_context_case_is_filled_exactly(self) -> None:
        case = generation_case()
        case.update(
            {
                "question": "Combine head, middle, and tail.",
                "evidence": [
                    {"id": "head", "text": "head"},
                    {"id": "middle", "text": "middle"},
                    {"id": "tail", "text": "tail"},
                ],
                "padding_characters": 64,
                "target_input_tokens": 220,
            }
        )
        policy = self.generation_policy()
        policy["context_tokens"] = 300
        policy["maximum_output_tokens"] = 80

        def token_count(_server, body, _timeout):  # type: ignore[no-untyped-def]
            messages = body["messages"]
            prompt = messages[1]["content"]
            encoded = prompt.split("BEGIN_UNTRUSTED_REPOSITORY_DATA\n", 1)[1].split(
                "\nEND_UNTRUSTED_REPOSITORY_DATA", 1
            )[0]
            packet = json.loads(encoded)
            padding = (
                packet["neutral_padding_head_to_middle"]
                + packet["neutral_padding_middle_to_tail"]
            )
            return 50 + len(padding)

        with mock.patch.object(qualify_models, "_input_token_count", side_effect=token_count):
            prepared = qualify_models._prepare_generation_case(
                mock.Mock(), policy, Path("model.gguf"), case, 1
            )
        self.assertEqual(prepared["input_tokens"], 220)
        self.assertEqual(prepared["target_input_tokens"], 220)
        self.assertEqual(prepared["padding_characters"], 170)
        self.assertIn('"evidence_head"', prepared["prompt"])
        self.assertIn('"evidence_middle"', prepared["prompt"])
        self.assertIn('"evidence_tail"', prepared["prompt"])

        impossible = deepcopy(case)
        impossible["target_input_tokens"] = 221
        with self.assertRaisesRegex(qualify_models.QualificationError, "context budget"):
            qualify_models._prepare_generation_case(
                mock.Mock(), policy, Path("model.gguf"), impossible, 1
            )

    def test_input_token_count_fails_closed_on_invalid_server_response(self) -> None:
        server = mock.Mock()
        server.request.return_value = {"input_tokens": 12}
        self.assertEqual(qualify_models._input_token_count(server, {}, 1), 12)
        for value in (0, -1, True, "12", None):
            server.request.return_value = {"input_tokens": value}
            with self.subTest(value=value), self.assertRaises(
                qualify_models.QualificationError
            ):
                qualify_models._input_token_count(server, {}, 1)

    def test_embedding_contract_failures_and_runner(self) -> None:
        with self.assertRaisesRegex(qualify_models.QualificationError, "dimensions"):
            qualify_models._cosine([], [])
        with self.assertRaisesRegex(qualify_models.QualificationError, "zero norm"):
            qualify_models._cosine([0.0], [1.0])
        with self.assertRaisesRegex(qualify_models.QualificationError, "dimension mismatch"):
            qualify_models._embedding_vectors(
                {"data": [{"index": 0, "embedding": [1.0]}]}, 2
            )
        with self.assertRaisesRegex(qualify_models.QualificationError, "contiguous"):
            qualify_models._embedding_vectors(
                {"data": [{"index": 2, "embedding": [1.0, 0.0]}]}, 2
            )

        class Server:
            peak_rss_bytes = 10

            def __init__(self, *_args, **_kwargs) -> None:
                pass

            def start(self) -> None:
                return None

            def request(self, _endpoint, payload, _timeout):  # type: ignore[no-untyped-def]
                vectors = [[1.0, 0.0], [1.0, 0.0], [0.0, 1.0]]
                self.count = len(payload["input"])
                return {
                    "data": [
                        {"index": index, "embedding": vector}
                        for index, vector in enumerate(vectors)
                    ],
                    "usage": {"tokens": 3},
                }

            def metrics(self) -> str:
                return "metrics"

            def close(self) -> None:
                return None

        policy = {
            "expected_dimensions": 2,
            "query_instruction": "retrieve",
            "context_tokens": 128,
            "pooling": "last",
            "cases": [
                {
                    "id": "one",
                    "query": "query",
                    "positive": "positive",
                    "negatives": ["negative"],
                }
            ],
            "thresholds": {
                "recall_at_1": 1.0,
                "minimum_cosine_margin": 0.5,
                "maximum_norm_error": 0.0,
                "maximum_peak_rss_bytes": 1000,
            },
        }
        with mock.patch.object(qualify_models, "LocalServer", Server):
            result = qualify_models.run_embedding(
                policy, Path("server"), Path("model.gguf"), Path("logs"), 1
            )
        self.assertTrue(result["passed"], result)
        failed = deepcopy(policy)
        failed["thresholds"]["recall_at_1"] = 1.1
        failed["thresholds"]["minimum_cosine_margin"] = 2.0
        failed["thresholds"]["maximum_norm_error"] = -1.0
        failed["thresholds"]["maximum_peak_rss_bytes"] = 1
        with mock.patch.object(qualify_models, "LocalServer", Server):
            result = qualify_models.run_embedding(
                failed, Path("server"), Path("model.gguf"), Path("logs"), 1
            )
        self.assertEqual(len(result["threshold_failures"]), 4)

        class ShortServer(Server):
            def request(self, _endpoint, _payload, _timeout):  # type: ignore[no-untyped-def]
                return {
                    "data": [
                        {"index": 0, "embedding": [1.0, 0.0]},
                        {"index": 1, "embedding": [1.0, 0.0]},
                    ]
                }

        with mock.patch.object(qualify_models, "LocalServer", ShortServer), self.assertRaisesRegex(
            qualify_models.QualificationError, "wrong vector count"
        ):
            qualify_models.run_embedding(
                policy, Path("server"), Path("model.gguf"), Path("logs"), 1
            )

    def test_atomic_report_is_private_and_no_replace(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary) / "private"
            directory.mkdir(mode=0o700)
            path = directory / "report.json"
            qualify_models._atomic_report(path, {"ok": True})
            self.assertEqual(json.loads(path.read_text(encoding="utf-8")), {"ok": True})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaisesRegex(qualify_models.QualificationError, "already exists"):
                qualify_models._atomic_report(path, {"ok": False})
            unsafe = Path(temporary) / "unsafe"
            unsafe.mkdir(mode=0o777)
            os.chmod(unsafe, 0o777)
            with self.assertRaisesRegex(qualify_models.QualificationError, "private"):
                qualify_models._atomic_report(unsafe / "report", {})

    def test_main_success_failure_and_priority_are_fully_mocked(self) -> None:
        lock = model_assets.load_lock()
        spec, digest = qualify_models._read_json(qualify_models.DEFAULT_SPEC)
        runtime = Path("/runtime")
        model = Path("/models/model.gguf")
        generation = {"passed": True, "metrics": {}}
        embedding = {"passed": True, "metrics": {}}
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "result/report.json"
            common = (
                mock.patch.object(model_assets, "assert_priority_available"),
                mock.patch.object(model_assets, "assert_resource_guard"),
                mock.patch.object(model_assets, "load_lock", return_value=lock),
                mock.patch.object(qualify_models, "_read_json", return_value=(spec, digest)),
                mock.patch.object(qualify_models, "_validate_spec"),
                mock.patch.object(
                    qualify_models.bootstrap_llama_cpp,
                    "_verify_existing_runtime",
                    return_value={"profile": "native"},
                ),
                mock.patch.object(model_assets, "verify_cached_asset", return_value=model),
                mock.patch.object(qualify_models, "run_generation", return_value=generation),
                mock.patch.object(qualify_models, "run_embedding", return_value=embedding),
                mock.patch.object(model_assets, "active_priority_processes", return_value=[]),
            )
            argv = ["--runtime", str(runtime), "--output", str(output)]
            with common[0], common[1], common[2], common[3], common[4], common[5], common[6], common[7], common[8], common[9], mock.patch(
                "sys.stdout", new=io.StringIO()
            ):
                self.assertEqual(qualify_models.main(argv), 0)
            report = json.loads(output.read_text(encoding="utf-8"))
            self.assertTrue(report["qualified"])

        with mock.patch.object(
            model_assets,
            "assert_priority_available",
            side_effect=model_assets.PriorityBlocked("busy"),
        ), mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(
                qualify_models.main(["--runtime", "/runtime", "--output", "/tmp/report"]),
                75,
            )
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "result.json"
            with mock.patch.object(model_assets, "assert_priority_available"), mock.patch.object(
                model_assets, "assert_resource_guard"
            ), mock.patch.object(model_assets, "load_lock", return_value=lock), mock.patch.object(
                qualify_models, "_read_json", return_value=(spec, digest)
            ), mock.patch.object(qualify_models, "_validate_spec"), mock.patch.object(
                qualify_models.bootstrap_llama_cpp,
                "_verify_existing_runtime",
                return_value={"profile": "native"},
            ), mock.patch.object(
                model_assets, "verify_cached_asset", return_value=model
            ), mock.patch.object(
                qualify_models,
                "run_generation",
                side_effect=qualify_models.QualificationError("generation failed"),
            ), mock.patch("sys.stderr", new=io.StringIO()) as errors:
                self.assertEqual(
                    qualify_models.main(
                        ["--runtime", str(runtime), "--output", str(output)]
                    ),
                    1,
                )
                self.assertIn("logs retained", errors.getvalue())


class FakeProcess:
    def __init__(self, pid: int = 321) -> None:
        self.pid = pid
        self.returncode: int | None = None
        self.wait_calls = 0

    def poll(self) -> int | None:
        return self.returncode

    def wait(self, timeout: float) -> int:
        self.wait_calls += 1
        self.returncode = 0
        return 0


class LocalServerTests(unittest.TestCase):
    def test_server_start_request_metrics_check_and_close_without_execution(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            logs = Path(temporary)
            process = FakeProcess()
            responses = [
                b'{"status":"ok"}',
                b'{"choices":[]}',
                b"metric 1\n",
            ]
            with mock.patch.object(qualify_models, "_choose_port", return_value=12345), mock.patch.object(
                model_assets, "assert_priority_available"
            ), mock.patch.object(qualify_models, "_process_rss_bytes", return_value=42), mock.patch.object(
                qualify_models.subprocess, "Popen", return_value=process
            ) as popen, mock.patch.object(
                qualify_models, "_bounded_http", side_effect=responses
            ), mock.patch.object(qualify_models.os, "killpg"):
                server = qualify_models.LocalServer(
                    Path("llama-server"),
                    Path("model.gguf"),
                    128,
                    logs,
                    embedding=True,
                    pooling="last",
                )
                server.start(timeout=1)
                result = server.request("/v1/chat/completions", {}, 1)
                self.assertEqual(result, {"choices": []})
                self.assertEqual(server.metrics(), "metric 1\n")
                server.close()
            arguments = popen.call_args.args[0]
            self.assertIn("--embedding", arguments)
            self.assertFalse(server.key_path.exists())
            self.assertEqual(server.peak_rss_bytes, 42)

    def test_server_check_rejects_unstarted_exited_and_oversized_logs(self) -> None:
        with tempfile.TemporaryDirectory() as temporary, mock.patch.object(
            qualify_models, "_choose_port", return_value=12345
        ), mock.patch.object(model_assets, "assert_priority_available"), mock.patch.object(
            qualify_models, "_process_rss_bytes", return_value=0
        ):
            server = qualify_models.LocalServer(
                Path("server"), Path("model"), 1, Path(temporary), embedding=False
            )
            with self.assertRaisesRegex(qualify_models.QualificationError, "not started"):
                server._check()
            server.process = FakeProcess()
            server.process.returncode = 9
            with self.assertRaisesRegex(qualify_models.QualificationError, "status 9"):
                server._check()
            server.process.returncode = None
            server.stdout_path.write_bytes(b"x")
            with mock.patch.object(qualify_models, "MAX_LOG_BYTES", 0), self.assertRaisesRegex(
                qualify_models.QualificationError, "log exceeded"
            ):
                server._check()

    def test_non_embedding_timeout_context_and_forced_close_paths(self) -> None:
        class TimeoutProcess(FakeProcess):
            def wait(self, timeout: float) -> int:
                self.wait_calls += 1
                if self.wait_calls == 1:
                    raise subprocess.TimeoutExpired("server", timeout)
                self.returncode = 0
                return 0

        with tempfile.TemporaryDirectory() as temporary:
            logs = Path(temporary)
            process = TimeoutProcess()
            with mock.patch.object(qualify_models, "_choose_port", return_value=12345), mock.patch.object(
                model_assets, "assert_priority_available"
            ), mock.patch.object(qualify_models.subprocess, "Popen", return_value=process) as popen, mock.patch.object(
                qualify_models.time, "monotonic", side_effect=[0.0, 2.0]
            ):
                server = qualify_models.LocalServer(
                    Path("server"), Path("model"), 128, logs, embedding=False
                )
                with self.assertRaisesRegex(qualify_models.QualificationError, "healthy"):
                    server.start(timeout=1)
            self.assertIn("--jinja", popen.call_args.args[0])
            with mock.patch.object(qualify_models, "_process_rss_bytes", return_value=0), mock.patch.object(
                qualify_models.os, "killpg", side_effect=ProcessLookupError
            ):
                server.close()
            self.assertEqual(process.wait_calls, 2)
            empty = qualify_models.LocalServer(
                Path("server"), Path("model"), 1, logs, embedding=False
            )
            empty.close()
            with mock.patch.object(empty, "start") as start, mock.patch.object(empty, "close") as close:
                with empty as entered:
                    self.assertIs(entered, empty)
                start.assert_called_once()
                close.assert_called_once()

    def test_long_request_preempts_and_closes_server_when_erais_appears(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            server = qualify_models.LocalServer(
                Path("server"),
                Path("model"),
                128,
                Path(temporary),
                embedding=False,
            )
            release = qualify_models.threading.Event()

            def blocked_http(*_args, **_kwargs):  # type: ignore[no-untyped-def]
                release.wait(1)
                return b"{}"

            def priority_check() -> None:
                release.set()
                raise model_assets.PriorityBlocked("busy")

            with mock.patch.object(
                qualify_models, "_bounded_http", side_effect=blocked_http
            ), mock.patch.object(
                server, "_check", side_effect=priority_check
            ), mock.patch.object(server, "close") as close, mock.patch.object(
                qualify_models, "HTTP_MONITOR_SECONDS", 0.001
            ), self.assertRaises(model_assets.PriorityBlocked):
                server._monitored_http(
                    "http://127.0.0.1:12345/v1/chat/completions",
                    payload={},
                    timeout=1,
                )
            close.assert_called_once()

            with mock.patch.object(
                qualify_models,
                "_bounded_http",
                side_effect=model_assets.PriorityBlocked("busy"),
            ), mock.patch.object(server, "close") as worker_close, self.assertRaises(
                model_assets.PriorityBlocked
            ):
                server._monitored_http(
                    "http://127.0.0.1:12345/health",
                    timeout=1,
                )
            worker_close.assert_called_once()


if __name__ == "__main__":
    unittest.main()
