package search

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestBuildFromBundleIndexesCanonicalObjectTypesAndSelectedText(t *testing.T) {
	t.Parallel()

	bundle := rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{
			{
				ID: "node", Kind: "function", Language: "go", Name: "CompileGraph", QualifiedName: "pkg.CompileGraph", Signature: "func CompileGraph()",
				Source:     &rkcmodel.SourceRange{Path: "pkg/compile.go"},
				Attributes: map[string]any{"docstring": "Compile a graph.", "summary": "Fast summary.", "description": "Detailed description.", "purpose": "Index repositories.", "ignored": "not indexed", "non_string": 7},
			},
			{ID: "source-less", Kind: "class", Name: "SourceLess"},
		},
		Artifacts: []rkcmodel.Artifact{{ID: "artifact", Kind: "source", Language: "go", Path: "internal/search/index.go", MediaType: "text/x-go", Status: "syntax_parsed"}},
		Documents: []rkcmodel.Document{{
			ID: "document", Kind: "guide", Title: "Search Guide", Path: "docs/search.md",
			Sections: []rkcmodel.DocumentSection{{Heading: "Overview", PlainText: "Ranked retrieval."}, {Heading: "Usage", PlainText: "Query the graph."}},
		}},
	}
	index := BuildFromBundle(bundle)
	if index.DocumentCount != 4 || len(index.Documents) != 4 || index.Version != IndexVersion {
		t.Fatalf("BuildFromBundle index metadata = %+v", index)
	}
	node := index.Documents["node"]
	if node.ObjectType != "node" || node.Path != "pkg/compile.go" || !strings.Contains(node.Body, "Compile a graph.") || !strings.Contains(node.Body, "Index repositories.") || strings.Contains(node.Body, "not indexed") {
		t.Fatalf("node search document = %+v", node)
	}
	if index.Documents["source-less"].Path != "" {
		t.Fatal("source-less node should have an empty path")
	}
	artifact := index.Documents["artifact"]
	if artifact.ObjectType != "artifact" || artifact.Title != "index.go" || artifact.QualifiedName != artifact.Path || artifact.Body != "text/x-go syntax_parsed" {
		t.Fatalf("artifact search document = %+v", artifact)
	}
	document := index.Documents["document"]
	if document.ObjectType != "document" || !strings.Contains(document.Body, "Overview\nRanked retrieval.") || !strings.Contains(document.Body, "Usage\nQuery the graph.") {
		t.Fatalf("generated document search document = %+v", document)
	}
	for _, term := range []string{"compile", "graph", "ranked", "retrieval"} {
		if len(index.Postings[term]) == 0 {
			t.Errorf("expected posting for %q", term)
		}
	}
}

func TestBuildIsDeterministicAndPreservesBoostedFieldTraces(t *testing.T) {
	t.Parallel()

	documents := searchDocuments()
	left := Build(documents)
	reversed := append([]Document(nil), documents...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	right := Build(reversed)
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	if string(leftJSON) != string(rightJSON) {
		t.Fatalf("Build is input-order dependent:\n%s\n%s", leftJSON, rightJSON)
	}
	if left.DocumentCount != len(documents) || left.AverageLength <= 0 {
		t.Fatalf("index accounting = %+v", left)
	}
	postings := left.Postings["graph"]
	if !sort.SliceIsSorted(postings, func(i, j int) bool { return postings[i].DocumentID < postings[j].DocumentID }) {
		t.Fatalf("postings are not deterministic: %+v", postings)
	}
	var graphSearch Posting
	for _, posting := range postings {
		if posting.DocumentID == "n1" {
			graphSearch = posting
		}
	}
	if graphSearch.TermCount < 2 || graphSearch.FieldBoost != 8 || !sort.StringsAreSorted(graphSearch.Fields) || !contains(graphSearch.Fields, "title") || !contains(graphSearch.Fields, "body") {
		t.Fatalf("boosted posting = %+v", graphSearch)
	}

	empty := Build(nil)
	if empty.DocumentCount != 0 || empty.AverageLength != 0 || len(empty.Documents) != 0 || len(empty.Postings) != 0 {
		t.Fatalf("empty Build() = %+v", empty)
	}
}

func TestSearchRanksExactMatchesAndReturnsDeterministicReasons(t *testing.T) {
	t.Parallel()

	index := Build(searchDocuments())
	response := index.Search(Query{Text: "GraphSearch", Limit: 10})
	if response.Query != "GraphSearch" || response.Mode != "embedded-bm25-lexical" || response.IndexVersion != IndexVersion || response.Truncated || len(response.Hits) < 2 {
		t.Fatalf("Search response envelope = %+v", response)
	}
	first := response.Hits[0]
	if first.Document.ID != "n1" || first.Score <= response.Hits[1].Score || !contains(first.Reasons, "exact_title") ||
		!contains(first.Reasons, "title:graph") || !contains(first.Terms, "graph") || !contains(first.Terms, "search") ||
		!sort.StringsAreSorted(first.Reasons) || !sort.StringsAreSorted(first.Terms) {
		t.Fatalf("exact ranked hit = %+v", first)
	}

	prefix := index.Search(Query{Text: "internal/search"})
	if len(prefix.Hits) == 0 || !contains(prefix.Hits[0].Reasons, "prefix_path") {
		t.Fatalf("path prefix bonus missing: %+v", prefix.Hits)
	}

	duplicateTerms := index.Search(Query{Text: "graph graph graph"})
	if len(duplicateTerms.Hits) == 0 || !reflect.DeepEqual(duplicateTerms.Hits[0].Terms, []string{"graph"}) {
		t.Fatalf("duplicate query terms were not collapsed: %+v", duplicateTerms.Hits)
	}
	if noHits := index.Search(Query{Text: "term-that-does-not-exist"}); len(noHits.Hits) != 0 || noHits.Truncated {
		t.Fatalf("unknown term response = %+v", noHits)
	}
}

func TestSearchParsesAndAppliesFiltersWithExplicitOptionsTakingPrecedence(t *testing.T) {
	t.Parallel()

	index := Build(searchDocuments())
	parsed := index.Search(Query{Text: "graph kind:function lang:go type:node path:internal/"})
	if len(parsed.Hits) != 1 || parsed.Hits[0].Document.ID != "n1" {
		t.Fatalf("parsed filters = %+v", parsed.Hits)
	}
	alias := index.Search(Query{Text: "graph language:go type:node"})
	if len(alias.Hits) != 2 {
		t.Fatalf("language alias filter = %+v", alias.Hits)
	}

	explicit := index.Search(Query{
		Text:  "graph kind:function lang:python type:artifact path:wrong/",
		Kinds: map[string]struct{}{"method": {}}, Languages: map[string]struct{}{"go": {}},
		ObjectTypes: map[string]struct{}{"node": {}}, PathPrefix: "pkg/",
	})
	if len(explicit.Hits) != 1 || explicit.Hits[0].Document.ID != "n2" {
		t.Fatalf("explicit filters did not override parsed filters = %+v", explicit.Hits)
	}

	for name, query := range map[string]Query{
		"kind":        {Text: "graph", Kinds: map[string]struct{}{"missing": {}}},
		"language":    {Text: "graph", Languages: map[string]struct{}{"rust": {}}},
		"object type": {Text: "graph", ObjectTypes: map[string]struct{}{"missing": {}}},
		"path":        {Text: "graph", PathPrefix: "missing/"},
	} {
		if got := index.Search(query); len(got.Hits) != 0 {
			t.Errorf("%s negative filter returned %+v", name, got.Hits)
		}
	}
	if got := index.Search(Query{Text: "kind:function"}); len(got.Hits) != 0 {
		t.Fatalf("filter-only query should not fabricate lexical matches: %+v", got.Hits)
	}
}

func TestSearchLimitBoundsTruncationAndTieBreaking(t *testing.T) {
	t.Parallel()

	documents := make([]Document, 1002)
	for i := range documents {
		documents[i] = Document{ID: "id-" + strconv.Itoa(i), QualifiedName: "same", Title: "Common", Body: "common"}
	}
	index := Build(documents)
	limited := index.Search(Query{Text: "common", Limit: 2})
	if !limited.Truncated || len(limited.Hits) != 2 || limited.Hits[0].Document.ID != "id-0" || limited.Hits[1].Document.ID != "id-1" {
		t.Fatalf("limited deterministic hits = %+v", limited)
	}
	defaulted := index.Search(Query{Text: "common", Limit: 0})
	if !defaulted.Truncated || len(defaulted.Hits) != 50 {
		t.Fatalf("default limit response count=%d truncated=%v", len(defaulted.Hits), defaulted.Truncated)
	}
	capped := index.Search(Query{Text: "common", Limit: 5000})
	if !capped.Truncated || len(capped.Hits) != 1000 {
		t.Fatalf("capped limit response count=%d truncated=%v", len(capped.Hits), capped.Truncated)
	}
}

func TestSearchHandlesZeroAverageLengthIndex(t *testing.T) {
	t.Parallel()

	index := &Index{
		Version: IndexVersion, DocumentCount: 1, AverageLength: 0,
		Documents:      map[string]Document{"id": {ID: "id", Title: "term"}},
		DocumentLength: map[string]int{"id": 0},
		Postings:       map[string][]Posting{"term": {{DocumentID: "id", TermCount: 1, FieldBoost: 1, Fields: []string{"title"}}}},
	}
	response := index.Search(Query{Text: "term"})
	if len(response.Hits) != 1 || math.IsNaN(response.Hits[0].Score) || math.IsInf(response.Hits[0].Score, 0) {
		t.Fatalf("zero-average search = %+v", response)
	}
}

func TestSaveLoadRoundTripAndFailureModes(t *testing.T) {
	t.Parallel()

	index := Build(searchDocuments())
	path := filepath.Join(t.TempDir(), "nested", "index.json")
	if err := index.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, index) {
		t.Fatalf("round trip mismatch:\nwant=%+v\ngot=%+v", index, loaded)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		t.Fatalf("saved index stat = %v, %v", info, err)
	}

	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, err := Load(missing); err == nil {
		t.Fatal("Load accepted a missing file")
	}
	malformed := filepath.Join(t.TempDir(), "malformed.json")
	if err := os.WriteFile(malformed, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(malformed); err == nil || !strings.Contains(err.Error(), "decode search index") {
		t.Fatalf("malformed Load error = %v", err)
	}
	unsupported := filepath.Join(t.TempDir(), "unsupported.json")
	if err := os.WriteFile(unsupported, []byte(`{"version":"future"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unsupported); err == nil || !strings.Contains(err.Error(), "unsupported search index version future") {
		t.Fatalf("unsupported Load error = %v", err)
	}

	fileParent := filepath.Join(t.TempDir(), "parent-file")
	if err := os.WriteFile(fileParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := index.Save(filepath.Join(fileParent, "index.json")); err == nil {
		t.Fatal("Save accepted a path beneath a regular file")
	}
	marshalFailure := *index
	marshalFailure.AverageLength = math.NaN()
	if err := marshalFailure.Save(filepath.Join(t.TempDir(), "nan.json")); err == nil {
		t.Fatal("Save accepted non-JSON floating-point state")
	}
	if err := index.Save(filepath.Join("/proc", "rkc-search-index-test.json")); err == nil {
		t.Fatal("Save unexpectedly created a temporary file in /proc")
	}

	destinationDirectory := filepath.Join(t.TempDir(), "existing-directory")
	if err := os.Mkdir(destinationDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destinationDirectory, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := index.Save(destinationDirectory); err == nil {
		t.Fatal("Save unexpectedly replaced a non-empty directory")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(destinationDirectory), ".search-index-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("failed Save leaked temporary files: %v, %v", matches, err)
	}
}

func TestQueryParsingTokenizationAndSmallHelpers(t *testing.T) {
	t.Parallel()

	text, parsed := parseQuery("Hello kind:function kind:method lang:go language:rust type:node path:internal/ unknown:value empty:")
	if text != "Hello unknown:value empty:" || !reflect.DeepEqual(parsed.kinds, map[string]struct{}{"function": {}, "method": {}}) ||
		!reflect.DeepEqual(parsed.languages, map[string]struct{}{"go": {}, "rust": {}}) || !reflect.DeepEqual(parsed.objectTypes, map[string]struct{}{"node": {}}) || parsed.pathPrefix != "internal/" {
		t.Fatalf("parseQuery = %q, %+v", text, parsed)
	}
	if text, parsed := parseQuery(""); text != "" || parsed.kinds != nil || parsed.languages != nil || parsed.objectTypes != nil || parsed.pathPrefix != "" {
		t.Fatalf("empty parseQuery = %q, %+v", text, parsed)
	}
	if got := splitCamel("HTTPServerID"); got != "HTTP Server ID" {
		t.Fatalf("splitCamel = %q", got)
	}
	if got := tokenize("A HTTPServerID user_id x 7 café"); !reflect.DeepEqual(got, []string{"http", "server", "id", "user_id", "7", "café"}) {
		t.Fatalf("tokenize = %v", got)
	}
	if got := normalize("HTTPServer-ID"); got != "http server id" {
		t.Fatalf("normalize = %q", got)
	}
	if got := unique([]string{"b", "a", "b", "c", "a"}); !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
		t.Fatalf("unique = %v", got)
	}
	if got := keys(map[string]struct{}{"b": {}, "a": {}}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("keys = %v", got)
	}
	if got := roundScore(1.23456789); got != 1.234568 {
		t.Fatalf("roundScore = %v", got)
	}

	document := Document{Kind: "function", Language: "go", ObjectType: "node", Path: "internal/search.go"}
	if !matchesFilters(document, Query{}) || !matchesFilters(document, Query{Kinds: map[string]struct{}{"function": {}}, Languages: map[string]struct{}{"go": {}}, ObjectTypes: map[string]struct{}{"node": {}}, PathPrefix: "internal/"}) {
		t.Fatal("matchesFilters rejected matching document")
	}
}

func searchDocuments() []Document {
	return []Document{
		{ID: "n1", ObjectType: "node", Kind: "function", Language: "go", Title: "GraphSearch", QualifiedName: "pkg.GraphSearch", Signature: "func GraphSearch(query string)", Path: "internal/search/index.go", Body: "deterministic graph lexical retrieval"},
		{ID: "n2", ObjectType: "node", Kind: "method", Language: "go", Title: "SearchGraph", QualifiedName: "pkg.SearchGraph", Signature: "func SearchGraph()", Path: "pkg/search.go", Body: "graph retrieval method"},
		{ID: "a1", ObjectType: "artifact", Kind: "source", Language: "go", Title: "index.go", QualifiedName: "internal/search/index.go", Path: "internal/search/index.go", Body: "text included graph"},
		{ID: "d1", ObjectType: "document", Kind: "guide", Title: "Graph Guide", QualifiedName: "docs/graph.md", Path: "docs/graph.md", Body: "advanced graph retrieval guide"},
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
