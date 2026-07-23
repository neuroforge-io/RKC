# Quickstart

## 1. Install prerequisites

For a source checkout on Linux or macOS:

```sh
go version       # use a currently supported release; CI pins 1.26.5
python3 --version # 3.11 or newer
git --version
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements-dev.txt
```

The virtual environment keeps the pinned validation dependencies out of the
system interpreter. Prebuilt binaries need none of these tools for a local
`--no-python` scan. Native Windows users can build the two Go commands and use
that portable profile, while the guarded development/release automation and
the optional Python adapter require Linux; WSL2 is the supported Windows route
for those Linux-only paths.

## 2. Verify the checkout

```sh
make safe-verify
```

This runs formatting, vetting, Go and Python tests, contract validation,
document-link validation, plugin-lock verification, a mixed-language scan,
deterministic replay, HTTP API smoke tests, MCP smoke tests, and remote-Git
acquisition tests.

`safe-*` targets are intentionally Linux-specific: they require a reachable
user-systemd manager and delegated CPU, memory, I/O, and process controllers.
Run `make build` for an unguarded portable build. Run `./bin/rkc doctor` after
building to see whether the Python adapter and remote-Git conveniences are
available on the current host.

Run the race detector separately or use the logged release sequence:

```sh
make safe-test-race
make safe-release-verify
```

## 3. Build

```sh
make build
./bin/rkc version
./bin/rkc doctor --repository .
```

On a supported Linux user-systemd host, `make safe-build` provides the same
binary build under RKC's deliberately subordinate resource envelope.

## 4. Generate configuration

```sh
./bin/rkc init --path rkc.json
```

Edit `rkc.json`, then pass it with `--config rkc.json`. Omit the option to use
safe local defaults. The older `--out` spelling remains a compatibility alias;
`--path` is the canonical flag.

`inventory.exclude` values are exact repository-relative paths, not globs. Each
value excludes that path and its descendants. RKC does not claim to interpret
`.gitignore`; its generated configuration instead lists explicit safe defaults
for virtual environments, local RKC model/runtime outputs, `bin`, `dist`, and
named root-level coverage/cache outputs. Add another exact path with a repeated
`--exclude` flag when scanning.

## 5. Scan a repository

Start with the portable deterministic profile:

```sh
./bin/rkc plan \
  --config rkc.json \
  --no-python \
  /path/to/repository

./bin/rkc scan \
  --config rkc.json \
  --no-python \
  --out /tmp/my-atlas \
  --state-dir /tmp/my-atlas-state \
  --force \
  /path/to/repository
```

`rkc plan` performs inventory and normalization only, then reports the complete
15-stage DAG, verified cache hits, misses, disabled stages, and invalidation
reasons. Analyzer payloads are stored outside the repository in the operating
system's user-cache directory. Use `scan --no-cache` when an explicitly clean
run is required; clean and incremental execution produce the same snapshot
identity and canonical digest. The scheduler admits concurrent stages within
the `--stage-workers` and `--stage-memory-mib` bounds; the safe defaults are
four workers and a 2048 MiB aggregate admission budget.

This still performs deterministic Go and JavaScript/TypeScript syntax
analysis, framework and document extraction, secret-pattern detection, graph
construction, search indexing, and every configured export. If
`./bin/rkc doctor --strict --config rkc.json --repository /path/to/repository`
passes on Linux, omit `--no-python` to enable the built-in Python AST adapter.
RKC never falls back to running that adapter without its isolation boundary.

Inspect or maintain the cache without scanning:

```sh
./bin/rkc cache inspect --verify
./bin/rkc cache verify
./bin/rkc cache prune --older-than 720h --dry-run
```

`cache prune --all` requires `--yes`; every prune mode supports `--dry-run` and
machine-readable `--json` output.

`--state-dir` must be missing, empty, or already marked as an RKC snapshot
store. RKC refuses to adopt arbitrary nonempty directories as transaction
state.

For the durable SQLite runtime, create an owner-only database directory and use
`--database` instead of `--state-dir`:

```sh
install -d -m 700 /tmp/rkc-store
./bin/rkc scan \
  --config rkc.json \
  --no-python \
  --database /tmp/rkc-store/rkc.sqlite \
  --out /tmp/my-atlas \
  --force \
  /path/to/repository

./bin/rkc snapshots list --database /tmp/rkc-store/rkc.sqlite --limit 20
./bin/rkc query --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>' authentication
./bin/rkc serve --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>'
./bin/rkc-mcp --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>'
```

Use the snapshot ID printed by `scan`, or select one repository's current
snapshot with `--repository`. Database readers open the existing file in
read-only mode and reject missing files, mixed selectors, and paths with unsafe
ownership or permissions.

Remote Git repositories are materialized without prompts or hooks:

```sh
./bin/rkc scan \
  --no-python \
  --ref main \
  --clone-depth 1 \
  --out /tmp/remote-atlas \
  --force \
  https://example.invalid/organisation/repository.git
```

Credentials should be supplied through an approved Git credential helper, not
embedded in URLs or configuration files.

## 6. Enforce quality

```sh
./bin/rkc check \
  --coverage /tmp/my-atlas/coverage.json \
  --bundle /tmp/my-atlas/bundle.json \
  --min-inventory-accounting 1 \
  --min-symbol-evidence 1 \
  --min-edge-resolution 0.5 \
  --min-claim-citation 1 \
  --max-errors 0 \
  --max-high-confidence-secrets 0
```

Edge resolution depends on analyzer precision. The reference syntax adapters
intentionally retain unresolved relations; lower the threshold for dynamic or
unsupported codebases rather than falsifying the denominator.

## 7. Search and browse

```sh
./bin/rkc query --dir /tmp/my-atlas --limit 20 authentication
./bin/rkc serve --dir /tmp/my-atlas --addr 127.0.0.1:8787
```

The static site is also available directly under `/tmp/my-atlas/site`.

## 8. Use MCP

```sh
./bin/rkc-mcp --dir /tmp/my-atlas
```

Example initialization request:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
```

## 9. Construct model evidence packets

Packet-only mode is useful even without a model:

```sh
./bin/rkc synthesize \
  --dir /tmp/my-atlas \
  --repo-root /path/to/repository \
  --query authentication \
  --task module_summary \
  --packet-only \
  --limit 5 \
  --force
```

The default destination is the deterministic sibling
`/tmp/my-atlas.rkc-derived/synthesis/<profile>`, never a directory inside the
verified atlas. An explicit `--out` must also resolve outside the atlas.

With `llama.cpp`:

```sh
make model-lock-check
make model-runtime-native
make model-fetch-generation
```

These explicit commands use the checked-in byte/digest/license lock and the
low-priority resource guard. They do not make the locked generation candidate a
default; a real guarded qualification and manual receipt review are still
required. See [`MODEL_RUNTIME.md`](MODEL_RUNTIME.md) for the portable build,
embedding candidate, and qualification commands.

```sh
./bin/rkc synthesize \
  --dir /tmp/my-atlas \
  --repo-root /path/to/repository \
  --query authentication \
  --model /models/coder-q4.gguf \
  --llama-cli /usr/local/bin/llama-cli \
  --context 4096 \
  --max-output 768 \
  --max-rss-mib 2560 \
  --limit 5 \
  --force
```

RKC rejects claims that cite unavailable evidence, reference unknown code
identifiers, omit certainty, or violate packet policy.

On Linux, model execution additionally fails closed unless it can enter a
low-priority user cgroup. It is CPU-only by default, limited to one CPU core at
the cgroup boundary, runs at nice level 19 with idle I/O priority, and receives
a hard memory limit derived from `--max-rss-mib`.

## 10. Compare snapshots

```sh
./bin/rkc diff /tmp/atlas-before /tmp/atlas-after
```

Use graph commands to inspect a changed node’s impact:

```sh
./bin/rkc impact --dir /tmp/atlas-after --node '<node-id>'
```

## 11. Produce the complete distributable

```sh
make safe-complete-package
```

The package builder refuses to proceed without release verification and two
cache-isolated, byte-identical builds. The coherent output is under
`dist/release`.
