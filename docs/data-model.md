# Repository Knowledge Representation

The Repository Knowledge Representation, or RKR, is the canonical language-
neutral model emitted by RKC. The current Go definitions are in
[`pkg/rkcmodel`](../pkg/rkcmodel), the portable JSON contract is
[`schemas/rkc-bundle.schema.json`](../schemas/rkc-bundle.schema.json), and the
planned transactional projection is [`storage/sqlite/schema.sql`](../storage/sqlite/schema.sql).

## Design rules

1. Physical bytes and logical entities are separate.
2. Every fact has provenance or an explicit diagnostic explaining its absence.
3. Unresolved relationships remain records; they are not discarded.
4. Contradictory evidence becomes a conflict rather than a silent overwrite.
5. Canonical truth is deterministic and model-independent.
6. Presentation records and model prose are derived and replaceable.
7. Published snapshots are immutable.

## Bundle

```go
type Bundle struct {
    Snapshot    Snapshot        `json:"snapshot"`
    Artifacts   []Artifact      `json:"artifacts"`
    Nodes       []Node          `json:"nodes"`
    Edges       []Edge          `json:"edges"`
    Evidence    []Evidence      `json:"evidence"`
    Diagnostics []Diagnostic    `json:"diagnostics"`
    Conflicts   []Conflict      `json:"conflicts,omitempty"`
    Documents   []Document      `json:"documents,omitempty"`
    Claims      []Claim         `json:"claims,omitempty"`
    Paths       []ExecutionPath `json:"execution_paths,omitempty"`
}
```

Record arrays are canonically sorted before hashing or export. Map keys are
serialized deterministically by the Go JSON encoder. Operational fields such as
wall-clock creation time and local absolute root paths are excluded from the
canonical content digest.

## Snapshot

A snapshot identifies one immutable analysis of one repository state.

```go
type Snapshot struct {
    SchemaVersion    string
    ID               string
    RepositoryID     string
    ParentSnapshotID string
    CreatedAt        time.Time
    Status           string
    RootName         string
    RootPath         string
    ContentDigest    string
    ConfigDigest     string
    PolicyDigest     string
    PluginLockDigest string
    ToolchainDigest  string
    Git              GitInfo
    Tool             ToolInfo
}
```

Source-truth identity incorporates repository content, Git state,
analysis-affecting configuration, policy, plugin lock, toolchain, and schema
version. It does not incorporate browser colors, output directories, server
ports, or derived model prose, despite those items’ obvious importance to
committee meetings.

Valid lifecycle states are:

```text
building -> validating -> committed
                   \-> failed
committed -> superseded
```

## Artifact

An artifact is a physical repository object in one snapshot. Examples include a
source file, document, binary, symlink, archive, notebook, manifest, or generated
file.

Important fields:

- `id`: occurrence identity in the snapshot;
- `logical_id`: identity intended to survive path movement when established;
- `content_id`: content-addressed identity for equal bytes;
- `path`: repository-relative path;
- `sha256`: exact source-byte digest;
- `text`: whether text analysis is eligible;
- `status`: `text`, `syntax_parsed`, `binary`, `excluded`, `oversized`, etc.;
- `disposition_reason`: policy reason for a non-default state;
- `generated` and `vendored`: classification, not an instruction to forget it.

A repository completeness claim is based on every encountered path receiving an
artifact or diagnostic disposition.

## Node

A node represents a logical repository entity or an explicit placeholder.

```go
type Node struct {
    ID            string
    LogicalID     string
    Kind          string
    Name          string
    QualifiedName string
    Signature     string
    Language      string
    Visibility    string
    Stability     string
    PublicSurface bool
    ArtifactID    string
    Source        *SourceRange
    SemanticHash  string
    EvidenceIDs   []string
    Attributes    map[string]any
}
```

Core node kinds include repositories, projects, packages, files, modules,
classes, interfaces, functions, methods, fields, parameters, tests, API
operations, CLI commands, configuration keys, environment variables, database
objects, schemas, events, build targets, deployments, documents, dependencies,
and `unresolved_symbol`.

Extension kinds must be namespaced until accepted into the core vocabulary.

## Edge

An edge is a typed relationship between two existing nodes.

```go
type Edge struct {
    ID          string
    Kind        string
    From        string
    To          string
    Resolution  string
    Confidence  float64
    Producer    string
    Lifecycle   string
    EvidenceIDs []string
    Attributes  map[string]any
}
```

Typical edge kinds are `contains`, `declares`, `imports`, `references`, `calls`,
`inherits`, `implements`, `routes_to`, `tests`, `documents`, `configures`,
`reads`, `writes`, `depends_on`, `builds`, `emits`, and `subscribes`.

### Resolution classes

```text
declared
compiler_resolved
syntax_inferred
runtime_observed
documentation_asserted
model_inferred
unresolved
```

These classes never collapse into a single “confidence” value. A runtime
observation proves that a path occurred in one authorized execution. It does not
prove that no other path exists. A syntax inference remains an inference even
when a graph renderer would prefer a cleaner label.

An unresolved target is represented by a real `unresolved_symbol` node so every
edge endpoint exists and the unresolved denominator remains measurable.

## Evidence

Evidence answers: “Why does this record exist?”

```go
type Evidence struct {
    ID          string
    Kind        string
    Method      string
    Confidence  float64
    Source      *SourceRange
    Tool        string
    ToolVersion string
    InputDigest string
    ObservedAt  *time.Time
    Detail      string
    Attributes  map[string]any
}
```

A confidence number without method, producer, input digest, and source context
is not provenance. It is punctuation with career aspirations.

Evidence should include byte ranges when exact slicing matters and line/column
coordinates for human and editor navigation. Columns are zero-based; lines are
one-based.

## Diagnostics

Diagnostics make analysis limitations queryable.

```go
type Diagnostic struct {
    ID       string
    Severity string // note, warning, error, fatal
    Code     string
    Message  string
    Source   *SourceRange
    Stage    string
    Plugin   string
}
```

Codes are stable. A quality gate operates on codes and severity, not brittle
message strings.

## Conflicts

A conflict records contradictory candidate facts and the selected preference.
Examples include signature disagreement, multiple definition candidates,
documentation contradiction, runtime/static disagreement, duplicate identity,
route collision, and configuration-default disagreement.

The preferred candidate may be selected by evidence strength, but all candidate
IDs and supporting evidence remain retained.

## Documents and claims

Documents are derived records. A document has subjects, generator identity,
status, content digest, and sections. Sections may cite claims and evidence.

A model-generated statement is a `Claim`:

```go
type Claim struct {
    ID               string
    SubjectID        string
    Text             string
    Category         string
    Certainty        string
    Generator        string
    GeneratorVersion string
    EvidenceIDs      []string
    Validation       string
}
```

Valid certainty states are `supported`, `inferred`, `uncertain`, and
`contradicted`. Valid validation states are `pending`, `accepted`, `rejected`,
`inference`, and `stale`.

Rejected claims remain available for audit but are not published as accepted
documentation.

## Execution paths

An execution path is a named, evidence-backed ordered path through the graph.
It records entry node, optional exit node, node IDs, edge IDs, and evidence IDs.
It may come from static traversal, an authorized runtime trace, or a combined
analysis, with the source stated explicitly.

## Identity construction

The public helpers construct domain-separated stable IDs:

```text
artifact:<digest>
node:<digest>
edge:<digest>
evidence:<digest>
document:<digest>
claim:<digest>
path:<digest>
```

Logical identity uses canonical attributes such as language, project, package,
qualified name, entity kind, and normalized signature discriminator. Occurrence
identity additionally includes snapshot and source location.

Logical identity is conservative. When a rename cannot be established safely,
RKC reports deletion plus addition rather than manufacturing continuity.

## Canonical invariants

A valid committed bundle must satisfy:

- snapshot schema version is supported;
- IDs are non-empty and unique within record families;
- every resolved edge endpoint exists;
- every evidence reference exists;
- every source range references an existing artifact where an artifact ID is
  supplied;
- every document subject and accepted claim subject exists;
- node, edge, evidence, status, severity, certainty, and validation vocabularies
  are registered;
- evidence confidence is within `[0,1]`;
- source ranges are ordered and non-negative;
- canonical sorting and digesting reproduce exactly;
- unresolved relationships use explicit unresolved targets;
- generated claims cannot become canonical parser facts.

## Coverage

Coverage is derived, not manually reported.

```text
inventory_accounting_ratio
  = artifacts_inventoried / artifacts_encountered

syntactic_parse_ratio
  = syntactically_parsed_eligible_artifacts / eligible_text_artifacts

semantic_parse_ratio
  = semantically_parsed_eligible_artifacts / eligible_text_artifacts

symbol_evidence_ratio
  = symbols_with_evidence / symbols_total

public_documentation_ratio
  = public_symbols_documented / public_symbols

edge_resolution_ratio
  = resolved_edges / edges_total

claim_citation_ratio
  = claims_with_evidence / claims_total
```

Every report retains numerators, denominators, classifications, and diagnostic
counts. No anonymous “93% documented” badge is permitted to roam unsupervised.

## SQLite projection

The validated DDL includes repositories, snapshots, artifacts, logical entities,
nodes, evidence, node/edge evidence, edges, documents, sections, chunks, FTS5,
embeddings, diagnostics, tool runs, jobs, cache entries, conflicts, claims,
execution paths, coverage records, and audit events.

The current release validates this schema but does not yet use it as the
canonical runtime writer. That migration is Workstream 1 of the remainder plan.
