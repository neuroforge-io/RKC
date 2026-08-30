# ADR 0004: Published snapshots are immutable

- Status: accepted
- Date: 2026-07-21

## Decision

A published repository graph is immutable and identified by source, config,
schema, plugin, and toolchain digests. Re-analysis creates a new snapshot.

## Consequences

- Citations remain stable.
- Diffs and audit are reliable.
- Storage retention must be managed.
- Mutable aliases such as `latest` resolve to immutable IDs.

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
