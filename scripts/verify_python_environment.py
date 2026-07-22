#!/usr/bin/env python3
"""Verify required Python and validation-distribution versions."""
from __future__ import annotations

import argparse
import importlib.metadata
import json
import os
import re
import sys
from pathlib import Path
from typing import Callable, Sequence


class PythonEnvironmentError(RuntimeError):
    """Raised when required validation versions differ from the checked-in lock."""


REQUIREMENT_PATTERN = re.compile(
    r"(?P<name>[A-Za-z0-9][A-Za-z0-9._-]*)==(?P<version>[A-Za-z0-9][A-Za-z0-9._+!-]*)"
)


def normalized_name(name: str) -> str:
    """Return the PEP 503 comparison form of one distribution name."""
    return re.sub(r"[-_.]+", "-", name).lower()


def read_lock(path: Path) -> list[tuple[str, str]]:
    """Read an exact, option-free requirements lock."""
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as exc:
        raise PythonEnvironmentError(f"cannot read Python requirements lock: {exc}") from exc

    requirements: list[tuple[str, str]] = []
    seen: set[str] = set()
    for line_number, raw_line in enumerate(lines, start=1):
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        match = REQUIREMENT_PATTERN.fullmatch(line)
        if match is None:
            raise PythonEnvironmentError(
                f"requirements lock line {line_number} is not one exact name==version pin"
            )
        name, version = match.group("name"), match.group("version")
        key = normalized_name(name)
        if key in seen:
            raise PythonEnvironmentError(f"duplicate Python requirement: {name}")
        seen.add(key)
        requirements.append((name, version))
    if not requirements:
        raise PythonEnvironmentError("Python requirements lock is empty")
    return requirements


def verify_environment(
    lock: Path,
    *,
    version_lookup: Callable[[str], str] = importlib.metadata.version,
    python_version: Sequence[int] = sys.version_info,
) -> dict[str, object]:
    """Verify Python and required distributions against one exact version lock."""
    if tuple(python_version[:2]) < (3, 11):
        raise PythonEnvironmentError("Python 3.11 or later is required")

    installed: list[dict[str, str]] = []
    failures: list[str] = []
    for name, expected in read_lock(lock):
        try:
            actual = version_lookup(name)
        except importlib.metadata.PackageNotFoundError:
            failures.append(f"{name} is missing (expected {expected})")
            continue
        except (OSError, ValueError) as exc:
            failures.append(f"{name} cannot be inspected: {exc}")
            continue
        if actual != expected:
            failures.append(f"{name}=={actual} is installed (expected {expected})")
        installed.append({"name": name, "version": actual})
    if failures:
        raise PythonEnvironmentError("; ".join(failures))
    return {
        "ok": True,
        "python": ".".join(str(value) for value in python_version[:3]),
        # Preserve the virtual-environment launcher path. Resolving its final
        # symlink would misleadingly report the base interpreter instead.
        "executable": os.path.abspath(sys.executable),
        "requirements": installed,
    }


def parse_args() -> argparse.Namespace:
    """Parse command-line arguments."""
    parser = argparse.ArgumentParser(
        description="Verify Python and required validation-distribution versions."
    )
    parser.add_argument(
        "--requirements",
        type=Path,
        default=Path("requirements-dev.txt"),
        help="exact requirements lock (default: requirements-dev.txt)",
    )
    return parser.parse_args()


def main() -> None:
    """Validate the current interpreter and print a machine-readable receipt."""
    args = parse_args()
    try:
        receipt = verify_environment(args.requirements)
    except PythonEnvironmentError as exc:
        raise SystemExit(f"Python environment verification failed: {exc}") from exc
    print(json.dumps(receipt, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
