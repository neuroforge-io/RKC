#!/usr/bin/env python3
"""Validate RKC license boundaries and release-notice completeness."""
from __future__ import annotations

import json
import os
import re
import stat
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ERRORS: list[str] = []
CHECKS: list[dict[str, object]] = []
REQUIRED_FILES = (
    Path("LICENSE"),
    Path("NOTICE"),
    Path("THIRD_PARTY_NOTICES.md"),
    Path("LICENSES/Go.txt"),
)
PROHIBITED_TRACKED_SUFFIXES = frozenset(
    {
        ".a",
        ".bin",
        ".ckpt",
        ".dll",
        ".dylib",
        ".exe",
        ".ggml",
        ".gguf",
        ".h5",
        ".hdf5",
        ".lib",
        ".model",
        ".o",
        ".obj",
        ".onnx",
        ".pt",
        ".pth",
        ".safetensors",
        ".so",
        ".tflite",
        ".wasm",
        ".weights",
    }
)
SIMPLE_SPDX = re.compile(r"^[A-Za-z0-9][A-Za-z0-9.+-]*(?: WITH [A-Za-z0-9.+-]+)?$")


def record(name: str, ok: bool, detail: str = "") -> None:
    """Record one deterministic validation result."""
    CHECKS.append({"name": name, "ok": ok, "detail": detail})
    if not ok:
        ERRORS.append(f"{name}: {detail}")


def read_regular(relative: Path, maximum_bytes: int = 1024 * 1024) -> str | None:
    """Read a bounded, non-symlink repository file."""
    path = ROOT / relative
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        record(str(relative), False, "missing")
        return None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        record(str(relative), False, "must be a regular file, not a link")
        return None
    if info.st_size > maximum_bytes:
        record(str(relative), False, f"exceeds {maximum_bytes} bytes")
        return None
    try:
        value = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as exc:
        record(str(relative), False, f"cannot read UTF-8 text: {exc}")
        return None
    record(str(relative), True, f"{info.st_size} bytes")
    return value


def require_markers(label: str, value: str | None, markers: tuple[str, ...]) -> None:
    """Require each exact marker in a previously read document."""
    if value is None:
        return
    missing = [marker for marker in markers if marker not in value]
    record(label, not missing, "missing: " + ", ".join(missing) if missing else "")


def validate_root_documents() -> None:
    """Validate the project license, notice, and third-party terms."""
    documents = {relative: read_regular(relative) for relative in REQUIRED_FILES}
    require_markers(
        "Apache-2.0 license text",
        documents[Path("LICENSE")],
        (
            "Apache License",
            "Version 2.0, January 2004",
            "END OF TERMS AND CONDITIONS",
        ),
    )
    require_markers(
        "RKC NOTICE",
        documents[Path("NOTICE")],
        ("Repository Knowledge Compiler (RKC)", "Copyright 2026 RKC contributors"),
    )
    require_markers(
        "third-party inventory",
        documents[Path("THIRD_PARTY_NOTICES.md")],
        (
            "RKC-owned source code",
            "Apache-2.0",
            "Go runtime and standard library",
            "BSD-3-Clause",
            "LICENSES/Go.txt",
            "do not bundle model weights",
            "llama.cpp",
            "MIT licensed",
            "Qwen3.5-2B",
            "Qwen3-Embedding-0.6B",
            "models/models.lock.json",
        ),
    )
    require_markers(
        "Go redistribution terms",
        documents[Path("LICENSES/Go.txt")],
        (
            "SPDX-License-Identifier: BSD-3-Clause",
            "Copyright 2009 The Go Authors.",
            "Redistributions in binary form must reproduce",
            "Neither the name of Google LLC",
            "Additional IP Rights Grant (Patents)",
            "Google hereby grants to You a perpetual",
        ),
    )

    third_party = documents[Path("THIRD_PARTY_NOTICES.md")] or ""
    license_directory = ROOT / "LICENSES"
    listed: list[str] = []
    if license_directory.is_dir() and not license_directory.is_symlink():
        for path in sorted(license_directory.rglob("*")):
            if path.is_file() and not path.is_symlink():
                relative = path.relative_to(ROOT).as_posix()
                listed.append(relative)
    missing_notices = [relative for relative in listed if relative not in third_party]
    record(
        "license-file notice closure",
        bool(listed) and not missing_notices,
        "unlisted: " + ", ".join(missing_notices) if missing_notices else "",
    )


def validate_declared_metadata() -> None:
    """Validate public API and official plugin license declarations."""
    openapi = read_regular(Path("api/openapi.yaml"))
    require_markers(
        "implemented OpenAPI license metadata",
        openapi,
        ("license:", "name: Apache-2.0", "identifier: Apache-2.0"),
    )

    plugin_failures: list[str] = []
    for path in sorted((ROOT / "plugins").glob("*/plugin.json")):
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
            identity = document["plugin"]
            plugin_id = identity["id"]
            expression = identity["license"]
            if not isinstance(expression, str) or not SIMPLE_SPDX.fullmatch(expression):
                plugin_failures.append(f"{plugin_id}: invalid simple SPDX expression")
            if isinstance(plugin_id, str) and plugin_id.startswith("rkc.") and expression != "Apache-2.0":
                plugin_failures.append(f"{plugin_id}: official RKC plugin is not Apache-2.0")
        except (KeyError, OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            plugin_failures.append(f"{path.relative_to(ROOT)}: {exc}")
    record(
        "official plugin license metadata",
        not plugin_failures,
        "; ".join(plugin_failures),
    )

    model_lock = read_regular(Path("models/models.lock.json"))
    model_failures: list[str] = []
    if model_lock is not None:
        try:
            document = json.loads(model_lock)
            llama = document["llama_cpp"]
            if llama["license_spdx"] != "MIT":
                model_failures.append("llama.cpp is not recorded as MIT")
            assets = document["assets"]
            for asset in assets:
                kind = asset["kind"]
                expression = asset["license_spdx"]
                if kind == "source-archive" and expression != "MIT":
                    model_failures.append(f"{asset['id']}: source license is not MIT")
                if kind in ("generation-model", "embedding-model") and expression != "Apache-2.0":
                    model_failures.append(f"{asset['id']}: model license is not Apache-2.0")
        except (KeyError, TypeError, json.JSONDecodeError) as exc:
            model_failures.append(f"invalid model lock license metadata: {exc}")
    record(
        "optional model/runtime license metadata",
        model_lock is not None and not model_failures,
        "; ".join(model_failures),
    )


def validate_dependency_boundary() -> None:
    """Keep the present no-third-party-Go-module assertion honest."""
    go_mod = read_regular(Path("go.mod"))
    if go_mod is None:
        return
    active_lines = [
        line.split("//", 1)[0].strip()
        for line in go_mod.splitlines()
        if line.split("//", 1)[0].strip()
    ]
    require_lines = [line for line in active_lines if line == "require (" or line.startswith("require ")]
    record(
        "third-party Go module boundary",
        not require_lines,
        "dependencies require an expanded license inventory: " + ", ".join(require_lines)
        if require_lines
        else "no third-party Go modules",
    )


def validate_tracked_artifacts() -> None:
    """Reject tracked links, submodules, model weights, and native artifacts."""
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--stage", "-z"],
        cwd=ROOT,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if result.returncode != 0:
        record(
            "tracked release artifact policy",
            False,
            result.stderr.decode("utf-8", errors="replace").strip(),
        )
        return
    failures: list[str] = []
    for raw in result.stdout.split(b"\0"):
        if not raw:
            continue
        try:
            header, raw_path = raw.split(b"\t", 1)
            mode, _object_id, stage = header.split(b" ", 2)
            path = raw_path.decode("utf-8")
        except (UnicodeDecodeError, ValueError):
            failures.append("unportable Git index entry")
            continue
        if stage != b"0":
            failures.append(f"unmerged: {path}")
        if mode == b"120000":
            failures.append(f"symlink: {path}")
        elif mode == b"160000":
            failures.append(f"submodule: {path}")
        elif Path(path).suffix.lower() in PROHIBITED_TRACKED_SUFFIXES:
            failures.append(f"model/native artifact: {path}")
    record(
        "tracked release artifact policy",
        not failures,
        "; ".join(failures[:20]),
    )


def main() -> int:
    """Run all checks and print one machine-readable report."""
    validate_root_documents()
    validate_declared_metadata()
    validate_dependency_boundary()
    validate_tracked_artifacts()
    result = {
        "schema_version": "1.0",
        "ok": not ERRORS,
        "checks": CHECKS,
        "errors": ERRORS,
    }
    print(json.dumps(result, indent=2, sort_keys=True))
    return 1 if ERRORS else 0


if __name__ == "__main__":
    sys.exit(main())
