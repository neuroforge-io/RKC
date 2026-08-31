# ADR 0003: Plugins write only GraphPatch operations

- Status: accepted
- Date: 2026-07-21

## Decision

Plugins return versioned, bounded GraphPatch operations. They do not receive
database handles and cannot publish snapshots.

## Consequences

- Core validation, migrations, cache ownership, and audit remain enforceable.
- Plugins can be implemented in multiple languages.
- Large patches require streaming protocols and backpressure.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
