# Governance

RKC is intended to be a vendor-neutral open-source project.

## Decision classes

Routine implementation changes are reviewed through pull requests. The
following require an RFC and maintainer approval:

- canonical node, edge, evidence, and diagnostic semantics;
- stable-ID algorithms;
- plugin API or ABI changes;
- storage architecture;
- security defaults;
- model claim policy;
- API major versions;
- telemetry content;
- licensing or governance changes.

## Maintainer obligations

Maintainers disclose conflicts, review security-sensitive changes with a second
maintainer, publish deprecations, preserve documented compatibility, and avoid
creating proprietary-only hooks in canonical formats.

## Releases

Stable releases require passing conformance, reproducibility, security,
migration, and benchmark gates described in `docs/implementation-plan.md`.
