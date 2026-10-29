#!/usr/bin/env python3
"""Check Go formatting in small sequential batches and preserve tool failures."""
from __future__ import annotations

import os
import stat
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SOURCE_DIRECTORIES = ("cmd", "internal", "pkg", "storage")
FILES_PER_BATCH = 16
MAXIMUM_DIAGNOSTIC_CHARACTERS = 4096


def source_files(root: Path) -> list[str]:
    """Inventory Go source, refusing unsafe file types without following links."""
    files: list[str] = []

    def walk_error(error: OSError) -> None:
        raise error

    for directory in SOURCE_DIRECTORIES:
        top = root / directory
        if not top.is_dir() or top.is_symlink():
            raise OSError(f"Go source directory unavailable: {directory}")
        for parent, directories, names in os.walk(top, onerror=walk_error):
            # os.walk does not recurse through directory symlinks. A symlink
            # whose name ends in .go would nevertheless be a formatter input.
            names = names + [
                name for name in directories if (Path(parent) / name).is_symlink()
            ]
            for name in names:
                if name.startswith(".") or not name.endswith(".go"):
                    continue
                path = Path(parent) / name
                if not stat.S_ISREG(path.lstat().st_mode):
                    raise OSError(
                        f"Go source must be a regular file: {path.relative_to(root)}"
                    )
                files.append(str(path.relative_to(root)))
    return sorted(files)


def report(message: str, detail: str) -> None:
    """Bound and escape tool diagnostics, including unusual source filenames."""
    escaped = ascii(detail[:MAXIMUM_DIAGNOSTIC_CHARACTERS])
    if (
        len(detail) > MAXIMUM_DIAGNOSTIC_CHARACTERS
        or len(escaped) > MAXIMUM_DIAGNOSTIC_CHARACTERS
    ):
        escaped = escaped[:MAXIMUM_DIAGNOSTIC_CHARACTERS] + " [truncated]"
    print(
        f"{message}: {escaped}",
        file=sys.stderr,
    )


def check_format(root: Path) -> int:
    """Return failure for unformatted source, inventory errors, or tool errors."""
    try:
        files = source_files(root)
    except OSError as error:
        report("Go formatting inventory failed", str(error))
        return 1
    environment = os.environ.copy()
    environment["GOMAXPROCS"] = "1"
    unformatted = False
    for offset in range(0, len(files), FILES_PER_BATCH):
        try:
            result = subprocess.run(
                ["gofmt", "-l", *files[offset:offset + FILES_PER_BATCH]],
                cwd=root, env=environment, text=True, capture_output=True,
                encoding="utf-8", errors="backslashreplace", check=False,
            )
        except OSError as error:
            report("Go formatter could not start", str(error))
            return 1
        if result.returncode != 0:
            report(f"Go formatter failed (exit {result.returncode})", result.stderr)
            return 1
        if result.stdout:
            # Do not split on newlines: those are legal filename characters.
            report("Go files require gofmt", result.stdout)
            unformatted = True
    if unformatted:
        return 1
    print(
        f"Go formatting checked {len(files)} files in sequential batches "
        f"of {FILES_PER_BATCH}."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(check_format(ROOT))
