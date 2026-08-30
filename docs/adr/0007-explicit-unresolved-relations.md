# ADR 0007: Unresolved relationships use explicit placeholder nodes

- Status: accepted
- Date: 2026-07-21

## Context

Syntax and dynamic-language analyzers often identify a relationship without a
safe unique target. Dropping it falsifies coverage; choosing a convenient target
falsifies semantics.

## Decision

Create a stable `unresolved_symbol` node and connect the relation to it with
resolution `unresolved`. Preserve spelling, expected kind, source context,
candidates, and resolution attempts as attributes/evidence.

## Consequences

- every edge remains referentially valid;
- unresolved work is measurable and queryable;
- later semantic adapters can replace placeholders through explicit merge logic;
- graphs contain visible uncertainty rather than cleaner fiction.

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
