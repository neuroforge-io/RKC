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
VALUES ('schema_version', '0.4.0')
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

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL UNIQUE CHECK (length(name) > 0),
    target_schema_version TEXT NOT NULL UNIQUE
      CHECK (length(target_schema_version) > 0),
    sha256 TEXT NOT NULL CHECK (
        length(sha256) = 64
        AND sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    applied_at TEXT,
    CHECK (
      (version IN (1, 2) AND applied_at IS NULL)
      OR (
        version >= 3
        AND applied_at IS NOT NULL
        AND length(applied_at) > 0
      )
    )
) STRICT;

INSERT INTO schema_migrations(
    version,
    name,
    target_schema_version,
    sha256,
    applied_at
)
VALUES
  (
    1,
    'initial',
    '0.1.0',
    '648b7797e44c1346342959ec872ba3f210cac73d389b5c829f265b0c0cf91150',
    NULL
  ),
  (
    2,
    'claims_conflicts_paths',
    '0.2.0',
    '4a8f0853f4fc5fd3c2e1d5a3b5f17ad66c34b8585b892f46281d5b0fcaa105d2',
    NULL
  ),
  (
    3,
    'transactional_publication',
    '0.3.0',
    '340a18941b1db769620a364e8893669636b96cbc966f2750739d1f93bacbe2cc',
    '2026-07-22T00:00:00Z'
  ),
  (
    4,
    'publication_compare_and_swap',
    '0.4.0',
    'c75e1bc04038c3385acd15fc370c0a866929d33102885a72b305a1a28d9635fc',
    '2026-07-22T00:00:00Z'
  );

CREATE TABLE IF NOT EXISTS builds (
    build_id TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL
      REFERENCES repositories(repository_id) ON DELETE CASCADE,
    base_current_snapshot_id TEXT,
    parent_snapshot_id TEXT,
    expected_schema TEXT NOT NULL,
    state TEXT NOT NULL CHECK (
      state IN ('open', 'committed', 'aborted')
    ),
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (
      CASE WHEN json_valid(metadata_json)
      THEN json_type(metadata_json) = 'object'
      ELSE 0 END
    ),
    recovery_state TEXT NOT NULL DEFAULT 'none' CHECK (
      recovery_state IN ('none', 'required', 'recovering', 'recovered', 'failed')
    ),
    recovery_owner TEXT,
    recovery_started_at TEXT,
    recovery_json TEXT NOT NULL DEFAULT '{}' CHECK (
      CASE WHEN json_valid(recovery_json)
      THEN json_type(recovery_json) = 'object'
      ELSE 0 END
    ),
    abort_reason TEXT,
    committed_snapshot_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    validated_at TEXT,
    finished_at TEXT,
    UNIQUE(build_id, repository_id),
    CHECK (base_current_snapshot_id IS parent_snapshot_id),
    CHECK ((state = 'committed') = (committed_snapshot_id IS NOT NULL)),
    FOREIGN KEY(repository_id, base_current_snapshot_id)
      REFERENCES canonical_snapshots(repository_id, snapshot_id)
      ON DELETE RESTRICT,
    FOREIGN KEY(repository_id, parent_snapshot_id)
      REFERENCES canonical_snapshots(repository_id, snapshot_id)
      ON DELETE RESTRICT,
    FOREIGN KEY(repository_id, committed_snapshot_id)
      REFERENCES canonical_snapshots(repository_id, snapshot_id)
      ON DELETE RESTRICT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_builds_repository_state
ON builds(repository_id, state, updated_at);
CREATE INDEX IF NOT EXISTS idx_builds_recovery
ON builds(recovery_state, updated_at);

CREATE TABLE IF NOT EXISTS staged_canonical_records (
    build_id TEXT NOT NULL
      REFERENCES builds(build_id) ON DELETE CASCADE,
    record_family TEXT NOT NULL CHECK (
      record_family IN (
        'artifact',
        'node',
        'edge',
        'evidence',
        'diagnostic',
        'conflict',
        'document',
        'claim',
        'execution_path',
        'coverage'
      )
    ),
    record_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    canonical_record_json TEXT NOT NULL CHECK (
      CASE WHEN json_valid(canonical_record_json)
      THEN json_type(canonical_record_json) = 'object'
      ELSE 0 END
    ),
    canonical_record_sha256 TEXT NOT NULL CHECK (
      length(canonical_record_sha256) = 64
      AND canonical_record_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    PRIMARY KEY(build_id, record_family, record_id),
    UNIQUE(build_id, record_family, ordinal)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS canonical_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL
      REFERENCES repositories(repository_id) ON DELETE CASCADE,
    parent_snapshot_id TEXT,
    build_id TEXT NOT NULL UNIQUE,
    schema_version TEXT NOT NULL,
    publication_status TEXT NOT NULL DEFAULT 'committed'
      CHECK (publication_status = 'committed'),
    legacy_projection_status TEXT NOT NULL DEFAULT 'complete'
      CHECK (legacy_projection_status = 'complete'),
    canonical_snapshot_json TEXT NOT NULL CHECK (
      CASE WHEN json_valid(canonical_snapshot_json)
      THEN json_type(canonical_snapshot_json) = 'object'
        AND COALESCE(
          json_extract(canonical_snapshot_json, '$.id') = snapshot_id,
          0
        )
        AND COALESCE(
          json_extract(canonical_snapshot_json, '$.repository_id') = repository_id,
          0
        )
        AND COALESCE(
          json_extract(canonical_snapshot_json, '$.schema_version') = schema_version,
          0
        )
        AND COALESCE(
          json_extract(canonical_snapshot_json, '$.status') = 'committed',
          0
        )
      ELSE 0 END
    ),
    canonical_bundle_json TEXT NOT NULL CHECK (
      CASE WHEN json_valid(canonical_bundle_json)
      THEN json_type(canonical_bundle_json) = 'object'
        AND COALESCE(
          json_extract(canonical_bundle_json, '$.snapshot.id') = snapshot_id,
          0
        )
        AND COALESCE(
          json_extract(canonical_bundle_json, '$.snapshot.repository_id') = repository_id,
          0
        )
        AND COALESCE(
          json_extract(canonical_bundle_json, '$.snapshot.schema_version') = schema_version,
          0
        )
        AND COALESCE(
          json_extract(canonical_bundle_json, '$.snapshot.status') = 'committed',
          0
        )
      ELSE 0 END
    ),
    canonical_digest TEXT NOT NULL CHECK (
      length(canonical_digest) = 64
      AND canonical_digest NOT GLOB '*[^0-9a-f]*'
    ),
    published_at TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (
      CASE WHEN json_valid(metadata_json)
      THEN json_type(metadata_json) = 'object'
      ELSE 0 END
    ),
    UNIQUE(repository_id, snapshot_id),
    FOREIGN KEY(build_id, repository_id)
      REFERENCES builds(build_id, repository_id) ON DELETE RESTRICT,
    FOREIGN KEY(repository_id, parent_snapshot_id)
      REFERENCES canonical_snapshots(repository_id, snapshot_id)
      ON DELETE RESTRICT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_canonical_snapshots_repository_published
ON canonical_snapshots(repository_id, published_at DESC, snapshot_id);

CREATE TRIGGER IF NOT EXISTS canonical_snapshots_build_open_insert_guard
BEFORE INSERT ON canonical_snapshots
WHEN NOT EXISTS (
  SELECT 1
  FROM builds
  WHERE build_id = NEW.build_id
    AND repository_id = NEW.repository_id
    AND state = 'open'
)
BEGIN
  SELECT RAISE(ABORT, 'canonical snapshot requires an open owning build');
END;

CREATE TRIGGER IF NOT EXISTS canonical_snapshots_build_lineage_insert_guard
BEFORE INSERT ON canonical_snapshots
WHEN NOT EXISTS (
  SELECT 1
  FROM builds AS build
  WHERE build.build_id = NEW.build_id
    AND build.repository_id = NEW.repository_id
    AND build.base_current_snapshot_id IS NEW.parent_snapshot_id
    AND build.parent_snapshot_id IS NEW.parent_snapshot_id
)
BEGIN
  SELECT RAISE(ABORT, 'canonical snapshot lineage differs from its owning build');
END;

CREATE TRIGGER IF NOT EXISTS canonical_snapshots_update_guard
BEFORE UPDATE ON canonical_snapshots
BEGIN
  SELECT RAISE(ABORT, 'canonical snapshots are immutable');
END;

CREATE TRIGGER IF NOT EXISTS canonical_snapshots_delete_guard
BEFORE DELETE ON canonical_snapshots
BEGIN
  SELECT RAISE(ABORT, 'canonical snapshots are immutable');
END;

CREATE TRIGGER IF NOT EXISTS builds_initial_state_guard
BEFORE INSERT ON builds
WHEN NEW.state <> 'open'
BEGIN
  SELECT RAISE(ABORT, 'new builds must be open');
END;

CREATE TRIGGER IF NOT EXISTS builds_state_transition_guard
BEFORE UPDATE OF state ON builds
WHEN NEW.state <> OLD.state
 AND NOT (
   OLD.state = 'open'
   AND NEW.state IN ('committed', 'aborted')
 )
BEGIN
  SELECT RAISE(ABORT, 'build state transition is not monotonic');
END;

CREATE TRIGGER IF NOT EXISTS builds_closed_update_guard
BEFORE UPDATE ON builds
WHEN OLD.state IN ('committed', 'aborted')
BEGIN
  SELECT RAISE(ABORT, 'closed builds are immutable');
END;

CREATE TRIGGER IF NOT EXISTS builds_closed_delete_guard
BEFORE DELETE ON builds
WHEN OLD.state IN ('committed', 'aborted')
BEGIN
  SELECT RAISE(ABORT, 'closed builds are immutable');
END;

CREATE TRIGGER IF NOT EXISTS builds_commit_snapshot_guard
BEFORE UPDATE OF state, committed_snapshot_id ON builds
WHEN NEW.state = 'committed'
 AND NOT EXISTS (
   SELECT 1
   FROM canonical_snapshots
   WHERE snapshot_id = NEW.committed_snapshot_id
     AND repository_id = NEW.repository_id
     AND build_id = NEW.build_id
 )
BEGIN
  SELECT RAISE(ABORT, 'committed build requires its canonical snapshot');
END;

CREATE TRIGGER IF NOT EXISTS builds_canonical_snapshot_lineage_update_guard
BEFORE UPDATE OF base_current_snapshot_id, parent_snapshot_id ON builds
WHEN NEW.base_current_snapshot_id IS NOT OLD.base_current_snapshot_id
  OR NEW.parent_snapshot_id IS NOT OLD.parent_snapshot_id
BEGIN
  SELECT RAISE(ABORT, 'build lineage is immutable after creation');
END;

CREATE TRIGGER IF NOT EXISTS builds_commit_compare_and_swap_guard
BEFORE UPDATE OF state, committed_snapshot_id ON builds
WHEN OLD.state = 'open'
 AND NEW.state = 'committed'
 AND NOT EXISTS (
   SELECT 1
   FROM repositories AS repository
   WHERE repository.repository_id = NEW.repository_id
     AND repository.current_snapshot_id IS NEW.base_current_snapshot_id
 )
BEGIN
  SELECT RAISE(ABORT, 'current snapshot changed since the build started');
END;

CREATE TABLE IF NOT EXISTS canonical_snapshot_records (
    snapshot_id TEXT NOT NULL
      REFERENCES canonical_snapshots(snapshot_id) ON DELETE CASCADE,
    record_family TEXT NOT NULL CHECK (
      record_family IN (
        'artifact',
        'node',
        'edge',
        'evidence',
        'diagnostic',
        'conflict',
        'document',
        'claim',
        'execution_path',
        'coverage'
      )
    ),
    record_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    canonical_record_json TEXT NOT NULL CHECK (
      CASE WHEN json_valid(canonical_record_json)
      THEN json_type(canonical_record_json) = 'object'
      ELSE 0 END
    ),
    canonical_record_sha256 TEXT NOT NULL CHECK (
      length(canonical_record_sha256) = 64
      AND canonical_record_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    PRIMARY KEY(snapshot_id, record_family, record_id),
    UNIQUE(snapshot_id, record_family, ordinal)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER IF NOT EXISTS staged_canonical_records_insert_guard
BEFORE INSERT ON staged_canonical_records
WHEN NOT EXISTS (
  SELECT 1 FROM builds
  WHERE build_id = NEW.build_id AND state = 'open'
)
BEGIN
  SELECT RAISE(ABORT, 'staged records require an open build');
END;

CREATE TRIGGER IF NOT EXISTS staged_canonical_records_update_guard
BEFORE UPDATE ON staged_canonical_records
WHEN NOT EXISTS (
  SELECT 1 FROM builds
  WHERE build_id = OLD.build_id AND state = 'open'
) OR NOT EXISTS (
  SELECT 1 FROM builds
  WHERE build_id = NEW.build_id AND state = 'open'
)
BEGIN
  SELECT RAISE(ABORT, 'staged records require an open build');
END;

CREATE TRIGGER IF NOT EXISTS staged_canonical_records_delete_guard
BEFORE DELETE ON staged_canonical_records
WHEN NOT EXISTS (
  SELECT 1 FROM builds
  WHERE build_id = OLD.build_id AND state = 'open'
)
BEGIN
  SELECT RAISE(ABORT, 'staged records require an open build');
END;

CREATE TRIGGER IF NOT EXISTS builds_close_staging_guard
BEFORE UPDATE OF state ON builds
WHEN NEW.state IN ('committed', 'aborted')
 AND NEW.state <> OLD.state
 AND EXISTS (
   SELECT 1 FROM staged_canonical_records
   WHERE build_id = OLD.build_id
 )
BEGIN
  SELECT RAISE(ABORT, 'build staging must be empty before closure');
END;

CREATE TRIGGER IF NOT EXISTS canonical_snapshot_records_insert_guard
BEFORE INSERT ON canonical_snapshot_records
WHEN NOT EXISTS (
  SELECT 1
  FROM canonical_snapshots AS snapshot
  JOIN builds AS build ON build.build_id = snapshot.build_id
  WHERE snapshot.snapshot_id = NEW.snapshot_id
    AND build.state = 'open'
)
BEGIN
  SELECT RAISE(ABORT, 'canonical records require an open owning build');
END;

CREATE TRIGGER IF NOT EXISTS canonical_snapshot_records_update_guard
BEFORE UPDATE ON canonical_snapshot_records
BEGIN
  SELECT RAISE(ABORT, 'canonical snapshot records are immutable');
END;

CREATE TRIGGER IF NOT EXISTS canonical_snapshot_records_delete_guard
BEFORE DELETE ON canonical_snapshot_records
BEGIN
  SELECT RAISE(ABORT, 'canonical snapshot records are immutable');
END;

ALTER TABLE repositories
ADD COLUMN current_snapshot_id TEXT
  REFERENCES canonical_snapshots(snapshot_id) ON DELETE SET NULL;

CREATE TRIGGER IF NOT EXISTS repositories_current_snapshot_insert_guard
BEFORE INSERT ON repositories
WHEN NEW.current_snapshot_id IS NOT NULL
BEGIN
  SELECT RAISE(ABORT, 'new repository cannot publish an existing snapshot');
END;

CREATE TRIGGER IF NOT EXISTS repositories_current_snapshot_repository_guard
BEFORE UPDATE OF current_snapshot_id ON repositories
WHEN NEW.current_snapshot_id IS NOT NULL
 AND NOT EXISTS (
   SELECT 1
   FROM canonical_snapshots
   WHERE snapshot_id = NEW.current_snapshot_id
     AND repository_id = NEW.repository_id
 )
BEGIN
  SELECT RAISE(ABORT, 'current snapshot belongs to another repository');
END;

CREATE TRIGGER IF NOT EXISTS repositories_current_snapshot_committed_guard
BEFORE UPDATE OF current_snapshot_id ON repositories
WHEN NEW.current_snapshot_id IS NOT NULL
 AND NOT EXISTS (
   SELECT 1
   FROM canonical_snapshots AS snapshot
   JOIN builds AS build
     ON build.build_id = snapshot.build_id
    AND build.repository_id = snapshot.repository_id
   WHERE snapshot.snapshot_id = NEW.current_snapshot_id
     AND snapshot.repository_id = NEW.repository_id
     AND build.state = 'committed'
     AND build.committed_snapshot_id = snapshot.snapshot_id
 )
BEGIN
  SELECT RAISE(ABORT, 'current snapshot requires a committed owning build');
END;

CREATE TRIGGER IF NOT EXISTS repositories_current_snapshot_clear_guard
BEFORE UPDATE OF current_snapshot_id ON repositories
WHEN OLD.current_snapshot_id IS NOT NULL
 AND NEW.current_snapshot_id IS NULL
BEGIN
  SELECT RAISE(ABORT, 'current snapshot cannot be cleared after publication');
END;

CREATE TRIGGER IF NOT EXISTS repositories_current_snapshot_compare_and_swap_guard
BEFORE UPDATE OF current_snapshot_id ON repositories
WHEN NEW.current_snapshot_id IS NOT NULL
 AND NEW.current_snapshot_id IS NOT OLD.current_snapshot_id
 AND NOT EXISTS (
   SELECT 1
   FROM canonical_snapshots AS snapshot
   JOIN builds AS build
     ON build.build_id = snapshot.build_id
    AND build.repository_id = snapshot.repository_id
   WHERE snapshot.snapshot_id = NEW.current_snapshot_id
     AND snapshot.repository_id = NEW.repository_id
     AND snapshot.parent_snapshot_id IS build.base_current_snapshot_id
     AND snapshot.parent_snapshot_id IS build.parent_snapshot_id
     AND build.base_current_snapshot_id IS OLD.current_snapshot_id
     AND build.parent_snapshot_id IS OLD.current_snapshot_id
 )
BEGIN
  SELECT RAISE(ABORT, 'current snapshot changed since the owning build started');
END;

COMMIT;
