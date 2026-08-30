# Implementation status

Version: `0.3.0-reference`

The labels below mean:

- **implemented**: exercised by tests or release smoke checks;
- **planned**: architecture and work order exist, code does not yet satisfy the
  production exit gate.

## 2026-07-27 production acceptance checkpoint

| Area | Current state | Production boundary |
|---|---|---|
| Scan journals | Implemented | Command outcome includes post-DAG policy and publication failures; resilient bounded list/show, symlink-safe owner-only state paths, hash-chain replay, crash-tail recovery, and Unix runtime/Windows protected-DACL contract tests are present. Explicit journal pruning and SQLite history projection remain additive roadmap work rather than correctness dependencies |
| OpenAPI JSON/YAML | Implemented | Common bounded reads, duplicate-key rejection, YAML depth/token/node limits, safe scalar normalization, escaped local references, cache invalidation, and cold/warm/edit parity are covered. Cross-file reference resolution remains a separate roadmap item |
| Workbench lifecycle and GUI | Core implemented on guarded Linux; browser acceptance partial | Submission deadlines, cancellation, bounded cleanup proof, truthful `cleanup_failed`, sanitized user-bus access, close/shutdown, and DELETE API are targeted-test green. One typed catalogue now drives static and live command metadata, exact atlas/SQLite selectors, valid defaults, and the allowlist; token bootstrap is same-origin. Structural responsive/accessibility contracts are tested, but reproducible browser/assistive-technology CI remains required before claiming full GUI acceptance |
| Workbench containment | Implemented for the allowlisted command profile | One guarded server scope, single-job admission, per-command process groups, deadline/cancel TERM-to-KILL cleanup proof, and rejection of nested servers or commands that may create separately managed units prevent supported work from escaping. Unprovable cleanup fails visibly and blocks a success claim |
| Model default | Intentionally unset after complete qualification | Qwen3.5 4B Q4_0 improved to 4/6 generation cases with no unsupported claim or canary leakage, but over-abstained on the hostile-input case, timed out at exact 32K, and exceeded the strict process-RSS gate despite a safe 4.156 GB cgroup peak. Granite 4 H 1B and Qwen3.5 0.8B each passed only 2/6; Gemma 4 E2B and Qwen3.5 2B were also rejected. The paired Qwen3 embedding role passed all gates, but pair-level promotion correctly remained disabled |
| Release verification | Passed locally and enforced on every `main` push | The guarded immutable release verifier passed in 371 seconds on production code commit `03f56e7`; Go coverage was above the 90% gate, Python line+branch coverage was above 90%, race/portability/contracts/licenses/smokes/benchmark passed, and two cache-isolated builds produced the identical 51,652,037-byte archive with SHA-256 `45e16892f0345b73cbad68adc174100c6cb65870e6969d4e0e2329dc7d4a0908`. The guarded self-catalogue then republished and answered live lexical queries over RKC itself; CI repeats release packaging, self-cataloguing, and a hardened read-only container smoke with an explicit isolated state volume |

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
| Transactional storage contract | Implemented | Typed reader/writer/recovery API; atomic, immutable in-memory conformance backend with authenticated cursors and lossless export |
| SQLite driver/bootstrap | Implemented | Pinned pure-Go driver, embedded digest-locked migrations through schema `0.4.0`, fail-closed build/publication compare-and-swap, monotonic current-pointer guards, CGO-free build gates, reader-key initialization, read-only consumers, and strict database-open health checks |
| SQLite runtime writer/query layer | Implemented | Transactional staging/publication, OS writer leases, recovery, digest-verified canonical reads, exact coverage binding, authenticated pagination, projections, and CLI/HTTP/MCP integration |
| Pipeline DAG and cache library | Implemented | All 16 canonical scan stages route through the deterministic DAG with bounded resource admission; owner-only hash-chained command journals and ownership-bound verified CAS payloads provide selective keys plus `plan`/inspect/verify/prune UX. Retries, additional derived-output stages, and SQLite journal projection are future extensions, not hidden fallbacks |
| Clean/incremental equivalence | Implemented | Cold, warm, reversed-input, and localized-change paths are differentially checked against clean canonical output; the release benchmark and guarded RKC self-catalogue exercise repository-scale determinism |
| Portable command builds | Implemented | `make portable-build` compiles both CGO-free commands for Linux, macOS, and Windows `amd64`/`arm64` targets in a private temporary workspace; the reproducible reference package still publishes Linux binaries only until native packaging and install smoke gates are added for the other targets |

## Analysis

| Adapter or pack | Status | Precision |
|---|---|---|
| Python | Implemented | Standard-library AST syntax tier |
| Go | Implemented | Go AST syntax tier |
| JavaScript/TypeScript | Implemented | Conservative dependency-free syntax tier |
| Markdown | Implemented | headings, hierarchy, links, fenced blocks |
| OpenAPI | Implemented | Bounded strict JSON and YAML 3.x plus Swagger 2 surfaces; duplicate keys, unsafe YAML constructs, parser limits, and external-reference fetching fail closed |
| JSON Schema | Implemented | Strict bounded JSON documents, nested properties, definitions, deterministic local JSON Pointer reference resolution, unresolved-reference placeholders, and fail-closed diagnostics |
| package/build manifests | Implemented | Deterministic npm, Go module, Python requirements, and Docker extraction with all dependency scopes, string/object CLI bins, multi-entry replacements, bounded readers, and secret-default redaction |
| environment templates | Implemented | keys, defaults, required/secret metadata |
| secret detection/redaction | Implemented | pattern scanner; not a complete DLP system |
| compiler-grade semantic adapters | Implemented through SCIP import | Streaming dependency-free SCIP ingestion preserves compiler-resolved symbols, definitions, references, implementations, signatures, documentation, diagnostics, roles, and exact UTF-8/16/32 source ranges. Repeatable `--scip-index` integration is available in `quickstart`, `plan`, `scan`, and the complete GUI command center for Python, JavaScript/TypeScript, Go, C/C++/CUDA, Rust, Java/Kotlin/Scala, C#/Visual Basic, and any conforming producer. RKC deliberately does not execute indexers during normal scans |
| Tree-sitter universal host | Planned | grammar registry and queries specified |
| runtime evidence | Planned | disabled by default and sandbox-dependent |

## Knowledge products

| Product | Status | Notes |
|---|---|---|
| Canonical bundle and JSONL | Implemented | portable, deterministic |
| Markdown documentation | Implemented | deterministic facts and symbol pages |
| normalized source envelopes | Implemented | likely secrets redacted by default |
| NotebookLM pack | Implemented | Byte-bounded grouping, deterministic source inventory, exact byte counts, and a generated `UPLOAD.md` guide with grounding and quota-handling instructions; the default target is 4,000,000 bytes |
| responsive browser and local workbench | Implemented core; browser acceptance partial | Accessible static fallback and opt-in token-authenticated guarded loopback execution; complete typed CLI palette, dataset-aware exact argument arrays, bounded output, deadlines, cancellation, all terminal/cleanup states, and responsive desktop/mobile layout contracts are unit-tested. Browser automation, assistive-technology acceptance, paging/virtualization, guided forms, and live incremental job output remain open |
| ranked lexical search | Implemented | persisted portable index |
| semantic/hybrid query | Implemented, model-gated | Exact-qualified `llama.cpp` embedding path, corpus-bound vector receipts, deterministic lexical fusion, and GraphRAG expansion are complete. With no pair-qualified default, model-backed mode fails closed while lexical/FTS5/graph search remains fully available |
| FTS5 runtime search | Implemented | `query --database` ranks the committed snapshot through SQLite FTS5/BM25 with literal-token MATCH construction, deterministic ties and traces, typed failures, cancellation, field filters, UTF-8/result bounds, and shared semantic-fusion/GraphRAG expansion |
| graph paths, impact, SCCs | Implemented | bounded in-memory graph operations |
| semantic diff | Implemented | conservative logical/signature comparison |
| guarded self-catalogue | Implemented | immutable commit-tree blob staging; recursive-output/model-weight exclusion; atomic complete publication and deterministic receipts |
| quality and delta index | Implemented | dependency-free deterministic source/documentation inventory with SHA-256 metadata, conservative test/documentation associations, optional Go/Python profile mapping, and Git change triage; percentages are explicit evidence signals rather than semantic 100% claims |
| embeddings | Implemented, model-gated | Exact asset/runtime qualification binding, vector receipt generation, CLI integration, and strict retrieval scoring are complete. The Qwen3 embedding candidate passed its isolated gate, but the required generation/embedding pair did not, so no default is selected |

## Model subsystem

| Capability | Status | Notes |
|---|---|---|
| bounded evidence packets | Implemented | coherent truncation and redaction |
| `llama.cpp` CLI provider | Implemented | fake-executable integration tested |
| pinned native `llama.cpp` bootstrap | Implemented | exact source digest, CPU-only portable/native profiles, guarded build |
| cgroup, priority, CPU-only and RSS policy | Implemented on Linux; fail-closed elsewhere | Linux cgroup v2 enforces one CPU, weight 1, nice 19, idle I/O, 4 GiB operating/4.5 GiB hard memory, bounded swap/tasks, CPU-only runtime flags, ERAIS pre-emption, process-group reap, and auditable receipts. Priority receipts contain only PID/fixed-class data, never process arguments, and doctor checks never reproduce the user-manager environment. Heavy model operations refuse platforms that cannot prove this contract |
| claim/summary validation | Implemented | Atomic statements, citations, identifiers, certainty, inference policy, unsafe markup, and bounds are checked; free-form summaries are never published |
| grounded repository answers | Implemented | CLI uses bounded lexical/semantic/hybrid plus graph evidence, canonical re-resolution, validation, and abstention. Up to two sanitized retrieval-repair passes repeat the full validator under one deadline, retain a pass audit, select the strongest grounded attempt, and never ingest generated output. Qualified embedding/generation bindings are required for model-backed modes |
| real GGUF benchmark below 4.5 GiB | Fully qualified rejection; no promotion | Qwen3.5 4B Q4_0 completed the guarded pair gate with a 4.156 GB cgroup peak and 4/6 generation cases, but failed hostile-input grounding, exact-32K latency, and strict process RSS. Qwen3 Embedding 0.6B Q8_0 again passed recall, margin, norm, memory, and latency checks. The required pair failed, so defaults remain null |
| remote model providers | Planned | policy/egress controls required |

## Interfaces

| Interface | Status |
|---|---|
| CLI | Implemented | `open` (alias `start`) composes scan, strict checks, loopback serving, and optional desktop-browser launch for a non-technical first run |
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
| native-worker OS sandbox | Implemented for the supported built-in adapter | The digest-pinned built-in Python adapter runs only through the fail-closed Linux user-systemd isolation boundary with bounded resources and sanitized environment. Third-party native execution and unsupported platforms are disabled rather than weakly sandboxed |
| OIDC/RBAC/tenancy/audit retention | Planned |
| per-binary Go-module SPDX SBOM | Implemented | Deterministic SPDX 2.3 JSON is generated for every Linux executable and independently rebound to its checksum, command, GOOS/GOARCH, normalized GOAMD64/GOARM64 target, default GOEXPERIMENT set, `GOFIPS140=off`, exact Go toolchain, immutable source commit/tree/time, module lock, canonical Go purls, and actual linked modules during packaging; audited declared expressions are retained and every unanalyzed package conclusion remains `NOASSERTION` |
| complete-distribution SPDX SBOM | Implemented | `SBOM.spdx.json` inventories substantive archive files, all four platform command components, and their linked Go modules; circular receipt files are explicitly excluded, the manifest hashes the SBOM, and final checksums hash both |
| release signing, container SBOM, provenance | Planned | No publication claim until signatures and attestations are generated and verified |
| Docker and CI reference files | Implemented |
| full logged release verification | Implemented |

## Release test surface

`make release-verify` runs:

1. checksum-locked Go module download and cache verification;
2. Python 3.11+ and exact required validation-distribution version checks;
3. Go formatting check;
4. `go vet`;
5. Go tests;
6. Python analyzer tests;
7. JSON Schema, OpenAPI, WIT, immutable SQLite migration, and lockfile validation;
8. local Markdown-link and code-fence checks;
9. model/runtime supply-chain lock validation;
10. CGO-disabled binary builds;
11. plugin manifest/lock validation;
12. mixed-language scan and quality gate;
13. deterministic duplicate scan comparison;
14. HTTP API smoke test;
15. MCP smoke test;
16. constrained remote-Git acquisition test;
17. Go race detector;
18. self-analysis benchmark.

The normal `make verify` target runs `make quality-index`, which emits the
file-level test, documentation, profiling, and Git-delta evidence described in
[`QUALITY_INDEX.md`](QUALITY_INDEX.md). CI additionally generates a fresh
guarded Go/Python profile first and feeds both reports into the uploaded index;
local fast runs omit profiles unless they are supplied explicitly.

The index is now package-aware for Go documentation, recognizes explicit
cross-language test-harness references, rejects symlinked output ancestors,
and imports the coverage gate's zero-statement/current-platform metadata. It
deduplicates Go blocks, rejects inconsistent profile denominators and Python
branch counters, and retains exact uncovered Go coordinates and Python
line/branch arcs for direct test triage. The current working tree reports 100%
test and documentation evidence under these heuristics; the fresh CI artifact
remains the authority for executable profile percentages and residual gaps.

`make safe-complete-package` runs that logged sequence inside the mandatory
resource guard. Commit-bound release commands reject tracked or untracked source
changes instead of silently validating an older `HEAD`. Validation itself uses
the required distribution versions in `requirements-dev.txt`, executes inside
an immutable private checkout, and atomically replaces `dist/evidence` only
after the complete validation and benchmark inventory passes. Packaging then
rebuilds binaries, SBOMs, and
deterministic demo inputs in two detached checkouts with lane-private Go build
and module caches, validates the exact successful raw evidence inventory, uses
stored ZIP entries, and requires byte equality of the final commit/tree-bound
archives. It publishes the ZIP, binaries, demo, and exact receipt-bound raw
evidence with one atomic `dist/release` generation swap.

The Python check is a dependency-version consistency gate, not a claim that a
mutable local virtual environment is hermetic: it does not reject unrelated
installed distributions or attest Python/package bytes. Release source and
publishing helpers are commit-bound; interpreter isolation and artifact signing
remain separate provenance work.

CI runs the complete release/package path and `make self-catalogue` inside the
delegated resource guard, then uploads the single `dist/release` generation and
the commit-bound `dist/self-catalogue` receipts and atlas. The workflow does not
qualify or promote a model; both committed model defaults remain null until the
separate measured qualification gate passes.
