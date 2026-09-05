// Copyright 2026 NeuroForgeIO. Licensed under the Apache License, Version 2.0.

package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	rkcclient "github.com/neuroforge-io/RKC/pkg/client"
	"github.com/neuroforge-io/RKC/pkg/rkcapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestTypedClientCollectionPaginationAgainstServer(t *testing.T) {
	dataset := paginationDataset(19)
	api, replacement := paginationTypedClients(t, dataset)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes, types, functions, artifacts := []string{}, []string{}, []string{}, []string{}
	for i, node := range dataset.Bundle.Nodes {
		nodes = append(nodes, node.ID)
		artifacts = append(artifacts, dataset.Bundle.Artifacts[i].ID)
		if node.Kind == "type" {
			types = append(types, node.ID)
		} else {
			functions = append(functions, node.ID)
		}
	}
	t.Run("nodes", func(t *testing.T) {
		checkTypedCollectionPages(t, api, replacement, dataset.Manifest.ID, types,
			func(api *rkcclient.Client, cursor string, limit int, snapshot string) (rkcapi.NodePage, error) {
				return api.ListNodes(ctx, rkcclient.NodeListOptions{Cursor: cursor, Limit: limit, ExpectedSnapshotID: snapshot, Kind: "type", Language: "go"})
			}, func(node rkcmodel.Node) string { return node.ID })
	})
	t.Run("ranked_nodes", func(t *testing.T) {
		checkTypedCollectionPages(t, api, replacement, dataset.Manifest.ID, functions,
			func(api *rkcclient.Client, cursor string, limit int, snapshot string) (rkcapi.NodePage, error) {
				return api.ListNodes(ctx, rkcclient.NodeListOptions{Cursor: cursor, Limit: limit, ExpectedSnapshotID: snapshot, Query: "shared", Kind: "function", Language: "go"})
			}, func(node rkcmodel.Node) string { return node.ID })
	})
	t.Run("artifacts", func(t *testing.T) {
		checkTypedCollectionPages(t, api, replacement, dataset.Manifest.ID, artifacts,
			func(api *rkcclient.Client, cursor string, limit int, snapshot string) (rkcapi.ArtifactPage, error) {
				return api.ListArtifacts(ctx, rkcclient.ArtifactListOptions{Cursor: cursor, Limit: limit, ExpectedSnapshotID: snapshot, Language: "go", Status: "parsed", PathPrefix: "src/"})
			}, func(artifact rkcmodel.Artifact) string { return artifact.ID })
	})
	t.Run("edges", func(t *testing.T) {
		checkTypedCollectionPages(t, api, replacement, dataset.Manifest.ID, nodes,
			func(api *rkcclient.Client, cursor string, limit int, snapshot string) (rkcapi.EdgePage, error) {
				return api.ListEdges(ctx, rkcclient.EdgeListOptions{Cursor: cursor, Limit: limit, ExpectedSnapshotID: snapshot, Kind: "calls", From: "a", To: "b", Resolution: "DECLARED"})
			}, func(edge rkcmodel.Edge) string { return edge.ID })
	})
	t.Run("diagnostics", func(t *testing.T) {
		checkTypedCollectionPages(t, api, replacement, dataset.Manifest.ID, nodes,
			func(api *rkcclient.Client, cursor string, limit int, snapshot string) (rkcapi.DiagnosticPage, error) {
				return api.ListDiagnostics(ctx, rkcclient.DiagnosticListOptions{Cursor: cursor, Limit: limit, ExpectedSnapshotID: snapshot, Severity: "warning", Code: "example"})
			}, func(diagnostic rkcmodel.Diagnostic) string { return diagnostic.ID })
	})
}

func checkTypedCollectionPages[T any](t *testing.T, api, replacement *rkcclient.Client, snapshot string, expected []string, fetch func(*rkcclient.Client, string, int, string) (rkcapi.CollectionPage[T], error), id func(T) string) {
	t.Helper()
	var cursor, firstCursor, expectedSnapshot string
	seen := map[string]bool{}
	var actual []string
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > len(expected) {
			t.Fatal("typed collection pagination did not terminate")
		}
		limit := []int{3, 2, 4}[pageNumber%3]
		page, err := fetch(api, cursor, limit, expectedSnapshot)
		if err != nil {
			t.Fatalf("page %d: %v", pageNumber, err)
		}
		if page.SnapshotID != snapshot || page.Total != len(expected) || len(page.Items) == 0 || len(page.Items) > limit || page.Truncated != (page.NextCursor != "") {
			t.Fatalf("invalid typed collection page %d: %+v", pageNumber, page)
		}
		if pageNumber == 0 {
			firstCursor, expectedSnapshot = page.NextCursor, page.SnapshotID
			if firstCursor == "" {
				t.Fatal("fixture did not require continuation")
			}
		}
		for _, item := range page.Items {
			objectID := id(item)
			if seen[objectID] {
				t.Fatalf("duplicate typed collection record %q", objectID)
			}
			seen[objectID] = true
			actual = append(actual, objectID)
		}
		cursor = page.NextCursor
		if !page.Truncated {
			if pageNumber == 0 || cursor != "" || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("incomplete typed collection traversal: cursor=%q actual=%v expected=%v", cursor, actual, expected)
			}
			break
		}
	}
	stale, err := fetch(replacement, firstCursor, 2, expectedSnapshot)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !reflect.DeepEqual(stale, rkcapi.CollectionPage[T]{}) {
		t.Fatalf("same-ID reload must return HTTP 400 and zero typed collection: page=%+v err=%v", stale, err)
	}
}

func TestTypedClientSearchPaginationAgainstServer(t *testing.T) {
	dataset := paginationDataset(19)
	api, replacement := paginationTypedClients(t, dataset)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	options := rkcclient.SearchPageOptions{Kinds: []string{"function", "type"}, Languages: []string{"go"}, ObjectTypes: []string{"node"}}
	var firstCursor string
	seen := map[string]bool{}
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > len(dataset.Bundle.Nodes) {
			t.Fatal("typed search pagination did not terminate")
		}
		options.Limit = []int{3, 2, 4}[pageNumber%3]
		page, err := api.SearchPage(ctx, "shared", options)
		if err != nil {
			t.Fatalf("page %d: %v", pageNumber, err)
		}
		if page.SnapshotID != dataset.Manifest.ID || page.Total != len(dataset.Bundle.Nodes) || len(page.Hits) == 0 || len(page.Hits) > options.Limit || page.Truncated != (page.NextCursor != "") {
			t.Fatalf("invalid typed search page %d: %+v", pageNumber, page)
		}
		if page.Query != "shared" || page.Mode != "embedded-bm25-lexical" || page.IndexVersion != dataset.Search.Version || page.Count != len(page.Hits) || len(page.Results) != len(page.Hits) {
			t.Fatalf("typed search lost retrieval metadata: %+v", page)
		}
		if pageNumber == 0 {
			firstCursor, options.ExpectedSnapshotID = page.NextCursor, page.SnapshotID
			if firstCursor == "" {
				t.Fatal("fixture did not require search continuation")
			}
		}
		for i, hit := range page.Hits {
			if seen[hit.Document.ID] || hit.Document.ID != fmt.Sprintf("id-%06d", len(seen)) || hit.Document.ObjectType != "node" || len(hit.Reasons) == 0 || len(hit.Terms) == 0 || page.Results[i].Node.ID != hit.Document.ID {
				t.Fatalf("typed search lost order, identity, or evidence: %+v", hit)
			}
			seen[hit.Document.ID] = true
		}
		options.Cursor = page.NextCursor
		if !page.Truncated {
			if pageNumber == 0 || options.Cursor != "" || len(seen) != len(dataset.Bundle.Nodes) {
				t.Fatalf("incomplete typed search traversal: cursor=%q reached=%d", options.Cursor, len(seen))
			}
			break
		}
	}
	options.Cursor, options.Limit = firstCursor, 2
	stale, err := replacement.SearchPage(ctx, "shared", options)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !reflect.DeepEqual(stale, rkcclient.SearchResponse{}) {
		t.Fatalf("same-ID reload must return HTTP 400 and zero typed search: response=%+v err=%v", stale, err)
	}
}

func paginationTypedClients(t *testing.T, dataset *Dataset) (*rkcclient.Client, *rkcclient.Client) {
	t.Helper()
	connect := func(dataset *Dataset) *rkcclient.Client {
		server := httptest.NewServer(dataset.Handler())
		t.Cleanup(server.Close)
		api, err := rkcclient.New(server.URL, rkcclient.WithHTTPClient(server.Client()))
		if err != nil {
			t.Fatal(err)
		}
		return api
	}
	return connect(dataset), connect(paginationDataset(len(dataset.Bundle.Nodes)))
}
