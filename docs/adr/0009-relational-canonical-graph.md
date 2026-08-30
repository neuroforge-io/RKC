# ADR 0009: Relational storage precedes an optional graph database

- Status: accepted
- Date: 2026-07-21

## Context

Most required graph operations are bounded neighbourhoods, paths, impact, and
component queries. Introducing a mandatory graph database would increase local
and service operational complexity before evidence shows it is necessary.

## Decision

Use adjacency tables and indexes in SQLite/PostgreSQL as the canonical graph
projection. Add an optional external graph engine only behind the read/query
interfaces and only after measured workloads justify it.

## Consequences

- local operation remains self-contained;
- storage and graph truth stay transactional;
- extreme cross-repository graph workloads may later require a derived engine;
- graph-engine synchronization must remain rebuildable and non-canonical.

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
