# Counterfactual analysis

RKC can compare a canonical graph route with a bounded hypothetical view in
which selected nodes or edges are absent:

```sh
rkc counterfactual \
  --dir .rkc \
  --from 'checkout.Handler' \
  --to 'payments.Charge' \
  --without-node 'retry.LegacyPolicy'
```

Use `--without-edge <exact-edge-id>` when the intervention concerns one
specific relationship. Both flags are repeatable. `--json` binds the result to
the exact snapshot ID and returns the baseline route, hypothetical route,
intervention IDs, evidence IDs and search bounds. Each route also reports
`truncated`, `depth_limit_reached`, `node_limit_reached`, and
`search_exhausted` when applicable, so callers can distinguish an exhaustive
absence result from an unresolved bounded search. This additive truth contract
is identified by counterfactual `schema_version: "1.1"`.

## Truth boundary

This command is deliberately a **bounded structural counterfactual**, not a
simulator and not proof of business causation. It asks:

> In the recorded graph, within these edge filters, depth and node limits,
> does a route remain when these facts are omitted from a derived view?

It does not ask RKC to delete or rewrite canonical facts. It never changes the
atlas. A `no_route_found` outcome is emitted only when the derived search
exhausted every admissible reachable node without hitting its depth or node
cap. If either the baseline or derived search reaches a cap, RKC reports
`search_truncated` instead and explicitly leaves route existence unresolved.
Even an exhaustive structural absence does not prove that a runtime path is
impossible, that an unindexed dynamic edge cannot exist, or that removing code
from a deployed system would be safe.

The result is always marked `authoritative: false`. Its evidence set includes
the nodes and edges in the baseline route, the hypothetical route and the
intervention itself, so an agent or human can inspect what supports the
comparison.

## Making a scenario stronger

Counterfactual quality rises with the evidence beneath it:

1. Import compiler-produced SCIP indexes so definitions and call relationships
   use compiler authority where available.
2. Use a future producer-authenticated runtime event source to distinguish
   observed execution from static possibilities. Current trace capture,
   including same-process capture, remains an operator assertion and cannot
   establish runtime truth.
3. Import semantic history when the question concerns an interface or
   responsibility that moved over time.
4. Filter to relevant edge kinds and resolution classes rather than treating
   every relationship as behavioural.
5. Treat configuration, build constraints and environment contracts as
   conditions that may require separate scenarios.

For example, compare a compiler-resolved call route without one retry-policy
node while retaining explicit search bounds:

```sh
rkc counterfactual \
  --dir .rkc \
  --from 'api.Invoice' \
  --to 'queue.Publish' \
  --without-node 'retry.Policy' \
  --edge-kinds calls,flows_to,returns_to \
  --resolutions compiler_resolved,syntax_inferred \
  --depth 16 \
  --limit 20000 \
  --json
```

If either search reaches the depth or node cap, RKC reports
`search_truncated`. Otherwise, an exhaustively absent baseline is
`baseline_not_found`; the same shortest route is `no_effect`; a different
route is `rerouted`; and an exhaustively absent alternative is
`no_route_found`. Every outcome is explicitly qualified by the recorded scope.

## Intended evolution

The structural layer is a safe base for richer causal work. Future scenarios
can condition the derived view on feature flags, test specifications, runtime
observations and federated service contracts. Those layers must remain
evidence-bearing and must not allow model-generated hypotheses to overwrite
canonical repository truth.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
