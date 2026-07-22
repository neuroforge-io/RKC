PRAGMA foreign_keys = ON;

BEGIN IMMEDIATE;

CREATE TEMP TABLE migration_0004_guard (
    version TEXT NOT NULL CHECK (version = '0.3.0')
) STRICT;
INSERT INTO migration_0004_guard(version)
VALUES (COALESCE(
    (SELECT value FROM schema_meta WHERE key = 'schema_version'),
    ''
));
DROP TABLE migration_0004_guard;

-- Version 0.3 admitted a lost update when two builds started from the same
-- current snapshot and the older build published last.  Refuse to install the
-- compare-and-swap guards over a catalogue whose existing snapshot lineage no
-- longer agrees with its owning build.
CREATE TEMP TABLE migration_0004_lineage_guard (
    is_consistent INTEGER NOT NULL CHECK (is_consistent = 1)
) STRICT;
INSERT INTO migration_0004_lineage_guard(is_consistent)
SELECT CASE WHEN EXISTS (
  SELECT 1
  FROM canonical_snapshots AS snapshot
  WHERE NOT EXISTS (
    SELECT 1
    FROM builds AS build
    WHERE build.build_id = snapshot.build_id
      AND build.repository_id = snapshot.repository_id
      AND build.base_current_snapshot_id IS snapshot.parent_snapshot_id
      AND build.parent_snapshot_id IS snapshot.parent_snapshot_id
  )
) THEN 0 ELSE 1 END;
DROP TABLE migration_0004_lineage_guard;

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

UPDATE schema_meta
SET value = '0.4.0'
WHERE key = 'schema_version';

COMMIT;
