# Contributing

RKC accepts contributions under Apache-2.0 using a Developer Certificate of
Origin sign-off. The project values reproducible facts, explicit uncertainty,
and small interfaces over clever coupling.

## Before opening a change

- Read `docs/implementation-plan.md`.
- Open an issue or RFC for canonical schema, plugin ABI, security-default,
  stable-ID, or storage changes.
- Add tests and fixtures for observable behavior.
- Do not add a model dependency to deterministic extraction paths.
- Do not give plugins direct storage access.
- Do not silently ignore files, errors, unsupported constructs, or conflicts.

## Local checks

```bash
make safe-coverage
make smoke
```

## Commit sign-off

Use:

```bash
git commit -s
```

The sign-off certifies the repository's [Developer Certificate of
Origin](DCO). Every non-merge commit contributed after the initial repository
import must carry a parsed `Signed-off-by:` trailer whose name and email exactly
match the commit author. Merge commits are excluded from this per-contribution
check. A lookalike line in the message body or a different identity does not
satisfy the gate.

## Adapter contributions

An official language or framework adapter must include:

- plugin manifest and capability declaration;
- source license metadata;
- fixtures and golden output;
- malformed-input and cancellation tests;
- resource bounds;
- accuracy notes and known limitations;
- documentation and ownership.

## Security findings

Do not open a public issue for a suspected vulnerability. Follow `SECURITY.md`.
