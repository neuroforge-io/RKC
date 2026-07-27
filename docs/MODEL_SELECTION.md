# Local model selection

RKC treats model selection as a measured operating-point decision, not a model
card popularity contest. The non-negotiable local envelope is one CPU core,
3 GiB `memory.high`, 3.5 GiB `memory.max`, 256 MiB swap, and one model role
loaded at a time. Deterministic compilation and lexical/graph retrieval remain
the fallback if no candidate passes.

## Decision

No generation model is selected as RKC's default. IBM Granite 4.0 H 1B
Q5_K_M was the strongest plausible Apache-2.0 candidate in the protected local
envelope, but the complete RKC qualification rejected it. The locked
qualification specification remains pointed at that exact rejected asset so
the result can be reproduced; this does not make it an active or recommended
default. The embedding candidate remains Qwen3 Embedding 0.6B Q8_0, qualified
separately at 8,192 tokens.

Granite is an Apache-2.0, 1.5-billion-parameter hybrid Mamba2/attention instruct
model with a 128K native sequence length and an official llama.cpp-compatible
GGUF. The locked Q5_K_M asset is 1,048,556,768 bytes. It was evaluated because
its official results report `82.37` strict instruction compliance, `73`
HumanEval, `68` HumanEval+, `69` MBPP, `60` MBPP+, and `50.21` BFCL v3 for the
hybrid 1B variant. Its model card explicitly includes extraction, question
answering, RAG, code, and function calling among intended capabilities. Those
upstream scores justified evaluation; they did not override RKC's
repository-specific gates.

The guarded run passed only two of six generation cases. It achieved
`0.8333333333` schema validity, `0.8333333333` citation validity, `0.75`
required-fact recall, a `0.3571428571` unsupported-claim rate, and a
`0.1666666667` injection-canary rate. The five standard cases peaked at
`61,800.716` ms. The tokenizer-exact 32,384-token case reached its
`300,178.653` ms deadline after processing only about 3,072 prompt tokens,
roughly 12 tokens/second. Peak process RSS was `2,702,090,240` bytes and peak
protected-cgroup charge was `1,688,436,736` bytes with no memory failure.
The paired Qwen3 embedding role passed again. Pair-level qualification therefore
failed, no defaults changed, and the private report SHA-256 is
`42361cdc5cd687eb6ceb31fd69d955da20a731f32291fafda8f3256d4f169faa`.

## Current small-model research snapshot

| Candidate | License / context | Why it was considered | RKC decision |
|---|---|---|---|
| Granite 4 H 1B Q5_K_M | Apache-2.0 / 128K | Strong instruction, code, function-calling, RAG, and hybrid-architecture evidence at 1.5B parameters | Fully measured; rejected on quality, injection, unsupported claims, and exact-32K latency |
| Qwen3 1.7B Q8_0 | Apache-2.0 / 32K | Strong modern small-model reasoning and instruction following | Official 1.83 GB dense GGUF cannot plausibly close the measured one-core prefill gap |
| SmolLM3 3B | Apache-2.0 / 64K | Competitive reasoning and long-context positioning | 3B dense compute and context footprint are outside the viable guarded operating point |
| LFM2.5 1.2B Instruct | LFM license / 32K | Excellent published edge-CPU throughput and sub-2B results | Excluded: not Apache-2.0, and upstream does not position it as a programming/knowledge default |
| Falcon-H1 1.5B Instruct | Falcon/custom / long context | Strong published small-model benchmark results | Excluded by the required Apache-2.0 model-license policy |
| Gemma 4 E2B QAT Q4 | Gemma terms / 32K test point | New efficient architecture and official quantization | Measured; rejected on grammar compatibility and roughly 27-minute projected 32K prefill |
| Laguna XS.2 | Apache-2.0 / 262K | Very strong current agentic-code and SWE benchmark positioning with 3B active parameters | 33B total weights cannot fit the 3.5 GiB hard ceiling |

The production latency gate requires at least about 108 prompt tokens/second to
consume 32,384 input tokens within 300 seconds. The guarded measurements were
about 12 tokens/second for Granite 4 H 1B and about 21 tokens/second for Qwen3.5
0.8B. A larger dense Apache-2.0 candidate cannot bridge that gap on the same
one-core host. The only researched model claiming the required edge-class speed
has a non-Apache license and weaker task positioning. Further heavyweight
downloads would therefore consume resources without a plausible promotion
path.

Qwen3 1.7B Q8_0 and SmolLM3 3B were considered as Apache-2.0 comparisons, but
their larger dense footprints cannot close the measured prompt-throughput gap
under the same one-core ceiling. Qwen's official GGUF is 1,834,426,016 bytes
and SmolLM3 has 3B parameters. LFM2.5 1.2B and Falcon-H1 1.5B publish excellent
edge or small-model results, but are excluded because their model licenses are
not Apache-2.0. Gemma 4 E2B, Qwen3.5 2B, and Qwen3.5 0.8B remain retained
measured comparisons.

Every included asset is checksum-pinned, downloaded on demand, and never
bundled in an RKC release. Exact revisions, filenames, byte counts, SHA-256
digests, redirect policy, and source licenses are in
[`models/models.lock.json`](../models/models.lock.json).

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

- [IBM Granite 4.0 H 1B model card](https://huggingface.co/ibm-granite/granite-4.0-h-1b)
- [official IBM Granite 4.0 H 1B GGUF](https://huggingface.co/ibm-granite/granite-4.0-h-1b-GGUF)
- [Qwen3 1.7B model card](https://huggingface.co/Qwen/Qwen3-1.7B)
- [official Qwen3 1.7B GGUF](https://huggingface.co/Qwen/Qwen3-1.7B-GGUF)
- [SmolLM3 3B model card](https://huggingface.co/HuggingFaceTB/SmolLM3-3B)
- [LFM2.5 1.2B Instruct model card](https://huggingface.co/LiquidAI/LFM2.5-1.2B-Instruct)
- [Falcon-H1 1.5B Instruct model card](https://huggingface.co/tiiuae/Falcon-H1-1.5B-Instruct)
- [Poolside Laguna XS.2 model card](https://huggingface.co/poolside/Laguna-XS.2)
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
