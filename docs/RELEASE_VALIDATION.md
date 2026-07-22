# Release validation

The complete package is built only after `scripts/verify-release.sh` succeeds.
Logs are retained under `dist/validation/logs`, and a machine-readable summary is
written to `dist/validation/summary.json`.

## Verification sequence

| Step | Command | Purpose |
|---|---|---|
| format | `make format-check` | canonical Go formatting |
| vet | `make vet` | static Go diagnostics |
| coverage gate | `make coverage` | all Go tests plus Python line/branch tests, inventory, and policy floors |
| contracts | `make contracts` | schemas, examples, OpenAPI parity, WIT, SQLite |
| docs | `make docs-check` | local links and code fences |
| licenses | `python3 scripts/validate-licenses.py` | Apache and third-party notice boundaries |
| build | `make build` | `rkc` and `rkc-mcp` |
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
- verifies the WIT package revision;
- checks the plugin lockfile shape.

## License validation

The dependency-free license validator fails closed when required Apache or
third-party notices are missing, altered into an unrecognized form, or replaced
by links. It checks the implemented OpenAPI and official plugin license metadata,
keeps the current no-third-party-Go-module assertion honest, requires every file
under `LICENSES/` to be referenced from `THIRD_PARTY_NOTICES.md`, and rejects
tracked symlinks, submodules, model weights, and native artifacts. A newly added
dependency must therefore expand the reviewed notice inventory before release.

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

The deterministic package builder:

1. enumerates source from stage-zero files in the Git index rather than copying
   arbitrary tracked or untracked worktree neighbours;
2. rejects source symlinks, submodules, model weights, native artifacts, and
   generated-output paths;
3. includes Linux amd64 and arm64 `rkc` and `rkc-mcp` binaries;
4. includes a fresh mixed-language demonstration atlas;
5. includes all release validation logs and summary;
6. places `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, and `LICENSES/` both at
   the archive root and inside the tracked source tree;
7. writes `README-FIRST.md` with the RKC/third-party license boundary;
8. writes a JSON manifest containing every payload file, size, and SHA-256;
9. writes `SHA256SUMS.txt`;
10. sorts archive entries and fixes ZIP timestamps and permissions;
11. accepts output only below `dist/`, uses an exclusive temporary file, and
    publishes without replacement unless `--force` was explicitly supplied.

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

Passing this suite proves the reference paths packaged here. It does not prove
future compiler adapters, production sandboxing, PostgreSQL team mode, or a
real-GGUF memory target that are not yet implemented. Those have separate exit
gates in the remainder plan.
