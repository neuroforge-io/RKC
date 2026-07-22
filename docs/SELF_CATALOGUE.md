# Self-catalogue workflow

RKC can build a complete, deterministic atlas of its own committed source while
keeping the source and generated-output boundaries physically disjoint:

```sh
make self-catalogue
```

The target always enters [`scripts/with-rkc-limits.sh`](../scripts/with-rkc-limits.sh)
before it builds or scans. The resource guard refuses to start while an ERAIS
training or evaluation process is active and otherwise runs the complete
process tree at the lowest CPU and I/O priorities inside a bounded cgroup.
Calling [`scripts/self-catalogue.sh`](../scripts/self-catalogue.sh) directly is
rejected.

## Non-recursive source boundary

The workflow does not point RKC at its mutable working directory. It requires a
clean Git worktree, enumerates only stage-zero entries in the Git index, verifies
every copied byte against its Git blob, and constructs a private temporary
source tree. Tracked symlinks, submodules, special files, duplicate paths,
Git-LFS pointers, files over 64 MiB, and files that change while being read are
fatal.

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
├── SHA256SUMS.txt             deterministic canonical-file hashes
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
Git object, the built `rkc` binary hash, snapshot identity, deterministic bundle
digest, RKC canonical-files digest, and every canonical atlas file. The outer
checksum list covers that manifest, ownership markers, and all canonical atlas
files.

`atlas/rkc.execution.json` is deliberately operational: it contains the run
timestamp. `atlas/rkc-export-manifest.json` includes that operational file's
hash. Both are validated by `rkc check` but explicitly excluded from the outer
deterministic hash set; the exclusion is recorded in `MANIFEST.json`. All other
atlas paths must match RKC's exact export and ownership manifests.

Verify a completed catalogue without rescanning:

```sh
cd dist/self-catalogue
sha256sum --check --strict SHA256SUMS.txt
../../bin/rkc check \
  --coverage atlas/coverage.json \
  --bundle atlas/bundle.json \
  --export-manifest atlas/rkc-export-manifest.json
```

An existing `dist/self-catalogue` directory is reusable only when its private
outer ownership marker matches exactly and it contains no unexpected entries or
links. RKC's own ownership manifests govern atomic replacement and recovery of
the `atlas` subdirectory. The wrapper never recursively deletes or adopts an
unmarked directory.
