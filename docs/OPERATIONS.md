# Operations

Original RKC operations guidance is copyright 2026 **NeuroForgeIO** and
released under [Apache-2.0](../LICENSE); preserve the applicable license and
`NOTICE` terms when redistributing it.

## Deployment profiles

### Local static

```sh
rkc quickstart .
```

Produces and verifies a portable dataset and static site with the deterministic
no-Python profile. No daemon, authentication, model, or Linux-only worker
sandbox is required.

### Local daemon

```sh
rkc serve --dir .rkc --addr 127.0.0.1:8787
rkc-mcp --dir .rkc
```

Intended for one trusted user and loopback clients.

### Protected first run

```sh
rkc open /path/to/repository
```

On Linux the installed binary self-reexecutes through the exact one-core,
nice-19, idle-I/O, 4 GiB pressure / 4.5 GiB hard-memory envelope before any
atlas, cache, journal, or snapshot write. The outer guard continuously checks
for configured higher-priority workload classes under the active policy (the
generic default is `torchrun,lm_eval`;
`RKC_HIGHER_PRIORITY_MARKERS=training,benchmark` selects 1-16 unique lower-case
ASCII markers of at most 32 bytes each and 255 bytes total; empty retains the
default and invalid configuration fails closed). By
default `yield`: proceed inside the subordinate envelope while such work merely
runs, and stop promptly when its CPU load reaches the threshold; set
`RKC_HIGHER_PRIORITY_POLICY=refuse` for the strict refusal), proves cleanup, and
launches the
desktop browser outside the disposable RKC service. `--no-browser` retains the
printed URL for headless use. The default server is read-only and remains the
portable macOS/Windows first-run path.

For a trusted single-user Linux checkout, `rkc open --workbench <path>` is the
explicit interactive route. The guarded child writes its one-time URL-fragment
bootstrap only to an atomically published, owner-private readiness receipt. The
outer `open` process strictly validates that bounded receipt and launches an
owner-private redirect outside the cgroup; the browser removes the fragment
before its one successful exchange for a same-origin session token.

The workbench is deliberately a trusted-user command launcher, not a filesystem
sandbox. Its token grants commands the invoking account's file authority;
`--workspace` sets the working directory and guided defaults. Use static mode for
untrusted users, and launch a separate workbench for each trusted workspace.

Running `scripts/with-rkc-limits.sh ./bin/rkc serve --workbench ...` is a
low-level route for an existing atlas. Direct workbench
serving requires `--ready-file` beneath an owner-private directory, rejects
`--open`, and expects a trusted launcher to consume the receipt without logging
its `browser_url`. It requires port `0` so the OS selects an ephemeral loopback
origin instead of deliberately reusing the fixed read-only port; non-loopback
listeners fail closed. The OS can reuse a prior ephemeral port, so use a trusted
browser profile. Generated browser policy forbids registration of new workers
and manifests. The readiness receipt is the authority for the resulting address.
Commands that might create a separately managed Python/model unit or launch
acquisition, live-history, or custom helpers fail closed until the session can
prove one aggregate resource ceiling and descendant cleanup boundary.
Direct serving re-proves its resource envelope and checks higher-priority work
before and after atlas preparation and continuously while serving (load-gated
under the default `yield` policy;
strict refusal with `RKC_HIGHER_PRIORITY_POLICY=refuse`), but
`rkc open --workbench` is preferred when the outer monitor
must be able to terminate work during the initial atlas load itself.

### CI

Use an immutable build image, disable model synthesis, scan the checked-out
revision, run quality gates, publish the atlas as a build artifact, and compare
against the target branch where semantic diff policy is enabled.

### Team service

Planned production topology:

```text
API/control plane
  -> PostgreSQL
  -> object store
  -> durable job queue
  -> isolated analysis workers
  -> plugin registry
  -> OIDC/RBAC/audit
  -> metrics, traces, logs
```

The team service is not implemented in this reference release.

## Local directory layout

```text
.rkc-state/
├── objects/sha256/
├── snapshots/<snapshot-id>/
├── builds/<build-id>/
├── current
├── cache/
└── logs/
```

Published snapshots are immutable. The `current` pointer is atomically replaced
after validation.

## Scheduler run journals

Scans that pass configuration, acquisition, and path validation write one
append-only JSONL journal beneath the platform user-cache directory, normally
`$XDG_CACHE_HOME/rkc/runs` on Linux. The directory and records are owner-only.
A run ID is a random canonical 128-bit identifier; opening an existing
identifier is refused. The terminal run result is delayed until policy checks
and all configured SQLite, atlas, and snapshot publications finish.

```sh
rkc runs list
rkc runs list --limit 20 --json
rkc runs show '<run-id>'
```

Inspection is read-only and strict: record schema, run binding, sequence,
plan binding, lifecycle transitions, and the digest chain are replayed before
output is returned. Listing is bounded to 1–1000 newest candidates and reports
invalid or concurrently changing entries individually without hiding healthy
runs; `show` validates only the requested run. A crash-truncated final fragment is reported as an
interrupted nonterminal prefix; a complete malformed or checksum-invalid
record fails closed. `scan --runs-dir` selects an explicit owner-only location,
which must remain disjoint from the repository, generated atlas, snapshot
store, SQLite database, and stage cache.

Unix ownership bits are runtime-tested. The native Windows path applies a
protected current-user-only DACL and cross-compiles in CI-oriented checks; a
real Windows runtime gate remains required before calling that platform proof
complete.

## Backup

For the filesystem reference store:

1. stop writers or copy only committed snapshots;
2. copy snapshot manifests and current pointer;
3. copy referenced content-addressed objects;
4. verify object digests;
5. run `rkc snapshots list` and `rkc snapshots show` against the restore;
6. preserve configuration, plugin lockfiles, and toolchain descriptors.

Production PostgreSQL mode requires point-in-time recovery, object-store
versioning/replication, restore drills, and consistency checks between database
object references and stored blobs.

## Retention and garbage collection

Retention policy should distinguish:

- current snapshots;
- release or tagged snapshots;
- pull-request snapshots;
- failed build logs;
- model-derived output;
- raw runtime traces;
- audit records.

Content-addressed objects are reclaimed only after a mark phase confirms no
retained snapshot, export, cache entry, or audit hold references them.

## Health and readiness

Local health reports process and dataset state. Production readiness additionally
requires database connectivity, object-store access, migration status, queue
availability, plugin registry status, worker capacity, and policy load status.

A worker that cannot enforce its configured sandbox must fail readiness rather
than silently downgrade containment.

## Observability

Production telemetry should include:

- acquisition, inventory, parse, merge, validation, export, and publication
  duration;
- files/bytes/symbols/edges per stage;
- cache hit/miss and invalidation causes;
- plugin timeout, memory, output, and diagnostic counts;
- unresolved edge and parse coverage ratios by language;
- queue latency and worker saturation;
- model packet size, latency, peak RSS, and claim acceptance;
- API latency, result counts, and bounded-traversal truncation;
- snapshot publication failures and recovery actions;
- tenant quota usage and denied actions.

Repository paths and source text should not become telemetry labels.

## Upgrades and migrations

1. read current schema metadata;
2. take a verified backup;
3. run idempotent forward migrations in a transaction where supported;
4. validate canonical invariants;
5. rebuild derived FTS/vector/browser projections;
6. run compatibility fixtures;
7. publish the new application version;
8. retain rollback binaries until the migration’s rollback window closes.

Canonical schema changes require explicit migration and compatibility policy.
Derived index formats may be deleted and rebuilt.

## Incident classes

- **source confidentiality**: possible secret/source egress;
- **canonical integrity**: corrupted or forged records/evidence;
- **plugin escape**: worker exceeded declared capabilities;
- **availability**: queue, database, object store, or worker exhaustion;
- **supply chain**: compromised binary, plugin, image, or model;
- **tenant isolation**: cross-workspace access;
- **documentation integrity**: published unsupported model claims.

Incident response must preserve snapshot IDs, plugin/model digests, audit logs,
worker logs, acquisition metadata, and release provenance.

## Release procedure

The reference release path is:

```sh
make safe-complete-package
```

The coherent output generation is `dist/release`; it contains the ZIP, binaries,
demo, and the exact receipt-bound validation/benchmark evidence. Production
publication still adds signed checksums, provenance, container
multi-architecture builds and SBOMs, vulnerability scanning, and a
clean-environment installation test.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
