package rkcstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func conformanceBundle(id string, repository RepositoryID, parent SnapshotID, created time.Time) rkcmodel.Bundle {
	artifact := rkcmodel.Artifact{
		ID: "artifact", Path: "main.go", Kind: "file", Language: "go", Status: "parsed", Text: true,
		Attributes: map[string]string{"owner": "fixture"},
	}
	evidence := rkcmodel.Evidence{
		ID: "evidence", Kind: "declared", Method: "conformance", Confidence: 1,
		Source:     &rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path, StartLine: 1, EndLine: 1},
		Attributes: map[string]any{"nested": map[string]any{"stable": true}},
	}
	nodes := []rkcmodel.Node{
		{ID: "node-a", Kind: "function", Name: "Alpha", Language: "go", Visibility: "public", ArtifactID: artifact.ID,
			Source:      &rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path, StartLine: 1, EndLine: 1},
			EvidenceIDs: []string{evidence.ID}, Attributes: map[string]any{"rank": 1}},
		{ID: "node-b", Kind: "function", Name: "Beta", Language: "go", Visibility: "private", ArtifactID: artifact.ID,
			EvidenceIDs: []string{evidence.ID}},
	}
	edge := rkcmodel.Edge{ID: "edge", Kind: "calls", From: nodes[0].ID, To: nodes[1].ID,
		Resolution: "declared", Confidence: 1, EvidenceIDs: []string{evidence.ID}}
	secondaryEdge := rkcmodel.Edge{ID: "edge-secondary", Kind: "related_to", From: nodes[1].ID, To: nodes[0].ID,
		Resolution: "unresolved", Confidence: 0, EvidenceIDs: []string{evidence.ID}}
	claim := rkcmodel.Claim{ID: "claim", SubjectID: nodes[0].ID, Text: "Alpha calls Beta.", Certainty: "supported",
		Generator: "conformance", EvidenceIDs: []string{evidence.ID}, Validation: "accepted"}
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			SchemaVersion: rkcmodel.SchemaVersion, ID: id, RepositoryID: string(repository), ParentSnapshotID: string(parent),
			CreatedAt: created, Status: "committed", RootName: string(repository), RootPath: "/private/root",
			ContentDigest: strings.Repeat("a", 64), Tool: rkcmodel.ToolInfo{Name: "rkc-test", Version: "1"},
			Metadata: map[string]string{"fixture": id},
		},
		Artifacts: []rkcmodel.Artifact{artifact}, Nodes: nodes, Edges: []rkcmodel.Edge{edge, secondaryEdge}, Evidence: []rkcmodel.Evidence{evidence},
		Diagnostics: []rkcmodel.Diagnostic{{ID: "diagnostic", Severity: "warning", Code: "TEST-001", Message: "fixture",
			Stage: "test", Source: &rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path}},
			{ID: "diagnostic-note", Severity: "note", Code: "TEST-002", Message: "fixture note", Stage: "inventory"}},
		Conflicts: []rkcmodel.Conflict{{ID: "conflict", Kind: "test", SubjectID: nodes[0].ID,
			CandidateIDs: []string{nodes[0].ID, nodes[1].ID}, PreferredID: nodes[0].ID, EvidenceIDs: []string{evidence.ID}}},
		Documents: []rkcmodel.Document{{ID: "document", Kind: "reference", Title: "Fixture", Generator: "conformance", Status: "validated",
			SubjectIDs: []string{nodes[0].ID}, Sections: []rkcmodel.DocumentSection{{ID: "section", Ordinal: 0, Markdown: "Fixture",
				ClaimIDs: []string{claim.ID}, EvidenceIDs: []string{evidence.ID}}}}},
		Claims: []rkcmodel.Claim{claim},
		Paths: []rkcmodel.ExecutionPath{{ID: "path", Name: "fixture", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[1].ID,
			NodeIDs: []string{nodes[0].ID, nodes[1].ID}, EdgeIDs: []string{edge.ID}, EvidenceIDs: []string{evidence.ID}}},
	}
}

func newConformanceStore(t *testing.T) *MemoryStore {
	t.Helper()
	store, err := NewMemoryStore()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func beginAndStage(t *testing.T, store Store, bundle rkcmodel.Bundle, includeCoverage bool) BuildID {
	t.Helper()
	ctx := context.Background()
	build, err := store.BeginBuild(ctx, BuildOptions{
		RepositoryID: RepositoryID(bundle.Snapshot.RepositoryID), ParentSnapshotID: SnapshotID(bundle.Snapshot.ParentSnapshotID),
		ExpectedSchema: bundle.Snapshot.SchemaVersion, Metadata: map[string]string{"build": bundle.Snapshot.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := []error{
		store.PutArtifacts(ctx, build, bundle.Artifacts), store.PutEvidence(ctx, build, bundle.Evidence),
		store.PutNodes(ctx, build, bundle.Nodes), store.PutEdges(ctx, build, bundle.Edges),
		store.PutDiagnostics(ctx, build, bundle.Diagnostics), store.PutClaims(ctx, build, bundle.Claims),
		store.PutConflicts(ctx, build, bundle.Conflicts), store.PutDocuments(ctx, build, bundle.Documents),
		store.PutPaths(ctx, build, bundle.Paths),
	}
	for _, err := range writes {
		if err != nil {
			t.Fatal(err)
		}
	}
	if includeCoverage {
		if err := store.PutCoverage(ctx, build, rkcmodel.BuildCoverage(bundle)); err != nil {
			t.Fatal(err)
		}
	}
	return build
}

func commitBundle(t *testing.T, store Store, bundle rkcmodel.Bundle) BuildID {
	t.Helper()
	build := beginAndStage(t, store, bundle, true)
	if err := store.Commit(context.Background(), build, bundle.Snapshot); err != nil {
		t.Fatal(err)
	}
	return build
}

func TestMemoryStoreTransactionalConformance(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	repository := RepositoryID("repo")
	bundle := conformanceBundle("snapshot-1", repository, "", time.Unix(100, 0).UTC())
	build := beginAndStage(t, store, bundle, true)

	if _, err := store.Snapshot(ctx, SnapshotID(bundle.Snapshot.ID)); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("uncommitted Snapshot error = %v", err)
	}
	if _, err := store.Current(ctx, repository); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("uncommitted Current error = %v", err)
	}
	page, err := store.ListSnapshots(ctx, SnapshotQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("uncommitted list = %+v, %v", page, err)
	}
	result, err := store.Validate(ctx, build)
	if err != nil || !result.Valid() || result.Report.HasErrors() {
		t.Fatalf("Validate = %+v, %v", result, err)
	}

	// Mutating the caller's values after staging must not alter stored state.
	bundle.Artifacts[0].Attributes["owner"] = "mutated"
	bundle.Nodes[0].EvidenceIDs[0] = "mutated"
	bundle.Snapshot.Metadata["fixture"] = "mutated"
	commitSnapshot := conformanceBundle("snapshot-1", repository, "", time.Unix(100, 0).UTC()).Snapshot
	if err := store.Commit(ctx, build, commitSnapshot); err != nil {
		t.Fatal(err)
	}

	current, err := store.Current(ctx, repository)
	if err != nil || current.ID != "snapshot-1" || current.RootPath != "/private/root" {
		t.Fatalf("Current = %+v, %v", current, err)
	}
	loaded, err := store.Snapshot(ctx, "snapshot-1")
	if err != nil || !reflect.DeepEqual(loaded, current) {
		t.Fatalf("Snapshot = %+v, %v", loaded, err)
	}
	exported, err := store.Bundle(ctx, "snapshot-1")
	if err != nil || exported.Snapshot.ID != "snapshot-1" || len(exported.Artifacts) != 1 || len(exported.Nodes) != 2 {
		t.Fatalf("Bundle = %+v, %v", exported, err)
	}
	artifact, err := store.Artifact(ctx, "snapshot-1", "artifact")
	if err != nil || artifact.Attributes["owner"] != "fixture" {
		t.Fatalf("Artifact = %+v, %v", artifact, err)
	}
	node, err := store.Node(ctx, "snapshot-1", "node-a")
	if err != nil || node.EvidenceIDs[0] != "evidence" {
		t.Fatalf("Node = %+v, %v", node, err)
	}
	evidence, err := store.Evidence(ctx, "snapshot-1", "evidence")
	if err != nil || evidence.Method != "conformance" {
		t.Fatalf("Evidence = %+v, %v", evidence, err)
	}
	coverage, err := store.Coverage(ctx, "snapshot-1")
	if err != nil || coverage.SnapshotID != "snapshot-1" || coverage.NodesTotal != 2 {
		t.Fatalf("Coverage = %+v, %v", coverage, err)
	}

	// Every read is a defensive clone, including nested maps and slices.
	current.Metadata["fixture"] = "reader mutation"
	exported.Nodes[0].Name = "reader mutation"
	artifact.Attributes["owner"] = "reader mutation"
	node.Attributes["rank"] = 99
	evidence.Attributes["nested"].(map[string]any)["stable"] = false
	coverage.NodeKinds["function"] = 99
	again, _ := store.Snapshot(ctx, "snapshot-1")
	againBundle, _ := store.Bundle(ctx, "snapshot-1")
	againArtifact, _ := store.Artifact(ctx, "snapshot-1", "artifact")
	againNode, _ := store.Node(ctx, "snapshot-1", "node-a")
	againEvidence, _ := store.Evidence(ctx, "snapshot-1", "evidence")
	againCoverage, _ := store.Coverage(ctx, "snapshot-1")
	if again.Metadata["fixture"] != "snapshot-1" || againBundle.Nodes[0].Name != "Alpha" || againArtifact.Attributes["owner"] != "fixture" ||
		againNode.Attributes["rank"] != float64(1) || !againEvidence.Attributes["nested"].(map[string]any)["stable"].(bool) ||
		againCoverage.NodeKinds["function"] != 2 {
		t.Fatal("reader values alias immutable storage")
	}

	if err := store.Commit(ctx, build, commitSnapshot); !errors.Is(err, ErrBuildCommitted) {
		t.Fatalf("second commit = %v", err)
	}
	if err := store.PutNodes(ctx, build, nil); !errors.Is(err, ErrBuildCommitted) {
		t.Fatalf("write committed build = %v", err)
	}
	if err := store.Abort(ctx, build, nil); !errors.Is(err, ErrBuildCommitted) {
		t.Fatalf("abort committed build = %v", err)
	}
}

func TestMemoryStorePaginationAndQueriesConformance(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	repo := RepositoryID("repo")
	first := conformanceBundle("s1", repo, "", time.Unix(100, 0).UTC())
	commitBundle(t, store, first)
	second := conformanceBundle("s2", repo, "s1", time.Unix(200, 0).UTC())
	commitBundle(t, store, second)

	page1, err := store.ListSnapshots(ctx, SnapshotQuery{RepositoryID: repo, PageRequest: PageRequest{Limit: 1}})
	if err != nil || len(page1.Items) != 1 || page1.Items[0].ID != "s2" || page1.Next == "" {
		t.Fatalf("snapshot page 1 = %+v, %v", page1, err)
	}
	page2, err := store.ListSnapshots(ctx, SnapshotQuery{RepositoryID: repo, PageRequest: PageRequest{Limit: 1, Cursor: page1.Next}})
	if err != nil || len(page2.Items) != 1 || page2.Items[0].ID != "s1" || page2.Next != "" {
		t.Fatalf("snapshot page 2 = %+v, %v", page2, err)
	}
	if _, err := store.ListSnapshots(ctx, SnapshotQuery{RepositoryID: "other", PageRequest: PageRequest{Cursor: page1.Next}}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cross-query cursor = %v", err)
	}
	cursorParts := strings.Split(string(page1.Next), ".")
	replacement := byte('A')
	if cursorParts[1][0] == replacement {
		replacement = 'B'
	}
	cursorParts[1] = string(replacement) + cursorParts[1][1:]
	tampered := Cursor(strings.Join(cursorParts, "."))
	if _, err := store.ListSnapshots(ctx, SnapshotQuery{RepositoryID: repo, PageRequest: PageRequest{Cursor: tampered}}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("tampered cursor = %v", err)
	}
	for _, request := range []PageRequest{{Limit: -1}, {Limit: MaxPageSize + 1}, {Cursor: Cursor(strings.Repeat("x", maxCursorLen+1))}} {
		if _, err := store.ListSnapshots(ctx, SnapshotQuery{PageRequest: request}); err == nil {
			t.Fatalf("invalid page request %+v succeeded", request)
		}
	}

	nodes1, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "s2", PageRequest: PageRequest{Limit: 1}})
	if err != nil || len(nodes1.Items) != 1 || nodes1.Items[0].ID != "node-a" || nodes1.Next == "" {
		t.Fatalf("node page 1 = %+v, %v", nodes1, err)
	}
	nodes2, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "s2", PageRequest: PageRequest{Limit: 1, Cursor: nodes1.Next}})
	if err != nil || len(nodes2.Items) != 1 || nodes2.Items[0].ID != "node-b" || nodes2.Next != "" {
		t.Fatalf("node page 2 = %+v, %v", nodes2, err)
	}
	filtered, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "s2", Kind: "function", Language: "go", ArtifactID: "artifact", Visibility: "public"})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].ID != "node-a" {
		t.Fatalf("filtered nodes = %+v, %v", filtered, err)
	}
	if _, err := store.QueryNodes(ctx, NodeQuery{SnapshotID: "s2", Kind: "method", PageRequest: PageRequest{Cursor: nodes1.Next}}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("node cursor scope = %v", err)
	}
	edges, err := store.QueryEdges(ctx, EdgeQuery{SnapshotID: "s2", Kind: "calls", From: "node-a", To: "node-b", Resolution: "declared"})
	if err != nil || len(edges.Items) != 1 || edges.Items[0].ID != "edge" {
		t.Fatalf("filtered edges = %+v, %v", edges, err)
	}
	if _, err := store.QueryEdges(ctx, EdgeQuery{SnapshotID: "s2", Resolution: "fictional"}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("invalid resolution = %v", err)
	}
	diagnostics, err := store.QueryDiagnostics(ctx, DiagnosticQuery{SnapshotID: "s2", Severity: "warning", Code: "TEST-001", Stage: "test"})
	if err != nil || len(diagnostics.Items) != 1 || diagnostics.Items[0].ID != "diagnostic" {
		t.Fatalf("filtered diagnostics = %+v, %v", diagnostics, err)
	}
	edgePage, err := store.QueryEdges(ctx, EdgeQuery{SnapshotID: "s2", PageRequest: PageRequest{Limit: 1}})
	if err != nil || len(edgePage.Items) != 1 || edgePage.Next == "" {
		t.Fatalf("edge page = %+v, %v", edgePage, err)
	}
	edgeLast, err := store.QueryEdges(ctx, EdgeQuery{SnapshotID: "s2", PageRequest: PageRequest{Limit: 1, Cursor: edgePage.Next}})
	if err != nil || len(edgeLast.Items) != 1 || edgeLast.Next != "" {
		t.Fatalf("edge last page = %+v, %v", edgeLast, err)
	}
	diagnosticPage, err := store.QueryDiagnostics(ctx, DiagnosticQuery{SnapshotID: "s2", PageRequest: PageRequest{Limit: 1}})
	if err != nil || len(diagnosticPage.Items) != 1 || diagnosticPage.Next == "" {
		t.Fatalf("diagnostic page = %+v, %v", diagnosticPage, err)
	}
	diagnosticLast, err := store.QueryDiagnostics(ctx, DiagnosticQuery{SnapshotID: "s2", PageRequest: PageRequest{Limit: 1, Cursor: diagnosticPage.Next}})
	if err != nil || len(diagnosticLast.Items) != 1 || diagnosticLast.Next != "" {
		t.Fatalf("diagnostic last page = %+v, %v", diagnosticLast, err)
	}
	for _, call := range []func() error{
		func() error { _, err := store.Artifact(ctx, "s2", "missing"); return err },
		func() error { _, err := store.Node(ctx, "s2", "missing"); return err },
		func() error { _, err := store.Evidence(ctx, "s2", "missing"); return err },
	} {
		if err := call(); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("missing record = %v", err)
		}
	}
}

func TestMemoryStoreValidationAbortRecoveryAndErrors(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	repo := RepositoryID("repo")
	valid := conformanceBundle("valid", repo, "", time.Unix(1, 0).UTC())

	missingCoverage := beginAndStage(t, store, valid, false)
	result, err := store.Validate(ctx, missingCoverage)
	if err != nil || result.Valid() || result.CoveragePresent {
		t.Fatalf("missing coverage validation = %+v, %v", result, err)
	}
	if err := store.Commit(ctx, missingCoverage, valid.Snapshot); !errors.Is(err, ErrCoverageMismatch) {
		t.Fatalf("missing coverage commit = %v", err)
	}
	if err := store.Abort(ctx, missingCoverage, errors.New("cancel fixture")); err != nil {
		t.Fatal(err)
	}
	if err := store.Abort(ctx, missingCoverage, nil); err != nil {
		t.Fatalf("idempotent abort = %v", err)
	}
	if _, err := store.Validate(ctx, missingCoverage); !errors.Is(err, ErrBuildClosed) {
		t.Fatalf("validate aborted = %v", err)
	}

	invalid := conformanceBundle("invalid", repo, "", time.Unix(2, 0).UTC())
	invalid.Nodes[0].ArtifactID = "missing-artifact"
	invalidBuild := beginAndStage(t, store, invalid, true)
	result, err = store.Validate(ctx, invalidBuild)
	if err != nil || !result.Report.HasErrors() || result.Valid() {
		t.Fatalf("invalid validation = %+v, %v", result, err)
	}
	err = store.Commit(ctx, invalidBuild, invalid.Snapshot)
	var failure *ValidationFailure
	if !errors.Is(err, ErrValidation) || !errors.As(err, &failure) || !failure.Result.Report.HasErrors() {
		t.Fatalf("validation failure = %#v", err)
	}
	_ = store.Abort(ctx, invalidBuild, err)

	recoverA, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: repo})
	if err != nil {
		t.Fatal(err)
	}
	recoverB, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: repo})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Recover(ctx)
	if err != nil || !reflect.DeepEqual(recovered.AbortedBuilds, []BuildID{minBuild(recoverA, recoverB), maxBuild(recoverA, recoverB)}) {
		t.Fatalf("Recover = %+v, %v", recovered, err)
	}
	if _, err := store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Abort(ctx, "missing", nil); !errors.Is(err, ErrBuildNotFound) {
		t.Fatalf("abort missing = %v", err)
	}

	for _, opts := range []BuildOptions{{}, {RepositoryID: repo, ExpectedSchema: "future"}, {RepositoryID: repo, ParentSnapshotID: "missing"}} {
		if _, err := store.BeginBuild(ctx, opts); err == nil {
			t.Fatalf("BeginBuild(%+v) succeeded", opts)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.BeginBuild(canceled, BuildOptions{RepositoryID: repo}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled begin = %v", err)
	}
	if _, err := store.ListSnapshots(nil, SnapshotQuery{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil context = %v", err)
	}
}

func TestMemoryStoreAtomicBatchesCoverageAndConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	store := newConformanceStore(t)
	repo := RepositoryID("repo")
	base := conformanceBundle("base", repo, "", time.Unix(1, 0).UTC())
	commitBundle(t, store, base)

	one := conformanceBundle("one", repo, "base", time.Unix(2, 0).UTC())
	two := conformanceBundle("two", repo, "base", time.Unix(3, 0).UTC())
	buildOne := beginAndStage(t, store, one, true)
	buildTwo := beginAndStage(t, store, two, true)
	if err := store.Commit(ctx, buildOne, one.Snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(ctx, buildTwo, two.Snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale concurrent commit = %v", err)
	}
	current, _ := store.Current(ctx, repo)
	if current.ID != "one" {
		t.Fatalf("stale writer changed current to %q", current.ID)
	}
	if _, err := store.Snapshot(ctx, "two"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("stale writer published snapshot: %v", err)
	}
	_ = store.Abort(ctx, buildTwo, errors.New("stale"))

	build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: repo, ParentSnapshotID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutNodes(ctx, build, []rkcmodel.Node{{ID: "same"}, {ID: "same"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate batch = %v", err)
	}
	if err := store.PutNodes(ctx, build, []rkcmodel.Node{{ID: "same", Kind: "function", Name: "same"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutNodes(ctx, build, []rkcmodel.Node{{ID: "same"}, {ID: "other"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate existing = %v", err)
	}
	// The rejected batch is atomic: its otherwise-new record was not inserted.
	store.mu.RLock()
	_, leaked := store.builds[build].nodes["other"]
	store.mu.RUnlock()
	if leaked {
		t.Fatal("failed batch partially mutated the build")
	}
	if err := store.PutNodes(ctx, build, []rkcmodel.Node{{}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty record id = %v", err)
	}
	oversized := make([]rkcmodel.Node, MaxBatchSize+1)
	if err := store.PutNodes(ctx, build, oversized); !errors.Is(err, ErrResourceExhausted) {
		t.Fatalf("oversized batch = %v", err)
	}
	badJSON := rkcmodel.Node{ID: "json", Attributes: map[string]any{"bad": make(chan int)}}
	if err := store.PutNodes(ctx, build, []rkcmodel.Node{badJSON}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("noncanonical record = %v", err)
	}

	// Readers run concurrently without observing an uncommitted or torn state.
	var wait sync.WaitGroup
	readerErr := make(chan error, 1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		for i := 0; i < 200; i++ {
			seen, err := store.Current(ctx, repo)
			if err != nil || (seen.ID != "one" && seen.ID != "next") {
				readerErr <- errors.New("reader observed invalid current state")
				return
			}
		}
	}()
	next := conformanceBundle("next", repo, "one", time.Unix(4, 0).UTC())
	commitBundle(t, store, next)
	wait.Wait()
	select {
	case err := <-readerErr:
		t.Fatal(err)
	default:
	}
}

func minBuild(left, right BuildID) BuildID {
	if left < right {
		return left
	}
	return right
}

func maxBuild(left, right BuildID) BuildID {
	if left > right {
		return left
	}
	return right
}
