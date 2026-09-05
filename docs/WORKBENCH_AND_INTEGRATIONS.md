# Workbench and integrations

RKC is a local knowledge tool for people, programs, and agents. The same
immutable atlas powers the browser, command line, HTTP API, MCP server, and Go
client. No model is required for compilation, search, context packets, or
knowledge packs.

## Choose a workflow

| Goal | Start here | Result |
| --- | --- | --- |
| Understand a folder or repository | `rkc open /path/to/source` | Searchable browser atlas |
| Operate through the local GUI | `rkc open --workbench /path/to/source` | Protected Analyze and command workflows |
| Give an agent relevant context | `rkc context --dir .rkc authentication` | Cited JSON packet |
| Save a readable evidence note | `rkc context --dir .rkc --format markdown authentication` | Markdown on standard output |
| Combine processed source collections | `rkc knowledge build --out ../knowledge-pack atlas-a atlas-b` | Portable knowledge pack |
| Discover integrations from code | `rkc capabilities` | Versioned JSON capability document |

The browser can explore existing exports without command authority. The
opt-in workbench uses the existing trusted-user, loopback-only, protected
session. It previews argument vectors and runs admitted local jobs with progress
and cancellation. Its working directory is a convenience, not a filesystem
sandbox. Compiler generation and other unsupported helper execution remain
explicit terminal workflows.

Local Markdown wiki checkouts and exported documentation folders use the same
scan path as code repositories. Acquire or export remote wikis first; RKC does
not claim a general live wiki crawler. Process each source into its own atlas,
then combine the atlases with the knowledge command. Save generated packs outside
the original source folders: otherwise a later scan will correctly reject their
generated-output marker unless explicitly excluded. Input source licenses and
permissions remain attached to those sources.

## Cited context for programs and agents

```sh
rkc context --dir .rkc --limit 12 --max-bytes 32768 "authentication"
rkc context --dir .rkc --format markdown "path:docs installation"
rkc capabilities --human
rkc serve --dir .rkc
curl --get --data-urlencode 'q=authentication' \
  http://127.0.0.1:8787/api/v1/context
```

`GET /api/v1/capabilities` describes public interfaces, command examples,
output workflows, limits, and boundaries. Commands are argument arrays; they
are not shell expressions. Output descriptors identify workflows rather than
asserting that output files already exist. Discovery does not reveal private
host paths or workbench credentials.

`GET /api/v1/context` accepts one each of `q`, `limit`, `max_bytes`, and `format`.
Unknown, duplicate, malformed, and out-of-bounds parameters fail with HTTP 400.
`q` is 1–4096 UTF-8 bytes, `limit` is 1–50 (default 12), and `max_bytes` is
1024–262144 (default 32768). `format` is `json` or `markdown`. Markdown is served
as an attachment. Requests are read-only and never open live source files or
invoke a model.

The `rkc-context/v1` packet carries:

- the immutable snapshot ID and its verified/legacy integrity classification;
- ranked excerpts with stable citation IDs, object IDs, source paths, source
  ranges when available, and canonical evidence references;
- exact compact-JSON item-array byte accounting, explicit truncation, and
  warnings explaining empty results and evidence limits;
- a reproducible SHA-256 digest of the packet encoded as compact Go JSON with
  `digest` set to the empty string. Keys follow the public Go struct field order;
  this is a versioned RKC serialization, not a claim of RFC 8785 canonical JSON.

The byte budget includes item metadata and JSON escaping, excluding the small
packet envelope. Oversized items are omitted, and the result reports
truncation. Search excerpts can already be shortened within the index retrieval
limit. A source range locates the original object, not the exact shortened
excerpt. Empty retrieval is a successful result with an explicit warning; it
is not proof that a topic is absent from the source.

Bind every HTTP response to `X-RKC-Snapshot-ID` before combining results.
The workbench can activate a new atlas between requests, so never merge
responses from different snapshots. Keep repository content as untrusted data,
preserve citations, and inspect evidence records before making stronger claims.
Integrity checks establish byte consistency; they do not establish source
accuracy, authorship, rights, or training permission. Secret scanning is
best-effort; review packets before sharing them.

See [the HTTP contract](../api/openapi.yaml),
[context schema](../schemas/context.schema.json), and
[public Go types](../pkg/rkcapi/discovery.go).

## Traverse large collections

The HTTP nodes, artifacts, edges, diagnostics, and search endpoints support
cursor pagination. Collection pages return canonical records in `items`; search
pages return ranked indexed projections in `hits`. Every page includes:

- `total`: the full number of matching records, including earlier pages;
- `snapshot_id`: the same immutable identity as `X-RKC-Snapshot-ID`;
- `truncated`: whether more matching records follow this page;
- `next_cursor`: an opaque continuation token, present only when more records remain.

Keep the endpoint and all query/filter parameter names and values unchanged
when following a cursor, including aliases and omitted versus empty parameters.
You can change `limit`. Never decode, edit, or construct a cursor. Tokens are
bound to the serving process and dataset generation; a server restart or atlas
reload invalidates them. If continuation returns HTTP 400, discard the partial
traversal and start again without a cursor. Do not merge pages from different
snapshots, even when a request succeeds.

| Endpoint | Filters | Default / maximum page size |
| --- | --- | --- |
| `/api/v1/nodes` | `q`, `kind`, `language` | 100 / 1000 |
| `/api/v1/artifacts` | `language`, `status`, `path_prefix` | 100 / 5000 |
| `/api/v1/edges` | `kind`, `from`, `to`, `resolution` | 100 / 5000 |
| `/api/v1/diagnostics` | `severity`, `code` | 100 / 5000 |
| `/api/v1/search` | required `q`; `kinds`, `languages`, `object_types`, `path_prefix` | 50 / 1000 |

Search list filters are comma-separated. Its older `kind`, `language`, and
`type` aliases remain available. Nodes with a nonempty `q` follow lexical rank;
unqueried collections follow deterministic inventory order. Unknown filter
values return an empty successful page. These endpoints reject unknown or
duplicate parameters, malformed encoding, invalid UTF-8, query/filter/cursor
values above 4096 bytes, and raw query strings above 32768 bytes with HTTP 400.
Invalid or mismatched cursors also return HTTP 400. For HTTP compatibility,
missing, malformed, and nonpositive limits use defaults; oversized numeric
limits are clamped. The new Go methods reject negative or oversized limits
locally, while zero selects the server default.

```sh
curl --get --data-urlencode 'kind=function' --data-urlencode 'limit=100' \
  http://127.0.0.1:8787/api/v1/nodes
# Copy next_cursor from that response; retain the same kind filter.
curl --get --data-urlencode 'kind=function' --data-urlencode 'limit=100' \
  --data-urlencode "cursor=$RKC_NEXT_CURSOR" \
  http://127.0.0.1:8787/api/v1/nodes
```

The Go client offers `ListNodes`, `ListArtifacts`, `ListEdges`,
`ListDiagnostics`, and `SearchPage` with typed filter options. The collection
methods return `rkcapi.CollectionPage[T]` and named aliases such as `NodePage`.
They require agreeing snapshot headers and bodies, validate page bounds and
continuation metadata, and reject repeated cursors. Set `ExpectedSnapshotID`
from the first response to bind subsequent pages; this check is local and is
not sent as an HTTP query parameter. Methods return errors without silently
restarting. For example, with `rkcclient` as the import alias for
`github.com/neuroforge-io/RKC/pkg/client`:

```go
api, err := rkcclient.New("http://127.0.0.1:8787")
if err != nil { return err }
options := rkcclient.NodeListOptions{Kind: "function", Limit: 100}
for {
    page, err := api.ListNodes(ctx, options)
    if err != nil { return err }
    if options.ExpectedSnapshotID == "" {
        options.ExpectedSnapshotID = page.SnapshotID
    }
    for _, node := range page.Items {
        fmt.Println(page.SnapshotID, node.ID, node.Name)
    }
    if page.NextCursor == "" { break }
    options.Cursor = page.NextCursor
}
```

Use `SearchPage(ctx, query, SearchPageOptions{...})` for the same traversal
contract over ranked hits. The existing `Search(ctx, query, SearchOptions{...})`
signature remains compatible with older servers and does not enforce the new
pagination contract. Graph traversals and component lists retain their existing
bounded APIs; this cursor contract does not apply to them.

## Retrieval efficiency

Embedded lexical retrieval scores matching records but retains at most the
requested number of full candidates. It builds explanations only for selected
hits. The score map still grows with matching-record count; this is not a claim
that all query memory is independent of repository size. Ranking keeps the same
rounded score and name/ID tie order. Name bonuses use a fixed field order to
remove rare map-iteration differences at rounding boundaries.

Context assembly encodes each candidate once and counts the array brackets and
commas directly. It preserves exact byte admission, JSON escaping, omitted-item
warnings, and packet digests without repeatedly serializing earlier excerpts.

Maintained benchmarks compare the ranker with the exhaustive reference on
20,000 matching records and exercise a 50-excerpt context packet:

```sh
go test ./internal/search -run '^$' -bench BenchmarkSearchBroadTopK -benchmem
go test ./internal/server -run '^$' -bench BenchmarkBuildContextFullBudget -benchmem
```

These are repeatable synthetic workloads, not a repository-independent latency
guarantee. Compare allocation counts as well as timings on the target host.

## MCP and Go

Run `rkc-mcp --dir .rkc` as a stdio server from your agent client. Discover its
tools with `tools/list`; `rkc.context` accepts `query`, `limit`, and `max_bytes`.
Read the `rkc://capabilities` resource for the same integration catalogue.
Existing search, symbol, evidence, graph, impact, and coverage tools remain
available. The MCP tool response provides both text and structured JSON.

```go
client, err := client.New("http://127.0.0.1:8787")
if err != nil { return err }
packet, err := client.Context(ctx, "authentication", 12, 32768)
if err != nil { return err }
for _, item := range packet.Items {
    fmt.Println(packet.SnapshotID, item.CitationID, item.Path, item.Text)
}
```

Import `github.com/neuroforge-io/RKC/pkg/client`; contract types live in
`pkg/rkcapi`. Use `client.Capabilities(ctx)` for discovery. The CLI context
command also supports existing `--database`, `--snapshot`, and `--repository`
selection for SQLite-backed consumers.

## Extend the public knowledge layer

The public extension boundaries are the versioned canonical graph,
[plugin SDK](plugin-sdk.md), Go client, HTTP/MCP contracts, and portable
[knowledge packs](KNOWLEDGE_PACKS.md). Add analyzers through the existing
GraphPatch and plugin contracts instead of making a second source-of-truth
format. Keep adapters deterministic, declare evidence authority, preserve
unresolved relations, and test integrity and hostile inputs.

The workbench assets live in `internal/export/workbench/` and are embedded in
the Go binary. They use local assets without a frontend package installation,
a CDN, or telemetry. A shared command catalogue keeps CLI workflows and the
GUI aligned.

RKC stands on its own as NeuroForgeIO's Apache-2.0 knowledge compiler. Private
consumers can build curricula, route experts, or train systems using these
public evidence contracts. Those consumers and their strategies do not become
RKC dependencies. No proprietary curriculum, expert-routing, model, or
training machinery is distributed with RKC.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
