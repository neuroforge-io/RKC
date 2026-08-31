# ADR 0001: Deterministic core with optional model synthesis

- Status: accepted
- Date: 2026-07-21

## Context

Repository documentation must be complete, auditable, and usable offline. A
language model cannot reliably enumerate repository facts or prove coverage.

## Decision

Parsers, compilers, indexes, manifests, and runtime imports create canonical
facts. Deterministic templates render complete baseline documentation. A model
may only transform bounded evidence packets into structured claims that are
validated before publication.

## Consequences

- RKC remains useful without a model.
- Model failures degrade prose, not source truth.
- Every generated claim can be traced and rejected.
- The graph and evidence model require more initial engineering than a simple
  retrieval wrapper.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
