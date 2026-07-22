# ADR 0008: Models receive bounded evidence packets

- Status: accepted
- Date: 2026-07-21

## Context

Passing an entire repository or arbitrary retrieved chunks to a model makes
coverage, citations, resource use, and prompt injection difficult to control.

## Decision

Construct deterministic task-specific packets containing one subject, bounded
related records, evidence, redacted excerpts, policy, and output schema. Validate
all returned claims against packet evidence and known identifiers.

## Consequences

- local small models can operate within controlled memory;
- unsupported claims can be rejected;
- packet/model/prompt digests support cache and audit;
- broad architectural synthesis requires hierarchical packet composition.
