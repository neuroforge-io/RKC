#!/usr/bin/env python3
"""Atomically publish one regular file beneath RKC's reserved dist tree."""
from __future__ import annotations

import argparse
import os
import secrets
import stat
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DIST = ROOT / "dist"
MAXIMUM_BYTES = 1024 * 1024 * 1024


class PublishError(RuntimeError):
    """Raised when a publication boundary or identity check fails."""


def identity(info: os.stat_result) -> tuple[int, int, int, int]:
    """Return the stable fields used to detect replacement and mutation."""
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns)


def destination_identity(parent_fd: int, name: str) -> tuple[int, int, int, int] | None:
    """Inspect an existing destination without following its final component."""
    try:
        info = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode):
        raise PublishError("destination must be absent or a regular file, never a link or directory")
    return identity(info)


def open_destination_parent(destination: Path) -> tuple[int, str]:
    """Open every destination parent component without following links."""
    absolute = Path(os.path.abspath(destination))
    try:
        relative = absolute.relative_to(DIST)
    except ValueError as exc:
        raise PublishError("destination must be inside the repository dist directory") from exc
    if not relative.parts or relative.name in {"", ".", ".."}:
        raise PublishError("destination must name a file below dist")

    flags = os.O_RDONLY
    if hasattr(os, "O_DIRECTORY"):
        flags |= os.O_DIRECTORY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    root_fd = os.open(ROOT, flags)
    opened = [root_fd]
    try:
        current = os.open("dist", flags, dir_fd=root_fd)
        opened.append(current)
        for part in relative.parts[:-1]:
            next_fd = os.open(part, flags, dir_fd=current)
            opened.append(next_fd)
            current = next_fd
        result = os.dup(current)
    except OSError as exc:
        raise PublishError(f"destination parent is unavailable or traverses a link: {exc}") from exc
    finally:
        for descriptor in reversed(opened):
            os.close(descriptor)
    return result, relative.name


def publish(source: Path, destination: Path, mode: int) -> None:
    """Copy through a same-parent inode and atomically replace one safe leaf."""
    if mode not in {0o644, 0o755}:
        raise PublishError("mode must be 0644 or 0755")
    source_flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        source_flags |= os.O_NOFOLLOW
    try:
        source_fd = os.open(source, source_flags)
    except OSError as exc:
        raise PublishError(f"cannot open source without following links: {exc}") from exc
    parent_fd = -1
    temporary_name = ""
    try:
        before = os.fstat(source_fd)
        if not stat.S_ISREG(before.st_mode) or before.st_size > MAXIMUM_BYTES:
            raise PublishError("source must be a bounded regular file")
        parent_fd, destination_name = open_destination_parent(destination)
        original_destination = destination_identity(parent_fd, destination_name)
        if original_destination is not None and (before.st_dev, before.st_ino) == original_destination[:2]:
            raise PublishError("source and destination are the same file")

        for _attempt in range(128):
            candidate = f".rkc-publish-{secrets.token_hex(12)}.tmp"
            flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
            if hasattr(os, "O_NOFOLLOW"):
                flags |= os.O_NOFOLLOW
            try:
                temporary_fd = os.open(candidate, flags, mode, dir_fd=parent_fd)
                temporary_name = candidate
                break
            except FileExistsError:
                continue
        else:
            raise PublishError("could not allocate an exclusive publication inode")

        try:
            with os.fdopen(os.dup(source_fd), "rb") as source_handle, os.fdopen(
                temporary_fd, "wb"
            ) as target_handle:
                copied = 0
                while chunk := source_handle.read(1024 * 1024):
                    copied += len(chunk)
                    if copied > MAXIMUM_BYTES:
                        raise PublishError("source grew beyond the publication limit")
                    target_handle.write(chunk)
                target_handle.flush()
                os.fchmod(target_handle.fileno(), mode)
                os.fsync(target_handle.fileno())
        except Exception:
            try:
                os.close(temporary_fd)
            except OSError:
                pass
            raise

        after = os.fstat(source_fd)
        if identity(after) != identity(before):
            raise PublishError("source changed while it was copied")
        if destination_identity(parent_fd, destination_name) != original_destination:
            raise PublishError("destination changed during publication")
        os.replace(
            temporary_name,
            destination_name,
            src_dir_fd=parent_fd,
            dst_dir_fd=parent_fd,
        )
        temporary_name = ""
        os.fsync(parent_fd)
    finally:
        if temporary_name and parent_fd >= 0:
            try:
                os.unlink(temporary_name, dir_fd=parent_fd)
            except FileNotFoundError:
                pass
        if parent_fd >= 0:
            os.close(parent_fd)
        os.close(source_fd)


def main() -> int:
    """Parse arguments and publish one file."""
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--destination", required=True, type=Path)
    parser.add_argument("--mode", required=True, choices=("0644", "0755"))
    args = parser.parse_args()
    try:
        publish(args.source, args.destination, int(args.mode, 8))
    except (OSError, PublishError) as exc:
        print(f"publish file: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
