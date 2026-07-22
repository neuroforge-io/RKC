# ADR 0006: Canonical records are separate from derived products

- Status: accepted
- Date: 2026-07-21

## Context

Markdown, static sites, search indexes, embeddings, diagrams, NotebookLM packs,
and model prose must be replaceable without changing repository truth.

## Decision

Snapshots, artifacts, nodes, edges, evidence, diagnostics, conflicts, and
coverage form canonical source truth. Documents and claims retain explicit
derivation metadata. Search, browser, integration, and model outputs are
rebuildable projections.

## Consequences

- deleting a derived index cannot destroy source truth;
- model/provider changes do not silently change snapshot identity;
- exporters can evolve independently;
- canonical and derived migrations need separate compatibility policies.
