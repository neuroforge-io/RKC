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
7. benchmark a real quantized GGUF model below the 3.5 GiB configured ceiling;
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

- Go 1.23 or later;
- Python 3.11 or later for the Python analyzer and validation scripts;
- Git for repository metadata and remote acquisition;
- `jsonschema` and `PyYAML` for offline contract validation;
- `curl` for the HTTP smoke test.

No third-party Go modules are required by the reference implementation.

## Build and fully verify

```sh
python3 -m pip install jsonschema PyYAML
make verify
make test-race
```

The full logged release sequence is:

```sh
make release-verify
```

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

Running a real local model additionally requires a compatible `llama-cli` and a
GGUF model. Model weights are not bundled.

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
└── derived/                    optional model packets and validated prose
```

## Configuration

Generate a complete configuration file:

```sh
./bin/rkc init --out rkc.json
```

The schema is [`schemas/config.schema.json`](schemas/config.schema.json), and a
maintained example is [`config/rkc.example.json`](config/rkc.example.json).
Configuration affecting repository truth enters the snapshot digest. Display,
server-address, and derived-model settings do not silently change source truth.

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

Plugin capabilities are validated and locked today. Enforced WASI and native
worker containment remain production blockers documented in the remainder plan.

## Container use

```sh
docker build -t rkc:local .
docker run --rm -v "$PWD:/workspace:ro" -v rkc-output:/output \
  rkc:local scan --out /output --force /workspace
```

The Compose file additionally applies a read-only root filesystem,
`no-new-privileges`, and drops Linux capabilities.

## Build the complete release archive

```sh
make complete-package
```

The resulting deterministic ZIP contains source, Linux amd64/arm64 binaries,
demonstration output, release logs, checksums, contracts, and all plans.

## Security status

Repositories are treated as hostile input. The reference build avoids project
code execution and redacts likely secrets from normalized exports by default.
The current native Python worker still runs with the invoking user’s OS
permissions. Do not deploy untrusted third-party plugins or expose the local
server as a multi-tenant internet service. The production isolation work is a
release gate, not optional varnish.

## License

Apache-2.0. Third-party compilers, parsers, language servers, grammars, plugins,
and model weights retain their own licenses and are not bundled by this project.
