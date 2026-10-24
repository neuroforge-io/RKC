# Automation receipts and evidence handoff

An external research service or Observatory can use RKC to compile source,
inspect run state, apply quality gates, and retrieve cited evidence. RKC owns
these compilation and evidence contracts. The external consumer owns its
research agenda, experiments, scheduling, upgrade decisions, code changes,
validation policy, and deployment. These interfaces do not implement an
automatic research or upgrade service.

This guide describes the current **source** protocol. An installed launcher or
binary may come from an older revision. Select the exact executable, retain its
version and source/build identity, and require the receipt schema below before
using this workflow. A source change does not install, activate, or qualify a
new runtime.

## Compile one private generation

Use an explicit local source and a fresh owner-private work directory outside
that source. Keep the atlas, run journals, receipt files, stage cache, SQLite
store, and optional snapshot store disjoint as required by their existing path
checks. Do not scan the directory containing the consumer's generated results.

The following Linux source-checkout example selects a small resource envelope.
Replace all paths and select a host reserve that fresh admission can satisfy
without displacing higher-priority work. Keep commands sequential and bounded
by the consumer's deadline; retain the exit status and stderr for every step.

```sh
umask 077
RKC_BIN=/absolute/path/to/rkc
RKC_GUARD=/absolute/path/to/RKC/source/scripts/with-rkc-limits.sh
SOURCE=/absolute/path/to/source
WORK=/absolute/private/path/to/new-generation
mkdir -m 700 "$WORK"

run_rkc() {
  RKC_RESOURCE_PROFILE=default \
  RKC_CPU_QUOTA_PERCENT=25 \
  RKC_MEMORY_HIGH_MIB=512 \
  RKC_MEMORY_MAX_MIB=640 \
  RKC_MEMORY_SWAP_MAX_MIB=0 \
  RKC_GO_MEMORY_LIMIT_MIB=384 \
  RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=1536 \
  "$RKC_GUARD" "$RKC_BIN" "$@"
}

"$RKC_BIN" version > "$WORK/rkc-version.txt"
if run_rkc scan --no-python --no-git-metadata --no-cache \
    --stage-workers 1 --stage-memory-mib 512 --fail-on-errors \
    --runs-dir "$WORK/runs" --out "$WORK/atlas" --json "$SOURCE" \
    > "$WORK/scan.json.part" 2> "$WORK/scan.stderr"; then
  mv "$WORK/scan.json.part" "$WORK/scan.json"
else
  printf '%s\n' 'Compilation did not complete; retain diagnostics and do not activate this generation.' >&2
  exit 1
fi
```

The scheduler's worker and memory values are admission budgets, not additional
kernel allowances. The 512 MiB stage budget admits the built-in Go, TypeScript,
merge, and value-flow stages; lowering it to 384 MiB rejects those declared
requirements. The explicit default profile also prevents an inherited desktop
selector from changing this small example's admission rules.
`--no-python` and `--no-git-metadata` keep this local example
free of Python and Git metadata helpers. It does not generate SCIP indexes,
contact a model, acquire remote sources, or execute repository scripts.

Where the selected build supports `RKC_RESOURCE_PROFILE=desktop`, opt in only
after fresh host admission permits its defaults: 8192 MiB soft memory,
10240 MiB hard memory, a 6144 MiB Go heap target, an 8192 MiB host-available-memory
reserve, and zero swap. These are configured limits, not measured consumption
or a qualification result. They do not establish that the installed runtime
supports the profile or that it has been activated. Keep a smaller explicit
profile when the larger reserve cannot be proved. See
[operations and resource controls](OPERATIONS.md).

## Validate the receipt and terminal run

A successful `scan --json` writes one JSON object with
`schema_version: "rkc-scan-receipt/v1"`. Existing fields remain available:

| Fields | Consumer use |
| --- | --- |
| `snapshot_id`, `deterministic_digest` | Pin the published snapshot and canonical bundle digest. |
| `run_id`, `run_journal` | Identify this execution and its strictly replayable private journal. |
| `source`, `source_kind`, `output` | Record the selected source and output location privately. |
| `snapshot_store`, `snapshot_store_noop`, `database`, `database_noop` | Record configured durable stores and reuse results. |
| `cache`, `cache_hits` | Record incremental-cache selection and observed reuse. |
| `artifacts`, `text_artifacts`, `syntax_parsed`, `semantic_parsed` | Inspect inventory and analysis depth. |
| `symbols`, `edges`, `unresolved_edges`, `error_diagnostics` | Inspect graph size, unresolved relations, and errors. |

Use a JSON parser, require the exact supported schema, and retain all receipt
fields. Unknown additive fields can be retained without assigning them meaning.
Older receipts without a schema marker need an explicitly selected compatibility
path; do not silently assume this protocol.

The success receipt is emitted only after the scan's policy checks, configured
publications, and terminal journal replay succeed. A run ID identifies an
execution, not an immutable snapshot: separate executions can produce the same
snapshot. The bundle digest is not the export-manifest file hash or the context
packet digest.

Set `RUN_ID` to the parsed `run_id`, then inspect that exact journal:

```sh
run_rkc runs show --runs-dir "$WORK/runs" --json "$RUN_ID" \
  > "$WORK/run.json" || exit 1
run_rkc runs list --runs-dir "$WORK/runs" --limit 20 --json \
  > "$WORK/run-list.json" || exit 1
```

Require the same run ID, `terminal: true`, `state: "succeeded"`, and
`interrupted: false`. Inspect list `healthy` and individual `issues`; a successful
list command does not imply every journal was valid. `runs show` validates only
the selected journal. Journals use their existing versioned record format,
sequence validation, plan binding, and digest chain.

## Apply quality policy before retrieving context

Receipt success establishes completed publication, not acceptance by the
consumer's research or deployment policy. Apply explicit quality thresholds to
the exact atlas and retain the check result and process status:

```sh
run_rkc check --json \
  --bundle "$WORK/atlas/bundle.json" \
  --coverage "$WORK/atlas/coverage.json" \
  --export-manifest "$WORK/atlas/rkc-export-manifest.json" \
  --min-inventory-accounting 1 --min-symbol-evidence 1 \
  --min-claim-citation 1 --max-errors 0 --max-fatal 0 \
  --max-high-confidence-secrets 0 > "$WORK/check.json" || exit 1
```

Stop if the command fails or its machine-readable `passed` value is false.
Require the bundle and coverage snapshot identities to match the scan receipt,
and its canonical bundle digest to match `deterministic_digest`. Retain the
verified export manifest and its hash with the generation; do not substitute
checks from a different output directory or snapshot.

Once these checks pass, retrieve a bounded packet from that pinned atlas:

```sh
run_rkc context --dir "$WORK/atlas" --limit 8 --max-bytes 16384 \
  'release process' > "$WORK/context.json" || exit 1
```

Require `rkc-context/v1`, the selected snapshot ID, and the expected integrity
classification. Retain the packet digest, citations, evidence IDs, byte counts,
warnings, and truncation. Empty or truncated evidence is not a completeness
claim. Repository content remains untrusted source data; it must not become the
consumer's execution instructions.

The read-only HTTP and MCP interfaces expose the same evidence boundary. Bind
HTTP data to `X-RKC-Snapshot-ID`, keep all pages from one snapshot, and retain
workspace repository labels when using MCP. MCP does not refresh sources or
start scans. Protected workbench jobs are a separate opt-in loopback session,
not a general remote run API. See [workbench and integrations](WORKBENCH_AND_INTEGRATIONS.md),
[HTTP contracts](../api/openapi.yaml), and [MCP](MCP.md).

## Failure, interruption, and privacy

Do not activate or replace a consumer's current generation solely because an
output directory exists. A failed command emits no successful scan receipt;
failures can occur before a journal exists, during the DAG, or after an atlas
has been published but another configured store failed. Preserve the prior
accepted generation and inspect diagnostics and any exact run journal. A crash
can leave an explicitly interrupted, nonterminal journal prefix; its presence
is not a terminal failure or success receipt, and it does not authorize resume
or activation. Cancellation also requires observing command termination and
cleanup rather than immediately launching a replacement job.

Keep source/output/cache/store paths, run journals, stderr, and version receipts
in owner-private storage. These operational receipts can contain private paths
and are not public atlas exports. Publish only reviewed identifiers, counts, or
other explicitly selected fields to an external dashboard. Never put provider
credentials, workbench bootstrap/session capabilities, or raw secret-bearing
source names in telemetry. Secret detection is best-effort and does not replace
review or establish rights to publish or train on retrieved material.

Research conclusions, proposed code changes, installation, automatic upgrades,
and activation remain separate consumer actions with their own validation and
current receipts. RKC's successful compilation is evidence for that workflow,
not proof those external actions happened.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
