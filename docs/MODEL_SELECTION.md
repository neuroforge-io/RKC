# Local model selection

RKC treats model selection as a measured operating-point decision, not a model
card popularity contest. The non-negotiable local envelope is one CPU core,
3 GiB `memory.high`, 3.5 GiB `memory.max`, 256 MiB swap, and one model role
loaded at a time. Deterministic compilation and lexical/graph retrieval remain
the fallback if no candidate passes.

## Decision

The generation candidate is
`Qwen_Qwen3.5-2B-Q4_K_M.gguf` and the embedding candidate is
`Qwen3-Embedding-0.6B-Q8_0.gguf`. Both are Apache-2.0, checksum-pinned,
downloaded on demand, and never bundled in an RKC release. The generation
candidate is 1,396,198,496 bytes and advertises a 262,144-token native context;
RKC qualifies a bounded 32,768-token operating point. The embedding candidate
is 639,150,592 bytes and is qualified separately at 8,192 tokens. Exact
revisions, filenames, byte counts, SHA-256 digests, redirect policy, and source
licenses are in [`models/models.lock.json`](../models/models.lock.json).

The candidates become defaults only after the real two-role qualification in
[`rkc-local-model-v1.json`](../models/qualification/rkc-local-model-v1.json)
passes and its raw receipt is manually reviewed. Promotion is never automatic.

## 2026-07-27 guarded qualification outcome

No local model is promoted as the RKC default.

The Qwen generation candidate was loaded through the pinned native
`llama.cpp` runtime inside RKC's one-CPU, 3 GiB `memory.high`, 3.5 GiB
`memory.max`, nice-19, idle-I/O guard. Explicit flash attention and a
512-token prefill batch reduced current cgroup memory from approximately
1.89 GiB to 1.46 GiB with no swap. The mandatory tokenizer-exact 32K case,
however, reached only 5,632 of 32,384 input tokens after 269.08 seconds, with
average prefill throughput declining to 20.93 tokens/second. The run was
stopped rather than occupying the protected single CPU for an impractical
extended period.

That interrupted run is not a qualification receipt and supplies no quality
pass. The generation and embedding assets therefore remain `unqualified`,
`default_generation_model` and `default_embedding_model` remain `null`, and
model-backed commands continue to fail closed unless a future candidate
completes every gate. RKC does not substitute a weaker quantization or silently
reduce the 32K requirement to manufacture a passing default.

## Why Gemma 4 E2B is not the RKC default

Gemma 4 E2B is a strong modern Apache-2.0 candidate with a 128K context window,
and Google publishes an official QAT Q4 GGUF. However, the official
`gemma-4-E2B_q4_0-it.gguf` is 3,349,516,256 bytes before KV cache, executable,
scratch buffers, prompt buffers, or safety margin. It exceeds RKC's
2,684,354,560-byte hard cgroup limit by 665,161,696 bytes on weights alone.
Using a more destructive community quantization merely to fit would violate the
quality requirement. Gemma 4 is therefore rejected for the 1.0 local default,
not rejected as a model family.

Primary sources:

- [Gemma 4 overview](https://ai.google.dev/gemma/docs/core)
- [official Gemma 4 E2B model](https://huggingface.co/google/gemma-4-E2B-it)
- [official Gemma 4 E2B QAT Q4 GGUF](https://huggingface.co/google/gemma-4-E2B-it-qat-q4_0-gguf)
- [Qwen3.5-2B model card](https://huggingface.co/Qwen/Qwen3.5-2B)
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
