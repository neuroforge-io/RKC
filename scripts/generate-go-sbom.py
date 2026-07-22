#!/usr/bin/env python3
"""Generate a deterministic SPDX 2.3 SBOM for one audited RKC Go binary."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_LOCK = ROOT / "third_party" / "go-modules.lock.json"
PROJECT_MODULE = "github.com/neuroforge-io/RKC"
MAX_INPUT_BYTES = 1024 * 1024 * 1024
LOCK_KEYS = frozenset(
    {"schema_version", "go", "root_requirements", "modules"}
)
GO_KEYS = frozenset({"directive", "toolchain"})
ROOT_REQUIREMENT_KEYS = frozenset({"path", "version"})
MODULE_KEYS = frozenset(
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
LICENSE_KEYS = frozenset({"source_path", "path", "sha256"})
SPDX_EXPRESSION = re.compile(
    r"[A-Za-z0-9][A-Za-z0-9.+-]*(?: (?:AND|OR) [A-Za-z0-9][A-Za-z0-9.+-]*)*"
)
SHA256 = re.compile(r"[0-9a-f]{64}")
GIT_OBJECT_ID = re.compile(r"(?:[0-9a-f]{40}|[0-9a-f]{64})")
PLATFORM_NAME = re.compile(r"[a-z0-9][a-z0-9._-]*")
LICENSE_REFERENCE = re.compile(r"LicenseRef-[A-Za-z0-9.-]+")
LICENSE_REFERENCE_SOURCES = {
    "LicenseRef-SQLite-Public-Domain": "SQLITE-LICENSE",
}
TARGET_TUNING = {
    "amd64": {"GOAMD64": "v1"},
    "arm64": {"GOARM64": "v8.0"},
}


class SBOMError(RuntimeError):
    """Raised when binary or supply-chain evidence is incomplete."""


def require(condition: bool, message: str) -> None:
    if not condition:
        raise SBOMError(message)


def regular_bytes(path: Path, label: str, maximum: int = MAX_INPUT_BYTES) -> bytes:
    """Read a bounded regular file without accepting a symbolic link."""
    try:
        info = os.lstat(path)
    except FileNotFoundError as exc:
        raise SBOMError(f"{label} is missing: {path}") from exc
    require(stat.S_ISREG(info.st_mode), f"{label} must be a regular file: {path}")
    require(not stat.S_ISLNK(info.st_mode), f"{label} must not be a symlink: {path}")
    require(info.st_size <= maximum, f"{label} exceeds {maximum} bytes: {path}")
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise SBOMError(f"cannot open {label}: {path}: {exc}") from exc
    with os.fdopen(descriptor, "rb") as handle:
        observed = os.fstat(handle.fileno())
        require(stat.S_ISREG(observed.st_mode), f"{label} changed file type: {path}")
        require(observed.st_size <= maximum, f"{label} exceeds {maximum} bytes: {path}")
        payload = handle.read(maximum + 1)
        final = os.fstat(handle.fileno())
    require(len(payload) <= maximum, f"{label} exceeds {maximum} bytes: {path}")
    require(
        (
            observed.st_dev,
            observed.st_ino,
            observed.st_size,
            observed.st_mtime_ns,
            observed.st_ctime_ns,
        )
        == (
            final.st_dev,
            final.st_ino,
            final.st_size,
            final.st_mtime_ns,
            final.st_ctime_ns,
        ),
        f"{label} changed while reading: {path}",
    )
    return payload


def sha256_file(path: Path, label: str, maximum: int = MAX_INPUT_BYTES) -> str:
    """Hash a bounded regular file without loading it into memory."""
    try:
        initial = os.lstat(path)
    except FileNotFoundError as exc:
        raise SBOMError(f"{label} is missing: {path}") from exc
    require(stat.S_ISREG(initial.st_mode), f"{label} must be a regular file: {path}")
    require(not stat.S_ISLNK(initial.st_mode), f"{label} must not be a symlink: {path}")
    require(initial.st_size <= maximum, f"{label} exceeds {maximum} bytes: {path}")
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise SBOMError(f"cannot open {label}: {path}: {exc}") from exc
    digest = hashlib.sha256()
    with os.fdopen(descriptor, "rb") as handle:
        observed = os.fstat(handle.fileno())
        require(stat.S_ISREG(observed.st_mode), f"{label} changed file type: {path}")
        require(observed.st_size <= maximum, f"{label} exceeds {maximum} bytes: {path}")
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
        final = os.fstat(handle.fileno())
    require(
        (
            observed.st_dev,
            observed.st_ino,
            observed.st_size,
            observed.st_mtime_ns,
            observed.st_ctime_ns,
        )
        == (
            final.st_dev,
            final.st_ino,
            final.st_size,
            final.st_mtime_ns,
            final.st_ctime_ns,
        ),
        f"{label} changed while hashing: {path}",
    )
    return digest.hexdigest()


def strict_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise SBOMError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def canonical_repo_path(value: object, label: str) -> Path:
    require(isinstance(value, str) and bool(value), f"{label} must be a string")
    assert isinstance(value, str)
    require("\\" not in value and "\x00" not in value, f"{label} is not portable")
    path = Path(value)
    require(not path.is_absolute(), f"{label} must be repository-relative")
    require(all(part not in {"", ".", ".."} for part in path.parts), f"{label} is unsafe")
    require(path.as_posix() == value, f"{label} is not canonical")
    return path


def string_field(document: dict[str, object], key: str, label: str) -> str:
    value = document.get(key)
    require(isinstance(value, str) and bool(value), f"{label}.{key} must be a string")
    assert isinstance(value, str)
    return value


def load_lock(
    path: Path, root: Path = ROOT
) -> tuple[dict[str, dict[str, object]], str, dict[str, str]]:
    """Load and independently verify the audited Go-module lock."""
    payload = regular_bytes(path, "Go module lock", 4 * 1024 * 1024)
    lock_digest = hashlib.sha256(payload).hexdigest()
    try:
        document = json.loads(payload.decode("utf-8"), object_pairs_hook=strict_object)
    except SBOMError:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise SBOMError(f"Go module lock is not strict UTF-8 JSON: {exc}") from exc
    require(isinstance(document, dict), "Go module lock must be an object")
    require(set(document) == LOCK_KEYS, "Go module lock keys drifted")
    require(document["schema_version"] == "1.0", "unsupported Go module lock schema")
    go = document["go"]
    require(isinstance(go, dict) and set(go) == GO_KEYS, "Go lock metadata drifted")
    assert isinstance(go, dict)
    go_metadata = {
        "directive": string_field(go, "directive", "go"),
        "toolchain": string_field(go, "toolchain", "go"),
    }
    requirements = document["root_requirements"]
    require(isinstance(requirements, list) and requirements, "root requirements are empty")
    root_requirement_versions: dict[str, str] = {}
    for index, requirement in enumerate(requirements):
        require(
            isinstance(requirement, dict) and set(requirement) == ROOT_REQUIREMENT_KEYS,
            f"root requirement {index} drifted",
        )
        requirement_path = string_field(requirement, "path", f"root requirement {index}")
        requirement_version = string_field(requirement, "version", f"root requirement {index}")
        require(
            requirement_path not in root_requirement_versions,
            f"duplicate root requirement: {requirement_path}",
        )
        root_requirement_versions[requirement_path] = requirement_version

    modules = document["modules"]
    require(isinstance(modules, list) and modules, "module inventory is empty")
    result: dict[str, dict[str, object]] = {}
    observed_order: list[str] = []
    license_paths: set[Path] = set()
    for index, module in enumerate(modules):
        label = f"module {index}"
        require(isinstance(module, dict) and set(module) == MODULE_KEYS, f"{label} keys drifted")
        assert isinstance(module, dict)
        module_path = string_field(module, "path", label)
        version = string_field(module, "version", label)
        require(module_path not in result, f"duplicate locked module: {module_path}")
        require(version.startswith("v"), f"{label}.version is not canonical")
        require(type(module["direct"]) is bool, f"{label}.direct must be boolean")
        module_sum = string_field(module, "module_sum", label)
        go_mod_sum = string_field(module, "go_mod_sum", label)
        require(module_sum.startswith("h1:"), f"{label}.module_sum is invalid")
        require(go_mod_sum.startswith("h1:"), f"{label}.go_mod_sum is invalid")
        source_url = string_field(module, "source_url", label)
        require(source_url.startswith("https://"), f"{label}.source_url must use HTTPS")
        expression = string_field(module, "license_spdx", label)
        require(SPDX_EXPRESSION.fullmatch(expression) is not None, f"{label}.license_spdx is invalid")
        notice_path = canonical_repo_path(module["notice_path"], f"{label}.notice_path")
        regular_bytes(root / notice_path, f"{label} notice", 4 * 1024 * 1024)
        licenses = module["licenses"]
        require(isinstance(licenses, list) and licenses, f"{label}.licenses is empty")
        for license_index, license_item in enumerate(licenses):
            license_label = f"{label}.licenses[{license_index}]"
            require(
                isinstance(license_item, dict) and set(license_item) == LICENSE_KEYS,
                f"{license_label} keys drifted",
            )
            assert isinstance(license_item, dict)
            string_field(license_item, "source_path", license_label)
            tracked = canonical_repo_path(license_item["path"], f"{license_label}.path")
            require(tracked not in license_paths, f"duplicate locked license path: {tracked}")
            license_paths.add(tracked)
            digest = string_field(license_item, "sha256", license_label)
            require(SHA256.fullmatch(digest) is not None, f"{license_label}.sha256 is invalid")
            observed = hashlib.sha256(
                regular_bytes(root / tracked, f"{license_label} file", 4 * 1024 * 1024)
            ).hexdigest()
            require(observed == digest, f"{license_label} digest drifted")
        result[module_path] = module
        observed_order.append(module_path)
    require(observed_order == sorted(observed_order), "module lock is not path-sorted")
    direct_versions = {
        path: str(module["version"])
        for path, module in result.items()
        if module["direct"] is True
    }
    require(
        direct_versions == root_requirement_versions,
        "direct module inventory drifted from root requirements",
    )
    return result, lock_digest, go_metadata


def binary_build_info(
    binary: Path,
    expected_toolchain: str,
    expected_goos: str,
    expected_goarch: str,
    expected_commit: str,
    expected_commit_time: str,
) -> tuple[dict[str, object], dict[str, str]]:
    """Return verified Go build metadata for a regular RKC binary."""
    sha256_file(binary, "Go binary")
    result = subprocess.run(
        ["go", "version", "-m", "-json", str(binary)],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.returncode != 0:
        detail = result.stderr.strip() or "unknown go version failure"
        raise SBOMError(f"cannot inspect Go binary metadata: {detail}")
    try:
        document = json.loads(result.stdout, object_pairs_hook=strict_object)
    except (json.JSONDecodeError, SBOMError) as exc:
        raise SBOMError(f"Go binary metadata is invalid JSON: {exc}") from exc
    require(isinstance(document, dict), "Go binary metadata must be an object")
    require(
        document.get("Path") == f"{PROJECT_MODULE}/cmd/{binary.name}",
        "Go binary command path does not match its release name",
    )
    require(
        document.get("GoVersion") == expected_toolchain,
        "Go binary toolchain differs from the audited module lock",
    )
    main = document.get("Main")
    require(isinstance(main, dict), "Go binary is missing main-module metadata")
    require(main.get("Path") == PROJECT_MODULE, "binary main module is not RKC")
    settings = document.get("Settings")
    require(isinstance(settings, list), "Go binary is missing build settings")
    setting_map: dict[str, str] = {}
    for setting in settings:
        require(isinstance(setting, dict), "Go binary build setting is malformed")
        key, value = setting.get("Key"), setting.get("Value")
        require(isinstance(key, str) and isinstance(value, str), "Go binary build setting is malformed")
        require(key not in setting_map, f"duplicate Go binary build setting: {key}")
        setting_map[key] = value
    require(setting_map.get("CGO_ENABLED") == "0", "Go binary was not built with CGO_ENABLED=0")
    require(setting_map.get("GOOS") == expected_goos, "Go binary GOOS differs from its release platform")
    require(
        setting_map.get("GOARCH") == expected_goarch,
        "Go binary GOARCH differs from its release platform",
    )
    target_tuning = TARGET_TUNING.get(expected_goarch)
    require(target_tuning is not None, f"unsupported release GOARCH: {expected_goarch}")
    assert target_tuning is not None
    for name, value in target_tuning.items():
        require(
            setting_map.get(name) == value,
            f"Go binary {name} differs from the normalized release target",
        )
    for name in {"GOAMD64", "GOARM64"} - set(target_tuning):
        require(
            name not in setting_map,
            f"Go binary contains irrelevant architecture tuning: {name}",
        )
    require(
        setting_map.get("GOEXPERIMENT", "") == "",
        "Go binary was built with a non-default GOEXPERIMENT",
    )
    require(
        setting_map.get("GOFIPS140", "off") == "off",
        "Go binary was not built with normalized GOFIPS140=off",
    )
    require(setting_map.get("-trimpath") == "true", "Go binary was not built with -trimpath")
    require(setting_map.get("vcs") == "git", "Go binary is missing Git VCS build metadata")
    require(
        setting_map.get("vcs.revision") == expected_commit,
        "Go binary source commit differs from the release source",
    )
    require(setting_map.get("vcs.modified") == "false", "Go binary source tree was dirty")
    require(
        setting_map.get("vcs.time") == expected_commit_time,
        "Go binary VCS timestamp differs from the source commit",
    )
    recorded_tuning = {
        "GOEXPERIMENT": "default",
        "GOFIPS140": "off",
        **target_tuning,
    }
    return document, recorded_tuning


def dependency_inventory(
    build_info: dict[str, object], locked: dict[str, dict[str, object]]
) -> list[dict[str, object]]:
    """Bind every linked module to an exact audited lock entry."""
    dependencies = build_info.get("Deps", [])
    require(isinstance(dependencies, list), "Go binary dependency metadata is malformed")
    observed: list[dict[str, object]] = []
    seen: set[str] = set()
    for index, dependency in enumerate(dependencies):
        require(isinstance(dependency, dict), f"Go dependency {index} is malformed")
        path, version, module_sum = (
            dependency.get("Path"),
            dependency.get("Version"),
            dependency.get("Sum"),
        )
        require(
            isinstance(path, str) and isinstance(version, str) and isinstance(module_sum, str),
            f"Go dependency {index} lacks exact identity",
        )
        require(not dependency.get("Replace"), f"Go dependency replacement is prohibited: {path}")
        require(path not in seen, f"duplicate Go dependency metadata: {path}")
        seen.add(path)
        module = locked.get(path)
        require(module is not None, f"Go binary contains an unaudited module: {path}")
        assert module is not None
        require(module["version"] == version, f"Go binary module version drifted: {path}")
        require(module["module_sum"] == module_sum, f"Go binary module checksum drifted: {path}")
        observed.append(module)
    return sorted(observed, key=lambda item: str(item["path"]))


def source_date(raw: str | None = None) -> str:
    raw = os.environ.get("SOURCE_DATE_EPOCH", "0") if raw is None else raw
    require(raw.isdecimal(), "SOURCE_DATE_EPOCH must be a non-negative integer")
    try:
        value = int(raw)
        instant = datetime.fromtimestamp(value, timezone.utc)
    except (OverflowError, OSError, ValueError) as exc:
        raise SBOMError("SOURCE_DATE_EPOCH is outside the supported range") from exc
    return instant.strftime("%Y-%m-%dT%H:%M:%SZ")


def canonical_go_purl(path: str, version: str) -> str:
    """Return a canonical Package URL while preserving module namespaces."""
    require(bool(path) and bool(version), "Go package URL identity is empty")
    segments = path.split("/")
    require(
        all(segment not in {"", ".", ".."} for segment in segments),
        f"Go package URL module path is invalid: {path}",
    )
    encoded_path = "/".join(quote(segment, safe=".-_~") for segment in segments)
    encoded_version = quote(version, safe=".-_~")
    return f"pkg:golang/{encoded_path}@{encoded_version}"


def spdx_package(module: dict[str, object], index: int) -> dict[str, object]:
    path, version = str(module["path"]), str(module["version"])
    expression = str(module["license_spdx"])
    return {
        "SPDXID": f"SPDXRef-Module-{index:04d}",
        "name": path,
        "versionInfo": version,
        "downloadLocation": module["source_url"],
        "filesAnalyzed": False,
        # The audited lock preserves upstream declared expressions and every
        # required license text. It deliberately does not claim file-level
        # analysis of generated/transitive source, so a concluded expression
        # would overstate the available evidence.
        "licenseConcluded": "NOASSERTION",
        "licenseDeclared": expression,
        "copyrightText": "NOASSERTION",
        "externalRefs": [
            {
                "referenceCategory": "PACKAGE-MANAGER",
                "referenceType": "purl",
                "referenceLocator": canonical_go_purl(path, version),
            }
        ],
    }


def extracted_license_info(
    modules: list[dict[str, object]], root: Path
) -> list[dict[str, object]]:
    """Resolve every non-standard SPDX LicenseRef to preserved audited text."""
    result: dict[str, dict[str, object]] = {}
    for module in modules:
        expression = str(module["license_spdx"])
        for license_id in LICENSE_REFERENCE.findall(expression):
            source_path = LICENSE_REFERENCE_SOURCES.get(license_id)
            require(source_path is not None, f"unsupported extracted license: {license_id}")
            licenses = module["licenses"]
            assert isinstance(licenses, list)
            candidates = [
                item
                for item in licenses
                if isinstance(item, dict) and item.get("source_path") == source_path
            ]
            require(len(candidates) == 1, f"extracted license source is ambiguous: {license_id}")
            tracked = canonical_repo_path(candidates[0]["path"], f"{license_id}.path")
            try:
                extracted = regular_bytes(
                    root / tracked, f"{license_id} text", 4 * 1024 * 1024
                ).decode("utf-8")
            except UnicodeDecodeError as exc:
                raise SBOMError(f"{license_id} text is not UTF-8") from exc
            entry = {
                "licenseId": license_id,
                "name": "SQLite public-domain dedication",
                "extractedText": extracted,
                "seeAlsos": [module["source_url"]],
            }
            previous = result.get(license_id)
            require(previous is None or previous == entry, f"extracted license drift: {license_id}")
            result[license_id] = entry
    return [result[key] for key in sorted(result)]


def generate(
    binary: Path,
    lock: Path,
    version: str,
    root: Path = ROOT,
    *,
    source_commit: str,
    source_tree: str,
    goos: str,
    goarch: str,
    source_date_epoch: str,
) -> dict[str, object]:
    """Generate one deterministic SPDX document without writing it."""
    require(bool(version) and "\n" not in version, "project version is invalid")
    require(GIT_OBJECT_ID.fullmatch(source_commit) is not None, "source commit is invalid")
    require(GIT_OBJECT_ID.fullmatch(source_tree) is not None, "source tree is invalid")
    require(PLATFORM_NAME.fullmatch(goos) is not None, "GOOS is invalid")
    require(PLATFORM_NAME.fullmatch(goarch) is not None, "GOARCH is invalid")
    commit_time = source_date(source_date_epoch)
    binary_digest = sha256_file(binary, "Go binary")
    locked, lock_digest, go_metadata = load_lock(lock, root)
    build_info, target_tuning = binary_build_info(
        binary,
        go_metadata["toolchain"],
        goos,
        goarch,
        source_commit,
        commit_time,
    )
    dependencies = dependency_inventory(build_info, locked)
    root_package = {
        "SPDXID": "SPDXRef-Package-RKC",
        "name": "Repository Knowledge Compiler",
        "versionInfo": version,
        "downloadLocation": "https://github.com/neuroforge-io/RKC",
        "filesAnalyzed": False,
        # SPDX 2.3 requires NOASSERTION when filesAnalyzed is false. The
        # repository's reviewed Apache-2.0 terms remain the declared license.
        "licenseConcluded": "NOASSERTION",
        "licenseDeclared": "Apache-2.0",
        "copyrightText": "Copyright 2026 RKC contributors",
        "sourceInfo": f"Git commit {source_commit}; Git tree {source_tree}",
        "comment": (
            f"Target platform: {goos}/{goarch}; normalized build settings: "
            + ", ".join(f"{key}={target_tuning[key]}" for key in sorted(target_tuning))
        ),
        "externalRefs": [
            {
                "referenceCategory": "OTHER",
                "referenceType": "vcs",
                "referenceLocator": "git+https://github.com/neuroforge-io/RKC.git@"
                + source_commit,
            }
        ],
    }
    packages = [root_package]
    packages.extend(spdx_package(module, index) for index, module in enumerate(dependencies, 1))
    relationships: list[dict[str, str]] = [
        {
            "spdxElementId": "SPDXRef-DOCUMENT",
            "relationshipType": "DESCRIBES",
            "relatedSpdxElement": "SPDXRef-Package-RKC",
        },
        {
            "spdxElementId": "SPDXRef-Package-RKC",
            "relationshipType": "CONTAINS",
            "relatedSpdxElement": "SPDXRef-File-Binary",
        },
    ]
    relationships.extend(
        {
            "spdxElementId": "SPDXRef-Package-RKC",
            "relationshipType": "DEPENDS_ON",
            "relatedSpdxElement": package["SPDXID"],
        }
        for package in packages[1:]
    )
    document: dict[str, object] = {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": f"RKC-{binary.name}-{version}",
        "documentNamespace": "https://neuroforge.io/rkc/spdx/"
        + quote(version, safe="")
        + "/"
        + binary_digest
        + "-"
        + lock_digest[:16]
        + "-"
        + source_tree[:16],
        "creationInfo": {
            "created": commit_time,
            "creators": ["Tool: RKC generate-go-sbom.py"],
        },
        "comment": (
            f"Audited Go module lock SHA-256: {lock_digest}; "
            f"source commit: {source_commit}; source tree: {source_tree}; "
            f"platform: {goos}/{goarch}; normalized build settings: "
            + ", ".join(f"{key}={target_tuning[key]}" for key in sorted(target_tuning))
        ),
        "packages": packages,
        "files": [
            {
                "SPDXID": "SPDXRef-File-Binary",
                "fileName": f"./{binary.name}",
                "checksums": [
                    {"algorithm": "SHA256", "checksumValue": binary_digest}
                ],
                "licenseConcluded": "NOASSERTION",
                "licenseInfoInFiles": ["NOASSERTION"],
                "copyrightText": "Copyright 2026 RKC contributors",
            }
        ],
        "relationships": relationships,
    }
    extracted = extracted_license_info(dependencies, root)
    if extracted:
        document["hasExtractedLicensingInfos"] = extracted
    require(
        sha256_file(binary, "Go binary") == binary_digest,
        "Go binary changed during SPDX generation",
    )
    return document


def verify_document(
    document_path: Path,
    binary: Path,
    lock: Path,
    version: str,
    root: Path = ROOT,
    *,
    source_commit: str,
    source_tree: str,
    goos: str,
    goarch: str,
    source_date_epoch: str,
) -> None:
    """Verify an SPDX document is the exact generator output for one binary."""
    payload = regular_bytes(document_path, "binary SPDX document", 16 * 1024 * 1024)
    try:
        document = json.loads(payload.decode("utf-8"), object_pairs_hook=strict_object)
    except SBOMError:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise SBOMError(f"binary SPDX document is not strict UTF-8 JSON: {exc}") from exc
    require(isinstance(document, dict), "binary SPDX document must be an object")
    expected = generate(
        binary,
        lock,
        version,
        root,
        source_commit=source_commit,
        source_tree=source_tree,
        goos=goos,
        goarch=goarch,
        source_date_epoch=source_date_epoch,
    )
    require(
        document == expected,
        "binary SPDX document does not exactly bind its binary, lock, and modules",
    )


def write_document(document: dict[str, object], output: Path, force: bool) -> None:
    """Atomically publish canonical UTF-8 JSON to a safe local path."""
    parent = output.parent
    try:
        parent_info = os.lstat(parent)
    except FileNotFoundError as exc:
        raise SBOMError(f"output parent is missing: {parent}") from exc
    require(stat.S_ISDIR(parent_info.st_mode) and not stat.S_ISLNK(parent_info.st_mode), "output parent must be a real directory")
    require(parent.resolve(strict=True) == parent.absolute(), "output parent path crosses a symbolic link")
    if output.exists() or output.is_symlink():
        info = os.lstat(output)
        require(stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode), "output must be a regular file")
        require(force, f"output exists; pass --force to replace it: {output}")
    payload = (json.dumps(document, indent=2, sort_keys=True) + "\n").encode("utf-8")
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{output.name}.", dir=parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o644)
        os.replace(temporary, output)
    finally:
        if temporary.exists():
            temporary.unlink()


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--binary", required=True, type=Path)
    destination = result.add_mutually_exclusive_group(required=True)
    destination.add_argument("--output", type=Path)
    destination.add_argument("--verify-document", type=Path)
    result.add_argument("--lock", type=Path, default=DEFAULT_LOCK)
    result.add_argument("--source-root", type=Path, default=ROOT)
    result.add_argument("--source-commit", required=True)
    result.add_argument("--source-tree", required=True)
    result.add_argument("--goos", required=True)
    result.add_argument("--goarch", required=True)
    result.add_argument("--source-date-epoch", required=True)
    result.add_argument("--version", default=(ROOT / "VERSION").read_text(encoding="utf-8").strip())
    result.add_argument("--force", action="store_true")
    return result


def main() -> int:
    arguments = parser().parse_args()
    try:
        binary = arguments.binary.absolute()
        lock = arguments.lock.absolute()
        source_root = arguments.source_root.absolute()
        if arguments.verify_document is not None:
            require(not arguments.force, "--force is invalid with --verify-document")
            verify_document(
                arguments.verify_document.absolute(),
                binary,
                lock,
                arguments.version,
                source_root,
                source_commit=arguments.source_commit,
                source_tree=arguments.source_tree,
                goos=arguments.goos,
                goarch=arguments.goarch,
                source_date_epoch=arguments.source_date_epoch,
            )
        else:
            assert arguments.output is not None
            document = generate(
                binary,
                lock,
                arguments.version,
                source_root,
                source_commit=arguments.source_commit,
                source_tree=arguments.source_tree,
                goos=arguments.goos,
                goarch=arguments.goarch,
                source_date_epoch=arguments.source_date_epoch,
            )
            write_document(document, arguments.output.absolute(), arguments.force)
    except SBOMError as exc:
        print(f"generate-go-sbom: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
