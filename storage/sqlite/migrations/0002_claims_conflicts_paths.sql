PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = MEMORY;
PRAGMA trusted_schema = OFF;

BEGIN IMMEDIATE;

CREATE TEMP TABLE migration_0002_guard (
    version TEXT NOT NULL CHECK (version = '0.1.0')
) STRICT;
INSERT INTO migration_0002_guard(version)
VALUES (COALESCE(
    (SELECT value FROM schema_meta WHERE key = 'schema_version'),
    ''
));
DROP TABLE migration_0002_guard;

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

UPDATE schema_meta
SET value = '0.2.0'
WHERE key = 'schema_version';

COMMIT;
