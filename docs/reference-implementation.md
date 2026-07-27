# Reference implementation

The executable reference release proves the canonical records, analyzer merge,
failure boundaries, deterministic exports, local interfaces, and optional model
pipeline with a small dependency surface.

## Current pipeline

```text
local path or constrained Git URL
  -> acquisition and Git identity
  -> inventory, limits, hash, language/media classification
  -> repository and artifact nodes
  -> Python AST adapter
  -> Go AST adapter
  -> JavaScript/TypeScript syntax adapter
  -> optional compiler-produced SCIP semantic indexes
  -> Markdown, OpenAPI, JSON Schema, manifest, environment, secret packs
  -> merge and deduplication
  -> conservative unique-target resolution
  -> explicit unresolved nodes
  -> vocabulary, evidence, and reference validation
  -> coverage and canonical digest
  -> immutable filesystem snapshot
  -> JSON/JSONL, docs, redacted source, NotebookLM, search, browser, integrations
```

## Language behavior

### Python

The standard-library AST worker extracts modules, classes, functions, methods,
tests, arguments, return annotations, imports, calls, and inheritance spelling.
It does not perform full import resolution or runtime type inference.

### Go

The Go AST adapter extracts packages, modules, declarations, functions, methods,
receivers, structs, interfaces, fields, parameters, returns, imports, calls, and
tests. It does not yet invoke `go/packages`, type checking, build-tag variants,
or SSA call graphs.

### JavaScript and TypeScript

The dependency-free parser extracts modules, imports, CommonJS references,
functions, arrows, classes, interfaces, type aliases, enums, constructors,
methods, parameters, return annotations, export state, inheritance/implementation
spelling, calls, and conservative Express-style routes.

It is explicitly a syntax adapter. The TypeScript compiler API remains the
semantic source for overloads, project references, path mappings, inferred
types, and resolved symbols.

### Compiler semantics through SCIP

The streaming SCIP importer preserves compiler/indexer-resolved symbols,
definitions, references, implementations, signatures, documentation,
diagnostics, and UTF-8/16/32 source ranges. The same adapter accepts indexes
from scip-python, scip-typescript, scip-go, rust-analyzer/scip-rust,
scip-clang, scip-java, scip-dotnet, and other conforming producers. Explicit
indexes are supplied with repeatable `--scip-index` flags on `quickstart`,
`plan`, or `scan`.

RKC does not execute those indexers. It rejects unsafe paths, symlinks,
malformed or oversized Protobuf, ambiguous positions, non-inventoried
documents, and inputs that change before merge. Every compiler fact is bound
to the exact index digest and remains distinguishable from syntax inference.
See [`SCIP_SEMANTIC_ADAPTERS.md`](SCIP_SEMANTIC_ADAPTERS.md).

## Framework behavior

- Markdown: heading tree, sections, internal/external links, fenced-code metadata;
- OpenAPI: bounded JSON/YAML services, paths, operations, parameters, responses, schemas,
  security schemes, serialization relations, and unresolved `$ref` records;
- JSON Schema: schemas, properties, definitions, required/type/format metadata,
  and references;
- manifests: `package.json`, `go.mod`, requirements files, Docker stages,
  dependencies, scripts, CLI entry points, base images, and environment values;
- environment files: keys, defaults, required state, and secret likelihood;
- security: deterministic high-signal credential patterns and redacted exports.

Cross-file OpenAPI reference resolution, deeper Docker/Compose/Kubernetes interpretation, SQL, protobuf,
GraphQL, Terraform, and CI workflows remain production work.

## Canonical and derived output

`bundle.json`, `coverage.json`, and `rkc.manifest.json` are the portable source
of truth for one generated output directory. Record-family JSONL is a canonical
export. Documentation, the static site, NotebookLM pack, search index, SARIF,
GraphML, Mermaid, and CSV are derived products.

Model packets and responses live in a separate sibling output tree
(`<atlas>.rkc-derived/synthesis/<profile>` by default). The synthesis command
rejects explicit atlas descendants and tests that `bundle.json` is unchanged
after derived output is written.

## Current storage

The scan always writes a deterministic portable filesystem atlas. It can also
publish through either the immutable filesystem snapshot store or the durable
SQLite `SnapshotWriter` transaction. SQLite-backed readers power query, answer,
graph, snapshots, browser serving, synthesis, and MCP with exact snapshot or
repository-current selectors; read paths open the existing database read-only.

## Search and graph

The persisted lexical index ranks exact names, qualified names, signatures,
paths, and textual fields. It supports language, kind, object-type, and path
filters. Graph operations include neighbourhood traversal, shortest paths,
reverse impact, and strongly connected components with bounded node/depth
limits.

## Local model path

The reference model runtime:

- builds coherent bounded evidence packets;
- redacts secret findings;
- estimates GGUF weight and KV-cache memory;
- invokes `llama-cli` without a shell;
- sanitizes the environment;
- enforces timeout, output, context, and configured RSS policy;
- extracts structured JSON responses;
- validates claim citations, certainty, inference policy, and identifiers;
- writes only derived records.

Fake executables provide deterministic integration tests. Real guarded runs of
Gemma 4 E2B, Qwen3.5 2B/0.8B, Granite 4 H 1B, and Qwen3 Embedding 0.6B are
documented in [`MODEL_SELECTION.md`](MODEL_SELECTION.md). No generation model
passed the unchanged pair-level production gate, so no default is promoted.

## Security limitations

The normal scan does not execute project code or package-manager hooks. Remote
Git acquisition disables prompts and hooks. Normalized source is redacted by
default.

External Python and native-worker execution is disabled. On Linux, the
digest-pinned built-in Python adapter fails closed unless it can start as a
transient user-systemd service with a one-core/lowest-priority cgroup, hard
memory/swap/task limits, a cleared worker environment, network-I/O syscall
denial, a one-task ceiling that prevents child processes, and control-group-wide
cancellation. The current user-service path does not claim a mount/filesystem
namespace; this is deliberately a narrow built-in adapter guard, not a general
third-party plugin sandbox. On platforms without that Linux enforcement path
the Python adapter fails closed; the in-process Go and TypeScript analyzers
remain available.

The static `scratch` reference image has no Python or user-systemd manager and
cannot provide this boundary. Its container and Compose examples select
`--no-python` explicitly and continue to exercise the in-process analyzers. No
container path silently downgrades to an unsandboxed Python worker.

## Verification

The complete release verifier runs unit, integration, contract, determinism,
API, MCP, Git-acquisition, race, and benchmark checks and preserves logs in
the atomic `dist/evidence` generation. The deterministic package builder hashes
the exact raw validation/benchmark evidence into its receipt, retains it outside
the ZIP in the same `dist/release` generation, and includes a fresh
mixed-language demonstration atlas plus a complete-distribution SPDX SBOM.

## Production deltas

| Reference implementation | Commercial-production target |
|---|---|
| filesystem bundle/snapshot | transactional SQLite local runtime |
| portable in-memory lexical index | SQLite FTS5/BM25 query implementation for durable snapshots |
| journalled 16-stage DAG and verified invalidation cache | additional distributed scheduling |
| guarded built-in Python worker | general WASI or OS-sandboxed third-party workers |
| syntax plus SCIP compiler semantics | measured per-indexer accuracy corpus |
| compact static browser | chunked TypeScript application with pagination |
| single local dataset | multi-repository PostgreSQL/object-store service |
| local unauthenticated API | OIDC/RBAC/tenant-aware service API |
| fake integration tests plus rejected real-GGUF receipts | a model that passes every unchanged gate |
| source checksums plus binary/distribution SPDX SBOMs | signed releases, container SBOM, provenance, transparency records |
