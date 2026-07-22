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
