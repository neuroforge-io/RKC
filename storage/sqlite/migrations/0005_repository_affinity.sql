PRAGMA foreign_keys = ON;

BEGIN IMMEDIATE;

CREATE TEMP TABLE migration_0005_guard (
    version TEXT NOT NULL CHECK (version = '0.4.0')
) STRICT;
INSERT INTO migration_0005_guard(version)
VALUES (COALESCE(
    (SELECT value FROM schema_meta WHERE key = 'schema_version'),
    ''
));
DROP TABLE migration_0005_guard;

-- Version 0.5 replaces the unused plaintext-origin slot with a privacy-safe
-- repository affinity. The Go migration preflight has already rejected any
-- historical noncanonical or identity-inconsistent snapshot provenance.
ALTER TABLE repositories
RENAME COLUMN canonical_origin TO repository_affinity;

-- RepositoryID is the public opaque identity retained by every privacy mode.
-- Overwrite all legacy values so a previously populated origin cannot survive
-- migration in the affinity slot. New repositories remain unbound (NULL) only
-- between BeginBuild and their first successful Commit.
UPDATE repositories
SET repository_affinity = repository_id;

CREATE TRIGGER IF NOT EXISTS repositories_affinity_immutable_guard
BEFORE UPDATE OF repository_affinity ON repositories
WHEN OLD.repository_affinity IS NOT NULL
 AND NEW.repository_affinity IS NOT OLD.repository_affinity
BEGIN
  SELECT RAISE(ABORT, 'repository affinity is immutable after binding');
END;

CREATE TRIGGER IF NOT EXISTS repositories_current_snapshot_affinity_required_guard
BEFORE UPDATE OF current_snapshot_id, repository_affinity ON repositories
WHEN NEW.current_snapshot_id IS NOT NULL
 AND NEW.repository_affinity IS NULL
BEGIN
  SELECT RAISE(ABORT, 'published repository requires a bound affinity');
END;

CREATE TRIGGER IF NOT EXISTS repositories_current_snapshot_affinity_guard
BEFORE UPDATE OF current_snapshot_id, repository_affinity ON repositories
WHEN NEW.current_snapshot_id IS NOT NULL
 AND NEW.repository_affinity <> NEW.repository_id
BEGIN
  SELECT RAISE(ABORT, 'published repository affinity differs from its identity');
END;

UPDATE schema_meta
SET value = '0.5.0'
WHERE key = 'schema_version';

COMMIT;
