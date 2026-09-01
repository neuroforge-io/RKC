# Shell workflows

RKC keeps release, smoke, reproducibility, and resource-safety entry points as
small shell workflows so they can be inspected and run from a minimal checkout.
Every workflow uses strict error handling and is exercised by the guarded CI
release sequence or the static/syntax contracts in
[`scripts/test_shell_workflows.py`](../scripts/test_shell_workflows.py).

## Entry points

| Workflow | Purpose and safe boundary |
| --- | --- |
| [`install.sh`](../install.sh) | Installs the host-built CGO-free commands and the complete license/attribution inventory into a user-owned prefix, rejecting unsafe destinations. |
| [`scripts/benchmark-reference.sh`](../scripts/benchmark-reference.sh) | Runs the bounded reference scan and publishes only benchmark receipts under `dist/benchmark`. |
| [`scripts/build-release-binaries.sh`](../scripts/build-release-binaries.sh) | Rebuilds Linux `amd64` and `arm64` binaries from an immutable, clean commit and records source identity. |
| [`scripts/check-portable-builds.sh`](../scripts/check-portable-builds.sh) | Compiles CGO-free `rkc` and `rkc-mcp` for Linux, macOS, and Windows `amd64`/`arm64` targets in a private temporary directory without publishing artifacts. |
| [`scripts/generate-demo.sh`](../scripts/generate-demo.sh) | Generates the small checked-in demo outputs from an immutable source tree. |
| [`scripts/install-package.sh`](../scripts/install-package.sh) | Installs the complete-package Linux prebuilt for `amd64` or `arm64`, verifies its checksum receipt before delegation to the source installer, and requires neither network nor root access. |
| [`scripts/reproducibility.sh`](../scripts/reproducibility.sh) | Scans the examples twice and compares canonical bundle, coverage, and digest outputs. |
| [`scripts/reproducible-complete-package.sh`](../scripts/reproducible-complete-package.sh) | Assembles the complete distributable twice from independent immutable checkouts and requires byte identity. |
| [`scripts/self-catalogue.sh`](../scripts/self-catalogue.sh) | Builds the RKC-on-RKC atlas from the commit tree without feeding generated output back into the input. Run through `make self-catalogue`. |
| [`scripts/smoke-api.sh`](../scripts/smoke-api.sh) | Starts a loopback-only API over a temporary atlas and verifies health and bounded search. |
| [`scripts/smoke-git-acquisition.sh`](../scripts/smoke-git-acquisition.sh) | Creates a temporary Git repository and verifies constrained `file://` acquisition and provenance. |
| [`scripts/smoke-mcp.sh`](../scripts/smoke-mcp.sh) | Exercises initialize, tool discovery, and a bounded search over the stdio MCP adapter. |
| [`scripts/smoke-reference.sh`](../scripts/smoke-reference.sh) | Runs the reference scan, coverage checks, query, and packet-only synthesis path. |
| [`scripts/test_install.sh`](../scripts/test_install.sh) | Verifies that the installer exposes a portable executable and first-run help without changing the source tree. |
| [`scripts/validate-dco.sh`](../scripts/validate-dco.sh) | Validates signed-off-by trailers and the approved repository ancestry for a commit range. |
| [`scripts/verify-release.sh`](../scripts/verify-release.sh) | Runs the full evidence-producing release validation sequence with exact step inventory and source binding. |
| [`scripts/verify-resource-guard.sh`](../scripts/verify-resource-guard.sh) | Proves the delegated cgroup, CPU/memory/swap/task limits, idle scheduling, and OOM policy of the local guard. |
| [`scripts/with-rkc-limits.sh`](../scripts/with-rkc-limits.sh) | Places local builds, scans, and model work in a subordinate one-core, low-priority cgroup and yields to configured higher-priority workload classes. The strict policy (`RKC_HIGHER_PRIORITY_POLICY=refuse`) refuses to start while higher-priority work is visible; the default `yield` policy starts inside the subordinate envelope and leaves continuous load monitoring to the guarded RKC binary. `RKC_HIGHER_PRIORITY_MARKERS` replaces the generic `torchrun,lm_eval` classes with 1-16 unique lower-case ASCII markers of at most 32 bytes each and 255 bytes total; empty retains the default and invalid values fail closed. `RKC_MEMORY_HIGH_MIB`, `RKC_MEMORY_MAX_MIB`, `RKC_MEMORY_SWAP_MAX_MIB`, and `RKC_GO_MEMORY_LIMIT_MIB` may select only a strictly equal-or-smaller host profile and are validated before systemd is invoked. |

## Operating rules

Use the corresponding `make` target where one exists. On a shared Linux host,
choose the `safe-*` target so systemd user cgroups enforce the documented
default 4 GiB soft / 4.5 GiB hard memory maxima, 256 MiB swap maximum, one CPU core,
idle I/O, and low process priority. The strict policy (`RKC_HIGHER_PRIORITY_POLICY=refuse`)
refuses to start while a visible configured higher-priority workload is active;
the default `yield` policy starts
inside the same subordinate envelope and defers continuous load monitoring to
the guarded RKC binary. Release and self-catalogue workflows
also refuse dirty source, symlinked output, model-weight input, and generated
output recursion.

When a co-resident workload needs a larger host-memory reserve, lower all four
memory profile values explicitly and use one stage worker. Example:

```sh
RKC_MEMORY_HIGH_MIB=1280 RKC_MEMORY_MAX_MIB=1536 \
RKC_MEMORY_SWAP_MAX_MIB=256 RKC_GO_MEMORY_LIMIT_MIB=1024 \
scripts/with-rkc-limits.sh ./bin/rkc scan --stage-workers 1 --no-python /path/to/repository
```

The scripts are intentionally not a general-purpose deployment supervisor:
team-scale service orchestration, authentication, and remote storage remain
outside this reference release. Their source-level contracts are checked on
every Python test discovery run, while the guarded CI workflow supplies the
runtime evidence for operations that create release or temporary artifacts.

RKC documentation and workflows are published by NeuroForgeIO, copyright 2026
NeuroForgeIO and RKC contributors, and Apache-2.0 licensed. Redistributions
must preserve the applicable
[`NOTICE`](../NOTICE); review
[`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md) for applicable third-party
obligations.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
