# ADR 0002: Go core, SQLite local, PostgreSQL server

- Status: accepted
- Date: 2026-07-21

## Decision

Use Go for CLI, daemon, scheduler, and workers. Use SQLite with FTS5 for embedded
operation and PostgreSQL for concurrent server deployments. Keep storage behind
the canonical repository interfaces.

## Consequences

- Local deployments can use a small number of binaries and files.
- Server deployments gain mature concurrency and operations.
- Storage parity requires conformance tests across both adapters.
- A dedicated graph database is optional, not canonical.
