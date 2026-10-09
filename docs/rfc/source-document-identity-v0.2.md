# RFC: source-document identity after path redaction

Status: accepted for the unreleased source-document producer integration.

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors, licensed under Apache-2.0.

## Problem

The source-document producer initially derived document and section IDs from
original relative paths. Credential redaction can change a canonical display
path, including a credential found elsewhere that also occurs in a filename.
Two different original filenames can then share the same safe display path.
Recomputing identity from that display path loses the original binding;
recovering the original path from its opaque artifact ID is not possible.

Skipping ID validation for redacted paths would weaken portable citation
checks. Using a display path to reopen a source file would also confuse public
metadata with private filesystem authority.

## Decision

`rkc.source-documents@0.2.0` derives document IDs, document-node IDs, and their
logical IDs from the inventoried opaque artifact ID. Section IDs and logical
IDs additionally include the original half-open byte range. The existing
`StableID` algorithm and type prefixes remain unchanged. Artifact IDs and
source-evidence IDs retain their existing formulas.

The artifact identity distinguishes separate original paths even when both
their safe display paths and their source contents are identical. All
document, node, and evidence display paths must still agree. Source references
also require the exact producer version, original content digest, byte/line
ranges, complete subject and section rosters, logical IDs, and record metadata.
No canonical schema fields or private path attributes are added.

The source-document stage cache includes the producer version, so an older
cached fragment cannot be reused as a v0.2 fragment. This is a one-time producer
identity migration: a rescan changes document and section IDs produced by v0.1.
It does not change artifact identity or source-evidence identity.

## Compatibility

Portable readers continue to validate v0.1 documents using their exact original
path-based formulas. The legacy artifact ID must equal the existing artifact
ID formula for the displayed path, proving that the original path remains
available for that validation. Evidence must have the matching v0.1 version.
A v0.1 document with a redacted path receives no source references; rescanning
with v0.2 restores the binding without publishing the original path. Unknown
producer versions and mixed-version receipts are rejected.

## Filesystem and export boundary

Source-body export uses a private scan-session registry keyed by artifact ID,
checked against inventoried file identity, size, and digest. That registry is
never serialized. Redacted or colliding display paths use artifact-derived
normalized output names to prevent overwrites. Portable cited context uses
canonical receipts and needs neither this registry nor the original checkout.

## Verification

Focused regressions cover v0.1 JSON imports, mixed or unknown producer versions,
tampered logical identities, a credential occurring in an unrelated filename,
and two genuine credential-bearing filenames with identical contents and a
shared redacted display name. End-to-end verification retains both normalized
source envelopes, both NotebookLM source records, and distinct portable cited
context identities after the checkout is moved away.

## Snapshot identity binding

Stage-cache invalidation alone does not distinguish immutable snapshots. The
same checkout, config, policy, external plugin lock, toolchain, and core schema
can produce different document IDs after a built-in producer upgrade. Reusing
the old snapshot ID would cause immutable storage to reject those new records.

The snapshot recipe therefore appends the domain-separated ordered inputs
`builtin-source-documents/v1`, `rkc.source-documents`, and the source producer
version, currently `0.2.0`, after its existing schema and provenance inputs.
The `StableID` algorithm and core schema version remain unchanged. This
compiler-contract input is unconditional, including scans with no admitted
source documents or disabled framework extraction. Existing stored snapshots
retain their identities and remain readable; new scans receive the migrated
snapshot identity. Source artifact identities remain unchanged.

A focused regression holds all checkout and externally pinned inputs constant,
distinguishes the current recipe from both the original recipe and the v0.1
producer recipe, and checks that source admission does not omit the binding.
Binding every other built-in analysis version is a separate follow-up; this
change makes the source-document migration safe within the existing contract.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
