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

On Linux the installed binary self-reexecutes through the protected
nice-19, idle-I/O envelope before any atlas, cache, journal, or snapshot write.
Its default limits are one core and 4 GiB pressure / 4.5 GiB hard memory;
a selected smaller host profile also applies to the installed launcher.
The outer guard continuously checks
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

For a source-checkout workload sharing a memory-constrained host, the wrapper
accepts a strictly smaller, fail-closed default profile. For example:

```sh
RKC_RESOURCE_PROFILE=default \
RKC_CPU_QUOTA_PERCENT=25 \
RKC_MEMORY_HIGH_MIB=512 \
RKC_MEMORY_MAX_MIB=640 \
RKC_MEMORY_SWAP_MAX_MIB=0 \
RKC_GO_MEMORY_LIMIT_MIB=384 \
RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=1536 \
scripts/with-rkc-limits.sh ./bin/rkc scan --stage-workers 1 --stage-memory-mib 512 --no-python /path/to/repository
```

The wrapper rejects non-integers, a hard ceiling below the soft ceiling, values
above the default 4 GiB / 4.5 GiB / 256 MiB maxima, and a Go heap target above
the soft ceiling. It also validates the optional host-wide reserve from 0
through 65,536 MiB. When nonzero, protected `open`, `quickstart`, `scan`, and
workbench execution fail closed before work and yield within one second if
Linux `MemAvailable` falls below that floor. The guarded binary re-proves
actual cgroup usage and accepts the smaller reserved unit; direct scan/open
admission reuses that unit rather than creating a sibling with the default
allowance. `rkc doctor` reports both the configured reserve and whether the
current host satisfies it.

`RKC_CPU_QUOTA_PERCENT` accepts integers from 1 through 100, defaults to 100,
and applies in both scope and service mode. A value of 25 caps the entire
workload at one quarter of a core, including child processes. Internal guarded
workers also inherit the selected profile when they need a separate service;
an explicit worker memory allowance cannot enlarge the selected host ceiling.
Smaller per-worker allowances retain their own lower hard limit. A reused RKC
unit must prove controls no higher than the selected profile.
Sibling worker services bind their lifetime to an outer RKC unit only after
kernel membership, current controls, and the manager's control-group identity
agree. An external or namespace-hidden parent cannot supply that proof; those
workers retain the explicit `Run` cancellation monitor. Use the owned wrapper
for the supervised development workflow described here.

Lower quotas
increase wall time; choose bounded checks one at a time, retain their receipts,
and leave full release assembly to CI on a busy host. The wrapper checks the
configured host-memory reserve before starting any command, including builds
and tests, and checks it approximately once per second while work runs. A
reserve violation stops the owned unit and returns temporary failure (75);
an unavailable or malformed memory reading fails closed when a reserve is set.

The wrapper supervises both scope and service modes. A small detached watchdog
binds the fresh unit to its launcher's process identity and stops that unit
after interruption or launcher death. Cleanup is bounded and only targets the
unit created by this invocation. Output publication still uses RKC's existing
owned staging and atomic publication rules. Verify the actual selected profile
with `scripts/verify-resource-guard.sh`; the probe checks the exact CPU ratio
alongside memory, swap, scheduling, and process-count controls.

### Explicit Desktop memory profile

On Linux, `RKC_RESOURCE_PROFILE=desktop` supports larger compilation and evidence
workloads on a machine with freshly verified headroom. It leaves the default
profile unchanged. Its defaults are 8 GiB soft memory, 10 GiB hard memory, a
6 GiB Go memory target, zero swap, and an 8 GiB host-available-memory reserve.
The Go target leaves room below the soft ceiling for runtime overhead, mapped
data, filesystem cache, and other charged allocations. It is a runtime memory
target, not an additional allocation or a prediction of resident memory.

Before a new Desktop launch, both physical total and available memory must
cover the effective hard allowance plus the reserve. The default launch
therefore needs at least 18 GiB available. While it runs, the existing monitor
enforces the reserve rather than repeatedly charging the unused hard allowance.
This does not reserve RAM against peer processes; the workload still yields
when actual host availability falls below the floor.

```sh
RKC_RESOURCE_PROFILE=desktop \
RKC_CPU_QUOTA_PERCENT=25 \
scripts/with-rkc-limits.sh ./bin/rkc scan --no-python --stage-workers 1 --stage-memory-mib 512 --out /private/new-atlas /path/to/repository
```

Existing MiB settings can narrow this profile. Desktop soft and hard ceilings
cannot exceed 12 GiB and 16 GiB respectively, the Go target cannot exceed the
soft ceiling, swap remains capped at 256 MiB, and the host reserve must be at
least 4 GiB. An omitted Go target is at most 6 GiB and narrows with the soft
ceiling. Every child retains the selector and reserve; a smaller explicit worker
budget cannot enlarge its parent allowance. CPU quotas, one-worker runtime
settings, idle I/O, priority, process limits, and interruption cleanup apply in
both profiles.

Worker memory accounting is sampled ten times per second; the kernel enforces
the hard CPU, memory, swap, and process limits continuously. Cancellation and
launcher exit are handled independently of that sampling timer. Host reserve
and higher-priority workload checks continue once per second. Cleanup retains
its separate, faster checks when proving a stopped unit is inactive. This reduces
routine accounting reads without changing the configured resource ceilings.

On Linux, a new guarded worker also binds its lifetime to its current user
service when the user manager and rechecked kernel membership agree. Stopping
that service, including a watcher whose main process crashes, stops its bound
worker. This lifetime proof does not admit analysis inside the ordinary service;
the worker still needs its own resource limits and fresh admission. It does not
prove cleanup after an arbitrary descendant process dies while its service stays
active. System services, login sessions and hidden namespace memberships do not
acquire this user-service binding through an environment variable.

A larger soft ceiling can avoid reclaim stalls when the host has spare RAM.
It is not a measured speedup. Compare the same bounded input and executable,
retain input and output digests, and measure wall time, CPU use, peak memory,
memory events and pressure. Do not infer current admission from an earlier
headroom reading. A source update also does not change an already installed
binary or running service. Preserve its configuration and last good generations,
prove ownership and shutdown, then qualify a new runtime before activation.
Optional model acquisition retains its separate exact 4 GiB / 4.5 GiB / 256 MiB
memory guard; this profile does not expand qualified model or download budgets.

The [automation integration guide](AUTOMATION_INTEGRATION.md) describes the
versioned scan receipt and the evidence checks an external research or upgrade
service can use.

For a bounded Linux comparison, `scripts/profile-resource-workload.py` freezes
up to 1 MiB across 128 encountered paths and runs two fresh native scans with
one executable. Supply an existing caller-owned execution lock used by the
other work on that host; the tool never creates or replaces it. A same-named
file on another machine is a separate lock. Keep inputs and receipt paths
private and outside the checkout being measured. For example:

```sh
python3 scripts/profile-resource-workload.py \
  --binary /absolute/path/to/rkc --source /private/bounded-fixture \
  --execution-lock /existing/shared-execution.lock \
  --resource-profile desktop --host-reserve-mib 8192 \
  --profile A:4096:4608:3072 --profile B:8192:10240:6144 \
  --cpu-percent 25 --seconds 30 --output /private/new-profile-receipt.json
```

The tool requires fresh physical headroom and acceptable global and cgroup
memory pressure before each arm, and monitors pressure, host reserve, caller
identity and lock identity while it runs. Each arm has a 5..45 second work
deadline plus bounded cleanup. A watchdog retains the lock lease through
cleanup after interruption. Permanent service-manager failure can prevent
cleanup proof; inspect any failed receipt before starting replacement work.
The private receipt records input and executable hashes, canonical output
parity, wall time, CPU counters, peak memory and pressure deltas. Failed or
refused comparisons retain failure receipts. These are bounded performance
observations, not the complete release or source-quality gate. No models,
remote acquisition, repository scripts or Python extractors are invoked.

Workspace refreshes can additionally select `RKC_WORKSPACE_PRIORITY_LEASE=erais`
to defer to the same user's canonical kernel model lease on Linux. Refused
watch passes wait their configured interval, canceled refreshes retain their
last accepted generations, and established readers keep serving. The option
requires proven kernel visibility and safe paths, adds no resource allowance,
and does not configure arbitrary commands or model acquisition. See
[workspace configuration](WORKSPACES.md#optionally-defer-refresh-during-erais-model-work)
and the [workspace model lease contract](rfc/WORKSPACE_MODEL_LEASE.md).

### Starting a trusted workbench

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
