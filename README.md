# Repository Knowledge Compiler (RKC)

**Turn a codebase into a cited, searchable map—without trusting an LLM to
invent the map.**

[![CI](https://github.com/neuroforge-io/RKC/actions/workflows/ci.yml/badge.svg)](https://github.com/neuroforge-io/RKC/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

RKC compiles a local directory or Git repository into an immutable,
evidence-backed atlas. Search symbols and signatures, follow compiler-resolved
relationships, inspect source and diagnostics, generate documentation, query
GraphRAG context, serve the responsive local workbench, or hand a bounded
evidence packet to an optional local model.

```sh
./install.sh
export PATH="$HOME/.local/bin:$PATH"
rkc open /path/to/repository
# In another terminal, query the verified atlas while the browser is running:
rkc query --dir /path/to/repository/.rkc "where is authentication enforced?"
```

No model, daemon, database server, package-manager hook, or network connection
is required for the default local workflow.

The governing rule is deliberately unromantic:

> Parsers, compilers, manifests, indexes, and authorized runtime observations
> establish facts. A language model may explain bounded facts, but it may not
> invent repository truth.

## Why RKC

- **Evidence before prose.** Every compiler, syntax, manifest, schema, and
  framework fact keeps its source and producer.
- **Compiler intelligence when available.** Import one or more SCIP indexes for
  exact definitions, references, implementations, signatures, documentation,
  diagnostics, and UTF-8/16/32 source ranges.
- **Useful without AI.** Lexical/FTS5 search, graph traversal, GraphRAG
  expansion, deterministic docs, browser exploration, HTTP, MCP, SQLite, and
  portable JSON/JSONL work with no model.
- **LLM output cannot become input truth.** Model products live outside the
  verified atlas, cite bounded evidence, and are never recursively scanned by
  the guarded self-catalogue.
- **Hostile-repository posture.** Normal scans do not execute repository code.
  Inputs, outputs, caches, journals, snapshots, and optional models have
  explicit containment and resource contracts.
- **Portable and commercially usable.** RKC-owned code is MIT-licensed with
  simple attribution, builds CGO-free binaries, and retains deterministic SPDX
  evidence. Third-party components remain separately attributed.

## Implemented now

The reference build provides:

- local-directory and constrained remote-Git acquisition;
- complete artifact accounting, SHA-256 hashing, language/media classification,
  explicit exclusion records, and repository/file limits;
- deterministic Python AST, Go AST, and JavaScript/TypeScript syntax adapters;
- a streaming, dependency-free SCIP semantic adapter for compiler-produced
  indexes, covering Python, JavaScript/TypeScript, Go, C/C++/CUDA, Rust,
  Java/Kotlin/Scala, C#/Visual Basic, Ruby, Dart, PHP, and any conforming SCIP
  producer;
- Markdown structure, package/build manifest, bounded OpenAPI JSON/YAML, JSON Schema,
  environment-template, Docker, and secret-pattern extraction;
- a versioned language-neutral graph containing artifacts, nodes, typed edges,
  evidence, diagnostics, conflicts, documents, claims, and execution paths;
- explicit unresolved-symbol nodes instead of discarded or guessed relations;
- canonical sorting, validation, deterministic digests, and coverage ratios;
- a typed transactional snapshot-store boundary with a concurrency-safe
  in-memory conformance backend and a durable pure-Go SQLite backend, including
  staged publication, OS-backed writer leases, recovery, authenticated keyset
  cursors, bounded pagination, exact coverage binding, and lossless export;
- SQLite-backed scan, query, answer, graph, snapshot, browser, synthesis, and
  MCP paths with immutable migrations, verified module hashes, CGO-disabled
  build gates, read-only consumers, and strict database-open health checks;
- crash-safe filesystem snapshots and content-addressed object storage;
- a 16-stage deterministic scan DAG with cancellation propagation, isolated
  analyzer fragments, bounded CPU/memory/process/open-file admission, ownership
  receipts, verified CAS payload caching,
  selective language/configuration invalidation, clean-scan equivalence tests,
  and `plan` plus cache inspect/verify/prune commands;
- ranked lexical search, qualification-gated semantic and hybrid retrieval,
  graph neighbourhoods, shortest paths, impact traversal, strongly connected
  components, and semantic snapshot diffs;
- deterministic Markdown documentation, normalized/redacted source envelopes,
  NotebookLM packs, JSONL, SARIF, GraphML, Mermaid, CSV, and a static browser;
- a read-only HTTP API and Model Context Protocol server;
- bounded evidence packets, a CPU-only `llama.cpp` CLI provider, RSS policy,
  prompt-isolation rules, citation and claim validation, and a grounded
  repository-answer command;
- plugin manifests, lockfiles, GraphPatch validation, and an external Python
  worker protocol;
- offline contract, documentation, determinism, API, MCP, Git-acquisition,
  race-detector, benchmark, and release-package verification, including
  deterministic SPDX 2.3 Go-module SBOMs cryptographically rebound to each
  binary's checksum, command path, target GOOS/GOARCH, normalized architecture
  tuning, default Go experiment set, `GOFIPS140=off`, exact Go toolchain, immutable source
  commit/tree/time, module lock, canonical Go purls, and linked module inventory
  during final package assembly; dependency declarations are retained while
  license conclusions remain `NOASSERTION` without file-level analysis.
- a deterministic complete-distribution SPDX 2.3 SBOM covering substantive
  archive files, platform binary components, and their linked Go modules; its
  self-reference exclusions are explicit, `MANIFEST.json` hashes the SBOM, and
  `SHA256SUMS.txt` hashes both receipts;

## Honest boundaries

RKC does not silently install or execute language toolchains. Compiler index
generation is an explicit, separately authorized build step; RKC then imports
the resulting `index.scip` as inert, digest-bound data. See
[`docs/SCIP_SEMANTIC_ADAPTERS.md`](docs/SCIP_SEMANTIC_ADAPTERS.md).

No tested permissively licensed generation model currently meets every RKC quality,
prompt-injection, exact-32K, latency, and 4.5 GiB gate on the protected one-core
CPU profile. RKC therefore ships no weak or misleading default. Deterministic
retrieval and GraphRAG remain complete, while model-backed commands fail closed.
The measurements and rejection receipts are in
[`docs/MODEL_SELECTION.md`](docs/MODEL_SELECTION.md).

Team-service tenancy, general third-party worker containment, and signed public
release attestations remain explicit future scopes. Current claims and exact
boundaries are maintained in
[`docs/IMPLEMENTATION_STATUS.md`](docs/IMPLEMENTATION_STATUS.md).

## Start here

- [`docs/QUICKSTART.md`](docs/QUICKSTART.md): install, scan, verify, browse, query,
  synthesize, and package.
- [`docs/IMPLEMENTATION_STATUS.md`](docs/IMPLEMENTATION_STATUS.md): implemented,
  partial, and planned features.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): system boundaries and data flow.
- [`docs/SCIP_SEMANTIC_ADAPTERS.md`](docs/SCIP_SEMANTIC_ADAPTERS.md):
  compiler-grade language indexing and safe import.
- [`docs/data-model.md`](docs/data-model.md): canonical records and invariants.
- [`docs/plugin-sdk.md`](docs/plugin-sdk.md): plugin and GraphPatch contracts.
- [`docs/MODEL_RUNTIME.md`](docs/MODEL_RUNTIME.md): bounded local-model design.
- [`docs/MODEL_SELECTION.md`](docs/MODEL_SELECTION.md): current small-model
  research, measured candidates, rejection evidence, and promotion gates.
- [`docs/SELF_CATALOGUE.md`](docs/SELF_CATALOGUE.md): guarded, non-recursive
  compilation of RKC's committed source into its own verified atlas.
- [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md): hostile-repository threat model.
- [`docs/OPERATIONS.md`](docs/OPERATIONS.md): deployment and operational practice.
- [`docs/RELEASE_VALIDATION.md`](docs/RELEASE_VALIDATION.md): exact verification
  performed by the package builder.
- [`docs/QUALITY_INDEX.md`](docs/QUALITY_INDEX.md): deterministic test,
  documentation, profiling, and change-delta inventory for maintainers.
- [`docs/BRANDING_AND_ATTRIBUTION.md`](docs/BRANDING_AND_ATTRIBUTION.md):
  NeuroForgeIO identity, MIT terms, commercial use, and third-party boundaries.
- [`docs/implementation-plan.md`](docs/implementation-plan.md): original complete
  product specification, retained and extended.
- [`docs/backlog.md`](docs/backlog.md): stable engineering issue catalogue.

## One-minute local atlas

From a source checkout, installation and the guided first run are two commands:

```sh
./install.sh
rkc wizard
```

The dependency-free terminal guide asks which folder to catalogue, then lets a
non-technical user build and open the read-only browser atlas, compile without
starting a server, view the complete command help, or cancel without starting
work. It intentionally covers the safe first-run workflows rather than claiming
full CLI parity. `rkc tui` is an alias. EOF and explicit cancellation stop
without starting a scan. Scripts and experienced users can continue to run
`rkc open .` or `rkc quickstart .` directly.

`open` performs the scan and locked integrity/quality checks, starts the
loopback read-only browser, and opens the default desktop browser when one is
available. On Linux, the installed command first re-executes itself inside the
exact one-core, nice-19, idle-I/O, 4 GiB pressure / 4.5 GiB hard-memory envelope;
admission and continuous monitoring yield to visible ERAIS or evaluation work
before RKC can create an atlas, cache, journal, or snapshot. The browser opener
runs outside that disposable service, so stopping RKC does not place the
desktop browser inside RKC's cgroup.

Press Ctrl-C in the terminal to stop cleanly. Use `open --no-browser` on
headless hosts; the URL is still printed. The command accepts any local folder,
not only Git worktrees, and places the atlas in `<folder>/.rkc` while retaining
immutable snapshots in `<folder>/.rkc-state`. On a trusted single-user Linux
checkout, `rkc open --workbench <folder>` explicitly adds the guarded local
command center. The static first-run path remains the portable default on
macOS and Windows while equivalent native workbench admission remains open.

The opt-in workbench is a trusted-user local command launcher, not a filesystem
sandbox. Its session token grants RKC commands the invoking account's file
authority; `--workspace` sets the working directory and defaults. Keep static
mode for untrusted users or content, and launch the workbench only on a trusted
single-user account.

The opt-in workbench capability never appears in a process argument or ordinary
server URL. The guarded child atomically writes it into an owner-private
readiness receipt; the outer `open` process validates that bounded receipt and
launches an owner-private redirect outside the cgroup. The browser removes the
fragment before exchanging it once for the same-origin session token.

Maintainers can pair the atlas with a deterministic quality/change report:
`make quality-index` writes `.rkc-quality/index.json` and `index.md`, including
source hashes, test and documentation evidence, optional Go/Python profile
coverage, exact exported-Go documentation gaps, and Git deltas.

The dependency-light profile does not need a model, daemon, database server, or
Python sandbox. `quickstart` remains the equivalent headless compile-and-check
command and prints the exact search, browser, and cited-answer entry points.
Protected `open` deliberately rejects `--python` until the adapter and parent
scan can prove one aggregate ceiling; the deterministic Go/TypeScript/document
path remains complete without it.

If a compiler indexer has produced `index.scip`, add compiler-grade semantics
without changing the safe scan boundary:

```sh
rkc quickstart --scip-index /path/to/index.scip /path/to/repository
```

Repeat `--scip-index` for polyglot workspaces. The same flag is available on
`plan` and `scan`, and in the GUI command center.

The generated `.rkc` atlas is portable and the `.rkc-state` directory retains
immutable local snapshots. Both paths are explicit default inventory
exclusions, so rerunning RKC on its own checkout does not recursively ingest
prior output.

Incremental analyzer payloads live under the operating system's user-cache
directory by default (for example, `$XDG_CACHE_HOME/rkc/stages` on Linux), never
inside the scanned repository or generated atlas. `rkc plan` shows exactly
which stages will execute or reuse verified payloads and why. Pass
`scan --no-cache` for a clean run; clean and cache-assisted scans intentionally
share the same snapshot identity and canonical digest.

Each scan that reaches DAG execution creates a new owner-only, append-only
command journal outside the repository (for example,
`$XDG_CACHE_HOME/rkc/runs` on Linux). Its terminal result covers later policy,
SQLite, atlas, and snapshot-publication failures—not only DAG completion. The
scan summary prints its unguessable run ID and exact path. Inspect recent runs
with `rkc runs list`, or strictly replay one complete lifecycle with
`rkc runs show <run-id>`. Use `scan --runs-dir <path>` and the corresponding
inspection flag when an explicit state location is required; RKC never
overwrites an existing run journal.

Run `./bin/rkc doctor --strict` before omitting `--no-python`. The built-in
Python adapter intentionally requires Python 3.11 or newer and its fail-closed
Linux user-systemd isolation boundary. Go and JavaScript/TypeScript analysis,
framework extraction, graph export, search, and browsing remain available in
the portable profile.

## Requirements

- a supported Go toolchain to build from source (CI and release images pin Go
  1.26.5; the module uses Go 1.25 language semantics required by its pinned
  SQLite dependency graph);
- Python 3.11 or later plus `requirements-dev.txt` for repository validation;
- Git for repository metadata and remote acquisition;
- `curl` for the HTTP smoke test;
- on Linux only, a reachable user-systemd manager for protected `rkc open`, the
  optional Python AST adapter, the local workbench, and guarded `safe-*`
  development targets.

Prebuilt RKC binaries do not require the Go toolchain. A local-directory scan
with `--no-python` does not require Python, Git, a model runtime, or network
access. `rkc doctor` reports which optional capabilities are available and
provides a remediation for each missing one.

The runtime pins `modernc.org/sqlite` and its transitive pure-Go module graph in
`go.mod` and `go.sum`. The reviewed dependency/license inventory is locked in
`third_party/go-modules.lock.json`; builds run with `CGO_ENABLED=0` and verify
the downloaded module cache before tests or packaging.

## Build and fully verify

```sh
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements-dev.txt
make go-mod-verify
make safe-verify
make safe-test-race
```

To verify that both CGO-free commands compile for the maintained Linux, macOS,
and Windows `amd64`/`arm64` targets without publishing artifacts, run
`make portable-build`. The reference distribution currently publishes Linux
binaries and keeps native packaging/install smoke for the other targets as a
separate release gate.

The full logged release sequence is:

```sh
make safe-release-verify
```

The `safe-*` targets run local builds and tests at nice level 19 and idle I/O
priority inside a fail-closed user cgroup capped at one CPU core and 4.5 GiB RAM.
CI provisions the same delegated guard around expensive verification and
package/self-catalogue assembly inside its disposable runner.

## Scan and browse

```sh
make build

./bin/rkc scan \
  --no-python \
  --out /tmp/rkc-output \
  --state-dir /tmp/rkc-state \
  --force \
  examples

./bin/rkc check \
  --coverage /tmp/rkc-output/coverage.json \
  --bundle /tmp/rkc-output/bundle.json \
  --min-inventory-accounting 1 \
  --min-symbol-evidence 1 \
  --min-edge-resolution 0.5 \
  --max-errors 0 \
  --max-high-confidence-secrets 0

./bin/rkc serve --dir /tmp/rkc-output --addr 127.0.0.1:8787
```

Snapshot state directories carry a bounded `.rkc-snapshot-store.json`
ownership marker. RKC initializes a missing or empty state directory, but
refuses to adopt a nonempty unmarked directory. Recovery deletes only building
directories whose exact inode, bounded marker, and bounded building record
still agree at the deletion boundary.

Open `http://127.0.0.1:8787`.

The served GUI starts from bounded overview, diagnostic, and entity windows and
uses the local API for search, node detail, evidence, and graph neighbourhoods;
it does not transfer the whole repository graph at browser startup. For a
trusted single-user Linux checkout, the preferred protected command-center
entry point is:

```sh
rkc open --workbench .
```

For an existing atlas, the low-level advanced route is to
keep the server loopback-only and start it inside RKC's fail-closed resource
envelope. Direct `serve --workbench` requires a nonexistent `--ready-file`
inside an owner-private directory and rejects `--open`; a trusted launcher must
consume the receipt without logging its `browser_url` capability:

```sh
rkc_ready_directory=$(mktemp -d)
chmod 700 "$rkc_ready_directory"
scripts/with-rkc-limits.sh ./bin/rkc serve \
  --dir /tmp/rkc-output \
  --addr 127.0.0.1:0 \
  --workbench \
  --workspace . \
  --ready-file "$rkc_ready_directory/ready.json"
```

Workbench jobs use exact argument arrays without a shell, require a random
same-origin token established through the one-time bootstrap, run one at a
time, cap captured output, and inherit the one-core / 4.5 GiB / idle-I/O
envelope. Static exports and ordinary `serve` remain read-only. Commands that
could create a separately managed Python or model unit currently fail closed in
the workbench until one aggregate session ceiling is proved; their normal
guarded CLI paths remain available.

The low-level route checks ERAIS before and after atlas preparation and during
the server lifetime, while remaining cgroup-subordinate throughout. Prefer
`rkc open --workbench` when continuous outer-process pre-emption during the
initial atlas load is required.

Workbench serving always uses an OS-selected ephemeral loopback port. Combined
with browser policy that forbids workers and manifests and with current-binary
UI regeneration, this prevents browser code left by an older or imported atlas
on the familiar read-only origin from intercepting a privileged session.
Every live `serve` path regenerates executable UI bytes from the current RKC
binary and validated bundle; persisted `site/` files remain a portable static
export but are never treated as authenticated publisher code by the server.

For a durable canonical store, place the database beneath an owner-only
directory and use `--database` instead of `--state-dir`:

```sh
install -d -m 700 /tmp/rkc-store
./bin/rkc scan --no-python --database /tmp/rkc-store/rkc.sqlite --out /tmp/rkc-output --force examples
./bin/rkc snapshots list --database /tmp/rkc-store/rkc.sqlite --limit 20
./bin/rkc query --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>' Login
./bin/rkc serve --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>' --addr 127.0.0.1:8787
./bin/rkc-mcp --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>'
```

The scan summary prints the committed snapshot ID. Read commands require
exactly one `--snapshot` or `--repository`; they never create a missing
database. The generated atlas remains a portable export, while the SQLite file
is the durable source for later reads and snapshot operations.

## Query and inspect graph relationships

```sh
./bin/rkc query --dir /tmp/rkc-output --limit 10 Login
./bin/rkc components --dir /tmp/rkc-output --cycles-only
./bin/rkc impact --dir /tmp/rkc-output --node '<node-id>'
./bin/rkc path --dir /tmp/rkc-output --from '<node-id>' --to '<node-id>'
```

Lexical retrieval is the dependency-free default. Semantic and hybrid retrieval
are available only when every model-supply-chain gate resolves to one exact,
qualified embedding asset, GGUF file, and `llama.cpp` runtime receipt. A new
vector index is published outside the verified atlas and is bound to the
current lexical corpus and model/runtime hashes:

```sh
./bin/rkc query \
  --dir /tmp/rkc-output \
  --mode hybrid \
  --build-vector-index \
  --embedding-model-lock models/models.lock.json \
  --embedding-asset '<qualified-embedding-asset-id>' \
  --embedding-model /path/to/model.gguf \
  --llama-embedding /path/to/llama-embedding \
  --embedding-runtime-receipt /path/to/build-receipt.json \
  Login
```

The committed model lock currently has no generation or embedding default and
its lightweight candidates remain unqualified. Therefore this path fails
closed until an operator runs the published qualification gate and promotes an
asset; RKC does not silently download, select, or trust a model.

## Ask a grounded repository question

`rkc answer` combines bounded lexical, semantic, or hybrid retrieval and graph
expansion with the grounded-answer validator. Lexical remains the zero-model
default; semantic and hybrid modes reuse the same qualified, corpus-bound
vector path documented above. The generation model receives only a
size-limited canonical evidence packet. Every claim is compiled as one atomic
statement. Unsupported claims and unresolved questions can trigger up to two
bounded repair passes: their text is sanitized into search-only queries,
retrieval is expanded, and every candidate source is re-resolved from the
canonical bundle before the model sees it again. The best independently
validated pass becomes either citation-backed claims or an explicit
abstention. Generated output is written to standard output and is never fed
back into the atlas:

```sh
./bin/rkc answer \
  --dir /tmp/rkc-output \
  --mode hybrid \
  --vector-index /tmp/rkc-output.rkc-derived/search/<embedding-asset-id>/vector-index.json \
  --embedding-model-lock models/models.lock.json \
  --embedding-asset '<qualified-embedding-asset-id>' \
  --embedding-model /path/to/embedding-model.gguf \
  --llama-embedding /path/to/llama-embedding \
  --embedding-runtime-receipt /path/to/build-receipt.json \
  --graph-hops 1 \
  --model-lock models/models.lock.json \
  --model-asset '<qualified-generation-asset-id>' \
  --model /path/to/model.gguf \
  --llama-cli /path/to/llama-cli \
  --runtime-receipt /path/to/build-receipt.json \
  --repair-passes 2 \
  'How does snapshot publication fail closed?'
```

The same exact model/runtime qualification boundary applies to both retrieval
and generation. With the committed lock's current null defaults, the command
intentionally refuses model execution rather than presenting an unqualified
answer. JSON output includes every verification pass, repair query, selected
evidence count, rejection count, usage total, and whether gaps remained.

## Build evidence packets without running a model

```sh
./bin/rkc synthesize \
  --dir /tmp/rkc-output \
  --repo-root examples \
  --query Login \
  --packet-only \
  --limit 1 \
  --force
```

Unless `--out` is supplied, synthesis is published to the deterministic sibling
`/tmp/rkc-output.rkc-derived/synthesis/<profile>`. RKC rejects the atlas itself
and every descendant of it as a synthesis destination, including paths that
reach the atlas through a symlinked parent.

Running a real local model additionally requires `llama.cpp` and a GGUF model.
The repository provides a checksum-pinned CPU-only source bootstrap, defensive
on-demand downloads, and a guarded qualification corpus; model weights remain
unbundled and no candidate is a default until it passes the published gate. See
[`docs/MODEL_RUNTIME.md`](docs/MODEL_RUNTIME.md).

## Output layout

```text
/tmp/rkc-output/
├── bundle.json                 canonical portable bundle
├── coverage.json               auditable numerators, denominators, ratios
├── rkc.manifest.json           immutable snapshot identity and provenance
├── graph/                      record-family JSONL exports
├── normalized/                 redacted Markdown source envelopes
├── docs/                       deterministic repository and symbol pages
├── notebooklm/                 ordered Markdown pack + UPLOAD.md guide
├── integrations/               SARIF, GraphML, Mermaid, and CSV
├── search/                     persisted lexical index
├── site/                       static repository atlas
```

Optional model packets and citation-linked prose are kept outside that verified
tree under `/tmp/rkc-output.rkc-derived/synthesis/<profile>/` by default.

The `notebooklm/` directory is ready for notebook-style tools: upload the
ordered Markdown files according to `notebooklm/UPLOAD.md`, then use the
manifest's exact source and byte counts to confirm the destination plan's
limits. RKC's deterministic retrieval and packet-only synthesis remain fully
usable when no local generation model is qualified.

## Catalogue RKC with RKC

From a clean Git worktree, build a complete atlas of RKC's own committed source:

```sh
make self-catalogue
```

The target runs inside the mandatory low-priority cgroup guard, extracts only
verified blobs from the recorded committed Git tree into a private temporary
source tree, and builds RKC from that immutable copy. The complete candidate is
validated in a private sibling before an atomic whole-directory publication to
`dist/self-catalogue`. Generated output, runtime/model trees, model-weight
formats, links, and uncommitted files cannot become scan input. No model is
invoked. `MANIFEST.json` and `SHA256SUMS.txt` bind the source commit, ephemeral
tool binary, snapshot, canonical files, and explicit non-recursion assertions.
See
[`docs/SELF_CATALOGUE.md`](docs/SELF_CATALOGUE.md) for the verification contract.
Measured self-analysis and an immutable scan of the recent
`img2threejs/img2threejs` project are recorded in
[`docs/SHOWCASE_2026-07-27.md`](docs/SHOWCASE_2026-07-27.md).

## Configuration

Generate a complete configuration file:

```sh
./bin/rkc init --path rkc.json
```

`--out` remains accepted as a compatibility alias, but new scripts should use
`--path`. `rkc init --stdout` emits the same configuration without writing a
file.

The schema is [`schemas/config.schema.json`](schemas/config.schema.json), and a
maintained example is [`config/rkc.example.json`](config/rkc.example.json).
Generated configuration uses an immutable commit-pinned URL for the published
`0.2.0` schema, so its editor association cannot drift when `main` advances.
The explicit `schema_version` remains the compatibility boundary. The
checked-in example uses a repository-local relative schema path for offline
checkout navigation.
Configuration affecting repository truth enters the snapshot digest. Display,
server-address, and derived-model settings do not silently change source truth.

The reference inventory does not interpret `.gitignore`. Each
`inventory.exclude` entry is one exact repository-relative path and excludes
that path plus its descendants; glob syntax is not supported. Safe defaults
explicitly omit `.venv`, `venv`, RKC model/runtime/download/generated trees
(including `.rkc-coverage`), `bin`, `dist`, and named root-level coverage and
cache outputs. Additional paths
can be supplied with repeated `--exclude` flags and remain visible as explicit
exclusion records in the atlas.

## API and MCP

The implemented local API is described by [`api/openapi.yaml`](api/openapi.yaml).
The future team-service contract is intentionally separate in
[`api/openapi-service-future.yaml`](api/openapi-service-future.yaml).

Run the MCP server over standard input/output:

```sh
./bin/rkc-mcp --dir /tmp/rkc-output
```

The MCP revision advertised by the reference server is `2025-11-25`.

## Plugins

```sh
./bin/rkc plugins validate --root plugins
./bin/rkc plugins verify --root plugins --lock plugins/plugins.lock.json
```

The manifest schema is [`schemas/plugin-manifest.schema.json`](schemas/plugin-manifest.schema.json),
the mutation contract is [`schemas/graph-patch.schema.json`](schemas/graph-patch.schema.json),
and the WASI component draft is [`plugins/plugin.wit`](plugins/plugin.wit).

Plugin capabilities are validated and locked today. The built-in Python worker
has the narrow fail-closed Linux guard described below; WASI and general
third-party native-worker containment remain production blockers documented in
the remainder plan.

## Container use

The shortest portable container workflow is:

```sh
docker compose build
docker compose run --rm rkc
docker compose run --rm rkc check \
  --coverage /output/atlas/coverage.json \
  --bundle /output/atlas/bundle.json
docker compose run --rm -p 127.0.0.1:8787:8787 rkc \
  serve --dir /output/atlas --addr 0.0.0.0:8787 --allow-remote
```

`serve` otherwise fails closed on non-loopback addresses. The container example
acknowledges its container-wide listener explicitly while publishing it only on
the host loopback interface. `--allow-remote` exposes the read-only API without
application authentication; use a firewall or authenticated reverse proxy when
intentionally publishing it beyond one machine. The command workbench remains
strictly loopback-only regardless of this flag.

The named output and state volumes survive the one-shot scan container. Remove
them only when their generated data is no longer needed (`docker compose down
--volumes`).

The equivalent explicit Docker invocation is:

```sh
docker build -t rkc:local .
docker run --rm \
  --cpus 1 --cpu-shares 2 \
  --memory 4608m --memory-reservation 4096m --memory-swap 4864m \
  --pids-limit 128 --oom-score-adj 750 --blkio-weight 10 \
  --read-only --tmpfs /tmp:size=256m,mode=1777 \
  --security-opt no-new-privileges:true --cap-drop ALL \
  -v "$PWD:/workspace:ro" -v rkc-output:/output \
  rkc:local scan --no-python --out /output/atlas --force /workspace
```

The static `scratch` image contains only the two CGO-free RKC executables,
runtime contracts/configuration, and attribution material; it has no shell,
package manager, Python, or user-systemd manager. Container scans must pass
`--no-python` explicitly; RKC never falls back to unsandboxed Python. The
Compose file encodes that portable profile and additionally applies a one-core quota, 4 GiB memory
reservation, 4.5 GiB hard memory limit, 256 MiB swap allowance, 128-process
limit, minimum CPU/block-I/O weights, high OOM-kill preference, a read-only root
filesystem, `no-new-privileges`, and dropped Linux capabilities. Scheduling
weights are subject to host-kernel support. Use a supported Linux host with
user-systemd when Python AST extraction is required.

## Build the complete release archive

```sh
make safe-complete-package
```

The resulting `dist/release/repository-knowledge-compiler-complete.zip` contains
source materialized directly from
the immutable `HEAD` commit tree, Linux amd64/arm64 binaries built in a private
checkout of that commit, deterministic demonstration artifacts, a canonical
successful-validation receipt, a complete-distribution SPDX SBOM, checksums,
contracts, and all plans. The exact raw validation and benchmark files named by
the receipt are retained at `dist/release/evidence` outside the ZIP but inside
the same atomically published generation. Verification preserves the prior
`dist/evidence` generation until a complete replacement is ready. Assembly
rebuilds binaries, SBOMs, and demo inputs in two detached checkouts with separate
Go build and module caches, uses implementation-independent stored ZIP entries,
and requires final byte equality before one atomic `dist/release` swap. The safe
target gives priority to ERAIS and applies the same one-core, 4 GiB operating /
4.5 GiB hard cgroup to release verification, cross-compilation, SBOM
rebinding, and ZIP assembly.

## Security status

Repositories are treated as hostile input. The reference build avoids project
code execution and redacts likely secrets from normalized exports by default.
The only executable Python adapter is the digest-pinned built-in worker. On
Linux it runs under hard cgroup limits with a cleared environment, network-I/O
syscalls denied, one task, and whole-unit cancellation. Its host and worker both
verify repository confinement, regular-file identity, exact size, and SHA-256
for every inventoried input before parsing. It still runs as the invoking user
and does not claim a mount/filesystem namespace. Third-party
Python/native workers are disabled. Do not expose the local server as a
multi-tenant internet service; full worker isolation remains a production gate.

## License

RKC-owned work is MIT-licensed and may be used in commercial products and
derivative works. The MIT License requires copies or substantial portions to
retain its copyright and permission notice. NeuroForgeIO additionally requests
that redistributions retain [`NOTICE`](NOTICE) and credit NeuroForgeIO and the
RKC contributors; this request adds no restriction. Third-party and model terms
remain separate and are listed in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
Third-party compilers, parsers, language servers, grammars, plugins, and model
weights retain their own licenses and are not bundled by this project.

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
