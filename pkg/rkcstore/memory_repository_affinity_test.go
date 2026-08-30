package rkcstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const memoryRepositorySecretSentinel = "memory-repository-secret-sentinel"

func TestMemoryStoreRepositoryAffinityAllowsPrivacyModeTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const origin = "https://example.test/Owner/Repo.git"
	repositoryID := RepositoryID(rkcmodel.StableID("repository", origin))
	store := newConformanceStore(t)

	canonicalFirst := memoryBundleWithCanonicalOrigin(
		"privacy-canonical-first",
		repositoryID,
		"",
		time.Unix(100, 0).UTC(),
		origin,
	)
	commitBundle(t, store, canonicalFirst)

	redacted := conformanceBundle(
		"privacy-redacted",
		repositoryID,
		"privacy-canonical-first",
		time.Unix(200, 0).UTC(),
	)
	commitBundle(t, store, redacted)

	canonicalLast := memoryBundleWithCanonicalOrigin(
		"privacy-canonical-last",
		repositoryID,
		"privacy-redacted",
		time.Unix(300, 0).UTC(),
		origin,
	)
	commitBundle(t, store, canonicalLast)

	store.mu.RLock()
	affinity, bound := store.repositoryAffinity[repositoryID]
	store.mu.RUnlock()
	if !bound || affinity != repositoryID {
		t.Fatalf("repository affinity = %q, bound=%t", affinity, bound)
	}
	redactedSnapshot, err := store.Snapshot(ctx, "privacy-redacted")
	if err != nil || redactedSnapshot.Git.Origin != "" {
		t.Fatalf("redacted snapshot = %+v, %v", redactedSnapshot, err)
	}
	current, err := store.Current(ctx, repositoryID)
	if err != nil || current.ID != "privacy-canonical-last" || current.Git.Origin != origin {
		t.Fatalf("current canonical snapshot = %+v, %v", current, err)
	}
}

func TestMemoryStoreRepositoryAffinityConflictRollsBackAndIsRetryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newConformanceStore(t)
	repositoryID := RepositoryID("affinity-repository")
	first := conformanceBundle("affinity-first", repositoryID, "", time.Unix(100, 0).UTC())
	commitBundle(t, store, first)
	second := conformanceBundle("affinity-second", repositoryID, "affinity-first", time.Unix(200, 0).UTC())
	build := beginAndStage(t, store, second, true)

	// Simulate corrupted persisted affinity. Public operations cannot create
	// this state; the mutation exercises the store-level defense directly.
	store.mu.Lock()
	store.repositoryAffinity[repositoryID] = RepositoryID(memoryRepositorySecretSentinel)
	beforeSnapshots := len(store.snapshots)
	beforeCurrent := store.current[repositoryID]
	beforeOpenBuilds := store.openBuilds
	beforeBuildState := store.builds[build].state
	store.mu.Unlock()

	err := store.Commit(ctx, build, second.Snapshot)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("repository affinity conflict = %v", err)
	}
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != CodeConflict {
		t.Fatalf("repository affinity conflict type = %#v", err)
	}
	assertMemoryRepositoryAffinityErrorIsSecretFree(t, err)

	store.mu.RLock()
	afterSnapshots := len(store.snapshots)
	afterCurrent := store.current[repositoryID]
	afterOpenBuilds := store.openBuilds
	afterBuildState := store.builds[build].state
	_, published := store.snapshots["affinity-second"]
	store.mu.RUnlock()
	if afterSnapshots != beforeSnapshots || afterCurrent != beforeCurrent ||
		afterOpenBuilds != beforeOpenBuilds || afterBuildState != beforeBuildState || published {
		t.Fatalf(
			"affinity conflict mutated store: snapshots %d->%d, current %q->%q, open builds %d->%d, state %d->%d, published=%t",
			beforeSnapshots,
			afterSnapshots,
			beforeCurrent,
			afterCurrent,
			beforeOpenBuilds,
			afterOpenBuilds,
			beforeBuildState,
			afterBuildState,
			published,
		)
	}

	// Restoring the stable repository identity allows the exact same build to
	// commit, proving that the rejected attempt did not consume or alter it.
	store.mu.Lock()
	store.repositoryAffinity[repositoryID] = repositoryID
	store.mu.Unlock()
	if err := store.Commit(ctx, build, second.Snapshot); err != nil {
		t.Fatalf("retry after restoring affinity = %v", err)
	}
	currentSnapshot, err := store.Current(ctx, repositoryID)
	if err != nil || currentSnapshot.ID != "affinity-second" {
		t.Fatalf("current after retry = %+v, %v", currentSnapshot, err)
	}
}

func TestMemoryStoreReadersRejectRepositoryAffinityCorruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newConformanceStore(t)
	repositoryID := RepositoryID("reader-affinity-repository")
	bundle := conformanceBundle("reader-affinity", repositoryID, "", time.Unix(100, 0).UTC())
	commitBundle(t, store, bundle)

	store.mu.Lock()
	store.repositoryAffinity[repositoryID] = RepositoryID(memoryRepositorySecretSentinel)
	store.mu.Unlock()

	readers := []struct {
		name string
		read func() error
	}{
		{name: "snapshot", read: func() error { _, err := store.Snapshot(ctx, "reader-affinity"); return err }},
		{name: "bundle", read: func() error { _, err := store.Bundle(ctx, "reader-affinity"); return err }},
		{name: "current", read: func() error { _, err := store.Current(ctx, repositoryID); return err }},
		{name: "list", read: func() error {
			_, err := store.ListSnapshots(ctx, SnapshotQuery{RepositoryID: repositoryID})
			return err
		}},
		{name: "artifact", read: func() error { _, err := store.Artifact(ctx, "reader-affinity", "artifact"); return err }},
		{name: "node", read: func() error { _, err := store.Node(ctx, "reader-affinity", "node-a"); return err }},
		{name: "evidence", read: func() error { _, err := store.Evidence(ctx, "reader-affinity", "evidence"); return err }},
		{name: "coverage", read: func() error { _, err := store.Coverage(ctx, "reader-affinity"); return err }},
		{name: "query nodes", read: func() error { _, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "reader-affinity"}); return err }},
		{name: "query edges", read: func() error { _, err := store.QueryEdges(ctx, EdgeQuery{SnapshotID: "reader-affinity"}); return err }},
		{name: "query diagnostics", read: func() error {
			_, err := store.QueryDiagnostics(ctx, DiagnosticQuery{SnapshotID: "reader-affinity"})
			return err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			err := reader.read()
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("reader error = %v", err)
			}
			var operationError *OperationError
			if !errors.As(err, &operationError) || operationError.Code != CodeValidation {
				t.Fatalf("reader error type = %#v", err)
			}
			assertMemoryRepositoryAffinityErrorIsSecretFree(t, err)
		})
	}
}

func memoryBundleWithCanonicalOrigin(
	id string,
	repositoryID RepositoryID,
	parent SnapshotID,
	created time.Time,
	origin string,
) rkcmodel.Bundle {
	bundle := conformanceBundle(id, repositoryID, parent, created)
	bundle.Snapshot.Git.Origin = origin
	bundle.Snapshot.Metadata["source_reference"] = origin
	bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{
		ID:            string(repositoryID),
		LogicalID:     string(repositoryID),
		Kind:          "repository",
		Name:          "Repo",
		QualifiedName: origin,
		Attributes:    map[string]any{"git_origin": origin},
	})
	return bundle
}

func assertMemoryRepositoryAffinityErrorIsSecretFree(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a repository-affinity error")
	}
	if strings.Contains(err.Error(), memoryRepositorySecretSentinel) {
		t.Fatalf("repository-affinity error disclosed secret sentinel: %v", err)
	}
}
