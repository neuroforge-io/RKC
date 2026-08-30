# Local model runtime

This RKC document is released under the NeuroForgeIO project MIT License.
Model weights, llama.cpp, and other external inputs retain their own terms;
see [`BRANDING_AND_ATTRIBUTION.md`](BRANDING_AND_ATTRIBUTION.md).

The model subsystem is optional and derived. It improves readability; it does not
determine inventory completeness, symbol existence, signatures, APIs, call
resolution, or quality denominators.

RKC's portable local provider is built around the upstream
[`llama.cpp` command-line and server interfaces](https://github.com/ggml-org/llama.cpp),
which support quantized CPU execution and structured/JSON-oriented serving.
The pinned build receipt and RKC's stricter resource, license, prompt, and
qualification contracts remain authoritative; upstream capability alone never
promotes a model or enables a default.

## Workflow

```text
subject selection
  -> exact/lexical search
  -> bounded graph expansion
  -> evidence selection
  -> redacted source excerpts
  -> coherent EvidencePacket
  -> local provider
  -> structured response
  -> claim and summary validation
  -> derived document records
```

## Evidence packet

A packet contains:

- task and subject identity;
- selected subject node;
- bounded related nodes;
- bounded incoming and outgoing edges;
- evidence records;
- source excerpts and source ranges;
- diagnostics;
- allowed claim categories;
- citation and inference policy;
- output limits;
- explicit truncation and missing-source records.

When related nodes are omitted due to limits, edges referring to them are also
omitted. A packet never presents dangling context and then asks the model to
invent the absent endpoint.

## Provider contract

```go
type Provider interface {
    Descriptor() ModelDescriptor
    Supports(task Task) bool
    Generate(ctx context.Context, request Request) (Response, error)
    Close() error
}
```

A provider descriptor includes model digest, format, quantization, context
limit, implementation version, deterministic controls, and estimated memory.

## `llama.cpp` CLI provider

The reference provider:

- inspects the GGUF file and estimates weight memory;
- estimates KV cache and runtime overhead;
- rejects requests exceeding configured memory before launch;
- invokes the executable directly with bounded context, output, batch, threads,
  seed, and timeout;
- forces CPU-only execution and rejects additional arguments that could
  override RKC-controlled resource or GPU policy;
- on Linux, fails closed unless the process can run in a user cgroup with one
  CPU core, CPU weight 1, nice level 19, idle I/O, bounded tasks, and hard
  memory/swap limits; I/O cgroup weight 1 is additionally required wherever
  the user manager delegates that controller (including CI);
- sanitizes the process environment;
- writes the bounded prompt to a private, short-lived `0600` file instead of
  exposing repository excerpts in process arguments;
- bounds stdout and stderr;
- records cgroup memory usage when available and stops the entire transient
  unit—not only its launcher—on cancellation or limit failure;
- extracts one JSON object from noisy CLI output;
- records the bounded runtime usage available from the provider.

The provider tests use a fake executable. No real model weights are bundled.

## Pinned native runtime and candidate assets

[`models/models.lock.json`](../models/models.lock.json) is the only supported
download manifest. It pins every byte count, SHA-256, upstream revision, HTTPS
redirect host, license, and qualification state. The downloader accepts no URL
or output filename from the shell, rejects links and non-regular files, streams
to an exact byte ceiling, and publishes a verified inode without replacing an
existing path. Cached multi-gigabyte verification rechecks ERAIS priority every
16 MiB, and the `verify` command requires the same live resource guard as a
download. A conservative free-space reserve is checked on the exact cache
filesystem before a temporary inode is created. Model downloads require
explicit `Apache-2.0` acceptance.

The current runtime pin is `llama.cpp` `b10082` at commit
`fb0e6b621917488d623437349fb5361e0ac21c70`. Its exact source archive hash and
portable CPU-only CMake policy are in the lock. RKC disables GPU, RPC, remote
model-fetch, prebuilt UI, and optional accelerator backends in this build. Two
profiles are available:

- `portable`: disables native and post-baseline x86 instruction selection;
- `native`: enables local CPU tuning while retaining the CPU-only and no-egress
  boundary.

Both profiles build `llama-cli`, `llama-server`, `llama-embedding`, and
`llama-bench` from the same verified MIT-licensed source. The build receipt
requires and hashes that exact binary inventory, including the direct embedding
provider executable. It also binds retained upstream `source/LICENSE` by path,
byte count, SHA-256, `MIT` SPDX expression, and the exact revision license URL;
fresh publication and runtime reuse both fail closed if the license or metadata
is missing or changed. CMake configures the upstream examples tree because the
pinned `llama-embedding` target is defined there, but the build command names
only those four targets. Built source, binaries, weights, and receipts stay
under ignored `.rkc-*` directories and are not part of RKC source or release
archives.

```sh
make model-lock-check
make model-runtime-portable
# Or, on the machine that will run the model:
make model-runtime-native
```

Every fetch, build, and real-model qualification command fails if ERAIS is
active and must prove it is inside the low-priority Linux cgroup before doing
heavy work. That guard limits the workload to one CPU core, 4 GiB operating memory,
4.5 GiB hard memory, 256 MiB swap, 128 tasks, nice 19, idle I/O, and high
OOM-kill preference. Configure and build commands run in private process groups
with fixed deadlines and sub-second ERAIS polling; priority, timeout, or
cancellation terminates and reaps the whole group. Runtime staging also has a
conservative disk-headroom gate, and failed `.building-*` trees are quarantined
and removed only when their original inode identity is still bound.

Five Apache-2.0 generation candidates are locked but deliberately not configured as
defaults:

- `Qwen3.5-0.8B-Q4_0.gguf`, revision
  `8fea620810c4afa23dd6443f999a48574c1611a3`, 563,036,064 bytes, SHA-256
  `57d1997790d1744fba5b40a7317df71ea5e2acee28c47e78f0cce39c0703f8cf`;
- `gemma-4-E2B_q4_0-it.gguf`, revision
  `675cff42a74c774d6cb76f76d8eacb49b48c9b93`, 3,349,516,256 bytes, SHA-256
  `fa401b55b07ee70a54c6dae3903c783a6e65064312529ea57175cb5f8dec6634`;
- `Qwen3.5-2B-Q4_K_M.gguf`, revision
  `7d26695454df6de5fbcce2e58681e62dae06ce43`, 1,396,198,496 bytes, SHA-256
  `57a1085840f497d764a7fc5d346922dbde961efb54cc792ea81d694fd846a1d8`;
- `Qwen3-Embedding-0.6B-Q8_0.gguf`, revision
  `370f27d7550e0def9b39c1f16d3fbaa13aa67728`, 639,150,592 bytes, SHA-256
  `06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439`.

The active Qwen3.5 0.8B candidate and retained Qwen3.5 2B comparison advertise
262,144-token native contexts; Gemma 4 advertises 131,072 and the embedding
candidate advertises 32,768 tokens. RKC's guarded qualification uses 32,768 and
8,192 tokens respectively: upstream generation capacity beyond 32K is not
represented as a measured local operating point. The generation stress case
asks llama.cpp's chat-tokenizer endpoint to construct an exact 32,384-token
input and reserves 384 tokens for structured output, filling the 32,768-token
runtime context without a character-count proxy. Generation and embedding are
loaded sequentially, never concurrently.

Fetching is an explicit, reviewable operation and does not run inference:

```sh
make model-fetch-generation
make model-fetch-embedding
```

The strict qualification corpus is
[`models/qualification/rkc-local-model-v1.json`](../models/qualification/rkc-local-model-v1.json).
It measures strict JSON, evidence citation validity, unsupported facts, prompt
injection resistance, tokenizer-counted head/middle/tail 32K retrieval,
semantic retrieval recall and margin, vector shape/norm, latency, and peak
memory. A run preserves raw responses and vectors in its no-replace report,
never edits the lock, and requires a human to review its receipt before any
future default promotion.

Production responsiveness is a hard qualification contract rather than a
post-hoc observation. Every standard generation case must complete within two
minutes and the exact 32K case within five minutes on the guarded one-CPU
profile. The per-request deadline is enforced while the server is running; a
long-context timeout stops the generation role, terminates its process group,
and produces a bounded failed result instead of consuming the machine for
hours. The retained Qwen3.5 2B Q4_K_M candidate is not eligible for promotion:
its measured 32K path was only 16% complete at the five-minute boundary.
The smaller official Qwen3.5 0.8B Q4_0 candidate also failed the unchanged
gate: only two of six generation cases passed, the required cited-fact and
abstention contracts failed, and the exact 32K request timed out after
`300,155.577 ms`. Its peak protected-cgroup charge was only
`1,551,167,488` bytes, so the rejection is quality and CPU responsiveness,
not memory capacity. The paired Qwen3 embedding role passed every retrieval
threshold, but pair-level promotion remains fail-closed.

Qualification HTTP is restricted to credential-free IP-literal loopback URLs.
It uses an explicit no-proxy opener, refuses every redirect, revalidates the
final response URL, and monitors ERAIS while long requests are pending. If a
higher-priority workload appears, RKC terminates the complete local server
process group before deferring the run.

```sh
make model-qualify \
  MODEL_RUNTIME=.rkc-runtime/llama.cpp/b10082-fb0e6b621917-3e40ee1adf9e-native \
  MODEL_QUALIFICATION_OUTPUT=dist/model-qualification/run-001.json
```

Until both roles pass that real guarded run, deterministic model-disabled
operation remains the supported default. A failed or interrupted run cannot
silently select either candidate.

## Strict local profile

```yaml
provider: llama.cpp
model_class: coder
parameter_class: benchmark-selected-small
quantization: q4
context_tokens: 4096
max_output_tokens: 768
parallel_requests: 1
max_rss_mib: 4608
embeddings: disabled-or-sequential
fallback: deterministic
```

The memory governor estimates:

```text
peak estimate
  = mapped model weights
  + KV cache bytes
  + compute/scratch buffers
  + prompt/output buffers
  + executable and allocator overhead
  + safety margin
```

If the estimate exceeds policy, the runtime reduces packet/context size, splits
the task, selects a smaller model, or disables synthesis. It does not borrow
memory from the operating system through positive thinking.

## Response schema

```json
{
  "claims": [
    {
      "text": "...",
      "category": "purpose",
      "certainty": "supported",
      "evidence_ids": ["evidence:..."]
    }
  ],
  "unresolved_questions": []
}
```

## Validation

A claim is rejected when it:

- contains more than one atomic statement;
- has no evidence citation;
- cites an ID not present in the packet;
- names a code identifier absent from the subject/related records;
- claims unsupported inference when inference is disabled;
- omits certainty;
- exceeds claim or output limits;
- contains malformed structured output;
- uses a certainty state outside the packet policy.

`rkc answer` then performs at most two bounded repair passes by default. A
rejected claim or unresolved question may be converted into a size-limited,
control-free search query; query filter syntax is neutralized, and the text is
never treated as trusted instructions or evidence. Each pass expands retrieval,
re-resolves hits from the immutable canonical bundle, runs the model from
scratch with fixed validator guidance, and applies the complete validator
again. The final compiler selects the attempt with the most accepted claims and
fewest unresolved gaps. It never merges uncited prose across attempts. One
absolute command deadline and the same process/memory guard cover the complete
sequence.

Protocol v1 does not provide an evidence-ID mapping for a free-form summary, so
all provider-authored `summary` values are retained only in the audit record and
rejected from publication. Rendered synthesis is built from individually cited,
accepted claims. A future protocol may add explicit summary-to-claim binding.

## Cache identity

Model-derived output cache keys include:

```text
packet digest
model file digest
provider implementation/version
prompt template digest
generation parameters
validation policy digest
schema version
```

Model output is never reused when source evidence changes.

## Real-model benchmark gate

Before claiming the protected 4 GiB operating / 4.5 GiB hard profile, publish:

1. exact model and GGUF SHA-256;
2. model license;
3. `llama.cpp` commit and build flags;
4. CPU, memory, OS, and kernel;
5. context, batch, thread, and output settings;
6. peak RSS measured externally and internally;
7. tokens per second and task latency;
8. packet corpus and task types;
9. claim acceptance/rejection rates;
10. factual precision against deterministic evidence;
11. comparison with model-disabled documentation;
12. raw logs and reproducible command line.

Until that gate is met, 4.5 GiB is a guarded engineering ceiling, not a
marketing measurement or evidence that any candidate model is satisfactory.

---
_RKC is stewarded by **NeuroForgeIO** and released under the **MIT License**.
Redistributions must retain the copyright and permission notices required by
that license. Attribution to NeuroForgeIO is requested, but is not an additional
license condition._
