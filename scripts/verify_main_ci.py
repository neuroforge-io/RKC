#!/usr/bin/env python3
"""Bind a tagged portable build to the latest exact-source main CI result."""
from __future__ import annotations

import argparse
import http.client
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

API = "https://api.github.com"
WORKFLOW = ".github/workflows/ci.yml"
MAX_RESPONSE_BYTES = 2 * 1024 * 1024
MAX_RUNS = 100
REQUEST_TIMEOUT = 15


class VerificationError(RuntimeError):
    """CI identity or its successful completion could not be established."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """Never forward a release credential to a redirected endpoint."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise VerificationError("GitHub API returned duplicate JSON fields")
        result[key] = value
    return result


def get_json(path: str, token: str) -> object:
    """Read at most one bounded response; errors never include headers or bodies."""
    request = urllib.request.Request(API + path, headers={
        "Accept": "application/vnd.github+json",
        "Authorization": "Bearer " + token,
        "User-Agent": "RKC-release-qualification",
        "X-GitHub-Api-Version": "2022-11-28",
    })
    try:
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=REQUEST_TIMEOUT) as response:
            if response.status != 200:
                raise VerificationError("GitHub API did not return HTTP 200")
            body = response.read(MAX_RESPONSE_BYTES + 1)
    except (urllib.error.URLError, OSError, ValueError, http.client.HTTPException):
        # Transport exceptions and response bodies may contain credentials.
        raise VerificationError("GitHub API request failed; CI proof is unavailable") from None
    if len(body) > MAX_RESPONSE_BYTES:
        raise VerificationError("GitHub API response exceeds the size bound")
    try:
        return json.loads(body.decode("utf-8"), object_pairs_hook=unique_object)
    except (ValueError, UnicodeError, RecursionError):
        raise VerificationError("GitHub API returned invalid JSON") from None


def positive_integer(value: object) -> bool:
    return type(value) is int and 0 < value < 2**63


def validate_run(run: object, repository: str, commit: str) -> dict:
    """Check even server-filtered fields before selecting or recording a run."""
    if not isinstance(run, dict):
        raise VerificationError("GitHub API returned an invalid CI run")
    if any(not positive_integer(run.get(key)) for key in ("id", "run_number", "run_attempt", "workflow_id")):
        raise VerificationError("CI run identifiers are invalid")
    expected = {"head_sha": commit, "head_branch": "main", "event": "push", "path": WORKFLOW}
    if any(run.get(key) != value for key, value in expected.items()):
        raise VerificationError("CI run does not match the exact main push and workflow")
    source, head = run.get("repository"), run.get("head_repository")
    if not isinstance(source, dict) or not isinstance(head, dict) or \
            source.get("full_name") != repository or head.get("full_name") != repository or \
            not positive_integer(source.get("id")) or not positive_integer(head.get("id")) or head.get("id") != source["id"]:
        raise VerificationError("CI run repository identity does not match")
    if run.get("html_url") != f"https://github.com/{repository}/actions/runs/{run['id']}":
        raise VerificationError("CI run URL does not match its repository and identifier")
    return run


def verify(repository: str, commit: str, token: str) -> dict:
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*", repository):
        raise VerificationError("repository must be an owner/name pair")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise VerificationError("source commit must be a full lowercase Git SHA")
    if not token or len(token) > 4096 or any(ord(character) < 33 or ord(character) > 126 for character in token):
        raise VerificationError("a valid RKC_GITHUB_TOKEN environment credential is required")
    query = urllib.parse.urlencode({"branch": "main", "event": "push", "head_sha": commit, "per_page": MAX_RUNS})
    result = get_json(f"/repos/{repository}/actions/workflows/ci.yml/runs?{query}", token)
    if not isinstance(result, dict) or type(result.get("total_count")) is not int or \
            not isinstance(result.get("workflow_runs"), list):
        raise VerificationError("GitHub API returned an invalid CI run collection")
    rows = result["workflow_runs"]
    if not 0 < result["total_count"] <= MAX_RUNS or len(rows) != result["total_count"]:
        raise VerificationError("exact-source main CI runs are absent or exceed the query bound")
    runs = [validate_run(row, repository, commit) for row in rows]
    if len({run["id"] for run in runs}) != len(runs) or \
            len({run["run_number"] for run in runs}) != len(runs) or \
            len({run["workflow_id"] for run in runs}) != 1:
        raise VerificationError("CI run collection has inconsistent identities")
    # run_number increments for a new workflow run; reruns keep that number.
    # Do not filter on success: a newer failure must supersede an older pass.
    latest = max(runs, key=lambda run: run["run_number"])
    current = validate_run(get_json(f"/repos/{repository}/actions/runs/{latest['id']}", token), repository, commit)
    if any(current[key] != latest[key] for key in ("id", "run_number", "workflow_id")) or \
            current["repository"]["id"] != latest["repository"]["id"] or current["run_attempt"] < latest["run_attempt"]:
        raise VerificationError("latest CI run identity or attempt changed inconsistently")
    if current.get("status") != "completed" or current.get("conclusion") != "success":
        raise VerificationError("latest exact-source main CI attempt has not completed successfully")
    return {
        "schema_version": "rkc-main-ci-proof/v1",
        "repository": repository,
        "repository_id": current["repository"]["id"],
        "source_commit": commit,
        "workflow_path": WORKFLOW,
        "workflow_id": current["workflow_id"],
        "run_id": current["id"],
        "run_number": current["run_number"],
        "run_attempt": current["run_attempt"],
        "event": "push",
        "head_branch": "main",
        "status": "completed",
        "conclusion": "success",
        "html_url": current["html_url"],
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--commit", required=True, help="checked-out HEAD^{commit}")
    parser.add_argument("--output", required=True, type=Path, help="new receipt outside the source tree")
    args = parser.parse_args(argv)
    try:
        proof = verify(args.repository, args.commit, os.environ.get("RKC_GITHUB_TOKEN", ""))
        # Refuse existing files and links. The workflow uses RUNNER_TEMP so the
        # receipt cannot change the clean source identity used by packaging.
        with args.output.open("x", encoding="utf-8") as output:
            output.write(json.dumps(proof, sort_keys=True, indent=2) + "\n")
    except VerificationError as error:
        print(f"main CI verification: {error}", file=sys.stderr)
        return 1
    except OSError:
        print("main CI verification: cannot write the new proof receipt", file=sys.stderr)
        return 1
    print(f"main CI verified: {proof['html_url']} (attempt {proof['run_attempt']})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
