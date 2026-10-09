# Working with messy data

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors, licensed under Apache-2.0.

An atlas can combine a repository with research notes, exported records, and
operator logs. Put related files in a folder and compile it normally:

```bash
rkc scan --out ./research-atlas ./research-export
rkc search --dir ./research-atlas "onboarding citations"
rkc context --dir ./research-atlas --format markdown "onboarding citations"
rkc serve --dir ./research-atlas
```

No model or extra parser installation is needed for these formats. Source
documents appear in search and cited context alongside code and Markdown.
Supported documents also supply canonical, redacted excerpts to grounded
answers; the [model connection guide](MODEL_ENDPOINTS.md) covers optional
providers. Use the [language guide](LANGUAGE_ANALYSIS.md) to understand the
different precision levels available for code analysis.

| Input | Representation | Interpretation boundary |
| --- | --- | --- |
| `.txt`, `.text` | Bounded text sections | Prose is retained as source assertions |
| `.log` | Bounded text sections | Timestamps and messages are not authenticated runtime observations |
| `.rst` | Bounded text sections | Directives, includes, and embedded code are never executed |
| `.jsonl`, `.ndjson` | One section per nonblank physical record | Validity is recorded; malformed records remain searchable text |
| `.csv`, `.tsv` | Header plus record sections, including quoted multiline cells | The first record supplies column labels; no field types or business schema are inferred |

Files with unknown extensions remain inventoried; this adapter does not guess
that unknown code is prose. Ordinary `.json` still uses the existing JSON
Schema, OpenAPI, and artifact-source paths. Binary office documents, PDFs,
OCR, archive expansion, and lossy character decoding are separate capabilities
and are not introduced by this adapter.

## Reading source receipts

The producer is `rkc.source-documents@0.1.0`. Every source document, section
node, and evidence record retains its inventoried artifact identity, relative
path, original SHA-256, and original line and half-open byte ranges. Original
UTF-8, CRLF/LF layout, large JSON numbers, duplicate JSON keys, and malformed
JSON lines are preserved rather than silently reserialized. A UTF-8 BOM is
retained; a JSONL record containing it is explicitly invalid.

CSV/TSV record sections use a readable column/value projection. CSV decoding
normalizes embedded CRLF in those values according to the standard parser;
citations still point to the original record bytes. The normalized source
export retains the original layout with redactions. Repeated or empty header
labels retain numbered column positions, so two similarly named columns can
be distinguished. Blank lines skipped by the CSV reader remain within the
next record's source range when present.

Evidence uses `documentation_asserted`. Parsing establishes what the source
contains, not whether its claims are true or its reported events occurred.
Source document extraction therefore does not upgrade an artifact to
`syntax_parsed` or improve code-semantic coverage.

For portability, cited context and grounded-answer readers validate the whole
document's producer, artifact digest, node roster, evidence roster, source
ranges, and projection metadata before granting source references. They do
not need the original folder after the atlas has been moved. This is
structural provenance validation; it does not authenticate an external
export's original producer.

## Sensitive fields and malformed exports

The shared credential scanner recognizes quoted JSON string keys, including
escaped key/value strings and short sensitive literals. CSV/TSV exports also
mask values in credential-like columns such as `password`, `api_key`, and
`access_token`. The same column masking applies to normalized source,
artifact-body search, static browser source data, and NotebookLM packs.
Findings record positions and fingerprints derived from those positions;
credential values do not enter finding receipts.

Malformed JSONL lines are retained with `record_valid: false`. Inconsistent
CSV column counts retain their raw redacted record. Broken CSV quoting makes
subsequent record boundaries ambiguous, so the remaining suffix becomes
bounded text. If sensitive columns were identified, uncertain column
alignment causes the entire remaining display suffix to be masked. Other
files continue to compile. Warnings explain malformed content and omitted
document projections.

Credential detection is conservative. Placeholder and indirect-reference
suppression remains part of the existing scanner policy, and sensitive
numeric/object JSON values are not recursively classified by this string
detector. An empty findings list is not a guarantee that an export contains no
private data. Repository text, including anything that resembles instructions,
is untrusted source material throughout retrieval and model context.

## Resource and interruption behavior

The adapter has fixed ceilings independent of larger inventory allowances:

| Resource | Ceiling |
| --- | --- |
| Original bytes per file | 8 MiB |
| Original bytes per pass | 64 MiB |
| Documents per pass | 4,096 |
| Sections per file / pass | 4,096 / 16,384 |
| Plain-text bytes per section | 16 KiB |
| Plain-text projection bytes per file / pass | 8 MiB / 32 MiB |

The section body limit also applies while constructing wide CSV projections,
before repeating header text can cause large allocations. Oversized files or
exhausted aggregate budgets remain in inventory with `RKC-DATA-1001` or
`RKC-DATA-1002` diagnostics. Invalid UTF-8 and NUL-containing text receive
`RKC-DATA-1003`; lossy conversion is not attempted. Partial documents have
`complete: false` and `RKC-DATA-1004`. A shortened record section also has
`projection_truncated: true`; its citation covers the original full record,
and the omitted suffix is not implied to be present in context. Malformed
records produce `RKC-DATA-1005`.

Larger structured artifacts retain the existing bounded artifact-search
policy; documents can make the admitted record projection searchable beyond
that artifact-body allowance. Limits never imply complete dataset recall.
Split larger exports by topic or date when more of their contents need to be
available in one atlas.

Source reads, redaction passes, CSV records, section assembly, and graph
construction check cancellation. No partially redacted output is returned
on cancellation. Regex evaluation and a single operating-system read are
bounded but cannot be interrupted internally. The compiler's existing
transactional publication and stage cache determine whether a complete atlas
is published; this adapter does not write a partially compiled atlas itself.

The dedicated `source-documents` stage shares framework admission. Disabling
framework extraction disables this stage while preserving inventory and
artifact text. Its cache key includes the producer version and only admitted
source-document inputs, so a Markdown-only change does not invalidate an
unrelated source-document projection. Fresh normalization and post-merge
sanitization remain active around cached analyzer results.

## Reproducible examples

The [messy-data fixture](../fixtures/messy-data/meeting-notes.txt) combines
fictional prose, logs, quoted multiline CSV, malformed NDJSON, exact large
integers, and synthetic credentials. Tests cover deterministic file-order
independence, source-range fidelity, source/cache/sequential equivalence,
export-wide credential withholding, search, structural context validation,
grounded answers, size/section limits, and cancellation.

Related guides: [Quickstart](QUICKSTART.md), [language analysis](LANGUAGE_ANALYSIS.md),
[model endpoints](MODEL_ENDPOINTS.md), and [implementation status](IMPLEMENTATION_STATUS.md).

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
