# Implementation status

Version: `0.3.0-reference`

The labels below mean:

- **implemented**: exercised by tests or release smoke checks;
- **partial**: useful path exists but production invariants remain incomplete;
- **planned**: architecture and work order exist, code does not yet satisfy the
  production exit gate.

## Core

| Capability | Status | Notes |
|---|---|---|
| Canonical RKR records | Implemented | Public Go package, schema 0.2.0 |
| Stable IDs and canonical ordering | Implemented | Deterministic digest tested |
| Referential and vocabulary validation | Implemented | Strict validation supported |
| Artifact inventory and SHA-256 | Implemented | Explicit exclusions and limits |
| Local/remote Git acquisition | Implemented | Promptless, hooks disabled, bounded timeout |
| Filesystem snapshot publication | Implemented | Building/committed states and recovery |
| Content-addressed object store | Implemented | Reference filesystem store |
| SQLite runtime writer/query layer | Planned | DDL exists and is validated |
| Pipeline DAG and cache library | Partial | Scheduler/cache exist; scan not fully staged |
| Clean/incremental equivalence | Planned | Deterministic clean replay passes |

## Analysis

| Adapter or pack | Status | Precision |
|---|---|---|
| Python | Implemented | Standard-library AST syntax tier |
| Go | Implemented | Go AST syntax tier |
| JavaScript/TypeScript | Implemented | Conservative dependency-free syntax tier |
| Markdown | Implemented | headings, hierarchy, links, fenced blocks |
| OpenAPI | Partial | JSON documents; YAML pending |
| JSON Schema | Partial | JSON documents and references |
| package/build manifests | Partial | npm, Go, Python requirements, Docker |
| environment templates | Implemented | keys, defaults, required/secret metadata |
| secret detection/redaction | Implemented | pattern scanner; not a complete DLP system |
| compiler-grade semantic adapters | Planned | Python, TypeScript, Go first |
| Tree-sitter universal host | Planned | grammar registry and queries specified |
| runtime evidence | Planned | disabled by default and sandbox-dependent |

## Knowledge products

| Product | Status | Notes |
|---|---|---|
| Canonical bundle and JSONL | Implemented | portable, deterministic |
| Markdown documentation | Implemented | deterministic facts and symbol pages |
| normalized source envelopes | Implemented | likely secrets redacted by default |
| NotebookLM pack | Implemented | byte-bounded grouping |
| static browser | Implemented | self-contained reference UI |
| ranked lexical search | Implemented | persisted portable index |
| semantic/hybrid query | Partial | qualified `llama.cpp` embedding path and corpus-bound vector receipts implemented; no qualified/default model active |
| FTS5 runtime search | Planned | depends on SQLite runtime writer |
| graph paths, impact, SCCs | Implemented | bounded in-memory graph operations |
| semantic diff | Implemented | conservative logical/signature comparison |
| guarded self-catalogue | Implemented | immutable commit-tree blob staging; recursive-output/model-weight exclusion; atomic complete publication and deterministic receipts |
| embeddings | Partial | exact qualified asset/runtime resolver and CLI integration implemented; committed candidate remains unqualified |

## Model subsystem

| Capability | Status | Notes |
|---|---|---|
| bounded evidence packets | Implemented | coherent truncation and redaction |
| `llama.cpp` CLI provider | Implemented | fake-executable integration tested |
| pinned native `llama.cpp` bootstrap | Implemented | exact source digest, CPU-only portable/native profiles, guarded build |
| cgroup, priority, CPU-only and RSS policy | Partial | guarded Linux path implemented; portable non-Linux hard limits pending |
| claim/summary validation | Implemented | citations and identifiers checked |
| grounded repository answers | Implemented | CLI uses bounded lexical/semantic/hybrid plus graph evidence, canonical re-resolution, validation, and abstention; qualified embedding/generation bindings required for model-backed modes |
| real GGUF benchmark below 2.5 GiB | Planned | generation and embedding candidates are unqualified and not defaults |
| remote model providers | Planned | policy/egress controls required |

## Interfaces

| Interface | Status |
|---|---|
| CLI | Implemented |
| local read-only HTTP API | Implemented |
| OpenAPI parity validation | Implemented |
| MCP stdio server | Implemented |
| Go read client | Implemented |
| TypeScript/Python generated SDKs | Planned |
| IDE extensions | Planned |
| team service API | Planned |

## Security and operations

| Capability | Status |
|---|---|
| repository code execution denied by normal scan | Implemented |
| secret redaction in normalized export | Implemented |
| bounded plugin stdout/stderr and timeout | Implemented |
| plugin manifests and lock digests | Implemented |
| WASI capability enforcement | Planned |
| native-worker OS sandbox | Partial | fail-closed Linux guard for the digest-pinned built-in Python adapter only; third-party execution disabled |
| OIDC/RBAC/tenancy/audit retention | Planned |
| signed binaries, SBOM, provenance | Planned |
| Docker and CI reference files | Implemented |
| full logged release verification | Implemented |

## Release test surface

`make release-verify` runs:

1. Go formatting check;
2. `go vet`;
3. Go tests;
4. Python analyzer tests;
5. JSON Schema, OpenAPI, WIT, SQLite, and lockfile validation;
6. local Markdown-link and code-fence checks;
7. binary builds;
8. plugin manifest/lock validation;
9. mixed-language scan and quality gate;
10. deterministic duplicate scan comparison;
11. HTTP API smoke test;
12. MCP smoke test;
13. constrained remote-Git acquisition test;
14. Go race detector;
15. self-analysis benchmark.

CI additionally runs `make self-catalogue` inside the delegated resource guard
and uploads the commit-bound `dist/self-catalogue` receipts and atlas. The
workflow does not qualify or promote a model; both committed model defaults
remain null until the separate measured qualification gate passes.
