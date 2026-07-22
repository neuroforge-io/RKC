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
| FTS5 runtime search | Planned | depends on SQLite runtime writer |
| graph paths, impact, SCCs | Implemented | bounded in-memory graph operations |
| semantic diff | Implemented | conservative logical/signature comparison |
| embeddings | Planned | configuration rejects unsupported enablement |

## Model subsystem

| Capability | Status | Notes |
|---|---|---|
| bounded evidence packets | Implemented | coherent truncation and redaction |
| `llama.cpp` CLI provider | Implemented | fake-executable integration tested |
| estimated and observed RSS policy | Partial | Linux monitoring path implemented |
| claim/summary validation | Implemented | citations and identifiers checked |
| real GGUF benchmark below 3.5 GiB | Planned | no model weights bundled |
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
| native-worker OS sandbox | Planned |
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
