# Release validation

The complete package is built only after `scripts/verify-release.sh` succeeds.
That verifier first rejects any tracked or non-ignored untracked source changes,
selects a Python 3.11+ interpreter with every required distribution at the
version pinned in `requirements-dev.txt`, materializes
a clean detached copy of exact `HEAD`, runs the full sequence only inside the
immutable checkout, confirms that its tracked and
untracked source state stayed clean, and preserves the prior evidence until the
complete validation and benchmark inventory can replace `dist/evidence` with one
atomic directory exchange. Logs and the machine-readable summary are under
`dist/evidence/validation`; raw benchmark outputs are under
`dist/evidence/benchmark`.

This package-version check catches validator dependency drift. It does not
claim that a mutable local virtual environment is hermetic, reject unrelated
installed distributions, or attest interpreter/package bytes. Release source
and publication helpers are separately executed from immutable Git checkouts;
cryptographic build provenance remains a planned release gate.

## Verification sequence

| Step | Command | Purpose |
|---|---|---|
| Go modules | `make go-mod-verify` | download checksum-locked modules and verify cached source |
| Python requirements | `make python-env-check` | require Python 3.11+ and each validation distribution at its pinned `requirements-dev.txt` version |
| format | `make format-check` | canonical Go formatting |
| vet | `make vet` | static Go diagnostics |
| coverage gate | `make coverage` | all Go tests plus Python line/branch tests, inventory, and policy floors |
| contracts | `make contracts` | schemas, examples, OpenAPI parity, WIT, SQLite |
| docs | `make docs-check` | local links and code fences |
| licenses | `make licenses` | Apache and third-party notice boundaries using the version-checked validation interpreter |
| model lock | `make model-lock-check` | optional runtime/model identities, hashes, licenses, and null-default policy |
| build | `make build` | CGO-disabled `rkc` and `rkc-mcp` |
| plugins | `make plugins` | manifest and lock digest verification |
| smoke | `make smoke` | mixed-language scan, gate, search, packet-only synthesis |
| reproducibility | `make reproducibility` | byte-identical bundle and coverage |
| HTTP | `make smoke-api` | health and ranked search over a live server |
| MCP | `make smoke-mcp` | initialize, tools/list, and search tool call |
| Git | `make smoke-git` | promptless `file://` acquisition in controlled mode |
| race | `make test-race` | Go race detector |
| benchmark | `make benchmark` | self-scan timing and coverage report |

## Test coverage policy

`scripts/coverage_gate.py` discovers every current-platform package in the main
Go module, instruments the exact package inventory, and runs the package tests
with `-p=1`. Because `-coverpkg` can emit the same source block once for each
test binary, the gate merges blocks by canonical file and source coordinates.
A statement is covered when at least one test binary covers its block, and its
denominator is counted once. Generated Go files are excluded by the standard
`Code generated ... DO NOT EDIT.` marker. Files excluded by current build tags
are listed in the JSON evidence and are not represented as tested on that
platform.

The enforced Go floors are 90% overall statement coverage and 80% for every
package with executable statements. Type-only packages remain in the inventory
and are reported explicitly rather than assigned a misleading denominator.

Python coverage includes every non-test, non-generated `.py` source under
`internal/`, `plugins/`, and `scripts/`. It combines lines and branches into the
denominator and enforces 90% overall plus 80% per executable file. Coverage.py's
subprocess patch writes parallel data for Python children. The gate also tracks
each test, validator, and child command's exit status independently, so a failed
subprocess cannot be hidden by a passing percentage.

Each run retains raw Go data, the deduplicated Go profile, Coverage.py JSON, and
`summary.json` in a unique directory below `.rkc-coverage/`. Local shared-host
validation should use `make safe-coverage`; the fail-closed resource guard gives
priority to ERAIS and bounds the complete test process tree.

## Contract validation

The offline validator:

- checks every JSON Schema with Draft 2020-12;
- validates the example configuration;
- validates every plugin manifest;
- validates a minimal bundle and GraphPatch;
- validates the smoke bundle when present;
- parses implemented and future OpenAPI documents;
- compares implemented OpenAPI paths with Go handler registration;
- executes the SQLite DDL in an in-memory database;
- verifies the exact migration-manifest digest, every ordered migration digest,
  forward-only schema versions, canonical UTF-8/LF encoding, clean migration
  execution, foreign-key and integrity checks, and catalogue equivalence with
  the consolidated SQLite DDL;
- probes canonical snapshot/build lineage, stale-build commit and publication
  rejection, and monotonic current-pointer enforcement;
- verifies the WIT package revision;
- checks the plugin lockfile shape.

## License validation

The dependency-aware license validator fails closed when required Apache or
third-party notices are missing, altered into an unrecognized form, or replaced
by links. It checks the implemented OpenAPI and official plugin metadata,
requires `go.mod`, `go.sum`, `third_party/go-modules.lock.json`, and every
reviewed file under `LICENSES/` to agree with the resolved Go module graph,
requires every license file to be referenced from `THIRD_PARTY_NOTICES.md`, and
rejects tracked links, submodules, model weights, and native artifacts. A newly
added dependency must expand the reviewed lock and notice inventory before
release.

## Determinism

Two clean scans of the same examples are compared byte for byte:

```text
bundle.json
coverage.json
deterministic_output_digest
```

Operational timestamps and absolute local paths are excluded from canonical
hashing. Derived output may include generation timestamps where appropriate but
does not alter source truth.

## Package construction

The byte-reproducible package builder:

1. resolves one exact `HEAD` commit, tree, and commit timestamp, enumerates that
   commit with `git ls-tree`, and materializes each source file from its verified
   Git blob rather than copying worktree neighbours;
2. rejects source symlinks, submodules, model weights, native artifacts, and
   generated-output paths;
3. includes Linux amd64 and arm64 `rkc` and `rkc-mcp` binaries plus a
   deterministic SPDX 2.3 Go-module SBOM for each executable, then independently
   recomputes and compares the exact binary checksum, command identity,
   GOOS/GOARCH, normalized GOAMD64/GOARM64 target, default GOEXPERIMENT set,
   `GOFIPS140=off`, Go toolchain, immutable source commit/tree/time, module-lock
   digest, canonical Go purls, and linked dependency inventory before copying;
   declared values come from the audited lock while every unanalyzed package's
   `licenseConcluded` remains the SPDX-required `NOASSERTION`;
4. includes the byte-stable `bundle.json` and `coverage.json` from a fresh
   mixed-language demonstration generated in a private immutable checkout;
5. requires the exact successful release step/log inventory, summary schema,
   source binding, every log digest, and the exact three-file raw benchmark
   inventory, then includes a receipt containing every validation/benchmark
   evidence path, size, SHA-256, and an aggregate manifest digest while leaving
   those volatile files outside the ZIP;
6. places `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, every audited
   `LICENSES/` file, and `third_party/go-modules.lock.json` at the archive root
   as well as in the tracked source tree, and requires every binary bundle to
   carry the exact audited license inventory and module lock;
7. writes `README-FIRST.md` with the RKC/third-party license boundary;
8. writes `SBOM.spdx.json`, a complete-distribution SPDX 2.3 inventory of
   substantive files, platform command components, and their linked Go modules;
   it explicitly excludes its own circular hash plus the later manifest and
   checksum receipts;
9. writes `MANIFEST.json`, including the distribution SBOM's size and SHA-256,
   then writes `SHA256SUMS.txt` over the SBOM, manifest, and every other file
   except the checksum file itself;
10. sorts archive entries, fixes ZIP timestamps and permissions, and uses stored
    entries so archive bytes do not depend on a host zlib implementation;
11. accepts lane output only below `dist/` and uses an exclusive temporary file;
12. independently rebuilds binaries, SBOMs, and demo artifacts in two clean
    detached checkouts with separate `GOCACHE` and `GOMODCACHE` directories,
    assembles a complete ZIP in each, and requires byte-for-byte equality before
    publishing the ZIP, binaries, demo, and its exact raw-evidence snapshot with
    one atomic `dist/release` generation exchange; staged directories are
    synced bottom-up, both rename parents are synced, and a sync failure rolls
    the namespace back before returning an error.

CI runs this complete release-verification, cross-platform binary/SBOM, and ZIP
assembly path inside the delegated one-core, 2.5-GiB low-priority resource
guard. CI uploads the one coherent `dist/release` generation, including the ZIP,
all SPDX documents, and the exact raw validation/benchmark evidence that is
intentionally excluded from the reproducible ZIP; mocked unit coverage is not
the release integration gate.

## Fresh extraction gate

After packaging, the delivered archive should be extracted into an empty
directory and tested using the source contained in the archive:

```sh
cd repository-knowledge-compiler-complete/source
make test
make build
./bin/rkc scan --out /tmp/rkc-package-test --force examples
./bin/rkc check --coverage /tmp/rkc-package-test/coverage.json \
  --min-inventory-accounting 1 --min-symbol-evidence 1 --max-errors 0
```

The outer package checksums and the internal `SHA256SUMS.txt` must also verify.

## Honest scope

Passing this suite proves the reference paths packaged here, including the
durable local SQLite backend. It does not prove future compiler adapters,
general third-party native-plugin sandboxing, PostgreSQL team mode, signed
publication, container SBOMs, provenance, or a qualified real-GGUF
memory/quality target. Those have separate exit gates in the remainder plan.
