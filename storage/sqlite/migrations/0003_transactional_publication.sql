PRAGMA foreign_keys = ON;

BEGIN IMMEDIATE;

CREATE TEMP TABLE migration_0003_guard (
    version TEXT NOT NULL CHECK (version = '0.2.0')
) STRICT;
INSERT INTO migration_0003_guard(version)
VALUES (COALESCE(
    (SELECT value FROM schema_meta WHERE key = 'schema_version'),
    ''
));
DROP TABLE migration_0003_guard;

-- Version 0.2 did not persist the lossless canonical bundle required by the
-- transactional publication contract.  Silently upgrading populated legacy
-- databases would therefore manufacture or discard source truth.  A later
-- importer may perform an explicit, audited backfill; this migration only
-- accepts an empty legacy catalogue.
CREATE TEMP TABLE migration_0003_empty_legacy_guard (
    is_empty INTEGER NOT NULL CHECK (is_empty = 1)
) STRICT;
INSERT INTO migration_0003_empty_legacy_guard(is_empty)
SELECT CASE WHEN
    EXISTS (SELECT 1 FROM repositories)
 OR EXISTS (SELECT 1 FROM snapshots)
 OR EXISTS (SELECT 1 FROM artifacts)
 OR EXISTS (SELECT 1 FROM logical_entities)
 OR EXISTS (SELECT 1 FROM nodes)
 OR EXISTS (SELECT 1 FROM evidence)
 OR EXISTS (SELECT 1 FROM node_evidence)
 OR EXISTS (SELECT 1 FROM edges)
 OR EXISTS (SELECT 1 FROM edge_evidence)
 OR EXISTS (SELECT 1 FROM documents)
 OR EXISTS (SELECT 1 FROM document_sections)
 OR EXISTS (SELECT 1 FROM section_evidence)
 OR EXISTS (SELECT 1 FROM chunks)
 OR EXISTS (SELECT 1 FROM embeddings)
 OR EXISTS (SELECT 1 FROM diagnostics)
 OR EXISTS (SELECT 1 FROM tool_runs)
 OR EXISTS (SELECT 1 FROM jobs)
 OR EXISTS (SELECT 1 FROM cache_entries)
 OR EXISTS (SELECT 1 FROM audit_events)
 OR EXISTS (SELECT 1 FROM search_fts)
 OR EXISTS (SELECT 1 FROM conflicts)
 OR EXISTS (SELECT 1 FROM claims)
 OR EXISTS (SELECT 1 FROM execution_paths)
 OR EXISTS (SELECT 1 FROM coverage_records)
THEN 0 ELSE 1 END;
DROP TABLE migration_0003_empty_legacy_guard;

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

UPDATE schema_meta
SET value = '0.3.0'
WHERE key = 'schema_version';

COMMIT;
