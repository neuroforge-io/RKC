# Flow, runtime evidence, and configuration semantics

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors and Apache-2.0 licensed.

RKC unifies four kinds of repository evidence in one canonical graph:

1. **Static possibility** — bounded call graphs, per-function control-flow
   graphs (CFGs), and value-flow edges compiled from source.
2. **Runtime assertions** — bounded test capture records claimed spans and test
   results. A content digest and source affinity protect trace integrity, but
   neither same-process handling nor a self-hash authenticates the producer.
   Current trace imports therefore remain operator assertions rather than
   canonical runtime truth. Go statement coverage contains no call-event stream
   and demonstrates no call edge or per-test call path.
3. **Configuration and environment** — build tags, CI workflows, Terraform
   declarations, environment-variable contracts, and code-level environment
   reads.
4. **History** — semantic deltas compiled from Git: when symbols appeared,
   how signatures changed, and which refactors renamed interfaces.

The distinction between static possibility and operator assertion is the point:
a static call edge says "this call is possible"; a future authenticated call
event could say "this call executed under these conditions". Their current gap
identifies questions for investigation, not proof of dead code, missing tests,
rare execution, or environment-specific behavior.

## Value flow (`rkc flow`)

Every scan compiles a `value-flow` stage that produces:

- **Call graphs** from resolved syntax-tier call edges (Go AST and compiler-
  grade SCIP references), with exact call-site evidence.
- **CFGs** per Go function body: entry/exit blocks, branch blocks (if/switch/
  select/type-switch), loop heads and exits, returns, breaks, continues,
  fallthroughs, and explicitly marked unreachable blocks. Successor edges are
  `precedes` edges with a `kind` attribute.
- **Value flow**: parameter value nodes, assignment writes, literals, call
  results, computed expressions, return values, and interprocedural seams —
  `binds_to` (call argument to callee parameter), `returns_to` (callee return
  to call result), and `reads` (environment variable to reading function).
- **Sources and sinks**: only package/type-authoritative `os.Getenv` and
  `os.LookupEnv`, compiler-resolved `net/http` request readers, and parameters
  whose AST type resolves through an exact `net/http` import to `*Request` are
  deterministic sources. Package-qualified SQL operations, subprocess
  execution, and file writes are deterministic sinks. A basename such as
  `Query`, `Body`, or `Getenv` is never authority.
- **Sanitizer hypotheses**: names beginning with patterns such as `sanitize`,
  `validate`, or `normalize` are retained only as confidence-0.25
  `related_to` hypotheses with `non_authoritative=true`. They are not
  `sanitizes` facts and are never traversed as protection in a lineage path.

Everything is bounded, deterministic, and path-insensitive within a function;
interprocedural facts flow through the explicit binds/returns seams only. The
production ceiling is 4,096 analyzed Go functions, 16,384 aggregate CFG
blocks, 32,768 CFG edges, 16,384 value nodes, 32,768 value-flow edges, and
131,072 admitted call edges. A second whole-fragment guard caps generated fact
plus evidence records at 210,000 and conservatively estimated retained storage
at 256 MiB. Generated labels and attribute strings are digest-suffixed and
bounded to 512 bytes. The scheduler admits the pass as a 512 MiB stage, which
preserves the supported low-memory scan profile while serializing it against
other memory-heavy stages; the outer guard remains the aggregate hard ceiling.
Crossing any ceiling emits a stable `RKC-FLOW-20xx` diagnostic and stops the
affected analysis; it never silently expands the resource envelope.

### Commands

```sh
rkc flow report --dir .rkc
rkc flow origins --dir .rkc --node <function-or-value-id>
rkc flow sinks   --dir .rkc --node <function-or-value-id>
rkc flow path    --dir .rkc --from <id> --to <id>
rkc flow env     --dir .rkc --name DATABASE_URL
```

`origins` answers "where can this value originate?"; `sinks` answers "what can
this value reach?"; `path` finds a bounded value-flow route; `env` answers
"what calls what when this environment variable is set" by walking the reads
edges into the call graph.

## Runtime evidence (`rkc trace`)

RKC never turns an editable trace file or process-local marker into execution
truth. `rkc trace
capture` runs an explicitly authorized command (typically `go test ./...`)
inside the same low-priority envelope and configured priority-workload policy
as scans, and records:

- executed statement spans per artifact (Go statement coverage);
- per-test results, status, and elapsed time (Go test events);
- exit status, duration, and working directory;
- only environment-variable names explicitly selected with repeatable
  `--environment-key NAME` flags (never values; none are recorded by default);
- a repository ID, bounded physical-content digest, and exact Git commit when
  Git is available;
- canonical repository-relative coverage paths with the source file's exact
  size and SHA-256;
- digest-bound sources: the coverage profile and test-event stream.

Go test runs are automatically instrumented with `-covermode=set
-coverprofile=<temp>` and `-json`; explicit instrumentation flags are refused
rather than overwritten. RKC inventories repository content before and after
the authorized command and refuses to publish a trace when the two endpoints
differ. Endpoint equality does not prove that no transient mutation and
restoration (an ABA change) occurred while the command ran.

```sh
rkc trace capture --dir . --out .rkc-trace.json --environment-key FEATURE_FLAG -- go test ./...
rkc trace verify --trace .rkc-trace.json
rkc scan --trace .rkc-trace.json --no-python --out .rkc --state-dir .rkc-state .
rkc trace report --dir .rkc
```

`.rkc-trace.json` and `.rkc-history.json` are exact default inventory
exclusions. A follow-up scan therefore cannot silently ingest RKC's own
generated evidence files. Custom output names remain the operator's explicit
responsibility.

The `trace-import` stage binds the trace digest into the snapshot identity and
first requires the repository, content, commit, artifact path, source size, and
source SHA-256 identities to match the scanned snapshot exactly. A foreign or
stale trace fails before it can add evidence. This establishes integrity and
source affinity, not producer authenticity: anyone able to edit a trace can
recompute its public content hash, and process locality is not a cryptographic
or operating-system attestation. RKC therefore records all current matching
coverage and results as confidence-0.5 `user_asserted` evidence and exposes
their unverified runtime assertion IDs. They do not set a function's `executed`
state, count tests as observed, or become more authoritative merely because
capture and import occur in one process;

- **authenticated runtime observation remains planned**: promotion requires a
  separately specified producer-identity, isolation, and observation receipt;
  no current CLI, GUI, or embedding seam emits `runtime_observed` or
  `test_result` truth;
- **call-edge observation remains unavailable**: covering both a call-site span
  and its callee does not prove that edge executed. Resolved static edges remain
  explicitly undemonstrated until a future call-event source supports them;
- **test outcomes** remain separate evidence. Aggregate statement coverage
  cannot truthfully assign a call path to one test, so RKC does not invent
  per-test execution paths.

Coverage tools may emit module-qualified paths
(`example.com/module/file.go`). Capture resolves them once against its bounded
inventory and persists only the canonical repository-relative path. Import no
longer performs suffix guessing.

Trace schema 1.3 is deliberately fail-closed: older traces lack the current
repository/source-affinity and call-observation truth contract and must be
recaptured. Arbitrary executable basenames are
stored only as the `custom-command` class, never as operator-controlled text.
Package identities are strictly bounded and credential-shaped packages are
redacted. Dynamic Go subtest suffixes are collapsed into a marked
`redacted-subtests` aggregate; control characters and oversized identifiers are
rejected rather than serialized.

`rkc trace report` separates unverified runtime assertions from static call
possibility. With statement-only traces it reports authenticated execution and
call-event observation as unavailable. It does not label unresolved questions
as dead code, untested behavior, or demonstrated non-execution.

### Coverage gates

The snapshot coverage report retains runtime-input and call-observation fields,
but current assertion-only imports cannot populate authenticated execution or
non-execution coverage. Zero values are therefore availability signals, not an
observation-coverage claim. `rkc check` enforces the standard gates; add runtime
policies only after a future authenticated producer contract is implemented.

## Configuration and environment (`config-env` stage)

The `config-env` stage compiles build configuration into the same graph:

- **Go build tags**: `//go:build` and legacy `// +build` constraints become
  `build_target` nodes with sorted tag lists and raw constraint text, bound to
  their files by `builds` edges. The same file compiled with different tags is
  an explicit, queryable fact.
- **CI workflows**: `.github/workflows/*.yml`, `.gitlab-ci.yml`,
  `bitbucket-pipelines.yml`, `azure-pipelines.yml`, `buildkite.yml`, and
  `Jenkinsfile` become workflow, job, and step nodes with `configures` edges;
  `run` commands are represented only by a bounded class and
  domain-separated digest. YAML structure is approximated by indentation;
  command bodies are never copied into the atlas.
- **Environment blocks**: workflow- and job-level `env:` declarations become
  `environment_variable` nodes. Secret-like names (`token`, `password`,
  `api_key`, ...) are recorded by name only — values never enter the atlas.
- **Terraform**: `resource`, `variable`, and `output` declarations become
  `build_target`/`config_key` nodes with their type names.
- **Docker**: the manifest framework already records `FROM` stages,
  `container_image` nodes, exposed ports, and environment declarations.
- **Code-level reads**: the value-flow stage records `os.Getenv`-family reads
  as `reads` edges from environment variables to the reading function.

Config/environment extraction reads at most 1 MiB per configuration file and
retains no individual canonical string above 4 KiB. Its whole fragment is also
admitted through a conservative ceiling of 65,536 facts and 64 MiB of estimated
retained storage, including 512 bytes of structural overhead per fact. Crossing
the text or aggregate ceiling skips the unsafe fact, stops further files once
the aggregate budget is exhausted, and emits `RKC-CFG-3006` or
`RKC-CFG-3007`. Diagnostics themselves are capped at 256 plus fixed summary
records, so hostile repositories cannot replace graph amplification with
diagnostic amplification.

The question "what calls what in what environment when feature X is enabled"
is then a graph query: build tag nodes (feature X) → file → package → calls;
environment variable nodes → readers → callers.

## History (`rkc history`)

```sh
rkc history build --dir . --out .rkc-history.json
rkc history report --history .rkc-history.json
rkc history symbol --name Greet --dir .
rkc scan --history .rkc-history.json --no-python --out .rkc --state-dir .rkc-state .
```

`history build` walks the repository newest-first, materializes each changed
source file at its exact commit, runs the same deterministic syntax extractors
used by scans, and records:

- when each symbol appeared (`first_seen`) and was last observed (`last_seen`);
- every commit that touched it and every file it lived in;
- its signature history — each observed signature snapshot with its commit;
- conservative refactors: a deleted symbol and an added symbol of the same
  kind with the same normalized signature (renames preserve interface shape)
  form a `supersedes` pair.

The `history-import` scan stage binds the history digest into the snapshot,
stamps matching nodes with lifecycle attributes (`first_seen_commit`,
`last_seen_commit`, `touched_commits`, `history_files`), and adds `supersedes`
edges for rename refactors. Symbols that moved packages appear as new
lifecycles rather than forged identities.

## Honest boundaries

- Value flow is path-insensitive within a function; branches are compiled as
  CFG structure, not as value predicates.
- Statement coverage never marks a call edge observed. Call-edge observation
  requires direct call-event evidence that the current capture does not emit.
- Trace hashes and same-process handling protect content flow, not producer
  identity. All current capture/import paths remain operator assertions;
  authenticated observation promotion requires a future attested producer
  contract.
- Pre/post repository equality detects endpoint drift, not transient ABA
  mutation during execution.
- Trace admission caps total executed ranges across all artifacts, and import
  merges line intervals rather than expanding covered lines into map entries.
- Traces record environment variable NAMES only; captured output is bounded
  and never interpreted.
- Rename detection requires matching signatures; arbitrary renames with
  changed interfaces are recorded as separate lifecycles.
- Every analysis pass is bounded and deterministic: exceeding a bound produces
  an explicit diagnostic, never an unbounded traversal or a silent truncation.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
