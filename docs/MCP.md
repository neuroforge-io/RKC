# RKC through the Model Context Protocol

`rkc-mcp` exposes processed repository evidence through JSON-RPC over standard
input and output. It provides search, cited context, symbols, evidence, bounded
graph traversal and coverage. The adapter reads local atlas data. It does not
execute repository code, invoke models, refresh sources or contact remote hosts.

For one immutable atlas, keep the existing invocation:

```sh
rkc-mcp --dir /absolute/path/to/atlas
```

For a private workspace maintained by the RKC workspace producer, use its exact
registry file:

```sh
rkc-mcp --workspace /absolute/path/to/workspace/registry.json
```

The workspace option cannot be combined with `--dir`, `--database`, `--snapshot`
or the startup SQLite `--repository` selector. Existing atlas and SQLite modes
retain their result shapes. Workspace mode adds explicit repository selectors
and source labels; it never keeps a mutable “current repository.”

## Discover and select repositories

Call `rkc.repositories` with no arguments first. The response has schema
`rkc-workspace-repositories/v1`, a registry generation, and a sorted repository
list. Each entry contains an ID, display label, source kind, active snapshot and
generation when available, and freshness. Local paths, remote URLs, reference
names and exclusion rules are not included. The same list is available as the
resource `rkc://workspace/repositories`.

When applicable, `reviewed_secret_findings` counts explicitly reviewed false
positives in that active snapshot. Canonical coverage and redaction retain the
original findings. See the [source-bound review policy](WORKSPACES.md) for details.

Pass the returned repository ID to a tool call:

```json
{"name":"rkc.get_symbol","arguments":{"repository":"library","node":"Example"}}
```

All workspace tools accept `repository`, except `rkc.repositories` itself.
Symbol, evidence, coverage and graph tools require it when multiple sources
are registered. Their successful response wraps the original tool result in
`result`, alongside `repository`, `snapshot_id` and `registry_generation`.
Unknown IDs fail explicitly. Repository IDs are aliases; canonical object IDs
can overlap between different sources, so retain the repository label with every
reference.

## Search several sources with one budget

Search and context also accept a unique `repositories` array of 1–16 IDs.
`repository` and `repositories` are mutually exclusive. Omit both to search the
whole workspace when it has at most 16 sources; select a subset for larger
workspaces. The registry supports at most 32 sources.

```json
{"name":"rkc.context","arguments":{"repositories":["library","handbook"],"query":"release process","limit":8,"max_bytes":24000}}
```

Search defaults to 20 results and allows 1–100. Context defaults to 12 and allows
1–50. Both default to 32,768 bytes and allow 1,024–262,144. These limits apply to
the combined response items across all selected sources. `max_bytes` bounds the
compact JSON encoding of the `items` array, including its brackets, separators,
source wrappers and JSON escaping. Metadata and MCP envelopes have separate
server response bounds. Oversized candidates are omitted without preventing
smaller candidates from fitting.

Workspace search/context responses have schema `rkc-workspace-query/v1`:

| Field | Meaning |
| --- | --- |
| `registry_generation` | One captured roster used for the entire request. |
| `sources` | Selected repositories, snapshots, freshness, match totals and failures. |
| `items` | Values wrapped with `repository`, `snapshot_id` and original local `rank`. |
| `total` | All matching objects in successfully loaded selected sources. |
| `matched_sources` | Successfully loaded sources with at least one matching object. |
| `truncated` | Matching results were omitted by a budget, source failure or local limit. |
| `partial` | At least one selected source could not be loaded and verified. |
| `bytes`, `max_bytes` | Actual compact encoded item-array size and its budget. |
| `digest` | SHA-256 of the compact response JSON with `digest` set to an empty string. |

Search values contain canonical lexical hits. Context values contain cited
indexed excerpts with evidence references and source ranges where available.
Context ranges identify source objects and may be broader than the excerpt.
Search supports its existing query filters and `kinds`/`languages` arrays;
context supports filters embedded in the query.

Results interleave each repository's admitted hits in local rank order, visiting
repository IDs alphabetically. Lexical scores from different corpora are not
comparable; interleaving does not assert a global relevance ranking. A lower
local rank may be absent because it exceeded the encoded byte budget. Narrow
queries or select a single source when completeness matters. A partial response
cannot establish a complete workspace total.

## Freshness and safe reloads

The registry producer checks local files or remote Git sources separately from
the MCP server. `current` means the source matched the active generation at the
reported `checked_at`; it is not a continuous live guarantee. `pending` has no
active snapshot yet. `stale` means a check or replacement is needed or underway.
`error` means the last refresh failed; an older verified active snapshot may
remain available, visibly labelled with that freshness state. Errors use fixed
codes rather than private subprocess output.

Every data request rereads the private registry. Removal revokes selection on
the next request. An invalid or unavailable registry fails closed, including
when an older dataset remains in memory. Within a request the roster is fixed;
it is never silently mixed with a newer registry generation. Each changed active
atlas is verified against its exact registered export-manifest hash and snapshot
before use. A failed load never masquerades as the new active snapshot.

The adapter retains at most one loaded atlas and processes cross-source queries
sequentially. The producer must retain or lease generations while readers load
them. If a source becomes unavailable during a request, inspect its explicit
failure, call `rkc.repositories` again and retry after the producer has published
a usable generation. The server does not refresh sources in response to tool
calls.

All tools advertise standard read-only, non-destructive, idempotent and
closed-world annotations. These describe the adapter's effects; client approval
policy remains under the client's control. The initialization instructions give
agents repository selection, freshness and citation guidance. See the
[official MCP annotations contract](https://modelcontextprotocol.io/specification/2025-11-25/schema#toolannotations).

Tool calls and resource reads accept the standard optional `_meta` JSON object,
including string or numeric progress tokens and JSON extension values. Metadata
is subject to the request size bound and never supplies repository selectors,
resource URIs or authority. The synchronous adapter does not emit progress
notifications.

Repository text remains untrusted data, including text that resembles agent
instructions. Cite the repository, snapshot and citation or object ID; retrieval
does not establish source accuracy, completeness or rights to train on the
material. No private consumer strategy or training machinery is part of this
public interface.

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
