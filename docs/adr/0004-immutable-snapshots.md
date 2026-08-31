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
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
