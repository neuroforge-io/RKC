# Implementation status

This RKC status evidence is published by **NeuroForgeIO** under
[Apache-2.0](../LICENSE). Copyright remains with NeuroForgeIO and applicable RKC
contributors; percentages below are evidence signals, not claims of unmeasured
semantic completeness.

Version: `0.3.0-reference`

The labels below mean:

- **implemented**: exercised by tests or release smoke checks;
- **planned**: architecture and work order exist, code does not yet satisfy the
  production exit gate.

## Latest commit-bound acceptance evidence (2026-08-30)

The latest independently reviewed evidence checkpoint is signed commit
`bb5de8250df99c7ee1c2ce91633c136818e5adcc`, tree
`3fdd5a26e3a5d1e0f2adc59bde84cf995be6390e`. Its [main-branch CI
run](https://github.com/neuroforge-io/RKC/actions/runs/33311156962) and
[CodeQL run](https://github.com/neuroforge-io/RKC/actions/runs/33311156881)
both passed. The CI run exercised the guarded release verifier, two
cache-isolated reproducibility builds, Linux runtime packages, portable target
compilation, Docker build/runtime smoke, fresh Go/Python profiles, the quality
index, and the non-recursive RKC self-catalogue.

- Both complete-package builds produced the identical 53,789,045-byte archive
  with SHA-256
  `5c1532dd9e6baaddf3a226a4ddf572629ef6f6a07a2c44700fc245f5d55d5aab`.
  Independent post-download review verified all 522 ZIP entries, 521 checksum
  records, 520 manifest records, 519 SPDX file records, 23 outer evidence
  files, and all 18 successful validation stages.
- The fresh quality index accounts for all 409 tracked files: 164 production
  source files, 144 test files, 38 documentation files, and 63 other admitted
  regular files. Conservative file-level test and documentation evidence is
  present for all 164 production files, every one of the 775 exported Go
  declarations has an attached comment, and all 132 applicable Go/Python files
  have a profile. Those are evidence/applicability measures, not claims of
  semantic completeness. Actual executable statement/branch coverage is
  **29,247/32,438 units (90.16%)**, leaving 3,191 units across 112 exact gap
  records. Go coverage is 21,377/23,748 (90.02%); Python line-plus-branch
  coverage is 7,862/8,684 (90.53%).
- The self-catalogue selected all 409 regular tracked commit-tree blobs and no
  generated output. It contains 7,318 canonical records, plus two declared
  operational receipts; all 7,322 outer checksum records and every canonical
  manifest record were independently rehashed successfully. The generated
  knowledge base contains 10,375 nodes, 6,864 symbols, 31,487 relationships,
  deterministic Markdown and browser assets, a persisted lexical index, and
  14 NotebookLM sources totalling 39,923,874 bytes with a largest source of
  3,999,986 bytes.
- Independent privacy review found no personal developer paths, maintainer
  credentials, private keys, active session tokens, recursive RKC output,
  model weights, or private unrelated-project artifacts. GitHub secret scanning and push
  protection are enabled, and secret, code-scanning, and Dependabot alert
  inventories were empty at review time. These are strong scanner and manual
  review results, not a claim that pattern matching is complete DLP.
- The reference self-run deliberately used no compiler-generated SCIP index
  and no model: it records zero semantic parses and resolves
  9,922/31,487 relationships (31.51%). The compiler-grade self-run described
  in [`SHOWCASE_2026-07-27.md`](SHOWCASE_2026-07-27.md) imports a pinned
  scip-go index and resolves 126,418/148,917 relationships (84.89%), with 246
  Go files compiler-parsed. Generation and
  embedding defaults remain null because the required pair gate did not pass.
  Browser/assistive-technology acceptance, large-atlas sharding, native
  non-Linux packages, aggregate Python/model ceilings, signed provenance, and
  the remaining executable coverage queue remain explicit 1.0 gates.

## Earlier 2026-08-30 commit-bound acceptance evidence

That earlier manually reviewed complete evidence checkpoint is commit
`0d04bcdd386c494046f0e99297099dec2ee9736c`, tree
`cd7d1be961ab7835ad2d641961409d731cb9280b`. Its [main-branch CI
run](https://github.com/neuroforge-io/RKC/actions/runs/33303846351) passed the
guarded release verifier, two cache-isolated reproducibility builds, portable
target builds, Docker build/runtime smoke, self-catalogue, fresh Go/Python
profiles, quality index, and every artifact upload.

- The two complete-package builds produced the same 53,468,068-byte archive,
  SHA-256
  `3b4dbd6cb96004c5722d87f2ab5853f9eb4bc5f9a334c9e1d6a26853de973eab`.
- The quality evidence denominator contains 162 production source files. All
  162 have conservative file-level test and documentation evidence; all 131
  applicable executable files have a profile. Executable coverage is
  **28,553/31,635 units (90.26%)**, not 100%. Go exported-declaration
  documentation is **467/773 (60.41%)**, leaving 306 exact declaration gaps
  and 110 files with uncovered executable units. A complete audit of the same
  tracked tree partitions all 405 files into 304 analyzable source files (162
  production and 142 test), 38 documentation files, and 63 build,
  configuration, schema, data, license, lock, and support files outside the
  source analyzers.
- The self-catalogue selected 405 regular tracked blobs totaling 5,078,939
  bytes directly from that commit tree. It produced 7,989 atlas files, and all
  7,991 published checksum entries (canonical atlas files plus two outer
  receipts) were manually rehashed successfully. The two operational atlas
  receipts were validated but not part of that old checksum set; complete-pack
  hashing is therefore a required successor gate. Its safety receipt proves no
  generated output, model weight, symlink, or model execution entered the build.
- The generated knowledge base contains 13,898 nodes, 7,549 symbols, 37,085
  relationships, a 30 MB persisted lexical index, browser assets, deterministic
  Markdown, and six NotebookLM knowledge sources totaling 11,676,045 bytes;
  the largest pack is 3,999,948 bytes. It reports zero potential or
  high-confidence secret findings.
- The same review exposes the next quality work rather than hiding it: this
  no-SCIP self-run has zero compiler-semantic parses, resolves only
  10,965/37,085 relationships (29.57%), and finds source documentation on
  892/4,720 public symbols (18.90%). Importing a pinned compiler-grade
  scip-go index raises relationship resolution to 84.89% on the same source
  (see [`SHOWCASE_2026-07-27.md`](SHOWCASE_2026-07-27.md)). The human repository
  overview is useful
  but too terse for first-class onboarding. The standalone atlas also lost the
  outer Git identity, and the static browser in that evidence eagerly loaded a
  68.8 MB copy of the graph while ignoring the separate 30.3 MB lexical index.
  Successor code now keeps ordinary offline search and filtering on a compact,
  snapshot-bound exact-set node projection while reserving the full atlas for
  detail, graph, diagnostics, and deep links; successor artifact measurements
  and true large-export term/detail sharding remain required. Commit retention,
  complete-pack hashing, compiler-index integration, narrative overview
  quality, residual executable coverage, exported Go comments, payload
  efficiency, and reproducible browser/assistive-technology acceptance
  therefore remain explicit acceptance gates until successor evidence closes
  each one.

## Production acceptance checkpoint (updated 2026-08-30)

| Area | Current state | Production boundary |
|---|---|---|
| Scan journals | Implemented | Command outcome includes post-DAG policy and publication failures; resilient bounded list/show, symlink-safe owner-only state paths, hash-chain replay, crash-tail recovery, and Unix runtime/Windows protected-DACL contract tests are present. Explicit journal pruning and SQLite history projection remain additive roadmap work rather than correctness dependencies |
| OpenAPI JSON/YAML | Implemented | Common bounded reads, duplicate-key rejection, YAML depth/token/node limits, safe scalar normalization, escaped local references, cache invalidation, and cold/warm/edit parity are covered. Cross-file reference resolution remains a separate roadmap item |
| Workbench lifecycle and GUI | Core implemented on guarded Linux; explicit opt-in; browser acceptance partial | Submission deadlines, cancellation, bounded cleanup proof, truthful `cleanup_failed`, sanitized user-bus access, close/shutdown, and DELETE API are targeted-test green. One typed catalogue drives static/live command metadata, exact atlas/SQLite selectors, valid defaults, and the allowlist. The graphical folder picker runs exact `quickstart <folder>` analysis; before success, the server loads the generated owned atlas through the normal integrity/canonical/coverage checks, verifies publication and snapshot identity, and atomically swaps the immutable dataset plus dataset-aware command defaults. Failed validation keeps the prior atlas. Dataset API responses carry an immutable snapshot-generation identity, and the browser retries then visibly rejects a parallel bootstrap assembled across an activation boundary. `rkc open --workbench` enters the exact guard before scan, receives the one-time URL-fragment bootstrap through an owner-private readiness file, launches the browser from the outer process, and strips/consumes the fragment before session-token use. Every live server regenerates executable UI bytes from the current binary and validated bundle; whole-origin `no-store`, worker/manifest/form denial, and an ephemeral loopback port reduce imported or older same-origin browser-code risk without claiming that an OS-selected port can never be reused. Direct workbench serving requires `--ready-file` and rejects `--open`. Doctor/helper probes, custom Git/Python scan helpers, remote scan acquisition, custom quickstart configuration, and live Git history compilation remain terminal-only because per-job process-group cleanup cannot prove containment of detached descendants. The server periodically re-proves both the resource envelope and higher-priority admission. Read-only remains the safe default while aggregate nested-model ceilings and reproducible browser/assistive-technology acceptance remain required before default-on promotion |
| HTTP listener confidentiality | Implemented | Loopback is the fail-closed default; non-loopback read-only serving requires explicit `--allow-remote`, the workbench requires an ephemeral loopback origin, API responses use `private, no-store`, and every workbench-origin response is non-cacheable plus same-origin resource protected |
| Workbench containment | Implemented for the allowlisted command profile | One guarded server scope, single-job admission, per-command process groups, deadline/cancel TERM-to-KILL cleanup proof, and rejection of nested servers or model/Python commands that may create separately managed units prevent supported work from escaping. Unprovable cleanup fails visibly and blocks a success claim; nested managed runtimes remain disabled until an aggregate session ceiling is proved |
| Model default | Intentionally unset after complete qualification | Qwen3.5 4B Q4_0 improved to 4/6 generation cases with no unsupported claim or canary leakage, but over-abstained on the hostile-input case, timed out at exact 32K, and exceeded the strict process-RSS gate despite a safe 4.156 GB cgroup peak. Granite 4 H 1B and Qwen3.5 0.8B each passed only 2/6; Gemma 4 E2B and Qwen3.5 2B were also rejected. The paired Qwen3 embedding role passed all gates, but pair-level promotion correctly remained disabled |
| Release verification | Passed remotely and enforced on every `main` push | Main-branch CI run `33311156962` and CodeQL run `33311156881` passed on signed commit `bb5de8250df99c7ee1c2ce91633c136818e5adcc`. Two cache-isolated builds produced the identical 53,789,045-byte archive with SHA-256 `5c1532dd9e6baaddf3a226a4ddf572629ef6f6a07a2c44700fc245f5d55d5aab`; all 521 checksum records, 520 manifest records, 519 SPDX file records, 23 outer evidence files, and 18 release-validation stages were independently verified. The commit/tree-bound self-catalogue republished atlas, graph, lexical search, deterministic docs, browser assets, 14 NotebookLM sources, and 7,322 verified outer checksum records without recursive output, model execution, or model-weight ingestion |

Workbench parity is deliberately incomplete: trace capture, SCIP index
generation, model/Python execution, and nested servers remain blocked because
the current process-group boundary cannot prove containment of detached
descendants. Verification and import of existing evidence remain available.
Kernel-enforced per-job containment plus reproducible browser and assistive-
technology acceptance are release gates; no GUI success state is emitted for a
rejected operation.

## Core

| Capability | Status | Notes |
|---|---|---|
| Canonical RKR records | Implemented | Public Go package, schema 0.2.0 |
| Stable IDs and canonical ordering | Implemented | Deterministic digest tested |
| Referential and vocabulary validation | Implemented | Strict validation supported |
| Workspace privacy publication | Implemented | Default `paths-relative` removes persistent absolute repository/output paths while retaining relative citations and canonical origin; `redacted` also removes public origin/source-reference/repository-node provenance, then revalidates and rebuilds coverage before atlas, filesystem snapshot, or SQLite publication |
| Artifact inventory and SHA-256 | Implemented | Explicit exclusions and limits |
| Local/remote Git acquisition | Implemented | Promptless, hooks/global configuration disabled, bounded timeout/output, deny-by-default protocol policy, no plaintext `git://` or inline HTTPS credentials/query/fragment metadata, and one credential-free canonical origin across identity/model/export |
| Filesystem snapshot publication | Implemented | Building/committed states and recovery |
| Content-addressed object store | Implemented | Reference filesystem store |
| Transactional storage contract | Implemented | Typed reader/writer/recovery API; atomic, immutable in-memory conformance backend with authenticated cursors and lossless export |
| SQLite driver/bootstrap | Implemented | Pinned pure-Go driver, embedded digest-locked migrations through schema `0.5.0`, privacy-safe opaque repository affinity, fail-closed build/publication compare-and-swap, monotonic current-pointer guards, CGO-free build gates, reader-key initialization, read-only consumers, and strict database-open health checks |
| SQLite runtime writer/query layer | Implemented | Transactional staging/publication, OS writer leases, recovery, digest-verified canonical reads, exact coverage binding, authenticated pagination, projections, and CLI/HTTP/MCP integration |
| Pipeline DAG and cache library | Implemented | All 20 canonical scan stages route through the deterministic DAG with bounded resource admission; owner-only hash-chained command journals and ownership-bound verified CAS payloads provide selective keys plus `plan`/inspect/verify/prune UX. `plan` also exposes non-executing evidence opportunities for missing compiler, runtime, and history authority; it is not yet a question-driven acquisition loop. Retries, additional derived-output stages, and SQLite journal projection are future extensions, not hidden fallbacks |
| Clean/incremental equivalence | Implemented | Cold, warm, reversed-input, and localized-change paths are differentially checked against clean canonical output; the release benchmark and guarded RKC self-catalogue exercise repository-scale determinism |
| Live atlas load efficiency | Implemented and self-profiled | A one-core, nice-19, idle-I/O load of the 7,840-file RKC self-atlas now streams verified projection files through one reusable hash buffer, retains only the three canonical inputs used by the live server, and validates canonical ordering without cloning the decoded 62,585,787-byte bundle. Cumulative allocation fell from 1,727.15 MiB to 1,031.66 MiB (40.27%); a separate final run completed in 2.90 seconds with 612,504 KiB maximum RSS and no swap. Every file remains size/SHA-256 verified, imported executable assets remain untrusted, and wall time is intentionally secondary while ERAIS has priority |
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
| bounded interprocedural flow, runtime assertions, configuration, and history | Flow/config/history implemented; authenticated runtime observation planned | The `value-flow` stage compiles bounded call graphs, per-function Go CFGs, and value-flow edges (`flows_to`, `binds_to`, `returns_to`, `reads`) with package/type-authoritative sources and sinks. Basename sanitizer matches are confidence-0.25 non-authoritative hypotheses, never `sanitizes` truth or traversable protection. `rkc flow` reports origins, sinks, paths, environment readership, and the separate hypothesis list. Trace capture is guarded, digest-bound, and source-affine, but neither a self-hash nor same-process handling authenticates its producer. All current imports therefore remain confidence-0.5 `user_asserted` evidence and do not set canonical executed/test/call truth. Pre/post inventories detect endpoint drift but not transient ABA mutation. Go statement coverage has no call-event stream, so `rkc trace report` explicitly marks authenticated execution and call-edge observation unavailable. Aggregate coverage cannot attribute an ordered call path to one test, so none is invented. An attested producer-identity and isolation receipt remains a 1.0 gate. The `config-env` stage compiles Go build tags, CI workflows, Terraform declarations, and environment contracts without recording secret values or raw CI command bodies; 4 KiB per-text, 65,536-fact, 64 MiB retained-output, and bounded-diagnostic ceilings prevent repository-controlled text amplification. The `history-import` stage stamps symbol lifecycles and conservative `supersedes` rename refactors from `rkc history build`. All passes are bounded and deterministic |
| compiler-grade semantic adapters | Implemented through SCIP import and first-class generation | Streaming dependency-free SCIP ingestion preserves symbols, definitions, references, implementations, signatures, documentation, diagnostics, roles, and exact UTF-8/16/32 source ranges. Repeatable `--scip-index` integration is available in the CLI and workbench `quickstart`, `plan`, and `scan` workflows for Python, JavaScript/TypeScript, Go, C/C++/CUDA, Rust, Java/Kotlin/Scala, C#/Visual Basic, and any conforming producer. Only same-process generation by a digest-pinned, non-bypassed indexer is confidence-1 `compiler_resolved`; bare imports are source-bound but producer-unverified confidence-0.75 `syntax_inferred` evidence. Terminal `rkc scip generate` and `--scip-generate` hash document sources before execution, verify them after exit, embed the unchanged bytes in `Document.text`, and publish a validated digest-bound index. Every imported document requires matching intrinsic text; editable receipts cannot authorize no-text facts. GUI generation is fail-closed until detached-descendant cleanup is kernel-enforced; GUI verification, pinning, and existing-index import remain available. Every document must have a canonical repository-relative path, identify an inventoried text artifact, and declare position encoding 1 (UTF-8), 2 (UTF-16), or 3 (UTF-32); unsupported/unspecified encodings and out-of-repository documents fail closed. External symbol records remain imported metadata rather than authenticated repository truth. RKC deliberately does not download or execute indexers during normal scans |
| Tree-sitter universal host | Planned | grammar registry and queries specified |
| runtime evidence | Guarded capture and assertion import implemented; authenticated observation pending | Capture is opt-in, bounded, digest-bound, disabled during ordinary scans, and records only explicitly selected environment key names—never values. Every current import remains an operator assertion regardless of process locality. A producer-authenticated capture contract, transient-mutation isolation, temporal call/branch/value events, and per-test paths remain explicit 1.0 gates |
| active evidence acquisition | Opportunity planning implemented; closed loop planned | `rkc plan` reports whether compiler, runtime, and semantic-history authority is admitted and emits exact separately authorized next-command vectors. It does not yet create canonical uncertainty/request/attempt/result records or execute a question-driven acquire → recompile → reason loop |

## Knowledge products

| Product | Status | Notes |
|---|---|---|
| Canonical bundle and JSONL | Implemented | portable, deterministic |
| Markdown documentation | Implemented | deterministic facts and symbol pages |
| normalized source envelopes | Implemented | likely secrets redacted by default |
| NotebookLM pack | Implemented | Direct source-bound scans provide enforced byte-bounded grouping, deterministic source inventory, exact byte counts, complete admitted code/configuration/document bodies with paths and hashes, mandatory broad-export secret redaction, and a generated `UPLOAD.md` guide with grounding and quota-handling instructions. Stored-snapshot exports without the exact checkout are explicitly labelled metadata-only. The default hard per-pack limit is 4,000,000 bytes and an oversized record fails rather than being truncated |
| responsive browser and local workbench | Implemented core; browser acceptance partial | Accessible static default and explicit token-authenticated guarded loopback execution; a one-time fragment capability travels only through an owner-private readiness file and private redirect, is stripped before exchange, and cannot be reused. Ordinary static search/filtering loads a compact snapshot-bound exact-set node projection, while canonical detail, evidence, diagnostics, and graph data remain lazy. The typed CLI palette, dataset-aware exact argument arrays, bounded output, deadlines, cancellation, all terminal/cleanup states, and responsive desktop/mobile layout contracts are unit-tested. Vectors that could create separately managed model/Python units or invoke uncontained acquisition/history/custom helpers visibly fail closed. Workspace confinement, aggregate model ceilings, browser automation, assistive-technology acceptance, true large-export sharding/virtualization, guided forms, and live incremental job output remain open |
| ranked lexical search | Implemented | A direct source-bound export includes complete admitted secret-redacted repository text, carries an exact snapshot/corpus binding, recomputes postings before live use, and returns bounded Unicode-aware match-centred excerpts without limiting full-text matching. Build-time and streaming pre-decode budgets bound document text, documents, terms, postings, tokens, and serialized bytes; the persisted cap is 1.5 GiB and sharding beyond the envelope remains open. Stored-snapshot exports without source bodies remain metadata-only |
| semantic/hybrid query | Implemented, model-gated | Exact-qualified `llama.cpp` embedding path, corpus-bound vector receipts, deterministic lexical fusion, and GraphRAG expansion are complete. With no pair-qualified default, model-backed mode fails closed while lexical/FTS5/graph search remains fully available |
| FTS5 runtime search | Implemented | `query --database` ranks the committed snapshot through SQLite FTS5/BM25 with literal-token MATCH construction, deterministic ties and traces, typed failures, cancellation, field filters, UTF-8/result bounds, and shared semantic-fusion/GraphRAG expansion |
| graph paths, impact, SCCs, structural counterfactuals | Implemented | bounded in-memory graph operations; counterfactuals compare an immutable baseline with a derived omission view, carry evidence and truncation state, and are always non-authoritative |
| semantic diff | Implemented | conservative logical/signature comparison |
| guarded self-catalogue | Implemented | immutable commit-tree blob staging; recursive-output/model-weight exclusion; atomic complete publication and deterministic receipts |
| quality and delta index | Implemented | deterministic standard-library source/documentation inventory with SHA-256 metadata, conservative test/documentation associations, exact local-Go-parser coverage for exported production declarations, optional Go/Python profile mapping, and Git change triage; percentages are explicit evidence signals rather than semantic 100% claims, and Go parsing never imports, builds, or executes the indexed repository |
| embeddings | Implemented, model-gated | Exact asset/runtime qualification binding, vector receipt generation, CLI integration, and strict retrieval scoring are complete. The Qwen3 embedding candidate passed its isolated gate, but the required generation/embedding pair did not, so no default is selected |

## Model subsystem

| Capability | Status | Notes |
|---|---|---|
| bounded evidence packets | Implemented | coherent truncation and redaction |
| `llama.cpp` CLI provider | Implemented | fake-executable integration tested |
| pinned native `llama.cpp` bootstrap | Implemented | exact source digest, CPU-only portable/native profiles, guarded build |
| cgroup, priority, CPU-only and RSS policy | Implemented on Linux; portable analysis is explicitly unprotected elsewhere | On ordinary Linux, `open`, direct `quickstart`, and direct `scan` self-re-execute before repository or generated-state writes. An existing RKC unit with the default envelope or strictly smaller, internally consistent memory/swap/task controls is reused rather than creating a sibling allowance; larger, unlimited, under-64-MiB, or current-usage-violating controls fail closed. The source-checkout wrapper validates optional smaller soft, hard, swap, and Go-heap values before systemd is invoked. It also validates an optional 0-65,536 MiB host-wide Linux `MemAvailable` reserve. Protected direct commands and the workbench check a nonzero reserve before work and every second, fail closed when `/proc/meminfo` is unavailable or malformed, and cancel when a peer drives the host below the floor. The constrained-container exception requires cgroup-namespace root plus proven equal-or-tighter CPU, hard-memory, swap, task, weight, OOM, and per-thread scheduling controls; generic external cgroups are rejected. Default host units retain one CPU, weight 1, nice 19, idle I/O, 4 GiB pressure/4.5 GiB hard memory maxima, bounded swap/tasks, higher-priority pre-emption, cancellation/reap, and auditable cleanup. Higher-priority admission is policy-driven: the default `yield` policy runs inside the subordinate envelope while processes matching configured workload markers merely exist and refuses or cancels when their aggregate CPU load reaches the configured fraction of one core. The generic marker set is `torchrun,lm_eval`; host-specific workloads are configured only through `RKC_HIGHER_PRIORITY_MARKERS`. Fifty percent of one core is the default load threshold, and `RKC_HIGHER_PRIORITY_POLICY=refuse` restores strict refusal whenever a match is visible. `rkc doctor` validates and reports the active configuration. Both reused paths re-prove controls and current usage during work. Direct scans require final-effective Python or plugin disablement; direct quickstart rejects Python until an aggregate parent/adapter ceiling is proved. macOS and Windows retain deterministic portable analysis without claiming kernel cgroup or scheduling enforcement |
| claim/summary validation | Implemented | Atomic statements, citations, identifiers, certainty, inference policy, unsafe markup, and bounds are checked; free-form summaries are never published |
| grounded repository answers | Implemented | CLI uses bounded lexical/semantic/hybrid plus graph evidence, canonical re-resolution, validation, and abstention. Up to two sanitized retrieval-repair passes repeat the full validator under one deadline, retain a pass audit, select the strongest grounded attempt, and never ingest generated output. Qualified embedding/generation bindings are required for model-backed modes |
| real GGUF benchmark below 4.5 GiB | Fully qualified rejection; no promotion | Qwen3.5 4B Q4_0 completed the guarded pair gate with a 4.156 GB cgroup peak and 4/6 generation cases, but failed hostile-input grounding, exact-32K latency, and strict process RSS. Qwen3 Embedding 0.6B Q8_0 again passed recall, margin, norm, memory, and latency checks. The required pair failed, so defaults remain null |
| remote model providers | Planned | policy/egress controls required |

## Interfaces

| Interface | Status | Notes |
|---|---|---|
| CLI and guided terminal first run | Implemented | `wizard` (alias `tui`) is a dependency-free, line-oriented guide over the existing safe workflows: choose a folder, open the verified read-only browser, compile only, show complete help, or cancel. It handles EOF without starting work and does not claim full CLI parity. `open` (alias `start`) composes scan, strict checks, loopback serving, and optional desktop-browser launch. On Linux, these first-run scans self-admit, reuse only a kernel-proven exact RKC/private-container envelope, and continuously yield to configured higher-priority workloads; help remains local and guarded internal context calls do not recursively admit. Static analysis stays portable but explicitly lacks Linux kernel enforcement on macOS and Windows, while `--workbench` is opt-in and Linux-only |
| local read-only HTTP API | Implemented | Bounded loopback-first reads over validated filesystem or SQLite snapshots |
| OpenAPI parity validation | Implemented | Generated operation inventory and handler parity are contract-checked |
| MCP stdio server | Implemented | Dependency-light local tools over the same validated snapshot readers |
| Go read client | Implemented | Typed in-process read API |
| TypeScript/Python generated SDKs | Planned | OpenAPI remains the machine contract until generated clients are release-gated |
| IDE extensions | Planned | No editor-specific package is published |
| team service API | Planned | Local single-user operation is the supported trust boundary |

## Security and operations

| Capability | Status | Notes |
|---|---|---|
| repository code execution denied by normal scan | Implemented | Repository files are inert inputs; compiler/indexer execution is separately authorized |
| secret redaction in normalized export | Implemented | Pattern-based and fail-closed, not represented as complete DLP |
| bounded plugin stdout/stderr and timeout | Implemented | Exact byte and wall-clock limits |
| plugin manifests and lock digests | Implemented | Admitted built-ins are digest-bound |
| WASI capability enforcement | Planned | No unsupported third-party execution fallback |
| native-worker OS sandbox | Worker boundary implemented; public direct admission pending aggregate proof | The digest-pinned built-in Python adapter can run only through the fail-closed Linux user-systemd isolation boundary with bounded resources and a sanitized environment. Public `scan`, `quickstart`, `open`, and workbench paths currently keep it disabled until worker and parent can prove one aggregate ceiling. Third-party native execution and unsupported platforms are disabled rather than weakly sandboxed |
| OIDC/RBAC/tenancy/audit retention | Planned | Local single-user server only; no multi-tenant claim |
| per-binary Go-module SPDX SBOM | Implemented | Deterministic SPDX 2.3 JSON is generated for every Linux executable and independently rebound to its checksum, command, GOOS/GOARCH, normalized GOAMD64/GOARM64 target, default GOEXPERIMENT set, `GOFIPS140=off`, exact Go toolchain, immutable source commit/tree/time, module lock, canonical Go purls, and actual linked modules during packaging; audited declared expressions are retained and every unanalyzed package conclusion remains `NOASSERTION` |
| complete-distribution SPDX SBOM | Implemented | `SBOM.spdx.json` inventories substantive archive files, all four platform command components, and their linked Go modules; circular receipt files are explicitly excluded, the manifest hashes the SBOM, and final checksums hash both |
| release signing, container SBOM, provenance | Planned | No publication claim until signatures and attestations are generated and verified |
| Docker and CI reference files | Implemented | Scratch runtime and pinned CI actions; publication attestations remain separate |
| full logged release verification | Implemented | Every main push exercises the guarded release path before artifacts are accepted |

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

The index is now package-aware for file-level Go documentation, recognizes
explicit cross-language test-harness references, rejects symlinked output
ancestors, and imports the coverage gate's zero-statement/current-platform
metadata. A separate fail-closed Go AST pass lists every undocumented exported
production declaration by symbol and source coordinate without importing,
building, or executing repository code. It deduplicates Go profile blocks,
rejects inconsistent profile denominators and Python branch counters, and
retains exact uncovered Go coordinates and Python line/branch arcs for direct
test triage. The reviewed `0d04bcdd386c494046f0e99297099dec2ee9736c`
index reports 100% test and file-documentation evidence under the documented
heuristics, **206/206 public `pkg/*` exported Go declarations documented**, and
  **467/773 across all production Go code**. The later commit-bound checkpoint
  described at the top of this document reports **775/775 attached declaration
  comments (100%)**. This proves comment attachment, not prose correctness or
  semantic completeness. That later reviewed Go/Python profile covers
  **29,247/32,438 units (90.16%)**, leaving 3,191 exact uncovered units across 112
  gap records. Fresh CI profiles remain authoritative for executable coverage
  percentages and residual test gaps; no number here claims the uncommitted tree.

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

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
