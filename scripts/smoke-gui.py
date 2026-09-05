#!/usr/bin/env python3
"""Exercise an installed RKC GUI through its real authenticated HTTP interface."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def smoke(binary: Path) -> dict[str, object]:
    with tempfile.TemporaryDirectory(prefix="rkc-gui-smoke-") as temporary:
        root = Path(temporary)
        workspace = root / "workspace"
        workspace.mkdir()
        source = workspace / "knowledge"
        source.mkdir()
        (source / "README.md").write_text("# Account guide\nLogin opens the account dashboard.\n", encoding="utf-8")
        (source / "account.go").write_text("package account\n\n// Login opens the account.\nfunc Login() string { return \"ready\" }\n", encoding="utf-8")
        ready = root / "ready.json"
        options = {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {"start_new_session": True}
        with (root / "server.log").open("w+b") as log:
            process = subprocess.Popen([str(binary), "gui", "--no-browser", "--ready-file", str(ready), str(workspace)], stdout=log, stderr=log, **options)
            try:
                deadline = time.monotonic() + 120
                while not ready.exists():
                    if process.poll() is not None:
                        log.seek(0)
                        raise RuntimeError("GUI exited before readiness: " + log.read(8192).decode("utf-8", errors="replace"))
                    if time.monotonic() > deadline:
                        raise RuntimeError("GUI did not become ready within 120 seconds")
                    time.sleep(0.1)
                receipt = json.loads(ready.read_text(encoding="utf-8"))
                origin = receipt["url"].rstrip("/")
                fragment = urllib.parse.parse_qs(urllib.parse.urlsplit(receipt["browser_url"]).fragment)
                capability = fragment["rkc-workbench"][0]
                token = ""

                def request(path: str, payload: dict | None = None, *, bootstrap: bool = False, authenticated: bool = True) -> dict:
                    headers = {"Origin": origin}
                    if bootstrap:
                        headers["X-RKC-Workbench-Bootstrap"] = capability
                    elif authenticated and token:
                        headers["X-RKC-Workbench-Token"] = token
                    data = None if payload is None else json.dumps(payload).encode("utf-8")
                    if data is not None:
                        headers["Content-Type"] = "application/json"
                    with urllib.request.urlopen(urllib.request.Request(origin + path, data=data, headers=headers), timeout=30) as response:
                        return json.load(response)

                assert receipt["snapshot_id"] == "rkc:workspace:empty", "GUI compiled before source selection"
                assert not (workspace / ".rkc").exists() and not (source / ".rkc").exists(), "GUI wrote an atlas before source selection"
                manifest = request("/api/v1/manifest", authenticated=False)
                assert manifest["metadata"]["rkc_workspace"] == "empty"
                try:
                    request("/api/v1/workbench/directories", authenticated=False)
                except urllib.error.HTTPError as error:
                    assert error.code == 403, "unauthenticated folder request returned an unexpected status"
                else:
                    raise AssertionError("folder browser allowed unauthenticated access")
                session = request("/api/v1/workbench/session", bootstrap=True)
                token = session["token"]
                listing = request("/api/v1/workbench/directories")
                assert any(entry["name"] == "knowledge" for entry in listing["directories"]), "folder chooser omitted source"
                job = request("/api/v1/workbench/jobs", {"args": ["quickstart", str(source)]})
                deadline = time.monotonic() + 180
                while job["status"] in ("queued", "running"):
                    if time.monotonic() > deadline:
                        raise RuntimeError("GUI folder compilation exceeded 180 seconds")
                    time.sleep(0.1)
                    job = request("/api/v1/workbench/jobs/" + urllib.parse.quote(job["id"], safe=""))
                assert job["status"] == "succeeded", "GUI folder compilation failed: " + job.get("error", "") + " " + job.get("output", "")[-4096:]
                identity = job["activated_dataset"]
                manifest = request("/api/v1/manifest")
                assert manifest["id"] == identity["snapshot_id"] != "rkc:workspace:empty", "compiled source was not activated"
                result = request("/api/v1/search?q=Login&limit=10")
                assert result["hits"], "activated atlas has no search results"
                if os.name == "nt":
                    process.send_signal(signal.CTRL_BREAK_EVENT)
                else:
                    os.killpg(process.pid, signal.SIGINT)
                assert process.wait(timeout=30) == 0, "GUI failed graceful shutdown"
                return {"schema_version": "rkc-gui-smoke/v1", "ok": True, "folder_compilation_only": session["folder_compilation_only"], "checks": ["empty_welcome", "no_scan_before_selection", "private_session_exchange", "folder_authorization", "folder_browsing", "folder_compilation", "dataset_activation", "search", "graceful_shutdown"]}
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=10)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rkc", required=True, type=Path)
    arguments = parser.parse_args()
    print(json.dumps(smoke(arguments.rkc.resolve()), sort_keys=True))


if __name__ == "__main__":
    main()
