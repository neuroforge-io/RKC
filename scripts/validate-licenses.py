#!/usr/bin/env python3
"""Validate RKC license boundaries and release-notice completeness."""
from __future__ import annotations

import hashlib
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
    Path("go.sum"),
    Path("third_party/go-modules.lock.json"),
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
SHA256 = re.compile(r"^[0-9a-f]{64}$")
GO_SUM = re.compile(r"^h1:[A-Za-z0-9+/]{43}=$")
EXPECTED_MODULE_PATH = "github.com/neuroforge-io/RKC"
EXPECTED_GO_DIRECTIVE = "1.25.0"
EXPECTED_TOOLCHAIN = "go1.26.5"
EXPECTED_ROOT_REQUIREMENTS = {"modernc.org/sqlite": "v1.54.0"}
EXPECTED_EXPLICIT_REQUIREMENTS = {
    "modernc.org/libc": "v1.74.1",
    "modernc.org/sqlite": "v1.54.0",
}
# Go retains the checksum of a transitive module's older go.mod when that
# metadata participates in minimal-version selection. It is not a resolved or
# shipped module version, so govern it separately from the reviewed build list.
EXPECTED_GO_MOD_COMPATIBILITY_SUMS = {
    ("golang.org/x/sys", "v0.6.0/go.mod"): (
        "h1:oPkhp1MJrh7nUepCBck5+mAzfO9JrbApNNgaTdGDITg="
    ),
}
EXPECTED_MODULES: dict[str, dict[str, object]] = {
    "github.com/dustin/go-humanize": {
        "version": "v1.0.1",
        "module_sum": "h1:GzkhY7T5VNhEkwH0PVJgjz+fX1rhBrR7pRT3mDkpeCY=",
        "go_mod_sum": "h1:Mu1zIs6XwVuF/gI1OepvI0qD18qycQx+mFykh5fBlto=",
        "license_spdx": "MIT",
        "licenses": {
            "LICENSE": "a973b4498c13eb74baa2a8e5c351426a6826f2fcdd909916dbe53ee2e755fd71"
        },
    },
    "github.com/google/pprof": {
        "version": "v0.0.0-20250317173921-a4b03ec1a45e",
        "module_sum": "h1:ijClszYn+mADRFY17kjQEVQ1XRhq2/JR1M3sGqeJoxs=",
        "go_mod_sum": "h1:boTsfXsheKC2y+lKOCMpSfarhxDeIzfZG1jqGcPl3cA=",
        "license_spdx": "Apache-2.0",
        "licenses": {
            "LICENSE": "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"
        },
    },
    "github.com/google/uuid": {
        "version": "v1.6.0",
        "module_sum": "h1:NIvaJDMOsjHA8n1jAhLSgzrAzy1Hgr+hNrb57e+94F0=",
        "go_mod_sum": "h1:TIyPZe4MgqvfeYDBFedMoGGpEw/LqOeaOT+nhxU+yHo=",
        "license_spdx": "BSD-3-Clause",
        "licenses": {
            "LICENSE": "0a8d61ed3cbfd5312326e8126c31ce9c627a283adc99131b56896d29ada04b2d"
        },
    },
    "github.com/mattn/go-isatty": {
        "version": "v0.0.20",
        "module_sum": "h1:xfD0iDuEKnDkl03q4limB+vH+GxLEtL/jb4xVJSWWEY=",
        "go_mod_sum": "h1:W+V8PltTTMOvKvAeJH7IuucS94S2C6jfK/D7dTCTo3Y=",
        "license_spdx": "MIT",
        "licenses": {
            "LICENSE": "08eab1118c80885fa1fa6a6dd7303f65a379fcb3733e063d20d1bbc2c76e6fa1"
        },
    },
    "github.com/ncruces/go-strftime": {
        "version": "v1.0.0",
        "module_sum": "h1:HMFp8mLCTPp341M/ZnA4qaf7ZlsbTc+miZjCLOFAw7w=",
        "go_mod_sum": "h1:Fwc5htZGVVkseilnfgOVb9mKy6w1naJmn9CehxcKcls=",
        "license_spdx": "MIT",
        "licenses": {
            "LICENSE": "38ae43959daf953a393a585b2988672cb65a5a541aca0d0be5e72595a0a16883"
        },
    },
    "github.com/remyoudompheng/bigfft": {
        "version": "v0.0.0-20230129092748-24d4a6f8daec",
        "module_sum": "h1:W09IVJc94icq4NjY3clb7Lk8O1qJ8BdBEF8z0ibU0rE=",
        "go_mod_sum": "h1:qqbHyh8v60DhA7CoWK5oRCqLrMHRGoxYCSS9EjAz6Eo=",
        "license_spdx": "BSD-3-Clause",
        "licenses": {
            "LICENSE": "dd26a7abddd02e2d0aba97805b31f248ef7835d9e10da289b22e3b8ab78b324d"
        },
    },
    "golang.org/x/sys": {
        "version": "v0.46.0",
        "module_sum": "h1:noSf2Fq6F8DBgS+LysIkx7rIExoNHJsxOAtPp4rthXw=",
        "go_mod_sum": "h1:4GL1E5IUh+htKOUEOaiffhrAeqysfVGipDYzABqnCmw=",
        "license_spdx": "BSD-3-Clause",
        "licenses": {
            "LICENSE": "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"
        },
    },
    "modernc.org/fileutil": {
        "version": "v1.4.0",
        "module_sum": "h1:j6ZzNTftVS054gi281TyLjHPp6CPHr2KCxEXjEbD6SM=",
        "go_mod_sum": "h1:EqdKFDxiByqxLk8ozOxObDSfcVOv/54xDs/DUHdvCUU=",
        "license_spdx": "BSD-3-Clause",
        "licenses": {
            "LICENSE": "7dc07397727fc500d5ce768d724e4d555eee8fb7687f421fd40d72249868b0ca"
        },
    },
    "modernc.org/libc": {
        "version": "v1.74.1",
        "module_sum": "h1:bdR4VTKFMC4966QSNZ05XLGI/VwzVa2kTUX51Dm0riQ=",
        "go_mod_sum": "h1:uH4t5bOx3G3g9Xcmj10YKlTcVISlRDwv8VoQJG9n8Os=",
        "license_spdx": "BSD-3-Clause",
        "licenses": {
            "LICENSE": "95ff867eb55a56935fa7492406cfa953fb7c13ca73f4c0a86ae05756b4605600",
            "LICENSE-3RD-PARTY.md": "f597097efe3d97021f89170746bd3a0fb9a8b6fb26b82043ed68a4e0283bee6c",
        },
    },
    "modernc.org/mathutil": {
        "version": "v1.7.1",
        "module_sum": "h1:GCZVGXdaN8gTqB1Mf/usp1Y/hSqgI2vAGGP4jZMCxOU=",
        "go_mod_sum": "h1:4p5IwJITfppl0G4sUEDtCr4DthTaT47/N3aT6MhfgJg=",
        "license_spdx": "BSD-3-Clause",
        "licenses": {
            "LICENSE": "bfa9bf72a72ca009fd62a8f84fca3dca67e51d93af96352723646599898b6cf5"
        },
    },
    "modernc.org/memory": {
        "version": "v1.11.0",
        "module_sum": "h1:o4QC8aMQzmcwCK3t3Ux/ZHmwFPzE6hf2Y5LbkRs+hbI=",
        "go_mod_sum": "h1:/JP4VbVC+K5sU2wZi9bHoq2MAkCnrt2r98UGeSK7Mjw=",
        "license_spdx": "BSD-3-Clause",
        "licenses": {
            "LICENSE": "59895e669f48f168b6b858358f6005779cdf40a265f7828813061b56af67b496",
            "LICENSE-GO": "2d36597f7117c38b006835ae7f537487207d8ec407aa9d9980794b2030cbc067",
            "LICENSE-MMAP-GO": "c2eba69f20d05414538c3a5df7694dde392e065ff70882e1625e90f5d6659fff",
        },
    },
    "modernc.org/sqlite": {
        "version": "v1.54.0",
        "module_sum": "h1:JCxR4qwkJvOaqAoYcgDoO25Nc+ROg6EJ2LfBVzdrgog=",
        "go_mod_sum": "h1:4ntCLuNmnH8+GNqjka1wNg7KJd5/Hi5FYp8K+XQ7GZw=",
        "license_spdx": "BSD-3-Clause AND LicenseRef-SQLite-Public-Domain",
        "licenses": {
            "LICENSE": "c6fe05491a60ae13bcd223088d2705e36dede24e5587226231d2459ada5c4822",
            "SQLITE-LICENSE": "8438c9c89b849131ead81d5435cb97fcf052df5b0b286dda8a2d4c29e6cb3fd0",
        },
    },
}


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


def reject_duplicate_keys(pairs: list[tuple[str, object]]) -> dict[str, object]:
    """Build a JSON object while rejecting ambiguous duplicate members."""
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def require_exact_keys(
    value: object,
    expected: frozenset[str],
    label: str,
    failures: list[str],
) -> dict[str, object] | None:
    """Require an object with exactly the governed schema members."""
    if not isinstance(value, dict):
        failures.append(f"{label} must be an object")
        return None
    actual = frozenset(value)
    if actual != expected:
        missing = sorted(expected - actual)
        unknown = sorted(actual - expected)
        failures.append(
            f"{label} keys differ; missing={missing!r}, unknown={unknown!r}"
        )
        return None
    return value


def parse_go_mod(value: str) -> tuple[dict[str, str], dict[str, str], list[str]]:
    """Parse the small fail-closed subset of go.mod accepted by RKC."""
    metadata: dict[str, str] = {}
    requirements: dict[str, str] = {}
    failures: list[str] = []
    in_require_block = False

    for line_number, raw_line in enumerate(value.splitlines(), start=1):
        line = raw_line.split("//", 1)[0].strip()
        if not line:
            continue
        if in_require_block:
            if line == ")":
                in_require_block = False
                continue
            fields = line.split()
            if len(fields) != 2:
                failures.append(f"go.mod:{line_number}: invalid require entry")
                continue
            path, version = fields
            if path in requirements:
                failures.append(f"go.mod:{line_number}: duplicate requirement {path}")
            requirements[path] = version
            continue

        fields = line.split()
        directive = fields[0]
        if directive == "require" and fields == ["require", "("]:
            in_require_block = True
        elif directive == "require" and len(fields) == 3:
            path, version = fields[1:]
            if path in requirements:
                failures.append(f"go.mod:{line_number}: duplicate requirement {path}")
            requirements[path] = version
        elif directive in ("module", "go", "toolchain") and len(fields) == 2:
            if directive in metadata:
                failures.append(
                    f"go.mod:{line_number}: duplicate {directive} directive"
                )
            metadata[directive] = fields[1]
        else:
            failures.append(
                f"go.mod:{line_number}: prohibited or invalid directive {line!r}"
            )
    if in_require_block:
        failures.append("go.mod: unterminated require block")
    return metadata, requirements, failures


def parse_go_sum(value: str) -> tuple[dict[tuple[str, str], str], list[str]]:
    """Parse go.sum without accepting duplicates or non-canonical lines."""
    entries: dict[tuple[str, str], str] = {}
    failures: list[str] = []
    lines = [line for line in value.splitlines() if line]
    if lines != sorted(lines):
        failures.append("go.sum entries are not sorted")
    for line_number, line in enumerate(lines, start=1):
        fields = line.split()
        if len(fields) != 3:
            failures.append(f"go.sum:{line_number}: expected three fields")
            continue
        path, version, digest = fields
        key = (path, version)
        if key in entries:
            failures.append(f"go.sum:{line_number}: duplicate {path} {version}")
        entries[key] = digest
    return entries, failures


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
            "modernc.org/sqlite v1.54.0",
            "modernc.org/libc v1.74.1",
            "third_party/go-modules.lock.json",
            "LICENSES/go-modules/",
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
            if (
                isinstance(plugin_id, str)
                and plugin_id.startswith("rkc.")
                and expression != "Apache-2.0"
            ):
                plugin_failures.append(
                    f"{plugin_id}: official RKC plugin is not Apache-2.0"
                )
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
                if (
                    kind in ("generation-model", "embedding-model")
                    and expression != "Apache-2.0"
                ):
                    model_failures.append(
                        f"{asset['id']}: model license is not Apache-2.0"
                    )
        except (KeyError, TypeError, json.JSONDecodeError) as exc:
            model_failures.append(f"invalid model lock license metadata: {exc}")
    record(
        "optional model/runtime license metadata",
        model_lock is not None and not model_failures,
        "; ".join(model_failures),
    )


def validate_dependency_boundary() -> None:
    """Validate the exact reviewed Go graph, sums, notices, and license bytes."""
    go_mod = read_regular(Path("go.mod"))
    go_sum = read_regular(Path("go.sum"))
    lock_value = read_regular(Path("third_party/go-modules.lock.json"))
    notice = read_regular(Path("THIRD_PARTY_NOTICES.md"))
    if None in (go_mod, go_sum, lock_value, notice):
        record(
            "third-party Go module boundary",
            False,
            "dependency governance files are missing or unreadable",
        )
        return

    assert go_mod is not None
    assert go_sum is not None
    assert lock_value is not None
    assert notice is not None
    failures: list[str] = []

    metadata, requirements, go_mod_failures = parse_go_mod(go_mod)
    failures.extend(go_mod_failures)
    expected_metadata = {
        "module": EXPECTED_MODULE_PATH,
        "go": EXPECTED_GO_DIRECTIVE,
        "toolchain": EXPECTED_TOOLCHAIN,
    }
    if metadata != expected_metadata:
        failures.append(
            f"go.mod metadata drift: expected {expected_metadata!r}, got {metadata!r}"
        )
    for path, version in requirements.items():
        expected = EXPECTED_MODULES.get(path)
        if expected is None:
            failures.append(f"go.mod contains unknown module {path} {version}")
        elif version != expected["version"]:
            failures.append(
                f"go.mod version drift for {path}: expected "
                f"{expected['version']}, got {version}"
            )
    for path, version in EXPECTED_EXPLICIT_REQUIREMENTS.items():
        if requirements.get(path) != version:
            failures.append(f"go.mod is missing explicit requirement {path} {version}")

    try:
        lock_document: object = json.loads(
            lock_value,
            object_pairs_hook=reject_duplicate_keys,
        )
    except (json.JSONDecodeError, ValueError) as exc:
        failures.append(f"invalid Go module lock JSON: {exc}")
        lock_document = None

    locked_license_paths: set[str] = set()
    top = require_exact_keys(
        lock_document,
        frozenset({"schema_version", "go", "root_requirements", "modules"}),
        "Go module lock",
        failures,
    )
    if top is not None:
        if top["schema_version"] != "1.0":
            failures.append("Go module lock schema_version must be 1.0")

        go_config = require_exact_keys(
            top["go"],
            frozenset({"directive", "toolchain"}),
            "Go module lock go",
            failures,
        )
        if go_config is not None and go_config != {
            "directive": EXPECTED_GO_DIRECTIVE,
            "toolchain": EXPECTED_TOOLCHAIN,
        }:
            failures.append("Go module lock toolchain metadata drift")

        expected_roots = [
            {"path": path, "version": version}
            for path, version in sorted(EXPECTED_ROOT_REQUIREMENTS.items())
        ]
        roots = top["root_requirements"]
        if roots != expected_roots:
            failures.append(
                f"Go module lock root requirements drift: expected {expected_roots!r}"
            )

        modules = top["modules"]
        if not isinstance(modules, list):
            failures.append("Go module lock modules must be an array")
            modules = []
        module_paths: list[str] = []
        seen_modules: set[str] = set()
        expected_module_keys = frozenset(
            {
                "path",
                "version",
                "direct",
                "module_sum",
                "go_mod_sum",
                "source_url",
                "license_spdx",
                "licenses",
                "notice_path",
            }
        )
        expected_license_keys = frozenset({"source_path", "path", "sha256"})
        for index, raw_module in enumerate(modules):
            module = require_exact_keys(
                raw_module,
                expected_module_keys,
                f"Go module lock modules[{index}]",
                failures,
            )
            if module is None:
                continue
            path = module["path"]
            version = module["version"]
            if not isinstance(path, str) or not isinstance(version, str):
                failures.append(f"Go module lock modules[{index}] identity is invalid")
                continue
            module_paths.append(path)
            if path in seen_modules:
                failures.append(f"Go module lock duplicates module {path}")
                continue
            seen_modules.add(path)
            expected = EXPECTED_MODULES.get(path)
            if expected is None:
                failures.append(
                    f"Go module lock contains unknown module {path} {version}"
                )
                continue
            if version != expected["version"]:
                failures.append(
                    f"Go module lock version drift for {path}: expected "
                    f"{expected['version']}, got {version}"
                )
            direct = path in EXPECTED_ROOT_REQUIREMENTS
            if module["direct"] is not direct:
                failures.append(f"Go module lock direct flag drift for {path}")
            for sum_key in ("module_sum", "go_mod_sum"):
                digest = module[sum_key]
                if not isinstance(digest, str) or not GO_SUM.fullmatch(digest):
                    failures.append(f"Go module lock {path} has invalid {sum_key}")
                if digest != expected[sum_key]:
                    failures.append(f"Go module lock {sum_key} drift for {path}")
            expected_url = f"https://proxy.golang.org/{path}/@v/{version}.zip"
            if module["source_url"] != expected_url:
                failures.append(f"Go module lock source URL drift for {path}")
            if module["license_spdx"] != expected["license_spdx"]:
                failures.append(f"Go module lock SPDX drift for {path}")
            if module["notice_path"] != "THIRD_PARTY_NOTICES.md":
                failures.append(f"Go module lock notice path drift for {path}")

            expected_licenses = expected["licenses"]
            if not isinstance(expected_licenses, dict):
                failures.append(
                    f"internal expected-license metadata invalid for {path}"
                )
                continue
            licenses = module["licenses"]
            if not isinstance(licenses, list):
                failures.append(f"Go module lock licenses must be an array for {path}")
                continue
            seen_sources: set[str] = set()
            for license_index, raw_license in enumerate(licenses):
                license_entry = require_exact_keys(
                    raw_license,
                    expected_license_keys,
                    f"Go module lock {path} licenses[{license_index}]",
                    failures,
                )
                if license_entry is None:
                    continue
                source_path = license_entry["source_path"]
                license_path = license_entry["path"]
                license_hash = license_entry["sha256"]
                if not all(
                    isinstance(item, str)
                    for item in (source_path, license_path, license_hash)
                ):
                    failures.append(f"Go module lock license fields invalid for {path}")
                    continue
                if source_path in seen_sources:
                    failures.append(f"Go module lock duplicates {path}/{source_path}")
                seen_sources.add(source_path)
                if (
                    Path(source_path).is_absolute()
                    or ".." in Path(source_path).parts
                    or Path(source_path).as_posix() != source_path
                ):
                    failures.append(
                        f"unsafe upstream license path for {path}: {source_path}"
                    )
                    continue
                expected_hash = expected_licenses.get(source_path)
                if expected_hash is None:
                    failures.append(
                        f"unknown upstream license for {path}: {source_path}"
                    )
                    continue
                expected_path = f"LICENSES/go-modules/{path}@{version}/{source_path}"
                if license_path != expected_path:
                    failures.append(
                        f"license path drift for {path}/{source_path}: {license_path}"
                    )
                    continue
                if not SHA256.fullmatch(license_hash) or license_hash != expected_hash:
                    failures.append(
                        f"license hash metadata drift for {path}/{source_path}"
                    )
                if license_path in locked_license_paths:
                    failures.append(f"duplicate tracked license path: {license_path}")
                locked_license_paths.add(license_path)
                license_text = read_regular(
                    Path(license_path), maximum_bytes=128 * 1024
                )
                if license_text is None:
                    failures.append(f"missing governed license file: {license_path}")
                else:
                    actual_hash = hashlib.sha256(
                        license_text.encode("utf-8")
                    ).hexdigest()
                    if actual_hash != license_hash:
                        failures.append(
                            f"license file hash drift for {path}/{source_path}: "
                            f"expected {license_hash}, got {actual_hash}"
                        )
                if license_path not in notice:
                    failures.append(f"notice omits license path {license_path}")
            if seen_sources != set(expected_licenses):
                failures.append(f"license inventory incomplete for {path}")
            if f"{path} {version}" not in notice:
                failures.append(f"notice omits module {path} {version}")

        expected_paths = sorted(EXPECTED_MODULES)
        if module_paths != expected_paths:
            failures.append(
                "Go module lock module order/set drift: "
                f"expected {expected_paths!r}, got {module_paths!r}"
            )
        if seen_modules != set(EXPECTED_MODULES):
            failures.append("Go module lock is missing governed modules")

    if "third_party/go-modules.lock.json" not in notice:
        failures.append("notice omits third_party/go-modules.lock.json")

    license_root = ROOT / "LICENSES" / "go-modules"
    present_license_paths: set[str] = set()
    if license_root.is_dir() and not license_root.is_symlink():
        for path in sorted(license_root.rglob("*")):
            if path.is_file() and not path.is_symlink():
                present_license_paths.add(path.relative_to(ROOT).as_posix())
    if present_license_paths != locked_license_paths:
        failures.append(
            "tracked Go module license set differs from lock: "
            f"missing={sorted(locked_license_paths - present_license_paths)!r}, "
            f"unknown={sorted(present_license_paths - locked_license_paths)!r}"
        )

    sum_entries, go_sum_failures = parse_go_sum(go_sum)
    failures.extend(go_sum_failures)
    expected_sum_entries: dict[tuple[str, str], str] = dict(
        EXPECTED_GO_MOD_COMPATIBILITY_SUMS
    )
    for path, expected in EXPECTED_MODULES.items():
        version = expected["version"]
        module_sum = expected["module_sum"]
        go_mod_sum = expected["go_mod_sum"]
        if not all(isinstance(item, str) for item in (version, module_sum, go_mod_sum)):
            failures.append(f"internal expected-module metadata invalid for {path}")
            continue
        expected_sum_entries[(path, version)] = module_sum
        expected_sum_entries[(path, f"{version}/go.mod")] = go_mod_sum
    for key, digest in sum_entries.items():
        if key not in expected_sum_entries:
            failures.append(f"go.sum contains unknown module entry {key[0]} {key[1]}")
        elif digest != expected_sum_entries[key]:
            failures.append(f"go.sum checksum drift for {key[0]} {key[1]}")
    missing_sums = sorted(set(expected_sum_entries) - set(sum_entries))
    if missing_sums:
        failures.append(f"go.sum is missing entries: {missing_sums!r}")
    for key, digest in sum_entries.items():
        if not GO_SUM.fullmatch(digest):
            failures.append(f"go.sum has invalid checksum for {key[0]} {key[1]}")

    record(
        "third-party Go module boundary",
        not failures,
        "; ".join(failures),
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
