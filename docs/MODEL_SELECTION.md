# Local model selection

RKC treats model selection as a measured operating-point decision, not a model
card popularity contest. The non-negotiable local envelope is one CPU core,
3 GiB `memory.high`, 3.5 GiB `memory.max`, 256 MiB swap, and one model role
loaded at a time. Deterministic compilation and lexical/graph retrieval remain
the fallback if no candidate passes.

## Decision

The active generation candidate is the official llama.cpp conversion of
Qwen3.5 0.8B Q4_0. Gemma 4 E2B QAT Q4_0 and Qwen3.5 2B Q4_K_M remain retained
comparison candidates; the embedding candidate is Qwen3 Embedding 0.6B Q8_0.
All are Apache-2.0, checksum-pinned, downloaded on demand, and never bundled
in an RKC release. The active generation candidate is 563,036,064 bytes and
advertises a 262,144-token native context; RKC targets a bounded 32,768-token
operating point. The embedding candidate is 639,150,592 bytes and is qualified
separately at 8,192 tokens. Exact
revisions, filenames, byte counts, SHA-256 digests, redirect policy, and source
licenses are in [`models/models.lock.json`](../models/models.lock.json).

The candidates become defaults only after the real two-role qualification in
[`rkc-local-model-v1.json`](../models/qualification/rkc-local-model-v1.json)
passes and its raw receipt is manually reviewed. Promotion is never automatic.

## 2026-07-27 guarded evaluation outcome

No local model is promoted as the RKC default.

Qwen's own 0.8B model card positions this parameter scale for prototyping,
task-specific fine-tuning, and research or development. RKC therefore treats
the official 563 MB Q4_0 conversion only as a tightly scoped candidate: it
must pass every grounded repository-answer, injection, exact-32K, latency,
memory, and embedding-pair gate before it can be considered for manual
promotion. Upstream positioning alone is not a production-quality claim.

The full guarded qualification confirmed that the 0.8B candidate is not
satisfactory for RKC's production intelligence role. It completed the five
short cases in 2.80–7.42 seconds, with roughly 69–74 prompt tokens/second and
21–22 generated tokens/second, but passed only two of six cases overall. It
omitted required cited facts in the exact-signature and injection cases and
failed the explicit unresolved-question requirement for insufficient evidence.
The tokenizer-exact 32,384-token request reached the five-minute deadline
without completing. Generation metrics were:

- case pass rate `0.3333333333`;
- schema-valid rate `0.8333333333`;
- citation-valid rate `0.5`;
- required-fact recall `0.5`;
- injection-canary and unsupported-claim rates `0.0`;
- maximum short-case latency `7,419.040 ms`;
- long-context latency `300,155.577 ms`, over the `300,000 ms` ceiling;
- peak process RSS `1,653,170,176` bytes and peak protected-cgroup charge
  `1,551,167,488` bytes, with no memory-pressure, swap, max, or OOM event.

The independently loaded Qwen3 Embedding 0.6B Q8_0 role passed: recall-at-1 was
`1.0`, minimum cosine margin `0.0731974186`, maximum norm error
`0.0000000474562`, peak RSS `1,242,824,704` bytes, and wall time
`12,661.429 ms`. RKC's promotion contract requires both roles, so this isolated
embedding pass is retained as evidence but is not promoted around the failed
generation role. The private raw report SHA-256 is
`e3f29b1dac6f95237b6b796755c6914bd76f881d1b1efe3762ee79e5672c8eac`;
it records `promotion_performed: false` and `defaults_changed: false`.

The Qwen generation candidate was loaded through the pinned native
`llama.cpp` runtime inside RKC's one-CPU, 3 GiB `memory.high`, 3.5 GiB
`memory.max`, nice-19, idle-I/O guard. Explicit flash attention and a
512-token prefill batch reduced current cgroup memory from approximately
1.89 GiB to 1.46 GiB with no swap. The mandatory tokenizer-exact 32K case,
however, reached only 5,632 of 32,384 input tokens after 269.08 seconds, with
average prefill throughput declining to 20.93 tokens/second. The run was
stopped rather than occupying the protected single CPU for an impractical
extended period.

Gemma 4 E2B was then evaluated through the checksum-pinned b10082 native
runtime after raising the protected host envelope to 3 GiB `memory.high` and
3.5 GiB `memory.max`. On one Ryzen 5 5500 core, a 512-token prompt ran at
19.96 tokens/second and 32 generated tokens ran at 7.95 tokens/second. The
cgroup recorded no pressure, max, OOM, or swap event; charged memory observed
during the run was approximately 1.9 GiB. `/usr/bin/time` reported 4,386,940
KiB maximum process RSS because Linux counts mmap-backed model pages in that
per-process figure, so both measurements are retained rather than conflated.

The required RKC JSON-schema generation path did not work: llama.cpp b10082
loaded the official model but failed to initialize its grammar sampler before
producing a token. This is a hard compatibility failure for RKC's
citation-checked response contract. At the measured prompt rate, a 32,384-token
prefill would also take roughly 27 minutes before generation, which is not a
satisfactory interactive default on the guarded one-core profile.

The earlier Gemma and Qwen 2B runs were bounded measurements rather than
qualification receipts. The complete Qwen 0.8B/Qwen embedding run is a failed
qualification receipt and cannot supply a pair-level quality pass. All
generation and embedding assets therefore remain `unqualified`,
`default_generation_model` and `default_embedding_model` remain `null`, and
model-backed commands continue to fail closed. RKC does not substitute a
weaker quantization, remove schema enforcement, or silently reduce the 32K gate
to manufacture a passing default. The rejected candidates remain pinned for
reproducible comparison, not selected for users.

Primary sources:

- [Gemma 4 overview](https://ai.google.dev/gemma/docs/core)
- [official Gemma 4 E2B model](https://huggingface.co/google/gemma-4-E2B-it)
- [official Gemma 4 E2B QAT Q4 GGUF](https://huggingface.co/google/gemma-4-E2B-it-qat-q4_0-gguf)
- [Qwen3.5-2B model card](https://huggingface.co/Qwen/Qwen3.5-2B)
- [Qwen3.5-0.8B model card](https://huggingface.co/Qwen/Qwen3.5-0.8B)
- [official llama.cpp Qwen3.5-0.8B GGUF](https://huggingface.co/ggml-org/Qwen3.5-0.8B-GGUF)
- [Qwen3 embedding GGUF model card](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF)

## Qualification gates

Generation must pass repeated deterministic structured-output cases for exact
signatures, cited error behavior, insufficient-evidence abstention, repository
prompt-injection resistance, graph relations, and tokenizer-counted
head/middle/tail retrieval across a filled 32K context. Every response must be
valid JSON, cite only supplied evidence, recall required facts, emit no
unsupported claims or injection canary, and remain below the hard memory limit.

Embedding must achieve perfect recall-at-1 on the qualification corpus, a
minimum cosine margin of 0.02, the locked 1,024 dimensions, normalized vectors,
and the same memory ceiling. Generation and embedding execute sequentially.

If either role fails, RKC retains no default model. Lexical retrieval, GraphRAG
expansion, evidence packets, deterministic documentation, and every canonical
repository fact remain fully usable without a model.
