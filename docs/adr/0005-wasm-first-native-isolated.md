# ADR 0005: WASM-first plugins with isolated native workers

- Status: accepted
- Date: 2026-07-21

## Decision

Use capability-scoped WebAssembly/WASI for pure extractors and validators. Use
out-of-process native workers for compilers, language servers, and tooling that
cannot reasonably run as WASM.

## Consequences

- Common plugins can run with strong in-process capability limits.
- Precise language tooling remains possible.
- Native worker sandboxing is a production release blocker, not an optional
  hardening exercise.
