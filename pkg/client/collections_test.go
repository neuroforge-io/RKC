package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type pageAPI struct {
	name   string
	path   string
	max    int
	invoke func(*Client, int, string, string) (any, error)
}

func pageAPIs() []pageAPI {
	return []pageAPI{
		{"nodes", "/api/v1/nodes", 1000, func(c *Client, limit int, cursor, snapshot string) (any, error) {
			return c.ListNodes(context.Background(), NodeListOptions{Limit: limit, Cursor: cursor, ExpectedSnapshotID: snapshot})
		}},
		{"artifacts", "/api/v1/artifacts", 5000, func(c *Client, limit int, cursor, snapshot string) (any, error) {
			return c.ListArtifacts(context.Background(), ArtifactListOptions{Limit: limit, Cursor: cursor, ExpectedSnapshotID: snapshot})
		}},
		{"edges", "/api/v1/edges", 5000, func(c *Client, limit int, cursor, snapshot string) (any, error) {
			return c.ListEdges(context.Background(), EdgeListOptions{Limit: limit, Cursor: cursor, ExpectedSnapshotID: snapshot})
		}},
		{"diagnostics", "/api/v1/diagnostics", 5000, func(c *Client, limit int, cursor, snapshot string) (any, error) {
			return c.ListDiagnostics(context.Background(), DiagnosticListOptions{Limit: limit, Cursor: cursor, ExpectedSnapshotID: snapshot})
		}},
		{"search", "/api/v1/search", 1000, func(c *Client, limit int, cursor, snapshot string) (any, error) {
			return c.SearchPage(context.Background(), "query", SearchPageOptions{Limit: limit, Cursor: cursor, ExpectedSnapshotID: snapshot})
		}},
	}
}

func TestPaginatedAPIsEncodeFiltersAndOpaqueCursor(t *testing.T) {
	const cursor = "opaque +/=字?&%cursor"
	cases := []struct {
		name  string
		query url.Values
		call  func(*Client) (any, error)
	}{
		{"nodes", url.Values{"q": {"query + 字"}, "kind": {"function"}, "language": {"go"}}, func(c *Client) (any, error) {
			return c.ListNodes(context.Background(), NodeListOptions{Limit: 2, Cursor: cursor, ExpectedSnapshotID: "snap", Query: "query + 字", Kind: "function", Language: "go"})
		}},
		{"artifacts", url.Values{"language": {"go"}, "status": {"parsed"}, "path_prefix": {"docs/字 & "}}, func(c *Client) (any, error) {
			return c.ListArtifacts(context.Background(), ArtifactListOptions{Limit: 2, Cursor: cursor, ExpectedSnapshotID: "snap", Language: "go", Status: "parsed", PathPrefix: "docs/字 & "})
		}},
		{"edges", url.Values{"kind": {"calls"}, "from": {"node:a/+"}, "to": {"node:b?&"}, "resolution": {"declared"}}, func(c *Client) (any, error) {
			return c.ListEdges(context.Background(), EdgeListOptions{Limit: 2, Cursor: cursor, ExpectedSnapshotID: "snap", Kind: "calls", From: "node:a/+", To: "node:b?&", Resolution: "declared"})
		}},
		{"diagnostics", url.Values{"severity": {"warning"}, "code": {"RKC-TEST-1"}}, func(c *Client) (any, error) {
			return c.ListDiagnostics(context.Background(), DiagnosticListOptions{Limit: 2, Cursor: cursor, ExpectedSnapshotID: "snap", Severity: "warning", Code: "RKC-TEST-1"})
		}},
		{"search", url.Values{"q": {"query + 字"}, "kinds": {"function,method"}, "languages": {"go,python"}, "object_types": {"node,document"}, "path_prefix": {"pkg/"}}, func(c *Client) (any, error) {
			return c.SearchPage(context.Background(), "query + 字", SearchPageOptions{Limit: 2, Cursor: cursor, ExpectedSnapshotID: "snap", Kinds: []string{"function", "method"}, Languages: []string{"go", "python"}, ObjectTypes: []string{"node", "document"}, PathPrefix: "pkg/"})
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.query.Set("limit", "2")
			test.query.Set("cursor", cursor)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/prefix/api/v1/"+test.name || !reflect.DeepEqual(r.URL.Query(), test.query) {
					t.Errorf("request: %s %v; want %v", r.URL.Path, r.URL.Query(), test.query)
				}
				if r.Header.Get("Authorization") != "Bearer token" {
					t.Error("pagination dropped client authentication")
				}
				w.Header().Set("X-RKC-Snapshot-ID", "snap")
				_, _ = w.Write([]byte(`{"items":[{"id":"record"}],"hits":[{"document":{"id":"record","object_type":"document","title":"Title","body":"Source text"},"score":3,"reasons":["title"],"terms":["query"]}],"query":"query + 字","mode":"lexical","index_version":"v1","total":7,"truncated":true,"snapshot_id":"snap","next_cursor":"next opaque"}`))
			}))
			defer host.Close()
			c, err := New(host.URL+"/prefix", WithBearerToken("token"))
			if err != nil {
				t.Fatal(err)
			}
			page, err := test.call(c)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Items      []struct{ ID string }
				Total      int
				Truncated  bool
				SnapshotID string `json:"snapshot_id"`
				NextCursor string `json:"next_cursor"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Total != 7 || !envelope.Truncated || envelope.SnapshotID != "snap" || envelope.NextCursor != "next opaque" {
				t.Fatalf("page metadata: %+v", envelope)
			}
			if test.name == "search" {
				search := page.(SearchResponse)
				if len(search.Hits) != 1 || search.Hits[0].Document.ObjectType != "document" || search.Hits[0].Document.Body != "Source text" || search.Count != 1 || len(search.Results) != 1 {
					t.Fatalf("search projection or compatibility lost: %+v", search)
				}
			} else if len(envelope.Items) != 1 || envelope.Items[0].ID != "record" {
				t.Fatalf("canonical record lost: %+v", envelope)
			}
		})
	}
}

func TestPaginatedAPIsDefaultsAndLocalLimitBounds(t *testing.T) {
	for _, api := range pageAPIs() {
		t.Run(api.name, func(t *testing.T) {
			requests := 0
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				query := r.URL.Query()
				if api.name == "search" {
					query.Del("q")
				}
				if r.URL.Path != api.path || len(query) != 0 {
					t.Errorf("defaults should be omitted: %s %v", r.URL.Path, query)
				}
				w.Header().Set("X-RKC-Snapshot-ID", "snap")
				_, _ = w.Write([]byte(`{"items":[],"hits":[],"total":0,"truncated":false,"snapshot_id":"snap"}`))
			}))
			defer host.Close()
			c, _ := New(host.URL)
			if _, err := api.invoke(c, 0, "", ""); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []int{-1, api.max + 1} {
				page, err := api.invoke(c, invalid, "", "")
				if err == nil || !strings.Contains(err.Error(), "page limit") || !reflect.ValueOf(page).IsZero() {
					t.Errorf("invalid limit %d: %+v %v", invalid, page, err)
				}
			}
			if requests != 1 {
				t.Fatalf("invalid limits made requests: %d", requests)
			}
		})
	}
}

func TestPaginatedAPIsRejectUnboundOrUnexpectedSnapshots(t *testing.T) {
	for _, api := range pageAPIs() {
		for _, test := range []struct{ name, header, body, expected string }{
			{"missing-header", "", "snap", ""},
			{"missing-body", "snap", "", ""},
			{"mismatch", "other", "snap", ""},
			{"changed-snapshot", "snap", "snap", "previous"},
		} {
			t.Run(api.name+"/"+test.name, func(t *testing.T) {
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("X-RKC-Snapshot-ID", test.header)
					_ = json.NewEncoder(w).Encode(map[string]any{"snapshot_id": test.body, "items": []any{map[string]string{"id": "must-not-leak"}}, "hits": []any{map[string]any{"document": map[string]string{"id": "must-not-leak"}}}})
				}))
				defer host.Close()
				c, _ := New(host.URL)
				page, err := api.invoke(c, 1, "cursor", test.expected)
				if err == nil || !strings.Contains(err.Error(), "snapshot") || !reflect.ValueOf(page).IsZero() {
					t.Fatalf("unbound partial result exposed: %+v %v", page, err)
				}
			})
		}
	}
}

func TestPaginatedAPIsReturnCursorErrorsWithoutRestart(t *testing.T) {
	for _, api := range pageAPIs() {
		t.Run(api.name, func(t *testing.T) {
			requests := 0
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":400,"title":"Bad Request","detail":"cursor belongs to another dataset generation"}`))
			}))
			defer host.Close()
			c, _ := New(host.URL)
			page, err := api.invoke(c, 1, "stale-cursor", "snap")
			if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "another dataset generation") || !reflect.ValueOf(page).IsZero() || requests != 1 {
				t.Fatalf("cursor error changed or retried: %+v %v, %d requests", page, err, requests)
			}
		})
	}
}

func TestPaginatedAPIsRejectMalformedPageMetadata(t *testing.T) {
	for _, api := range pageAPIs() {
		for _, test := range []struct {
			name   string
			limit  int
			mutate func(map[string]any)
		}{
			{"missing-array", 1, func(p map[string]any) { delete(p, "items"); delete(p, "hits") }},
			{"null-array", 1, func(p map[string]any) { p["items"] = nil; p["hits"] = nil }},
			{"negative-total", 1, func(p map[string]any) { p["total"] = -1 }},
			{"under-count", 1, func(p map[string]any) { p["total"] = 0 }},
			{"excess-page", 1, func(p map[string]any) { p["items"] = []any{map[string]any{}, map[string]any{}}; p["hits"] = p["items"] }},
			{"excess-default-page", 0, func(p map[string]any) {
				p["total"] = 101
				p["items"] = make([]map[string]any, 101)
				p["hits"] = p["items"]
			}},
			{"missing-cursor", 1, func(p map[string]any) { delete(p, "next_cursor") }},
			{"unexpected-cursor", 1, func(p map[string]any) { p["truncated"] = false }},
			{"empty-continuing-page", 1, func(p map[string]any) { p["items"] = []any{}; p["hits"] = []any{} }},
			{"no-further-matches", 1, func(p map[string]any) { p["total"] = 1 }},
			{"oversized-cursor", 1, func(p map[string]any) { p["next_cursor"] = strings.Repeat("字", 1366) }},
			{"repeating-cursor", 1, func(p map[string]any) { p["next_cursor"] = "request-cursor" }},
		} {
			t.Run(api.name+"/"+test.name, func(t *testing.T) {
				payload := map[string]any{
					"items": []any{map[string]string{"id": "must-not-leak"}}, "hits": []any{map[string]any{"document": map[string]string{"id": "must-not-leak"}}},
					"total": 7, "truncated": true, "next_cursor": "next", "snapshot_id": "snap",
				}
				test.mutate(payload)
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("X-RKC-Snapshot-ID", "snap")
					_ = json.NewEncoder(w).Encode(payload)
				}))
				defer host.Close()
				c, _ := New(host.URL)
				page, err := api.invoke(c, test.limit, "request-cursor", "snap")
				if err == nil || !reflect.ValueOf(page).IsZero() {
					t.Fatalf("malformed page exposed: %+v %v", page, err)
				}
			})
		}
	}
}

func TestPaginatedAPIsTraverseRealServer(t *testing.T) {
	dataset := clientTestDataset()
	dataset.Bundle.Nodes = nil
	dataset.Bundle.Artifacts = nil
	dataset.Bundle.Edges = nil
	dataset.NodeByID = map[string]rkcmodel.Node{}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("record-%d", i)
		node := rkcmodel.Node{ID: id, Name: "query", QualifiedName: "pkg.query." + id, Kind: "function", Language: "go"}
		dataset.Bundle.Nodes = append(dataset.Bundle.Nodes, node)
		dataset.NodeByID[id] = node
		dataset.Bundle.Artifacts = append(dataset.Bundle.Artifacts, rkcmodel.Artifact{ID: id, Path: "src/" + id, Language: "go", Status: "syntax_parsed"})
		dataset.Bundle.Edges = append(dataset.Bundle.Edges, rkcmodel.Edge{ID: id, Kind: "calls", From: "a", To: "b"})
		dataset.Bundle.Diagnostics = append(dataset.Bundle.Diagnostics, rkcmodel.Diagnostic{ID: id, Code: "test", Severity: "warning"})
	}
	// Search indexes nodes only so all five traversals have the same seven IDs.
	dataset.Search = search.BuildFromBundle(rkcmodel.Bundle{Snapshot: dataset.Bundle.Snapshot, Nodes: dataset.Bundle.Nodes})
	host := httptest.NewServer(dataset.Handler())
	defer host.Close()
	c, _ := New(host.URL)
	apis := append(pageAPIs(), pageAPI{"ranked-nodes", "/api/v1/nodes", 1000, func(c *Client, limit int, cursor, snapshot string) (any, error) {
		return c.ListNodes(context.Background(), NodeListOptions{Query: "query", Kind: "function", Limit: limit, Cursor: cursor, ExpectedSnapshotID: snapshot})
	}})
	for _, api := range apis {
		t.Run(api.name, func(t *testing.T) {
			cursor, expectedSnapshot := "", ""
			seen := map[string]bool{}
			limit := 2
			for pageNumber := 0; ; pageNumber++ {
				if pageNumber >= 7 {
					t.Fatal("traversal failed to terminate")
				}
				result, err := api.invoke(c, limit, cursor, expectedSnapshot)
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				var total int
				var snapshot, next string
				switch page := result.(type) {
				case rkcapi.NodePage:
					total, snapshot, next = page.Total, page.SnapshotID, page.NextCursor
					for _, item := range page.Items {
						ids = append(ids, item.ID)
					}
				case rkcapi.ArtifactPage:
					total, snapshot, next = page.Total, page.SnapshotID, page.NextCursor
					for _, item := range page.Items {
						ids = append(ids, item.ID)
					}
				case rkcapi.EdgePage:
					total, snapshot, next = page.Total, page.SnapshotID, page.NextCursor
					for _, item := range page.Items {
						ids = append(ids, item.ID)
					}
				case rkcapi.DiagnosticPage:
					total, snapshot, next = page.Total, page.SnapshotID, page.NextCursor
					for _, item := range page.Items {
						ids = append(ids, item.ID)
					}
				case SearchResponse:
					total, snapshot, next = page.Total, page.SnapshotID, page.NextCursor
					for _, hit := range page.Hits {
						ids = append(ids, hit.Document.ID)
					}
				default:
					t.Fatalf("unexpected page type %T", result)
				}
				if total != 7 || snapshot != "snapshot-client" || len(ids) > limit {
					t.Fatalf("page metadata: %+v", result)
				}
				for _, id := range ids {
					if seen[id] {
						t.Fatalf("duplicate record %q", id)
					}
					seen[id] = true
				}
				if next == "" {
					break
				}
				cursor, expectedSnapshot, limit = next, snapshot, 3
			}
			if len(seen) != 7 {
				t.Fatalf("reached %d of seven records", len(seen))
			}
		})
	}
}
