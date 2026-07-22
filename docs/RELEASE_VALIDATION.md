# Release validation

The complete package is built only after `scripts/verify-release.sh` succeeds.
Logs are retained under `dist/validation/logs`, and a machine-readable summary is
written to `dist/validation/summary.json`.

## Verification sequence

| Step | Command | Purpose |
|---|---|---|
| format | `make format-check` | canonical Go formatting |
| vet | `make vet` | static Go diagnostics |
| Go tests | `make test` | unit and integration tests |
| Python tests | `make python-test` | AST adapter fixtures |
| contracts | `make contracts` | schemas, examples, OpenAPI parity, WIT, SQLite |
| docs | `make docs-check` | local links and code fences |
| build | `make build` | `rkc` and `rkc-mcp` |
| plugins | `make plugins` | manifest and lock digest verification |
| smoke | `make smoke` | mixed-language scan, gate, search, packet-only synthesis |
| reproducibility | `make reproducibility` | byte-identical bundle and coverage |
| HTTP | `make smoke-api` | health and ranked search over a live server |
| MCP | `make smoke-mcp` | initialize, tools/list, and search tool call |
| Git | `make smoke-git` | promptless `file://` acquisition in controlled mode |
| race | `make test-race` | Go race detector |
| benchmark | `make benchmark` | self-scan timing and coverage report |

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

1. excludes transient build, Git, cache, and generated output directories from
   the source copy;
2. includes the complete source tree;
3. includes Linux amd64 and arm64 `rkc` and `rkc-mcp` binaries;
4. includes a fresh mixed-language demonstration atlas;
5. includes all release validation logs and summary;
6. writes `README-FIRST.md`;
7. writes a JSON manifest containing every payload file, size, and SHA-256;
8. writes `SHA256SUMS.txt`;
9. sorts archive entries and fixes ZIP timestamps and permissions;
10. writes the final archive atomically.

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
