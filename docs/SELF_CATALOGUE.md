# Self-catalogue workflow

RKC can build a complete, deterministic atlas of its own committed source while
keeping the source and generated-output boundaries physically disjoint:

```sh
make self-catalogue
```

The target always enters [`scripts/with-rkc-limits.sh`](../scripts/with-rkc-limits.sh)
before it builds or scans. The resource guard keeps the complete process tree
at the lowest CPU and I/O priorities inside a bounded cgroup and, under the
default `yield` policy, proceeds while a process matching a configured
higher-priority marker merely runs (the strict
`RKC_HIGHER_PRIORITY_POLICY=refuse` setting instead refuses while one is
visible). Calling [`scripts/self-catalogue.sh`](../scripts/self-catalogue.sh) directly is
rejected.

Compiler-grade self-analysis is opt-in and digest-bound: supply a host-built
`scip-go` binary through `SCIP_GO_BINARY`, and the workflow pins it to its
exact SHA-256, generates a strictly validated Go index, and imports it through
the normal `--scip-index` path:

```sh
SCIP_GO_BINARY="$HOME/go/bin/scip-go" make self-catalogue
```

Without the variable the self-run remains the deterministic no-SCIP profile
(31.5% relationship resolution); with it, the measured compiler-grade profile
resolves 84.89% of relationships (see
[`SHOWCASE_2026-07-27.md`](SHOWCASE_2026-07-27.md)).

## Non-recursive source boundary

The workflow does not point RKC at its mutable working directory or index. It
records `HEAD` and its tree while the worktree is clean, enumerates that exact
commit tree, reads each admitted blob from Git's object database, verifies its
object hash, and constructs a private detached source tree. The RKC executable
is built from that detached tree, not from the checkout. Because detached-tree
builds intentionally have no `.git` directory, the receipt reports absent Go
VCS metadata honestly and binds the build through the recorded commit/tree,
source-object manifest, build metadata, and executable hash instead.

Tracked symlinks, submodules, special files, duplicate paths, Git-LFS pointers,
and blobs over 64 MiB are fatal. The staged source is re-audited after both the
build and scan. Only `bin/rkc` and `bin/rkc-mcp` may appear beyond the recorded
blobs, and `bin` remains excluded from ingestion. Immediately before
publication, the workflow rechecks clean Git status and the exact `HEAD` and
tree identities; concurrent source changes therefore preserve the prior
catalogue.

All helper code runs with isolated standard-library-only system Python
(`/usr/bin/python3` or `/usr/local/bin/python3`). An activated or ignored
repository `.venv` is never selected.

The temporary source tree and `dist/self-catalogue` output are separate trees.
Consequently, RKC cannot discover output produced during the scan. The following
exact exclusions are also passed to RKC and recorded in the source-selection
manifest:

```text
.cache
.coverage
.git
.mypy_cache
.pytest_cache
.rkc
.rkc-coverage
.rkc-downloads
.rkc-models
.rkc-runtime
.rkc-state
.rkc.rkc-derived
.ruff_cache
.venv
__pycache__
bin
coverage
coverage.out
coverage.xml
dist
htmlcov
models/downloads
models/runtime
venv
```

Staging additionally excludes any path component beginning with `.rkc`, any
virtual-environment, Python-cache, or coverage component, and all tracked model
weight/tensor formats. GGUF/GGML magic, common weight suffixes, and Git-LFS
pointers are rejected regardless of their location. Model lockfiles and
qualification specifications remain ordinary source evidence; downloaded
runtimes and weights never enter the catalogue.

## Full deterministic products

The scan retains RKC's safe defaults. It does not disable graph export, lexical
search, generated documentation, normalized source, static-site, integration,
framework, language, or secret-analysis stages. It also passes
`--fail-on-errors`, then runs `rkc check` against the bundle, coverage report,
and export manifest with exact inventory, evidence, citation, diagnostic,
secret-redaction, and file-digest gates.

No model is invoked. Documentation is derived deterministically from cited
repository evidence, and the persisted search index is lexical. Optional model
weights and LLM-generated answers are neither needed nor emitted by this
workflow.

## Output and receipts

Successful output is published below `dist/self-catalogue`:

```text
dist/self-catalogue/
├── .rkc-self-catalogue.json   outer ownership marker
├── MANIFEST.json              source, tool, atlas, and safety provenance
├── SHA256SUMS.txt             complete per-run file hashes
└── atlas/
    ├── bundle.json
    ├── coverage.json
    ├── docs/
    ├── graph/
    ├── integrations/
    ├── normalized/
    ├── search/
    └── site/
```

`MANIFEST.json` binds the Git commit and tree, every admitted source path and
Git object, the ephemeral detached-tree `rkc` binary hash and normalized Go
build metadata, snapshot identity, deterministic bundle digest, RKC
canonical-files digest, every canonical atlas file, and the two operational
receipts. The same commit is retained inside the standalone atlas manifest and
derived browser/NotebookLM products. The executable itself is not retained.
The outer checksum list covers every regular published file except the checksum
list itself.

`atlas/rkc.execution.json` is deliberately operational: it contains the run
timestamp. `atlas/rkc-export-manifest.json` includes that operational file's
hash. Both remain excluded from RKC's reproducible canonical-files digest, and
that classification is recorded in `MANIFEST.json`; both are nevertheless
hashed by the complete outer `SHA256SUMS.txt`. All other atlas paths must match
RKC's exact export and ownership manifests.

Verify a completed catalogue without rescanning:

```sh
cd dist/self-catalogue
sha256sum --check --strict SHA256SUMS.txt
../../bin/rkc check \
  --coverage atlas/coverage.json \
  --bundle atlas/bundle.json \
  --export-manifest atlas/rkc-export-manifest.json
```

The complete candidate directory, including the atlas and both outer receipts,
is constructed and durably validated in a private sibling below `dist`. Only
then is the whole directory atomically renamed into place. When a prior
`dist/self-catalogue` exists, Linux `renameat2(RENAME_EXCHANGE)` swaps the two
complete directories in one operation; the prior verified catalogue is retained
under a unique `.rkc-self-catalogue-quarantine-*` sibling. Failed publication is
rolled back, while failed pre-publication staging is marker-checked and moved to
an explicit failed quarantine. The wrapper never mutates the last-known-good
catalogue before success, recursively deletes a catalogue, or adopts an
unmarked directory.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
