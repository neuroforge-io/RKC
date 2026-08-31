# Quickstart

## 1. Install

The shortest source-checkout path is:

```sh
git clone https://github.com/neuroforge-io/RKC.git
cd RKC
./install.sh
export PATH="$HOME/.local/bin:$PATH"
rkc wizard
```

The installer builds both CGO-free binaries, installs them under
`$HOME/.local/bin` by default, preserves the Apache-2.0 license and NOTICE
under `$HOME/.local/share/doc/rkc` (including the complete third-party notice),
and prints an immediately executable first-run command plus a copy-ready PATH
command when the install directory is not already reachable. Use
`./install.sh --prefix /another/prefix` for another user-owned destination.
Original RKC code and generated tooling are copyright 2026 **NeuroForgeIO**;
see [`BRANDING_AND_ATTRIBUTION.md`](BRANDING_AND_ATTRIBUTION.md)
for the commercial attribution and third-party license boundary.
Existing symbolic-link or non-file destinations are rejected.

`rkc wizard` (alias `rkc tui`) is a dependency-free terminal guide. It asks for
the folder and offers only three bounded first-run choices: build and open the
read-only local browser, compile and verify without a server, or print the full
CLI help. It also offers an explicit cancel choice, and EOF starts no work. The
wizard calls the existing `open` and `quickstart` implementations directly; it
does not invoke a shell or claim to expose every CLI option. You can still run
either command below directly when you already know which workflow you need.

`rkc open /path/to/repository` produces an atlas, retains immutable snapshot
state, runs the strict local integrity and quality checks, and prints the local
URL while opening the read-only browser when a desktop opener is available.
On an ordinary Linux host, `open`, direct `quickstart`, and direct `scan`
self-admit to RKC's exact one-core, lowest-priority, 4 GiB pressure / 4.5 GiB
hard ceiling before any atlas, cache, journal, or snapshot write. An existing
exact RKC unit is reused; the scratch-container path instead requires a private
cgroup root to prove an equal-or-tighter hard ceiling, swap/task/weight/OOM
controls, and lowered per-thread scheduling. Both paths continuously recheck
the envelope and yield to configured higher-priority workload classes. The
generic marker set is `torchrun,lm_eval`;
`RKC_HIGHER_PRIORITY_MARKERS=training,benchmark` replaces it with 1-16 unique
lower-case ASCII classes, each at most 32 bytes and 255 bytes total. Empty
retains the default and invalid configuration fails closed. The default `yield`
policy keeps RKC subordinate while such work merely runs and cancels promptly
when its aggregate CPU load reaches the configured threshold, while
`RKC_HIGHER_PRIORITY_POLICY=refuse` restores the strict refusal whenever any
higher-priority process is visible. Press Ctrl-C to stop
cleanly, or pass `--no-browser` on a headless host.
It safely replaces only a previous RKC-owned atlas. The portable default does
not invoke Python. Protected `open` and direct `quickstart` reject `--python`
until its separately sandboxed adapter and parent scan can prove one aggregate
ceiling; deterministic built-in analysis remains available.

For a trusted single-user Linux checkout, `rkc open --workbench
/path/to/repository` explicitly enables the protected local command center.
The default static browser remains portable on macOS and Windows; their
interactive workbench stays disabled until equivalent native resource
admission is proved. In workbench mode the guarded child publishes a one-time
URL-fragment capability only through an owner-private readiness file. The outer
`open` process validates the receipt, launches the desktop browser outside the
cgroup through an owner-private redirect, and the browser removes the fragment
before exchanging it once for its same-origin session token. The OS selects a
ephemeral loopback port for every `open` session, avoiding deliberate reuse of
the familiar fixed port. The OS may still reuse an earlier ephemeral port, so
this is risk reduction rather than proof of a new origin; use a trusted browser
profile. Browser policy forbids registration of new workers and manifests. The
advanced direct `serve` command retains its explicit address option.

Once open, use **Browse** to choose a folder and **Analyze this folder** to
compile it. Success is reported only after the generated `<folder>/.rkc`
dataset passes the normal canonical, coverage, ownership-marker, and manifest
checks. RKC then switches every graphical read view and dataset-aware command
default in one operation. A failed or corrupt analysis leaves the prior atlas
active.

For a compile-only first run, use `rkc quickstart /path/to/repository`; it
performs the same scan and quality gates without starting a server. A direct
advanced scan must include an explicit final `--no-python=true` or
`--no-plugins=true` setting before the repository path; `--no-python` is the
equivalent shorter true form. Both commands accept any local folder, including
folders that are not Git worktrees. macOS and Windows use the same deterministic
safety preflight and portable analysis, but cannot claim Linux's kernel cgroup
or scheduling enforcement.

## 2. Development prerequisites

For a source checkout on Linux or macOS:

```sh
go version       # use a currently supported release; CI pins 1.26.5
python3 --version # 3.11 or newer
git --version
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements-dev.txt
```

The virtual environment keeps the pinned validation dependencies out of the
system interpreter. Prebuilt binaries need none of these tools for a local
`--no-python` scan. Native Windows users can build the two Go commands and use
that portable profile, while guarded development/release automation and the
implemented Python worker boundary require Linux; WSL2 is the supported Windows
route for those Linux-only paths. Public direct analysis currently keeps that
worker disabled until it can share one proven aggregate ceiling with its parent.

## 3. Verify the checkout

```sh
make safe-verify
```

This runs formatting, vetting, Go and Python tests, contract validation,
document-link validation, plugin-lock verification, a mixed-language scan,
deterministic replay, HTTP API smoke tests, MCP smoke tests, and remote-Git
acquisition tests.

`safe-*` targets are intentionally Linux-specific: they require a reachable
user-systemd manager and delegated CPU, memory, I/O, and process controllers.
Run `make build` for an unguarded portable build. Run `./bin/rkc doctor` after
building to inspect the worker boundary and remote-Git prerequisites; a passing
worker diagnostic does not override the current direct-command
aggregate-ceiling gate.

Run the race detector separately or use the logged release sequence:

```sh
make safe-test-race
make safe-release-verify
```

## 4. Build

```sh
make build
./bin/rkc version
./bin/rkc doctor --repository .
```

On a supported Linux user-systemd host, `make safe-build` provides the same
binary build under RKC's deliberately subordinate resource envelope.

To verify the CGO-free commands compile for the maintained desktop/server
targets without publishing anything, run `make portable-build`. This checks
Linux, macOS, and Windows on `amd64` and `arm64`; the temporary binaries are
removed after the check. The complete reproducible package currently publishes
Linux `amd64`/`arm64` artifacts, while the portable source build contract keeps
the command layer ready for native packaging on the other targets.

## 5. Generate configuration

```sh
./bin/rkc init --path rkc.json
```

Edit `rkc.json`, then pass it with `--config rkc.json`. Omit the option to use
safe local defaults. The older `--out` spelling remains a compatibility alias;
`--path` is the canonical flag.

The generated configuration defaults to
`workspace.privacy_mode: "paths-relative"`. Atlas and durable snapshot records
then keep repository-relative citations and a credential-free remote origin,
but do not retain absolute repository or output locations. Select `redacted`
when the public knowledge product must also omit the Git origin and source
reference; opaque stable IDs still support deterministic search and graph
links. Select `full` only when retaining machine-local operational paths in
durable state is intentional. These modes do not weaken the independent secret
scanner or normalized-source redaction.

`inventory.exclude` values are exact repository-relative paths, not globs. Each
value excludes that path and its descendants. RKC does not claim to interpret
`.gitignore`; its generated configuration instead lists explicit safe defaults
for virtual environments, local RKC model/runtime outputs, the default
`.rkc-trace.json` and `.rkc-history.json` evidence files, `bin`, `dist`, and
named root-level coverage/cache outputs. Add another exact path with a repeated
`--exclude` flag when scanning.

## 6. Scan a repository

Start with the portable deterministic profile:

```sh
./bin/rkc plan \
  --config rkc.json \
  --no-python \
  /path/to/repository

./bin/rkc scan \
  --config rkc.json \
  --no-python \
  --out /tmp/my-atlas \
  --state-dir /tmp/my-atlas-state \
  --force \
  /path/to/repository
```

`rkc plan` performs inventory and normalization only, then reports the complete
20-stage DAG, verified cache hits, misses, disabled stages, and invalidation
reasons. Analyzer payloads are stored outside the repository in the operating
system's user-cache directory. Use `scan --no-cache` when an explicitly clean
run is required; clean and incremental execution produce the same snapshot
identity and canonical digest. The scheduler admits concurrent stages within
the `--stage-workers` and `--stage-memory-mib` bounds; the safe defaults are
four workers and a 2048 MiB aggregate admission budget.

The plan also lists evidence opportunities for missing compiler semantics,
runtime capture assertions, and semantic history. It labels compiler-authenticated
inputs separately from structured assertions. These are exact next-command
argument vectors with authorization requirements; planning never executes the
indexer, tests, or Git-history compiler itself. Supplying `--scip-index`,
repeatable `--trace`, or `--history` validates those inert inputs and marks the
corresponding opportunity admitted.

Each scan that reaches DAG execution durably records its scheduler and final
publication outcome in the operating system's owner-only user-cache location,
and prints both `run_id` and `run_journal` in JSON mode. The journal is outside
the repository and atlas, is never overwritten, and is closed and strictly
replayed before a successful scan returns. Inspect it with:

```sh
./bin/rkc runs list
./bin/rkc runs show '<run-id>'
./bin/rkc runs show --json '<run-id>'
```

Use the same `--runs-dir /owner-only/path` override on `scan`, `runs list`, and
`runs show` when durable operational state must live somewhere other than the
platform user-cache directory.

This still performs deterministic Go and JavaScript/TypeScript syntax
analysis, framework and document extraction, secret-pattern detection, graph
construction, search indexing, and every configured export. Direct `scan` must
retain `--no-python` (or explicitly set `--no-plugins`) even if
`./bin/rkc doctor --strict --config rkc.json --repository /path/to/repository`
passes. Direct `quickstart` likewise rejects `--python`. RKC does not enable the
built-in Python adapter in these direct workflows until its separately managed
unit and the parent scan can prove one aggregate resource ceiling, and it never
falls back to running that adapter without its isolation boundary.

### Add compiler-grade semantics

Generate a SCIP index with the appropriate compiler-backed indexer in a
separately authorized build environment, then import it as inert data:

```sh
./bin/rkc plan \
  --scip-index /path/to/index.scip \
  --no-python \
  /path/to/repository

./bin/rkc scan \
  --scip-index /path/to/index.scip \
  --no-python \
  --out /tmp/my-atlas \
  --state-dir /tmp/my-atlas-state \
  --force \
  /path/to/repository
```

Repeat `--scip-index` for a polyglot repository. RKC imports compiler-resolved
symbols, definitions, references, relationships, signatures, documentation,
diagnostics, and exact source ranges for Python, JavaScript/TypeScript, Go,
C/C++/CUDA, Rust, Java/Kotlin/Scala, C#/Visual Basic, and other conforming SCIP
producers. It does not run the indexer or repository build. Full setup,
language routes, GUI usage, and security limits are in
[`SCIP_SEMANTIC_ADAPTERS.md`](SCIP_SEMANTIC_ADAPTERS.md).

Inspect or maintain the cache without scanning:

```sh
./bin/rkc cache inspect --verify
./bin/rkc cache verify
./bin/rkc cache prune --older-than 720h --dry-run
```

`cache prune --all` requires `--yes`; every prune mode supports `--dry-run` and
machine-readable `--json` output.

`--state-dir` must be missing, empty, or already marked as an RKC snapshot
store. RKC refuses to adopt arbitrary nonempty directories as transaction
state.

For the durable SQLite runtime, create an owner-only database directory and use
`--database` instead of `--state-dir`:

```sh
install -d -m 700 /tmp/rkc-store
./bin/rkc scan \
  --config rkc.json \
  --no-python \
  --database /tmp/rkc-store/rkc.sqlite \
  --out /tmp/my-atlas \
  --force \
  /path/to/repository

./bin/rkc snapshots list --database /tmp/rkc-store/rkc.sqlite --limit 20
./bin/rkc query --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>' authentication
./bin/rkc serve --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>'
./bin/rkc-mcp --database /tmp/rkc-store/rkc.sqlite --snapshot '<snapshot-id>'
```

Use the snapshot ID printed by `scan`, or select one repository's current
snapshot with `--repository`. Database readers open the existing file in
read-only mode and reject missing files, mixed selectors, and paths with unsafe
ownership or permissions.

Remote Git repositories are materialized without prompts or hooks:

```sh
./bin/rkc scan \
  --no-python \
  --ref main \
  --clone-depth 1 \
  --out /tmp/remote-atlas \
  --force \
  https://example.invalid/organisation/repository.git
```

Credentials should be supplied through an approved Git credential helper, not
embedded in URLs or configuration files.

## 7. Enforce quality

```sh
./bin/rkc check \
  --coverage /tmp/my-atlas/coverage.json \
  --bundle /tmp/my-atlas/bundle.json \
  --min-inventory-accounting 1 \
  --min-symbol-evidence 1 \
  --min-edge-resolution 0.5 \
  --min-claim-citation 1 \
  --max-errors 0 \
  --max-high-confidence-secrets 0
```

Edge resolution depends on analyzer precision. The reference syntax adapters
intentionally retain unresolved relations; lower the threshold for dynamic or
unsupported codebases rather than falsifying the denominator.

## 8. Search and browse

```sh
./bin/rkc query --dir /tmp/my-atlas --limit 20 authentication
./bin/rkc query \
  --database /tmp/rkc-store/rkc.sqlite \
  --snapshot '<snapshot-id>' \
  --limit 20 \
  authentication
./bin/rkc serve --dir /tmp/my-atlas --addr 127.0.0.1:8787
```

Database queries use the snapshot-bound SQLite FTS5/BM25 projection rather
than rebuilding lexical rankings in memory. Query text is treated as bounded
literal terms, while `--kinds`, `--languages`, `--objects`, and `--path-prefix`
provide explicit filters. Hybrid mode fuses the same FTS result with the
qualified vector index before bounded graph expansion.

The static site is also available directly under `/tmp/my-atlas/site`. It loads
a compact snapshot-bound overview first. The first search or filter loads only
the compact, exact-set node search projection; the complete offline graph stays
lazy until a deep link, diagnostic, symbol detail, or graph navigation needs
canonical details and evidence.

The responsive GUI covers repository overview, bounded search, entity and
evidence inspection, graph navigation, diagnostics, coverage, and the bounded
command catalogue. Normal serving and the default `rkc open` mode remain read-only.
The protected workbench executes only its explicit allowlist; server lifecycle
and separately managed model or Python operations stay in their guarded CLI
paths.
For a trusted single-user Linux repository, the preferred opt-in command center
is:

```sh
rkc open --workbench .
```

Use the folder picker to select another folder available to the invoking
account and run Analyze. The
same live page then changes to the verified new snapshot—Overview, Search,
Graph, details, and command defaults cannot diverge onto different datasets.
Activation failure is terminal for that job and does not replace the previous
snapshot.

For an already-built atlas, use this low-level advanced route.
Direct `serve --workbench` requires a nonexistent readiness path in an
owner-private directory and cannot be combined with `--open`; a trusted
launcher must consume the receipt without logging its `browser_url`:

```sh
rkc_ready_directory=$(mktemp -d)
chmod 700 "$rkc_ready_directory"
scripts/with-rkc-limits.sh ./bin/rkc serve \
  --dir /tmp/my-atlas \
  --addr 127.0.0.1:0 \
  --workbench \
  --workspace . \
  --ready-file "$rkc_ready_directory/ready.json"
```

The workbench refuses fixed-port or non-loopback binding and refuses to start
outside the protected cgroup. It authenticates same-origin requests through the one-time
bootstrap, invokes RKC directly without a shell, serializes jobs, and bounds
both duration and captured output. Commands that could create a separately
managed Python or model unit fail closed until an aggregate session ceiling is
proved; use their normal guarded CLI entry points in the meantime.
This direct route rechecks higher-priority work before and after atlas
preparation and while serving, refusing or stopping under the configured
policy (load-gated by default, strict with `RKC_HIGHER_PRIORITY_POLICY=refuse`).
Prefer `rkc open --workbench` when the outer monitor must pre-empt even
during the initial atlas load.

## 9. Use MCP

```sh
./bin/rkc-mcp --dir /tmp/my-atlas
```

Example initialization request:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
```

## 10. Upload the wiki-style knowledge pack

Every successful scan writes three complementary views of the same immutable
snapshot:

- `docs/` — linked Markdown pages suitable for a repository wiki or static
  documentation site;
- `site/` — the responsive static browser served read-only by ordinary
  `serve`/`open`, or augmented by the explicitly protected Linux workbench; its
  bounded bootstrap avoids making the complete graph an initial page-load tax;
- `notebooklm/` — ordered Markdown sources plus `manifest.json` and
  `UPLOAD.md` for NotebookLM-style notebooks and agent context windows. For a
  direct scan with an exact verified source checkout, the
  `05_repository_sources_*.md` files contain every admitted textual code,
  configuration, and documentation body with its repository path and hashes;
  probable credential material is always redacted in these broad-use packs.
  An export from a stored snapshot without that checkout is explicitly marked
  metadata-only in both `UPLOAD.md` and `manifest.json`.

Open `notebooklm/UPLOAD.md` first. It records the exact source count, byte
sizes, recommended upload order, grounding rules, and how to coalesce packs
with `--notebook-pack-bytes` when a service imposes a source-count limit. The
exporter never silently truncates a record. The same verified, redacted bodies
feed the snapshot-bound lexical index, so terminal and live-Web-UI searches can
find terms that occur only inside repository files. Search results bound the
returned body around a matched term while retaining full-text matching. The
4,000,000-byte default is an enforced per-pack limit: if one record cannot fit,
export fails with an actionable error instead of exceeding the configured
limit. Index construction and live loading also enforce aggregate text,
document, term, posting, token, and streaming pre-decode budgets; persisted
indexes above 1.5 GiB fail closed. Larger corpora require the still-planned
sharded index. Google maintains NotebookLM's
supported source types and plan-specific quotas in its
[NotebookLM help center](https://support.google.com/gemininotebook/answer/16215270).

The default pack target is 4,000,000 bytes: large enough to keep ordinary
repositories to a small source set while remaining comfortably below common
per-file limits. Verify `manifest.json` after changing the target or uploading
to another notebook provider.

## 11. Construct model evidence packets

Packet-only mode is useful even without a model:

```sh
./bin/rkc synthesize \
  --dir /tmp/my-atlas \
  --repo-root /path/to/repository \
  --query authentication \
  --task module_summary \
  --packet-only \
  --limit 5 \
  --force
```

The default destination is the deterministic sibling
`/tmp/my-atlas.rkc-derived/synthesis/<profile>`, never a directory inside the
verified atlas. An explicit `--out` must also resolve outside the atlas.

With `llama.cpp`:

```sh
make model-lock-check
make model-runtime-native
make model-fetch-generation
```

These explicit commands use the checked-in byte/digest/license lock and the
low-priority resource guard. They do not make the locked generation candidate a
default; a real guarded qualification and manual receipt review are still
required. See [`MODEL_RUNTIME.md`](MODEL_RUNTIME.md) for the portable build,
embedding candidate, and qualification commands.

```sh
./bin/rkc synthesize \
  --dir /tmp/my-atlas \
  --repo-root /path/to/repository \
  --query authentication \
  --model /models/coder-q4.gguf \
  --llama-cli /usr/local/bin/llama-cli \
  --context 4096 \
  --max-output 768 \
  --max-rss-mib 4608 \
  --limit 5 \
  --force
```

RKC rejects claims that cite unavailable evidence, reference unknown code
identifiers, contain multiple statements, omit certainty, or violate packet
policy. `rkc answer` uses two bounded repair passes by default: rejected text can
only become a sanitized retrieval query, while the final claims must cite
canonical evidence selected during an independently validated pass. Use
`--repair-passes 1` for a lower-latency single repair.

On Linux, model execution additionally fails closed unless it can enter a
low-priority user cgroup. It is CPU-only by default, limited to one CPU core at
the cgroup boundary, runs at nice level 19 with idle I/O priority, and receives
a hard memory limit derived from `--max-rss-mib`.

## 12. Compare snapshots

```sh
./bin/rkc diff /tmp/atlas-before /tmp/atlas-after
```

Use graph commands to inspect a changed node’s impact:

```sh
./bin/rkc impact --dir /tmp/atlas-after --node '<node-id>'
```

## 13. Produce the complete distributable

```sh
make safe-complete-package
```

The package builder refuses to proceed without release verification and two
cache-isolated, byte-identical builds. The coherent output is under
`dist/release`.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
