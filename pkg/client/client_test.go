package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	searchindex "github.com/neuroforge-io/RKC/internal/search"
	rkcserver "github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type failingBody struct {
	closed bool
	err    error
}

func (body *failingBody) Read([]byte) (int, error) { return 0, body.err }
func (body *failingBody) Close() error {
	body.closed = true
	return nil
}

func TestNewValidationAndOptions(t *testing.T) {
	for _, raw := range []string{"", "localhost:8080", "ftp://example.test", "http:///missing-host", "https://", "://bad%"} {
		if _, err := New(raw); err == nil {
			t.Errorf("New(%q) succeeded, want error", raw)
		}
	}

	customHTTP := &http.Client{Timeout: 42 * time.Second}
	client, err := New(" https://example.test/rkc/ ", WithHTTPClient(customHTTP), WithBearerToken("  secret-token  "))
	if err != nil {
		t.Fatal(err)
	}
	if client.http != customHTTP || client.token != "secret-token" || client.baseURL.Host != "example.test" {
		t.Fatalf("New() options = %+v", client)
	}

	defaulted, err := New("http://example.test", WithHTTPClient(nil))
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.http == nil || defaulted.http.Timeout != 15*time.Second {
		t.Fatalf("default HTTP client = %+v", defaulted.http)
	}
}

func TestReadAPIEndpointsHeadersQueriesAndBasePath(t *testing.T) {
	t.Helper()
	type observedRequest struct {
		path    string
		query   url.Values
		headers http.Header
	}
	var mu sync.Mutex
	var observed []observedRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		observed = append(observed, observedRequest{path: request.URL.Path, query: request.URL.Query(), headers: request.Header.Clone()})
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		suffix := strings.TrimPrefix(request.URL.Path, "/prefix")
		switch {
		case suffix == "/api/v1/health":
			_, _ = io.WriteString(writer, `{"status":"ok","schema_version":"0.2.0","snapshot_id":"snap"}`)
		case suffix == "/api/v1/manifest":
			_, _ = io.WriteString(writer, `{"schema_version":"0.2.0","id":"snap","root_name":"repo","root_path":"","content_digest":"digest","git":{},"tool":{"name":"rkc","version":"test"}}`)
		case suffix == "/api/v1/coverage":
			_, _ = io.WriteString(writer, `{"snapshot_id":"snap","nodes_total":3}`)
		case strings.HasPrefix(suffix, "/api/v1/nodes/"):
			_, _ = io.WriteString(writer, `{"node":{"id":"node id","kind":"function","name":"Example"},"incoming_edges":[],"outgoing_edges":[],"evidence":[]}`)
		case suffix == "/api/v1/search":
			_, _ = io.WriteString(writer, `{"query":"hello world","hits":[{"document":{"id":"n","object_type":"node","kind":"function","title":"N"},"score":0.9,"reasons":["name"],"terms":["hello"]}],"truncated":false,"mode":"lexical","index_version":"1"}`)
		case suffix == "/api/v1/graph/neighborhood":
			_, _ = io.WriteString(writer, `{"seed_id":"node:1","nodes":[],"edges":[],"depth_by_id":{"node:1":0},"truncated":true}`)
		case suffix == "/api/v1/graph/path":
			_, _ = io.WriteString(writer, `{"found":true,"from_id":"a","to_id":"b","node_ids":["a","b"],"edge_ids":[],"nodes":[],"edges":[],"depth":1,"visited":2}`)
		case suffix == "/api/v1/impact":
			_, _ = io.WriteString(writer, `{"seed_id":"node:1","impacted_nodes":[],"impact_edges":[],"depth_by_id":{"node:1":0},"truncated":false}`)
		default:
			http.Error(writer, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := New(server.URL+"/prefix/?stale=query", WithHTTPClient(server.Client()), WithBearerToken(" token "))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	health, err := client.Health(ctx)
	if err != nil || health.Status != "ok" || health.SnapshotID != "snap" {
		t.Fatalf("Health() = %+v, %v", health, err)
	}
	manifest, err := client.Manifest(ctx)
	if err != nil || manifest.ID != "snap" || manifest.RootName != "repo" {
		t.Fatalf("Manifest() = %+v, %v", manifest, err)
	}
	coverage, err := client.Coverage(ctx)
	if err != nil || coverage.SnapshotID != "snap" || coverage.NodesTotal != 3 {
		t.Fatalf("Coverage() = %+v, %v", coverage, err)
	}
	node, err := client.Node(ctx, "node id")
	if err != nil || node.ID != "node id" {
		t.Fatalf("Node() = %+v, %v", node, err)
	}
	search, err := client.Search(ctx, "hello world", SearchOptions{Limit: 10, Kind: "function", Language: "go"})
	if err != nil || search.Count != 1 || len(search.Hits) != 1 || search.Hits[0].Document.ID != "n" ||
		len(search.Results) != 1 || search.Results[0].Node.ID != "n" || search.Mode != "lexical" || search.IndexVersion != "1" {
		t.Fatalf("Search() = %+v, %v", search, err)
	}
	neighborhood, err := client.Neighborhood(ctx, "node:1", NeighborhoodOptions{Hops: 2, Direction: "both", EdgeKinds: []string{"calls", "contains"}, Limit: 4})
	if err != nil || neighborhood.SeedID != "node:1" || neighborhood.Center != "node:1" || neighborhood.DepthByID["node:1"] != 0 || !neighborhood.Truncated {
		t.Fatalf("Neighborhood() = %+v, %v", neighborhood, err)
	}
	pathResponse, err := client.FindPath(ctx, "a", "b", []string{"calls", "contains"}, 5)
	if err != nil || !pathResponse.Found || pathResponse.FromID != "a" || pathResponse.ToID != "b" ||
		pathResponse.Depth != 1 || pathResponse.Visited != 2 || !reflect.DeepEqual(pathResponse.NodeIDs, []string{"a", "b"}) {
		t.Fatalf("FindPath() = %+v, %v", pathResponse, err)
	}
	impact, err := client.Impact(ctx, "node:1", ImpactOptions{Direction: "outgoing", EdgeKinds: []string{"calls"}, MaxDepth: 3, Limit: 7})
	if err != nil || impact.SeedID != "node:1" || impact.Root != "node:1" || impact.DepthByID["node:1"] != 0 {
		t.Fatalf("Impact() = %+v, %v", impact, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 8 {
		t.Fatalf("observed %d requests, want 8", len(observed))
	}
	for _, request := range observed {
		if request.headers.Get("Accept") != "application/json" || request.headers.Get("User-Agent") != "rkc-go-client/0.2" || request.headers.Get("Authorization") != "Bearer token" {
			t.Errorf("headers for %s = %v", request.path, request.headers)
		}
		if request.query.Has("stale") {
			t.Errorf("base URL query leaked into %s: %v", request.path, request.query)
		}
		if !strings.HasPrefix(request.path, "/prefix/api/v1/") {
			t.Errorf("base path not preserved: %s", request.path)
		}
	}
	assertQuery(t, observed[4].query, url.Values{"q": {"hello world"}, "limit": {"10"}, "kind": {"function"}, "language": {"go"}})
	assertQuery(t, observed[5].query, url.Values{"node_id": {"node:1"}, "hops": {"2"}, "direction": {"both"}, "edge_kinds": {"calls,contains"}, "limit": {"4"}})
	assertQuery(t, observed[6].query, url.Values{"from": {"a"}, "to": {"b"}, "edge_kinds": {"calls,contains"}, "max_depth": {"5"}})
	assertQuery(t, observed[7].query, url.Values{"node_id": {"node:1"}, "direction": {"outgoing"}, "edge_kinds": {"calls"}, "max_depth": {"3"}, "limit": {"7"}})
}

func TestReadAPIWireCompatibilityWithActualDatasetHandler(t *testing.T) {
	dataset := clientTestDataset()
	server := httptest.NewServer(dataset.Handler())
	defer server.Close()

	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	health, err := client.Health(ctx)
	if err != nil || health.Status != "ok" || health.SnapshotID != "snapshot-client" || health.SchemaVersion != rkcmodel.SchemaVersion {
		t.Fatalf("Health() = %+v, %v", health, err)
	}
	manifest, err := client.Manifest(ctx)
	if err != nil || manifest.ID != "snapshot-client" || manifest.RootName != "fixture" {
		t.Fatalf("Manifest() = %+v, %v", manifest, err)
	}
	coverage, err := client.Coverage(ctx)
	if err != nil || coverage.SnapshotID != "snapshot-client" || coverage.NodesTotal != 3 {
		t.Fatalf("Coverage() = %+v, %v", coverage, err)
	}

	node, err := client.Node(ctx, "a")
	if err != nil || node.ID != "a" || node.QualifiedName != "pkg.Alpha" {
		t.Fatalf("Node() = %+v, %v", node, err)
	}
	details, err := client.NodeDetails(ctx, "a")
	if err != nil || details.Node.ID != "a" || len(details.IncomingEdges) != 0 ||
		len(details.OutgoingEdges) != 1 || details.OutgoingEdges[0].ID != "edge-ab" ||
		len(details.Evidence) != 1 || details.Evidence[0].ID != "evidence-a" {
		t.Fatalf("NodeDetails() = %+v, %v", details, err)
	}

	search, err := client.Search(ctx, "Alpha", SearchOptions{Limit: 1, Kind: "function", Language: "go"})
	if err != nil || search.Query != "Alpha" || len(search.Hits) != 1 ||
		search.Hits[0].Document.ID != "a" || search.Hits[0].Document.ObjectType != "node" ||
		search.Hits[0].Document.QualifiedName != "pkg.Alpha" || search.Mode != "embedded-bm25-lexical" ||
		search.IndexVersion != searchindex.IndexVersion || search.Count != len(search.Hits) ||
		len(search.Results) != 1 || search.Results[0].Node.ID != "a" {
		t.Fatalf("Search() = %+v, %v", search, err)
	}

	neighborhood, err := client.Neighborhood(ctx, "a", NeighborhoodOptions{
		Hops: 2, Direction: "outgoing", EdgeKinds: []string{"calls"}, Limit: 10,
	})
	if err != nil || neighborhood.SeedID != "a" || neighborhood.Center != "a" ||
		neighborhood.DepthByID["a"] != 0 || neighborhood.DepthByID["b"] != 1 ||
		neighborhood.DepthByID["c"] != 2 || len(neighborhood.Nodes) != 3 ||
		len(neighborhood.Edges) != 2 {
		t.Fatalf("Neighborhood() = %+v, %v", neighborhood, err)
	}

	foundPath, err := client.FindPath(ctx, "a", "c", []string{"calls"}, 3)
	if err != nil || !foundPath.Found || foundPath.FromID != "a" || foundPath.ToID != "c" ||
		!reflect.DeepEqual(foundPath.NodeIDs, []string{"a", "b", "c"}) ||
		!reflect.DeepEqual(foundPath.EdgeIDs, []string{"edge-ab", "edge-bc"}) ||
		len(foundPath.Nodes) != 3 || len(foundPath.Edges) != 2 ||
		foundPath.Depth != 2 || foundPath.Visited != 3 {
		t.Fatalf("FindPath() = %+v, %v", foundPath, err)
	}

	impact, err := client.Impact(ctx, "c", ImpactOptions{
		Direction: "incoming", EdgeKinds: []string{"calls"}, MaxDepth: 3, Limit: 10,
	})
	if err != nil || impact.SeedID != "c" || impact.Root != "c" ||
		len(impact.ImpactedNodes) != 2 || len(impact.ImpactEdges) != 2 ||
		!reflect.DeepEqual(impact.Nodes, impact.ImpactedNodes) ||
		!reflect.DeepEqual(impact.Edges, impact.ImpactEdges) ||
		impact.DepthByID["c"] != 0 || impact.DepthByID["b"] != 1 || impact.DepthByID["a"] != 2 {
		t.Fatalf("Impact() = %+v, %v", impact, err)
	}
}

func TestOptionalQueryParametersAreOmitted(t *testing.T) {
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries = append(queries, request.URL.Query())
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{}`)
	}))
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.Search(ctx, "q", SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Neighborhood(ctx, "n", NeighborhoodOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FindPath(ctx, "a", "b", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Impact(ctx, "n", ImpactOptions{}); err != nil {
		t.Fatal(err)
	}
	want := []url.Values{{"q": {"q"}}, {"node_id": {"n"}}, {"from": {"a"}, "to": {"b"}}, {"node_id": {"n"}}}
	if !reflect.DeepEqual(queries, want) {
		t.Fatalf("optional queries = %v, want %v", queries, want)
	}
}

func TestHTTPErrorMessages(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "error field", status: http.StatusBadRequest, body: `{"error":"bad request"}`, want: "HTTP 400: bad request"},
		{name: "message field", status: http.StatusUnprocessableEntity, body: `{"message":"invalid query"}`, want: "HTTP 422: invalid query"},
		{name: "plain body", status: http.StatusInternalServerError, body: " backend unavailable \n", want: "HTTP 500: backend unavailable"},
		{name: "status fallback", status: http.StatusTeapot, body: "", want: "HTTP 418: 418 I'm a teapot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Health(context.Background())
			if err == nil || !strings.Contains(err.Error(), "RKC GET /api/v1/health") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Health() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMalformedJSONTransportReadAndContextErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "not-json")
	}))
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "decode RKC /api/v1/health response") {
		t.Fatalf("Health(malformed JSON) = %v", err)
	}
	server.Close()
	if _, err := client.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "RKC GET /api/v1/health") {
		t.Fatalf("Health(transport failure) = %v", err)
	}

	readFailure := errors.New("body read failed")
	body := &failingBody{err: readFailure}
	readClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: body}, nil
	})}
	client, err = New("http://example.test", WithHTTPClient(readClient))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Health(context.Background()); !errors.Is(err, readFailure) {
		t.Fatalf("Health(read failure) = %v", err)
	}
	if !body.closed {
		t.Fatal("response body was not closed after read failure")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	contextClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})}
	client, err = New("http://example.test", WithHTTPClient(contextClient))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Health(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Health(cancelled) = %v", err)
	}
}

func TestNodePreservesEscapedSlashAsOneOpaquePathSegment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"node":{"id":`+strconvQuote(request.PathValue("nodeID"))+`,"kind":"function","name":"N"},"incoming_edges":[],"outgoing_edges":[],"evidence":[]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	node, err := client.Node(context.Background(), "namespace/item")
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != "namespace/item" {
		t.Fatalf("Node() path value = %q, want opaque ID with slash", node.ID)
	}
}

func TestFirstSelectsFirstNonEmptyValue(t *testing.T) {
	if got := first("", "message", "later"); got != "message" {
		t.Fatalf("first() = %q", got)
	}
	if got := first("", ""); got != "unknown error" {
		t.Fatalf("first(empty) = %q", got)
	}
}

func assertQuery(t *testing.T, got, want url.Values) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query = %v, want %v", got, want)
	}
}

func strconvQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func clientTestDataset() *rkcserver.Dataset {
	artifact := rkcmodel.Artifact{
		ID: "artifact-a", Path: "src/a.go", Kind: "file", Language: "go",
		Status: "syntax_parsed", Text: true,
	}
	evidence := rkcmodel.Evidence{
		ID: "evidence-a", Kind: "syntax_inferred", Method: "test", Confidence: 1,
		Source: &rkcmodel.SourceRange{
			ArtifactID: artifact.ID, Path: artifact.Path, StartLine: 1, EndLine: 1,
		},
	}
	a := rkcmodel.Node{
		ID: "a", Kind: "function", Name: "Alpha", QualifiedName: "pkg.Alpha",
		Language: "go", ArtifactID: artifact.ID, EvidenceIDs: []string{evidence.ID},
	}
	b := rkcmodel.Node{
		ID: "b", Kind: "function", Name: "Beta", QualifiedName: "pkg.Beta",
		Language: "go", ArtifactID: artifact.ID,
	}
	c := rkcmodel.Node{
		ID: "c", Kind: "function", Name: "Gamma", QualifiedName: "pkg.Gamma",
		Language: "go", ArtifactID: artifact.ID,
	}
	edges := []rkcmodel.Edge{
		{ID: "edge-ab", Kind: "calls", From: "a", To: "b", Resolution: "declared", Confidence: 1},
		{ID: "edge-bc", Kind: "calls", From: "b", To: "c", Resolution: "declared", Confidence: 1},
	}
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			ID: "snapshot-client", SchemaVersion: rkcmodel.SchemaVersion,
			RootName: "fixture", RootPath: "", ContentDigest: "digest",
			Tool: rkcmodel.ToolInfo{Name: "rkc", Version: "test"},
		},
		Artifacts: []rkcmodel.Artifact{artifact},
		Nodes:     []rkcmodel.Node{a, b, c},
		Edges:     edges,
		Evidence:  []rkcmodel.Evidence{evidence},
	}
	return &rkcserver.Dataset{
		Manifest:     bundle.Snapshot,
		Coverage:     rkcmodel.BuildCoverage(bundle),
		Bundle:       bundle,
		NodeByID:     map[string]rkcmodel.Node{"a": a, "b": b, "c": c},
		ArtifactByID: map[string]rkcmodel.Artifact{artifact.ID: artifact},
		EvidenceByID: map[string]rkcmodel.Evidence{evidence.ID: evidence},
		Graph:        graphindex.Build(bundle.Nodes, bundle.Edges),
		Search:       searchindex.BuildFromBundle(bundle),
		Integrity:    rkcserver.IntegrityVerified,
		LoadedAt:     time.Unix(1, 0).UTC(),
	}
}
