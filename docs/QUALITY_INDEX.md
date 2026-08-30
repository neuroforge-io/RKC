# Quality and change index

RKC includes a small, dependency-free quality index for maintainers and
release reviewers. It inventories every admitted regular file, records
byte/line counts and SHA-256 digests, associates analyzable production files
with nearby tests, records documentation evidence, reads optional Go and
branch-aware Python profiles, and reports Git deltas. The JSON output is the
machine contract; the Markdown file is the review surface. When admitted Go
source exists, the index also uses the local Go parser to report exact
exported-declaration documentation coverage independently of file-level
documentation evidence.

The inventory and the evidence denominator are deliberately different. Every
regular file is represented exactly once as analyzable source, documentation,
or `other_files`. The latter includes build metadata, schemas, configuration,
locks, license material, and formats for which no analyzer is configured; each
record is explicit `not-applicable`, not silently omitted and not falsely
treated as executable code. Summary fields report the full partition and a
separate inventory-accounting percentage. `production_source_files` alone is
the denominator for file-level test and documentation evidence; the legacy
`source_files` field remains an alias for that value in schema 1.1.

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
Folders containing Go source require an installed local Go toolchain. RKC
compiles a standard-library-only parser helper in a private temporary
directory with module downloads disabled; the helper parses admitted files as
syntax and never imports, builds, or executes the indexed repository. A
missing toolchain, parse failure, timeout, or malformed helper result fails the
index instead of silently overstating documentation coverage.

Each regular file is opened without following symlinks and must keep one
device/inode/size/mtime identity throughout hashing. Before returning a report,
the indexer rechecks the admitted path set and file identities. In a Git
worktree it also rechecks the exact commit/tree, staged index digest, tracked
paths, and working-tree status digest. Concurrent change fails the run instead
of mixing provenance. A non-Git folder remains a stable per-file observation;
no repository-wide atomic snapshot primitive is claimed for it.

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
- `go_exported_documentation`: a separate Go-AST measurement for exported
  production declarations. It counts exported functions, methods, types,
  constants, and variables with a non-empty leading comment attached by
  `go/parser`. The record gives exact declaration/documented counts and lists
  every missing symbol with declaration kind, line, and column. Generated and
  test sources are explicitly `not-applicable`; a production file with no
  exported declarations is `no-exported-declarations`. This measures comment
  attachment, not prose quality, examples, or API correctness.
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

The `documentation` and `other_files` arrays complete the regular-file
inventory. Every record carries `git_status`; `git_inventory` separately lists
the exact tracked universe, its admitted intersection, and tracked paths outside
the configured walk. In a non-Git folder those fields are `unavailable` without
preventing the filesystem inventory. A 100% inventory-accounting result means
only that every admitted regular file belongs to exactly one category—it is not
a test, documentation, profiling, or semantic-coverage claim.

When a coverage-gate report is supplied, the summary separately counts
`profiling_scope_excluded_files` and `profiling_zero_executable_files`; those
files remain in the source inventory but are removed from the percentage
denominator for a principled, auditable reason.
The summary also counts `profile_errors`; the Markdown report lists each one,
and any supplied profile error makes the command fail closed.

The `gaps` array is the actionable queue. Changed files are marked `high`
priority when they lack a test, documentation evidence, or an applicable
profile. Every undocumented exported Go declaration is a distinct
`go-exported-documentation` gap carrying its symbol, declaration kind, and
source coordinate; this queue is intentionally separate from the heuristic
file-level `documentation` status. A profiled file with uncovered executable
units is also listed with the exact uncovered/total count, so a passing file
association cannot hide unfinished behavior coverage. The `deltas` object
records the Git comparison scope and status. A non-Git folder remains fully
indexable; its delta status is simply `unavailable`.

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
record. For a committed comparison, pass a stable ref with `--base`; the report
records both that base and the exact ending commit rather than the mutable word
`HEAD`. Without a base the report compares the current worktree with `HEAD` and
includes standard untracked files. Profiles should be generated by the normal
RKC coverage workflow so their paths and branch semantics remain auditable.

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
License](../LICENSE). The license requires copies or substantial portions to
retain its copyright and permission notice. Retaining [`NOTICE`](../NOTICE) and
crediting NeuroForgeIO/RKC contributors are requested, but are not additional
license conditions. Third-party dependencies, model weights, and compiler
indexes keep their own licenses as documented in
[`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
