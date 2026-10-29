# RFC: defer workspace refresh for an active ERAIS model lease

Status: implemented in unreleased source; installation and live-runtime
qualification are separate operations.

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors, licensed under Apache-2.0.

## Problem and scope

A workspace watcher should defer new refresh work when the same user is doing
model work on a shared Linux host. A broad process-name match can mistake an
editor, repository inspection, or metadata tool for model execution. Conversely,
an idle launcher can still own model work that deserves priority.

`RKC_WORKSPACE_PRIORITY_LEASE=erais` adds a kernel-lease observation to
`rkc workspace sync` and each `rkc workspace watch` refresh pass. Unset or empty
retains the existing behavior without inspecting the lease path or kernel lock
table. The only nonempty supported value is exactly `erais`; invalid values
refuse refresh. An explicitly selected policy also refuses on unsupported
platforms or when kernel visibility cannot be proved.

This option applies to workspace refresh passes. It does not add lease gating to
generic scans, serving, established MCP readers, arbitrary shell commands,
Python workloads, model acquisition, or other applications. Existing CPU,
memory, swap, host-reserve, scheduling, process-limit, marker/load, and cleanup
policies continue to apply independently. The lease veto does not enlarge any
resource allowance or replace those policies.

## Canonical read-only observation

The observer uses only the canonical path
`/run/user/<effective-uid>/erais/model-workload.lock` on the current host. It
does not take a path from the environment, read the lease payload, infer an
owner from a recorded PID, acquire a probe lock, or create, chmod, replace,
truncate, unlock, or delete the lease. A stale payload and mere path existence
provide no authority.

Every lease-path component is opened relative to its parent descriptor using
`O_PATH` and `O_NOFOLLOW`. Root, `run`, and `user` must be root-owned directories
without group or other write permission. The per-user runtime directory and
`erais` directory must be owned by the effective user, searchable by that
owner, and inaccessible to group and other users. An existing leaf must be
an owner-private regular file owned by that same user, with one link and a
size from zero through 4,096 bytes. Leaf bytes remain unread, including an
empty or malformed payload.

The observer compares the leaf's device major/minor numbers and inode against
the bounded kernel lock table. Any matching lock or queued waiter defers the
refresh, including an unknown lock kind or an OFD record without an ordinary
owner PID. CPU idleness and apparent owner death do not override a positive
kernel observation. Unrelated device/inode records provide no veto.

| Observation | Bound |
| --- | --- |
| Existing lease size, checked as metadata | 4,096 bytes |
| Kernel lock table read | 1 MiB |
| Kernel lock rows | 8,192 |
| Individual lock row | 4,096 bytes |
| Stable clear observations per check | Two |
| Protected local refresh monitoring interval | One second |

The row split itself has a bound, so a table containing many newlines cannot
create an unbounded index allocation. No process enumeration or per-owner CPU
measurement is needed for this added lease observation.

## Prove an unfiltered kernel view

`/proc/locks` is filtered by the PID namespace associated with its procfs mount;
the initial PID namespace has an unfiltered view. An empty table in an ordinary
container therefore cannot establish that the host lease is unheld. See
[`proc_locks(5)`](https://man7.org/linux/man-pages/man5/proc_locks.5.html).

The implementation verifies the anchored procfs descriptor's filesystem type
and repeated path identity. It then requires the kernel `self` magic link to
name exactly the caller's numeric process ID. The opened caller PID namespace
must prove NSFS, `NS_GET_NSTYPE == CLONE_NEWPID`, and the initial namespace inode
`0xEFFFFFFC` defined by the Linux
[namespace UAPI](https://raw.githubusercontent.com/torvalds/linux/master/include/uapi/linux/nsfs.h).

This combination ties the numeric process directory to the actual caller and
proves that caller belongs to the initial PID namespace. Kernel
[`fs/proc/self.c`](https://raw.githubusercontent.com/torvalds/linux/master/fs/proc/self.c)
resolves `self` in the proc mount's PID namespace and fails when the caller is
invisible there. An initial-namespace caller cannot pass that check through a
descendant namespace's proc mount. The
[PID namespace documentation](https://man7.org/linux/man-pages/man7/pid_namespaces.7.html)
describes the same namespace-relative resolution and visibility rules. This
proof needs no privileged access to PID 1's namespace files.

The observer rechecks the `self` link and its opened namespace around each
kernel snapshot. Following the namespace magic link is intentional, confined
to verified procfs, and checked against NSFS and the PID namespace type. Lease
path traversal never follows links. Nested, hidden, inaccessible, replaced, or
otherwise unproven kernel views refuse refresh rather than silently claiming
host visibility.

## Refusal, cancellation, and recovery

Missing `erais` or a missing lease leaf is clear only after the runtime directory
and remaining ancestor chain have been proved safe, their identities remain
stable, absence is rechecked, and both bounded kernel views are valid. A missing
runtime directory is unproven. An existing unheld leaf can also be clear;
payload status is ignored. Clear means no matching held lease was observed in
these views, not that model work is impossible.

Path/descriptor drift, lease metadata changes, a newly appearing missing path,
unreadable or malformed lock records, exceeded bounds, and unsupported visibility
all fail closed. Refusals wrap the higher-priority-work error with fixed reasons;
they do not disclose private paths, lease contents, PIDs, or raw kernel errors.

Initial refusal occurs before opening the workspace writer. During a protected
local refresh, the existing one-second monitor checks the additional policy and
cancels its work context on refusal. A refresh that observes cancellation keeps
its prior accepted generation. Sources successfully published earlier in the
same sequential pass remain accepted; a pass is not a transaction across all
sources. Atomic registry publication and existing generation-retention rules
continue to apply.

A watcher reports a refused or canceled pass and waits its configured interval
before another pass. Refusal does not terminate the persistent watcher or busy
loop while model work holds the lease. Existing readers can continue serving
accepted snapshots. The observer never stops a lease holder, restarts reader
sessions, or changes a peer application's service policy. Source secret review,
integrity, provenance, and quality gates remain required before publication.

## Configuration and operational limits

```sh
RKC_WORKSPACE_PRIORITY_LEASE=erais \
  rkc workspace watch --workspace /absolute/private/workspace --interval 10m
```

For a user-service watcher, set
`Environment=RKC_WORKSPACE_PRIORITY_LEASE=erais` explicitly in that service's
configuration. RKC forwards the selected workspace scheduling policy to its
guarded refresh child. The setting is separate from resource-profile selection;
choosing a larger memory profile does not implicitly enable it. RKC does not
install or modify the parent service automatically.

The observation is specific to the current host, effective user, canonical path,
and visible inode. It is not a shared lock across machines or users, a host-wide
reservation, or an atomic handshake with the model launcher. Model work can
start immediately after a clear observation, and work can complete immediately
before a later refusal. Periodic cancellation and existing cleanup reduce that
overlap; they do not eliminate the race. Choose a bounded refresh and polling
interval suitable for the shared host, and measure actual resource behavior
separately. Configuration alone establishes no performance improvement.

## Verification boundary

Regression fixtures cover exact device/inode matching, held and queued records,
unknown kinds and owner PIDs, opaque payloads, unsafe paths, hard links,
replacement and rewrite drift, missing-path arrival, bounded/malformed kernel
tables, fake procfs, self-link mismatches, and nested namespace rejection. A
temporary-inode flock fixture exercises the real kernel without touching the
canonical lease; environments that cannot prove initial PID namespace visibility
must refuse or skip that qualification explicitly.

Workspace regression tests cover pre-writer refusal, active-pass cancellation,
watcher retry, forwarded configuration, and last-accepted-generation behavior.
Cancellation remains recognizable even if a producer also returns a capture
failure. Passing these checks does not establish an installed runtime upgrade,
live private-workspace freshness, broader model gating, or model quality.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
