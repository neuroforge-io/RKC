# Quality and change index

RKC includes a small, dependency-free quality index for maintainers and
release reviewers. It inventories admitted source and documentation files,
records byte/line counts and SHA-256 digests, associates production files with
nearby tests, records documentation evidence, reads optional Go and
branch-aware Python profiles, and reports Git deltas. The JSON output is the
machine contract; the Markdown file is the review surface.

## Run it

From any checkout or arbitrary local folder:

```sh
make quality-index
```

The default output is `.rkc-quality/index.json` and `.rkc-quality/index.md`.
The generated directory is ignored by Git and excluded automatically if the
indexed folder contains it. For a folder without a Makefile, run the portable
script directly:

On a shared development host, use `make safe-quality-index` so the scan runs
inside RKC's fail-closed one-core, low-priority resource envelope and yields to
higher-priority ERAIS work.

```sh
python3 scripts/quality_index.py \
  --root /path/to/folder \
  --output /path/to/folder/.rkc-quality \
  --base origin/main \
  --go-profile /path/to/merged-go-cover.out \
  --go-report /path/to/coverage-summary.json \
  --python-report /path/to/coverage.json
```

Add `--fail-on-gaps` in a release or repository policy job when the explicit
evidence queue must be empty. The default remains non-blocking so that an
arbitrary folder can be inventoried before its tests and documentation have
been brought up to the selected policy.

All arguments are passed as an array to the local process. The indexer never
executes repository code, follows symlinks, or downloads a model. It skips
standard dependency, build, cache, RKC-output, and virtual-environment trees,
and records skipped symlinks/non-regular files explicitly. JSON and Markdown
files are replaced through same-directory temporary files and `fsync`.

## Reading the report

Each `files` record contains:

- `tests`: a conservative filename/directory association. `associated` means
  a candidate test file was found; an explicit source path/basename in a test
  harness is also accepted, including cross-language harnesses such as Python
  tests that exercise shell workflows. `gap` means no candidate was found;
  `test-file` identifies a test source that is excluded from production
  percentages.
- `documentation`: a Go package comment (in any non-test Go file in the
  package), a Python module docstring, or an exact path/basename mention in
  admitted Markdown/reStructuredText is recorded as `evidence`. This is a
  review signal, not a semantic documentation proof.
- `profile`: Go statement blocks and Python statements plus branches are
  reported when the corresponding profile is supplied. `missing` means a
  supplied profile omitted the file; `not-provided` means no profile was
  supplied; `platform-excluded` and `zero-executable` are explicit statuses
  imported from the coverage-gate JSON report rather than guessed from a
  missing row; other languages are `not-applicable` until a compatible
  profiler is integrated. Pass `--go-report` alongside `--go-profile` when
  using `scripts/coverage_gate.py` so build-tag exclusions and type-only files
  do not become false profiling gaps.
- `profile.uncovered`: every uncovered Go block retains its exact start/end
  line and column plus statement count. Python reports retain every missing
  executable line and branch arc. The Markdown report shows a bounded preview;
  `index.json` retains the complete deterministic location inventory so a
  human or agent can go directly from a gap to the behavior that needs a test.

When a coverage-gate report is supplied, the summary separately counts
`profiling_scope_excluded_files` and `profiling_zero_executable_files`; those
files remain in the source inventory but are removed from the percentage
denominator for a principled, auditable reason.
The summary also counts `profile_errors`; the Markdown report lists each one,
and any supplied profile error makes the command fail closed.

The `gaps` array is the actionable queue. Changed files are marked `high`
priority when they lack a test, documentation evidence, or an applicable
profile. A profiled file with uncovered executable units is also listed with
the exact uncovered/total count, so a passing file association cannot hide
unfinished behavior coverage. The `deltas` object records the Git comparison
scope and status. A non-Git folder remains fully indexable; its delta status is
simply `unavailable`.

Malformed or unowned profile rows are retained in `profile_errors`. Go profiles
must declare a supported mode, use valid coordinates, and keep one statement
denominator for each block; repeated blocks are deduplicated with covered-OR
semantics. Python reports must prove branch coverage, use non-negative exact
integer counters, retain complete missing-line/branch details, and cannot map
two entries to one source file. The command returns a failure when supplied
profile evidence cannot be trusted, including in non-strict mode; this prevents
a duplicated or internally inconsistent report from being mistaken for a
complete one.

The report deliberately does not claim 100% coverage. RKC's release gate
continues to enforce measured Go and Python line/branch thresholds; this index
makes the remaining file-level evidence and profiling gaps visible so they can
be closed deliberately. A 100% percentage in one column means every admitted
file satisfied that column's configured heuristic, not that all behavior,
sentences, branches, or generated outputs are semantically complete.

## Keeping it current

Run the index after edits and attach `index.json` to a review or release
record. For a committed comparison, pass a stable ref with `--base`; without a
base the report compares the current worktree with `HEAD` and includes
standard untracked files. Profiles should be generated by the normal RKC
coverage workflow so their paths and branch semantics remain auditable.

The main CI workflow runs that coverage gate in the same low-priority
envelope before building its quality artifact, then supplies the fresh merged
Go profile (including the maintained nested Go example module) and branch-aware
Python report to this index. CI always fetches complete history and passes an
event-bound comparison: a pull request uses its recorded base commit and a
push uses the recorded pre-push commit. A first/root or branch-creation push
has an all-zero pre-push SHA, so CI creates an empty Git tree and reports the
complete committed tree as its delta. A missing non-zero base is an explicit
CI error; it never degrades to a misleading clean-worktree comparison. Local
`make quality-index` runs remain intentionally fast and omit profiles unless
you pass them explicitly.

The report is branded and licensed with the project: RKC-owned code and docs
are released by **NeuroForgeIO** and RKC contributors under the [MIT
License](../LICENSE). Retain [`NOTICE`](../NOTICE) and credit NeuroForgeIO/RKC
contributors in redistributed products; third-party dependencies, model
weights, and compiler indexes keep their own licenses as documented in
[`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
