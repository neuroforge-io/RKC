#!/usr/bin/env python3
"""Fail closed when a commit-bound operation sees uncommitted source."""
from __future__ import annotations

import argparse
import subprocess
from pathlib import Path


class SourceGuardError(RuntimeError):
    """Raised when a Git worktree is unavailable or not release-clean."""


def _git(root: Path, arguments: list[str]) -> subprocess.CompletedProcess[bytes]:
    """Run one bounded, promptless Git inspection command."""
    try:
        return subprocess.run(
            ["git", "-C", str(root), *arguments],
            check=False,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise SourceGuardError(f"cannot inspect Git worktree: {exc}") from exc


def render_status_entry(entry: bytes) -> str:
    """Render arbitrary Git status bytes without terminal control characters."""
    text = entry.decode("utf-8", errors="backslashreplace")
    return text.encode("unicode_escape").decode("ascii")


def require_clean_worktree(root: Path, operation: str) -> None:
    """Require an exact clean Git worktree for a commit-bound operation.

    Ignored build output is permitted because it cannot enter the immutable Git
    source tree. Tracked modifications and non-ignored untracked files are
    rejected so a release command cannot silently package an older ``HEAD``.
    """
    root = root.resolve()
    if not root.is_dir():
        raise SourceGuardError(f"repository root is not a directory: {root}")

    repository = _git(root, ["rev-parse", "--show-toplevel"])
    if repository.returncode != 0:
        detail = repository.stderr.decode("utf-8", errors="replace").strip()
        raise SourceGuardError(f"cannot resolve Git repository: {detail or 'unknown error'}")
    try:
        top_level = Path(repository.stdout.decode("utf-8").strip()).resolve()
    except UnicodeDecodeError as exc:
        raise SourceGuardError("Git repository root is not valid UTF-8") from exc
    if top_level != root:
        raise SourceGuardError(
            f"repository root mismatch: expected {root}, Git resolved {top_level}"
        )

    status = _git(
        root,
        [
            "status",
            "--porcelain=v1",
            "-z",
            "--untracked-files=all",
            "--ignore-submodules=none",
        ],
    )
    if status.returncode != 0:
        detail = status.stderr.decode("utf-8", errors="replace").strip()
        raise SourceGuardError(f"cannot inspect Git status: {detail or 'unknown error'}")
    entries = [entry for entry in status.stdout.split(b"\0") if entry]
    if not entries:
        return

    rendered = [render_status_entry(entry) for entry in entries[:20]]
    if len(entries) > len(rendered):
        rendered.append(f"... and {len(entries) - len(rendered)} more")
    details = "\n  ".join(rendered)
    raise SourceGuardError(
        f"{operation}: Git worktree is dirty; commit or remove source changes first\n"
        f"  {details}"
    )


def parse_args() -> argparse.Namespace:
    """Parse command-line arguments."""
    parser = argparse.ArgumentParser(
        description="Require a clean, exact Git worktree before a commit-bound operation."
    )
    parser.add_argument("--root", required=True, type=Path, help="exact repository root")
    parser.add_argument(
        "--operation",
        required=True,
        help="human-readable operation name used in failures",
    )
    return parser.parse_args()


def main() -> None:
    """Run the clean-worktree guard."""
    args = parse_args()
    try:
        require_clean_worktree(args.root, args.operation)
    except SourceGuardError as exc:
        raise SystemExit(str(exc)) from exc


if __name__ == "__main__":
    main()
