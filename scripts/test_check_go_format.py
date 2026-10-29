#!/usr/bin/env python3
"""Regression checks for bounded formatter dispatch and fail-closed exits."""
from __future__ import annotations

import contextlib
import io
import os
import runpy
import subprocess
import tempfile
import unittest
from pathlib import Path
from typing import Callable
from unittest import mock

from scripts import check_go_format as formatter


class GoFormatTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="rkc-format-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        for directory in formatter.SOURCE_DIRECTORIES:
            (self.root / directory).mkdir()

    def write(self, relative: str) -> None:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("package fixture\n", encoding="utf-8")

    def invoke(
        self, result: subprocess.CompletedProcess[str] | OSError
    ) -> tuple[int, str, mock.Mock]:
        output = io.StringIO()
        with (
            mock.patch.object(formatter.subprocess, "run") as runner,
            contextlib.redirect_stdout(output),
            contextlib.redirect_stderr(output),
        ):
            if isinstance(result, OSError):
                runner.side_effect = result
            else:
                runner.return_value = result
            status = formatter.check_format(self.root)
        return status, output.getvalue(), runner

    def test_dispatches_sorted_small_batches_without_shell_or_writes(self) -> None:
        for number in range(35):
            self.write(f"internal/z{number:02}.go")
        self.write("cmd/with space\nand$(false).go")
        self.write("pkg/.hidden.go")
        self.write("storage/.hidden/visible.go")
        self.write("internal/ignore.txt")
        status, _, runner = self.invoke(subprocess.CompletedProcess([], 0, "", ""))
        self.assertEqual(status, 0)
        batches = [call.args[0][2:] for call in runner.call_args_list]
        expected = formatter.source_files(self.root)
        self.assertEqual([path for batch in batches for path in batch], expected)
        self.assertEqual([len(batch) for batch in batches], [16, 16, 5])
        self.assertIn("cmd/with space\nand$(false).go", expected)
        self.assertNotIn("pkg/.hidden.go", expected)
        self.assertIn("storage/.hidden/visible.go", expected)
        for call in runner.call_args_list:
            self.assertEqual(call.args[0][:2], ["gofmt", "-l"])
            self.assertNotIn("-w", call.args[0])
            self.assertFalse(call.kwargs.get("shell", False))
            self.assertEqual(call.kwargs["cwd"], self.root)
            self.assertEqual(call.kwargs["env"]["GOMAXPROCS"], "1")

    def test_nonzero_tool_exit_with_empty_stdout_cannot_pass(self) -> None:
        self.write("cmd/a.go")
        status, output, runner = self.invoke(
            subprocess.CompletedProcess([], 2, "", "parse error\n")
        )
        self.assertEqual(status, 1)
        self.assertIn("exit 2", output)
        self.assertIn("parse error\\n", output)
        self.assertEqual(runner.call_count, 1)

    def test_unformatted_output_fails_and_escapes_special_filenames(self) -> None:
        self.write("cmd/a.go")
        status, output, _ = self.invoke(
            subprocess.CompletedProcess([], 0, "cmd/line\nbreak.go\x1b\n", "")
        )
        self.assertEqual(status, 1)
        self.assertIn("line\\nbreak", output)
        self.assertNotIn("\x1b", output)

    def test_missing_formatter_or_source_root_fails_before_further_work(self) -> None:
        self.write("cmd/a.go")
        status, output, _ = self.invoke(FileNotFoundError("missing gofmt"))
        self.assertEqual(status, 1)
        self.assertIn("could not start", output)
        (self.root / "storage").rmdir()
        status, output, runner = self.invoke(subprocess.CompletedProcess([], 0, "", ""))
        self.assertEqual(status, 1)
        self.assertIn("inventory failed", output)
        runner.assert_not_called()

    def test_walk_errors_propagate_instead_of_omitting_source(self) -> None:
        def failed_walk(
            top: Path, *, onerror: Callable[[OSError], None]
        ) -> list[object]:
            onerror(PermissionError("unreadable fixture"))
            return []

        with mock.patch.object(formatter.os, "walk", side_effect=failed_walk):
            status, output, runner = self.invoke(
                subprocess.CompletedProcess([], 0, "", "")
            )
        self.assertEqual(status, 1)
        self.assertIn("inventory failed", output)
        runner.assert_not_called()

    @unittest.skipUnless(hasattr(os, "symlink"), "symlink support required")
    def test_source_symlinks_refuse_inventory_without_following_them(self) -> None:
        self.write("cmd/a.go")
        try:
            (self.root / "cmd/link.go").symlink_to(self.root / "cmd/a.go")
            (self.root / "pkg/link").symlink_to(
                self.root / "cmd", target_is_directory=True
            )
        except OSError as error:
            self.skipTest(f"symlink permission unavailable: {error}")
        status, output, runner = self.invoke(subprocess.CompletedProcess([], 0, "", ""))
        self.assertEqual(status, 1)
        self.assertIn("regular file", output)
        runner.assert_not_called()
        (self.root / "cmd/link.go").unlink()
        self.assertEqual(formatter.source_files(self.root), ["cmd/a.go"])

    @unittest.skipUnless(hasattr(os, "mkfifo"), "FIFO support required")
    def test_special_go_file_refuses_without_opening_it(self) -> None:
        os.mkfifo(self.root / "cmd/pipe.go")
        status, output, runner = self.invoke(subprocess.CompletedProcess([], 0, "", ""))
        self.assertEqual(status, 1)
        self.assertIn("regular file", output)
        runner.assert_not_called()

    def test_diagnostics_remain_bounded_after_escaping(self) -> None:
        self.write("cmd/a.go")
        status, output, _ = self.invoke(
            subprocess.CompletedProcess([], 2, "", "\x1b" * 10000)
        )
        self.assertEqual(status, 1)
        self.assertIn("[truncated]", output)
        self.assertNotIn("\x1b", output)
        self.assertLess(len(output), formatter.MAXIMUM_DIAGNOSTIC_CHARACTERS + 100)

    def test_cli_preserves_checker_exit(self) -> None:
        result = subprocess.CompletedProcess([], 2, "", "fixture formatter failure")
        with (
            mock.patch.object(formatter.subprocess, "run", return_value=result),
            contextlib.redirect_stderr(io.StringIO()),
        ):
            with self.assertRaises(SystemExit) as raised:
                runpy.run_path(str(Path(formatter.__file__)), run_name="__main__")
        self.assertEqual(raised.exception.code, 1)


if __name__ == "__main__":
    unittest.main()
