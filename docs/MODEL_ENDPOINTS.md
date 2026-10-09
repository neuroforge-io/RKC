# Model endpoints and ChatGPT context

Compilation, search and cited context work without a model. `rkc answer` can
also send its bounded evidence packet to an explicitly selected,
credential-free OpenAI-compatible server on this computer. Portable profiles
also connect explicit HTTPS APIs; start with [Model providers](MODEL_PROVIDERS.md)
for presets, native Claude/Gemini/OpenAI protocols and supported client sign-in.
The model layer is opt-in and has no qualified default. It does not download weights, start a
server or verify the server's model identity, resource use or answer quality.

## Choose a supported path

| Connection | What RKC supports | Verification boundary |
| --- | --- | --- |
| Local OpenAI-compatible model | `answer --provider openai-compatible --endpoint … --model-name …` | Explicit structured or native extractive profile, loopback HTTP and canonical citation checks; real model quality requires a separate evaluation |
| Qualified local GGUF | Existing `answer --provider llama.cpp` and model/runtime receipts | Existing supply-chain and qualification gates; no default model is selected |
| ChatGPT conversation | Export cited Markdown with `rkc context` and review it before manually sharing | Context export works locally; no ChatGPT conversation was run for this change |
| Local MCP client | Existing `rkc-mcp --dir …` over stdio | Retrieval only; no model invocation or refresh |
| Local Streamable HTTP MCP | `rkc-mcp --transport http --dir … --listen 127.0.0.1:0` | Stateless JSON transport with read-only evidence tools; tested locally, no public listener |
| ChatGPT developer-mode MCP app | Local side implements a supported Streamable HTTP contract | Actual ChatGPT connection still needs separately reviewed reachable hosting, TLS, authentication and account setup |
| OpenAI, Claude or Gemini API | Explicit native API connection profile or answer flags | Remote HTTPS consent, named-environment API credential and bounded validated claims; tested with mocks |
| Your API or NeuroForge API | Explicit compatible API connection profile | HTTPS remote consent; bearer environment credential or explicit anonymous deployment; see [Model providers](MODEL_PROVIDERS.md) |

A ChatGPT subscription is not an OpenAI API credential. OpenAI's API has its
own bearer authentication and project usage. Do not copy ChatGPT session
cookies into RKC. See the official [API authentication reference](https://developers.openai.com/api/reference/overview).
ChatGPT developer-mode apps use remote MCP servers with SSE or streaming HTTP;
RKC's local HTTP transport is tested against that protocol, but ChatGPT cannot
reach a loopback URL on this computer. Its stdio launch configuration also
cannot be pasted into the remote-server field.
See [official ChatGPT developer-mode documentation](https://developers.openai.com/api/docs/guides/developer-mode)
and [RKC's MCP contract](MCP.md).

## Use an existing local server

First compile a source collection and inspect its context. Then, if you already
have a suitable server, supply the full chat-completions URL and exact model ID:

```sh
rkc answer --dir ./fictional-atlas \
  --provider openai-compatible \
  --endpoint http://127.0.0.1:8080/v1/chat/completions \
  --model-name YOUR_LOCAL_MODEL_ID \
  --context 4096 --max-output 768 --timeout 60s --json \
  "Lantern lending period"
```

Use the model ID configured by your server. The example does not assert that
a server exists at port 8080. RKC appends no path and performs no model-list discovery. The transport does
not retry failed HTTP requests; grounding may make the initial call plus up to
`--repair-passes` additional model calls (two by default), under one deadline.
`--provider` is selected on the command line;
the configuration file's qualified local-model policy is unchanged. GGUF
`--model`, model-lock, runtime-receipt, threads, batch-size and process RSS flags
cannot be mixed with this path.

The default `--endpoint-profile structured-claims` requires non-streaming chat completions with `messages`,
`temperature: 0`, `max_tokens` and strict `response_format` JSON schema. It
must return exactly one choice with `finish_reason: "stop"` and a complete JSON
object containing `claims` and `unresolved_questions`. Provider compatibility
does not mean every Ollama, llama-server or model version supports this schema.
An incompatible server fails visibly; RKC never silently drops the schema or
falls back to plaintext.

### Restricted plaintext providers

For an existing local mock or service implementing NeuroForge's native request
subset, explicitly select the extractive profile:

```sh
rkc answer --dir ./fictional-atlas \
  --provider openai-compatible \
  --endpoint http://127.0.0.1:8080/v1/chat/completions \
  --model-name YOUR_LOCAL_MODEL_ID \
  --endpoint-profile neuroforge-native-extractive \
  --context 512 --max-output 128 --timeout 60s --json \
  "Lantern lending period"
```

This profile sends only `model`, one user `messages` entry, `max_tokens`,
`stream: false` and `n: 1`. It sends no schema, temperature or system message.
Its prompt is at most 2,048 UTF-8 bytes, output at most 128 tokens and configured
context at most 512 tokens. These are explicit adapter controls, not measured
server capacity or proof of deterministic sampling.

The prompt presents complete atomic Markdown source lines, already bound to
canonical selected evidence and exactly one source owner. Truncated excerpts,
duplicate or ambiguous bindings, unmatched source ranges, overlarge candidate
sets and unsupported categories fail closed. The entire returned text must
equal exactly one admitted line: no paraphrase, invented number, additional
prose, JSON object, Markdown wrapper or trailing newline is accepted. The
adapter derives a single `constraint` quotation claim and its citation, then
runs the existing claim and canonical-ownership validators. Missing complete
source candidates cause an error without an HTTP call. This is an extractive
quotation mode, not general prose generation or proof that a selected quote
answers the question.

The answer provenance records the selected capabilities, adapter protocol
revision and hash/byte count of the actual transmitted prompt. It does not
reuse the longer structured prompt's audit or claim a schema was enforced.
The original direct local adapter retains its loopback-only endpoint rules.
The separate portable API adapter adds explicit HTTPS consent and optional
named-environment authentication, while retaining the profile limits.

For the original credential-free local adapter, endpoint rules are deliberately narrow:

- Use an IP-literal loopback address such as `127.0.0.1` or `[::1]`.
- Hostnames, remote addresses, URL credentials, queries and fragments are rejected.
- HTTP proxy settings and redirects are never used.
- No authentication header is sent and no credential environment is read.
- Prompt text is bounded to 256 KiB and HTTP responses to 1 MiB. Caller
  cancellation and the answer deadline also cover inference.

The external server owns its model process. RKC bounds the client request and
response but cannot enforce that server's context window, CPU or memory limit.
The descriptor therefore reports `external-http` / `openai-compatible-http`,
with no invented weight digest or measured server RSS. Zero temperature is a
request, not proof of reproducibility. Ensure an existing local server's own
policy prevents forwarding data to another host.

Markdown answer packets include bounded, redacted source sections already
present in the canonical bundle. They never reopen a current source file or
trust prose from a retrieval hit.

Every response still goes through the existing canonical evidence and citation
validator. Invented evidence IDs cannot be published. Missing evidence produces
an abstention without calling the endpoint. These are structural grounding
checks; inspect the cited source to assess whether the prose follows from it.

## Fictional novice workflow

Use a new directory containing only invented material. The commands below
create all input themselves and keep generated data outside the source:

```sh
demo_root=$(mktemp -d)
mkdir "$demo_root/source"
cat > "$demo_root/source/handbook.md" <<'EOF'
# Lantern library handbook

This library is wholly fictional test material.

## Lantern lending period

Lantern lending period is 14 days.
Lantern renewals are allowed once.
EOF

rkc scan --no-python --no-git-metadata --no-cache \
  --runs-dir "$demo_root/runs" --out "$demo_root/atlas-v1" \
  --state-dir "$demo_root/state" "$demo_root/source"
rkc query --dir "$demo_root/atlas-v1" --json "Lantern lending period"
rkc context --dir "$demo_root/atlas-v1" --format markdown "Lantern lending period"
```

Inspect `handbook.md`, the returned relative path, source range, evidence IDs,
snapshot and citation. Source ranges may be broader than a shortened excerpt.
To prepare context for a ChatGPT conversation, the last command provides cited
Markdown. Review what you would share and ask the assistant to answer only
from those citations and state missing information. RKC does not automatically
send that export to ChatGPT.

Change the invented period from 14 to 21 days in the fictional handbook. Compile
again with `--out "$demo_root/atlas-v2"` and the same state directory. Repeat
query/context against `atlas-v2`, inspect the new source, then compare:

```sh
rkc diff --format json "$demo_root/atlas-v1" "$demo_root/atlas-v2"
```

The old atlas remains an immutable view of the earlier text. Select the new
atlas explicitly; a stdio MCP process for the old atlas does not watch sources
or switch generations. Tracked-workspace freshness is described in
[Workspaces](WORKSPACES.md).

## Repeatable integration receipt

The included standard-library harness exercises the real binaries with two
fictional source versions and an in-process loopback mock:

```sh
sh scripts/with-rkc-limits.sh make build
sh scripts/with-rkc-limits.sh python3 scripts/synthetic_model_workflow.py \
  --rkc ./bin/rkc --rkc-mcp ./bin/rkc-mcp > /tmp/rkc-fictional-receipt.json
```

It compiles, retrieves JSON/Markdown context, produces a citation-checked mock
answer, fetches evidence through the actual stdio and Streamable HTTP MCP
servers, inspects the cited
fictional source, updates it and repeats. It also verifies missing-evidence
abstention causes zero model calls. Temporary source and atlas directories are
cleaned after the run; the receipt retains the invented source and outputs.
The mock copies a supplied sentence. Its latency and answer correctness are
integration evidence, not a benchmark of real model usefulness.

Repeat with the restricted native contract (still a local mock):

```sh
sh scripts/with-rkc-limits.sh python3 -O scripts/synthetic_model_workflow.py \
  --rkc ./bin/rkc --rkc-mcp ./bin/rkc-mcp \
  --endpoint-profile neuroforge-native-extractive > /tmp/rkc-native-receipt.json
```

The harness verifies initialize, the initialized notification, read-only tool
discovery, search, cited context and `get_evidence` through the HTTP binary.
It compares HTTP/stdio evidence and the recorded prompt digest with the mock's
actual received text, then retains the old atlas context after the source
update. It also resolves the document's four source evidence references,
re-exports each immutable snapshot without normalized source-file copies,
relocates the export, temporarily hides the authored fictional source directory,
checks the imported document fields and verifies tampered imports are rejected.
Export search projection may read the fictional source; the relocated context
import must work without that source path.
See [MCP](MCP.md) for HTTP limits and supported ChatGPT boundaries.

### Existing Sinter context consumer

Sinter's current adapter uses REST `GET /api/v1/context`, not MCP. Its
`rkc-context/v1` parser agrees with RKC's snapshot-bound citation identity:
SHA-256 of snapshot, NUL, object type, NUL, object ID. Imports remain unchanged
until explicitly reread from a newly selected/served atlas. The new MCP
transport does not silently replace that REST route.

The shared [fictional bridge fixture](../fixtures/sinter_context_bridge.json)
retains before/after/old packets, canonical source references, portable import
results and corrupt packet examples. It verifies
14→21 days, changed snapshot/citation IDs, retained old context and rejection of
corrupt citations or a snapshot-only swap. Regenerate the RKC-side fixture from
a new workflow receipt without depending on concurrent Sinter edits:

```sh
sh scripts/with-rkc-limits.sh env PYTHONDONTWRITEBYTECODE=1 python3 -O \
  scripts/verify_sinter_context_contract.py \
  --receipt /tmp/rkc-fictional-receipt.json \
  --fixture-out /tmp/rkc-sinter-context-bridge.json
```

This producer-only check makes no network calls and does not import Sinter or
test its HTTP GET/header handling. Optionally add
`--sinter-source /absolute/path/to/sinter/src` to test the exact previously
audited pure consumer; its source is hash-pinned, and changed code requires
independent review rather than relaxing the pin.

RKC now preserves the complete canonical Markdown document source range and
sorted document/section evidence IDs using existing context fields. Invalid or
missing bindings suppress all references for that row; the excerpt remains
untrusted data. The earlier audited Sinter consumer dropped supplied sources
and imported node names/signatures rather than Markdown bodies. Consumer
changes and refreshed consumer results are independently owned; this fixture
does not assume they have landed. Citation identity, producer digest and source
accuracy are distinct checks. Neither a citation nor a digest proves truth.

## Lightweight model candidate before acquisition

No usable local inference service was established by this evaluation. The
smallest reputable instruction model audited was
[SmolLM2-135M-Instruct](https://huggingface.co/HuggingFaceTB/SmolLM2-135M-Instruct),
Apache-2.0. Ollama lists
[`smollm2:135m-instruct-q4_K_M`](https://ollama.com/library/smollm2:135m-instruct-q4_K_M)
at 105 MB (rounded). Publisher BF16 weights are 269,060,552 bytes. Plan for
roughly 0.5–1 GiB RAM with a native quantized CPU runtime at a 2K context,
plus at least 1 GiB host headroom. Reserve 250 MB for quantized-weight staging
and storage, with runtime/build storage separate (approximately 1–2 GiB,
unverified). These are planning estimates, not measured guarantees.

The audited host had an i5-1135G7 and about 1.61 GiB memory available at the
check; that does not establish sufficient headroom for a new model run.
No model or runtime was acquired, installed, started or qualified. This 135M
model is a small fictional smoke-test candidate with limited instruction
following, not a selected RKC default. A later reviewed test must recheck
resources and pin artifacts before downloading or installing anything.

## Before any remote evaluation

The original local adapter does not provide an egress override. The separate
portable API adapter now supports remote HTTPS only through explicit consent;
see [Model providers](MODEL_PROVIDERS.md). Review the exact URL,
synthetic-only body, authentication mechanism, selected model and verified
cost before making a remote request. Neither credentials nor new access were
created or inspected during this implementation.

The NeuroForge technical source inspected on 2026-09-30 declares
`https://neuroforge.io/v1/models` and
`https://neuroforge.io/v1/chat/completions`. Its public gateway authenticates
issued developer bearer tokens with an `nf_` prefix by checking their digests;
anonymous access is deployment-controlled. This is source-contract evidence,
not a live deployment check. The advertised native model is
`erais-native-qwen3`. Its request subset rejects `response_format`, temperature
and system messages, caps the final user text at 2,048 bytes and output at 128
tokens, and reports `account_billing_complete: false`. The legacy contract also
rejects `response_format`. Cost could not be verified. A compatible endpoint
name alone is insufficient for RKC's grounded-answer protocol.

A possible future **synthetic preflight body**, which was not sent, is:

```json
{"model":"erais-native-qwen3","messages":[{"role":"user","content":"Fictional Lantern library: lending period is 14 days. Repeat that fact only."}],"max_tokens":32,"stream":false,"n":1}
```

The local extractive profile adapted that restricted request subset and
validated exact source quotations. Its original receipt used mocks and rejected
remote URLs; the later portable API adapter adds an explicit remote connection
without extending those live-inference or quality receipts. A separate task's metadata-only `/v1/models` GET
was authorized by the parent; this implementation did not duplicate it. No
generation POST was authorized or sent by this RKC task. The parent forwarded that GET's live
HTTP 200 JSON: `erais-native-qwen3`, text-only buffered output, a 128-token output
limit, a 2,048-byte question limit, and no qualified general-chat/assistant
quality. It confirms `account_billing_complete: false` without pricing. These
limits align with the explicit extractive profile; metadata availability does
not verify live inference, generation authentication, quality or zero cost.
The parent later reported a separate authorized anonymous synthetic completion:
HTTP 200 in 7.164 seconds, answer `4` to a fictional four-cubes note,
`finish_reason: stop`, 54 prompt and 2 completion tokens. This establishes one
basic completion only; it neither tests RKC's extractive protocol nor qualifies
general assistant quality or pricing. This task did not duplicate that request.

That small preflight body fits the inspected native request shape, but it neither tests
RKC's JSON claims protocol nor establishes live availability, quality or cost.
An OpenAI API evaluation would likewise require a separately authorized API
bearer credential and a selected model's current [API price](https://developers.openai.com/api/docs/pricing).
No remote cost is assumed to be zero.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
