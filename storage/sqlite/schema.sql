PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = MEMORY;
PRAGMA trusted_schema = OFF;

BEGIN IMMEDIATE;

CREATE TABLE IF NOT EXISTS schema_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

INSERT INTO schema_meta(key, value)
VALUES ('schema_version', '0.2.0')
ON CONFLICT(key) DO UPDATE SET value = excluded.value;

CREATE TABLE IF NOT EXISTS repositories (
    repository_id TEXT PRIMARY KEY,
    canonical_origin TEXT,
    display_name TEXT NOT NULL,
    created_at TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE TABLE IF NOT EXISTS snapshots (
    snapshot_id TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories(repository_id) ON DELETE CASCADE,
    parent_snapshot_id TEXT REFERENCES snapshots(snapshot_id) ON DELETE SET NULL,
    schema_version TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    commit_sha TEXT,
    ref_name TEXT,
    dirty INTEGER NOT NULL DEFAULT 0 CHECK (dirty IN (0, 1)),
    created_at TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    tool_version TEXT NOT NULL,
    config_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('building', 'complete', 'failed', 'superseded')),
    metadata_json TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX IF NOT EXISTS idx_snapshots_repository_created
ON snapshots(repository_id, created_at DESC);

CREATE TABLE IF NOT EXISTS artifacts (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    artifact_id TEXT NOT NULL,
    logical_artifact_id TEXT,
    path TEXT NOT NULL,
    kind TEXT NOT NULL,
    language TEXT,
    media_type TEXT,
    size_bytes INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    content_sha256 TEXT,
    line_count INTEGER CHECK (line_count IS NULL OR line_count >= 0),
    is_text INTEGER NOT NULL CHECK (is_text IN (0, 1)),
    status TEXT NOT NULL,
    exclusion_reason TEXT,
    generated_classification TEXT,
    vendor_classification TEXT,
    license_expression TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, artifact_id),
    UNIQUE(snapshot_id, path)
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_artifacts_snapshot_language
ON artifacts(snapshot_id, language);
CREATE INDEX IF NOT EXISTS idx_artifacts_snapshot_status
ON artifacts(snapshot_id, status);
CREATE INDEX IF NOT EXISTS idx_artifacts_content_hash
ON artifacts(content_sha256);

CREATE TABLE IF NOT EXISTS logical_entities (
    repository_id TEXT NOT NULL REFERENCES repositories(repository_id) ON DELETE CASCADE,
    logical_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    canonical_name TEXT NOT NULL,
    first_snapshot_id TEXT REFERENCES snapshots(snapshot_id) ON DELETE SET NULL,
    last_snapshot_id TEXT REFERENCES snapshots(snapshot_id) ON DELETE SET NULL,
    identity_basis TEXT NOT NULL,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(repository_id, logical_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS nodes (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    node_id TEXT NOT NULL,
    logical_id TEXT,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT,
    signature TEXT,
    language TEXT,
    visibility TEXT,
    artifact_id TEXT,
    start_byte INTEGER,
    end_byte INTEGER,
    start_line INTEGER,
    start_column INTEGER,
    end_line INTEGER,
    end_column INTEGER,
    semantic_hash TEXT,
    public_surface INTEGER NOT NULL DEFAULT 0 CHECK (public_surface IN (0, 1)),
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, node_id),
    FOREIGN KEY(snapshot_id, artifact_id)
      REFERENCES artifacts(snapshot_id, artifact_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_nodes_snapshot_kind
ON nodes(snapshot_id, kind);
CREATE INDEX IF NOT EXISTS idx_nodes_snapshot_qname
ON nodes(snapshot_id, qualified_name);
CREATE INDEX IF NOT EXISTS idx_nodes_snapshot_artifact
ON nodes(snapshot_id, artifact_id);
CREATE INDEX IF NOT EXISTS idx_nodes_logical
ON nodes(logical_id);

CREATE TABLE IF NOT EXISTS evidence (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    evidence_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    method TEXT NOT NULL,
    confidence REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    artifact_id TEXT,
    start_byte INTEGER,
    end_byte INTEGER,
    start_line INTEGER,
    start_column INTEGER,
    end_line INTEGER,
    end_column INTEGER,
    tool_id TEXT,
    tool_version TEXT,
    detail TEXT,
    input_digest TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, evidence_id),
    FOREIGN KEY(snapshot_id, artifact_id)
      REFERENCES artifacts(snapshot_id, artifact_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_evidence_snapshot_kind
ON evidence(snapshot_id, kind);
CREATE INDEX IF NOT EXISTS idx_evidence_snapshot_artifact
ON evidence(snapshot_id, artifact_id);

CREATE TABLE IF NOT EXISTS node_evidence (
    snapshot_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'supports',
    PRIMARY KEY(snapshot_id, node_id, evidence_id, role),
    FOREIGN KEY(snapshot_id, node_id)
      REFERENCES nodes(snapshot_id, node_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, evidence_id)
      REFERENCES evidence(snapshot_id, evidence_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS edges (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    edge_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    from_node_id TEXT NOT NULL,
    to_node_id TEXT NOT NULL,
    resolution TEXT NOT NULL CHECK (
      resolution IN (
        'declared',
        'compiler_resolved',
        'syntax_inferred',
        'runtime_observed',
        'documentation_asserted',
        'model_inferred',
        'unresolved'
      )
    ),
    confidence REAL NOT NULL DEFAULT 1.0 CHECK (confidence >= 0.0 AND confidence <= 1.0),
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, edge_id),
    FOREIGN KEY(snapshot_id, from_node_id)
      REFERENCES nodes(snapshot_id, node_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, to_node_id)
      REFERENCES nodes(snapshot_id, node_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_edges_snapshot_from_kind
ON edges(snapshot_id, from_node_id, kind);
CREATE INDEX IF NOT EXISTS idx_edges_snapshot_to_kind
ON edges(snapshot_id, to_node_id, kind);
CREATE INDEX IF NOT EXISTS idx_edges_snapshot_kind_resolution
ON edges(snapshot_id, kind, resolution);

CREATE TABLE IF NOT EXISTS edge_evidence (
    snapshot_id TEXT NOT NULL,
    edge_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'supports',
    PRIMARY KEY(snapshot_id, edge_id, evidence_id, role),
    FOREIGN KEY(snapshot_id, edge_id)
      REFERENCES edges(snapshot_id, edge_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, evidence_id)
      REFERENCES evidence(snapshot_id, evidence_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS documents (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    document_id TEXT NOT NULL,
    logical_document_id TEXT,
    kind TEXT NOT NULL,
    title TEXT NOT NULL,
    path TEXT,
    generator TEXT NOT NULL,
    generator_version TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('draft', 'validated', 'rejected', 'published')),
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, document_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS document_sections (
    snapshot_id TEXT NOT NULL,
    section_id TEXT NOT NULL,
    document_id TEXT NOT NULL,
    parent_section_id TEXT,
    ordinal INTEGER NOT NULL,
    heading TEXT,
    body_markdown TEXT NOT NULL,
    body_text TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, section_id),
    FOREIGN KEY(snapshot_id, document_id)
      REFERENCES documents(snapshot_id, document_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, parent_section_id)
      REFERENCES document_sections(snapshot_id, section_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_sections_document_ordinal
ON document_sections(snapshot_id, document_id, ordinal);

CREATE TABLE IF NOT EXISTS section_evidence (
    snapshot_id TEXT NOT NULL,
    section_id TEXT NOT NULL,
    evidence_id TEXT NOT NULL,
    claim_key TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(snapshot_id, section_id, evidence_id, claim_key),
    FOREIGN KEY(snapshot_id, section_id)
      REFERENCES document_sections(snapshot_id, section_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, evidence_id)
      REFERENCES evidence(snapshot_id, evidence_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS chunks (
    snapshot_id TEXT NOT NULL,
    chunk_id TEXT NOT NULL,
    document_id TEXT,
    section_id TEXT,
    node_id TEXT,
    ordinal INTEGER NOT NULL,
    token_count INTEGER,
    text TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, chunk_id),
    FOREIGN KEY(snapshot_id, document_id)
      REFERENCES documents(snapshot_id, document_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, section_id)
      REFERENCES document_sections(snapshot_id, section_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, node_id)
      REFERENCES nodes(snapshot_id, node_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE VIRTUAL TABLE IF NOT EXISTS search_fts USING fts5(
    snapshot_id UNINDEXED,
    object_type UNINDEXED,
    object_id UNINDEXED,
    title,
    qualified_name,
    signature,
    body,
    tokenize = 'unicode61 remove_diacritics 2 tokenchars ''_:.#/-'''
);

CREATE TABLE IF NOT EXISTS embeddings (
    snapshot_id TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    dimensions INTEGER NOT NULL CHECK (dimensions > 0),
    vector_blob BLOB NOT NULL,
    content_sha256 TEXT NOT NULL,
    PRIMARY KEY(snapshot_id, object_type, object_id, model_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS diagnostics (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    diagnostic_id TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('note', 'warning', 'error', 'fatal')),
    code TEXT NOT NULL,
    message TEXT NOT NULL,
    artifact_id TEXT,
    start_line INTEGER,
    start_column INTEGER,
    stage TEXT,
    plugin_id TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, diagnostic_id),
    FOREIGN KEY(snapshot_id, artifact_id)
      REFERENCES artifacts(snapshot_id, artifact_id) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_diagnostics_snapshot_severity
ON diagnostics(snapshot_id, severity);

CREATE TABLE IF NOT EXISTS tool_runs (
    run_id TEXT PRIMARY KEY,
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    stage TEXT NOT NULL,
    tool_id TEXT NOT NULL,
    tool_version TEXT NOT NULL,
    input_digest TEXT NOT NULL,
    config_digest TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'complete', 'failed', 'cancelled', 'cached')),
    exit_code INTEGER,
    peak_rss_bytes INTEGER,
    cpu_time_ms INTEGER,
    output_digest TEXT,
    log_object_key TEXT,
    attributes_json TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX IF NOT EXISTS idx_tool_runs_snapshot_stage
ON tool_runs(snapshot_id, stage);

CREATE TABLE IF NOT EXISTS jobs (
    job_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    repository_id TEXT,
    snapshot_id TEXT,
    kind TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    priority INTEGER NOT NULL DEFAULT 0,
    attempt INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    lease_owner TEXT,
    lease_until TEXT,
    not_before TEXT,
    payload_json TEXT NOT NULL,
    result_json TEXT,
    error_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_jobs_claim
ON jobs(state, not_before, priority DESC, created_at);

CREATE TABLE IF NOT EXISTS cache_entries (
    cache_key TEXT PRIMARY KEY,
    stage TEXT NOT NULL,
    plugin_id TEXT,
    plugin_version TEXT,
    schema_version TEXT NOT NULL,
    input_digest TEXT NOT NULL,
    config_digest TEXT NOT NULL,
    toolchain_digest TEXT,
    output_object_key TEXT NOT NULL,
    output_digest TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    created_at TEXT NOT NULL,
    last_accessed_at TEXT NOT NULL,
    expires_at TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX IF NOT EXISTS idx_cache_last_accessed
ON cache_entries(last_accessed_at);

CREATE TABLE IF NOT EXISTS audit_events (
    event_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    actor_type TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT,
    outcome TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    source_ip TEXT,
    request_id TEXT,
    detail_json TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX IF NOT EXISTS idx_audit_workspace_time
ON audit_events(workspace_id, occurred_at DESC);


CREATE TABLE IF NOT EXISTS conflicts (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    conflict_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    preferred_id TEXT,
    resolution TEXT,
    candidate_ids_json TEXT NOT NULL DEFAULT '[]',
    evidence_ids_json TEXT NOT NULL DEFAULT '[]',
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, conflict_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS claims (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    claim_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    text TEXT NOT NULL,
    category TEXT,
    certainty TEXT NOT NULL CHECK (certainty IN ('supported','inferred','uncertain','contradicted')),
    generator TEXT NOT NULL,
    generator_version TEXT,
    validation TEXT NOT NULL CHECK (validation IN ('pending','accepted','rejected','inference','stale')),
    evidence_ids_json TEXT NOT NULL DEFAULT '[]',
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, claim_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS execution_paths (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    path_id TEXT NOT NULL,
    name TEXT NOT NULL,
    entry_node_id TEXT NOT NULL,
    exit_node_id TEXT,
    node_ids_json TEXT NOT NULL,
    edge_ids_json TEXT NOT NULL,
    evidence_ids_json TEXT NOT NULL DEFAULT '[]',
    attributes_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(snapshot_id, path_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS coverage_records (
    snapshot_id TEXT PRIMARY KEY REFERENCES snapshots(snapshot_id) ON DELETE CASCADE,
    content_json TEXT NOT NULL,
    deterministic_output_digest TEXT NOT NULL
) STRICT;

COMMIT;
