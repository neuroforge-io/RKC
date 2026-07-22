# Local model runtime

The model subsystem is optional and derived. It improves readability; it does not
determine inventory completeness, symbol existence, signatures, APIs, call
resolution, or quality denominators.

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
    Generate(ctx context.Context, request Request) (Response, error)
    Describe(ctx context.Context) (Descriptor, error)
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
- sanitizes the process environment;
- bounds stdout and stderr;
- monitors Linux process RSS and terminates the process on limit violation;
- extracts one JSON object from noisy CLI output;
- returns model/output digests for audit.

The provider tests use a fake executable. No real model weights are bundled.

## Strict local profile

```yaml
provider: llama.cpp
model_class: coder
parameter_class: 1.5b
quantization: q4
context_tokens: 4096
max_output_tokens: 768
parallel_requests: 1
max_rss_mib: 3584
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
  "summary": "...",
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

- has no evidence citation;
- cites an ID not present in the packet;
- names a code identifier absent from the subject/related records;
- claims unsupported inference when inference is disabled;
- omits certainty;
- exceeds claim or output limits;
- contains malformed structured output;
- conflicts with deterministic signature/type facts without marking the
  contradiction.

A summary is rejected when it exceeds its limit, names unknown code identifiers,
or lacks accepted supporting claims.

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

Before claiming the under-4-GiB profile, publish:

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

Until that gate is met, 3.5 GiB is a configured engineering ceiling, not a
marketing measurement.
