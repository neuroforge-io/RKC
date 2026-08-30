package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestInitializeAndSearch(t *testing.T) {
	node := rkcmodel.Node{ID: "n1", Kind: "function", Name: "Login", QualifiedName: "auth.Login", EvidenceIDs: []string{"e1"}}
	evidence := rkcmodel.Evidence{ID: "e1", Kind: "declared", Method: "test", Confidence: 1}
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "s1", SchemaVersion: rkcmodel.SchemaVersion}, Nodes: []rkcmodel.Node{node}, Evidence: []rkcmodel.Evidence{evidence}}
	dataset := &server.Dataset{Manifest: bundle.Snapshot, Bundle: bundle, NodeByID: map[string]rkcmodel.Node{"n1": node}, ArtifactByID: map[string]rkcmodel.Artifact{}, EvidenceByID: map[string]rkcmodel.Evidence{"e1": evidence}, Graph: graphindex.Build(bundle.Nodes, bundle.Edges), Search: search.BuildFromBundle(bundle)}
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"rkc.search","arguments":{"query":"Login"}}}` + "\n")
	var output bytes.Buffer
	if err := New(dataset, "test").Serve(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, ProtocolVersion) || !strings.Contains(text, "auth.Login") {
		t.Fatalf("unexpected response: %s", text)
	}
}

func TestToolSearchPreservesCanonicalHitsAcrossObjectTypes(t *testing.T) {
	t.Parallel()

	node := rkcmodel.Node{ID: "node", Kind: "function", Name: "SharedIndex", QualifiedName: "search.SharedIndex"}
	artifact := rkcmodel.Artifact{ID: "artifact", Kind: "source", Path: "internal/shared-index.go", Status: "syntax_parsed"}
	document := rkcmodel.Document{
		ID: "document", Kind: "guide", Title: "Shared Index Guide", Path: "docs/shared-index.md",
		Sections: []rkcmodel.DocumentSection{{Heading: "Overview", PlainText: "Shared search index guidance."}},
	}
	bundle := rkcmodel.Bundle{Nodes: []rkcmodel.Node{node}, Artifacts: []rkcmodel.Artifact{artifact}, Documents: []rkcmodel.Document{document}}
	index := search.BuildFromBundle(bundle)
	dataset := &server.Dataset{
		Bundle: bundle, NodeByID: map[string]rkcmodel.Node{node.ID: node},
		ArtifactByID: map[string]rkcmodel.Artifact{artifact.ID: artifact}, Search: index,
	}
	mcp := New(dataset, "test")
	type decodedResult struct {
		search.Hit
		Node *rkcmodel.Node `json:"node,omitempty"`
	}
	type decodedResponse struct {
		Query        string          `json:"query"`
		Results      []decodedResult `json:"results"`
		Truncated    bool            `json:"truncated"`
		Mode         string          `json:"mode"`
		IndexVersion string          `json:"index_version"`
	}
	decode := func(t *testing.T, query string) decodedResponse {
		t.Helper()
		got, err := mcp.toolSearch(map[string]any{"query": query})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var response decodedResponse
		if err := json.Unmarshal(encoded, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	canonicalHits := func(response decodedResponse) []search.Hit {
		hits := make([]search.Hit, len(response.Results))
		for index, result := range response.Results {
			hits[index] = result.Hit
		}
		return hits
	}

	tests := []struct {
		name       string
		query      string
		objectType string
		wantID     string
		wantNode   bool
	}{
		{name: "node remains enriched", query: "type:node SharedIndex", objectType: "node", wantID: node.ID, wantNode: true},
		{name: "artifact filter", query: "type:artifact shared", objectType: "artifact", wantID: artifact.ID},
		{name: "document filter", query: "type:document Shared", objectType: "document", wantID: document.ID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			canonical := index.Search(search.Query{Text: test.query, Limit: 20})
			response := decode(t, test.query)
			if len(response.Results) != 1 || response.Results[0].Document.ID != test.wantID || response.Results[0].Document.ObjectType != test.objectType {
				t.Fatalf("toolSearch(%q) results = %+v", test.query, response.Results)
			}
			if (response.Results[0].Node != nil) != test.wantNode {
				t.Fatalf("toolSearch(%q) node enrichment = %+v, want present %t", test.query, response.Results[0].Node, test.wantNode)
			}
			gotHits := canonicalHits(response)
			if !reflect.DeepEqual(gotHits, canonical.Hits) {
				t.Fatalf("toolSearch(%q) changed canonical ranking:\n got: %+v\nwant: %+v", test.query, gotHits, canonical.Hits)
			}
			if response.Query != canonical.Query || response.Truncated != canonical.Truncated || response.Mode != canonical.Mode || response.IndexVersion != canonical.IndexVersion {
				t.Fatalf("toolSearch(%q) metadata = %+v, canonical = %+v", test.query, response, canonical)
			}
		})
	}

	canonical := index.Search(search.Query{Text: "Shared", Limit: 20})
	response := decode(t, "Shared")
	gotHits := canonicalHits(response)
	if len(gotHits) != 3 || !reflect.DeepEqual(gotHits, canonical.Hits) {
		t.Fatalf("mixed tool search changed or discarded canonical ranking:\n got: %+v\nwant: %+v", gotHits, canonical.Hits)
	}
}

func TestServeRejectsInvalidRuntimeDependencies(t *testing.T) {
	dataset := &server.Dataset{}
	tests := []struct {
		name   string
		server *Server
		ctx    context.Context
		input  io.Reader
		output io.Writer
		want   string
	}{
		{name: "nil context", server: New(dataset, "test"), input: strings.NewReader(""), output: io.Discard, want: "context is required"},
		{name: "nil input", server: New(dataset, "test"), ctx: context.Background(), output: io.Discard, want: "input is required"},
		{name: "nil output", server: New(dataset, "test"), ctx: context.Background(), input: strings.NewReader(""), want: "output is required"},
		{name: "nil server", ctx: context.Background(), input: strings.NewReader(""), output: io.Discard, want: "dataset is required"},
		{name: "nil dataset", server: New(nil, "test"), ctx: context.Background(), input: strings.NewReader(""), output: io.Discard, want: "dataset is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.server.Serve(test.ctx, test.input, test.output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Serve error = %v, want %q", err, test.want)
			}
		})
	}
}
