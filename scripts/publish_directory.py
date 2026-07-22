#!/usr/bin/env python3
"""Atomically publish one complete RKC release-generation directory."""
from __future__ import annotations

import argparse
import ctypes
import json
import os
import stat
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DIST = ROOT / "dist"
ALLOWED_DESTINATIONS = frozenset({"evidence", "release"})
RENAME_NOREPLACE = 1
RENAME_EXCHANGE = 2


class PublishDirectoryError(RuntimeError):
    """Raised when an atomic generation publication cannot be completed."""


def require_real_tree(path: Path, label: str) -> None:
    """Reject links and special files anywhere in a publication tree."""
    try:
        root_info = os.lstat(path)
    except FileNotFoundError as exc:
        raise PublishDirectoryError(f"{label} is missing: {path}") from exc
    if stat.S_ISLNK(root_info.st_mode) or not stat.S_ISDIR(root_info.st_mode):
        raise PublishDirectoryError(f"{label} must be a real directory: {path}")
    for current, directories, files in os.walk(path, topdown=True, followlinks=False):
        for name in directories:
            item = Path(current) / name
            info = os.lstat(item)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                raise PublishDirectoryError(f"unsafe directory in {label}: {item}")
        for name in files:
            item = Path(current) / name
            info = os.lstat(item)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise PublishDirectoryError(f"unsafe file in {label}: {item}")


def sync_tree_directories(path: Path) -> None:
    """Persist every real directory in one staged tree from leaves to root."""
    require_real_tree(path, "publication source")
    directories = [
        Path(current)
        for current, _names, _files in os.walk(
            path, topdown=False, followlinks=False
        )
    ]
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    for directory in directories:
        try:
            descriptor = os.open(directory, flags)
        except OSError as exc:
            raise PublishDirectoryError(
                f"cannot open staged publication directory for sync: {directory}: {exc}"
            ) from exc
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)


def sync_rename_parents(source_parent_fd: int, destination_parent_fd: int) -> None:
    """Persist both directory entries changed by a cross-directory rename."""
    os.fsync(source_parent_fd)
    os.fsync(destination_parent_fd)


def renameat2(
    source_directory_fd: int,
    source: str,
    destination_directory_fd: int,
    destination: str,
    flags: int,
) -> None:
    """Invoke Linux renameat2 relative to already-open parent directories."""
    libc = ctypes.CDLL(None, use_errno=True)
    rename = getattr(libc, "renameat2", None)
    if rename is None:
        raise PublishDirectoryError("libc renameat2 is unavailable")
    rename.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    rename.restype = ctypes.c_int
    if (
        rename(
            source_directory_fd,
            os.fsencode(source),
            destination_directory_fd,
            os.fsencode(destination),
            flags,
        )
        != 0
    ):
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))


def publish(
    source: Path, destination: Path, repository_root: Path | None = None
) -> str:
    """Publish a whole generation with one rename and rollback on sync failure."""
    if not sys.platform.startswith("linux"):
        raise PublishDirectoryError("atomic generation publication requires Linux")
    source = Path(os.path.abspath(source))
    destination = Path(os.path.abspath(destination))
    root = Path(os.path.abspath(repository_root if repository_root is not None else ROOT))
    dist = root / "dist"
    try:
        relative_source = source.relative_to(dist)
        relative_destination = destination.relative_to(dist)
    except ValueError as exc:
        raise PublishDirectoryError("source and destination must be inside dist") from exc
    if (
        len(relative_source.parts) < 2
        or not relative_source.parts[0].startswith(".rkc-")
        or source.resolve(strict=True) != source
    ):
        raise PublishDirectoryError("source must be inside a private dist staging directory")
    if (
        len(relative_destination.parts) != 1
        or relative_destination.name not in ALLOWED_DESTINATIONS
    ):
        raise PublishDirectoryError("destination must be dist/evidence or dist/release")
    require_real_tree(source, "publication source")
    sync_tree_directories(source)

    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        dist_fd = os.open(dist, flags)
    except OSError as exc:
        raise PublishDirectoryError(f"dist is unavailable or unsafe: {exc}") from exc
    try:
        source_parent_fd = os.open(source.parent, flags)
    except OSError as exc:
        os.close(dist_fd)
        raise PublishDirectoryError(
            f"publication source parent is unavailable or unsafe: {exc}"
        ) from exc
    source_name = source.name
    destination_name = relative_destination.name
    publication = ""
    try:
        # Persist the staging-directory entry itself before it is moved.
        os.fsync(source_parent_fd)
        try:
            destination_info = os.stat(
                destination_name, dir_fd=dist_fd, follow_symlinks=False
            )
        except FileNotFoundError:
            renameat2(
                source_parent_fd,
                source_name,
                dist_fd,
                destination_name,
                RENAME_NOREPLACE,
            )
            publication = "created"
        else:
            if stat.S_ISLNK(destination_info.st_mode) or not stat.S_ISDIR(
                destination_info.st_mode
            ):
                raise PublishDirectoryError("destination must be absent or a real directory")
            require_real_tree(destination, "existing publication")
            renameat2(
                source_parent_fd,
                source_name,
                dist_fd,
                destination_name,
                RENAME_EXCHANGE,
            )
            publication = "replaced"

        try:
            require_real_tree(destination, "published generation")
            sync_rename_parents(source_parent_fd, dist_fd)
        except BaseException:
            try:
                if publication == "replaced":
                    renameat2(
                        source_parent_fd,
                        source_name,
                        dist_fd,
                        destination_name,
                        RENAME_EXCHANGE,
                    )
                elif publication == "created":
                    renameat2(
                        dist_fd,
                        destination_name,
                        source_parent_fd,
                        source_name,
                        RENAME_NOREPLACE,
                    )
                sync_rename_parents(source_parent_fd, dist_fd)
            except BaseException as rollback_error:
                raise PublishDirectoryError(
                    f"publication failed and rollback also failed: {rollback_error}"
                ) from rollback_error
            raise
    finally:
        os.close(source_parent_fd)
        os.close(dist_fd)
    return publication


def main() -> int:
    """Parse arguments and publish one generation."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--destination", required=True, type=Path)
    parser.add_argument(
        "--repository-root",
        type=Path,
        help="repository whose dist directory receives the generation (default: script root)",
    )
    arguments = parser.parse_args()
    try:
        publication = publish(
            arguments.source,
            arguments.destination,
            repository_root=arguments.repository_root,
        )
    except (OSError, PublishDirectoryError) as exc:
        print(f"publish directory: {exc}", file=sys.stderr)
        return 1
    print(
        json.dumps(
            {
                "destination": str(arguments.destination),
                "publication": publication,
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
