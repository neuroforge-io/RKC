# Architecture

Copyright 2026 NeuroForgeIO and RKC contributors. NeuroForgeIO publishes and
maintains this Apache-2.0-licensed architecture and documentation. External dependencies, model assets, and
analyzed repositories retain their own ownership and licenses; see
[`BRANDING_AND_ATTRIBUTION.md`](BRANDING_AND_ATTRIBUTION.md).

## System purpose

RKC is a compiler pipeline for repository knowledge. Its target is a bounded
causal and executable model of a software system—not merely better code search.
It creates one immutable, evidence-bearing world model and treats every user
interface, export, agent request, and model response as a projection of that
model.

```text
repository or Git URL
  -> constrained acquisition
  -> complete inventory and policy dispositions
  -> immutable source hashes / content-addressed objects
  -> language and framework analyzers
  -> compiler semantics + bounded flow/configuration/history evidence
  -> explicitly authorized runtime capture assertions
  -> future producer-authenticated runtime events
  -> bounded GraphPatch fragments
  -> merge, conservative resolution, conflict retention
  -> canonical validation and coverage
  -> immutable snapshot publication
  -> search, graph, documentation, exports, HTTP, MCP, optional grounded answer
```

## Truth plane and presentation plane

The truth plane owns repository identity, artifact accounting, symbols,
relationships, evidence, diagnostics, conflicts, coverage, and immutable
snapshots. It must remain usable with the model subsystem disabled.

The presentation plane owns Markdown, browser pages, diagrams, search result
rendering, NotebookLM packs, model prose, and external integration formats. It
can be deleted and rebuilt from canonical records.

## Current component map

```text
cmd/rkc
  acquisition + configuration + scan orchestration + exports + quality gates

cmd/rkc-mcp
  standard-input/output MCP adapter

internal/pipeline
  current integrated inventory/analyzer/merge/validate path

internal/acquire
  local and constrained Git materialization

internal/inventory
  artifact traversal, classification, hashes, limits, dispositions

internal/lang/goast
internal/lang/tssyntax
internal/lang/scipindex
plugins/python-ast
  syntax adapters plus streaming compiler-semantic index ingestion

internal/docparse
internal/framework/*
  document, interface, manifest, environment, and security packs

internal/flow + internal/configenv
  bounded Go control/value flow plus build, CI, Terraform, and environment
  contracts

internal/runtime + internal/history
  digest-bound execution observations and bounded semantic Git deltas

pkg/rkcmodel
  public canonical records, stable IDs, sorting, validation, coverage

pkg/graphpatch
  plugin mutation contract and host-side validation/application

internal/snapshot + internal/cas
  filesystem reference snapshots and content-addressed objects

internal/search + internal/graph + internal/retrieval
  lexical/vector indexes, qualified embedding adapter, hybrid ranking, and
  bounded graph expansion

internal/answerapp + internal/groundedanswer
  shared retrieval-to-answer orchestration, evidence bounding, citation and
  claim validation, abstention, and answer provenance

internal/modelassets + internal/modelruntime
  exact qualified model/runtime binding, evidence packets, llama.cpp provider,
  memory policy, and structured generation validation

internal/export
  deterministic docs, normalized text, NotebookLM, static site, integrations

internal/server + internal/mcpserver
  read-only local interfaces plus an explicit Linux-only guarded workbench
```

## Snapshot identity

A source-truth snapshot is derived from:

```text
repository content digest
Git commit or working-tree digest
optional compiler-semantic input digest
optional runtime-observation input digests
optional semantic-history input digest
analysis-affecting configuration digest
policy digest
plugin lock digest
toolchain digest
canonical schema version
```

Wall-clock timestamps, output directories, browser settings, server addresses,
and model prose do not alter canonical repository identity.

Publication follows:

```text
building -> validating -> committed
```

Only a fully validated snapshot becomes current. Aborted builds retain logs but
cannot partially replace a committed snapshot.

## Analyzer precision tiers

| Tier | Mechanism | Assertion strength |
|---|---|---|
| 0 | inventory | path, bytes, hash, disposition |
| 1 | normalization | exact text derivative plus source mapping |
| 2 | syntax | declared syntax and structurally inferred relations |
| 3 | semantic | compiler/indexer-resolved symbols and types |
| 4 | framework | routes, APIs, configuration, schemas, build conventions |
| 5 | runtime | producer-authenticated events from an explicitly authorized execution |
| 6 | model | validated derived explanations only |

The current release implements Tiers 0–2 broadly for Python, Go, and
JavaScript/TypeScript, Tier 3 when SCIP input retains authenticated compiler
provenance, selected Tier-4 packs, bounded runtime capture assertions below
Tier 5, and Tier-6 packet/provider infrastructure. Portable SCIP inputs remain
producer-unverified structured assertions. SCIP
provides one deterministic semantic boundary for Python,
JavaScript/TypeScript, Go, C/C++/CUDA, Rust, Java/Kotlin/Scala, C#/Visual Basic,
and other conforming producers. Current aggregate coverage asserts that spans
were reported covered in one capture, but it does not authenticate execution,
call events, or individual test paths; RKC therefore does not invent them.

## Graph merge policy

Evidence is accumulated rather than overwritten. Resolution strength is
approximately:

```text
compiler_resolved
runtime_observed
declared
syntax_inferred
documentation_asserted
model_inferred
unresolved
```

That order selects a preferred canonical view but does not erase contradictory
records. Disagreements become `Conflict` records with candidate evidence.

Unresolved relations point to explicit `unresolved_symbol` nodes. This preserves
referential integrity and makes analyzer weakness measurable.

## Storage

The portable canonical interchange is `bundle.json`; immutable record-family
JSONL is also emitted. The current runtime publishes a filesystem snapshot and
content-addressed objects.

The production local target is SQLite with FTS5 and transactional snapshot
publication. The production service target is PostgreSQL plus S3-compatible
object storage. Neither a vector database nor graph database is canonical.

## Plugin boundary

Plugins declare identity, input selection, outputs, limits, determinism, and
capabilities. They return a versioned GraphPatch and never receive database
handles or publication authority.

Pure analyzers should use a capability-scoped WASI component. RKC does not
execute compiler indexers during a scan: operators generate SCIP in a separately
authorized environment, and the streaming importer treats it as bounded,
digest-bound untrusted data. General third-party native-worker sandboxing
remains future scope.

## Model boundary

A model receives one bounded evidence packet containing selected subject facts,
related nodes, edges, evidence, and redacted excerpts. It returns structured
atomic claims. The validator rejects compound statements, unknown citations,
unknown code identifiers, unsupported inference, malformed certainty, and
excess output.

Model results are written under `derived/` and cannot mutate `bundle.json`.
The user-facing `rkc answer` path likewise writes only to standard output. It
uses lexical, semantic, or hybrid retrieval plus bounded graph expansion,
re-resolves every selected record against the canonical bundle, and either
returns validated cited claims or abstains. Lexical remains the zero-model
default; model-backed modes require exact qualified retrieval and generation
bindings. Rejected claims and unresolved questions can drive at most two
sanitized, filter-neutralized retrieval repairs. Generated text never becomes
evidence, prompt instructions, or canonical graph input; every repair pass
starts from a newly bounded packet and repeats the complete validator.

Semantic and hybrid query modes use a vector index outside the verified atlas.
They are fail-closed: the model lock, GGUF digest, external model-license
qualification state, `llama.cpp` executable, and native-build receipt must
identify the same approved embedding binding. Lexical retrieval remains the
default. The
committed lock intentionally names no generation or embedding default because
its current lightweight candidates have not passed the qualification gate.

## Self-catalogue boundary

Self-cataloguing never scans the mutable checkout or a directory containing its
own output:

```text
clean recorded Git commit tree
  -> verified private blob copy and detached build
  -> guarded RKC build and scan
  -> complete disjoint staging catalogue and checksums
  -> atomic whole-directory publication
```

Every admitted byte is read from and checked against its recorded-tree Git
object before scanning. Links, submodules, special files, model weights,
generated/runtime trees, and dirty worktrees are rejected. The last-known-good
catalogue is never mutated before the replacement is fully validated. The
output manifest records that model execution and generated-output ingestion
were disabled.

## Interface boundary

HTTP and MCP load the same immutable dataset model and expose compatible
bounded lexical and graph projections. `rkc answer` uses the shared answer
application service and grounded-answer validator; model answering is not
currently an HTTP or MCP endpoint. The implemented HTTP routes are generated
into `api/openapi.yaml`. The larger multi-repository service design is retained
separately and must not be confused with the local daemon's current surface.

## Dependency direction rules

- canonical model packages do not import storage or UI code;
- plugins depend only on public contracts;
- storage implements read/write interfaces rather than leaking SQL upward;
- interface handlers and commands consume read-only dataset, search, and graph
  services;
- grounded model answers pass through the shared answer application service;
- model code receives read-only packets and cannot mutate graph state;
- exporters receive immutable snapshot readers;
- language adapters emit fragments or GraphPatch records;
- derived products never become hidden sources of canonical truth.

## Analysis stages

`scan` executes a deterministic 20-stage DAG: inventory, normalize, env-keys,
go-syntax, json-schema, manifests, markdown, openapi, python-syntax,
scip-semantic, secret-scan, typescript-syntax, merge, resolve, value-flow,
config-env, trace-import, history-import, validate, and coverage. The
`value-flow` stage compiles bounded call graphs, CFGs, and value-flow edges;
`config-env` compiles build tags, CI workflows, Terraform declarations, and
environment contracts; `trace-import` binds digest/source-affine runtime
assertions. Same-process capture authenticates record integrity only, so no
current capture is promoted to observed spans or terminal test outcomes.
Statement coverage never establishes an
observed call edge; `history-import` stamps symbol lifecycles and supersedes
edges. Every stage is deterministic and
cacheable where its inputs permit; trace and history digests bind into the
snapshot identity.

## Evidence acquisition loop and causal maturity

`rkc plan` performs evidence-opportunity planning. It inventories the target,
shows the exact 20-stage execution/cache plan, validates any supplied SCIP,
trace, or history inputs, and emits exact argument vectors for missing
higher-authority evidence. It does not execute analyzers, tests, indexers, or
history acquisition during planning. Runtime capture and compiler index
generation remain separately authorized operations.

The implemented loop is therefore:

```text
inspect current evidence
  -> expose missing compiler/runtime/history authority
  -> human or agent authorizes a bounded acquisition
  -> validate and digest the inert result
  -> compile a new immutable snapshot
  -> compare static possibility with observed actuality
```

Question-driven uncertainty records, targeted test generation, predicate-aware
symbolic execution, machine-readable test setup/stimulus/assertion contracts,
and cross-repository service federation remain explicit next evidence layers.
They must be implemented as bounded, cited records—not model guesses or hidden
background execution.

`rkc counterfactual` is the first derived causal-analysis surface. It compares
a canonical baseline route with a read-only view that omits exact nodes or
edges. Results are always non-authoritative, snapshot-bound, evidence-linked,
and explicit about traversal limits. They never mutate canonical truth or claim
that bounded structural reachability proves production causation.

## System and agent independence

RKC has no code, data, model, or runtime dependency on ERAIS. It is designed for
humans, local applications, CI, arbitrary agents, and future reasoning systems
through versioned CLI JSON, HTTP, MCP, SQLite, JSON/JSONL, GraphML, Markdown,
NotebookLM packs, and static browser artifacts. Higher-priority process
detection is a configurable shared-host scheduling policy, not an application
integration. Any future ERAIS use consumes the same public evidence interfaces
as every other agent.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
