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
- crash-safe filesystem snapshots and content-addressed object storage;
- ranked lexical search, graph neighbourhoods, shortest paths, impact traversal,
  strongly connected components, and semantic snapshot diffs;
- deterministic Markdown documentation, normalized/redacted source envelopes,
  NotebookLM packs, JSONL, SARIF, GraphML, Mermaid, CSV, and a static browser;
- a read-only HTTP API and Model Context Protocol server;
- bounded evidence packets, a CPU-only `llama.cpp` CLI provider, RSS policy,
  prompt-isolation rules, and model-claim validation;
- plugin manifests, lockfiles, GraphPatch validation, and an external Python
  worker protocol;
- offline contract, documentation, determinism, API, MCP, Git-acquisition,
  race-detector, benchmark, and release-package verification.

## What remains before commercial production 1.0

The highest-value unfinished work is:

1. make SQLite, rather than the filesystem bundle, the canonical local runtime
   writer and query store;
2. route every scan stage through the deterministic DAG scheduler and cache;
3. enforce plugin capabilities with a WASI host and isolated native workers;
4. add compiler-grade semantic adapters, beginning with Python, TypeScript, Go,
   C/C++, Rust, Java/Kotlin, and C#;
5. add SQL, protobuf, GraphQL, Terraform, Kubernetes, CI, and richer build packs;
6. build the paginated TypeScript browser and editor integrations;
7. benchmark a real quantized GGUF model below the 2.5 GiB guarded ceiling;
8. implement PostgreSQL/object-storage team mode, authentication, authorization,
   queues, audit retention, backups, and operational telemetry;
9. publish signed binaries, containers, SBOMs, provenance, and measured adapter
   accuracy over a maintained benchmark corpus.

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
- [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md): hostile-repository threat model.
- [`docs/OPERATIONS.md`](docs/OPERATIONS.md): deployment and operational practice.
- [`docs/RELEASE_VALIDATION.md`](docs/RELEASE_VALIDATION.md): exact verification
  performed by the package builder.
- [`docs/implementation-plan.md`](docs/implementation-plan.md): original complete
  product specification, retained and extended.
- [`docs/backlog.md`](docs/backlog.md): stable engineering issue catalogue.

## Requirements

- a supported Go toolchain (CI and release images pin Go 1.26.5; the module
  retains Go 1.23 language compatibility);
- Python 3.11 or later for the Python analyzer and validation scripts;
- Git for repository metadata and remote acquisition;
- `jsonschema` and `PyYAML` for offline contract validation;
- `curl` for the HTTP smoke test.

No third-party Go modules are required by the reference implementation.

## Build and fully verify

```sh
python3 -m pip install jsonschema PyYAML
make safe-verify
make safe-test-race
```

The full logged release sequence is:

```sh
make safe-release-verify
```

The `safe-*` targets run local builds and tests at nice level 19 and idle I/O
priority inside a fail-closed user cgroup capped at one CPU core and 2.5 GiB RAM.
CI uses the ordinary targets inside its disposable runner.

## Scan and browse

```sh
make build

./bin/rkc scan \
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

## Query and inspect graph relationships

```sh
./bin/rkc query --dir /tmp/rkc-output --limit 10 Login
./bin/rkc components --dir /tmp/rkc-output --cycles-only
./bin/rkc impact --dir /tmp/rkc-output --node '<node-id>'
./bin/rkc path --dir /tmp/rkc-output --from '<node-id>' --to '<node-id>'
```

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

## Configuration

Generate a complete configuration file:

```sh
./bin/rkc init --out rkc.json
```

The schema is [`schemas/config.schema.json`](schemas/config.schema.json), and a
maintained example is [`config/rkc.example.json`](config/rkc.example.json).
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

The Alpine image intentionally has no user-systemd manager and therefore cannot
enforce the host Python-worker sandbox. Container scans must pass `--no-python`
explicitly; RKC never falls back to unsandboxed Python. The Compose file encodes
that portable profile and additionally applies a one-core quota, 2 GiB memory
reservation, 2.5 GiB hard memory limit, 256 MiB swap allowance, 128-process
limit, minimum CPU/block-I/O weights, high OOM-kill preference, a read-only root
filesystem, `no-new-privileges`, and dropped Linux capabilities. Scheduling
weights are subject to host-kernel support. Use a supported Linux host with
user-systemd when Python AST extraction is required.

## Build the complete release archive

```sh
make complete-package
```

The resulting deterministic ZIP contains source, Linux amd64/arm64 binaries,
demonstration output, release logs, checksums, contracts, and all plans.

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
