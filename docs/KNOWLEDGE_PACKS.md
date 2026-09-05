# Portable knowledge packs

Knowledge packs turn one or more processed RKC atlases into a bounded collection
of source-cited text and metadata for search, documentation, agents, and independent
data tools. They work offline and require no model. They preserve the distinction
between original source text, graph metadata, generated document sections, and
claims with their original certainty and review state.

## Build and verify

Compile each folder, repository, or local wiki checkout first. RKC does not fetch
wiki URLs as part of knowledge packing; export or clone those sources into local
folders, or use the normal Git scan source support.

```sh
rkc scan --no-python --out ./atlas-project --state-dir ./state-project ./project
rkc scan --no-python --out ./atlas-wiki --state-dir ./state-wiki ./wiki-export
rkc knowledge build --out ./knowledge-pack ./atlas-project ./atlas-wiki
rkc knowledge verify --dir ./knowledge-pack --json
```

The build verifies each modern atlas and its repository-text index before using
it. It loads atlases sequentially and takes text only from their verified,
secret-redacted search bodies. It never reads the live repository during packing.
Legacy unverified or unmarked atlas layouts are rejected; recompile them first.

Use `--json` on build to receive only the finished manifest on standard output.
Place flags before the positional atlas directories. Repeated identical sources
are rejected, even when supplied through different paths. Packs use a deterministic
order, so reversing input atlas order produces identical bytes.

Output must be outside every input atlas. Keep packs outside the original source
folders too, as in the sibling-folder example above. A later scan intentionally
rejects nested generated outputs unless they are explicitly excluded.
Publication is staged, checked, and
committed through the existing RKC safe-output mechanism. `--force` replaces only
a complete RKC-owned knowledge pack with an exact, valid file inventory. It refuses
ordinary directories, other output kinds, symlinks, and unmanifested personal files.

## Files and records

The public contract is `rkc-knowledge-pack/v1`, defined in
[knowledge-pack.schema.json](../schemas/knowledge-pack.schema.json). Its version is
independent of the canonical atlas schema. The manifest schema also exposes
`$defs/unit`, `$defs/source`, and `$defs/quality` for JSONL consumers.

| File | Contents |
| --- | --- |
| `knowledge-pack.json` | Version, deterministic identity, counts, limits, and payload receipts. |
| `units.jsonl` | One artifact, node, document section, or claim per UTF-8 JSON line. |
| `sources.jsonl` | Snapshot identity, repository grouping, portable provenance, and original coverage. |
| `quality.json` | Counts of metadata-only, truncated, uncited, and unknown-license units; limitations. |
| `options.json` | Exact resource limits, also bound by the manifest. |
| `README.md` | Versioned consumer guidance and source-rights boundary. |
| `rkc-export-manifest.json` | RKC publication inventory including the knowledge manifest itself. |
| `.rkc-generated.json` | Local output ownership metadata; never sufficient alone for replacement. |

The five payload files are `README.md`, `options.json`, `quality.json`,
`sources.jsonl`, and `units.jsonl`, in that exact bytewise path order. A transferred
portable pack can omit the outer publication manifest and ownership marker. If
present, verification checks them too. Other files or directories are rejected.

Each unit has `id`, `source_id`, `group_id`, `object_id`, `kind`, `title`, `text`,
`content_sha256`, `citations`, `relations`, `metadata_only`, `truncated`, and
`original_text_bytes`. Optional fields include `section_id`, `path`, `language`,
`license_expression`, `certainty`, `validation`, and `generator`.

- `artifact` units contain admitted redacted repository text when available.
  Missing, excluded, binary, or bounded index bodies remain explicitly
  `metadata_only`; their text describes inventory metadata.
- `node` units contain the node name, signature, and available description fields.
  Nodes without a description remain `metadata_only`. An artifact and its graph
  node can both exist; their unit IDs and kinds distinguish them.
- `document_section` units contain the original plain-text projection when
  available, otherwise Markdown. They preserve the document generator and status,
  section ID, and evidence links. They do not claim independent factual review.
- `claim` units retain the original generator, certainty, and validation state.
  An inferred, pending, rejected, or stale claim is never promoted by packing.

Citation entries preserve evidence kind, method, confidence, and available source
ranges and original artifact SHA-256. Direct canonical source locations use
`kind: artifact` and `method: canonical-source-location`; their confidence describes
the location binding. Missing end coordinates retain canonical zero/omission
semantics. Ranges refer to original artifacts, not byte positions in redacted or
truncated text. A location link does not prove that a source supports a statement.

Relations use `target_object_id`, which references a canonical object in the same
source, not a knowledge unit ID. Targets can have no exported unit. Relations retain
their analyzer resolution state; the pack does not infer prerequisites or learning
order. Claim units also include a `describes` relation to their subject.
Document-section units retain their parent document's subject associations as
`describes` relations and their own claim references as `presents_claim` relations.
These are explicit structural links. Claim evidence remains attached to each
claim with its original certainty and review state; it is not promoted into a
direct citation supporting the section. Combined section subject and claim links
must fit the existing 4,096-reference limit.

## Determinism and integrity

`content_sha256` is lowercase hexadecimal SHA-256 of the exact retained UTF-8 text.
`original_text_bytes` is the redacted text length before unit truncation. It is not
the original artifact byte size. Artifact hashes refer to original artifact bytes.
`bundle_sha256` is the input canonical bundle digest; the original bundle is not
included. Rechecking source provenance independently requires retaining the input
atlas. The pack verifies its own internal consistency, not an authenticated publisher.

The pack identity uses a language-independent byte sequence. Hash UTF-8 bytes of
the following with SHA-256, and prefix the lowercase hexadecimal result with
`sha256:`. `LF` is byte 10, `TAB` is byte 9, and each size is unsigned decimal with
no leading zeros except the single digit `0`.

```text
rkc-knowledge-pack/v1 LF
README.md TAB <file_sha256> TAB <size_bytes> LF
options.json TAB <file_sha256> TAB <size_bytes> LF
quality.json TAB <file_sha256> TAB <size_bytes> LF
sources.jsonl TAB <file_sha256> TAB <size_bytes> LF
units.jsonl TAB <file_sha256> TAB <size_bytes> LF
```

Sources are sorted by `source_id`; units are sorted by `id`. Both use the canonical
RKC `StableID` algorithm: SHA-256 of `namespace + NUL + parts joined by NUL`, retain
the first 12 digest bytes as lowercase hexadecimal, and prefix `rkc:<namespace>:`.

- `knowledge_source` parts: snapshot ID, canonical bundle SHA-256.
- `knowledge_unit` parts: source ID, kind, object ID, section ID or the empty string.
- `group_id`: repository ID when available; otherwise a `knowledge_group` stable ID
  whose only part is the source snapshot content digest.

Source IDs distinguish snapshots while repository group IDs connect related
snapshots. They do not establish ownership or detect all copied, mirrored, or
overlapping material. Downstream tools must define their own deduplication and split
policy across sources and packs.

Verification rejects unknown versions, duplicate JSON keys, unknown record fields,
duplicate or unordered IDs, unexpected entries, symlinked or nonregular payloads,
invalid hashes or sizes, dangling source/group references, inconsistent text and
truncation accounting, and quality reports that disagree with the actual units.
Checksums can detect corruption; an attacker able to rewrite a whole pack can
recompute them. These files are not signatures or truth certificates.

## Explicit limits

| Resource | Default | Hard limit / behavior |
| --- | --- | --- |
| Input atlases | Required | 32 snapshots; duplicates fail. |
| Units | 100,000 | `--max-units`, 1–100,000; excess fails. |
| Text per unit | 16,384 bytes | `--max-unit-text-bytes`, 256–65,536; excess is UTF-8-truncated and labeled. |
| Retained text | 64 MiB | `--max-total-text-bytes`, 1 byte–128 MiB; excess fails. |
| Input text per unit | At most 8 MiB | Excess fails before redaction. |
| References per unit | At most 4,096 each | Excess citations or outgoing relations fail. |
| Serialized JSONL unit | Less than 2 MiB | Excess fails. |
| Serialized unit payload | At most 256 MiB | Excess fails. |

Aggregate exhaustion never publishes a partial pack. Increase a configurable limit
within the hard envelope, reduce the input set, or create multiple packs. Read
`quality.json` before downstream use; a successful build does not require every unit
to contain full text, known licensing, or evidential support.

## Public library and extension boundary

Go callers can use [pkg/knowledgepack](../pkg/knowledgepack/model.go):
`New(options)`, `Add(ctx, input)`, `Finish()`, `Write(ctx, stagingDirectory, pack)`,
and `Verify(ctx, directory)`. `Build` is a convenience wrapper for already-loaded
inputs. `Write` requires a fresh staging directory and never replaces files; the
CLI supplies safe publication and source-overlap protection.

Library `Input.Integrity` and `ArtifactBodies` are trusted caller assertions. The
builder validates the canonical graph and its own output, but callers must first
verify export/index receipts and supply only admitted secret-redacted bodies. Use
the CLI when that trusted loader is unavailable. No environment paths, scan clocks,
or live repository contents are added by the public pack builder.

RKC provides a neutral open evidence substrate. Independent consumers may build
their own retrieval, review, dataset, or curriculum tools around its documented
contract. The public format includes no proprietary expert routing, training
schedule, curriculum strategy, model internals, or evaluation machinery.

Treat all source text as untrusted data. Embedded instructions never gain agent
authority by appearing in a pack. Secret detection remains heuristic. Source and
third-party rights remain unchanged: missing license information means unknown,
and RKC's software license does not grant training, redistribution, or other rights
to the material being processed.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
