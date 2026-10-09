#!/usr/bin/env python3
"""Exercise the real CLI/MCP with invented documents and a loopback mock.

This is an integration receipt, not a model-quality benchmark. It never opens
an existing collection or contacts a remote model. All collection input is
authored below and compiled in a new temporary directory.
"""

from __future__ import annotations

import argparse
import hashlib
import http.client
import ipaddress
import json
import re
import selectors
import shutil
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import cast
from urllib.parse import urlsplit


def require(condition: bool, message: str) -> None:
    """Enforce a receipt check even when Python optimization is enabled."""
    if not condition:
        raise RuntimeError(message)


def fictional_handbook(days: int) -> str:
    """Return the complete fictional source for one workflow generation."""
    return (
        "# Lantern library handbook\n\n"
        "This library is wholly fictional test material.\n\n"
        "## Lantern lending period\n\n"
        f"Lantern lending period is {days} days.\n"
        "Lantern renewals are allowed once.\n\n"
        "## Lantern opening hours\n\n"
        "Lantern opens at 09:00 on Tuesday.\n"
    )


class FictionalModel(BaseHTTPRequestHandler):
    """Copy one admitted fictional sentence; perform no real inference."""

    calls: list[dict[str, object]] = []

    def log_message(self, format: str, *args: object) -> None:
        """Keep HTTP request logs out of the machine-readable receipt."""

    def do_POST(self) -> None:
        """Return cited JSON based only on the request's evidence excerpts."""
        if self.path != "/v1/chat/completions" or self.headers.get("Authorization"):
            self.send_error(400)
            return
        size = int(self.headers.get("Content-Length", "0"))
        if size < 1 or size > 512 * 1024:
            self.send_error(413)
            return
        payload = json.loads(self.rfile.read(size))
        prompt = payload["messages"][-1]["content"]
        packet_text = prompt.split("BEGIN_UNTRUSTED_REPOSITORY_DATA\n", 1)[1]
        packet = json.loads(packet_text.split("\nEND_UNTRUSTED_REPOSITORY_DATA", 1)[0])
        claims = []
        for excerpt in packet.get("source_excerpts", []):
            match = re.search(
                r"Lantern lending period is (\d+) days\.", excerpt["text"]
            )
            if match:
                claims.append(
                    {
                        "text": match.group(0),
                        "category": "constraint",
                        "certainty": "supported",
                        "evidence_ids": [excerpt["evidence_id"]],
                    }
                )
                break
        type(self).calls.append(
            {
                "model": payload["model"],
                "prompt_bytes": len(prompt.encode()),
                "prompt_digest": "sha256:"
                + hashlib.sha256(prompt.encode()).hexdigest(),
                "snapshot_id": packet["snapshot_id"],
                "claim": claims[0]["text"] if claims else None,
            }
        )
        content = json.dumps(
            {
                "claims": claims,
                "unresolved_questions": [] if claims else ["Missing lending period."],
            }
        )
        body = json.dumps(
            {
                "object": "chat.completion",
                "model": "fictional-mock",
                "choices": [
                    {
                        "index": 0,
                        "finish_reason": "stop",
                        "message": {"role": "assistant", "content": content},
                    }
                ],
                "usage": {
                    "prompt_tokens": 0,
                    "completion_tokens": 0,
                    "total_tokens": 0,
                },
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class FictionalNativeModel(FictionalModel):
    """Copy an admitted candidate using the restricted native plaintext shape."""

    def do_POST(self) -> None:
        """Reject schema/temperature fields, then quote one fictional candidate."""
        if self.path != "/v1/chat/completions" or self.headers.get("Authorization"):
            self.send_error(400)
            return
        size = int(self.headers.get("Content-Length", "0"))
        if size < 1 or size > 512 * 1024:
            self.send_error(413)
            return
        payload = json.loads(self.rfile.read(size))
        required = {"model", "messages", "max_tokens", "stream", "n"}
        messages = payload.get("messages", [])
        if (
            set(payload) != required
            or len(messages) != 1
            or messages[0].get("role") != "user"
            or payload["max_tokens"] > 128
            or payload["stream"] is not False
            or payload["n"] != 1
        ):
            self.send_error(400)
            return
        prompt = messages[0]["content"]
        if len(prompt.encode()) > 2048 or not prompt.startswith(
            "RKC_EXTRACTIVE_PROTOCOL 1\n"
        ):
            self.send_error(400)
            return
        packet = json.loads(prompt[prompt.index("{") :])
        candidates = packet["candidates"]
        matched = [
            value
            for value in candidates
            if re.fullmatch(r"Lantern lending period is \d+ days\.", value["text"])
        ]
        if len(matched) != 1:
            self.send_error(400)
            return
        claim = matched[0]["text"]
        type(self).calls.append(
            {
                "model": payload["model"],
                "prompt_bytes": len(prompt.encode()),
                "prompt_digest": "sha256:"
                + hashlib.sha256(prompt.encode()).hexdigest(),
                "evidence_id": matched[0]["evidence_id"],
                "claim": claim,
                "request_keys": sorted(payload),
            }
        )
        body = json.dumps(
            {
                "object": "chat.completion",
                "model": "fictional-mock",
                "choices": [
                    {
                        "index": 0,
                        "finish_reason": "stop",
                        "message": {"role": "assistant", "content": claim},
                    }
                ],
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def run(binary: Path, args: list[str], cwd: Path) -> tuple[str, int]:
    """Execute one bounded CLI action without a shell or ambient source input."""
    started = time.monotonic()
    result = subprocess.run(
        [str(binary), *args],
        cwd=cwd,
        text=True,
        capture_output=True,
        timeout=120,
        check=False,
    )
    require(result.returncode == 0, f"{args[0]} failed: {result.stderr[:2000]}")
    return result.stdout, round((time.monotonic() - started) * 1000)


def mcp_evidence(binary: Path, atlas: Path, evidence_id: str) -> dict[str, object]:
    """Inspect a cited source through the actual stdio MCP server."""
    requests = [
        {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}},
        {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "tools/call",
            "params": {
                "name": "rkc.get_evidence",
                "arguments": {"evidence_id": evidence_id},
            },
        },
    ]
    result = subprocess.run(
        [str(binary), "--dir", str(atlas)],
        input="".join(json.dumps(value) + "\n" for value in requests),
        text=True,
        capture_output=True,
        timeout=30,
        check=True,
    )
    responses = [json.loads(line) for line in result.stdout.splitlines()]
    response = next(value for value in responses if value.get("id") == 2)
    require(
        "error" not in response and not response["result"].get("isError"),
        "workflow validation failed at original line 122",
    )
    return cast(dict[str, object], response["result"]["structuredContent"])


def document_references(
    packet: dict[str, object], mcp: Path, atlas: Path
) -> dict[str, object]:
    """Resolve the source-document's exact canonical references through MCP."""
    documents = [row for row in packet["items"] if row["object_type"] == "document"]
    require(len(documents) == 1, "Expected one fictional source document")
    document = documents[0]
    source = document.get("source", {})
    require(
        source.get("path") == "handbook.md"
        and source.get("start_line") == 1
        and source.get("end_line") == 12
        and "anchor" not in source,
        "Document context lost its complete canonical source range",
    )
    ids = document["evidence_ids"]
    require(
        len(ids) == 4 and ids == sorted(set(ids)),
        "Document evidence must include the whole document and three sections",
    )
    records = {identity: mcp_evidence(mcp, atlas, identity) for identity in ids}
    require(
        all(
            record["id"] == identity
            and record["source"]["artifact_id"] == source["artifact_id"]
            and record["source"]["path"] == source["path"]
            for identity, record in records.items()
        ),
        "Context document evidence did not resolve to its canonical owner",
    )
    return {"item": document, "resolved_evidence": records}


def export_import_context(
    rkc: Path, root: Path, snapshot: str, expected: dict[str, object], revision: int
) -> dict[str, object]:
    """Re-export immutable data, relocate it, and reject tampered import data."""
    exported = root / f"export-v{revision}"
    run(
        rkc,
        [
            "snapshots",
            "export",
            "--state-dir",
            str(root / "state"),
            "--out",
            str(exported),
            snapshot,
        ],
        root,
    )
    imported = root / f"import-v{revision}"
    shutil.copytree(exported, imported)
    # Temporarily relocate only this harness's newly authored fictional source.
    # Import must use immutable exported data without the original source path.
    source_directory = root / "fictional-source"
    hidden_source = root / f"source-hidden-v{revision}"
    source_directory.rename(hidden_source)
    try:
        output, _ = run(
            rkc, ["context", "--dir", str(imported), "Lantern lending period"], root
        )
    finally:
        hidden_source.rename(source_directory)
    packet = json.loads(output)
    require(packet["snapshot_id"] == snapshot, "Import changed snapshot identity")
    rows = [row for row in packet["items"] if row["object_type"] == "document"]
    require(len(rows) == 1, "Relocated export lost its source document")
    fields = (
        "object_id",
        "object_type",
        "citation_id",
        "text",
        "source",
        "evidence_ids",
    )
    require(
        all(rows[0][field] == expected[field] for field in fields),
        "Export/import changed canonical document identity, content or references",
    )
    # This corrupts newly generated fictional export data only. Load must reject
    # the stale manifest; it must not silently rebuild authority from tampering.
    bundle = imported / "bundle.json"
    bundle.write_bytes(bundle.read_bytes() + b"\n")
    result = subprocess.run(
        [str(rkc), "context", "--dir", str(imported), "Lantern lending period"],
        cwd=root,
        text=True,
        capture_output=True,
        timeout=30,
        check=False,
    )
    require(result.returncode != 0, "Tampered export was admitted")
    return {
        "normalized_source_files_included": False,
        "source_directory_hidden_during_import": True,
        "export_search_projection_may_read_fictional_source": True,
        "relocated_snapshot_id": snapshot,
        "document": rows[0],
        "canonical_fields_preserved": list(fields),
        "tampered_export_rejected": True,
    }


def mcp_http_workflow(
    binary: Path, atlas: Path, evidence_id: str, days: int
) -> dict[str, object]:
    """Test the real local Streamable HTTP binary with fictional evidence."""
    process = subprocess.Popen(
        [str(binary), "--dir", str(atlas), "--transport", "http"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        text=True,
    )
    require(process.stderr is not None, "HTTP MCP startup output unavailable")
    stderr = process.stderr
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(stderr, selectors.EVENT_READ)
            require(bool(selector.select(timeout=15)), "HTTP MCP startup timed out")
            startup = stderr.readline()
        match = re.fullmatch(
            r"rkc-mcp: listening on (http://\S+/mcp) "
            r"\(snapshot ([^;]+); credential-free local only\)\n",
            startup,
        )
        require(match is not None, "Unexpected HTTP MCP startup: " + startup[:1000])
        endpoint, snapshot = match.group(1), match.group(2)
        parsed = urlsplit(endpoint)
        require(
            parsed.hostname is not None
            and ipaddress.ip_address(parsed.hostname).is_loopback
            and parsed.port is not None
            and parsed.path == "/mcp",
            "HTTP MCP endpoint is not numeric loopback",
        )

        def request(message: dict[str, object]) -> dict[str, object]:
            connection = http.client.HTTPConnection(
                parsed.hostname, parsed.port, timeout=10
            )
            try:
                connection.request(
                    "POST",
                    "/mcp",
                    body=json.dumps(message),
                    headers={
                        "Content-Type": "application/json",
                        "Accept": "application/json, text/event-stream",
                        "MCP-Protocol-Version": "2025-11-25",
                    },
                )
                response = connection.getresponse()
                data = response.read(4 * 1024 * 1024 + 1)
                require(len(data) <= 4 * 1024 * 1024, "HTTP MCP response too large")
                if "id" not in message:
                    require(
                        response.status == 202 and data == b"", "Notification rejected"
                    )
                    return {"http_status": 202}
                require(response.status == 200, "HTTP MCP request rejected")
                value = json.loads(data)
                require("error" not in value, "HTTP MCP returned protocol error")
                result = value["result"]
                require(not result.get("isError", False), "HTTP MCP tool failed")
                return cast(dict[str, object], result)
            finally:
                connection.close()

        initialized = request(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-11-25",
                    "capabilities": {},
                    "clientInfo": {"name": "rkc-fictional-local", "version": "1"},
                },
            }
        )
        require(initialized["protocolVersion"] == "2025-11-25", "Version mismatch")
        notification = request(
            {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}}
        )
        tools = request({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
        require(
            all(tool["annotations"]["readOnlyHint"] for tool in tools["tools"]),
            "HTTP MCP advertised writable tools",
        )

        def tool(
            name: str, arguments: dict[str, object], identity: int
        ) -> dict[str, object]:
            result = request(
                {
                    "jsonrpc": "2.0",
                    "id": identity,
                    "method": "tools/call",
                    "params": {"name": name, "arguments": arguments},
                }
            )
            return cast(dict[str, object], result["structuredContent"])

        search = tool("rkc.search", {"query": "Lantern lending period"}, 3)
        require("handbook.md" in json.dumps(search), "HTTP search missed handbook")
        context = tool("rkc.context", {"query": "Lantern lending period"}, 4)
        require(
            context["integrity"] == "verified"
            and context["snapshot_id"] == snapshot
            and any(f"is {days} days" in item["text"] for item in context["items"]),
            "HTTP context did not preserve fictional source/snapshot",
        )
        evidence = tool("rkc.get_evidence", {"evidence_id": evidence_id}, 5)
        require(evidence["source"]["path"] == "handbook.md", "HTTP evidence changed")
        return {
            "transport": "stateless Streamable HTTP; loopback JSON",
            "initialize": initialized,
            "initialized_notification": notification,
            "tools": tools,
            "search": search,
            "context": context,
            "evidence": evidence,
            "snapshot_id": snapshot,
            "authentication": "none; no credentials",
        }
    finally:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
        stderr.close()


def exercise(rkc: Path, mcp: Path, profile: str) -> dict[str, object]:
    """Ingest, retrieve, inspect, update, repeat and check safe abstention."""
    handler = (
        FictionalNativeModel
        if profile == "neuroforge-native-extractive"
        else FictionalModel
    )
    handler.calls = []
    receipt: dict[str, object] = {
        "schema": "rkc-synthetic-model-workflow/v1",
        "data": "wholly fictional Lantern library",
        "inference": "loopback mock; no real model quality claim",
        "remote_requests": 0,
        "authentication": "none; no credentials read",
        "api_cost": 0,
        "endpoint_profile": profile,
    }
    server = HTTPServer(("127.0.0.1", 0), handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        with tempfile.TemporaryDirectory(prefix="rkc-fictional-") as directory:
            root = Path(directory)
            source = root / "fictional-source"
            source.mkdir()
            handbook = source / "handbook.md"
            handbook.write_text(fictional_handbook(14))
            stages = []
            snapshots = []
            for revision, days in ((1, 14), (2, 21)):
                if revision == 2:
                    handbook.write_text(fictional_handbook(days))
                atlas = root / f"atlas-v{revision}"
                _, elapsed = run(
                    rkc,
                    [
                        "scan",
                        "--no-python",
                        "--no-git-metadata",
                        "--no-cache",
                        "--runs-dir",
                        str(root / "runs"),
                        "--out",
                        str(atlas),
                        "--state-dir",
                        str(root / "state"),
                        str(source),
                    ],
                    root,
                )
                query, query_ms = run(
                    rkc,
                    ["query", "--dir", str(atlas), "--json", "Lantern lending period"],
                    root,
                )
                context, context_ms = run(
                    rkc,
                    ["context", "--dir", str(atlas), "Lantern lending period"],
                    root,
                )
                query_result = json.loads(query)
                require(
                    any(
                        hit["document"]["path"] == "handbook.md"
                        for hit in query_result["hits"]
                    ),
                    "query omitted the fictional handbook",
                )
                packet = json.loads(context)
                require(
                    any(f"is {days} days" in item["text"] for item in packet["items"]),
                    "workflow validation failed at original line 161",
                )
                require(
                    packet["integrity"] == "verified" and packet["items"],
                    "workflow validation failed at original line 162",
                )
                document_sources = document_references(packet, mcp, atlas)
                portable_context = export_import_context(
                    rkc, root, packet["snapshot_id"], document_sources["item"], revision
                )
                markdown, _ = run(
                    rkc,
                    [
                        "context",
                        "--dir",
                        str(atlas),
                        "--format",
                        "markdown",
                        "Lantern lending period",
                    ],
                    root,
                )
                require(
                    "handbook.md" in markdown and f"is {days} days" in markdown,
                    "workflow validation failed at original line 164",
                )
                answer_args = [
                    "answer",
                    "--dir",
                    str(atlas),
                    "--provider",
                    "openai-compatible",
                    "--endpoint",
                    f"http://127.0.0.1:{server.server_port}/v1/chat/completions",
                    "--model-name",
                    "fictional-mock",
                    "--endpoint-profile",
                    profile,
                    "--timeout",
                    "10s",
                    "--json",
                ]
                if profile == "neuroforge-native-extractive":
                    answer_args.extend(["--context", "512", "--max-output", "128"])
                output, answer_ms = run(
                    rkc, [*answer_args, "Lantern lending period"], root
                )
                answer = json.loads(output)
                require(
                    answer["provenance"]["prompt_digest"]
                    == handler.calls[-1]["prompt_digest"],
                    "Answer audit differs from transmitted mock prompt",
                )
                require(
                    answer["status"] == "answered",
                    "workflow validation failed at original line 172",
                )
                require(
                    answer["claims"][0]["text"]
                    == f"Lantern lending period is {days} days.",
                    "workflow validation failed at original line 173",
                )
                citation = answer["citations"][0]
                evidence = mcp_evidence(mcp, atlas, citation["evidence_id"])
                http_workflow = mcp_http_workflow(
                    mcp, atlas, citation["evidence_id"], days
                )
                require(
                    http_workflow["evidence"] == evidence, "HTTP/stdio evidence differs"
                )
                require(http_workflow["context"] == packet, "HTTP/CLI context differs")
                source_range = evidence["source"]
                require(
                    source_range["path"] == "handbook.md",
                    "workflow validation failed at original line 177",
                )
                lines = handbook.read_text().splitlines()
                cited_text = "\n".join(
                    lines[source_range["start_line"] - 1 : source_range["end_line"]]
                )
                require(
                    answer["claims"][0]["text"] in cited_text,
                    "workflow validation failed at original line 180",
                )
                snapshots.append(packet["snapshot_id"])
                calls_before = len(handler.calls)
                missing, _ = run(rkc, [*answer_args, "zorblaxquux"], root)
                abstention = json.loads(missing)
                require(
                    abstention["status"] == "abstained"
                    and not abstention.get("claims"),
                    "workflow validation failed at original line 185",
                )
                require(
                    len(handler.calls) == calls_before,
                    "workflow validation failed at original line 186",
                )
                stages.append(
                    {
                        "revision": revision,
                        "source": fictional_handbook(days),
                        "scan_ms": elapsed,
                        "query_ms": query_ms,
                        "context_ms": context_ms,
                        "answer_ms": answer_ms,
                        "query": query_result,
                        "context": packet,
                        "document_references": document_sources,
                        "export_import": portable_context,
                        "context_markdown": markdown,
                        "answer": answer,
                        "inspected_evidence": evidence,
                        "mcp_http": http_workflow,
                        "inspected_source": cited_text,
                        "missing_evidence": abstention,
                        "missing_evidence_model_calls": 0,
                    }
                )
            require(
                snapshots[0] != snapshots[1],
                "workflow validation failed at original line 195",
            )
            difference, _ = run(
                rkc,
                [
                    "diff",
                    "--format",
                    "json",
                    str(root / "atlas-v1"),
                    str(root / "atlas-v2"),
                ],
                root,
            )
            diff_result = json.loads(difference)
            require(
                diff_result["summary"]["artifacts_modified"] == 1,
                "diff failed to detect the changed handbook",
            )
            require(
                diff_result["from_snapshot"] == snapshots[0]
                and diff_result["to_snapshot"] == snapshots[1],
                "diff snapshot provenance changed",
            )
            old_context, _ = run(
                rkc,
                ["context", "--dir", str(root / "atlas-v1"), "Lantern lending period"],
                root,
            )
            require(
                any(
                    "is 14 days" in item["text"]
                    for item in json.loads(old_context)["items"]
                ),
                "old snapshot changed after source update",
            )
            receipt.update(
                {
                    "old_snapshot_retained": True,
                    "old_snapshot_context_after_update": json.loads(old_context),
                    "status": "passed",
                    "stages": stages,
                    "snapshots_changed": True,
                    "diff": diff_result,
                    "model_requests": handler.calls,
                }
            )
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=5)
    return receipt


def main() -> None:
    """Run the fictional workflow and print one JSON receipt."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rkc", required=True, type=Path)
    parser.add_argument("--rkc-mcp", required=True, type=Path)
    parser.add_argument(
        "--endpoint-profile",
        choices=("structured-claims", "neuroforge-native-extractive"),
        default="structured-claims",
    )
    args = parser.parse_args()
    print(
        json.dumps(
            exercise(
                args.rkc.resolve(strict=True),
                args.rkc_mcp.resolve(strict=True),
                args.endpoint_profile,
            ),
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
