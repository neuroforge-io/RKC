# RFC: canonical structural metadata during credential redaction

Status: implemented in unreleased source.

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors, licensed under Apache-2.0.

## Problem

Detected source literals propagate into parser and plugin payload redaction.
A credential-shaped source field can contain a canonical type name such as
`function`, a content digest, or a substring of a digest. Treating every string
in the canonical bundle as payload then changes valid node kinds and artifact
hashes. Strict validation correctly rejects the resulting graph. Repeated
refreshes cannot publish it, even though the original metadata was valid.

## Decision

`canonical-structural-redaction/v1` distinguishes exact typed metadata from
free-form payload. Valid SHA-256 fields on canonical snapshot, artifact, node,
evidence, document, and Git records retain their digests. Canonical artifact
content addresses retain their exact lowercase `sha256:` recipe. Fixed kind,
status, resolution, severity, and claim-state values are preserved only when
they belong to that exact record field's published vocabulary.

Unknown vocabularies and malformed digests still undergo payload redaction
and strict validation. A digest-shaped value or a key named `sha256`, `kind`,
or `password` in an open map remains payload. The same applies to canonical
Go structs nested within those maps. Display names, source text, document
bodies, arbitrary attributes, and ID strings retain their existing redaction
policy. This change adds no general ID exemption.

Built-in source documents carry a duplicate `source_sha256` receipt. Preserve
that copy only when its known producer, source path, artifact ID, and original
digest already match a valid typed artifact digest. Rebuild the three inventory
node receipt fields from the sanitized artifact. Other map values remain
redacted. Neither operation authorizes a source citation: the existing complete
producer, node, evidence, range, and record checks still apply independently.

Source findings, confidences, fingerprints, review expiry, and workspace quality
gates remain unchanged. Preserving structural metadata does not approve a
credential finding. The original source still needs the existing explicit
review before a workspace may activate a generation with such findings.

## Identity and cache compatibility

Append `canonical-structural-redaction/v1` unconditionally to the existing
snapshot recipe after the source-document producer version. Identical source
and externally pinned inputs can otherwise produce a different canonical
bundle under an already-used immutable snapshot ID. The core schema and
`StableID` algorithm remain unchanged; existing stored snapshots retain their
identities and remain readable. No workspace registry fields are added.

The resolve stage records this contract version and runs without cache reuse.
Restored analyzer fragments therefore pass through the new boundary. Upstream
analysis cache entries remain reusable under their existing exact contracts.
The additional bookkeeping is linear in the bounded artifact, node, and
document rosters, with in-memory indices and no new persistent cache.

## Verification

A runtime-generated five-file fixture supplies exact content digests, the
empty-file digest, and `function` as credential values. The prior binary fails
strict validation with three invalid artifact hashes and one invalid node kind.
Regressions require the corrected scan to retain all source secret findings,
preserve independently computed hashes and the function node, and keep the
matching values masked in payload text and generic attributes.

Tests compare cold, warm-cache, and sequential canonical output; verify export
loading and cited document context; reject malformed typed values; and exercise
wrong producer, source path, artifact identity, and digest copies. Snapshot
tests distinguish the new recipe from every prior source-document recipe,
including scans with framework or plugin extraction disabled.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
