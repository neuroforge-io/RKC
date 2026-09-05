package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/search"
)

func paginationDataset(count int) *Dataset {
	dataset := testDataset()
	dataset.Bundle.Nodes = nil
	dataset.Bundle.Artifacts = nil
	dataset.Bundle.Edges = nil
	dataset.Bundle.Diagnostics = nil
	dataset.NodeByID = map[string]model.Node{}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("id-%06d", i)
		kind := "function"
		if i%3 == 0 {
			kind = "type"
		}
		node := model.Node{ID: id, Name: "shared", QualifiedName: "shared." + id, Kind: kind, Language: "go"}
		dataset.Bundle.Nodes = append(dataset.Bundle.Nodes, node)
		dataset.NodeByID[id] = node
		dataset.Bundle.Artifacts = append(dataset.Bundle.Artifacts, model.Artifact{ID: "artifact-" + id, Path: "src/" + id, Language: "go", Status: "parsed"})
		dataset.Bundle.Edges = append(dataset.Bundle.Edges, model.Edge{ID: id, Kind: "calls", From: "a", To: "b", Resolution: "declared"})
		dataset.Bundle.Diagnostics = append(dataset.Bundle.Diagnostics, model.Diagnostic{ID: id, Severity: "warning", Code: "example"})
	}
	dataset.Search = search.BuildFromBundle(dataset.Bundle)
	dataset.pagination = newPaginationState(dataset.Search)
	return dataset
}

type testPage struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
	Hits       []search.Hit     `json:"hits"`
	Total      int              `json:"total"`
	Truncated  bool             `json:"truncated"`
	SnapshotID string           `json:"snapshot_id"`
	NextCursor string           `json:"next_cursor"`
	Retrieval  *search.Response `json:"retrieval"`
}

func readTestPage(t *testing.T, handler http.Handler, path string) testPage {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != 200 {
		t.Fatalf("page %s: %d %s", path, response.Code, response.Body.String())
	}
	var page testPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.SnapshotID != "snapshot" || response.Header().Get(snapshotGenerationHeader) != page.SnapshotID {
		t.Fatalf("snapshot binding: %+v", page)
	}
	if page.Truncated != (page.NextCursor != "") {
		t.Fatalf("continuation disagreement: %+v", page)
	}
	if len(page.NextCursor) > maximumPageCursorBytes {
		t.Fatalf("oversize cursor: %d", len(page.NextCursor))
	}
	return page
}

func TestCollectionPagingReachesEveryRecord(t *testing.T) {
	dataset := paginationDataset(1237)
	handler := dataset.Handler()
	for _, endpoint := range []string{"nodes", "artifacts", "edges", "diagnostics"} {
		t.Run(endpoint, func(t *testing.T) {
			values := url.Values{"limit": {"127"}}
			seen := map[string]bool{}
			pageCount := 0
			for {
				page := readTestPage(t, handler, "/api/v1/"+endpoint+"?"+values.Encode())
				if page.Total != 1237 || len(page.Items) == 0 {
					t.Fatalf("page counts: %+v", page)
				}
				for _, item := range page.Items {
					if seen[item.ID] {
						t.Fatalf("duplicate %q", item.ID)
					}
					expected := fmt.Sprintf("id-%06d", len(seen))
					if endpoint == "artifacts" {
						expected = "artifact-" + expected
					}
					if item.ID != expected {
						t.Fatalf("order %q want %q", item.ID, expected)
					}
					seen[item.ID] = true
				}
				pageCount++
				if pageCount > 30 {
					t.Fatal("pagination did not terminate")
				}
				if page.NextCursor == "" {
					break
				}
				values.Set("cursor", page.NextCursor)
				values.Set("limit", "73")
			}
			if len(seen) != 1237 {
				t.Fatalf("reached %d records", len(seen))
			}
		})
	}
}

func TestPagingSparseFilterTotalsAndExactEnd(t *testing.T) {
	handler := paginationDataset(12).Handler()
	first := readTestPage(t, handler, "/api/v1/nodes?kind=type&limit=2")
	if first.Total != 4 || !first.Truncated || first.Items[0].ID != "id-000000" {
		t.Fatalf("first: %+v", first)
	}
	second := readTestPage(t, handler, "/api/v1/nodes?kind=type&limit=2&cursor="+url.QueryEscape(first.NextCursor))
	if second.Total != 4 || second.Truncated || len(second.Items) != 2 || second.Items[1].ID != "id-000009" {
		t.Fatalf("last: %+v", second)
	}
	empty := readTestPage(t, handler, "/api/v1/nodes?kind=unknown")
	if empty.Total != 0 || empty.Items == nil || len(empty.Items) != 0 || empty.Truncated {
		t.Fatalf("empty: %+v", empty)
	}
}

func TestRankedPagingReachesEveryResult(t *testing.T) {
	dataset := paginationDataset(1237)
	handler := dataset.Handler()
	for _, endpoint := range []string{"search", "nodes"} {
		t.Run(endpoint, func(t *testing.T) {
			values := url.Values{"q": {"shared"}, "limit": {"97"}}
			if endpoint == "search" {
				values.Set("object_types", "node")
			}
			var ids []string
			var firstHits []search.Hit
			for pageIndex := 0; ; pageIndex++ {
				if pageIndex > 30 {
					t.Fatal("paging did not terminate")
				}
				page := readTestPage(t, handler, "/api/v1/"+endpoint+"?"+values.Encode())
				if page.Total != 1237 {
					t.Fatalf("total %d", page.Total)
				}
				if endpoint == "nodes" {
					if page.Retrieval == nil {
						t.Fatal("lost retrieval metadata")
					}
					page.Hits = page.Retrieval.Hits
					if len(page.Items) != len(page.Hits) {
						t.Fatal("lost node records")
					}
				}
				firstHits = append(firstHits, page.Hits...)
				for _, hit := range page.Hits {
					ids = append(ids, hit.Document.ID)
				}
				if page.NextCursor == "" {
					break
				}
				values.Set("cursor", page.NextCursor)
				values.Set("limit", "61")
			}
			if len(ids) != 1237 {
				t.Fatalf("reached %d results", len(ids))
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if seen[id] {
					t.Fatalf("duplicate %q", id)
				}
				seen[id] = true
			}
			expected := dataset.Search.Search(search.Query{Text: "shared", ObjectTypes: map[string]struct{}{"node": {}}, Limit: 1000})
			if !reflect.DeepEqual(firstHits[:1000], expected.Hits) {
				t.Fatal("paged ranking diverged from unpaged results")
			}
		})
	}
}

func TestCursorsRejectTamperingAndScopeChanges(t *testing.T) {
	dataset := paginationDataset(12)
	handler := dataset.Handler()
	first := readTestPage(t, handler, "/api/v1/nodes?kind=type&limit=1")
	cursor := first.NextCursor
	for _, path := range []string{
		"/api/v1/nodes?kind=function&cursor=" + url.QueryEscape(cursor),
		"/api/v1/artifacts?cursor=" + url.QueryEscape(cursor),
		"/api/v1/nodes?q=shared&kind=type&cursor=" + url.QueryEscape(cursor),
		"/api/v1/nodes?kind=type&cursor=" + url.QueryEscape("x"+cursor),
		"/api/v1/nodes?kind=type&cursor=" + url.QueryEscape(cursor+"x"),
		"/api/v1/nodes?kind=type&cursor=" + url.QueryEscape(strings.Repeat("x", 4097)),
		"/api/v1/nodes?kind=type&cursor=" + url.QueryEscape(cursor) + "&cursor=" + url.QueryEscape(cursor),
		"/api/v1/nodes?unknown=yes", "/api/v1/nodes?kind=%zz", "/api/v1/nodes?kind=%ff",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 400 {
			t.Fatalf("accepted invalid page %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	other := paginationDataset(12).Handler()
	response := httptest.NewRecorder()
	other.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/nodes?kind=type&cursor="+url.QueryEscape(cursor), nil))
	if response.Code != 400 {
		t.Fatalf("same-ID replacement accepted old cursor: %d", response.Code)
	}
	// Old pages remain reproducible within the same immutable generation.
	repeated := readTestPage(t, handler, "/api/v1/nodes?kind=type&limit=1")
	if !reflect.DeepEqual(first, repeated) {
		t.Fatal("immutable first page changed")
	}
}

func TestPagingLargeIdentityKeepsCursorBounded(t *testing.T) {
	dataset := paginationDataset(3)
	for i := range dataset.Bundle.Nodes {
		node := &dataset.Bundle.Nodes[i]
		delete(dataset.NodeByID, node.ID)
		node.ID = strings.Repeat("x", 6000) + strconv.Itoa(i)
		node.QualifiedName = strings.Repeat("y", 7000)
		dataset.NodeByID[node.ID] = *node
	}
	dataset.Search = search.BuildFromBundle(dataset.Bundle)
	dataset.pagination = newPaginationState(dataset.Search)
	handler := dataset.Handler()
	first := readTestPage(t, handler, "/api/v1/search?q=shared&object_types=node&limit=1")
	if first.NextCursor == "" || len(first.NextCursor) > 1024 {
		t.Fatalf("identity escaped into cursor (%d bytes)", len(first.NextCursor))
	}
	second := readTestPage(t, handler, "/api/v1/search?q=shared&object_types=node&limit=1&cursor="+url.QueryEscape(first.NextCursor))
	if second.Hits[0].Document.ID == first.Hits[0].Document.ID {
		t.Fatal("large identity did not advance")
	}
}

func TestPagingStopsCanceledRequest(t *testing.T) {
	dataset := paginationDataset(12)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, path := range []string{"/api/v1/nodes?limit=1", "/api/v1/search?q=shared&limit=1"} {
		response := httptest.NewRecorder()
		dataset.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		if response.Code == 200 {
			t.Fatal("canceled page succeeded")
		}
	}
}

func TestPagingRejectsSameIDWorkbenchActivation(t *testing.T) {
	firstDataset := paginationDataset(12)
	firstDataset.staticSiteTrusted = true
	workbench := &Workbench{}
	handler := firstDataset.HandlerWithWorkbench(workbench)
	for _, endpoint := range []string{"nodes?", "search?q=shared&object_types=node&"} {
		workbench.mu.Lock()
		workbench.activeDataset = firstDataset
		workbench.mu.Unlock()
		page := readTestPage(t, handler, "/api/v1/"+endpoint+"limit=1")
		replacement := paginationDataset(12)
		workbench.mu.Lock()
		workbench.activeDataset = replacement
		workbench.mu.Unlock()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/"+endpoint+"limit=1&cursor="+url.QueryEscape(page.NextCursor), nil))
		if response.Code != 400 {
			t.Fatalf("same-ID activation retained cursor for %s: %d", endpoint, response.Code)
		}
	}
}

func TestPagingConcurrentReadersShareImmutableGeneration(t *testing.T) {
	dataset := paginationDataset(24)
	handler := dataset.Handler()
	for i := 0; i < 12; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			first := readTestPage(t, handler, "/api/v1/search?q=shared&object_types=node&limit=3")
			next := readTestPage(t, handler, "/api/v1/search?q=shared&object_types=node&limit=3&cursor="+url.QueryEscape(first.NextCursor))
			if next.Hits[0].Document.ID == first.Hits[0].Document.ID || next.Total != 24 {
				t.Fatal("concurrent generation did not advance")
			}
		})
	}
}

func BenchmarkCollectionPageContinuation(b *testing.B) {
	records := make([]int, 250000)
	for i := range records {
		records[i] = i
	}
	dataset := testDataset()
	dataset.pagination = newPaginationState(dataset.Search)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/nodes?limit=100", nil)
	parsed, err := dataset.parsePageRequest(request, "collection")
	if err != nil {
		b.Fatal(err)
	}
	match := func(value int) bool { return value%7 == 0 }
	first, err := collectionPage(dataset, request, parsed, records, 100, match)
	if err != nil {
		b.Fatal(err)
	}
	cursor, err := decodePageCursor(first.NextCursor)
	if err != nil {
		b.Fatal(err)
	}
	continued := parsed
	continued.cursor = &cursor
	for _, test := range []struct {
		name   string
		parsed pageRequest
	}{{"first_page_counts_all", parsed}, {"continuation_resumes_scan", continued}} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				page, err := collectionPage(dataset, request, test.parsed, records, 100, match)
				if err != nil || len(page.Items) != 100 || page.Total != 35715 || !page.Truncated {
					b.Fatalf("page contract failed: %+v %v", page, err)
				}
			}
		})
	}
}
