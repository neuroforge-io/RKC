package mcpserver

import (
	"bytes"
	"context"
	"io"
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
