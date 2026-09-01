package search

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestMaximumIndexWriterFailsBeforeCrossingLimit(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writer := &maximumIndexWriter{writer: &output, maximum: 3}
	if written, err := writer.Write([]byte("ab")); err != nil || written != 2 {
		t.Fatalf("initial bounded write = %d, %v", written, err)
	}
	if written, err := writer.Write([]byte("cd")); err == nil || written != 0 || !strings.Contains(err.Error(), "3-byte limit") {
		t.Fatalf("overflowing bounded write = %d, %v", written, err)
	}
	if output.String() != "ab" {
		t.Fatalf("bounded writer crossed its limit: %q", output.String())
	}
}

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

func TestBuildFromBundleRedactsNodeAndParsedDocumentSecrets(t *testing.T) {
	t.Parallel()
	secret := "sk_live_" + strings.Repeat("a", 32)
	bundle := rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{{
			ID: "node", Kind: "function", Name: "Login",
			Attributes: map[string]any{"docstring": "api_key=" + secret},
		}},
		Documents: []rkcmodel.Document{{
			ID: "document", Kind: "guide", Title: "Guide", Path: "docs/guide.md",
			Sections: []rkcmodel.DocumentSection{{Heading: "Example", PlainText: "access_token=" + secret}},
		}},
	}
	index := BuildFromBundle(bundle)
	for _, id := range []string{"node", "document"} {
		body := index.Documents[id].Body
		if strings.Contains(body, secret) || !strings.Contains(body, "***") {
			t.Fatalf("%s body was not secret-redacted: %q", id, body)
		}
	}
	if response := index.Search(Query{Text: secret}); len(response.Hits) != 0 {
		t.Fatalf("secret literal remained searchable: %+v", response)
	}
}

func TestRepositoryTextCorpusIsSearchableSnapshotBoundAndRecomputed(t *testing.T) {
	t.Parallel()
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "snapshot-text"},
		Artifacts: []rkcmodel.Artifact{
			{ID: "source", Kind: "file", Path: "config/service.yaml", Language: "yaml", MediaType: "application/yaml", SHA256: strings.Repeat("a", 64), Text: true, Status: "parsed"},
			{ID: "binary", Kind: "file", Path: "assets/model.bin", MediaType: "application/octet-stream", SHA256: strings.Repeat("b", 64), Status: "binary"},
		},
	}
	index := BuildFromBundleWithArtifactBodies(bundle, map[string]string{
		"source":  "counterfactual routing threshold: 0.75\napi_key: ********",
		"binary":  "BINARY_SENTINEL_MUST_NOT_BE_INDEXED",
		"unknown": "UNKNOWN_SENTINEL_MUST_NOT_BE_INDEXED",
	})
	if index.SnapshotID != bundle.Snapshot.ID || index.CorpusVersion != RepositoryTextCorpusVersion {
		t.Fatalf("repository text corpus binding = %+v", index)
	}
	if err := ValidateBundleIndex(index, bundle, true); err != nil {
		t.Fatal(err)
	}
	response := index.Search(Query{Text: "counterfactual threshold", ObjectTypes: map[string]struct{}{"artifact": {}}})
	if len(response.Hits) != 1 || response.Hits[0].Document.ID != "source" || !contains(response.Hits[0].Reasons, "body:counterfactual") {
		t.Fatalf("repository body search = %+v", response)
	}
	for _, forbidden := range []string{"BINARY_SENTINEL_MUST_NOT_BE_INDEXED", "UNKNOWN_SENTINEL_MUST_NOT_BE_INDEXED"} {
		if hits := index.Search(Query{Text: forbidden}).Hits; len(hits) != 0 {
			t.Fatalf("inadmissible body %q was indexed: %+v", forbidden, hits)
		}
	}

	for name, mutate := range map[string]func(*Index){
		"snapshot": func(candidate *Index) { candidate.SnapshotID = "other" },
		"body": func(candidate *Index) {
			document := candidate.Documents["source"]
			document.Body += " tampered"
			candidate.Documents["source"] = document
		},
		"metadata": func(candidate *Index) {
			document := candidate.Documents["source"]
			document.Metadata["rkc_secret_redacted"] = "false"
			candidate.Documents["source"] = document
		},
		"posting": func(candidate *Index) { candidate.Postings["counterfactual"][0].TermCount++ },
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			var candidate Index
			if err := json.Unmarshal(data, &candidate); err != nil {
				t.Fatal(err)
			}
			mutate(&candidate)
			if err := ValidateBundleIndex(&candidate, bundle, true); err == nil {
				t.Fatal("tampered repository text index was accepted")
			}
		})
	}

	metadataOnly := BuildFromBundle(bundle)
	if err := ValidateBundleIndex(metadataOnly, bundle, false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBundleIndex(metadataOnly, bundle, true); err == nil {
		t.Fatal("metadata-only index satisfied the repository text contract")
	}
}

func TestRepositoryTextCorpusBoundsLargeStructuredDataWithReceipts(t *testing.T) {
	t.Parallel()
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "snapshot-bounded-structured-data"},
		Artifacts: []rkcmodel.Artifact{
			{ID: "large-json", Kind: "file", Path: "data/records.json", Language: "json", MediaType: "application/json", SHA256: strings.Repeat("a", 64), SizeBytes: MaximumStructuredDataBodyBytes + 1, Text: true, Status: "text"},
			{ID: "small-json", Kind: "file", Path: "config/service.json", Language: "json", MediaType: "application/json", SHA256: strings.Repeat("b", 64), SizeBytes: MaximumStructuredDataBodyBytes, Text: true, Status: "parsed"},
			{ID: "notebook", Kind: "file", Path: "research/demo.ipynb", Language: "jupyter", MediaType: "application/x-ipynb+json", SHA256: strings.Repeat("c", 64), SizeBytes: MaximumStructuredDataBodyBytes + 1, Text: true, Status: "text"},
		},
	}
	index := BuildFromBundleWithArtifactBodies(bundle, map[string]string{
		"large-json": "large_dataset_sentinel",
		"small-json": "small_config_sentinel",
		"notebook":   "notebook_sentinel",
	})
	if err := ValidateBundleIndex(index, bundle, true); err != nil {
		t.Fatal(err)
	}
	large := index.Documents["large-json"]
	if large.Body != "application/json text" || !reflect.DeepEqual(large.Metadata, repositoryTextMetadataOnly(bundle.Artifacts[0])) {
		t.Fatalf("large structured-data document = %+v", large)
	}
	if hits := index.Search(Query{Text: "large_dataset_sentinel"}).Hits; len(hits) != 0 {
		t.Fatalf("large structured-data body was indexed: %+v", hits)
	}
	for _, query := range []string{"small_config_sentinel", "notebook_sentinel"} {
		if hits := index.Search(Query{Text: query}).Hits; len(hits) != 1 {
			t.Fatalf("admitted repository body %q was not indexed: %+v", query, hits)
		}
	}

	tampered := *index
	tampered.Documents = make(map[string]Document, len(index.Documents))
	for id, candidate := range index.Documents {
		if candidate.Metadata != nil {
			candidate.Metadata = map[string]string{}
			for key, value := range index.Documents[id].Metadata {
				candidate.Metadata[key] = value
			}
		}
		tampered.Documents[id] = candidate
	}
	document := tampered.Documents["large-json"]
	document.Metadata["rkc_body_exclusion"] = "tampered"
	tampered.Documents["large-json"] = document
	if err := ValidateBundleIndex(&tampered, bundle, true); err == nil || !strings.Contains(err.Error(), "exclusion metadata") {
		t.Fatalf("tampered exclusion receipt was accepted: %v", err)
	}
}

func TestValidateBundleIndexRejectsCrossTypeDuplicateIDs(t *testing.T) {
	t.Parallel()
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "snapshot-shared-id"},
		Nodes:    []rkcmodel.Node{{ID: "shared", Kind: "function", Name: "SupersededNode"}},
		Artifacts: []rkcmodel.Artifact{{
			ID: "shared", Kind: "file", Path: "main.go", Language: "go", MediaType: "text/x-go",
			SHA256: strings.Repeat("a", 64), Text: true, Status: "syntax_parsed",
		}},
	}
	index := BuildFromBundleWithArtifactBodies(bundle, map[string]string{"shared": "package fixture\n// searchable body\n"})
	if err := ValidateBundleObjectIDs(bundle); err == nil || !strings.Contains(err.Error(), "shared by node and artifact") {
		t.Fatalf("cross-type duplicate identity was accepted: %v", err)
	}
	if err := ValidateBundleIndex(index, bundle, true); err == nil || !strings.Contains(err.Error(), "shared by node and artifact") {
		t.Fatalf("duplicate-ID index was accepted: %v", err)
	}
}

func TestValidateBundleIndexCoalescesCanonicalArtifactNodeAlias(t *testing.T) {
	t.Parallel()
	artifact := rkcmodel.Artifact{
		ID: "shared", Kind: "file", Path: "main.go", Language: "go", MediaType: "text/x-go",
		SHA256: strings.Repeat("a", 64), Text: true, Status: "syntax_parsed",
	}
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "snapshot-artifact-alias"},
		Nodes: []rkcmodel.Node{{
			ID: artifact.ID, ArtifactID: artifact.ID, Kind: artifact.Kind,
			Name: "main.go", QualifiedName: artifact.Path, Language: artifact.Language,
		}},
		Artifacts: []rkcmodel.Artifact{artifact},
	}
	index := BuildFromBundleWithArtifactBodies(bundle, map[string]string{"shared": "package fixture\n"})
	if err := ValidateBundleObjectIDs(bundle); err != nil {
		t.Fatalf("canonical artifact-node alias was rejected: %v", err)
	}
	if err := ValidateBundleIndex(index, bundle, true); err != nil {
		t.Fatalf("coalesced artifact-node index was rejected: %v", err)
	}
	if index.DocumentCount != 1 || index.Documents[artifact.ID].ObjectType != "artifact" {
		t.Fatalf("canonical artifact-node alias was not deterministically coalesced: %+v", index)
	}
}

func TestSearchIndexesFullRepositoryBodyButBoundsReturnedUTF8(t *testing.T) {
	t.Parallel()
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "snapshot-large-text"},
		Artifacts: []rkcmodel.Artifact{{
			ID: "source", Kind: "file", Path: "docs/large.md", Language: "markdown",
			SHA256: strings.Repeat("a", 64), Text: true, Status: "text",
		}},
	}
	body := strings.Repeat("界", MaximumResultBodyBytes/2) + " distalneedle"
	index := BuildFromBundleWithArtifactBodies(bundle, map[string]string{"source": body})
	response := index.Search(Query{Text: "distalneedle"})
	if len(response.Hits) != 1 || len(response.Hits[0].Document.Body) > MaximumResultBodyBytes ||
		!utf8.ValidString(response.Hits[0].Document.Body) ||
		!strings.Contains(response.Hits[0].Document.Body, "distalneedle") ||
		!contains(response.Hits[0].Reasons, "body:excerpt") ||
		!contains(response.Hits[0].Reasons, "body:truncated") {
		t.Fatalf("bounded repository body result = %+v", response)
	}
	start, startErr := strconv.Atoi(response.Hits[0].Document.Metadata["rkc_excerpt_start_byte"])
	end, endErr := strconv.Atoi(response.Hits[0].Document.Metadata["rkc_excerpt_end_byte"])
	if startErr != nil || endErr != nil || start < 0 || end <= start || end > len(body) || response.Hits[0].Document.Body != body[start:end] {
		t.Fatalf("invalid excerpt receipt start=%d end=%d errors=%v/%v metadata=%v", start, end, startErr, endErr, response.Hits[0].Document.Metadata)
	}
	if index.Documents["source"].Body != body {
		t.Fatal("search mutated the indexed full repository body")
	}
}

func TestSafePreallocationCapacityRejectsIntegerOverflow(t *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	if got := safePreallocationCapacity(3, 5, 7); got != 15 {
		t.Fatalf("safePreallocationCapacity() = %d, want 15", got)
	}
	for _, lengths := range [][]int{{maximumInt, 1}, {1, -1}} {
		if got := safePreallocationCapacity(lengths...); got != 0 {
			t.Fatalf("safePreallocationCapacity(%v) = %d, want fail-safe zero", lengths, got)
		}
	}
}

func TestBoundedBundleBuildersBindCorpusAndRejectOversizedBodies(t *testing.T) {
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "bounded-snapshot"},
		Artifacts: []rkcmodel.Artifact{{
			ID: "source", Kind: "file", Path: "README.md", Text: true, Status: "text",
		}},
	}
	repositoryIndex, err := BuildFromBundleWithArtifactBodiesBounded(bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	if repositoryIndex.SnapshotID != bundle.Snapshot.ID || repositoryIndex.CorpusVersion != RepositoryTextCorpusVersion {
		t.Fatalf("bounded repository index binding = %+v", repositoryIndex)
	}
	metadataIndex, err := BuildFromBundleBounded(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if metadataIndex.SnapshotID != bundle.Snapshot.ID || metadataIndex.CorpusVersion != "" {
		t.Fatalf("bounded metadata index binding = %+v", metadataIndex)
	}
	if unbounded := BuildFromBundleWithArtifactBodies(bundle, nil); unbounded.CorpusVersion != RepositoryTextCorpusVersion {
		t.Fatalf("nil body map lost repository corpus binding: %+v", unbounded)
	}

	oversized := strings.Repeat("x", MaximumIndexedDocumentBytes+1)
	oversizedNode := rkcmodel.Bundle{Nodes: []rkcmodel.Node{{
		ID: "node", Attributes: map[string]any{"docstring": oversized},
	}}}
	if _, err := BuildFromBundleBounded(oversizedNode); err == nil || !strings.Contains(err.Error(), "per-document limit") {
		t.Fatalf("oversized metadata-only bundle = %v", err)
	}
	if _, err := BuildFromBundleWithArtifactBodiesBounded(bundle, map[string]string{"source": oversized}); err == nil || !strings.Contains(err.Error(), "per-document limit") {
		t.Fatalf("oversized repository body bundle = %v", err)
	}
}

func TestMatchesQueryAppliesInlineFiltersAndExplicitOverrides(t *testing.T) {
	t.Parallel()
	document := Document{
		ID: "node", ObjectType: "node", Kind: "function", Language: "go",
		Path: "internal/search/index.go",
	}
	if !MatchesQuery(document, Query{Text: "kind:function lang:go type:node path:internal/"}) {
		t.Fatal("matching inline filters rejected the document")
	}
	if MatchesQuery(document, Query{Text: "lang:python"}) {
		t.Fatal("mismatched inline language admitted the document")
	}
	explicit := Query{
		Text:        "kind:class lang:python type:artifact path:vendor/",
		Kinds:       map[string]struct{}{"function": {}},
		Languages:   map[string]struct{}{"go": {}},
		ObjectTypes: map[string]struct{}{"node": {}},
		PathPrefix:  "internal/",
	}
	if !MatchesQuery(document, explicit) {
		t.Fatal("explicit filters did not take precedence over inline filters")
	}
}

func TestValidateBundleObjectIDsRejectsAmbiguousCollisions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		bundle rkcmodel.Bundle
		want   string
	}{
		{
			name:   "duplicate nodes",
			bundle: rkcmodel.Bundle{Nodes: []rkcmodel.Node{{ID: "shared"}, {ID: "shared"}}},
			want:   "duplicated by nodes",
		},
		{
			name:   "duplicate artifacts",
			bundle: rkcmodel.Bundle{Artifacts: []rkcmodel.Artifact{{ID: "shared"}, {ID: "shared"}}},
			want:   "duplicated by artifacts",
		},
		{
			name:   "duplicate documents",
			bundle: rkcmodel.Bundle{Documents: []rkcmodel.Document{{ID: "shared"}, {ID: "shared"}}},
			want:   "duplicated by documents",
		},
		{
			name: "node and document",
			bundle: rkcmodel.Bundle{
				Nodes: []rkcmodel.Node{{ID: "shared"}}, Documents: []rkcmodel.Document{{ID: "shared"}},
			},
			want: "shared by node and document",
		},
		{
			name: "artifact and document",
			bundle: rkcmodel.Bundle{
				Artifacts: []rkcmodel.Artifact{{ID: "shared"}}, Documents: []rkcmodel.Document{{ID: "shared"}},
			},
			want: "shared by artifact and document",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateBundleObjectIDs(test.bundle)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateBundleObjectIDs(%s) = %v, want error containing %q", test.name, err, test.want)
			}
		})
	}
}

func TestValidateBundleIndexRejectsAccountingAndCanonicalDrift(t *testing.T) {
	t.Parallel()
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "validation-snapshot"},
		Nodes: []rkcmodel.Node{{
			ID: "node", Kind: "function", Name: "SearchGraph", QualifiedName: "pkg.SearchGraph",
			Attributes: map[string]any{"summary": "shared retrieval"},
		}},
	}
	valid := BuildFromBundle(bundle)
	clone := func(t *testing.T) *Index {
		t.Helper()
		encoded, err := json.Marshal(valid)
		if err != nil {
			t.Fatal(err)
		}
		var candidate Index
		if err := json.Unmarshal(encoded, &candidate); err != nil {
			t.Fatal(err)
		}
		return &candidate
	}
	if err := ValidateBundleIndex(nil, bundle, false); err == nil || !strings.Contains(err.Error(), "index is nil") {
		t.Fatalf("nil index = %v", err)
	}
	tests := []struct {
		name   string
		want   string
		mutate func(*Index)
	}{
		{name: "version", want: "unsupported search index version", mutate: func(index *Index) { index.Version = "future" }},
		{name: "corpus version", want: "unsupported search corpus version", mutate: func(index *Index) { index.CorpusVersion = "future" }},
		{name: "missing maps", want: "maps are missing", mutate: func(index *Index) { index.Documents = nil }},
		{name: "document set", want: "document set", mutate: func(index *Index) { index.Documents["extra"] = Document{ID: "extra"} }},
		{name: "missing canonical document", want: "missing canonical document", mutate: func(index *Index) {
			delete(index.Documents, "node")
			index.Documents["other"] = Document{ID: "other"}
		}},
		{name: "document identity", want: "differs from canonical identity", mutate: func(index *Index) {
			document := index.Documents["node"]
			document.Title = "Different"
			index.Documents["node"] = document
		}},
		{name: "document length", want: "document length is invalid", mutate: func(index *Index) { index.DocumentLength["node"]++ }},
		{name: "missing posting", want: "missing posting", mutate: func(index *Index) { delete(index.Postings, "search") }},
		{name: "invalid posting", want: "posting \"search\" is invalid", mutate: func(index *Index) { index.Postings["search"][0].TermCount++ }},
		{name: "document accounting", want: "document accounting", mutate: func(index *Index) { index.DocumentCount++ }},
		{name: "average length", want: "average document length", mutate: func(index *Index) { index.AverageLength++ }},
		{name: "empty posting list", want: "empty posting list", mutate: func(index *Index) { index.Postings["extra"] = nil }},
		{name: "unknown posting document", want: "references an unknown document", mutate: func(index *Index) {
			index.Postings["extra"] = []Posting{{DocumentID: "missing"}}
		}},
		{name: "posting count", want: "posting count does not match", mutate: func(index *Index) {
			index.Postings["extra"] = []Posting{{DocumentID: "node"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := clone(t)
			test.mutate(candidate)
			err := ValidateBundleIndex(candidate, bundle, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateBundleIndex(%s) = %v, want error containing %q", test.name, err, test.want)
			}
		})
	}

	twoNodeBundle := bundle
	twoNodeBundle.Nodes = append(append([]rkcmodel.Node(nil), bundle.Nodes...), rkcmodel.Node{
		ID: "node-two", Kind: "function", Name: "OtherGraph", QualifiedName: "pkg.OtherGraph",
		Attributes: map[string]any{"summary": "shared retrieval"},
	})
	unsorted := BuildFromBundle(twoNodeBundle)
	unsorted.Postings["extra"] = []Posting{{DocumentID: "node-two"}, {DocumentID: "node"}}
	if err := ValidateBundleIndex(unsorted, twoNodeBundle, false); err == nil || !strings.Contains(err.Error(), "unsorted") {
		t.Fatalf("unsorted posting list = %v", err)
	}
}

func TestValidateResourceEnvelopeRejectsCheapBoundaryViolations(t *testing.T) {
	t.Parallel()
	if isAdmittedTextArtifact(rkcmodel.Artifact{Text: true, Status: "binary"}) {
		t.Fatal("unsupported text status was admitted")
	}
	tests := []struct {
		name  string
		index *Index
		want  string
	}{
		{name: "nil", index: nil, want: "index is nil"},
		{
			name: "oversized document",
			index: &Index{Documents: map[string]Document{
				"large": {ID: "large", Body: strings.Repeat("x", MaximumIndexedDocumentBytes+1)},
			}},
			want: "per-document limit",
		},
		{
			name: "oversized term",
			index: &Index{Postings: map[string][]Posting{
				strings.Repeat("t", MaximumIndexedTermBytes+1): nil,
			}},
			want: "term exceeds",
		},
		{
			name:  "negative document length",
			index: &Index{DocumentLength: map[string]int{"negative": -1}},
			want:  "token corpus limit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateResourceEnvelope(test.index)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateResourceEnvelope(%s) = %v, want error containing %q", test.name, err, test.want)
			}
		})
	}
}

func TestBoundedMatchExcerptAndCaseFoldBoundaries(t *testing.T) {
	t.Parallel()
	if excerpt, start, end := boundedMatchExcerpt("value", []string{"value"}, 0); excerpt != "" || start != 0 || end != 0 {
		t.Fatalf("zero-sized excerpt = %q, %d, %d", excerpt, start, end)
	}
	if excerpt, start, end := boundedMatchExcerpt("short", []string{"short"}, 10); excerpt != "short" || start != 0 || end != 5 {
		t.Fatalf("short excerpt = %q, %d, %d", excerpt, start, end)
	}
	value := "0123456789xyzQ"
	if excerpt, start, end := boundedMatchExcerpt(value, []string{"q"}, 6); excerpt != "89xyzQ" || start != 8 || end != len(value) {
		t.Fatalf("tail excerpt = %q, %d, %d", excerpt, start, end)
	}
	if excerpt, start, end := boundedMatchExcerpt(value, []string{"absent"}, 6); excerpt != "012345" || start != 0 || end != 6 {
		t.Fatalf("no-match excerpt = %q, %d, %d", excerpt, start, end)
	}
	if got := indexFoldASCII("value", ""); got != -1 {
		t.Fatalf("empty ASCII term index = %d", got)
	}
	if got := indexFoldASCII("ABC", "bc"); got != 1 {
		t.Fatalf("ASCII case-fold index = %d", got)
	}
	if got := indexFoldUnicode("value", ""); got != -1 {
		t.Fatalf("empty Unicode term index = %d", got)
	}
	if got := indexFoldUnicode("café", "thé"); got != -1 {
		t.Fatalf("missing Unicode term index = %d", got)
	}
	if excerpt, start, end := boundedMatchExcerpt("界界A界", []string{"a"}, 5); excerpt != "界A" || start != 3 || end != 7 {
		t.Fatalf("UTF-8 boundary excerpt = %q, %d, %d", excerpt, start, end)
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

func TestSearchMatchExcerptUsesUnicodeCaseFolding(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("prefix ", MaximumResultBodyBytes/7+100) + "ÉCLAIR evidence"
	index := Build([]Document{{ID: "unicode", ObjectType: "artifact", Body: body}})
	response := index.Search(Query{Text: "éclair"})
	if len(response.Hits) != 1 || !strings.Contains(response.Hits[0].Document.Body, "ÉCLAIR") ||
		!contains(response.Hits[0].Reasons, "body:excerpt") {
		t.Fatalf("Unicode-folded match excerpt = %+v", response)
	}
}

func TestBuildDeduplicatesDocumentIDsConsistently(t *testing.T) {
	t.Parallel()

	winner := Document{ID: "shared", ObjectType: "artifact", Title: "winner", Body: "current term"}
	index := Build([]Document{
		{ID: "shared", ObjectType: "node", Title: "superseded", Body: "stale term"},
		winner,
	})
	want := Build([]Document{winner})

	gotJSON, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("duplicate-ID index is inconsistent:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if index.DocumentCount != len(index.Documents) || index.DocumentCount != len(index.DocumentLength) {
		t.Fatalf("document accounting count=%d documents=%d lengths=%d", index.DocumentCount, len(index.Documents), len(index.DocumentLength))
	}
	if len(index.Postings["stale"]) != 0 || len(index.Postings["current"]) != 1 {
		t.Fatalf("duplicate-ID postings = %+v", index.Postings)
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
