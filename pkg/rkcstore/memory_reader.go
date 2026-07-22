package rkcstore

import (
	"context"
	"sort"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func (store *MemoryStore) Snapshot(ctx context.Context, id SnapshotID) (rkcmodel.Snapshot, error) {
	const operation = "read snapshot"
	if err := validReadID(ctx, operation, "snapshot_id", string(id)); err != nil {
		return rkcmodel.Snapshot{}, err
	}
	store.mu.RLock()
	snapshot, ok := store.snapshots[id]
	store.mu.RUnlock()
	if !ok {
		return rkcmodel.Snapshot{}, storeError(CodeSnapshotNotFound, operation, "", id, "", nil)
	}
	return cloneJSON(snapshot.bundle.Snapshot)
}

func (store *MemoryStore) Bundle(ctx context.Context, id SnapshotID) (rkcmodel.Bundle, error) {
	const operation = "read bundle"
	if err := validReadID(ctx, operation, "snapshot_id", string(id)); err != nil {
		return rkcmodel.Bundle{}, err
	}
	store.mu.RLock()
	snapshot, ok := store.snapshots[id]
	store.mu.RUnlock()
	if !ok {
		return rkcmodel.Bundle{}, storeError(CodeSnapshotNotFound, operation, "", id, "", nil)
	}
	return cloneJSON(snapshot.bundle)
}

func (store *MemoryStore) Current(ctx context.Context, repositoryID RepositoryID) (rkcmodel.Snapshot, error) {
	const operation = "read current snapshot"
	if err := validReadID(ctx, operation, "repository_id", string(repositoryID)); err != nil {
		return rkcmodel.Snapshot{}, err
	}
	store.mu.RLock()
	id, ok := store.current[repositoryID]
	if !ok {
		store.mu.RUnlock()
		return rkcmodel.Snapshot{}, storeError(CodeSnapshotNotFound, operation, "", "", "repository_id", nil)
	}
	snapshot, ok := store.snapshots[id]
	store.mu.RUnlock()
	if !ok {
		return rkcmodel.Snapshot{}, storeError(CodeSnapshotNotFound, operation, "", id, "current", nil)
	}
	return cloneJSON(snapshot.bundle.Snapshot)
}

func (store *MemoryStore) ListSnapshots(ctx context.Context, query SnapshotQuery) (SnapshotPage, error) {
	const operation = "list snapshots"
	if err := checkContext(ctx, operation); err != nil {
		return SnapshotPage{}, err
	}
	limit, err := pageLimit(operation, query.PageRequest)
	if err != nil {
		return SnapshotPage{}, err
	}
	if query.RepositoryID != "" {
		if err := validIdentifier(operation, "repository_id", string(query.RepositoryID)); err != nil {
			return SnapshotPage{}, err
		}
	}
	scope := scopeFingerprint(string(query.RepositoryID))
	payload, err := store.openCursor(operation, query.Cursor, "snapshots", scope)
	if err != nil {
		return SnapshotPage{}, err
	}

	store.mu.RLock()
	values := make([]rkcmodel.Snapshot, 0, len(store.snapshots))
	for _, stored := range store.snapshots {
		if query.RepositoryID == "" || RepositoryID(stored.bundle.Snapshot.RepositoryID) == query.RepositoryID {
			values = append(values, stored.bundle.Snapshot)
		}
	}
	store.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].CreatedAt.After(values[j].CreatedAt)
	})
	start, err := snapshotStart(operation, values, payload)
	if err != nil {
		return SnapshotPage{}, err
	}
	end := boundedEnd(start, limit, len(values))
	items, err := cloneJSON(values[start:end])
	if err != nil {
		return SnapshotPage{}, err
	}
	page := SnapshotPage{Items: items}
	if end < len(values) {
		last := values[end-1]
		page.Next, err = store.sealCursor("snapshots", scope, snapshotSortTime(last), last.ID)
	}
	return page, err
}

func (store *MemoryStore) Artifact(ctx context.Context, snapshotID SnapshotID, artifactID string) (rkcmodel.Artifact, error) {
	const operation = "read artifact"
	if err := validLookup(ctx, operation, snapshotID, "artifact_id", artifactID); err != nil {
		return rkcmodel.Artifact{}, err
	}
	store.mu.RLock()
	snapshot, ok := store.snapshots[snapshotID]
	if !ok {
		store.mu.RUnlock()
		return rkcmodel.Artifact{}, storeError(CodeSnapshotNotFound, operation, "", snapshotID, "", nil)
	}
	var value rkcmodel.Artifact
	found := false
	for _, candidate := range snapshot.bundle.Artifacts {
		if candidate.ID == artifactID {
			value, found = candidate, true
			break
		}
	}
	store.mu.RUnlock()
	if !found {
		return rkcmodel.Artifact{}, recordNotFound(operation, snapshotID, "artifact_id", artifactID)
	}
	return cloneJSON(value)
}

func (store *MemoryStore) Node(ctx context.Context, snapshotID SnapshotID, nodeID string) (rkcmodel.Node, error) {
	const operation = "read node"
	if err := validLookup(ctx, operation, snapshotID, "node_id", nodeID); err != nil {
		return rkcmodel.Node{}, err
	}
	store.mu.RLock()
	snapshot, ok := store.snapshots[snapshotID]
	if !ok {
		store.mu.RUnlock()
		return rkcmodel.Node{}, storeError(CodeSnapshotNotFound, operation, "", snapshotID, "", nil)
	}
	var value rkcmodel.Node
	found := false
	for _, candidate := range snapshot.bundle.Nodes {
		if candidate.ID == nodeID {
			value, found = candidate, true
			break
		}
	}
	store.mu.RUnlock()
	if !found {
		return rkcmodel.Node{}, recordNotFound(operation, snapshotID, "node_id", nodeID)
	}
	return cloneJSON(value)
}

func (store *MemoryStore) Evidence(ctx context.Context, snapshotID SnapshotID, evidenceID string) (rkcmodel.Evidence, error) {
	const operation = "read evidence"
	if err := validLookup(ctx, operation, snapshotID, "evidence_id", evidenceID); err != nil {
		return rkcmodel.Evidence{}, err
	}
	store.mu.RLock()
	snapshot, ok := store.snapshots[snapshotID]
	if !ok {
		store.mu.RUnlock()
		return rkcmodel.Evidence{}, storeError(CodeSnapshotNotFound, operation, "", snapshotID, "", nil)
	}
	var value rkcmodel.Evidence
	found := false
	for _, candidate := range snapshot.bundle.Evidence {
		if candidate.ID == evidenceID {
			value, found = candidate, true
			break
		}
	}
	store.mu.RUnlock()
	if !found {
		return rkcmodel.Evidence{}, recordNotFound(operation, snapshotID, "evidence_id", evidenceID)
	}
	return cloneJSON(value)
}

func (store *MemoryStore) QueryNodes(ctx context.Context, query NodeQuery) (NodePage, error) {
	const operation = "query nodes"
	limit, snapshot, err := store.querySnapshot(ctx, operation, query.SnapshotID, query.PageRequest)
	if err != nil {
		return NodePage{}, err
	}
	scope := scopeFingerprint(string(query.SnapshotID), query.Kind, query.Language, query.ArtifactID, query.Visibility)
	payload, err := store.openCursor(operation, query.Cursor, "nodes", scope)
	if err != nil {
		return NodePage{}, err
	}
	values := make([]rkcmodel.Node, 0, len(snapshot.bundle.Nodes))
	for _, value := range snapshot.bundle.Nodes {
		if (query.Kind == "" || value.Kind == query.Kind) &&
			(query.Language == "" || value.Language == query.Language) &&
			(query.ArtifactID == "" || value.ArtifactID == query.ArtifactID) &&
			(query.Visibility == "" || value.Visibility == query.Visibility) {
			values = append(values, value)
		}
	}
	return store.nodePage(operation, values, limit, payload, scope)
}

func (store *MemoryStore) QueryEdges(ctx context.Context, query EdgeQuery) (EdgePage, error) {
	const operation = "query edges"
	limit, snapshot, err := store.querySnapshot(ctx, operation, query.SnapshotID, query.PageRequest)
	if err != nil {
		return EdgePage{}, err
	}
	resolution := query.Resolution
	if resolution != "" {
		resolution = rkcmodel.NormalizeResolution(resolution)
		if !rkcmodel.IsKnownResolution(resolution) {
			return EdgePage{}, invalidQuery(operation, "resolution", "unknown resolution")
		}
	}
	scope := scopeFingerprint(string(query.SnapshotID), query.Kind, query.From, query.To, resolution)
	payload, err := store.openCursor(operation, query.Cursor, "edges", scope)
	if err != nil {
		return EdgePage{}, err
	}
	values := make([]rkcmodel.Edge, 0, len(snapshot.bundle.Edges))
	for _, value := range snapshot.bundle.Edges {
		if (query.Kind == "" || value.Kind == query.Kind) && (query.From == "" || value.From == query.From) &&
			(query.To == "" || value.To == query.To) && (resolution == "" || value.Resolution == resolution) {
			values = append(values, value)
		}
	}
	return store.edgePage(operation, values, limit, payload, scope)
}

func (store *MemoryStore) QueryDiagnostics(ctx context.Context, query DiagnosticQuery) (DiagnosticPage, error) {
	const operation = "query diagnostics"
	limit, snapshot, err := store.querySnapshot(ctx, operation, query.SnapshotID, query.PageRequest)
	if err != nil {
		return DiagnosticPage{}, err
	}
	scope := scopeFingerprint(string(query.SnapshotID), query.Severity, query.Code, query.Stage)
	payload, err := store.openCursor(operation, query.Cursor, "diagnostics", scope)
	if err != nil {
		return DiagnosticPage{}, err
	}
	values := make([]rkcmodel.Diagnostic, 0, len(snapshot.bundle.Diagnostics))
	for _, value := range snapshot.bundle.Diagnostics {
		if (query.Severity == "" || value.Severity == query.Severity) &&
			(query.Code == "" || value.Code == query.Code) && (query.Stage == "" || value.Stage == query.Stage) {
			values = append(values, value)
		}
	}
	return store.diagnosticPage(operation, values, limit, payload, scope)
}

func (store *MemoryStore) Coverage(ctx context.Context, snapshotID SnapshotID) (rkcmodel.Coverage, error) {
	const operation = "read coverage"
	if err := validReadID(ctx, operation, "snapshot_id", string(snapshotID)); err != nil {
		return rkcmodel.Coverage{}, err
	}
	store.mu.RLock()
	snapshot, ok := store.snapshots[snapshotID]
	store.mu.RUnlock()
	if !ok {
		return rkcmodel.Coverage{}, storeError(CodeSnapshotNotFound, operation, "", snapshotID, "", nil)
	}
	return cloneJSON(snapshot.coverage)
}

func (store *MemoryStore) querySnapshot(ctx context.Context, operation string, snapshotID SnapshotID, request PageRequest) (int, *memorySnapshot, error) {
	if err := validReadID(ctx, operation, "snapshot_id", string(snapshotID)); err != nil {
		return 0, nil, err
	}
	limit, err := pageLimit(operation, request)
	if err != nil {
		return 0, nil, err
	}
	store.mu.RLock()
	snapshot, ok := store.snapshots[snapshotID]
	store.mu.RUnlock()
	if !ok {
		return 0, nil, storeError(CodeSnapshotNotFound, operation, "", snapshotID, "", nil)
	}
	return limit, snapshot, nil
}

func (store *MemoryStore) nodePage(operation string, values []rkcmodel.Node, limit int, payload cursorPayload, scope string) (NodePage, error) {
	start, err := idStart(operation, len(values), payload, func(index int) string { return values[index].ID })
	if err != nil {
		return NodePage{}, err
	}
	end := boundedEnd(start, limit, len(values))
	items, err := cloneJSON(values[start:end])
	page := NodePage{Items: items}
	if err == nil && end < len(values) {
		page.Next, err = store.sealCursor("nodes", scope, "", values[end-1].ID)
	}
	return page, err
}

func (store *MemoryStore) edgePage(operation string, values []rkcmodel.Edge, limit int, payload cursorPayload, scope string) (EdgePage, error) {
	start, err := idStart(operation, len(values), payload, func(index int) string { return values[index].ID })
	if err != nil {
		return EdgePage{}, err
	}
	end := boundedEnd(start, limit, len(values))
	items, err := cloneJSON(values[start:end])
	page := EdgePage{Items: items}
	if err == nil && end < len(values) {
		page.Next, err = store.sealCursor("edges", scope, "", values[end-1].ID)
	}
	return page, err
}

func (store *MemoryStore) diagnosticPage(operation string, values []rkcmodel.Diagnostic, limit int, payload cursorPayload, scope string) (DiagnosticPage, error) {
	start, err := idStart(operation, len(values), payload, func(index int) string { return values[index].ID })
	if err != nil {
		return DiagnosticPage{}, err
	}
	end := boundedEnd(start, limit, len(values))
	items, err := cloneJSON(values[start:end])
	page := DiagnosticPage{Items: items}
	if err == nil && end < len(values) {
		page.Next, err = store.sealCursor("diagnostics", scope, "", values[end-1].ID)
	}
	return page, err
}

func idStart(operation string, length int, payload cursorPayload, identifier func(int) string) (int, error) {
	if payload.ID == "" {
		return 0, nil
	}
	if payload.Primary != "" {
		return 0, invalidCursor(operation, "cursor sort key is invalid")
	}
	for index := 0; index < length; index++ {
		if identifier(index) == payload.ID {
			return index + 1, nil
		}
	}
	return 0, invalidCursor(operation, "cursor position no longer exists")
}

func snapshotStart(operation string, values []rkcmodel.Snapshot, payload cursorPayload) (int, error) {
	if payload.ID == "" {
		return 0, nil
	}
	for index, value := range values {
		if value.ID == payload.ID && snapshotSortTime(value) == payload.Primary {
			return index + 1, nil
		}
	}
	return 0, invalidCursor(operation, "cursor position no longer exists")
}

func snapshotSortTime(snapshot rkcmodel.Snapshot) string {
	return snapshot.CreatedAt.UTC().Format(time.RFC3339Nano)
}

func boundedEnd(start, limit, length int) int {
	end := start + limit
	if end > length {
		return length
	}
	return end
}

func validReadID(ctx context.Context, operation, field, value string) error {
	if err := checkContext(ctx, operation); err != nil {
		return err
	}
	return validIdentifier(operation, field, value)
}

func validLookup(ctx context.Context, operation string, snapshotID SnapshotID, field, value string) error {
	if err := validReadID(ctx, operation, "snapshot_id", string(snapshotID)); err != nil {
		return err
	}
	return validIdentifier(operation, field, value)
}
