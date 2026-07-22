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
