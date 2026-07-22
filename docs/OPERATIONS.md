# Operations

## Deployment profiles

### Local static

```sh
rkc scan --out .rkc --force .
```

Produces a portable dataset and static site. No daemon, authentication, or model
is required.

### Local daemon

```sh
rkc serve --dir .rkc --addr 127.0.0.1:8787
rkc-mcp --dir .rkc
```

Intended for one trusted user and loopback clients.

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
