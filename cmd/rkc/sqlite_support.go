package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/neuroforge-io/RKC/internal/server"
	sqlitestore "github.com/neuroforge-io/RKC/internal/storage/sqlite"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

type sqlitePublication struct {
	database  *sqlitestore.Database
	Bundle    rkcmodel.Bundle
	Coverage  rkcmodel.Coverage
	Path      string
	Noop      bool
	build     rkcstore.BuildID
	committed bool
	closed    bool
}

func prepareSQLiteBundle(ctx context.Context, path string, bundle rkcmodel.Bundle, metadata map[string]string) (*sqlitePublication, error) {
	absolute, err := canonicalSQLitePath(path)
	if err != nil {
		return nil, err
	}
	database, err := sqlitestore.Open(ctx, sqlitestore.Options{Path: absolute})
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*sqlitePublication, error) {
		return nil, errors.Join(cause, database.Close())
	}
	if _, err := database.Recover(ctx); err != nil {
		return fail(fmt.Errorf("recover SQLite store: %w", err))
	}

	repositoryID := rkcstore.RepositoryID(bundle.Snapshot.RepositoryID)
	current, err := database.Current(ctx, repositoryID)
	switch {
	case err == nil && current.ID == bundle.Snapshot.ID:
		existing, loadErr := database.Coverage(ctx, rkcstore.SnapshotID(current.ID))
		if loadErr != nil {
			return fail(fmt.Errorf("verify idempotent SQLite snapshot: %w", loadErr))
		}
		bundle.Snapshot.ParentSnapshotID = current.ParentSnapshotID
		coverage := rkcmodel.BuildCoverage(bundle)
		if coverage.DeterministicOutputDigest != existing.DeterministicOutputDigest {
			return fail(fmt.Errorf("SQLite snapshot ID %q already identifies different canonical content", current.ID))
		}
		return &sqlitePublication{
			database: database, Bundle: bundle, Coverage: coverage, Path: absolute, Noop: true,
		}, nil
	case err == nil:
		bundle.Snapshot.ParentSnapshotID = current.ID
	case errors.Is(err, rkcstore.ErrSnapshotNotFound):
		bundle.Snapshot.ParentSnapshotID = ""
	case err != nil:
		return fail(fmt.Errorf("load SQLite current snapshot: %w", err))
	}

	coverage := rkcmodel.BuildCoverage(bundle)
	build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{
		RepositoryID: repositoryID, ParentSnapshotID: rkcstore.SnapshotID(bundle.Snapshot.ParentSnapshotID),
		ExpectedSchema: bundle.Snapshot.SchemaVersion, Metadata: metadata,
	})
	if err != nil {
		return fail(fmt.Errorf("begin SQLite snapshot: %w", err))
	}
	if err := rkcstore.StageBundle(ctx, database, build, bundle); err != nil {
		cause := fmt.Errorf("stage SQLite snapshot: %w", err)
		return fail(errors.Join(cause, database.Abort(context.WithoutCancel(ctx), build, cause)))
	}
	validation, err := database.Validate(ctx, build)
	if err != nil {
		cause := fmt.Errorf("validate SQLite snapshot: %w", err)
		return fail(errors.Join(cause, database.Abort(context.WithoutCancel(ctx), build, cause)))
	}
	if !validation.Valid() {
		cause := errors.New("validate SQLite snapshot: staged canonical bundle is invalid")
		return fail(errors.Join(cause, database.Abort(context.WithoutCancel(ctx), build, cause)))
	}
	return &sqlitePublication{
		database: database, Bundle: bundle, Coverage: coverage, Path: absolute, build: build,
	}, nil
}

func (publication *sqlitePublication) Commit(ctx context.Context) error {
	if publication == nil || publication.database == nil || publication.closed {
		return errors.New("SQLite publication is closed")
	}
	if publication.committed || publication.Noop {
		publication.committed = true
		return nil
	}
	if err := publication.database.Commit(ctx, publication.build, publication.Bundle.Snapshot); err != nil {
		return fmt.Errorf("commit SQLite snapshot: %w", err)
	}
	publication.committed = true
	return nil
}

func (publication *sqlitePublication) Close(reason error) error {
	if publication == nil || publication.closed {
		return nil
	}
	publication.closed = true
	var result error
	if !publication.committed && !publication.Noop && publication.build != "" {
		result = publication.database.Abort(context.Background(), publication.build, reason)
	}
	return errors.Join(result, publication.database.Close())
}

func loadSQLiteDataset(ctx context.Context, path string, snapshotID, repositoryID string) (dataset *server.Dataset, resultErr error) {
	absolute, err := canonicalSQLitePath(path)
	if err != nil {
		return nil, err
	}
	if (snapshotID == "") == (repositoryID == "") {
		return nil, errors.New("SQLite dataset requires exactly one of --snapshot or --repository")
	}
	database, err := sqlitestore.Open(ctx, sqlitestore.Options{Path: absolute, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := database.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite store: %w", err))
		}
	}()
	selected := rkcstore.SnapshotID(snapshotID)
	if repositoryID != "" {
		current, err := database.Current(ctx, rkcstore.RepositoryID(repositoryID))
		if err != nil {
			return nil, fmt.Errorf("load SQLite current snapshot: %w", err)
		}
		selected = rkcstore.SnapshotID(current.ID)
	}
	dataset, err = server.LoadStore(ctx, database, selected)
	if err != nil {
		return nil, err
	}
	// Derived semantic and synthesis outputs use the database file as their
	// immutable identity and must remain outside that file.
	dataset.Root = absolute
	return dataset, nil
}

func canonicalSQLitePath(path string) (string, error) {
	if path == "" || path != strings.TrimSpace(path) {
		return "", errors.New("SQLite database path is required without surrounding whitespace")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite database path: %w", err)
	}
	return filepath.Clean(absolute), nil
}
