from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

try:
    from scripts import git_source_guard as guard
except ModuleNotFoundError as exc:
    if exc.name != "scripts":
        raise
    import git_source_guard as guard

SourceGuardError = guard.SourceGuardError
require_clean_worktree = guard.require_clean_worktree


class GitSourceGuardTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.git("init", "--quiet")
        self.git("config", "user.email", "rkc-tests@example.invalid")
        self.git("config", "user.name", "RKC tests")
        (self.root / ".gitignore").write_text("ignored-output\n", encoding="utf-8")
        (self.root / "tracked.txt").write_text("committed\n", encoding="utf-8")
        self.git("add", ".gitignore", "tracked.txt")
        self.git("commit", "--quiet", "-m", "test fixture")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def git(self, *arguments: str) -> None:
        subprocess.run(
            ["git", "-C", str(self.root), *arguments],
            check=True,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=30,
        )

    def test_clean_worktree_passes(self) -> None:
        require_clean_worktree(self.root, "test release")

    def test_ignored_output_is_permitted(self) -> None:
        (self.root / "ignored-output").write_text("derived\n", encoding="utf-8")
        require_clean_worktree(self.root, "test release")

    def test_tracked_change_is_rejected(self) -> None:
        (self.root / "tracked.txt").write_text("changed\n", encoding="utf-8")
        with self.assertRaisesRegex(SourceGuardError, "tracked.txt"):
            require_clean_worktree(self.root, "test release")

    def test_untracked_source_is_rejected(self) -> None:
        (self.root / "new-source.txt").write_text("new\n", encoding="utf-8")
        with self.assertRaisesRegex(SourceGuardError, "new-source.txt"):
            require_clean_worktree(self.root, "test release")

    def test_nested_root_is_rejected(self) -> None:
        nested = self.root / "nested"
        nested.mkdir()
        with self.assertRaisesRegex(SourceGuardError, "root mismatch"):
            require_clean_worktree(nested, "test release")

    def test_missing_and_non_repository_roots_are_rejected(self) -> None:
        with self.assertRaisesRegex(SourceGuardError, "not a directory"):
            require_clean_worktree(self.root / "missing", "test release")
        unrelated = Path(tempfile.mkdtemp())
        self.addCleanup(unrelated.rmdir)
        with self.assertRaisesRegex(SourceGuardError, "cannot resolve Git repository"):
            require_clean_worktree(unrelated, "test release")

    def test_git_execution_and_status_failures_are_reported(self) -> None:
        for failure in (OSError("missing Git"), subprocess.TimeoutExpired("git", 30)):
            with self.subTest(failure=type(failure).__name__), mock.patch.object(
                guard.subprocess, "run", side_effect=failure
            ), self.assertRaisesRegex(SourceGuardError, "cannot inspect Git worktree"):
                guard._git(self.root, ["status"])

        repository = subprocess.CompletedProcess(
            ["git"], 0, stdout=(str(self.root) + "\n").encode(), stderr=b""
        )
        bad_status = subprocess.CompletedProcess(
            ["git"], 1, stdout=b"", stderr=b"status failed"
        )
        with mock.patch.object(guard, "_git", side_effect=[repository, bad_status]), self.assertRaisesRegex(
            SourceGuardError, "status failed"
        ):
            require_clean_worktree(self.root, "test release")

    def test_non_utf8_root_and_large_dirty_inventory_are_bounded(self) -> None:
        malformed = subprocess.CompletedProcess(
            ["git"], 0, stdout=b"\xff\n", stderr=b""
        )
        with mock.patch.object(guard, "_git", return_value=malformed), self.assertRaisesRegex(
            SourceGuardError, "valid UTF-8"
        ):
            require_clean_worktree(self.root, "test release")

        for index in range(21):
            (self.root / f"source-{index:02}.txt").write_text("new\n", encoding="utf-8")
        with self.assertRaisesRegex(SourceGuardError, r"\.\.\. and 1 more"):
            require_clean_worktree(self.root, "test release")

    def test_status_control_characters_are_escaped(self) -> None:
        rendered = guard.render_status_entry(b"?? dangerous\x1b[2J\nname\t.py")
        self.assertNotIn("\x1b", rendered)
        self.assertNotIn("\n", rendered)
        self.assertNotIn("\t", rendered)
        self.assertIn(r"\x1b[2J", rendered)
        self.assertIn(r"\n", rendered)
        self.assertIn(r"\t", rendered)

    def test_main_reports_guard_failures(self) -> None:
        arguments = [
            "git_source_guard.py",
            "--root",
            str(self.root),
            "--operation",
            "test release",
        ]
        with mock.patch.object(sys, "argv", arguments):
            guard.main()
        with mock.patch.object(sys, "argv", arguments), mock.patch.object(
            guard, "require_clean_worktree", side_effect=SourceGuardError("dirty")
        ), self.assertRaisesRegex(SystemExit, "dirty"):
            guard.main()


if __name__ == "__main__":
    unittest.main()
