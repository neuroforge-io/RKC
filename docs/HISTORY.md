# History: semantic deltas from Git

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors and Apache-2.0 licensed.

`rkc history build` compiles Git history into semantic deltas: when each
symbol appeared, how its signature changed, which commits touched it, where it
lived, and which refactors renamed it. The result is a `.rkc-history.json`
record that can be browsed, reported, or imported into a matching atlas
snapshot with `scan --history`. The current history schema is **1.1** and its
producer version is **rkc.history 1.2.0**.

```sh
rkc history build --dir . --out .rkc-history.json
rkc history report --history .rkc-history.json
rkc history symbol --name Greet --dir .
rkc scan --history .rkc-history.json --no-python --out .rkc --state-dir .rkc-state .
```

## What is compiled

- **Commits** (bounded by `--max-commits`, default 500) with date,
  subject, changed files, and the symbols each commit changed.
- **Symbol lifecycles**: first-seen and last-seen commits, every touching
  commit, every file the symbol lived in, and its signature history — each
  observed signature snapshot with its commit.
- **Refactors**: a deleted symbol and an added symbol of the same kind with
  the same normalized signature (function name stripped) form a conservative
  `supersedes` pair, catching renames that preserve interface shape.

## How it works

The compiler walks history oldest-first for lifecycle boundaries, materializes
each changed source file at its exact commit into a private temporary tree,
and runs the same deterministic syntax extractors used by scans (Go AST and
TypeScript syntax). Nothing is inferred from timestamps alone; every fact is
bound to the commit that produced it. Unsupported file types are skipped;
malformed supported source and NUL-containing source fail closed. Per-commit
file counts are bounded, and the output is deterministically sorted for
byte-identical recompilation. Multiline extractor signatures are normalized to
one control-safe semantic line before comparison.

Every retained repository label, Git date, commit subject, source path, symbol
name, qualified name, and signature has a byte bound and must be valid UTF-8
without terminal controls, bidirectional format controls, or line/paragraph
separators. Human-readable terminal output escapes untrusted control bytes as a
second line of defense. The compiler never serializes the repository's absolute
host path, Git author identity, or credentials.

Git discovery is bound to the exact requested working-tree directory. A plain
folder nested beneath some other checkout is not silently attributed to that
parent repository. Explicit external work trees remain supported when Git
reports that the requested directory itself is the work-tree root and both
`GIT_DIR` and `GIT_WORK_TREE` are present. Unpaired ambient Git affinity is
rejected.

## Immutable source affinity

Schema 1.1 binds every history record to:

- `repository_id`, derived from a canonical credential-free remote origin when
  one exists, or from the private-path-free repository basename otherwise;
- `source_revision`, the exact immutable Git object at compilation time;
- `revision_policy: exact_head` and `ancestry_policy: first_parent`; and
- `source_id`, a deterministic identity over the repository, revision, and
  both policies.

The observed commits must be an exact first-parent chain beginning at
`source_revision`. Import validates this contract before mutating any bundle.
The target must represent the same repository identity and source provenance,
the same exact Git HEAD, and a clean working tree. Foreign repositories,
different revisions, dirty snapshots, malformed chains, and origin mismatches
fail closed.

Ancestor-only import is deliberately not supported. A portable atlas bundle
does not carry enough Git object state to prove that a compiled head is an
ancestor of a later bundle head without consulting mutable external state.
Compile a new history at the target head instead. Local-path and `file://`
remotes are omitted because they are host-specific and can disclose private
paths; an originless history therefore matches only an originless bundle with
the same safe repository label and exact revision.

## Importing into an atlas

`scan --history .rkc-history.json` binds the history digest into the snapshot
identity, stamps matching nodes with lifecycle attributes
(`history_first_observed_commit`, `history_last_observed_commit`,
`history_touched_commits`, `history_files`) and immutable source-affinity
attributes. Imported evidence records identify `rkc.history` version 1.2.0 and
the compiled `source_id`. RKC adds `supersedes` edges only for conservative
rename candidates whose endpoints exist in the matching bundle. A symbol that
moved to another package appears as a new lifecycle with its own identity
rather than a forged rename.

The machine-readable contract is
[`schemas/history.schema.json`](../schemas/history.schema.json). The Go importer
also rejects unknown fields and enforces relational invariants that JSON Schema
cannot express, including source-ID derivation, exact-head equality, chain
continuity, symbol-ID derivation, and references into the observed window.

See [`docs/FLOW_AND_RUNTIME.md`](FLOW_AND_RUNTIME.md) for the related runtime
evidence and value-flow machinery, and
[`docs/IMPLEMENTATION_STATUS.md`](IMPLEMENTATION_STATUS.md) for the current
status of every feature.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
