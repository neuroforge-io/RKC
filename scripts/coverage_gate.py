#!/usr/bin/env python3
"""Run and enforce RKC's first-party Go and Python coverage policy.

The Go compiler emits one copy of an instrumented block for every test binary
that includes the package.  Adding those profile rows directly repeats both
the numerator and denominator.  This gate instead identifies a block by its
canonical source coordinates, keeps it covered if any test binary covered it,
and counts its statements exactly once.

Python is measured with branch coverage.  Coverage.py's subprocess patch is
enabled so Python children participate in the same parallel data set.  A child
command failure is recorded independently and can never be hidden by a passing
percentage.
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import re
import secrets
import shlex
import stat
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_OUTPUT = ROOT / ".rkc-coverage"
DEFAULT_GO_OVERALL_MINIMUM = 90.0
DEFAULT_GO_PACKAGE_MINIMUM = 80.0
DEFAULT_PYTHON_OVERALL_MINIMUM = 90.0
DEFAULT_PYTHON_FILE_MINIMUM = 80.0
GENERATED_RE = re.compile(r"^// Code generated .* DO NOT EDIT\.$")
PYTHON_GENERATED_RE = re.compile(r"^# Code generated .* DO NOT EDIT\.$")
PROFILE_RE = re.compile(
    r"^(?P<file>.+):(?P<sl>\d+)\.(?P<sc>\d+),(?P<el>\d+)\.(?P<ec>\d+) "
    r"(?P<statements>\d+) (?P<count>\d+)$"
)


class GateError(RuntimeError):
    """A fail-closed coverage inventory or profile error."""


@dataclass(frozen=True)
class GoPackage:
    """One current-platform, first-party Go package."""

    import_path: str
    directory: Path
    source_files: tuple[Path, ...]
    generated_files: tuple[Path, ...]
    ignored_files: tuple[Path, ...]


@dataclass
class GoBlock:
    """One unique instrumented Go source block."""

    relative_file: str
    start_line: int
    start_column: int
    end_line: int
    end_column: int
    statements: int
    covered: bool

    @property
    def key(self) -> tuple[object, ...]:
        return (
            self.relative_file,
            self.start_line,
            self.start_column,
            self.end_line,
            self.end_column,
            self.statements,
        )

    def profile_line(self) -> str:
        count = 1 if self.covered else 0
        return (
            f"{self.relative_file}:{self.start_line}.{self.start_column},"
            f"{self.end_line}.{self.end_column} {self.statements} {count}"
        )


@dataclass(frozen=True)
class CommandSpec:
    """A measured Python command and its optional standard input."""

    arguments: tuple[str, ...]
    input_text: str | None = None
    capture_json: bool = False


def _relative(path: Path, root: Path = ROOT) -> str:
    try:
        return path.resolve(strict=False).relative_to(root.resolve()).as_posix()
    except ValueError as exc:
        raise GateError(f"path escapes repository root: {path}") from exc


def _is_generated(path: Path, pattern: re.Pattern[str]) -> bool:
    try:
        with path.open("r", encoding="utf-8", errors="replace") as handle:
            for _ in range(20):
                line = handle.readline()
                if not line:
                    return False
                if pattern.match(line.rstrip("\r\n")):
                    return True
    except OSError as exc:
        raise GateError(f"cannot read source inventory entry {path}: {exc}") from exc
    return False


def _json_stream(text: str) -> list[object]:
    """Decode the concatenated JSON objects produced by ``go list -json``."""
    decoder = json.JSONDecoder()
    offset = 0
    values: list[object] = []
    while True:
        while offset < len(text) and text[offset].isspace():
            offset += 1
        if offset == len(text):
            return values
        try:
            value, offset = decoder.raw_decode(text, offset)
        except json.JSONDecodeError as exc:
            raise GateError(f"invalid go list JSON at byte {offset}: {exc}") from exc
        values.append(value)


def discover_go_packages(
    root: Path = ROOT,
    *,
    go_executable: str = "go",
) -> tuple[str, list[GoPackage]]:
    """Discover every current-platform package in the main Go module."""
    result = subprocess.run(
        [go_executable, "list", "-json", "./..."],
        cwd=root,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip()
        raise GateError(f"go list failed ({result.returncode}): {detail}")

    packages: list[GoPackage] = []
    module_path = ""
    root_resolved = root.resolve()
    for raw in _json_stream(result.stdout):
        if not isinstance(raw, dict):
            raise GateError("go list returned a non-object package")
        module = raw.get("Module")
        if not isinstance(module, dict) or module.get("Main") is not True:
            raise GateError(f"non-main-module package entered ./...: {raw.get('ImportPath')}")
        current_module = module.get("Path")
        if not isinstance(current_module, str) or not current_module:
            raise GateError("go list package has no main module path")
        if module_path and module_path != current_module:
            raise GateError("go list returned inconsistent main module paths")
        module_path = current_module

        import_path = raw.get("ImportPath")
        directory_value = raw.get("Dir")
        if not isinstance(import_path, str) or not import_path:
            raise GateError("go list package has no import path")
        if not isinstance(directory_value, str) or not directory_value:
            raise GateError(f"go list package has no directory: {import_path}")
        directory = Path(directory_value).resolve()
        try:
            directory.relative_to(root_resolved)
        except ValueError as exc:
            raise GateError(f"first-party package escapes repository: {directory}") from exc

        current: list[Path] = []
        generated: list[Path] = []
        for name in [*(raw.get("GoFiles") or []), *(raw.get("CgoFiles") or [])]:
            if not isinstance(name, str) or Path(name).name != name:
                raise GateError(f"invalid Go source name in {import_path}: {name!r}")
            path = directory / name
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise GateError(f"Go source is not a regular file: {_relative(path, root)}")
            if _is_generated(path, GENERATED_RE):
                generated.append(path)
            else:
                current.append(path)

        ignored: list[Path] = []
        for name in raw.get("IgnoredGoFiles") or []:
            if not isinstance(name, str) or Path(name).name != name:
                raise GateError(f"invalid ignored Go source name in {import_path}: {name!r}")
            path = directory / name
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise GateError(f"ignored Go source is not regular: {_relative(path, root)}")
            if not _is_generated(path, GENERATED_RE):
                ignored.append(path)

        if not current and not generated:
            raise GateError(f"Go package contains no current source files: {import_path}")
        packages.append(
            GoPackage(
                import_path=import_path,
                directory=directory,
                source_files=tuple(sorted(current)),
                generated_files=tuple(sorted(generated)),
                ignored_files=tuple(sorted(ignored)),
            )
        )

    if not module_path or not packages:
        raise GateError("go list discovered no first-party packages")
    packages.sort(key=lambda item: item.import_path)
    return module_path, packages


def _profile_file_map(
    module_path: str,
    packages: Sequence[GoPackage],
    root: Path,
) -> tuple[dict[str, tuple[str, str]], set[str]]:
    mapping: dict[str, tuple[str, str]] = {}
    generated: set[str] = set()
    for package in packages:
        for path in [*package.source_files, *package.generated_files]:
            relative = _relative(path, root)
            value = (relative, package.import_path)
            keys = {
                relative,
                path.resolve().as_posix(),
                f"{module_path}/{relative}",
                f"{package.import_path}/{path.name}",
            }
            for key in keys:
                prior = mapping.get(key)
                if prior is not None and prior != value:
                    raise GateError(f"ambiguous Go profile path mapping: {key}")
                mapping[key] = value
            if path in package.generated_files:
                generated.add(relative)
    return mapping, generated


def parse_go_profile(
    profile: Path,
    module_path: str,
    packages: Sequence[GoPackage],
    root: Path = ROOT,
) -> dict[str, object]:
    """Parse, deduplicate, and summarize a Go coverage profile."""
    try:
        lines = profile.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        raise GateError(f"cannot read Go coverage profile {profile}: {exc}") from exc
    if not lines or not lines[0].startswith("mode: "):
        raise GateError("Go coverage profile has no mode header")
    mode = lines[0].removeprefix("mode: ").strip()
    if mode not in {"set", "count", "atomic"}:
        raise GateError(f"unsupported Go coverage mode: {mode}")

    file_map, generated_files = _profile_file_map(module_path, packages, root)
    file_to_package = {
        _relative(path, root): package.import_path
        for package in packages
        for path in package.source_files
    }
    blocks: dict[tuple[object, ...], GoBlock] = {}
    coordinate_statements: dict[tuple[object, ...], int] = {}
    raw_records = 0
    generated_records = 0
    for number, line in enumerate(lines[1:], 2):
        if not line.strip():
            continue
        raw_records += 1
        match = PROFILE_RE.fullmatch(line)
        if match is None:
            raise GateError(f"invalid Go coverage row {number}: {line!r}")
        profile_name = match.group("file").replace("\\", "/")
        mapped = file_map.get(profile_name)
        if mapped is None:
            raise GateError(f"Go profile contains an unowned source file: {profile_name}")
        relative_file, _package = mapped
        if relative_file in generated_files:
            generated_records += 1
            continue
        block = GoBlock(
            relative_file=relative_file,
            start_line=int(match.group("sl")),
            start_column=int(match.group("sc")),
            end_line=int(match.group("el")),
            end_column=int(match.group("ec")),
            statements=int(match.group("statements")),
            covered=int(match.group("count")) > 0,
        )
        if (
            block.start_line > block.end_line
            or (block.start_line == block.end_line and block.start_column > block.end_column)
        ):
            raise GateError(f"Go profile has reversed coordinates on row {number}")
        coordinate_key = block.key[:-1]
        previous_statements = coordinate_statements.get(coordinate_key)
        if previous_statements is not None and previous_statements != block.statements:
            raise GateError(
                f"Go profile changes the statement count for one block on row {number}"
            )
        coordinate_statements[coordinate_key] = block.statements
        prior = blocks.get(block.key)
        if prior is None:
            blocks[block.key] = block
        else:
            prior.covered = prior.covered or block.covered

    if not blocks:
        raise GateError("Go coverage profile contains no first-party executable blocks")

    package_counts: dict[str, list[int]] = {
        package.import_path: [0, 0] for package in packages
    }
    files_with_blocks: set[str] = set()
    total_statements = 0
    covered_statements = 0
    for block in blocks.values():
        package = file_to_package.get(block.relative_file)
        if package is None:
            raise GateError(f"non-generated block has no package: {block.relative_file}")
        files_with_blocks.add(block.relative_file)
        if block.statements == 0:
            continue
        total_statements += block.statements
        package_counts[package][1] += block.statements
        if block.covered:
            covered_statements += block.statements
            package_counts[package][0] += block.statements

    if total_statements == 0:
        raise GateError("Go coverage profile has zero executable statements")
    package_rows: list[dict[str, object]] = []
    for package in packages:
        covered, total = package_counts[package.import_path]
        percent = 100.0 if total == 0 else covered * 100.0 / total
        package_rows.append(
            {
                "package": package.import_path,
                "covered_statements": covered,
                "statements": total,
                "percent": round(percent, 4),
                "threshold_applicable": total > 0,
                "source_files": [_relative(path, root) for path in package.source_files],
            }
        )
    all_source_files = {
        _relative(path, root) for package in packages for path in package.source_files
    }
    return {
        "mode": mode,
        "raw_records": raw_records,
        "unique_blocks": len(blocks),
        "deduplicated_records": raw_records - generated_records - len(blocks),
        "generated_records_excluded": generated_records,
        "covered_statements": covered_statements,
        "statements": total_statements,
        "percent": round(covered_statements * 100.0 / total_statements, 4),
        "packages": package_rows,
        "source_files": sorted(all_source_files),
        "zero_statement_files": sorted(all_source_files - files_with_blocks),
        "blocks": list(blocks.values()),
    }


def write_merged_go_profile(path: Path, blocks: Iterable[GoBlock]) -> None:
    """Write a stable set-mode profile containing each source block once."""
    ordered = sorted(blocks, key=lambda block: block.key)
    payload = "mode: set\n" + "".join(block.profile_line() + "\n" for block in ordered)
    path.write_text(payload, encoding="utf-8")


def evaluate_go(
    report: Mapping[str, object],
    *,
    overall_minimum: float,
    package_minimum: float,
) -> list[str]:
    failures: list[str] = []
    overall = float(report["percent"])
    if overall + 1e-9 < overall_minimum:
        failures.append(f"Go overall {overall:.2f}% is below {overall_minimum:.2f}%")
    packages = report.get("packages")
    if not isinstance(packages, list):
        return [*failures, "Go report has no package metrics"]
    for row in packages:
        if not isinstance(row, dict) or not row.get("threshold_applicable"):
            continue
        percent = float(row["percent"])
        if percent + 1e-9 < package_minimum:
            failures.append(
                f"Go package {row['package']} {percent:.2f}% is below "
                f"{package_minimum:.2f}%"
            )
    return failures


def discover_python_sources(root: Path = ROOT) -> list[str]:
    """Inventory non-test, non-generated first-party Python sources."""
    sources: list[str] = []
    for directory_name in ("internal", "plugins", "scripts"):
        directory = root / directory_name
        for path in sorted(directory.rglob("*.py")):
            relative_path = path.relative_to(root)
            if "__pycache__" in relative_path.parts or path.name.startswith("test_"):
                continue
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise GateError(f"Python source is not a regular file: {relative_path.as_posix()}")
            if not _is_generated(path, PYTHON_GENERATED_RE):
                sources.append(relative_path.as_posix())
    if not sources:
        raise GateError("no first-party Python source files were discovered")
    return sources


def parse_python_report(
    report_path: Path,
    expected_sources: Sequence[str],
) -> dict[str, object]:
    """Validate Coverage.py JSON and return branch-aware file metrics."""
    try:
        raw = json.loads(report_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise GateError(f"cannot read Python coverage report {report_path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise GateError("Python coverage report is not an object")
    meta = raw.get("meta")
    if not isinstance(meta, dict) or meta.get("branch_coverage") is not True:
        raise GateError("Python coverage report did not enable branch coverage")
    files = raw.get("files")
    if not isinstance(files, dict):
        raise GateError("Python coverage report has no file map")
    normalized: dict[str, object] = {}
    for key, value in files.items():
        name = str(key).replace("\\", "/")
        candidate = Path(name)
        if candidate.is_absolute():
            name = _relative(candidate)
        if name in normalized:
            raise GateError(f"Python coverage repeats a source file: {name}")
        normalized[name] = value
    missing = sorted(set(expected_sources) - set(normalized))
    unexpected = sorted(set(normalized) - set(expected_sources))
    if missing:
        raise GateError("Python coverage omitted source files: " + ", ".join(missing))
    if unexpected:
        raise GateError("Python coverage included non-production files: " + ", ".join(unexpected))

    rows: list[dict[str, object]] = []
    total_units = 0
    covered_units = 0
    for name in sorted(expected_sources):
        entry = normalized[name]
        if not isinstance(entry, dict) or not isinstance(entry.get("summary"), dict):
            raise GateError(f"Python coverage entry has no summary: {name}")
        summary = entry["summary"]
        statements = int(summary.get("num_statements", -1))
        covered_lines = int(summary.get("covered_lines", -1))
        branches = int(summary.get("num_branches", -1))
        covered_branches = int(summary.get("covered_branches", -1))
        if min(statements, covered_lines, branches, covered_branches) < 0:
            raise GateError(f"Python coverage entry has invalid counters: {name}")
        units = statements + branches
        covered = covered_lines + covered_branches
        if covered > units:
            raise GateError(f"Python coverage entry exceeds its denominator: {name}")
        percent = 100.0 if units == 0 else covered * 100.0 / units
        rows.append(
            {
                "file": name,
                "covered_lines": covered_lines,
                "statements": statements,
                "covered_branches": covered_branches,
                "branches": branches,
                "covered_units": covered,
                "units": units,
                "percent": round(percent, 4),
                "threshold_applicable": units > 0,
            }
        )
        total_units += units
        covered_units += covered
    if total_units == 0:
        raise GateError("Python coverage inventory has zero executable units")
    return {
        "branch_coverage": True,
        "covered_units": covered_units,
        "units": total_units,
        "percent": round(covered_units * 100.0 / total_units, 4),
        "files": rows,
    }


def evaluate_python(
    report: Mapping[str, object],
    *,
    overall_minimum: float,
    file_minimum: float,
) -> list[str]:
    failures: list[str] = []
    overall = float(report["percent"])
    if overall + 1e-9 < overall_minimum:
        failures.append(f"Python overall {overall:.2f}% is below {overall_minimum:.2f}%")
    files = report.get("files")
    if not isinstance(files, list):
        return [*failures, "Python report has no file metrics"]
    for row in files:
        if not isinstance(row, dict) or not row.get("threshold_applicable"):
            continue
        percent = float(row["percent"])
        if percent + 1e-9 < file_minimum:
            failures.append(
                f"Python file {row['file']} {percent:.2f}% is below {file_minimum:.2f}%"
            )
    return failures


def run_visible(
    arguments: Sequence[str],
    *,
    cwd: Path = ROOT,
    environment: Mapping[str, str] | None = None,
    input_text: str | None = None,
    stdout_path: Path | None = None,
) -> int:
    """Run a child transparently and return its unmodified exit status."""
    print("+ " + shlex.join(arguments), flush=True)
    output_handle = None
    try:
        if stdout_path is not None:
            output_handle = stdout_path.open("x", encoding="utf-8")
        result = subprocess.run(
            list(arguments),
            cwd=cwd,
            env=dict(environment) if environment is not None else None,
            input=input_text,
            stdout=output_handle,
            text=True,
            check=False,
        )
    finally:
        if output_handle is not None:
            output_handle.close()
    if result.returncode != 0:
        print(f"coverage child failed with status {result.returncode}", file=sys.stderr)
    return result.returncode


def run_logged(
    arguments: Sequence[str],
    log_path: Path,
    *,
    cwd: Path = ROOT,
) -> int:
    """Run a noisy child into an evidence log and expose bounded failure detail."""
    print("+ " + shlex.join(arguments), flush=True)
    with log_path.open("x", encoding="utf-8") as log:
        result = subprocess.run(
            list(arguments),
            cwd=cwd,
            stdout=log,
            stderr=subprocess.STDOUT,
            text=True,
            check=False,
        )
    if result.returncode != 0:
        print(
            f"coverage child failed with status {result.returncode}; log: {log_path}",
            file=sys.stderr,
        )
        with log_path.open("r", encoding="utf-8", errors="replace") as log:
            tail = collections.deque(log, maxlen=80)
        print("--- bounded child log tail ---", file=sys.stderr)
        for line in tail:
            print(line.rstrip("\n"), file=sys.stderr)
        print("--- end child log tail ---", file=sys.stderr)
    else:
        print(f"coverage child log: {log_path}")
    return result.returncode


def _coverage_command(
    python: str,
    config: Path,
    arguments: Sequence[str],
) -> list[str]:
    return [
        python,
        "-m",
        "coverage",
        "run",
        f"--rcfile={config}",
        "--parallel-mode",
        *arguments,
    ]


def _extractor_request(root: Path) -> str:
    request = {
        "root": str((root / "examples" / "sample-python").resolve()),
        "files": [
            {"id": "rkc:artifact:auth", "path": "auth.py", "language": "python"},
            {
                "id": "rkc:artifact:test-auth",
                "path": "test_auth.py",
                "language": "python",
            },
        ],
    }
    return json.dumps(request, sort_keys=True)


def python_commands(root: Path = ROOT) -> list[CommandSpec]:
    """Return safe, deterministic commands used to measure Python sources."""
    extractor_request = _extractor_request(root)
    return [
        CommandSpec(
            (
                "-m",
                "unittest",
                "discover",
                "-s",
                "plugins/python-ast",
                "-p",
                "test_*.py",
                "-v",
            )
        ),
        CommandSpec(("-m", "unittest", "discover", "-s", "scripts", "-p", "test_*.py", "-v")),
        CommandSpec(("plugins/python-ast/extractor.py",), extractor_request, True),
        CommandSpec(
            ("internal/builtinplugins/python_extractor.py",), extractor_request, True
        ),
        CommandSpec(("scripts/validate-contracts.py",)),
        CommandSpec(("scripts/validate-docs.py",)),
        CommandSpec(("scripts/validate-licenses.py",)),
        CommandSpec(("scripts/model_assets.py", "validate-lock")),
        CommandSpec(("scripts/bootstrap_llama_cpp.py", "--help")),
        CommandSpec(("scripts/package-complete.py", "--help")),
        CommandSpec(("scripts/publish_file.py", "--help")),
        CommandSpec(("scripts/qualify_models.py", "--help")),
        CommandSpec(("scripts/coverage_gate.py", "--help")),
    ]


def write_coverage_config(path: Path, data_file: Path) -> None:
    """Create an isolated branch/subprocess Coverage.py configuration."""
    payload = f"""[run]
branch = True
parallel = True
relative_files = True
data_file = {data_file}
source =
    internal
    plugins
    scripts
patch =
    subprocess

[report]
omit =
    */test_*.py
    */__pycache__/*
skip_empty = False
show_missing = True
"""
    path.write_text(payload, encoding="utf-8")


def _make_run_directory(output: Path) -> Path:
    if output.exists():
        info = os.lstat(output)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise GateError(f"coverage output is not a real directory: {output}")
    else:
        output.mkdir(mode=0o700, parents=True)
    name = f"run-{os.getpid()}-{secrets.token_hex(8)}"
    run = output / name
    run.mkdir(mode=0o700)
    return run


def _validate_minimum(value: float, label: str) -> float:
    if value < 0.0 or value > 100.0:
        raise argparse.ArgumentTypeError(f"{label} must be between 0 and 100")
    return value


def preferred_python(root: Path = ROOT) -> str:
    """Prefer the repository's dependency-locked development interpreter."""
    relative = Path("Scripts/python.exe") if os.name == "nt" else Path("bin/python")
    candidate = root / ".venv" / relative
    if candidate.is_file() and os.access(candidate, os.X_OK):
        return str(candidate.absolute())
    return sys.executable


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument(
        "--python",
        default=preferred_python(),
        help="coverage interpreter (defaults to dependency-locked .venv when available)",
    )
    parser.add_argument("--go", default="go")
    parser.add_argument("--go-overall-min", type=float, default=DEFAULT_GO_OVERALL_MINIMUM)
    parser.add_argument("--go-package-min", type=float, default=DEFAULT_GO_PACKAGE_MINIMUM)
    parser.add_argument(
        "--python-overall-min", type=float, default=DEFAULT_PYTHON_OVERALL_MINIMUM
    )
    parser.add_argument("--python-file-min", type=float, default=DEFAULT_PYTHON_FILE_MINIMUM)
    return parser


def _print_summary(go_report: Mapping[str, object], python_report: Mapping[str, object]) -> None:
    print(
        f"Go statement coverage: {float(go_report['percent']):.2f}% "
        f"({go_report['covered_statements']}/{go_report['statements']})"
    )
    for row in go_report["packages"]:  # type: ignore[index]
        if isinstance(row, dict):
            suffix = (
                "no executable statements"
                if not row["threshold_applicable"]
                else f"{float(row['percent']):.2f}%"
            )
            print(f"  {row['package']}: {suffix}")
    print(
        f"Python line+branch coverage: {float(python_report['percent']):.2f}% "
        f"({python_report['covered_units']}/{python_report['units']})"
    )
    for row in python_report["files"]:  # type: ignore[index]
        if isinstance(row, dict):
            suffix = (
                "no executable units"
                if not row["threshold_applicable"]
                else f"{float(row['percent']):.2f}%"
            )
            print(f"  {row['file']}: {suffix}")


def main(argv: Sequence[str] | None = None) -> int:
    arguments = build_parser().parse_args(argv)
    for value, label in (
        (arguments.go_overall_min, "--go-overall-min"),
        (arguments.go_package_min, "--go-package-min"),
        (arguments.python_overall_min, "--python-overall-min"),
        (arguments.python_file_min, "--python-file-min"),
    ):
        try:
            _validate_minimum(value, label)
        except argparse.ArgumentTypeError as exc:
            print(f"coverage gate: {exc}", file=sys.stderr)
            return 2

    failures: list[str] = []
    try:
        run_directory = _make_run_directory(arguments.output_dir)
        module_path, packages = discover_go_packages(go_executable=arguments.go)
        raw_profile = run_directory / "go.raw.out"
        cover_packages = ",".join(package.import_path for package in packages)
        go_command = [
            arguments.go,
            "test",
            "-p=1",
            "-count=1",
            "-covermode=atomic",
            f"-coverpkg={cover_packages}",
            f"-coverprofile={raw_profile}",
            *(package.import_path for package in packages),
        ]
        go_log = run_directory / "go-test.log"
        go_status = run_logged(go_command, go_log)
        if go_status != 0:
            failures.append(f"Go test subprocess failed with status {go_status}")
        go_report = parse_go_profile(raw_profile, module_path, packages)
        merged_profile = run_directory / "go.merged.out"
        write_merged_go_profile(merged_profile, go_report.pop("blocks"))  # type: ignore[arg-type]
        go_report["generated_files_excluded"] = sorted(
            _relative(path) for package in packages for path in package.generated_files
        )
        go_report["current_platform_excluded_files"] = sorted(
            _relative(path) for package in packages for path in package.ignored_files
        )
        go_report["test_log"] = go_log.name
        failures.extend(
            evaluate_go(
                go_report,
                overall_minimum=arguments.go_overall_min,
                package_minimum=arguments.go_package_min,
            )
        )

        expected_python = discover_python_sources()
        coverage_config = run_directory / "coverage.ini"
        coverage_data = run_directory / ".coverage"
        python_json = run_directory / "python.json"
        write_coverage_config(coverage_config, coverage_data)
        environment = os.environ.copy()
        environment["COVERAGE_FILE"] = str(coverage_data)
        for index, command in enumerate(python_commands(), 1):
            captured = (
                run_directory / f"python-command-{index:02d}.json"
                if command.capture_json
                else None
            )
            status = run_visible(
                _coverage_command(arguments.python, coverage_config, command.arguments),
                environment=environment,
                input_text=command.input_text,
                stdout_path=captured,
            )
            if status != 0:
                failures.append(
                    f"Python coverage subprocess failed ({status}): "
                    + shlex.join(command.arguments)
                )
            elif captured is not None:
                try:
                    captured_payload = json.loads(captured.read_text(encoding="utf-8"))
                    if not isinstance(captured_payload, dict):
                        raise ValueError("top-level output is not an object")
                except (OSError, json.JSONDecodeError, ValueError) as exc:
                    failures.append(
                        f"Python coverage subprocess emitted invalid JSON: "
                        f"{shlex.join(command.arguments)}: {exc}"
                    )
        combine_status = run_visible(
            [
                arguments.python,
                "-m",
                "coverage",
                "combine",
                f"--rcfile={coverage_config}",
                "--keep",
                str(run_directory),
            ],
            environment=environment,
        )
        if combine_status != 0:
            failures.append(f"coverage combine failed with status {combine_status}")
        json_status = run_visible(
            [
                arguments.python,
                "-m",
                "coverage",
                "json",
                f"--rcfile={coverage_config}",
                "-o",
                str(python_json),
            ],
            environment=environment,
        )
        if json_status != 0:
            failures.append(f"coverage json failed with status {json_status}")
        python_report = parse_python_report(python_json, expected_python)
        failures.extend(
            evaluate_python(
                python_report,
                overall_minimum=arguments.python_overall_min,
                file_minimum=arguments.python_file_min,
            )
        )

        summary = {
            "schema_version": "1.0.0",
            "ok": not failures,
            "policy": {
                "go_statement_overall_minimum_percent": arguments.go_overall_min,
                "go_statement_per_package_minimum_percent": arguments.go_package_min,
                "python_line_and_branch_overall_minimum_percent": arguments.python_overall_min,
                "python_line_and_branch_per_file_minimum_percent": arguments.python_file_min,
            },
            "go": go_report,
            "python": python_report,
            "failures": failures,
        }
        summary_path = run_directory / "summary.json"
        summary_path.write_text(
            json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        _print_summary(go_report, python_report)
        print(f"Coverage evidence: {summary_path}")
    except (GateError, OSError) as exc:
        print(f"coverage gate failed closed: {exc}", file=sys.stderr)
        return 1

    if failures:
        print("Coverage policy failures:", file=sys.stderr)
        for failure in failures:
            print(f"- {failure}", file=sys.stderr)
        return 1
    print("Coverage gate: passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
