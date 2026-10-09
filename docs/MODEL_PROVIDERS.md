# Connect a model or use your existing AI account

RKC compiles, searches, explains source records and exports cited context without
a model. When you want generated answers, choose an existing local server or an
API, save a small connection profile, and reuse it across projects. For a
ChatGPT, Claude or Google account, keep sign-in in the supported client and give
that client RKC context or its read-only MCP tools.

## Find the right connection

```sh
rkc providers list
rkc providers login-guide
```

| You have | Connection | Credential handling |
| --- | --- | --- |
| Ollama or LM Studio | Local OpenAI-compatible HTTP server | Anonymous by default; optional named environment credential |
| Your own API or gateway | Exact OpenAI-compatible URL | Remote HTTPS requires consent; bearer token read from the named environment variable |
| An OpenAI API key | Native `openai` connection | `OPENAI_API_KEY`; Chat Completions structured output |
| A Claude API key | Native `anthropic` connection | `ANTHROPIC_API_KEY`; Messages structured output |
| A Gemini API key | Native `gemini` connection | `GEMINI_API_KEY`; GenerateContent structured output |
| NeuroForge native API access | Restricted extractive connection | `NEUROFORGE_API_KEY`, or explicitly anonymous if your deployment permits it |
| ChatGPT, Claude Code or Gemini CLI sign-in | Supported client plus cited context/MCP | RKC does not read or copy that client's credentials |
| A qualified local GGUF runtime | Existing `llama.cpp` answer path | Model lock, runtime receipts and qualification gates in [Model runtime](MODEL_RUNTIME.md) |

Connection templates do not select a model, download weights, start a server,
check your account entitlement, or promise a price. Use a model available to
your account that supports the selected API's structured output mode.

## Save a local connection

With an existing local server running, choose its exact model ID:

```sh
rkc providers init --preset ollama --model YOUR_MODEL_ID --out provider.json
rkc providers doctor --file provider.json
rkc answer --dir ./atlas --provider-config provider.json --json "How does validation work?"
```

Use `--preset lm-studio` for its usual port, or `--preset openai-compatible
--endpoint http://127.0.0.1:8080/v1/chat/completions` for your own local URL. The
API must support the declared schema. An incompatible model fails visibly;
RKC does not silently switch to free-form prose. See the separately tested
[local endpoint contract](MODEL_ENDPOINTS.md).

## Save an API connection

Set the API credential in the environment of the terminal running RKC. The
profile stores only that variable's name. For OpenAI:

```sh
rkc providers init --preset openai --model YOUR_MODEL_ID --allow-remote --out provider.json
rkc providers doctor --file provider.json
rkc answer --dir ./atlas --provider-config provider.json --json "How does validation work?"
```

Use `--preset anthropic` with `ANTHROPIC_API_KEY`, or `--preset gemini` with
`GEMINI_API_KEY`. RKC uses the provider's native wire format and authentication
header. API accounts, schema support, quotas, and charges belong to that
provider. Creating a profile and running `doctor` do not send a request.

To use your own compatible gateway:

```sh
rkc providers init --preset openai-compatible --model YOUR_MODEL_ID \
  --endpoint https://YOUR_HOST/v1/chat/completions \
  --api-key-env RKC_API_KEY --allow-remote --out gateway.json
rkc providers doctor --file gateway.json
rkc answer --dir ./atlas --provider-config gateway.json --json "How does validation work?"
```

`--allow-remote` is deliberate consent to send the selected evidence to this
HTTPS endpoint. The saved `allow_remote` value carries that choice when the
profile is reused. A profile contains no credential value, but review its
endpoint before using a profile from someone else. Keep API keys out of shell
arguments, repository files, screenshots and copied commands. The workbench
offers copyable setup commands; authenticated API execution runs in your
terminal, whose environment holds the credential.

For one-off use, the same connection is available through answer flags:

```sh
rkc answer --dir ./atlas --provider openai --model-name YOUR_MODEL_ID \
  --allow-remote --context 4096 --max-output 768 --timeout 60s --json \
  "How does validation work?"
```

Native providers supply their documented default endpoint and environment name.
`--endpoint` and `--api-key-env` override those defaults. Compatible gateways
need an explicit endpoint. GGUF/process flags belong to `llama.cpp` and cannot
be mixed with API connections.

## Discover models without generating text

Model discovery is a separate, explicit metadata GET. It never sends repository
evidence or a generation prompt:

```sh
rkc providers models --preset openai --allow-remote --json
rkc providers models --file provider.json --json
```

The first form uses the preset's environment credential without choosing a
generation model. For local models use `--preset ollama` or `--preset lm-studio`.
For your own gateway, use the same endpoint and environment-name overrides as
`init`. RKC derives `/models` from the configured API prefix, reads one bounded
page and reports `more_available` if the server advertises further pages. A
listed ID is metadata, not proof of inference access, structured output support,
model quality, availability or price. Unsupported catalog endpoints fail
visibly; choose a model ID from your server or provider documentation instead.

## Use ChatGPT, Claude or Gemini sign-in

RKC shows the supported client steps and whether the executable is on PATH:

```sh
rkc providers login-guide --client codex
rkc providers login-guide --client claude
rkc providers login-guide --client gemini
```

- Codex: run `codex login`, follow its ChatGPT sign-in flow, then check
  `codex login status`. Codex owns account and plan access. See
  [official authentication guidance](https://learn.chatgpt.com/docs/auth).
- Claude Code: run `claude`, follow sign-in, then inspect `/status` in its
  session. Account credentials and API credentials can select different billing
  paths. See [Claude Code authentication](https://code.claude.com/docs/en/authentication).
- Gemini CLI: run `gemini` and select **Sign in with Google**. Google account
  type and organization policy determine access. See
  [Gemini CLI authentication](https://geminicli.com/docs/get-started/authentication/).

After signing in, export selected evidence:

```sh
rkc context --dir ./atlas --format markdown "How does validation work?"
```

Review the exported text, then share it with your client. Ask it to use the cited
sources and state missing information. For repeated work, configure the client's
[RKC MCP integration](MCP.md) so it can retrieve evidence directly. This route
uses the client's authentication and capabilities; RKC does not execute the
client, inspect its login state, or transform subscription tokens into API keys.

OpenAI separately documents a **Sign in with ChatGPT** preview for participating
applications and eligible plan usage. RKC has not implemented that OAuth/client
registration integration. Existing Codex sign-in plus context/MCP works through
Codex; the `openai` API provider uses API credentials. See the
[official preview boundaries](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations).

## Profile format and precedence

Profiles are standalone JSON, separate from the deterministic compiler config:

```json
{
  "schema_version": "rkc.provider.v1",
  "provider": "openai-compatible",
  "endpoint": "http://127.0.0.1:8080/v1/chat/completions",
  "model": "YOUR_MODEL_ID",
  "profile": "structured-claims",
  "allow_remote": false,
  "context_tokens": 4096,
  "max_output_tokens": 768,
  "timeout_seconds": 60
}
```

Allowed providers are `openai-compatible`, `openai`, `anthropic` and `gemini`.
`api_key_env` is optional for compatible endpoints and required for native APIs.
The native extractive profile is available only for compatible endpoints and
requires context 512/output at most 128. `neuroforge` is a connection preset for
that profile, not a new wire protocol.

`answer --provider-config` loads these defaults. Explicit answer flags override
them, including `--allow-remote=false` to revoke remote consent for this call.
Changing a Gemini model also requires its matching `--endpoint`, because that
API embeds the model ID in its URL. Unknown fields, duplicate keys, multiple
JSON values, malformed policies and profiles over 32 KiB are rejected. Context
must be 512–262144 tokens, output positive and no larger than context; profile
timeouts are whole seconds from 1–3600. These are client admission ceilings,
not measurements of server capacity.

`providers init --out` writes a private staging file, flushes and closes it, then
publishes its complete inode without overwriting an existing path. Concurrent
creators cannot clobber each other. Cancellation before publication leaves the
destination absent. Abrupt termination can leave a private `.tmp-…` staging
file; it cannot expose a partial destination profile. Filesystems without
hard-link publication fail explicitly. Unix directory entries are synced;
Windows uses the existing private-path ACL/identity contract and file flush,
without claiming Unix directory-entry power-loss guarantees.

## Transport, validation and audit boundaries

Remote connections require HTTPS and explicit consent. Local HTTP remains
restricted to IP-literal loopback addresses. URL credentials, queries,
fragments, encoded/relative paths, redirects and environment HTTP proxies are
rejected or disabled. TLS certificate validation remains enabled. API errors
report status and guidance without echoing response bodies or credential values.
Requests do not retry; the grounded answer service can make bounded repair
attempts under the answer deadline. Interrupts cancel metadata and generation
requests. Closing a provider cancels its active request and closes idle sockets.

Prompt text is bounded to 256 KiB and responses to 1 MiB; the native quotation
profile has its smaller documented limits. The model receives selected
canonical evidence and bounded normalized text, not permission to open the
repository, run commands or use tools. It cannot silently add canonical facts.

Native APIs use their documented JSON schema subsets. RKC translates `const`
to an enum and removes unsupported transport constraints, while its local
decoder still enforces the original supported-claim limits, categories,
certainty, unique citation IDs and text bounds. The exact transmitted schema
has its own digest. Truncated, refused, duplicate-key, multi-part/tool output
and incompatible envelopes fail visibly. Schema support is requested, not
independently attested.

The immutable logical model ID stays in `model_id`. An API-reported resolved
revision is retained separately as optional `reported_model_id`, including
each validation attempt; absent reported identities are not invented. Both
are provider metadata, not verified model-weight identity. Token telemetry is
also provider-reported: native cache input and Gemini thinking tokens count
conservatively against the configured limits. Remote CPU/RSS and model quality
are not measured or enforced by the client.

Every publishable claim still passes canonical evidence/ownership/citation
checks. Those checks do not prove that prose logically follows from its cited
source. Inspect the source and qualify your model for your actual task.

## Protocol references and verification

The wire formats were checked against official documentation on 2026-10-09:

- [OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create):
  bearer authentication, strict response schema and `max_completion_tokens`.
- [Claude structured outputs](https://platform.claude.com/docs/en/build-with-claude/structured-outputs):
  Messages `output_config.format` and supported schema constraints.
- [Gemini GenerateContent](https://ai.google.dev/api/generate-content):
  `contents`, `generationConfig.responseJsonSchema`, bounded non-streaming candidates.

The tests exercise fictional loopback HTTP/TLS servers, native request bodies,
credential-header isolation, logical/reported model provenance, response bounds,
refusals, cancellation, metadata-only discovery, no-clobber publication and CLI
profile precedence. No paid remote generation, subscription session or hosted
model quality evaluation is part of this implementation.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
