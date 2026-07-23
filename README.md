# Repository Knowledge Compiler (RKC)

RKC compiles a source repository into an immutable, evidence-backed Repository
Knowledge Representation and derives documentation, graph navigation, search,
NotebookLM-ready text, CI quality reports, integration exports, and optional
local-model explanations from that representation.

The governing rule is deliberately unromantic:

> Parsers, compilers, manifests, indexes, and authorized runtime observations
> establish facts. A language model may explain bounded facts, but it may not
> invent repository truth.

The current release is a runnable, dependency-light reference platform. It is
substantially beyond a toy scanner, while still stating the remaining
commercial-production work instead of assigning it a heroic version number.

## Implemented now

The reference build provides:

- local-directory and constrained remote-Git acquisition;
- complete artifact accounting, SHA-256 hashing, language/media classification,
  explicit exclusion records, and repository/file limits;
- deterministic Python AST, Go AST, and JavaScript/TypeScript syntax adapters;
- Markdown structure, package/build manifest, OpenAPI JSON, JSON Schema,
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
- a 15-stage deterministic scan DAG with cancellation propagation, isolated
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

## What remains before commercial production 1.0

The highest-value unfinished work is:

1. **Completed on current `main`:** make the durable SQLite `rkcstore`
   writer/query implementation available across local CLI, HTTP, and MCP paths,
   with transactional staging, recovery, pagination, and read-only consumers;
2. route every scan stage through the deterministic DAG scheduler and cache;
3. enforce plugin capabilities with a WASI host and isolated native workers;
4. add compiler-grade semantic adapters, beginning with Python, TypeScript, Go,
   C/C++, Rust, Java/Kotlin, and C#;
5. add SQL, protobuf, GraphQL, Terraform, Kubernetes, CI, and richer build packs;
6. build the paginated TypeScript browser and editor integrations;
7. benchmark a real quantized GGUF model below the 2.5 GiB guarded ceiling;
8. implement PostgreSQL/object-storage team mode, authentication, authorization,
   queues, audit retention, backups, and operational telemetry;
9. publish signed binaries and containers, container SBOMs, provenance, and
   measured adapter accuracy over a maintained benchmark corpus; per-binary
   Go-module and complete-distribution SPDX SBOMs are already generated and
   packaged.

The exact ordered work, interfaces, migrations, tests, and exit gates are in
[`docs/REMAINDER_IMPLEMENTATION_PLAN.md`](docs/REMAINDER_IMPLEMENTATION_PLAN.md).

## Start here

- [`docs/QUICKSTART.md`](docs/QUICKSTART.md): install, scan, verify, browse, query,
  synthesize, and package.
- [`docs/IMPLEMENTATION_STATUS.md`](docs/IMPLEMENTATION_STATUS.md): implemented,
  partial, and planned features.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): system boundaries and data flow.
- [`docs/data-model.md`](docs/data-model.md): canonical records and invariants.
- [`docs/plugin-sdk.md`](docs/plugin-sdk.md): plugin and GraphPatch contracts.
- [`docs/MODEL_RUNTIME.md`](docs/MODEL_RUNTIME.md): bounded local-model design.
- [`docs/SELF_CATALOGUE.md`](docs/SELF_CATALOGUE.md): guarded, non-recursive
  compilation of RKC's committed source into its own verified atlas.
- [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md): hostile-repository threat model.
- [`docs/OPERATIONS.md`](docs/OPERATIONS.md): deployment and operational practice.
- [`docs/RELEASE_VALIDATION.md`](docs/RELEASE_VALIDATION.md): exact verification
  performed by the package builder.
- [`docs/implementation-plan.md`](docs/implementation-plan.md): original complete
  product specification, retained and extended.
- [`docs/backlog.md`](docs/backlog.md): stable engineering issue catalogue.

## One-minute local atlas

The dependency-light profile works on every supported RKC platform and does
not need a model, daemon, database server, or Python sandbox:

```sh
make build
./bin/rkc doctor --repository .
./bin/rkc plan --no-python .
./bin/rkc scan --no-python --out .rkc --state-dir .rkc-state --force .
./bin/rkc cache verify
./bin/rkc check --coverage .rkc/coverage.json --bundle .rkc/bundle.json
./bin/rkc serve --dir .rkc
```

Open `http://127.0.0.1:8787`. The generated `.rkc` atlas is portable and the
`.rkc-state` directory retains immutable local snapshots. Both paths are
explicit default inventory exclusions, so rerunning RKC on its own checkout
does not recursively ingest prior output.

Incremental analyzer payloads live under the operating system's user-cache
directory by default (for example, `$XDG_CACHE_HOME/rkc/stages` on Linux), never
inside the scanned repository or generated atlas. `rkc plan` shows exactly
which stages will execute or reuse verified payloads and why. Pass
`scan --no-cache` for a clean run; clean and cache-assisted scans intentionally
share the same snapshot identity and canonical digest.

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
- on Linux only, a reachable user-systemd manager for the optional Python AST
  adapter and the guarded `safe-*` development targets.

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

The full logged release sequence is:

```sh
make safe-release-verify
```

The `safe-*` targets run local builds and tests at nice level 19 and idle I/O
priority inside a fail-closed user cgroup capped at one CPU core and 2.5 GiB RAM.
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
size-limited canonical evidence packet. Output is either citation-backed claims
or an explicit abstention, and it is written to standard output rather than fed
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
  'How does snapshot publication fail closed?'
```

The same exact model/runtime qualification boundary applies to both retrieval
and generation. With the committed lock's current null defaults, the command
intentionally refuses model execution rather than presenting an unqualified
answer.

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
├── notebooklm/                 bounded coherent source pack
├── integrations/               SARIF, GraphML, Mermaid, and CSV
├── search/                     persisted lexical index
├── site/                       static repository atlas
```

Optional model packets and citation-linked prose are kept outside that verified
tree under `/tmp/rkc-output.rkc-derived/synthesis/<profile>/` by default.

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
Generated configuration uses an immutable commit-pinned GitHub schema URL, so
its editor association remains valid regardless of the chosen output directory;
the checked-in example uses a repository-local relative schema path for
offline checkout navigation.
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
  serve --dir /output/atlas --addr 0.0.0.0:8787
```

The named output and state volumes survive the one-shot scan container. Remove
them only when their generated data is no longer needed (`docker compose down
--volumes`).

The equivalent explicit Docker invocation is:

```sh
docker build -t rkc:local .
docker run --rm \
  --cpus 1 --cpu-shares 2 \
  --memory 2560m --memory-reservation 2048m --memory-swap 2816m \
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
Compose file encodes that portable profile and additionally applies a one-core quota, 2 GiB memory
reservation, 2.5 GiB hard memory limit, 256 MiB swap allowance, 128-process
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
target gives priority to ERAIS and applies the same one-core, 2.5-GiB cgroup to
release verification, cross-compilation, SBOM rebinding, and ZIP assembly.

## Security status

Repositories are treated as hostile input. The reference build avoids project
code execution and redacts likely secrets from normalized exports by default.
The only executable Python adapter is the digest-pinned built-in worker. On
Linux it runs under hard cgroup limits with a cleared environment, network-I/O
syscalls denied, one task, and whole-unit cancellation; it still runs as the
invoking user and does not claim a mount/filesystem namespace. Third-party
Python/native workers are disabled. Do not expose the local server as a
multi-tenant internet service; full worker isolation remains a production gate.

## License

RKC-owned work is Apache-2.0 and may be used in commercial products and
derivative works subject to the license's notice and attribution requirements;
retain [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE) as required by Section 4.
Third-party compilers, parsers, language servers, grammars, plugins, and model
weights retain their own licenses and are not bundled by this project.
