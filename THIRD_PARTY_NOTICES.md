# Third-party notices

RKC-owned source code, schemas, documentation, and built-in plugins are licensed
under Apache-2.0 as stated in [`LICENSE`](LICENSE). Third-party components retain
their original licenses; inclusion here does not relicense them as Apache-2.0.

## Components linked into RKC executables

### Go runtime and standard library

- Component: Go runtime and standard library
- Copyright: Copyright 2009 The Go Authors.
- License: BSD-3-Clause, with the Go additional patent grant
- Source: <https://go.dev/>
- License text: [`LICENSES/Go.txt`](LICENSES/Go.txt)

RKC executables are built with Go and contain linked portions of the Go runtime
and standard library. The exact toolchain version used for an executable remains
available in its Go build metadata.

### CGO-free SQLite Go module graph

RKC uses `modernc.org/sqlite v1.54.0` as its CGO-free SQLite driver. The exact
selected module graph, Go module checksums, immutable Go proxy source URLs,
license expressions, and SHA-256 hashes of every preserved license file are
locked in [`third_party/go-modules.lock.json`](third_party/go-modules.lock.json).
The dependency is BSD-3-Clause licensed and embeds SQLite, whose deliverable
code is dedicated to the public domain. `modernc.org/libc v1.74.1` is the exact
selected libc implementation; its upstream third-party notice includes the Go,
musl libc, go-netdb, and NixOS/nixpkgs terms and attributions used by that
module.

The complete reviewed graph and preserved upstream license texts are:

- `github.com/dustin/go-humanize v1.0.1` — MIT —
  [`LICENSES/go-modules/github.com/dustin/go-humanize@v1.0.1/LICENSE`](LICENSES/go-modules/github.com/dustin/go-humanize@v1.0.1/LICENSE)
- `github.com/google/pprof v0.0.0-20250317173921-a4b03ec1a45e` —
  Apache-2.0 —
  [`LICENSES/go-modules/github.com/google/pprof@v0.0.0-20250317173921-a4b03ec1a45e/LICENSE`](LICENSES/go-modules/github.com/google/pprof@v0.0.0-20250317173921-a4b03ec1a45e/LICENSE)
- `github.com/google/uuid v1.6.0` — BSD-3-Clause —
  [`LICENSES/go-modules/github.com/google/uuid@v1.6.0/LICENSE`](LICENSES/go-modules/github.com/google/uuid@v1.6.0/LICENSE)
- `github.com/mattn/go-isatty v0.0.20` — MIT —
  [`LICENSES/go-modules/github.com/mattn/go-isatty@v0.0.20/LICENSE`](LICENSES/go-modules/github.com/mattn/go-isatty@v0.0.20/LICENSE)
- `github.com/ncruces/go-strftime v1.0.0` — MIT —
  [`LICENSES/go-modules/github.com/ncruces/go-strftime@v1.0.0/LICENSE`](LICENSES/go-modules/github.com/ncruces/go-strftime@v1.0.0/LICENSE)
- `github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec`
  — BSD-3-Clause —
  [`LICENSES/go-modules/github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec/LICENSE`](LICENSES/go-modules/github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec/LICENSE)
- `golang.org/x/sys v0.46.0` — BSD-3-Clause —
  [`LICENSES/go-modules/golang.org/x/sys@v0.46.0/LICENSE`](LICENSES/go-modules/golang.org/x/sys@v0.46.0/LICENSE)
- `modernc.org/fileutil v1.4.0` — BSD-3-Clause —
  [`LICENSES/go-modules/modernc.org/fileutil@v1.4.0/LICENSE`](LICENSES/go-modules/modernc.org/fileutil@v1.4.0/LICENSE)
- `modernc.org/libc v1.74.1` — BSD-3-Clause plus preserved upstream component
  notices —
  [`LICENSES/go-modules/modernc.org/libc@v1.74.1/LICENSE`](LICENSES/go-modules/modernc.org/libc@v1.74.1/LICENSE),
  [`LICENSES/go-modules/modernc.org/libc@v1.74.1/LICENSE-3RD-PARTY.md`](LICENSES/go-modules/modernc.org/libc@v1.74.1/LICENSE-3RD-PARTY.md)
- `modernc.org/mathutil v1.7.1` — BSD-3-Clause —
  [`LICENSES/go-modules/modernc.org/mathutil@v1.7.1/LICENSE`](LICENSES/go-modules/modernc.org/mathutil@v1.7.1/LICENSE)
- `modernc.org/memory v1.11.0` — BSD-3-Clause, including the preserved Go and
  mmap-go derivative terms —
  [`LICENSES/go-modules/modernc.org/memory@v1.11.0/LICENSE`](LICENSES/go-modules/modernc.org/memory@v1.11.0/LICENSE),
  [`LICENSES/go-modules/modernc.org/memory@v1.11.0/LICENSE-GO`](LICENSES/go-modules/modernc.org/memory@v1.11.0/LICENSE-GO),
  [`LICENSES/go-modules/modernc.org/memory@v1.11.0/LICENSE-MMAP-GO`](LICENSES/go-modules/modernc.org/memory@v1.11.0/LICENSE-MMAP-GO)
- `modernc.org/sqlite v1.54.0` — BSD-3-Clause driver and public-domain SQLite
  deliverable code —
  [`LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE`](LICENSES/go-modules/modernc.org/sqlite@v1.54.0/LICENSE),
  [`LICENSES/go-modules/modernc.org/sqlite@v1.54.0/SQLITE-LICENSE`](LICENSES/go-modules/modernc.org/sqlite@v1.54.0/SQLITE-LICENSE)

## Components not bundled

The RKC source and complete binary archives do not bundle model weights or a
`llama.cpp` source tree or executable. The optional bootstrap downloads exact
objects from [`models/models.lock.json`](models/models.lock.json) into ignored
local directories only. The downloaded software and models retain their own
licenses:

- `llama.cpp` is MIT licensed by its upstream contributors. RKC pins release
  `b10082` and the full source revision and archive digest in the model lock.
- Qwen3.5-2B and the locked `Qwen3.5-2B-Q4_K_M` GGUF derivative are
  Apache-2.0 licensed. The quantized file is attributed to Bartowski and the
  underlying model to Qwen.
- Qwen3-Embedding-0.6B and its official locked Q8_0 GGUF are Apache-2.0
  licensed by Qwen.

Downloading these components does not add them to an RKC distribution. Anyone
who redistributes a combined package must preserve each upstream copyright,
license, NOTICE obligations where applicable, model attribution, and the exact
corresponding source/provenance record. RKC release checks continue to reject
tracked native binaries and model weights.

The optional container uses an immutable official Alpine Linux base and contains
Alpine/BusyBox/musl system components under their respective licenses. Their APK
package metadata and upstream notices remain authoritative and must be preserved.
Container publication requires a component-level SBOM and license report; this
source-tree notice is not a substitute for that image inventory. Python is not
present in the runtime image.
