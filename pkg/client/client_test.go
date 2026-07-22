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
			_, _ = io.WriteString(writer, `{"id":"node id","kind":"function","name":"Example"}`)
		case suffix == "/api/v1/search":
			_, _ = io.WriteString(writer, `{"query":"hello world","count":1,"results":[{"node":{"id":"n","kind":"function","name":"N"},"score":0.9,"reasons":["name"]}]}`)
		case suffix == "/api/v1/graph/neighborhood":
			_, _ = io.WriteString(writer, `{"center":"node:1","nodes":[],"edges":[],"truncated":true}`)
		case suffix == "/api/v1/graph/path":
			_, _ = io.WriteString(writer, `{"found":true,"node_ids":["a","b"],"edges":[]}`)
		case suffix == "/api/v1/impact":
			_, _ = io.WriteString(writer, `{"root":"node:1","nodes":[],"edges":[],"truncated":false}`)
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
	if err != nil || search.Count != 1 || len(search.Results) != 1 || search.Results[0].Node.ID != "n" {
		t.Fatalf("Search() = %+v, %v", search, err)
	}
	neighborhood, err := client.Neighborhood(ctx, "node:1", NeighborhoodOptions{Hops: 2, Direction: "both", EdgeKinds: []string{"calls", "contains"}, Limit: 4})
	if err != nil || neighborhood.Center != "node:1" || !neighborhood.Truncated {
		t.Fatalf("Neighborhood() = %+v, %v", neighborhood, err)
	}
	pathResponse, err := client.FindPath(ctx, "a", "b", []string{"calls", "contains"}, 5)
	if err != nil || !pathResponse.Found || !reflect.DeepEqual(pathResponse.NodeIDs, []string{"a", "b"}) {
		t.Fatalf("FindPath() = %+v, %v", pathResponse, err)
	}
	impact, err := client.Impact(ctx, "node:1", ImpactOptions{Direction: "outgoing", EdgeKinds: []string{"calls"}, MaxDepth: 3, Limit: 7})
	if err != nil || impact.Root != "node:1" {
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
		_, _ = io.WriteString(writer, `{"id":`+strconvQuote(request.PathValue("nodeID"))+`,"kind":"function","name":"N"}`)
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
