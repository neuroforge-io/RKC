package history

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

var (
	testHistoryHead       = strings.Repeat("a", 40)
	testHistoryOtherHead  = strings.Repeat("b", 40)
	testHistoryRepository = "fixture"
)

func validHistoryFixture() History {
	repositoryID := rkcmodel.StableID("repository", testHistoryRepository)
	return History{
		SchemaVersion:   SchemaVersion,
		Repository:      testHistoryRepository,
		RepositoryID:    repositoryID,
		SourceRevision:  testHistoryHead,
		RevisionPolicy:  RevisionPolicyExactHead,
		AncestryPolicy:  AncestryPolicyFirstParent,
		SourceID:        historySourceID(repositoryID, testHistoryHead),
		Commit:          testHistoryHead,
		CommitLimit:     2,
		WindowTruncated: false,
		Commits: []CommitRecord{{
			ID: testHistoryHead, Date: "2026-08-31T00:00:00Z", Subject: "fixture",
			ChangedFiles: []string{}, AddedSymbols: []string{},
			RemovedSymbols: []string{}, ChangedSymbols: []string{},
		}},
		Symbols:   []SymbolHistory{},
		Refactors: []Refactor{},
	}
}

func validHistoryBundle() *rkcmodel.Bundle {
	return &rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{
		RepositoryID: rkcmodel.StableID("repository", testHistoryRepository),
		RootName:     testHistoryRepository,
		Git:          rkcmodel.GitInfo{Commit: testHistoryHead},
	}}
}

func bindHistoryToOrigin(compiled *History, bundle *rkcmodel.Bundle, origin string) {
	repositoryID := rkcmodel.StableID("repository", origin)
	compiled.SourceReference = origin
	compiled.RepositoryID = repositoryID
	compiled.SourceID = historySourceID(repositoryID, compiled.SourceRevision)
	bundle.Snapshot.RepositoryID = repositoryID
	bundle.Snapshot.Git.Origin = origin
	bundle.Snapshot.Metadata = map[string]string{"source_reference": origin}
}

func TestImportUsesObservedNamesAndRefactorLanguage(t *testing.T) {
	oldHistoryID := rkcmodel.StableID("history-symbol", "typescript", "function", "src.Old")
	newHistoryID := rkcmodel.StableID("history-symbol", "typescript", "function", "src.New")
	compiled := validHistoryFixture()
	compiled.Commits[0].ChangedFiles = []string{"src/value.ts"}
	compiled.Commits[0].AddedSymbols = []string{newHistoryID}
	compiled.Commits[0].RemovedSymbols = []string{oldHistoryID}
	compiled.Symbols = []SymbolHistory{
		{
			ID: oldHistoryID, Kind: "function", Name: "Old", QualifiedName: "src.Old",
			Language: "typescript", FirstObserved: testHistoryHead, LastObserved: testHistoryHead,
			Files: []string{"src/value.ts"}, CommitsTouching: []string{testHistoryHead},
		},
		{
			ID: newHistoryID, Kind: "function", Name: "New", QualifiedName: "src.New",
			Language: "typescript", FirstObserved: testHistoryHead, LastObserved: testHistoryHead,
			Files: []string{"src/value.ts"}, CommitsTouching: []string{testHistoryHead},
		},
	}
	compiled.Refactors = []Refactor{{
		Commit: testHistoryHead, Language: "typescript", Kind: "function",
		From: "src.Old", To: "src.New", QualifiedFrom: "src.Old", QualifiedTo: "src.New",
	}}
	bundle := validHistoryBundle()
	bundle.Nodes = []rkcmodel.Node{
		{ID: "old", Kind: "function", QualifiedName: "src.Old", Language: "typescript"},
		{ID: "new", Kind: "function", QualifiedName: "src.New", Language: "typescript"},
	}
	stats, err := Import(context.Background(), bundle, compiled)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SymbolsTouched != 2 || stats.SupersedesEdges != 1 || stats.EvidenceRecords != 3 {
		t.Fatalf("import stats = %+v", stats)
	}
	attributes := bundle.Nodes[1].Attributes
	if attributes["history_first_observed_commit"] != testHistoryHead ||
		attributes["history_last_observed_commit"] != testHistoryHead ||
		attributes["history_source_id"] != compiled.SourceID ||
		attributes["history_repository_id"] != compiled.RepositoryID ||
		attributes["first_seen_commit"] != nil {
		t.Fatalf("observed lifecycle attributes = %+v", attributes)
	}
	if bundle.Edges[0].Attributes["language"] != "typescript" ||
		bundle.Edges[0].Attributes["history_source_id"] != compiled.SourceID {
		t.Fatalf("refactor edge = %+v", bundle.Edges[0])
	}
	if bundle.Edges[0].Resolution != "syntax_inferred" || len(bundle.Edges[0].EvidenceIDs) != 1 ||
		len(bundle.Evidence) != 3 || len(bundle.Nodes[1].EvidenceIDs) != 1 {
		t.Fatalf("history provenance is incomplete: edge=%+v evidence=%+v", bundle.Edges[0], bundle.Evidence)
	}
	for _, evidence := range bundle.Evidence {
		if evidence.Tool != PluginID || evidence.ToolVersion != PluginVersion ||
			evidence.InputDigest != compiled.SourceID {
			t.Fatalf("history evidence is not source-bound: %+v", evidence)
		}
	}
}

func TestImportAcceptsMatchingCanonicalOrigin(t *testing.T) {
	compiled := validHistoryFixture()
	bundle := validHistoryBundle()
	bindHistoryToOrigin(&compiled, bundle, "https://example.test/NeuroforgeIO/RKC.git")
	if _, err := Import(context.Background(), bundle, compiled); err != nil {
		t.Fatalf("matching origin-backed history rejected: %v", err)
	}
}

func TestImportRejectsForeignOrUnprovenHistoryBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*History, *rkcmodel.Bundle)
	}{
		{
			name: "foreign repository identity",
			mutate: func(_ *History, bundle *rkcmodel.Bundle) {
				bundle.Snapshot.RepositoryID = rkcmodel.StableID("repository", "foreign")
			},
		},
		{
			name: "ancestor only is not exact head",
			mutate: func(_ *History, bundle *rkcmodel.Bundle) {
				bundle.Snapshot.Git.Commit = testHistoryOtherHead
			},
		},
		{
			name:   "dirty target",
			mutate: func(_ *History, bundle *rkcmodel.Bundle) { bundle.Snapshot.Git.Dirty = true },
		},
		{
			name:   "Git unavailable",
			mutate: func(_ *History, bundle *rkcmodel.Bundle) { bundle.Snapshot.Git.Unavailable = true },
		},
		{
			name: "unexpected target origin",
			mutate: func(_ *History, bundle *rkcmodel.Bundle) {
				bundle.Snapshot.Git.Origin = "https://example.test/foreign/repo.git"
			},
		},
		{
			name: "unexpected target source metadata",
			mutate: func(_ *History, bundle *rkcmodel.Bundle) {
				bundle.Snapshot.Metadata = map[string]string{
					"source_reference": "https://example.test/foreign/repo.git",
				}
			},
		},
		{
			name: "origin-backed history against originless target",
			mutate: func(compiled *History, bundle *rkcmodel.Bundle) {
				origin := "https://example.test/NeuroforgeIO/RKC.git"
				compiled.SourceReference = origin
				compiled.RepositoryID = rkcmodel.StableID("repository", origin)
				compiled.SourceID = historySourceID(compiled.RepositoryID, compiled.SourceRevision)
				bundle.Snapshot.RepositoryID = compiled.RepositoryID
			},
		},
		{
			name: "foreign origin with same revision",
			mutate: func(compiled *History, bundle *rkcmodel.Bundle) {
				bindHistoryToOrigin(compiled, bundle, "https://example.test/NeuroforgeIO/RKC.git")
				bundle.Snapshot.Git.Origin = "https://example.test/foreign/RKC.git"
			},
		},
		{
			name: "unsupported revision policy",
			mutate: func(compiled *History, _ *rkcmodel.Bundle) {
				compiled.RevisionPolicy = "ancestor"
			},
		},
		{
			name: "unsupported ancestry policy",
			mutate: func(compiled *History, _ *rkcmodel.Bundle) {
				compiled.AncestryPolicy = "all_parents"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled := validHistoryFixture()
			bundle := validHistoryBundle()
			test.mutate(&compiled, bundle)
			before, err := json.Marshal(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Import(context.Background(), bundle, compiled); err == nil {
				t.Fatal("foreign or unproven history was accepted")
			}
			after, err := json.Marshal(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("rejected import mutated bundle:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestImportRejectsRefactorWithoutLanguage(t *testing.T) {
	compiled := validHistoryFixture()
	compiled.Refactors = []Refactor{{
		Commit: testHistoryHead, Kind: "function", From: "a", To: "b",
		QualifiedFrom: "a", QualifiedTo: "b",
	}}
	if _, err := Import(context.Background(), validHistoryBundle(), compiled); err == nil {
		t.Fatal("language-free refactor was accepted")
	}
}
