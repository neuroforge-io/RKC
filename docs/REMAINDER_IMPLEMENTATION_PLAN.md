# Remainder implementation plan

## Purpose

This document is the ordered engineering plan from the runnable
`0.3.0-reference` release to a defensible commercial-production `1.0`. It does
not repeat the full product vision in `implementation-plan.md`; it converts the
remaining gaps into code changes, interfaces, migrations, tests, rollout steps,
and objective exit gates.

The implementation order is deliberate. Storage and stage ownership precede
more analyzers. Sandboxing precedes third-party plugins. Deterministic
retrieval precedes model polish. Service mode follows local correctness. This is
less glamorous than starting with an animated graph and a chat bubble, which is
precisely why it has a chance of working.

## Production definition

Version 1.0 is reached only when all of the following are true:

1. local scans publish transactionally into SQLite and recover from interruption;
2. incremental scans are byte-equivalent to clean scans over the conformance
   history corpus;
3. plugin capabilities are enforced, not merely declared;
4. at least Python, TypeScript/JavaScript, and Go have compiler-grade semantic
   adapters with published precision/recall and limitations;
5. C/C++, Rust, Java/Kotlin, and C# have supported production adapters or
   explicitly scoped beta status;
6. public interfaces, configuration, build, tests, data schemas, and deployment
   artifacts are indexed by maintained framework packs;
7. every published generated claim cites validated evidence;
8. the browser and APIs operate over bounded/paginated reads, not whole-bundle
   loading;
9. signed releases include checksums, SBOM, provenance, and clean-install tests;
10. team mode passes authentication, tenant isolation, backup/restore, migration,
    queue, and operational acceptance tests;
11. a real local GGUF profile has a published reproducible memory/quality
    benchmark, or the under-4-GiB claim is removed;
12. measured release gates pass on the maintained small, medium, and large
    repository corpus.

## Dependency graph

```text
P0 baseline freeze
  -> P1 canonical SQLite runtime
      -> P2 staged pipeline and incremental cache
          -> P3 enforced plugin runtimes
              -> P4 universal syntax host
                  -> P5 compiler semantic adapters
                      -> P6 framework and runtime packs
  -> P7 search/browser/API pagination
  -> P8 model benchmark and synthesis quality
  -> P9 CI semantic diff and policy
  -> P10 team service
  -> P11 supply chain and release
  -> P12 benchmark, hardening, and 1.0 acceptance
```

P7 can proceed after P1. P8 can proceed from the current packet interface but
must be revalidated after P1/P2. P10 must not begin public rollout before P3 and
P11 security controls exist.

## Progress ledger

This ledger records completed slices without deleting or weakening any exit
gate below.

| Date | Work item | Evidence | Remaining boundary |
|---|---|---|---|
| 2026-07-22 | P0 signed reference identity and compatibility floor | Signed `v0.3.0-reference` tag; executable canonical bundle, SHA-256, canonical-digest, stable-ID, exact CI/CodeQL, and self-catalogue artifact fixtures | Archive/sign the complete release payload and logs and add the maintained cross-platform/toolchain fixture matrix |
| 2026-07-22 | `STORE-001` and `STORE-002` | Public typed store interfaces plus concurrency-safe in-memory conformance backend; atomic current publication, stale-writer conflict, abort/recovery, strict validation, exact coverage binding, authenticated cursors, defensive clones, and lossless bundle export | Durable conformance and bounded pages completed by the 2026-07-23 SQLite runtime row; retain the memory backend as the fast contract oracle |
| 2026-07-22 | `STORE-003` | Ordered SQLite migrations with pinned file and manifest SHA-256 values; offline validation proves execution, integrity, foreign keys, version order, and consolidated-catalogue equivalence | Durable schema/runtime work completed by the 2026-07-23 SQLite runtime row; future migrations remain append-only and digest-governed |
| 2026-07-22 | P1 CGO-free SQLite bootstrap and dependency closure | Pinned pure-Go SQLite module graph and hashes, reviewed module/license lock, embedded immutable migrations, strict open/health checks, CGO-disabled Docker/CI/release builds, module-cache verification, deterministic SPDX 2.3 SBOMs exactly rebound to every release binary, a manifest-bound complete-distribution SPDX SBOM, GOOS/GOARCH and normalized architecture tuning, default Go experiments, `GOFIPS140=off`, immutable source commit/tree/time, toolchain, canonical Go purls, and linked-module inventory, plus guarded cache-isolated independent-checkout byte-reproducible complete-package assembly with atomic evidence/release generation publication and exact validation/benchmark digest linkage | Local SQLite conformance completed by the 2026-07-23 row; remaining release work is signed publication, container SBOMs, and verified provenance |
| 2026-07-23 | `STORE-004` through `STORE-008` durable SQLite runtime | Digest-verified canonical reader/writer, transactional staging and CAS publication, OS-backed writer leases, recovery, authenticated pagination, projections, read-only consumers, and scan/query/answer/graph/snapshot/browser/synthesis/MCP integration | Keep the portable atlas export; next storage work is team-mode PostgreSQL/object storage rather than another local SQLite rewrite |

---

# Phase P0: freeze the reference baseline

## Goal

Create a reproducible baseline so later improvements cannot quietly change
canonical meaning, IDs, or coverage formulas.

## Deliverables

- tag `v0.3.0-reference`;
- archive of the complete release verification logs;
- canonical fixture bundles and digests;
- schema compatibility fixtures;
- current plugin lockfile;
- benchmark hardware/software descriptor;
- architecture decision register through ADR 0009.

## Steps

1. Run `make release-verify` in a clean Linux amd64 environment.
2. Generate the mixed-language demo and self-analysis benchmark.
3. Copy `bundle.json`, `coverage.json`, `rkc.manifest.json`, search index, and
   integration exports into `fixtures/golden/reference-0.3/`.
4. Add a fixture manifest:

```json
{
  "schema_version": "1.0",
  "rkc_version": "0.3.0-reference",
  "canonical_schema": "0.2.0",
  "source_commit": "<commit>",
  "files": [
    {"path": "bundle.json", "sha256": "..."},
    {"path": "coverage.json", "sha256": "..."}
  ]
}
```

5. Add cross-platform stable-ID vectors for Unicode, Windows paths, path case,
   overload signatures, anonymous functions, generated artifacts, and dirty Git
   trees.
6. Freeze current vocabulary entries in a generated reference document.
7. Add a compatibility test that reads every prior canonical fixture.
8. Sign the baseline source archive and checksum manifest in the release
   environment.

## Tests

- clean checkout reproduces fixture canonical digest;
- Go 1.23 on Linux amd64 and arm64 produces the same canonical bundle;
- Python 3.11–3.13 AST worker outputs equivalent records for supported syntax;
- every schema validates its examples;
- every prior bundle is readable or has a documented migration.

## Exit gate

No later phase begins merging canonical schema changes until baseline fixtures
and compatibility tests exist.

---

# Phase P1: make SQLite the canonical local runtime

## Goal

Replace direct filesystem-bundle coupling with a transactional storage
abstraction. JSON/JSONL remains an export, not the database impersonating a
runtime architecture.

## Target package layout

```text
pkg/rkcstore/
├── reader.go
├── writer.go
├── query.go
├── errors.go
└── conformance.go

internal/storage/sqlite/
├── database.go
├── migrations.go
├── writer.go
├── reader.go
├── query.go
├── fts.go
├── recovery.go
└── sqlite_test.go

storage/sqlite/migrations/
├── 0001_initial.sql
├── 0002_claims_conflicts_paths.sql
└── manifest.json
```

## Public interfaces

```go
package rkcstore

type BuildID string
type SnapshotID string

type BuildOptions struct {
    RepositoryID     string
    ParentSnapshotID string
    ExpectedSchema   string
    Metadata         map[string]string
}

type SnapshotWriter interface {
    BeginBuild(ctx context.Context, opts BuildOptions) (BuildID, error)
    PutArtifacts(ctx context.Context, build BuildID, values []rkcmodel.Artifact) error
    PutNodes(ctx context.Context, build BuildID, values []rkcmodel.Node) error
    PutEdges(ctx context.Context, build BuildID, values []rkcmodel.Edge) error
    PutEvidence(ctx context.Context, build BuildID, values []rkcmodel.Evidence) error
    PutDiagnostics(ctx context.Context, build BuildID, values []rkcmodel.Diagnostic) error
    PutConflicts(ctx context.Context, build BuildID, values []rkcmodel.Conflict) error
    PutDocuments(ctx context.Context, build BuildID, values []rkcmodel.Document) error
    PutClaims(ctx context.Context, build BuildID, values []rkcmodel.Claim) error
    PutPaths(ctx context.Context, build BuildID, values []rkcmodel.ExecutionPath) error
    PutCoverage(ctx context.Context, build BuildID, coverage rkcmodel.Coverage) error
    Validate(ctx context.Context, build BuildID) (ValidationResult, error)
    Commit(ctx context.Context, build BuildID, snapshot rkcmodel.Snapshot) error
    Abort(ctx context.Context, build BuildID, reason error) error
}

type SnapshotReader interface {
    Snapshot(ctx context.Context, id SnapshotID) (rkcmodel.Snapshot, error)
    Current(ctx context.Context, repositoryID string) (rkcmodel.Snapshot, error)
    ListSnapshots(ctx context.Context, query SnapshotQuery) (SnapshotPage, error)
    Artifact(ctx context.Context, snapshotID, artifactID string) (rkcmodel.Artifact, error)
    Node(ctx context.Context, snapshotID, nodeID string) (rkcmodel.Node, error)
    Evidence(ctx context.Context, snapshotID, evidenceID string) (rkcmodel.Evidence, error)
    QueryNodes(ctx context.Context, query NodeQuery) (NodePage, error)
    QueryEdges(ctx context.Context, query EdgeQuery) (EdgePage, error)
    QueryDiagnostics(ctx context.Context, query DiagnosticQuery) (DiagnosticPage, error)
    Coverage(ctx context.Context, snapshotID string) (rkcmodel.Coverage, error)
}
```

All pages use opaque cursors. Offset pagination is permitted only for local
administrative screens, not stable APIs.

## Transaction model

1. `BeginBuild` inserts a build record and opens an isolated staging namespace.
2. Record batches are validated structurally before insertion.
3. Foreign-key checks may be deferred until the validation transaction.
4. Validation checks IDs, endpoints, evidence, source artifacts, vocabulary,
   canonical counts, and coverage.
5. `Commit` inserts/updates the immutable snapshot, marks the build committed,
   and atomically moves the repository’s `current_snapshot_id`.
6. FTS and derived projections are either updated in the same transaction or
   explicitly versioned and marked rebuilding.
7. An interrupted build remains invisible and can be aborted by recovery.

## SQLite connection policy

```go
type SQLiteOptions struct {
    Path            string
    BusyTimeout     time.Duration
    ReadConnections int
    Synchronous     string // FULL for high assurance, NORMAL default
    MMapBytes       int64
    CacheKiB        int
}
```

Initialize every connection with:

```sql
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA trusted_schema = OFF;
PRAGMA temp_store = MEMORY;
```

Set WAL mode once at database creation. Do not execute arbitrary repository SQL
against the canonical database.

## Migration framework

Migration manifest:

```json
{
  "schema_version": "1.0",
  "migrations": [
    {
      "version": 1,
      "name": "initial",
      "sha256": "...",
      "minimum_rkc": "0.4.0"
    }
  ]
}
```

Migration algorithm:

```go
func Migrate(ctx context.Context, db *sql.DB, target int) error {
    lockDatabaseMigration(db)
    current := readSchemaVersion(db)
    for _, migration := range planned(current, target) {
        verifyEmbeddedDigest(migration)
        tx := beginImmediate(db)
        executeStatements(tx, migration.SQL)
        recordMigration(tx, migration)
        runMigrationAssertions(tx, migration)
        tx.Commit()
    }
    return integrityCheck(db)
}
```

No migration edits after release. Corrections are new migrations.

## Object store integration

Store source/normalized/export blobs through:

```go
type ObjectStore interface {
    Put(ctx context.Context, reader io.Reader, expectedDigest string) (ObjectRef, error)
    Open(ctx context.Context, digest string) (io.ReadCloser, error)
    Stat(ctx context.Context, digest string) (ObjectInfo, error)
    Delete(ctx context.Context, digest string) error
}
```

The database stores object digests and sizes. Object upload is staged before
snapshot commit. Failed builds’ unreferenced objects are reclaimed after a grace
period.

## Refactor sequence

1. Introduce `pkg/rkcstore` interfaces with an in-memory conformance store.
2. Add SQLite migrations and schema-version table.
3. Implement write batches for each record family.
4. Implement read/query pages and cursors.
5. Add canonical bundle import/export through `SnapshotReader`/`Writer`.
6. Refactor server, MCP, graph, search, and exporters to accept a
   `SnapshotReader` instead of reading `bundle.json` directly.
7. Refactor `rkc scan` to write a build transaction and export from the
   committed snapshot.
8. Retain `bundle.json` for portability.
9. Add `rkc db migrate`, `rkc db check`, `rkc db export`, and `rkc db vacuum`.
10. Deprecate direct dataset loading after one compatibility release.

## Tests

- conformance suite runs against memory and SQLite stores;
- process kill after each write stage publishes no partial snapshot;
- foreign-key, duplicate-ID, dangling-edge, missing-evidence, and bad-range
  fixtures fail validation;
- concurrent readers observe only committed snapshots;
- two writers serialize or receive a clear conflict;
- database corruption produces a hard error, not an empty repository;
- export-import-export produces the same canonical digest;
- migration fixtures upgrade from every supported schema version;
- FTS results correspond to the committed snapshot only;
- backup/restore reproduces snapshot and object digests.

## Exit gate

All CLI, HTTP, MCP, export, search, graph, diff, and synthesis reads use
`SnapshotReader`. A scan killed at any tested failure point cannot publish a
partial snapshot.

---

# Phase P2: stage the pipeline and implement incremental equivalence

## Goal

Convert the integrated scan into a journalled DAG whose cache and invalidation
behavior is testable and whose incremental output is identical to a clean scan.

## Stage interface

```go
type Stage interface {
    ID() string
    Version() string
    Dependencies() []string
    Resources(input StageInput) ResourceRequest
    CacheKey(input StageInput) (string, error)
    Run(ctx context.Context, input StageInput, sink PatchSink) (StageResult, error)
}

type ResourceRequest struct {
    MemoryMiB int64
    CPU       int
    Processes int
    OpenFiles int
    IOClass   string
}

type StageResult struct {
    OutputDigest string
    Statistics   map[string]int64
    Diagnostics  []rkcmodel.Diagnostic
    Cacheable    bool
}
```

## Initial DAG

```text
acquire
  -> inventory
      -> normalize
      -> classify-projects
          -> python-syntax
          -> go-syntax
          -> typescript-syntax
          -> markdown
          -> openapi
          -> json-schema
          -> manifests
          -> env-keys
          -> secret-scan
              -> merge
                  -> resolve
                      -> validate
                          -> coverage
                              -> publish
                                  -> search-index
                                  -> docs
                                  -> notebooklm
                                  -> static-site
                                  -> integrations
```

Derived stages may fail independently after canonical publication and be retried
without rebuilding source truth.

## Cache key

```text
SHA-256(
  stage ID + stage version
  canonical input object digests
  relevant configuration projection
  plugin artifact/version
  toolchain descriptor
  policy digest
  canonical schema version
)
```

Each stage declares exactly which configuration fields enter its key. A browser
page-size setting must not invalidate Python parsing. A Python interpreter
upgrade must.

## Invalidation graph

Track ownership:

```text
stage run -> artifacts read
stage run -> records produced
record -> source evidence
record -> downstream records/documents/index entries
```

For a changed artifact:

1. invalidate its normalization and syntax outputs;
2. invalidate semantic records whose source or dependency context changed;
3. invalidate relationship resolution involving removed/changed symbols;
4. invalidate graph-derived summaries within bounded affected components;
5. invalidate documentation and search entries for affected subjects;
6. retain unrelated package output.

Manifest/build changes may invalidate an entire project context. Plugin,
toolchain, policy, or schema changes invalidate their dependent stages.

## Journal schema

```sql
CREATE TABLE stage_runs (
  run_id TEXT PRIMARY KEY,
  build_id TEXT NOT NULL,
  stage_id TEXT NOT NULL,
  stage_version TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  state TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  started_at TEXT,
  finished_at TEXT,
  output_digest TEXT,
  error_json TEXT,
  resource_json TEXT NOT NULL
);
```

States are `planned`, `queued`, `running`, `cached`, `succeeded`, `failed`, and
`cancelled`.

## Implementation steps

1. Wrap existing inventory and each analyzer in `Stage` implementations.
2. Add a `PatchSink` that writes staged records through `SnapshotWriter`.
3. Persist the scheduler journal and stage output ownership.
4. Replace direct goroutines with resource-admitted scheduling.
5. Implement cancellation propagation to Python, native, and WASI workers.
6. Implement file-backed cache compatibility, then migrate cache metadata to
   SQLite and blobs to CAS.
7. Add tombstones for removed stage-owned records.
8. Add `rkc plan` to print the DAG, cache hits, misses, and invalidation causes.
9. Add `rkc cache inspect`, `prune`, and `verify`.
10. Add incremental scan mode against a parent snapshot.
11. Run the history corpus both incrementally and clean after every commit.
12. Fail CI on digest divergence.

## History corpus

Each language fixture repository should contain commits for:

- comment-only edit;
- function-body edit;
- signature edit;
- function move;
- file rename;
- package rename;
- manifest dependency change;
- build-flag change;
- generated-code change;
- deleted symbol;
- route/config/schema change;
- plugin/toolchain version change.

## Exit gate

For every supported history fixture, each incremental snapshot’s canonical
digest equals a clean scan of the same commit. The benchmark demonstrates a
material warm-scan improvement without reducing coverage.

---

# Phase P3: enforce plugin capabilities

## Goal

Make plugin policy a runtime property, not well-formatted JSON.

## Host interface

```go
type Runner interface {
    Inspect(ctx context.Context, installed InstalledPlugin) (Descriptor, error)
    Run(ctx context.Context, installed InstalledPlugin, request pluginapi.Request,
        sink graphpatch.StreamSink) (RunResult, error)
}
```

The registry selects a verified artifact. The runner enforces the manifest and
host policy. The patch validator remains the final trust boundary.

## WASI host

Recommended Go implementation: a maintained WASI component runtime with no
ambient capabilities.

Steps:

1. load and hash the component;
2. verify lockfile and signature policy;
3. create a fresh runtime instance per run or proven-safe pool;
4. preopen only approved read-only descriptors;
5. provide a bounded plugin-private temporary directory if granted;
6. expose no environment except approved keys;
7. expose no network by default;
8. configure memory/fuel/epoch interruption;
9. stream requests and patches through the WIT interface;
10. enforce cumulative output limits;
11. cancel on context deadline;
12. destroy the instance and temporary files;
13. record run descriptor, peak resources, and output digest.

## Native worker launcher

Define a small launcher executable:

```text
rkc-worker-launcher
  --manifest <path>
  --repository <read-only-materialization>
  --scratch <private-tmpfs>
  --request-fd <n>
  --response-fd <n>
  --timeout <duration>
  --memory <bytes>
  --pids <count>
```

Linux implementation:

- unshare user, mount, PID, IPC, UTS, and network namespaces;
- map an unprivileged UID/GID;
- read-only bind mount repository;
- mount private tmpfs scratch;
- mount minimal `/proc` inside PID namespace if required;
- set `PR_SET_NO_NEW_PRIVS`;
- apply seccomp allowlist;
- join cgroup v2 limits;
- set RLIMIT_AS, CPU, NOFILE, NPROC, FSIZE, CORE;
- close inherited file descriptors;
- clear environment;
- execute by verified absolute path;
- capture bounded protocol output and logs.

macOS and Windows require equivalent containment profiles and must publish any
weaker guarantees.

## Protocol hardening

- length-prefix every message;
- maximum request/record/batch/patch sizes;
- streaming JSON or protobuf decoder with unknown-field policy;
- monotonic sequence numbers;
- explicit end-of-patch digest;
- heartbeat for long compiler tasks;
- host cancellation message;
- no repository-supplied method names or executable paths;
- all source paths resolved from host artifact IDs.

## Supply-chain policy

Installed plugin record:

```go
type InstalledPlugin struct {
    ManifestDigest string
    ArtifactDigest string
    Signature      SignatureResult
    Source         string
    InstalledAt    time.Time
    TrustLevel     string
}
```

Policies can require official publisher identity, transparency entry, license
allowlist, vulnerability scan, and reproducible build provenance.

## Tests

- filesystem read outside grant fails;
- symlink escape fails;
- undeclared network fails;
- environment and process spawning fail;
- fork bomb and memory bomb are terminated;
- stdout/stderr and GraphPatch limits are enforced;
- timeout/cancellation terminates descendants;
- malformed protocol cannot crash host;
- valid patch from sandbox equals trusted fixture output;
- signed/locked artifact mismatch fails before execution.

## Exit gate

No third-party plugin can run outside an enforcing WASI or native-worker runner.
The trusted direct-process compatibility path is opt-in and visibly unsafe.

---

# Phase P4: universal Tree-sitter syntax host

## Goal

Replace bespoke lexical fallbacks with a broad, versioned, query-driven syntax
substrate while retaining language-specific adapters where the standard
compiler AST is stronger.

## Grammar registry

```go
type GrammarDescriptor struct {
    LanguageID      string
    Version         string
    ArtifactDigest  string
    ABI              int
    Extensions       []string
    Shebangs         []string
    QueryPackDigest  string
    License          string
}
```

Pin grammar source and compiled artifact digests. Grammar version is part of the
stage cache key and evidence toolchain.

## Query pack layout

```text
languages/tree-sitter/<language>/
├── grammar.lock.json
├── declarations.scm
├── imports.scm
├── references.scm
├── calls.scm
├── comments.scm
├── tests.scm
├── normalizer.go
├── fixtures/
└── limitations.md
```

Queries produce neutral captures; language normalizers convert captures into RKR
records and source ranges.

## Parse service

```go
type SyntaxService interface {
    Parse(ctx context.Context, language string, source []byte) (Tree, []Diagnostic, error)
    Query(ctx context.Context, tree Tree, querySet QuerySet) (CaptureStream, error)
}
```

Pool parsers by grammar, bound total tree memory, and release trees promptly.
Malformed input yields recovery diagnostics and partial declarations.

## Initial language sequence

1. JavaScript/TypeScript/TSX/JSX;
2. Python, compared against standard AST;
3. Go, compared against Go AST;
4. C/C++ syntax baseline;
5. Java/Kotlin;
6. Rust;
7. C#;
8. Ruby, PHP, Swift, Dart, Lua, shell, PowerShell, R, Julia;
9. HCL, SQL dialects, Dockerfile, CMake, Make, Nix, YAML, TOML.

Native AST adapters remain preferred when they produce stronger facts. The
Tree-sitter host supplies a universal floor and malformed-source behavior.

## Differential tests

For Python and Go, compare Tree-sitter declaration/source ranges with native AST
output. For TypeScript, compare against the compiler adapter when available.
Disagreements become adapter diagnostics and benchmark data.

## Exit gate

Every supported text language is either parsed by a pinned syntax adapter or
explicitly reported as unsupported. A malformed file cannot crash the scan or
vanish from coverage.

---

# Phase P5: compiler-grade semantic adapters

## Common semantic adapter contract

```go
type SemanticContext struct {
    SnapshotID       string
    Project          rkcmodel.Node
    Root              string
    ArtifactIDs       []string
    BuildDescriptor   BuildDescriptor
    Toolchain         ToolchainDescriptor
    PreviousIndex     *ObjectRef
}

type SemanticAdapter interface {
    Detect(ctx context.Context, project ProjectCandidate) (Detection, error)
    Prepare(ctx context.Context, project ProjectCandidate) (BuildDescriptor, error)
    Index(ctx context.Context, semantic SemanticContext, sink graphpatch.StreamSink) error
    Capabilities() SemanticCapabilities
}
```

Every adapter must report whether it resolves definitions, references, types,
inheritance, calls, generics, generated code, build variants, and diagnostics.

## P5A Python

### Preferred stack

- standard AST for deterministic declarations;
- import graph aware of packages, namespace packages, editable roots, and
  `pyproject.toml` configuration;
- Pyright or another licensed semantic engine for types/references;
- optional runtime import/coverage observer only under explicit authorization.

### Steps

1. detect project roots and interpreter constraints from `pyproject.toml`,
   setup metadata, requirements, and workspace configuration;
2. model import search paths without importing project modules;
3. resolve relative and absolute imports;
4. import semantic index output into stable symbol IDs;
5. distinguish annotations, inferred types, and `Any`;
6. model overloads, protocols, decorators, dataclasses, properties, and async;
7. map framework decorators in separate packs;
8. retain unresolved dynamic imports and monkey patching diagnostics;
9. benchmark against typed and dynamic repositories.

### Exit gate

Published precision/recall for definitions/references exceeds agreed thresholds;
all unresolved imports enter coverage; adapter runs without executing project
imports.

## P5B TypeScript and JavaScript

### Stack

Use the TypeScript compiler API in a native worker. Discover `tsconfig` project
references, workspaces, path aliases, JSX settings, declaration files, and
JavaScript check modes.

### Steps

1. enumerate effective projects without running npm scripts;
2. load compiler with package-manager execution disabled;
3. create programs per project reference graph;
4. emit definitions, references, symbols, overloads, inferred types, exports,
   inheritance, interface implementation, and diagnostics;
5. map source/declaration/generated files;
6. resolve CommonJS/ESM interop and conditional exports;
7. model monorepository package boundaries;
8. add framework packs after semantic symbols exist;
9. retain dynamic `require`, `eval`, proxy, and reflection limitations.

### Exit gate

Compiler symbol identity resolves imports and references across fixture
workspaces, incremental index equals clean index, and no package lifecycle script
runs.

## P5C Go

### Stack

Use `go/packages`, `go/types`, and SSA/callgraph packages in an isolated worker.

### Steps

1. discover modules and `go.work`;
2. enumerate target GOOS/GOARCH/build-tag contexts;
3. load syntax/types without running arbitrary generators;
4. emit packages, declarations, interfaces, method sets, instantiations,
   definitions, references, and type relations;
5. add conservative static call graph with method/interface dispatch metadata;
6. model cgo and generated code explicitly;
7. support module replacements and vendor policy;
8. benchmark variants and large workspaces.

### Exit gate

Definitions/references and interface implementation pass golden fixtures for all
supported build contexts; context-specific facts are not merged without labels.

## P5D C, C++, and CUDA

Use Clang tooling with a verified compilation database.

Required work:

- detect/import `compile_commands.json` or generate it only in an approved build
  worker;
- store compile command/context digest per translation unit;
- model macros, includes, declarations, templates, overloads, inheritance,
  references, and calls;
- distinguish headers across multiple compile contexts;
- retain conditional-compilation alternatives;
- index CUDA host/device contexts;
- sandbox compiler plugins and include paths;
- publish unresolved/missing-command coverage.

A source file without a valid compile command receives syntax coverage and an
explicit semantic diagnostic, not a guessed default toolchain.

## P5E Rust

Use rust-analyzer or compiler metadata in an isolated worker.

Model crates, features, targets, modules, traits, implementations, macros,
generics, references, and calls. Index each selected feature/target context.
Procedural macros require a strict execution policy and may remain disabled in
high-assurance mode.

## P5F Java and Kotlin

Use JDT/Kotlin compiler or SCIP-compatible indexers. Detect Maven/Gradle project
models without running untrusted build logic by default. Provide an opt-in
isolated build-model extraction mode.

Model modules, packages, overloads, generics, inheritance, interfaces,
annotations, generated sources, and references. Kotlin-specific extension,
coroutine, data/sealed class, and nullability semantics remain explicit.

## P5G C#

Use Roslyn workspaces in a native worker. Load solution/project files with
restricted MSBuild evaluation, model target frameworks and conditional symbols,
and emit exact symbols, types, references, inheritance, attributes, and calls.

## Adapter benchmark requirements

Each production adapter publishes:

- fixture and real-repository corpus commits;
- toolchain versions and contexts;
- definition/reference precision and recall;
- unresolved counts by cause;
- parse/index time and peak memory;
- incremental speed and clean-equivalence;
- unsupported constructs;
- license and redistribution constraints;
- false-positive/false-negative examples.

---

# Phase P6: framework, interface, data, build, and runtime packs

## Common pack interface

```go
type FrameworkPack interface {
    ID() string
    Match(ctx context.Context, snapshot SnapshotView) ([]Candidate, error)
    Extract(ctx context.Context, candidate Candidate, sink graphpatch.StreamSink) error
}
```

Packs consume semantic facts when available and degrade to declared syntax or
manifest evidence with the weaker resolution class preserved.

## P6A HTTP and API

Implement:

- OpenAPI JSON and YAML 3.x plus Swagger 2;
- REST framework route registration for supported languages;
- middleware chain and authorization annotations;
- request/response schema links;
- GraphQL schemas, operations, resolvers, and directives;
- protobuf/gRPC services, messages, fields, and RPC handlers;
- AsyncAPI and WebSocket/event endpoints.

Route collisions, unresolved handlers, schema reference failures, and divergent
code/spec declarations become conflicts.

## P6B CLI

Recognize standard libraries/frameworks such as Cobra, argparse, Click/Typer,
Commander, clap, picocli, System.CommandLine, and common native parsers.

Emit commands, subcommands, positional arguments, flags, defaults, environment
inputs, exit codes, handler functions, examples, and tests.

## P6C Configuration

Build a configuration lineage model:

```text
declaration -> default -> validation -> override source -> read sites -> effect
```

Support environment variables, JSON/YAML/TOML/HCL configuration, command-line
overrides, feature flags, secret classification, and framework settings.
Do not record raw secret values.

## P6D SQL and data lineage

Add dialect-aware SQL parsers and adapters for migrations/ORMs.

Emit databases, schemas, tables, columns, indexes, views, procedures, queries,
read/write relations, migrations, and application-model mappings. Lineage edges
must state whether they are declared, statically inferred, or runtime observed.

## P6E Messaging and events

Support Kafka, AMQP, cloud queues/topics, and framework event buses. Emit topics,
queues, event types, producers, consumers, serialization, delivery semantics,
and handler links.

## P6F Build and deployment

Index:

- Docker/Compose;
- Kubernetes and Helm;
- Terraform/HCL;
- GitHub Actions and common CI systems;
- Make/CMake/Bazel/Ninja/MSBuild/Gradle/Maven;
- package lockfiles and dependency graphs;
- generated artifact relationships;
- deployment targets, images, ports, health checks, and secrets references.

Parsing must not execute templates or build hooks in default mode.

## P6G Runtime evidence

Runtime collection remains opt-in and profile-scoped.

```go
type ObservationContext struct {
    EnvironmentID string
    TestRunID     string
    StartedAt     time.Time
    ToolDigest    string
    PolicyDigest  string
}
```

Collect observed calls, coverage, loaded plugins/modules, registered routes,
configuration reads, SQL queries, and emitted/consumed events. Every observation
includes environment and run context. Lack of observation never becomes proof of
absence.

## Exit gate

Every supported public surface has a node, handler/resolver link or explicit
unresolved diagnostic, source/spec evidence, and coverage denominator.

---

# Phase P7: production search, browser, APIs, and SDKs

## Goal

Serve large snapshots without loading the entire bundle into process memory or
browser memory.

## FTS5 search

Index documents with fields:

```text
object_type
object_id
name
qualified_name
signature
path
language
kind
body
```

Ranking combines:

```text
exact ID/name match
prefix/name score
BM25 field score
path/package proximity
public surface weight
test/documentation relationship
graph expansion score
optional semantic score
```

Return a retrieval trace showing every score component and expansion edge.

## Query API

```go
type SearchRequest struct {
    SnapshotID string
    Text       string
    Kinds      []string
    Languages  []string
    PathPrefix string
    PublicOnly *bool
    Limit      int
    Cursor     string
    Expand     GraphExpansion
}
```

All list/search/graph endpoints require bounded limits and opaque cursors.
Server-side timeouts and graph-node limits are mandatory.

## Browser architecture

```text
web/
├── src/api/
├── src/features/overview/
├── src/features/source/
├── src/features/symbols/
├── src/features/graph/
├── src/features/search/
├── src/features/coverage/
├── src/features/diff/
├── src/features/evidence/
└── src/workers/
```

Views:

- repository/project overview;
- source tree and syntax-highlighted source;
- symbol details and evidence;
- callers/callees/references/inheritance/tests;
- API, CLI, configuration, data, event, build, and deployment surfaces;
- bounded graph neighbourhood explorer;
- search with filters and retrieval trace;
- coverage, unresolved, conflicts, and diagnostics;
- semantic diff and impact;
- optional evidence-grounded explanation.

Large static exports shard data by prefix/project and load on demand. The graph
view defaults to one neighbourhood and never downloads the entire graph because
someone discovered WebGL.

## SDK generation

Generate and test:

- Go client maintained manually or generated;
- TypeScript client;
- Python client;
- API examples and contract tests.

The MCP adapter calls the same application services as REST. No private MCP-only
truth store.

## Exit gate

The medium and large benchmark snapshots meet p95 query/graph/browser targets,
and browser startup transfers only bounded overview data.

---

# Phase P8: complete local-model validation and benchmark

## Goal

Turn the existing provider and packet pipeline into a measured optional feature
with reproducible quality and resource bounds.

## Steps

1. define approved model profiles by exact file digest and license;
2. add GGUF metadata parsing tests over representative quantizations;
3. add external RSS measurement in benchmark harness;
4. add task corpus for symbol purpose, module summary, execution explanation,
   and documentation-gap analysis;
5. define deterministic evidence-based scoring;
6. measure unsupported-claim rejection and false acceptance;
7. add summary cache keyed by packet/model/prompt/policy digests;
8. invalidate summaries when any cited evidence changes;
9. load embedding and generation models sequentially in the strict profile;
10. add optional remote provider interface with explicit egress policy;
11. expose model status, digest, resource estimate, and validation statistics;
12. publish model-disabled baseline comparison.

## Claim quality metrics

```text
citation validity
identifier validity
fact precision against packet
unsupported claim rejection rate
accepted claim contradiction rate
coverage of allowed claim categories
human usefulness score
latency
peak RSS
tokens per second
```

## Exit gate

The chosen local profile remains below the published RSS ceiling on the
reference machine, and no accepted claim in the benchmark corpus lacks valid
supporting evidence. If the model cannot meet this, deterministic docs remain
the supported default and the product claim is narrowed.

---

# Phase P9: semantic diff, CI policy, and documentation maintenance

## Goal

Make repository changes reviewable as semantic changes, not merely textual
patches.

## Diff classes

- artifact addition/removal/move/content change;
- logical symbol addition/removal/rename/signature/visibility/stability change;
- API route, request, response, or auth change;
- CLI command/argument/flag change;
- configuration key/default/validation change;
- schema/table/column/migration change;
- dependency/build/deployment change;
- edge addition/removal/resolution change;
- documentation freshness and contradiction change;
- coverage regression;
- analyzer/toolchain context change.

## Breaking policy engine

```go
type PolicyRule interface {
    ID() string
    Evaluate(ctx context.Context, diff SemanticDiff, snapshot SnapshotView) []Finding
}
```

Rules are versioned and produce stable findings with evidence paths. Examples:

- removed public symbol;
- incompatible signature;
- removed API response field;
- new unauthenticated route;
- database destructive migration;
- undocumented public configuration key;
- reduced semantic parse or edge-resolution ratio;
- accepted generated claim made stale by source change.

## CI outputs

- nonzero exit status;
- JSON policy report;
- SARIF;
- pull-request Markdown summary;
- links into static atlas;
- machine-readable semantic diff;
- affected tests and owners where available.

## Documentation maintenance

On change:

1. identify changed canonical facts;
2. mark dependent documents/claims stale;
3. regenerate deterministic sections;
4. rebuild only affected model packets;
5. revalidate claims;
6. preserve human-authored sections unless their evidence is contradicted;
7. show documentation drift as a review finding.

## Exit gate

CI detects fixture breaking changes with no unsupported path claims, and every
reported impact path is reproducible from canonical edges/evidence.

---

# Phase P10: PostgreSQL team service

## Goal

Add concurrent multi-repository operation without changing canonical meaning.

## Services

```text
rkcd              API, auth, orchestration, read queries
rkc-worker        isolated acquisition/analysis jobs
rkc-plugin-host   WASI/native execution boundary
rkc-migrate       database migrations
object store      source and generated blobs
PostgreSQL        metadata, graph, jobs, audit, FTS
queue             durable job claiming and leases
```

A separate broker may be added only when PostgreSQL queue semantics are measured
insufficient. Distributed systems remain a cost, not a maturity badge.

## Tenant model

```text
organisation
  -> workspace
      -> repository
          -> snapshot
```

Every row/object key includes tenant scope. PostgreSQL row-level security is a
defense-in-depth layer; application authorization remains mandatory.

## Authentication and authorization

- OIDC authorization code flow for users;
- workload identity/service accounts for automation;
- short-lived access tokens;
- RBAC roles such as viewer, analyst, maintainer, admin, security auditor;
- repository and export permissions;
- separate permission for source text, secret findings, model egress, runtime
  evidence, plugin installation, and retention holds.

## Job model

```sql
UPDATE jobs
SET state='running', lease_owner=$worker, lease_until=now()+$lease
WHERE job_id = (
  SELECT job_id FROM jobs
  WHERE state='queued' AND not_before <= now()
  ORDER BY priority DESC, created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING *;
```

Workers heartbeat leases. Jobs are idempotent by source/config/plugin/toolchain
identity. Cancellation propagates to plugin runners.

## Object-store consistency

- upload staged blobs with digest verification;
- commit database references transactionally;
- mark committed snapshot;
- garbage-collect orphaned uploads after grace period;
- periodically verify sampled/full object digests;
- encrypt with tenant or service keys according to deployment policy.

## Service API

Implement the future OpenAPI contract in slices:

1. repositories and snapshots;
2. nodes, edges, evidence, coverage, diagnostics;
3. search and graph;
4. jobs and exports;
5. semantic diff and impact;
6. plugins and model profiles;
7. policies and audit.

Use cursor pagination, idempotency keys, problem details, request IDs, and
asynchronous export jobs.

## Operations

- migrations and rolling upgrade compatibility;
- database PITR and restore drills;
- object-store versioning/replication;
- queue backlog and worker saturation alerts;
- tenant quotas;
- audit retention and legal holds;
- support bundles with source redaction;
- disaster recovery objectives;
- chaos tests for worker/database/object failures.

## Exit gate

Tenant isolation, authorization, backup/restore, rolling upgrade, queue recovery,
worker cancellation, quota, audit, and degraded-mode acceptance suites pass.

---

# Phase P11: release and supply-chain engineering

## Goal

Make every distributed artifact identifiable, verifiable, reproducible, and
reviewable.

## Release outputs

- source archive;
- Linux/macOS/Windows binaries for supported architectures;
- signed multi-architecture container images;
- checksum manifest;
- SPDX and/or CycloneDX SBOM;
- SLSA-compatible build provenance;
- vulnerability and license reports;
- plugin lock and official plugin signatures;
- database migration manifest;
- benchmark and conformance reports;
- release notes and compatibility statement.

## Build pipeline

1. protected tag triggers an isolated build;
2. source commit and submodules are pinned;
3. dependencies and base images use lock/digest references;
4. tests and release verification run;
5. binaries build with `-trimpath` and version metadata;
6. clean-room install tests run on each platform;
7. SBOM and provenance are generated;
8. binaries/images/checksums/attestations are signed;
9. signatures are verified before publication;
10. artifacts are uploaded immutably;
11. release manifest records every digest and compatibility version;
12. rollback/revocation procedure is tested.

## Reproducibility

Canonical outputs must be reproducible. Binary bit-for-bit reproducibility is a
separate target affected by toolchain/linker/platform details and must be
measured honestly. At minimum, provenance must make non-identical builds
explainable.

## Exit gate

A clean machine can verify source, checksum, signature, SBOM, provenance, and
plugin/model digests before executing the release.

---

# Phase P12: benchmark, hardening, and 1.0 acceptance

## Corpus classes

| Class | Files | Text | Symbols | Edges |
|---|---:|---:|---:|---:|
| Small | 1,000 | 20 MB | 20,000 | 100,000 |
| Medium | 20,000 | 500 MB | 500,000 | 3,000,000 |
| Large | 150,000 | 5 GB | 5,000,000 | 30,000,000 |
| Extreme | policy-specific | policy-specific | 20M+ | 100M+ |

The corpus includes public repositories pinned to commits, synthetic edge-case
fixtures, malformed/adversarial repositories, monorepositories, generated code,
dynamic frameworks, and history sequences.

## Performance targets

Initial targets, subject to published revision:

- no-model local daemon idle RSS below 300 MiB;
- strict local model peak RSS below 3.5 GiB on declared hardware;
- warm exact symbol query p95 below 100 ms;
- warm lexical query p95 below 200 ms on medium corpus;
- 1,000-node neighbourhood p95 below one second;
- initial browser shell and overview below 2 MB compressed;
- plugin cancellation response below two seconds after host cancellation;
- no whole-bundle memory load for server pagination;
- incremental scan materially faster than clean scan on localized changes;
- clean/incremental canonical equality at 100%.

## Reliability targets

- interrupted writes publish no partial snapshot;
- committed snapshots remain readable after restart;
- object corruption is detected;
- backup/restore reproduces canonical digests;
- worker retries are idempotent;
- migrations are forward tested from every supported version;
- derived projections can be deleted and rebuilt;
- API and MCP return bounded errors rather than panic.

## Security targets

- sandbox escape suite passes;
- no undeclared plugin capability succeeds;
- path/archive/symlink corpus cannot escape materialization;
- secret-redaction fixtures do not leak raw values;
- prompt-injection corpus cannot alter policy or accepted unsupported claims;
- tenant isolation suite has zero cross-tenant reads/writes;
- release verification proves signatures, SBOM, and provenance.

## Accuracy targets

Publish by adapter and relation class:

- declaration precision/recall;
- definition/reference precision/recall;
- call-edge precision/recall where claimed;
- route/API/CLI/config/schema extraction accuracy;
- unresolved counts and causes;
- documentation claim precision;
- false secret-finding rate;
- regression from previous release.

## Final 1.0 gate

A release candidate is accepted only when:

1. all required phases’ exit gates pass;
2. no critical/high unresolved security issue lacks an approved exception;
3. the migration/backup/restore drill succeeds;
4. adapter accuracy reports are published;
5. benchmark raw data is published;
6. clean installation succeeds on every supported platform;
7. release artifacts verify cryptographically;
8. documentation and examples match the executable interfaces;
9. known limitations are explicit;
10. the product remains fully useful with all models disabled.

---

# Exact ordered implementation work list

The following order can be copied into an issue tracker. Dependencies are
strict unless an item states otherwise.

## Storage and pipeline

1. `STORE-001`: create `pkg/rkcstore` read/write/query interfaces.
2. `STORE-002`: add in-memory conformance implementation.
3. `STORE-003`: split SQLite DDL into immutable migrations.
4. `STORE-004`: implement migration journal and digest verification.
5. `STORE-005`: implement SQLite connection initialization and health checks.
6. `STORE-006`: implement build/snapshot transaction lifecycle.
7. `STORE-007`: implement artifact/evidence/node/edge batch writers.
8. `STORE-008`: implement documents/claims/conflicts/paths/coverage writers.
9. `STORE-009`: implement paginated readers.
10. `STORE-010`: implement bundle import/export.
11. `STORE-011`: integrate CAS references and orphan collection.
12. `STORE-012`: add process-kill recovery fixtures.
13. `PIPE-001`: convert acquisition and inventory to stages.
14. `PIPE-002`: convert each analyzer/pack to stages.
15. `PIPE-003`: persist stage journal and ownership.
16. `PIPE-004`: integrate resource admission and cancellation.
17. `PIPE-005`: migrate cache metadata to store.
18. `PIPE-006`: implement tombstones and invalidation graph.
19. `PIPE-007`: add parent-snapshot incremental planning.
20. `PIPE-008`: build history corpus and equivalence CI.

## Plugin runtime

21. `PLUG-001`: finalize WIT/component protocol and compatibility fixtures.
22. `PLUG-002`: implement WASI runner with no ambient capabilities.
23. `PLUG-003`: implement streamed GraphPatch sink and limits.
24. `PLUG-004`: implement Linux native worker launcher.
25. `PLUG-005`: add macOS and Windows containment profiles.
26. `PLUG-006`: add signature/provenance verification.
27. `PLUG-007`: build conformance harness and malicious plugins.
28. `PLUG-008`: route Python worker through native launcher.

## Parsing and semantics

29. `SYN-001`: grammar registry and artifact locking.
30. `SYN-002`: parser pool, cancellation, and memory limits.
31. `SYN-003`: TypeScript/JavaScript query pack.
32. `SYN-004`: Python and Go differential query packs.
33. `SEM-PY-001..010`: Python semantic workstream.
34. `SEM-TS-001..010`: TypeScript compiler workstream.
35. `SEM-GO-001..010`: Go type/SSA workstream.
36. `SEM-CPP-001..010`: Clang workstream.
37. `SEM-RS-001..008`: Rust workstream.
38. `SEM-JVM-001..010`: Java/Kotlin workstream.
39. `SEM-CS-001..008`: Roslyn workstream.
40. `SEM-OTH-001`: define beta adapter acceptance for remaining languages.

## Framework packs

41. `PACK-API-001`: OpenAPI YAML and reference resolver.
42. `PACK-API-002`: HTTP framework registrations.
43. `PACK-GQL-001`: GraphQL schemas/resolvers.
44. `PACK-RPC-001`: protobuf/gRPC.
45. `PACK-CLI-001`: common CLI frameworks.
46. `PACK-CFG-001`: configuration lineage.
47. `PACK-SQL-001`: SQL dialect parser and schema graph.
48. `PACK-ORM-001`: ORM mappings.
49. `PACK-MSG-001`: messaging/events.
50. `PACK-BUILD-001`: build systems and generated artifacts.
51. `PACK-DEPLOY-001`: Docker/Compose/Kubernetes/Terraform/CI.
52. `PACK-RUN-001`: authorized runtime observation protocol.

## User-facing products

53. `SEARCH-001`: FTS5 indexing from committed snapshots.
54. `SEARCH-002`: field-aware rank and retrieval traces.
55. `API-001`: cursor pagination and problem-details standardization.
56. `API-002`: generated TypeScript/Python clients.
57. `WEB-001`: TypeScript application shell and API layer.
58. `WEB-002`: source/symbol/evidence views.
59. `WEB-003`: bounded graph explorer.
60. `WEB-004`: coverage/conflict/diagnostic views.
61. `WEB-005`: semantic diff/impact views.
62. `IDE-001`: VS Code proof-of-concept using local API/MCP.

## Models and documentation

63. `MODEL-001`: approved profile registry and model digests.
64. `MODEL-002`: real GGUF memory benchmark harness.
65. `MODEL-003`: evidence-scored task corpus.
66. `MODEL-004`: summary cache and invalidation.
67. `MODEL-005`: remote-provider policy and egress audit.
68. `DOC-001`: claim freshness and contradiction engine.
69. `DOC-002`: deterministic architecture/execution-path templates.
70. `DOC-003`: validated examples and code-snippet execution policy.

## CI, service, release

71. `DIFF-001`: robust logical identity and rename confidence.
72. `DIFF-002`: breaking-change policy library.
73. `CI-001`: pull-request report and SARIF integration.
74. `SERVICE-001`: PostgreSQL store conformance.
75. `SERVICE-002`: object-store adapter and consistency jobs.
76. `SERVICE-003`: durable queue and worker leases.
77. `SERVICE-004`: OIDC, RBAC, tenancy, quotas, and audit.
78. `SERVICE-005`: backups, restore, migrations, and rolling upgrades.
79. `OBS-001`: OpenTelemetry metrics, traces, and structured logs.
80. `REL-001`: multi-platform release builds.
81. `REL-002`: SBOM, signing, and provenance.
82. `BENCH-001`: maintained benchmark corpus and dashboard.
83. `SEC-001`: full adversarial and sandbox suite.
84. `GA-001`: production acceptance rehearsal.
85. `GA-002`: 1.0 release review and publication.

---

# Recommended staffing and parallelism

A credible production effort is a small senior team, not one person multitasking
between compiler internals, browser UX, supply-chain security, and customer
support until causality gives up.

Suggested workstreams:

- core storage/pipeline: 2 engineers;
- language/plugin platform: 2–4 engineers plus adapter contributors;
- browser/API/integrations: 2 engineers;
- security/service/operations: 2 engineers;
- model/documentation quality: 1–2 engineers;
- testing/benchmark/release: shared ownership with one dedicated lead.

P1 and P2 form the critical path. P3 and P4 can begin after public store/stage
interfaces stabilize. P7 UI work can use the current API while P1 proceeds, but
must migrate to paginated store-backed endpoints before release. Language
adapter work can proceed in isolated workers once GraphPatch and conformance
contracts are frozen.

# Proof path when blocked

When a task cannot be completed safely or accurately, record:

1. the exact missing input or invariant;
2. the smallest reproducible fixture;
3. current evidence and diagnostics;
4. the responsible stage/plugin/toolchain;
5. whether the result is unsupported, unresolved, failed, or policy-denied;
6. a deterministic command that reproduces the block;
7. the acceptance test that will prove the future fix.

RKC should never replace a known block with a smooth paragraph. Smooth
paragraphs are plentiful. Auditable boundaries are the scarce resource.
