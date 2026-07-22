# Engineering delivery backlog

This backlog translates the implementation plan into issue-sized work. IDs are
stable planning labels, not promises about tracker software, because apparently
humans require a second database to remember what their first database is for.

## Epic 0: contracts and conformance

### RKC-001 Canonical vocabulary registry

Deliver:

- versioned registry of core node, edge, evidence, and diagnostic kinds;
- namespaced extension rules;
- field ownership and merge precedence.

Accept:

- registry validates all reference fixture outputs;
- unknown unnamespaced kinds are rejected;
- documentation is generated from the registry.

### RKC-002 Canonical serialization and ID library

Deliver:

- canonical JSON encoding;
- base32 stable IDs;
- snapshot, artifact, node, edge, evidence, and document ID constructors;
- cross-platform path normalization.

Accept:

- golden vectors in Go, Python, and TypeScript produce identical IDs;
- volatile fields cannot enter deterministic digests;
- Unicode and Windows path fixtures pass.

### RKC-003 GraphPatch validator

Deliver:

- streaming decoder;
- schema validation;
- kind/attribute validation;
- source-range validation;
- output limits and diagnostics.

Accept:

- malformed, duplicate, dangling, and path-escape patches are rejected;
- valid patches stream without full in-memory buffering;
- fuzz target has no crash on corpus.

### RKC-004 Plugin conformance harness

Deliver:

- fixture runner;
- timeout, cancellation, memory, output, and deterministic replay tests;
- report format.

Accept:

- included Python adapter produces a signed conformance report;
- intentional capability violations fail.

### RKC-005 Contract compatibility CI

Deliver:

- JSON Schema checks;
- OpenAPI compatibility check;
- WIT and protobuf compatibility checks;
- migration fixture checks.

Accept:

- incompatible changes fail pull requests unless a major-version RFC is linked.

## Epic 1: production local core

### RKC-101 SQLite repository implementation

Deliver:

- migrations;
- transactional snapshot writer;
- read/query interface;
- FTS5 schema;
- integrity and recovery commands.

Accept:

- all graph invariants enforced;
- interrupted writes publish no partial snapshot;
- database passes integrity check after forced process termination tests.

### RKC-102 Content-addressed object store

Deliver:

- atomic put/get;
- digest verification;
- optional compression;
- garbage-collection mark and sweep;
- local encryption hook.

Accept:

- duplicate bytes deduplicate within policy scope;
- corrupt objects are detected;
- uncommitted temporary objects are reclaimed.

### RKC-103 Acquisition subsystem

Deliver:

- local directory;
- local Git worktree;
- remote Git clone/fetch with ref pinning;
- submodule and LFS policy;
- immutable view.

Accept:

- dirty working trees receive content digests;
- Git hooks are never run;
- URL and credential policy tests pass.

### RKC-104 Inventory and classification

Deliver:

- descriptor-safe traversal;
- symlink containment;
- explicit exclusion records;
- generated/vendor/minified classifiers;
- encoding and newline detection.

Accept:

- 100% fixture artifact accounting;
- archive, huge-file, unusual-name, and permission fixtures produce explicit diagnostics.

### RKC-105 Normalized text and source maps

Deliver:

- original bytes and normalized text;
- original-to-normalized offset mapping;
- structural chunk records;
- Markdown envelope exporter.

Accept:

- exact source slices map in both directions;
- no lossy decode unless configured;
- clean replay is deterministic.

### RKC-106 Pipeline scheduler and journal

Deliver:

- DAG planning;
- stage leases;
- cancellation;
- retries;
- stage journal;
- local resource admission.

Accept:

- crash recovery resumes or safely restarts;
- stages never exceed declared parallel memory budget in stress test.

### RKC-107 Cache and invalidation

Deliver:

- cache keys;
- stage ownership;
- patch tombstones;
- affected-neighborhood invalidation;
- cache inspect/prune commands.

Accept:

- incremental result equals clean result for every change fixture;
- plugin/config/toolchain changes invalidate only dependent outputs.

## Epic 2: universal parsing spine

### RKC-201 Tree-sitter host

Deliver:

- grammar registry;
- parser pooling;
- cancellation and limits;
- query packages;
- parse diagnostics.

Accept:

- malformed files do not crash;
- parser memory is bounded;
- grammar identity enters cache key.

### RKC-202 Project and manifest detection

Deliver support for:

- Python project metadata;
- npm/pnpm/Yarn workspaces;
- Go modules/workspaces;
- Cargo;
- Maven/Gradle;
- CMake/compile databases;
- MSBuild/NuGet.

Accept:

- monorepo fixture creates correct overlapping project nodes;
- dependency manifests link to external-dependency nodes.

### RKC-203 Python first-class adapter

Deliver:

- syntax extraction;
- precise definitions/references/types;
- imports/re-exports;
- public API rules;
- pytest links;
- FastAPI and Django initial packs.

Accept:

- published precision/recall report;
- argument and source-range fidelity at target threshold;
- dynamic limitations documented.

### RKC-204 TypeScript/JavaScript first-class adapter

Deliver:

- TypeScript compiler service or SCIP import;
- package/workspace resolution;
- overloads and types;
- Jest/Vitest links;
- Express/Fastify/Nest initial packs.

Accept:

- JS and TS mixed monorepo fixture;
- path aliases and re-exports resolve.

### RKC-205 Go first-class adapter

Deliver:

- `go/packages` and `go/types` integration;
- module/workspace graph;
- test and benchmark links;
- net/http and Cobra packs.

Accept:

- build tags and multiple packages handled by explicit contexts.

### RKC-206 C/C++ first-class adapter

Deliver:

- compile database ingestion;
- Clang worker;
- macros and conditional-context evidence;
- GoogleTest links.

Accept:

- output records compile-command identity;
- missing compile database degrades to syntax without false precision.

### RKC-207 SCIP importer

Deliver:

- document, symbol, occurrence, relationship, and diagnostic import;
- symbol identity mapping;
- index metadata provenance.

Accept:

- fixture index round-trips expected definitions and references;
- imported symbols merge with syntax nodes without duplication.

### RKC-208 Infrastructure and schema adapters

Deliver:

- OpenAPI, JSON Schema, Protobuf, GraphQL;
- Dockerfile/Compose;
- GitHub Actions;
- Terraform/HCL;
- SQL migrations.

Accept:

- interface and deployment nodes connect to code and projects where evidence exists.

## Epic 3: plugin platform

### RKC-301 WASM/WASI host

Deliver:

- wazero host;
- WIT bindings;
- preopened capability paths;
- fuel/epoch cancellation;
- memory/output limits.

Accept:

- plugin cannot read outside grants or open network;
- hostile guest fixtures are contained.

### RKC-302 Native worker protocol

Deliver:

- handshake and version negotiation;
- streamed patch frames;
- cancellation;
- health;
- one-time authentication token.

Accept:

- worker replacement and protocol mismatch diagnostics;
- stdout/stderr are bounded.

### RKC-303 Native worker sandbox

Deliver per-platform implementation and capability report.

Accept:

- no repository write by default;
- no network by default;
- process-tree kill;
- memory/CPU/disk limits;
- external security review before stable.

### RKC-304 Plugin package, signing, and registry client

Deliver:

- immutable bundles;
- signature verification;
- manifest inspection;
- capability grant UI/CLI;
- quarantine and rollback.

Accept:

- tampered package rejected;
- capability changes require renewed approval.

### RKC-305 Plugin SDKs

Deliver Go, Python, and TypeScript SDKs with GraphPatch builders and fixtures.

Accept:

- one example plugin in each SDK passes conformance.

## Epic 4: documentation and model

### RKC-401 Deterministic document renderer

Deliver repository, project, module, file, symbol, API, CLI, config, data,
test, build, diagnostics, and coverage templates.

Accept:

- every eligible public symbol receives a document record;
- links and evidence validate;
- no model required.

### RKC-402 Existing documentation linker

Deliver symbol mention resolution, path/link validation, signature drift, and
staleness checks.

Accept:

- fixture reports stale and broken docs without modifying originals.

### RKC-403 Evidence packet builder

Deliver task-specific bounded packets with token budgeting, source excerpts,
graph facts, and allowed evidence IDs.

Accept:

- no packet exceeds configured budget;
- secrets and disallowed source are excluded.

### RKC-404 Model provider abstraction

Deliver llama.cpp, Ollama, OpenAI-compatible, and fake test providers.

Accept:

- provider health, resource estimate, cancellation, and structured-output errors are normalized.

### RKC-405 Claim schema and validator

Deliver claim-level citations, certainty, unknowns, canonical-fact checks, and
repair/reject path.

Accept:

- known hallucination corpus produces zero published unsupported claims in strict mode.

### RKC-406 Strict 4 GB profile

Deliver memory admission, sequential embedding/model loading, context limits,
and measured profiles for benchmark candidates.

Accept:

- peak RSS remains below configured ceiling on published reference hardware;
- over-budget tasks fall back rather than crash or swap the machine into geological time.

### RKC-407 Model benchmark harness

Deliver task corpus, metrics, reproducible runner, and report generator.

Accept:

- default model decision has a public measured report by language and task.

## Epic 5: search, browser, and integration

### RKC-501 Lexical and exact search

Deliver FTS5 indexing, exact symbol/path/signature search, fuzzy names, filters,
and retrieval trace.

Accept:

- benchmark MRR/recall target met;
- index rebuild is deterministic.

### RKC-502 Optional embeddings

Deliver ONNX embedding provider, vector storage interface, model namespace, and
hybrid scoring.

Accept:

- vectors never mix across model definitions;
- strict profile can unload embedding model before generation.

### RKC-503 Static site v1

Deliver chunked data, local search, overview, explorer, symbol, diagnostics,
coverage, and graph neighborhood.

Accept:

- opens offline;
- initial payload budget met;
- accessibility baseline met.

### RKC-504 Daemon browser

Deliver paginated API use, job progress, snapshot diff, impact, source view, and
shared annotations where enabled.

Accept:

- medium benchmark repository remains responsive;
- graph queries obey limits.

### RKC-505 Read API

Implement OpenAPI read endpoints, cursor pagination, problem responses, ETags,
and SSE job events.

Accept:

- generated Go, TypeScript, and Python clients pass contract tests.

### RKC-506 MCP server

Implement resources, tools, and prompts over canonical query services.

Accept:

- MCP and REST return identical node/evidence facts;
- source permissions are enforced.

### RKC-507 NotebookLM exporter

Deliver configurable profiles, semantic packing, manifest, checksums, and limit
validation.

Accept:

- no broken stable-ID links;
- pack sizes and source counts obey selected profile.

### RKC-508 SARIF and graph exports

Deliver SARIF, GraphML, DOT, Mermaid, CSV, SPDX, and CycloneDX adapters.

Accept:

- schema validators pass;
- large graph exports require explicit filters or stream safely.

## Epic 6: diff, CI, and release

### RKC-601 Logical identity and rename tracking

Deliver compiler-ID mapping, Git rename evidence, semantic fingerprints, and
reviewable match confidence.

Accept:

- fixture history preserves identity without false merges at target threshold.

### RKC-602 Semantic diff

Deliver artifact, symbol, edge, interface, documentation, config, schema, and
migration change classes.

Accept:

- breaking-change fixtures classify expected changes with evidence.

### RKC-603 Impact analysis

Deliver bounded reverse traversal, path explanations, ranking, and tests/public
surface targeting.

Accept:

- every listed impact includes an explicit graph path.

### RKC-604 CI integrations

Deliver GitHub Actions, GitLab CI, generic container, cache, SARIF, and artifact
examples.

Accept:

- deterministic and public-API gate examples pass on fixture repos.

### RKC-605 Release supply chain

Deliver multi-platform builds, signed artifacts, checksums, SPDX SBOM, SLSA
provenance, and release verification command.

Accept:

- independent verification job validates every published artifact.

## Epic 7: server and enterprise operation

### RKC-701 PostgreSQL storage adapter

Accept parity with SQLite conformance suite and transactional publication.

### RKC-702 Remote object store

Accept digest verification, tenant scope, encryption metadata, lifecycle, and
failure recovery.

### RKC-703 Job queue and worker pool

Accept leases, retries, drain, cancellation, priority, and capability routing.

### RKC-704 OIDC, RBAC, and service accounts

Accept complete authorization matrix and negative cross-workspace tests.

### RKC-705 Audit and retention

Accept immutable audit events, export, deletion workflow, legal hold, and source/model/log retention separation.

### RKC-706 Observability

Accept OpenTelemetry traces, OTLP export, privacy-safe metrics/logs, and dashboards.

### RKC-707 Backup, restore, and migration

Accept automated restore test and supported-version migration matrix.

### RKC-708 Multi-tenant isolation review

Accept external review of data, cache, worker, network, object, and authorization boundaries before managed-service launch.

## Release gates

### Alpha

- M1 vertical slice in production storage;
- explicit security warning;
- no compatibility promise.

### Beta

- M2 browser/search;
- M3 plugin platform and four semantic language paths;
- migrations and signed artifacts;
- documented known limitations.

### Stable 1.0

- production acceptance criteria in the implementation plan;
- security review;
- backup/restore and upgrade exercises;
- published accuracy and performance benchmarks;
- stable schema/plugin/API commitments.

---

# Second-pass status and production sequence

The following items are implemented in the `0.3.0-reference` tree at reference
quality:

- public canonical model, vocabulary, sorting, validation, and coverage;
- GraphPatch validation/application;
- filesystem snapshot and content-addressed stores;
- deterministic scheduler and file cache libraries;
- local and remote Git acquisition;
- Python, Go, and JavaScript/TypeScript syntax extraction;
- Markdown, OpenAPI JSON, JSON Schema, manifests, environment, and secret packs;
- ranked lexical search and graph operations;
- semantic diff;
- deterministic docs, normalized/redacted source, NotebookLM, browser, and
  integration exports;
- read-only HTTP and MCP;
- bounded model packets, `llama.cpp` provider, and claim validation;
- plugin manifests/locks;
- contract, determinism, API/MCP/Git/race/benchmark release checks.

These are useful implementations, not closure of the corresponding production
epics where the original acceptance criteria require transactional SQLite,
enforced sandboxes, compiler precision, measured benchmarks, or service
operations.

## Critical path backlog

### RKC-900 SQLite canonical runtime conversion

Depends on: RKC-101, RKC-102.

Deliver:

- public `SnapshotReader` and `SnapshotWriter` interfaces;
- migration framework;
- transactional build lifecycle;
- paginated readers;
- bundle import/export;
- FTS5 projection;
- crash recovery and database commands;
- refactor of CLI, HTTP, MCP, search, graph, diff, synthesis, and exporters to
  use the store.

Accept:

- killed writes publish no partial snapshot;
- bundle export/import preserves canonical digest;
- all current smoke tests run against SQLite;
- database integrity and migration fixtures pass.

### RKC-901 Pipeline stage conversion and incremental equivalence

Depends on: RKC-900.

Deliver:

- acquisition, inventory, analyzers, merge, resolution, validation, coverage,
  and exports as journalled DAG stages;
- stage resource admission;
- persisted cache and ownership;
- invalidation graph and tombstones;
- parent-snapshot incremental planning;
- history fixture corpus.

Accept:

- incremental digest equals clean digest after every fixture commit;
- cancellation terminates workers;
- warm localized scans show measured improvement.

### RKC-902 Enforcing plugin runtimes

Depends on: public stage/store contracts.

Deliver:

- WASI component host;
- Linux native worker launcher and platform-equivalent profiles;
- streamed GraphPatch protocol;
- signature/provenance policy;
- malicious plugin conformance suite.

Accept:

- undeclared filesystem, network, process, environment, memory, and output access
  fails;
- current Python adapter runs through the native launcher;
- third-party direct execution is disabled by default.

### RKC-903 Compiler semantic adapters

Depends on: RKC-902 and Tree-sitter host.

Deliver in order:

1. Python import/type/reference adapter;
2. TypeScript compiler adapter;
3. Go packages/types/SSA adapter;
4. Clang compilation-database adapter;
5. Rust analyzer adapter;
6. JVM adapter;
7. Roslyn adapter.

Accept:

- each publishes precision/recall, contexts, performance, unresolved causes, and
  limitations;
- no project hook or package lifecycle script runs in default mode;
- incremental output equals clean output.

### RKC-904 Framework and system packs

Deliver:

- OpenAPI YAML, GraphQL, protobuf/gRPC, HTTP frameworks;
- CLI frameworks;
- configuration lineage;
- SQL/schema/ORM lineage;
- messaging/events;
- Docker/Compose/Kubernetes/Terraform/CI/build systems;
- authorized runtime observation protocol.

Accept:

- every supported public surface has handler/resolver/source evidence or an
  explicit unresolved diagnostic;
- route/schema/config conflicts remain queryable.

### RKC-905 Production browser and APIs

Depends on: RKC-900.

Deliver:

- FTS5 search and retrieval trace;
- opaque cursor pagination;
- TypeScript browser;
- sharded static export;
- generated TypeScript/Python clients;
- editor proof of concept.

Accept:

- medium/large snapshots do not require whole-bundle loading;
- p95 query and browser transfer targets pass.

### RKC-906 Real model benchmark and documentation freshness

Deliver:

- approved model profile registry;
- real GGUF resource benchmark;
- evidence-scored task corpus;
- model cache and invalidation;
- claim freshness/contradiction engine;
- optional remote-provider egress policy.

Accept:

- published peak RSS and quality measurements are reproducible;
- no accepted benchmark claim lacks valid evidence;
- deterministic documentation remains complete with model disabled.

### RKC-907 Team service

Depends on: RKC-900, RKC-902, RKC-905.

Deliver:

- PostgreSQL store conformance;
- S3-compatible objects;
- durable jobs and worker leases;
- OIDC, RBAC, tenancy, quotas, and audit;
- backups, restore, migrations, and rolling upgrades;
- OpenTelemetry and operational dashboards.

Accept:

- tenant isolation and authorization suites pass;
- backup/restore reproduces canonical digests;
- queue recovery and cancellation are idempotent;
- degraded mode is documented and tested.

### RKC-908 Supply chain and general availability

Deliver:

- multi-platform signed release;
- SBOM and provenance;
- official plugin signatures;
- benchmark corpus and dashboard;
- adversarial security suite;
- release acceptance rehearsal.

Accept:

- clean systems verify every distributed artifact before execution;
- critical security, accuracy, reliability, and operability gates pass;
- known limitations are published;
- model-disabled operation remains first-class.

The code-level sequence, interfaces, SQL, tests, and exit gates are expanded in
`REMAINDER_IMPLEMENTATION_PLAN.md`.
