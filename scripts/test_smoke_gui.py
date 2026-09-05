"""Failure-path tests for the native GUI qualification driver.

Actual binaries are exercised separately by portable-release. These controlled
responses prove the driver rejects broken startup, authorization, activation,
and cleanup instead of producing a successful release receipt.
"""
import importlib.util
import io
import json
from pathlib import Path
import signal
import subprocess
import unittest
from unittest import mock
import urllib.error

SPEC = importlib.util.spec_from_file_location("rkc_smoke_gui", Path(__file__).with_name("smoke-gui.py"))
GUI = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUI)


class DriverHarness:
    def __init__(self, mode="success"):
        self.mode = mode
        self.status = None
        self.pid = 45678
        self.manifests = 0
        self.terminated = False
        self.killed = False
        self.signaled = False

    def launch(self, args, **kwargs):
        self.workspace = Path(args[-1])
        self.ready = Path(args[-2])
        assert args[1:4] == ["gui", "--no-browser", "--ready-file"]
        if self.mode == "early_exit":
            kwargs["stderr"].write(b"readiness failed")
            self.status = 3
        elif self.mode != "startup_timeout":
            self.ready.write_text(json.dumps({"url": "http://127.0.0.1:12345", "browser_url": "http://127.0.0.1:12345/#rkc-workbench=bootstrap-fixture", "snapshot_id": "rkc:workspace:empty"}))
        if self.mode == "premature_scan":
            (self.workspace / ".rkc").mkdir()
        return self

    def poll(self):
        return self.status

    def terminate(self):
        self.terminated = True
        if self.mode != "stuck_cleanup":
            self.status = 0

    def kill(self):
        self.killed = True
        self.status = -9

    def wait(self, timeout):
        if self.mode == "stuck_cleanup" and not self.killed:
            raise subprocess.TimeoutExpired("fake-gui", timeout)
        return self.status if self.status is not None else 0

    def send_signal(self, selected):
        self.signaled = True
        self.status = 0

    def killpg(self, pid, selected):
        assert pid == self.pid and selected == signal.SIGINT
        self.send_signal(selected)

    def request(self, request, timeout):
        assert request.get_header("Origin") == "http://127.0.0.1:12345"
        path = request.full_url.removeprefix("http://127.0.0.1:12345")
        headers = {key.lower(): value for key, value in request.header_items()}
        if path.startswith("/api/v1/workbench"):
            if path.endswith("/session"):
                assert headers["x-rkc-workbench-bootstrap"] == "bootstrap-fixture"
            elif "x-rkc-workbench-token" not in headers:
                if self.mode == "unauthorized_allowed":
                    return io.BytesIO(b'{}')
                raise urllib.error.HTTPError(request.full_url, 403, "Forbidden", {}, None)
            else:
                assert headers["x-rkc-workbench-token"] == "session-fixture"
        if path == "/api/v1/manifest":
            self.manifests += 1
            if self.mode == "stuck_cleanup":
                raise RuntimeError("failed read")
            body = {"metadata": {"rkc_workspace": "empty"}} if self.manifests == 1 else {"id": "wrong" if self.mode == "wrong_activation" else "snapshot-ready"}
        elif path.endswith("/session"):
            body = {"token": "session-fixture", "folder_compilation_only": False}
        elif path.endswith("/directories"):
            body = {"directories": [] if self.mode == "missing_folder" else [{"name": "knowledge"}]}
        elif path.endswith("/jobs"):
            assert request.get_method() == "POST"
            assert json.loads(request.data) == {"args": ["quickstart", str(self.workspace / "knowledge")]}
            body = {"id": "job-fixture", "status": "queued"}
        elif path.endswith("/jobs/job-fixture"):
            body = {"id": "job-fixture", "status": "failed" if self.mode == "job_failure" else "succeeded", "activated_dataset": {"snapshot_id": "snapshot-ready"}, "error": "compile rejected"}
        elif path == "/api/v1/search?q=Login&limit=10":
            body = {"hits": [] if self.mode == "empty_search" else [{"id": "login"}]}
        else:
            raise AssertionError("unexpected driver request")
        return io.BytesIO(json.dumps(body).encode())

    def run(self):
        with mock.patch.object(GUI.subprocess, "Popen", self.launch), \
             mock.patch.object(GUI.urllib.request, "urlopen", self.request), \
             mock.patch.object(GUI.os, "killpg", self.killpg), \
             mock.patch.object(GUI.time, "sleep"):
            return GUI.smoke(Path("/fake/rkc"))


class GUISmokeDriverTests(unittest.TestCase):
    def test_success_receipt_requires_authenticated_real_flow(self):
        driver = DriverHarness()
        result = driver.run()
        self.assertTrue(result["ok"])
        self.assertIn("graceful_shutdown", result["checks"])
        self.assertTrue(driver.signaled)
        self.assertFalse(driver.terminated)
        self.assertFalse(driver.ready.exists())

    def test_broken_qualification_never_returns_success(self):
        for mode in ("early_exit", "premature_scan", "unauthorized_allowed", "missing_folder", "job_failure", "wrong_activation", "empty_search"):
            with self.subTest(mode=mode):
                driver = DriverHarness(mode)
                with self.assertRaises((AssertionError, RuntimeError)):
                    driver.run()
                self.assertEqual(driver.terminated, mode != "early_exit")

    def test_readiness_and_job_timeouts_stop_the_process(self):
        for mode, clock in (("startup_timeout", [0, 121]), ("job_timeout", [0, 0, 181])):
            with self.subTest(mode=mode), mock.patch.object(GUI.time, "monotonic", side_effect=clock):
                driver = DriverHarness(mode)
                with self.assertRaisesRegex(RuntimeError, "120 seconds|180 seconds"):
                    driver.run()
                self.assertTrue(driver.terminated)

    def test_stuck_cleanup_escalates_to_kill_and_reaps(self):
        driver = DriverHarness("stuck_cleanup")
        with self.assertRaisesRegex(RuntimeError, "failed read"):
            driver.run()
        self.assertTrue(driver.terminated)
        self.assertTrue(driver.killed)

    def test_main_prints_the_verified_receipt(self):
        with mock.patch.object(GUI, "smoke", return_value={"ok": True}) as smoke, \
             mock.patch("sys.argv", ["smoke-gui.py", "--rkc", "./rkc"]), \
             mock.patch("sys.stdout", new_callable=io.StringIO) as output:
            GUI.main()
            self.assertEqual(json.loads(output.getvalue()), {"ok": True})
            self.assertTrue(smoke.call_args.args[0].is_absolute())


if __name__ == "__main__":
    unittest.main()
