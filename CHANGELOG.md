# Release history

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors and Apache-2.0 licensed.

## Unreleased

- Check Go formatting in small sequential batches and fail on tool errors,
  including failures with empty output, to reduce task pressure on shared hosts.
- Add optional workspace-only ERAIS kernel-lease observation, with bounded
  read-only checks, proven initial PID namespace visibility, fail-closed
  refresh deferral and cancellation, retained accepted generations, and normal
  watcher retry intervals. Existing reader sessions and secret gates remain.
- Preserve canonical digests and fixed vocabularies when source credential
  literals happen to match them, retaining strict validation, source-document
  receipts, payload redaction, and unchanged source secret-review requirements.
- Reduce routine worker accounting polls from forty to ten per second while
  retaining kernel limits, independent cancellation, priority checks, and the
  existing cleanup verification cadence.
- Bind guarded workers to a proven owning user service so a stopped or crashed
  watcher service cannot leave its refresh running. Lifetime binding does not
  grant resource admission or permission to stop the parent service.
- Add a bounded, lock-coordinated resource comparison tool with private receipts,
  deterministic-output parity, pressure checks, and interruption cleanup tests.
- Cleaner workbench onboarding, search examples, analysis-depth guidance, and
  assistant handoffs that retain source citations and clear stale context when
  retrieval settings change.
- Portable model profiles and local readiness checks for existing local servers,
  native OpenAI, Claude, Gemini, and explicitly selected compatible HTTPS APIs;
  environment-based credentials, cancellable requests, bounded responses, and
  separate requested/provider-reported model identities.
- Evidence-bearing TXT, RST, log, CSV, TSV, JSONL, and NDJSON source documents,
  including malformed-record diagnostics, byte/line provenance, bounded
  projections, and structured secret redaction across documents and exports.
- Local Streamable HTTP MCP, canonical document context bindings, and cited
  endpoint answer protocols preserved from the previous development work.
- Lower configurable CPU quotas for busy development hosts, alongside smaller
  memory ceilings and one-at-a-time local checks. Short credential values stay
  masked in their fields without renaming unrelated files or code symbols.
- Private, verified source references for file reads after display-name
  redaction, distinct normalized outputs for colliding names, and versioned
  source-document identities that keep portable citations intact. New snapshot
  identities bind that producer version; stored snapshots remain readable.
- Source-verification failures identify changed inputs by opaque references,
  keeping private filenames and filesystem paths out of errors and run journals.
- Inherited busy-host limits for guarded workers, exact CPU-quota verification,
  and supervised shell workloads with host-memory admission and cleanup after
  interruption or launcher death. Release benchmarks run before cold race
  checks so failures stop expensive validation earlier.
- Consistent SQLite ownership preflight during simultaneous opens and
  migrations, retaining strict schema and journal verification in one read
  snapshot, including read-only opens.
- An explicit Desktop memory profile for larger compilation and evidence
  workloads, with coherent Go and kernel budgets, fresh physical headroom
  admission, inherited limits, and continuous reserve monitoring. Existing
  default limits and CPU, I/O, cancellation, and cleanup protections remain.
- Versioned successful scan receipts for external automation, including
  documented journal, snapshot, digest, quality-gate, and context checks.

These changes are in development source. Portable release qualification and
real hosted-model quality remain separate from local tests and protocol mocks.

## 0.4.1 (2026-09-30)

The [published 0.4.1 release](https://github.com/neuroforge-io/RKC/releases/tag/v0.4.1)
is signed tag `v0.4.1` at source commit
`3ca9bf5962a9f152224a17273e605be8d6b45c76`. Its
[main CI](https://github.com/neuroforge-io/RKC/actions/runs/36651490838),
[CodeQL](https://github.com/neuroforge-io/RKC/actions/runs/36651490287), and
[tagged native qualification](https://github.com/neuroforge-io/RKC/actions/runs/36654461053)
passed. All six downloadable archives carry matching native installation,
compilation, cited-context and local GUI receipts.

- Cited context fills its bounded result with records that contain body text
  or a signature. Empty structural records no longer take excerpt slots in
  the GUI, CLI, HTTP or MCP context paths.
- Private tracked workspaces refresh several repositories and serve their
  combined context through MCP. Refresh compiles stable private captures while
  source repositories continue changing, and unchanged refreshes verify pinned
  exports without loading every payload into memory.
- Compilation and repository inventory respond to cancellation. Large
  indexed signatures retain the canonical source and explicit truncation
  receipts, and merged source ranges keep their original artifact identity.
- Secret review decisions are bound to the exact source file and detector
  semantics. Configuration references are distinguished from secret literals,
  and refresh failures explain the affected source.
- Native filesystem tests cover workspace refresh and private review fixtures
  on macOS and Windows. Release qualification still requires installation,
  compilation, cited context and the local GUI on every downloadable platform.

See [tracked workspaces](docs/WORKSPACES.md) for the new workspace flow and
[release validation](docs/RELEASE_VALIDATION.md) for the publication gates.

## 0.4.0 (2026-09-05)

The [published 0.4.0 release](https://github.com/neuroforge-io/RKC/releases/tag/v0.4.0)
added the graphical starting point, local folder selection, optional GitHub
connections, bounded browser collections, and checksum-verifying portable
installers for Linux, macOS and Windows on amd64 and arm64.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
