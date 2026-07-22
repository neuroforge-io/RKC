#!/usr/bin/env python3
"""Check local Markdown links and fenced-code balance without network access."""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parents[1]
EXCLUDED = {".git", "bin", "dist", ".rkc", ".rkc-smoke", ".rkc-state", ".rkc-state-smoke"}
issues: list[dict[str, object]] = []
checked = 0


def report(path: Path, line: int, message: str) -> None:
    issues.append({"path": str(path.relative_to(ROOT)), "line": line, "message": message})


def markdown_files():
    for path in sorted(ROOT.rglob("*.md")):
        if any(part in EXCLUDED or part.startswith(".rkc-") for part in path.relative_to(ROOT).parts):
            continue
        yield path

link_pattern = re.compile(r"(?<!!)\[[^\]]*\]\(([^)]+)\)")
for path in markdown_files():
    checked += 1
    lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    fence: str | None = None
    for number, line in enumerate(lines, 1):
        stripped = line.lstrip()
        marker = None
        if stripped.startswith("```"):
            marker = "```"
        elif stripped.startswith("~~~"):
            marker = "~~~"
        if marker:
            if fence is None:
                fence = marker
            elif fence == marker:
                fence = None
            continue
        if fence is not None:
            continue
        for match in link_pattern.finditer(line):
            raw = match.group(1).strip()
            if raw.startswith("<") and raw.endswith(">"):
                raw = raw[1:-1]
            # Optional title follows a whitespace-delimited quoted string.
            target = re.split(r'\s+["\']', raw, maxsplit=1)[0]
            if not target or target.startswith(("http://", "https://", "mailto:", "tel:", "#", "sandbox:", "data:")):
                continue
            target = unquote(target.split("#", 1)[0].split("?", 1)[0])
            if not target:
                continue
            candidate = (path.parent / target).resolve()
            try:
                candidate.relative_to(ROOT.resolve())
            except ValueError:
                report(path, number, f"local link escapes repository: {raw}")
                continue
            if not candidate.exists():
                report(path, number, f"missing local link target: {raw}")
    if fence is not None:
        report(path, len(lines), f"unclosed {fence} code fence")

result = {"schema_version": "1.0", "ok": not issues, "files_checked": checked, "issues": issues}
print(json.dumps(result, indent=2, sort_keys=True))
if issues:
    sys.exit(1)
