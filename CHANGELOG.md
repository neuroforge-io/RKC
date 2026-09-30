# Release history

This NeuroForgeIO-published RKC documentation is copyright 2026 NeuroForgeIO
and RKC contributors and Apache-2.0 licensed.

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
