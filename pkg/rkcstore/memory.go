package rkcstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	MaxBatchSize      = 10_000
	MaxIdentifierSize = 1_024
)

type buildState uint8

const (
	buildOpen buildState = iota
	buildCommitted
	buildAborted
)

type memoryBuild struct {
	options     BuildOptions
	baseCurrent SnapshotID
	state       buildState
	abortReason string
	artifacts   map[string]rkcmodel.Artifact
	nodes       map[string]rkcmodel.Node
	edges       map[string]rkcmodel.Edge
	evidence    map[string]rkcmodel.Evidence
	diagnostics map[string]rkcmodel.Diagnostic
	conflicts   map[string]rkcmodel.Conflict
	documents   map[string]rkcmodel.Document
	claims      map[string]rkcmodel.Claim
	paths       map[string]rkcmodel.ExecutionPath
	coverage    *rkcmodel.Coverage
}

type memorySnapshot struct {
	bundle   rkcmodel.Bundle
	coverage rkcmodel.Coverage
}

// MemoryStore is a concurrency-safe reference backend. It deliberately uses
// the same transaction and cursor rules required of durable implementations,
// making it suitable for contract tests without weakening production semantics.
type MemoryStore struct {
	mu        sync.RWMutex
	secret    [32]byte
	builds    map[BuildID]*memoryBuild
	snapshots map[SnapshotID]*memorySnapshot
	current   map[RepositoryID]SnapshotID
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore creates an empty isolated backend. Cursor authentication keys
// are per-store, so a cursor cannot accidentally be replayed against another
// repository database.
func NewMemoryStore() (*MemoryStore, error) {
	store := &MemoryStore{
		builds: make(map[BuildID]*memoryBuild), snapshots: make(map[SnapshotID]*memorySnapshot),
		current: make(map[RepositoryID]SnapshotID),
	}
	if _, err := rand.Read(store.secret[:]); err != nil {
		return nil, fmt.Errorf("initialize memory-store cursor key: %w", err)
	}
	return store, nil
}

func (store *MemoryStore) BeginBuild(ctx context.Context, opts BuildOptions) (BuildID, error) {
	const operation = "begin build"
	if err := checkContext(ctx, operation); err != nil {
		return "", err
	}
	if err := validIdentifier(operation, "repository_id", string(opts.RepositoryID)); err != nil {
		return "", err
	}
	if opts.ParentSnapshotID != "" {
		if err := validIdentifier(operation, "parent_snapshot_id", string(opts.ParentSnapshotID)); err != nil {
			return "", err
		}
	}
	if opts.ExpectedSchema == "" {
		opts.ExpectedSchema = rkcmodel.SchemaVersion
	}
	if opts.ExpectedSchema != rkcmodel.SchemaVersion {
		return "", invalidArgument(operation, "expected_schema", "unsupported RKC schema version")
	}
	opts.Metadata = cloneStrings(opts.Metadata)

	store.mu.Lock()
	defer store.mu.Unlock()
	if err := checkContext(ctx, operation); err != nil {
		return "", err
	}
	base := store.current[opts.RepositoryID]
	if base != opts.ParentSnapshotID {
		return "", conflict(operation, "", opts.ParentSnapshotID,
			"parent snapshot %q is not the current snapshot %q", opts.ParentSnapshotID, base)
	}
	if opts.ParentSnapshotID != "" {
		parent, ok := store.snapshots[opts.ParentSnapshotID]
		if !ok {
			return "", storeError(CodeSnapshotNotFound, operation, "", opts.ParentSnapshotID, "parent_snapshot_id", nil)
		}
		if RepositoryID(parent.bundle.Snapshot.RepositoryID) != opts.RepositoryID {
			return "", conflict(operation, "", opts.ParentSnapshotID, "parent belongs to another repository")
		}
	}

	var id BuildID
	for {
		bytes := make([]byte, 16)
		if _, err := rand.Read(bytes); err != nil {
			return "", fmt.Errorf("generate build identifier: %w", err)
		}
		id = BuildID("build_" + hex.EncodeToString(bytes))
		if _, exists := store.builds[id]; !exists {
			break
		}
	}
	store.builds[id] = &memoryBuild{
		options: opts, baseCurrent: base, state: buildOpen,
		artifacts: make(map[string]rkcmodel.Artifact), nodes: make(map[string]rkcmodel.Node),
		edges: make(map[string]rkcmodel.Edge), evidence: make(map[string]rkcmodel.Evidence),
		diagnostics: make(map[string]rkcmodel.Diagnostic), conflicts: make(map[string]rkcmodel.Conflict),
		documents: make(map[string]rkcmodel.Document), claims: make(map[string]rkcmodel.Claim),
		paths: make(map[string]rkcmodel.ExecutionPath),
	}
	return id, nil
}

func (store *MemoryStore) PutArtifacts(ctx context.Context, build BuildID, values []rkcmodel.Artifact) error {
	return store.putArtifacts(ctx, build, values)
}

func (store *MemoryStore) putArtifacts(ctx context.Context, buildID BuildID, values []rkcmodel.Artifact) error {
	const operation = "put artifacts"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Artifact) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.artifacts, cloned, func(value rkcmodel.Artifact) string { return value.ID })
}

func (store *MemoryStore) PutNodes(ctx context.Context, buildID BuildID, values []rkcmodel.Node) error {
	const operation = "put nodes"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Node) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.nodes, cloned, func(value rkcmodel.Node) string { return value.ID })
}

func (store *MemoryStore) PutEdges(ctx context.Context, buildID BuildID, values []rkcmodel.Edge) error {
	const operation = "put edges"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Edge) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.edges, cloned, func(value rkcmodel.Edge) string { return value.ID })
}

func (store *MemoryStore) PutEvidence(ctx context.Context, buildID BuildID, values []rkcmodel.Evidence) error {
	const operation = "put evidence"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Evidence) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.evidence, cloned, func(value rkcmodel.Evidence) string { return value.ID })
}

func (store *MemoryStore) PutDiagnostics(ctx context.Context, buildID BuildID, values []rkcmodel.Diagnostic) error {
	const operation = "put diagnostics"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Diagnostic) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.diagnostics, cloned, func(value rkcmodel.Diagnostic) string { return value.ID })
}

func (store *MemoryStore) PutConflicts(ctx context.Context, buildID BuildID, values []rkcmodel.Conflict) error {
	const operation = "put conflicts"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Conflict) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.conflicts, cloned, func(value rkcmodel.Conflict) string { return value.ID })
}

func (store *MemoryStore) PutDocuments(ctx context.Context, buildID BuildID, values []rkcmodel.Document) error {
	const operation = "put documents"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Document) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.documents, cloned, func(value rkcmodel.Document) string { return value.ID })
}

func (store *MemoryStore) PutClaims(ctx context.Context, buildID BuildID, values []rkcmodel.Claim) error {
	const operation = "put claims"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.Claim) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.claims, cloned, func(value rkcmodel.Claim) string { return value.ID })
}

func (store *MemoryStore) PutPaths(ctx context.Context, buildID BuildID, values []rkcmodel.ExecutionPath) error {
	const operation = "put paths"
	cloned, err := prepareBatch(ctx, operation, values, func(value rkcmodel.ExecutionPath) string { return value.ID })
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	return mergeBatch(operation, buildID, build.paths, cloned, func(value rkcmodel.ExecutionPath) string { return value.ID })
}

func (store *MemoryStore) PutCoverage(ctx context.Context, buildID BuildID, coverage rkcmodel.Coverage) error {
	const operation = "put coverage"
	if err := checkContext(ctx, operation); err != nil {
		return err
	}
	cloned, err := cloneJSON(coverage)
	if err != nil {
		return invalidArgument(operation, "coverage", "coverage is not canonically serializable: "+err.Error())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	build.coverage = &cloned
	return nil
}

func (store *MemoryStore) Validate(ctx context.Context, buildID BuildID) (ValidationResult, error) {
	const operation = "validate build"
	store.mu.RLock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		store.mu.RUnlock()
		return ValidationResult{}, err
	}
	bundle := provisionalBundle(buildID, build)
	provided := cloneCoveragePointer(build.coverage)
	store.mu.RUnlock()
	if err := checkContext(ctx, operation); err != nil {
		return ValidationResult{}, err
	}
	return validateBundle(bundle, provided), nil
}

func (store *MemoryStore) Commit(ctx context.Context, buildID BuildID, snapshot rkcmodel.Snapshot) error {
	const operation = "commit build"
	if err := checkContext(ctx, operation); err != nil {
		return err
	}
	clonedSnapshot, err := cloneJSON(snapshot)
	if err != nil {
		return invalidArgument(operation, "snapshot", "snapshot is not canonically serializable: "+err.Error())
	}
	if err := validIdentifier(operation, "snapshot_id", clonedSnapshot.ID); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	build, err := store.openBuildLocked(ctx, operation, buildID)
	if err != nil {
		return err
	}
	snapshotID := SnapshotID(clonedSnapshot.ID)
	if RepositoryID(clonedSnapshot.RepositoryID) != build.options.RepositoryID {
		return invalidArgument(operation, "repository_id", "snapshot does not match build repository")
	}
	if SnapshotID(clonedSnapshot.ParentSnapshotID) != build.options.ParentSnapshotID {
		return invalidArgument(operation, "parent_snapshot_id", "snapshot does not match build parent")
	}
	if clonedSnapshot.SchemaVersion != build.options.ExpectedSchema {
		return invalidArgument(operation, "schema_version", "snapshot does not match expected schema")
	}
	if clonedSnapshot.Status != "committed" {
		return invalidArgument(operation, "status", "committed snapshot must have status committed")
	}
	if store.current[build.options.RepositoryID] != build.baseCurrent {
		return conflict(operation, buildID, snapshotID, "repository current snapshot changed during build")
	}
	if _, exists := store.snapshots[snapshotID]; exists {
		return conflict(operation, buildID, snapshotID, "snapshot identifier already exists")
	}

	bundle := bundleFromBuild(clonedSnapshot, build)
	result := validateBundle(bundle, cloneCoveragePointer(build.coverage))
	if result.Report.HasErrors() {
		return &ValidationFailure{Operation: operation, BuildID: buildID, Result: result}
	}
	result.CoverageConsistent = build.coverage != nil && reflect.DeepEqual(*build.coverage, result.ExpectedCoverage)
	if !result.CoveragePresent || !result.CoverageConsistent {
		return storeError(CodeCoverageMismatch, operation, buildID, snapshotID, "coverage", nil)
	}
	canonical := canonicalStoredBundle(bundle)
	coverage, cloneErr := cloneJSON(result.ExpectedCoverage)
	if cloneErr != nil {
		return fmt.Errorf("clone validated coverage: %w", cloneErr)
	}
	store.snapshots[snapshotID] = &memorySnapshot{bundle: canonical, coverage: coverage}
	store.current[build.options.RepositoryID] = snapshotID
	build.state = buildCommitted
	build.clearPayload()
	return nil
}

func (store *MemoryStore) Abort(ctx context.Context, buildID BuildID, reason error) error {
	const operation = "abort build"
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := checkContext(ctx, operation); err != nil {
		return err
	}
	build, ok := store.builds[buildID]
	if !ok {
		return storeError(CodeBuildNotFound, operation, buildID, "", "", nil)
	}
	switch build.state {
	case buildCommitted:
		return storeError(CodeBuildCommitted, operation, buildID, "", "", nil)
	case buildAborted:
		return nil
	}
	build.state = buildAborted
	if reason != nil {
		build.abortReason = reason.Error()
	}
	build.clearPayload()
	return nil
}

func (store *MemoryStore) Recover(ctx context.Context) (RecoveryResult, error) {
	const operation = "recover builds"
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := checkContext(ctx, operation); err != nil {
		return RecoveryResult{}, err
	}
	result := RecoveryResult{AbortedBuilds: make([]BuildID, 0)}
	for id, build := range store.builds {
		if build.state != buildOpen {
			continue
		}
		build.state = buildAborted
		build.abortReason = "recovered incomplete build"
		build.clearPayload()
		result.AbortedBuilds = append(result.AbortedBuilds, id)
	}
	sort.Slice(result.AbortedBuilds, func(i, j int) bool { return result.AbortedBuilds[i] < result.AbortedBuilds[j] })
	return result, nil
}

func (store *MemoryStore) openBuildLocked(ctx context.Context, operation string, id BuildID) (*memoryBuild, error) {
	if err := checkContext(ctx, operation); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, invalidArgument(operation, "build_id", "build identifier is required")
	}
	build, ok := store.builds[id]
	if !ok {
		return nil, storeError(CodeBuildNotFound, operation, id, "", "", nil)
	}
	switch build.state {
	case buildCommitted:
		return nil, storeError(CodeBuildCommitted, operation, id, "", "", nil)
	case buildAborted:
		return nil, storeError(CodeBuildClosed, operation, id, "", "", nil)
	default:
		return build, nil
	}
}

func (build *memoryBuild) clearPayload() {
	build.artifacts = nil
	build.nodes = nil
	build.edges = nil
	build.evidence = nil
	build.diagnostics = nil
	build.conflicts = nil
	build.documents = nil
	build.claims = nil
	build.paths = nil
	build.coverage = nil
}

func bundleFromBuild(snapshot rkcmodel.Snapshot, build *memoryBuild) rkcmodel.Bundle {
	bundle := rkcmodel.Bundle{Snapshot: snapshot}
	for _, value := range build.artifacts {
		bundle.Artifacts = append(bundle.Artifacts, value)
	}
	for _, value := range build.nodes {
		bundle.Nodes = append(bundle.Nodes, value)
	}
	for _, value := range build.edges {
		bundle.Edges = append(bundle.Edges, value)
	}
	for _, value := range build.evidence {
		bundle.Evidence = append(bundle.Evidence, value)
	}
	for _, value := range build.diagnostics {
		bundle.Diagnostics = append(bundle.Diagnostics, value)
	}
	for _, value := range build.conflicts {
		bundle.Conflicts = append(bundle.Conflicts, value)
	}
	for _, value := range build.documents {
		bundle.Documents = append(bundle.Documents, value)
	}
	for _, value := range build.claims {
		bundle.Claims = append(bundle.Claims, value)
	}
	for _, value := range build.paths {
		bundle.Paths = append(bundle.Paths, value)
	}
	rkcmodel.SortBundle(&bundle)
	return bundle
}

func provisionalBundle(id BuildID, build *memoryBuild) rkcmodel.Bundle {
	snapshot := rkcmodel.Snapshot{
		SchemaVersion: build.options.ExpectedSchema,
		ID:            string(id), RepositoryID: string(build.options.RepositoryID),
		ParentSnapshotID: string(build.options.ParentSnapshotID), Status: "validating",
		RootName: string(build.options.RepositoryID), ContentDigest: strings.Repeat("0", 64),
		Tool: rkcmodel.ToolInfo{Name: "rkcstore-memory", Version: "1"},
	}
	return bundleFromBuild(snapshot, build)
}

func validateBundle(bundle rkcmodel.Bundle, provided *rkcmodel.Coverage) ValidationResult {
	report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true})
	expected := rkcmodel.BuildCoverage(bundle)
	result := ValidationResult{Report: report, ExpectedCoverage: expected, CoveragePresent: provided != nil}
	if provided != nil {
		left, right := *provided, expected
		// Validate does not yet know the snapshot accepted by Commit. Every
		// canonical count and ratio is checked here; identity binds exactly at
		// Commit once the real snapshot is available.
		left.SnapshotID, right.SnapshotID = "", ""
		left.DeterministicOutputDigest, right.DeterministicOutputDigest = "", ""
		result.CoverageConsistent = reflect.DeepEqual(left, right)
	}
	return result
}

func canonicalStoredBundle(bundle rkcmodel.Bundle) rkcmodel.Bundle {
	// Input records were JSON-cloned on ingestion; sorting here establishes the
	// same deterministic collection order used by canonical export without
	// discarding operational Snapshot fields.
	rkcmodel.SortBundle(&bundle)
	return bundle
}

func cloneCoveragePointer(value *rkcmodel.Coverage) *rkcmodel.Coverage {
	if value == nil {
		return nil
	}
	cloned, err := cloneJSON(*value)
	if err != nil {
		return nil
	}
	return &cloned
}

func prepareBatch[T any](ctx context.Context, operation string, values []T, identifier func(T) string) ([]T, error) {
	if err := checkContext(ctx, operation); err != nil {
		return nil, err
	}
	if len(values) > MaxBatchSize {
		return nil, invalidArgument(operation, "values", "batch exceeds MaxBatchSize")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := identifier(value)
		if err := validIdentifier(operation, "record_id", id); err != nil {
			return nil, err
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, conflict(operation, "", "", "duplicate record identifier %q in batch", id)
		}
		seen[id] = struct{}{}
	}
	cloned, err := cloneJSON(values)
	if err != nil {
		return nil, invalidArgument(operation, "values", "batch is not canonically serializable: "+err.Error())
	}
	return cloned, checkContext(ctx, operation)
}

func mergeBatch[T any](operation string, buildID BuildID, destination map[string]T, values []T, identifier func(T) string) error {
	for _, value := range values {
		id := identifier(value)
		if _, exists := destination[id]; exists {
			return conflict(operation, buildID, "", "record identifier %q already exists", id)
		}
	}
	for _, value := range values {
		destination[identifier(value)] = value
	}
	return nil
}

func validIdentifier(operation, field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalidArgument(operation, field, "identifier is required")
	}
	if len(value) > MaxIdentifierSize {
		return invalidArgument(operation, field, "identifier exceeds MaxIdentifierSize")
	}
	return nil
}

func checkContext(ctx context.Context, operation string) error {
	if ctx == nil {
		return invalidArgument(operation, "context", "context is required")
	}
	if err := ctx.Err(); err != nil {
		return storeError(CodeCanceled, operation, "", "", "", err)
	}
	return nil
}

func cloneJSON[T any](value T) (T, error) {
	var cloned T
	data, err := json.Marshal(value)
	if err != nil {
		return cloned, err
	}
	if err := json.Unmarshal(data, &cloned); err != nil {
		return cloned, err
	}
	return cloned, nil
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func recordNotFound(operation string, snapshot SnapshotID, family, id string) error {
	return storeError(CodeRecordNotFound, operation, "", snapshot, family, errors.New(id))
}
