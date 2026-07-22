# Plugin SDK contract

Plugins extend RKC without receiving storage credentials or snapshot publication
authority. They consume a bounded request and return a versioned GraphPatch.

The public contracts are:

- [`pkg/pluginapi`](../pkg/pluginapi): host/worker request and response types;
- [`pkg/graphpatch`](../pkg/graphpatch): validated mutation contract;
- [`schemas/plugin-manifest.schema.json`](../schemas/plugin-manifest.schema.json):
  capability and identity manifest;
- [`schemas/graph-patch.schema.json`](../schemas/graph-patch.schema.json): portable
  patch schema;
- [`plugins/plugin.wit`](../plugins/plugin.wit): WASI component interface draft;
- [`plugins/plugins.lock.json`](../plugins/plugins.lock.json): reproducible plugin
  selection and artifact digests.

## Plugin classes

```text
acquirer
classifier
normalizer
syntax extractor
semantic extractor
framework pack
runtime observer
document renderer
search provider
model provider
exporter
policy provider
```

The first production registry should admit syntax, semantic, framework, and
export plugins. Acquisition, policy, and model plugins require additional
security review because they can affect source access or external egress.

## Manifest

A manifest declares identity, runtime, capabilities, input selection, outputs,
limits, determinism, and distribution metadata.

```json
{
  "schema_version": "1.0",
  "plugin": {
    "id": "org.example.rkc.python",
    "name": "Example Python analyzer",
    "version": "1.2.0",
    "api_version": "1.0",
    "license": "Apache-2.0"
  },
  "runtime": {
    "kind": "wasm-wasi",
    "entrypoint": "python-analyzer.wasm",
    "protocol": "component-rkc-extractor-v1",
    "sha256": "..."
  },
  "capabilities": {
    "filesystem_read": ["repository:materialized-read-only"],
    "filesystem_write": ["temporary:plugin-private"],
    "environment": [],
    "network": [],
    "process_spawn": [],
    "clock": false,
    "random": false
  },
  "inputs": {
    "languages": ["python"],
    "globs": ["**/*.py", "**/*.pyi"],
    "capabilities": ["extract"]
  },
  "outputs": {
    "node_kinds": ["module", "class", "function", "method"],
    "edge_kinds": ["declares", "imports", "calls"],
    "evidence_kinds": ["declared", "syntax_inferred"]
  },
  "limits": {
    "timeout_seconds": 60,
    "memory_mib": 256,
    "max_output_bytes": 67108864,
    "max_parallelism": 1
  },
  "determinism": {
    "level": "toolchain-dependent",
    "cache_inputs": [
      "artifact.sha256",
      "plugin.sha256",
      "toolchain.digest",
      "schema.version"
    ]
  }
}
```

A plugin may request a capability. The host policy decides whether it receives
it. A declaration is not a permission slip written by the applicant.

## Request

A host request contains:

- protocol and schema versions;
- immutable snapshot ID;
- opaque repository-root token or preopened directory;
- bounded artifact references;
- content digests;
- selected configuration;
- cancellation/deadline metadata;
- granted capabilities;
- toolchain descriptor;
- maximum output budget.

Workers should prefer artifact IDs and host-provided streams over arbitrary
filesystem path access.

## GraphPatch

The current Go contract is:

```go
type Patch struct {
    ProtocolVersion string
    SchemaVersion   string
    SnapshotID      string
    Producer        Producer
    GeneratedAt     time.Time
    InputDigest     string
    Fragment        rkcmodel.Fragment
    Metadata        map[string]string
}
```

A fragment can contain artifacts, nodes, edges, evidence, diagnostics,
conflicts, documents, claims, and execution paths. The host validates limits,
vocabulary, IDs, evidence, ownership, endpoint resolution, and canonical graph
invariants before applying it.

Plugins do not send SQL, migrations, database handles, or arbitrary host method
names.

## Evidence requirements

A semantic node or edge should have evidence containing:

- source artifact and range when available;
- method, such as AST declaration, compiler reference, manifest key, route
  registration, runtime trace, or documentation assertion;
- evidence class;
- confidence where meaningful;
- analyzer ID/version;
- input digest;
- context such as build tags, compile flags, target framework, or runtime profile.

Unsupported constructs should produce diagnostics, not fabricated success.

## Runtime classes

### WASI component

Use for pure parsers, validators, and deterministic framework packs.

Production host requirements:

- no ambient filesystem;
- preopened read-only repository or host streams;
- plugin-private temporary directory only when granted;
- no network unless allowlisted;
- no inherited environment;
- bounded memory, fuel/CPU, output, and wall clock;
- deterministic clock/random policy;
- host-controlled cancellation;
- signed module and digest verification.

### Native worker

Use when an existing compiler, language server, or platform tool cannot
reasonably run as WASI.

Production containment requirements:

- read-only materialized input;
- ephemeral work directory;
- no network by default;
- sanitized environment;
- process, CPU, memory, file, and output limits;
- Linux namespace/seccomp/cgroup profile or platform-equivalent isolation;
- no project hooks or package-manager lifecycle scripts;
- bounded protocol decoder;
- complete audit log and artifact digest.

The current Python worker has timeout and output bounds but not full OS
containment. It is a trusted reference worker, not a third-party execution
service.

## Ownership and replacement

Every plugin output is associated with:

```text
plugin ID
plugin version
plugin artifact digest
run ID
input digest
configuration digest
toolchain digest
schema version
```

Incremental invalidation removes stale output owned by a prior run before
applying replacement output. Plugins may not overwrite records owned by another
plugin merely because they selected the same convenient ID.

## Streaming

Large semantic indexers require a streaming protocol. The production host should
support:

```text
begin patch
  -> batches of evidence
  -> batches of nodes
  -> batches of edges
  -> diagnostics and statistics
commit patch
```

The host stages batches transactionally, enforces cumulative limits, and aborts
the entire patch on validation failure. Partial plugin output never becomes a
published snapshot.

## Conformance suite

An official plugin must pass:

1. manifest and lock validation;
2. deterministic replay at its declared level;
3. fixture golden tests;
4. malformed and truncated input tests;
5. cancellation and deadline tests;
6. memory, process, open-file, and output limits;
7. path-containment and symlink-escape tests;
8. undeclared network/environment/process capability tests;
9. schema and vocabulary compatibility;
10. graph invariant validation;
11. benchmark precision/recall publication;
12. known-limitations and unsupported-construct report;
13. license and supply-chain review;
14. signed artifact verification.

## Minimal native worker pseudocode

```python
request = decode_one_bounded_json(stdin, max_bytes=REQUEST_LIMIT)
assert request["schema_version"] == "0.2.0"

fragment = analyze_only_host_granted_artifacts(request)
validate_local_ids(fragment)

response = {
    "schema_version": "0.2.0",
    "snapshot_id": request["snapshot_id"],
    "artifacts": fragment.artifacts,
    "nodes": fragment.nodes,
    "edges": fragment.edges,
    "evidence": fragment.evidence,
    "diagnostics": fragment.diagnostics,
}
encode_one_bounded_json(stdout, response)
```

The worker must never interpret repository comments as protocol instructions.
