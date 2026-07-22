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

## Framework behavior

- Markdown: heading tree, sections, internal/external links, fenced-code metadata;
- OpenAPI: JSON services, paths, operations, parameters, responses, schemas,
  security schemes, serialization relations, and unresolved `$ref` records;
- JSON Schema: schemas, properties, definitions, required/type/format metadata,
  and references;
- manifests: `package.json`, `go.mod`, requirements files, Docker stages,
  dependencies, scripts, CLI entry points, base images, and environment values;
- environment files: keys, defaults, required state, and secret likelihood;
- security: deterministic high-signal credential patterns and redacted exports.

YAML OpenAPI, deeper Docker/Compose/Kubernetes interpretation, SQL, protobuf,
GraphQL, Terraform, and CI workflows remain production work.

## Canonical and derived output

`bundle.json`, `coverage.json`, and `rkc.manifest.json` are the portable source
of truth for one generated output directory. Record-family JSONL is a canonical
export. Documentation, the static site, NotebookLM pack, search index, SARIF,
GraphML, Mermaid, and CSV are derived products.

Model packets and responses live under `derived/`. The synthesis command tests
that `bundle.json` is unchanged after derived output is written.

## Current storage

The current scan writes a deterministic filesystem dataset and can publish it to
an immutable filesystem snapshot store with a content-addressed object store.
The SQLite DDL is validated and comprehensive, but the scan has not yet been
refactored to write through the production `SnapshotWriter` transaction.

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

A fake executable is used in tests. A real-model benchmark is intentionally not
claimed because no GGUF file is bundled or measured in this release.

## Security limitations

The normal scan does not execute project code or package-manager hooks. Remote
Git acquisition disables prompts and hooks. Normalized source is redacted by
default.

The Python worker still executes as the invoking operating-system user. Plugin
manifests and lockfiles are enforced at selection and digest level, but no WASI
host or namespace/seccomp native-worker sandbox is present yet. Do not run
untrusted third-party workers.

## Verification

The complete release verifier runs unit, integration, contract, determinism,
API, MCP, Git-acquisition, race, and benchmark checks and preserves logs in
`dist/validation`. The deterministic package builder includes those logs and a
fresh mixed-language demonstration atlas.

## Production deltas

| Reference implementation | Commercial-production target |
|---|---|
| filesystem bundle/snapshot | transactional SQLite local runtime |
| in-memory lexical index | SQLite FTS5 query implementation |
| integrated sequential scan | journalled DAG stages and invalidation cache |
| trusted Python worker | WASI or OS-sandboxed native worker |
| syntax adapters | compiler/indexer semantic adapters |
| compact static browser | chunked TypeScript application with pagination |
| single local dataset | multi-repository PostgreSQL/object-store service |
| local unauthenticated API | OIDC/RBAC/tenant-aware service API |
| fake model executable tests | measured real-GGUF resource benchmark |
| source checksums | signed releases, SBOM, provenance, transparency records |
