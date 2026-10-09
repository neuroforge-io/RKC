package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const fictionalInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fictional-lantern-client","version":"test"}}}`

// All records are invented in memory. No user document or filesystem source is
// consulted; command-level tests separately exercise verified atlas loading.
func fictionalHTTPDataset() *server.Dataset {
	text := "Lantern Library lends paper books for 14 days."
	digest := sha256.Sum256([]byte(text))
	artifact := rkcmodel.Artifact{ID: "fictional-artifact", Path: "handbook.md", Kind: "file", Language: "markdown", Status: "parsed", Text: true, SHA256: hex.EncodeToString(digest[:])}
	source := &rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path, StartLine: 5, EndLine: 9}
	evidence := rkcmodel.Evidence{ID: "fictional-evidence", Kind: "declared", Method: "fictional-section", Tool: "fictional-fixture", Confidence: 1, Source: source, InputDigest: artifact.SHA256, Detail: "Lending period"}
	node := rkcmodel.Node{ID: "fictional-node", Kind: "document_section", Name: "Lending period", QualifiedName: "handbook.md#lending-period", ArtifactID: artifact.ID, Language: "markdown", Source: source, EvidenceIDs: []string{evidence.ID}, Attributes: map[string]any{"description": text}}
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{SchemaVersion: rkcmodel.SchemaVersion, ID: "fictional-snapshot", ContentDigest: artifact.SHA256}, Nodes: []rkcmodel.Node{node}, Artifacts: []rkcmodel.Artifact{artifact}, Evidence: []rkcmodel.Evidence{evidence}}
	return &server.Dataset{
		Manifest: bundle.Snapshot, Coverage: rkcmodel.BuildCoverage(bundle), Bundle: bundle,
		NodeByID: map[string]rkcmodel.Node{node.ID: node}, ArtifactByID: map[string]rkcmodel.Artifact{artifact.ID: artifact}, EvidenceByID: map[string]rkcmodel.Evidence{evidence.ID: evidence},
		Graph: graphindex.Build(bundle.Nodes, bundle.Edges), Search: search.BuildFromBundle(bundle), Integrity: server.IntegrityVerified,
	}
}

func fictionalHTTPTransport() *httpTransport {
	return &httpTransport{authority: "127.0.0.1:8712", slots: make(chan struct{}, 2), dispatch: New(fictionalHTTPDataset(), "fictional-test").handle}
}

func fictionalHTTPRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Host, request.RemoteAddr = "127.0.0.1:8712", "127.0.0.1:10001"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	return request
}

func TestHTTPInitializeNotificationsAndReadOnlyDiscovery(t *testing.T) {
	transport := fictionalHTTPTransport()
	for _, protocol := range []string{"2025-03-26", "2025-06-18", ProtocolVersion, "fictional-unsupported-version"} {
		t.Run(protocol, func(t *testing.T) {
			body := strings.Replace(fictionalInitialize, ProtocolVersion, protocol, 1)
			response := httptest.NewRecorder()
			transport.ServeHTTP(response, fictionalHTTPRequest(body))
			want := protocol
			if !supportedHTTPVersion(want) {
				want = ProtocolVersion
			}
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"protocolVersion":"`+want+`"`) || !strings.Contains(response.Body.String(), `"name":"rkc-mcp"`) {
				t.Fatalf("initialize status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("MCP-Session-Id") != "" || response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected session or content headers: %v", response.Header())
			}
		})
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_meta":{"progressToken":%q}}}`, "fictional-token"),
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"fictional cancellation"}}`,
	} {
		response := httptest.NewRecorder()
		transport.ServeHTTP(response, fictionalHTTPRequest(body))
		if response.Code != 202 || response.Body.Len() != 0 {
			t.Fatalf("notification status=%d body=%s", response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	transport.ServeHTTP(response, fictionalHTTPRequest(`{"jsonrpc":"2.0","id":"tools","method":"tools/list"}`))
	var message struct {
		Result struct {
			Tools []toolDefinition `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(message.Result.Tools) == 0 {
		t.Fatalf("tools status=%d body=%s", response.Code, response.Body.String())
	}
	for _, tool := range message.Result.Tools {
		if !tool.Annotations["readOnlyHint"] || tool.Annotations["destructiveHint"] || !tool.Annotations["idempotentHint"] || tool.Annotations["openWorldHint"] {
			t.Fatalf("tool effects are not bounded read-only: %+v", tool)
		}
	}
}

func TestHTTPRejectsAuthorityOriginsCredentialsAndUnsupportedMethods(t *testing.T) {
	cases := []struct {
		name string
		edit func(*http.Request)
		code int
	}{
		{"DNS host", func(r *http.Request) { r.Host = "localhost:8712" }, 403},
		{"spoof host", func(r *http.Request) { r.Host = "fictional.example:8712" }, 403},
		{"wrong port", func(r *http.Request) { r.Host = "127.0.0.1:8713" }, 403},
		{"remote peer", func(r *http.Request) { r.RemoteAddr = "192.0.2.10:10001" }, 403},
		{"missing peer", func(r *http.Request) { r.RemoteAddr = "" }, 403},
		{"remote origin", func(r *http.Request) { r.Header.Set("Origin", "https://fictional.example") }, 403},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"duplicate origin", func(r *http.Request) { r.Header["Origin"] = []string{"http://127.0.0.1:8712", "http://127.0.0.1:8712"} }, 403},
		{"authorization", func(r *http.Request) { r.Header.Set("Authorization", "Bearer fictional-no-credential") }, 403},
		{"empty authorization", func(r *http.Request) { r.Header["Authorization"] = []string{""} }, 403},
		{"proxy authorization", func(r *http.Request) { r.Header.Set("Proxy-Authorization", "fictional") }, 403},
		{"cookie", func(r *http.Request) { r.Header.Set("Cookie", "fictional=fictional") }, 403},
		{"forwarded", func(r *http.Request) { r.Header.Set("Forwarded", "host=fictional.example") }, 403},
		{"x forwarded", func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "fictional.example") }, 403},
		{"GET", func(r *http.Request) { r.Method = http.MethodGet }, 405},
		{"DELETE", func(r *http.Request) { r.Method = http.MethodDelete }, 405},
		{"wrong path", func(r *http.Request) { r.URL.Path = "/" }, 404},
		{"query", func(r *http.Request) { r.URL.RawQuery = "fictional=1" }, 404},
		{"encoded path", func(r *http.Request) { r.URL.RawPath = "/%6dcp" }, 404},
		{"absolute URI", func(r *http.Request) { r.URL.Scheme, r.URL.Host = "http", r.Host }, 404},
		{"session", func(r *http.Request) { r.Header.Set("MCP-Session-Id", "fictional") }, 400},
		{"unsupported version", func(r *http.Request) { r.Header.Set("MCP-Protocol-Version", "fictional") }, 400},
		{"empty version", func(r *http.Request) { r.Header["Mcp-Protocol-Version"] = []string{""} }, 400},
		{"duplicate version", func(r *http.Request) { r.Header.Add("MCP-Protocol-Version", ProtocolVersion) }, 400},
		{"JSON only", func(r *http.Request) { r.Header.Set("Accept", "application/json") }, 406},
		{"unacceptable SSE", func(r *http.Request) { r.Header.Set("Accept", "application/json, text/event-stream;q=0") }, 406},
		{"NaN quality", func(r *http.Request) { r.Header.Set("Accept", "application/json, text/event-stream;q=NaN") }, 406},
		{"wrong content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"compressed body", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 415},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := fictionalHTTPRequest(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
			test.edit(request)
			response := httptest.NewRecorder()
			fictionalHTTPTransport().ServeHTTP(response, request)
			if response.Code != test.code || strings.Contains(response.Body.String(), "fictional-no-credential") {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.code, response.Body.String())
			}
		})
	}
	for _, edit := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("MCP-Protocol-Version") },
		func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:8712") },
		func(r *http.Request) { r.Header.Set("Accept", "application/json;q=1.000, text/event-stream;q=0.5") },
	} {
		request := fictionalHTTPRequest(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		edit(request)
		response := httptest.NewRecorder()
		fictionalHTTPTransport().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("valid local request status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestHTTPStrictJSONRequestAndInitialization(t *testing.T) {
	cases := []struct {
		name, body string
		code, rpc  int
	}{
		{"syntax", `{`, 400, -32700},
		{"trailing value", `{"jsonrpc":"2.0","id":1,"method":"ping"} {}`, 400, -32700},
		{"array batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, 400, -32600},
		{"null envelope", `null`, 400, -32600},
		{"duplicate method", `{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/list"}`, 400, -32600},
		{"case method", `{"jsonrpc":"2.0","id":1,"Method":"ping"}`, 400, -32600},
		{"case duplicate", `{"jsonrpc":"2.0","id":1,"method":"ping","METHOD":"tools/list"}`, 400, -32600},
		{"unknown field", `{"jsonrpc":"2.0","id":1,"method":"ping","extra":true}`, 400, -32600},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, 400, -32600},
		{"null ID", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, 400, -32600},
		{"object ID", `{"jsonrpc":"2.0","id":{},"method":"ping"}`, 400, -32600},
		{"boolean ID", `{"jsonrpc":"2.0","id":true,"method":"ping"}`, 400, -32600},
		{"fraction ID", `{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, 400, -32600},
		{"large ID", `{"jsonrpc":"2.0","id":9223372036854775808,"method":"ping"}`, 400, -32600},
		{"null params", `{"jsonrpc":"2.0","id":1,"method":"ping","params":null}`, 400, -32600},
		{"array params", `{"jsonrpc":"2.0","id":1,"method":"ping","params":[]}`, 400, -32600},
		{"nested duplicates", `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"fictional":1,"fictional":2}}}`, 400, -32600},
		{"request without ID", `{"jsonrpc":"2.0","method":"ping"}`, 400, 0},
		{"notification with ID", `{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}`, 400, 0},
		{"initialized null", `{"jsonrpc":"2.0","method":"notifications/initialized","params":null}`, 400, -32600},
		{"initialized case meta", `{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_META":{}}}`, 400, 0},
		{"missing init params", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, 200, -32602},
		{"empty init params", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, 200, -32602},
		{"null capabilities", strings.Replace(fictionalInitialize, `"capabilities":{}`, `"capabilities":null`, 1), 200, -32602},
		{"protocol control", strings.Replace(fictionalInitialize, ProtocolVersion, `fictional\nversion`, 1), 200, -32602},
		{"protocol invisible", strings.Replace(fictionalInitialize, ProtocolVersion, `fictional\u200bversion`, 1), 200, -32602},
		{"protocol line separator", strings.Replace(fictionalInitialize, ProtocolVersion, `fictional\u2028version`, 1), 200, -32602},
		{"invalid client", strings.Replace(fictionalInitialize, `"name":"fictional-lantern-client"`, `"name":""`, 1), 200, -32602},
		{"client control", strings.Replace(fictionalInitialize, `"version":"test"`, `"version":"te\nst"`, 1), 200, -32602},
		{"initialize metadata null", strings.Replace(fictionalInitialize, `"capabilities":{}`, `"_meta":null,"capabilities":{}`, 1), 200, -32602},
		{"tools case name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"NAME":"rkc.search","arguments":{"query":"Lantern"}}}`, 200, -32602},
		{"resource case URI", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"URI":"rkc://snapshot/manifest"}}`, 200, -32602},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"fictional/mutate","params":{}}`, 200, -32601},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rkc.refresh","arguments":{}}}`, 200, -32602},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			fictionalHTTPTransport().ServeHTTP(response, fictionalHTTPRequest(test.body))
			if response.Code != test.code {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.code, response.Body.String())
			}
			if test.rpc != 0 {
				var result struct{ Error *rpcError }
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Error == nil || result.Error.Code != test.rpc {
					t.Fatalf("error code want=%d body=%s decode=%v", test.rpc, response.Body.String(), err)
				}
			}
		})
	}
	response := httptest.NewRecorder()
	fictionalHTTPTransport().ServeHTTP(response, fictionalHTTPRequest(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"fictional":1e400}}}`))
	if response.Code != 200 {
		t.Fatalf("valid JSON extension number rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	deep := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"fictional":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + `}}}`
	for _, body := range []string{deep, `{"jsonrpc":"2.0","id":"` + strings.Repeat("a", 257) + `","method":"ping"}`} {
		response := httptest.NewRecorder()
		fictionalHTTPTransport().ServeHTTP(response, fictionalHTTPRequest(body))
		if response.Code != 400 {
			t.Fatalf("bounded validation status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestHTTPBoundsBodyResponseDeadlineAndConcurrentWork(t *testing.T) {
	response := httptest.NewRecorder()
	fictionalHTTPTransport().ServeHTTP(response, fictionalHTTPRequest(strings.Repeat("x", maximumRequestBytes+1)))
	if response.Code != 413 {
		t.Fatalf("body limit status=%d", response.Code)
	}
	transport := fictionalHTTPTransport()
	transport.dispatch = func(context.Context, string, json.RawMessage) (any, *rpcError) {
		return strings.Repeat("x", maximumResponseBytes), nil
	}
	response = httptest.NewRecorder()
	transport.ServeHTTP(response, fictionalHTTPRequest(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if response.Body.Len() >= maximumResponseBytes || !strings.Contains(response.Body.String(), "response too large") {
		t.Fatalf("response limit bytes=%d", response.Body.Len())
	}

	started, deadline, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	transport.slots = make(chan struct{}, 1)
	transport.dispatch = func(ctx context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		close(started)
		<-ctx.Done()
		close(deadline)
		// Deliberately retain the worker after its response deadline, as an
		// existing synchronous index loop can. The permit must remain occupied.
		<-release
		return map[string]any{}, nil
	}
	handler := http.TimeoutHandler(transport, 50*time.Millisecond, "bounded deadline")
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(first, fictionalHTTPRequest(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not start")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not bound the response")
	}
	select {
	case <-deadline:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch context did not expire")
	}
	if first.Code != 503 {
		t.Fatalf("deadline status=%d", first.Code)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, fictionalHTTPRequest(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	if second.Code != 429 {
		t.Fatalf("timed-out work lost its concurrency permit: status=%d", second.Code)
	}
}

func TestHTTPListenBoundsAndVerifiedPinnedAtlas(t *testing.T) {
	valid := HTTPOptions{Listen: "127.0.0.1:0", RequestTimeout: time.Second, MaximumConcurrent: 2}
	for _, listen := range []string{"localhost:0", "0.0.0.0:0", "127.0.0.2:0", "[::]:0", "[::ffff:127.0.0.1]:0", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1:000", "http://127.0.0.1:0"} {
		options := valid
		options.Listen = listen
		if err := ValidateHTTPOptions(options); err == nil {
			t.Fatalf("unsafe listen accepted: %s", listen)
		}
	}
	for _, bounds := range []HTTPOptions{
		{Listen: valid.Listen, RequestTimeout: 0, MaximumConcurrent: 2},
		{Listen: valid.Listen, RequestTimeout: 31 * time.Second, MaximumConcurrent: 2},
		{Listen: valid.Listen, RequestTimeout: time.Second, MaximumConcurrent: 0},
		{Listen: valid.Listen, RequestTimeout: time.Second, MaximumConcurrent: 9},
	} {
		if err := ValidateHTTPOptions(bounds); err == nil {
			t.Fatalf("unbounded options accepted: %+v", bounds)
		}
	}
	if err := ValidateHTTPOptions(HTTPOptions{Listen: "[::1]:0", RequestTimeout: time.Second, MaximumConcurrent: 1}); err != nil {
		t.Fatal(err)
	}
	unverified, unnamed := fictionalHTTPDataset(), fictionalHTTPDataset()
	unverified.Integrity, unnamed.Manifest.ID = server.IntegrityLegacyUnverified, ""
	for _, candidate := range []*Server{nil, New(nil, "test"), New(unverified, "test"), New(unnamed, "test"), {dataset: fictionalHTTPDataset(), workspace: &workspaceServer{}}} {
		if adapter, err := candidate.ListenHTTP(valid); err == nil || adapter != nil {
			t.Fatalf("unverified or mutable atlas accepted: %v", candidate)
		}
	}
	adapter, err := New(fictionalHTTPDataset(), "fictional-test").ListenHTTP(valid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(adapter.Endpoint(), "http://127.0.0.1:") || !strings.HasSuffix(adapter.Endpoint(), "/mcp") {
		t.Fatalf("unsafe endpoint=%q", adapter.Endpoint())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := adapter.Serve(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPLoopbackNoviceQueryAndInspectEvidence(t *testing.T) {
	dataset := fictionalHTTPDataset()
	before, err := json.Marshal(dataset.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(dataset, "fictional-test").ListenHTTP(HTTPOptions{Listen: "127.0.0.1:0", RequestTimeout: time.Second, MaximumConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- adapter.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("HTTP shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("HTTP listener did not close")
		}
	})
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(func() { client.CloseIdleConnections() })
	post := func(body string, want int) []byte {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, adapter.Endpoint(), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
		if err != nil || response.StatusCode != want {
			t.Fatalf("POST status=%d want=%d body=%s error=%v", response.StatusCode, want, data, err)
		}
		return data
	}
	post(fictionalInitialize, 200)
	if body := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202); len(body) != 0 {
		t.Fatalf("notification body=%s", body)
	}
	tools := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 200)
	if !strings.Contains(string(tools), "rkc.get_evidence") {
		t.Fatalf("missing evidence tool: %s", tools)
	}
	hits := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rkc.search","arguments":{"query":"Lantern","limit":5}}}`, 200)
	if !strings.Contains(string(hits), "fictional-node") || !strings.Contains(string(hits), "handbook.md") {
		t.Fatalf("missing fictional source hit: %s", hits)
	}
	contextData := post(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"rkc.context","arguments":{"query":"Lantern","limit":5,"max_bytes":4096}}}`, 200)
	var contextReply struct {
		Result struct {
			StructuredContent rkcapi.ContextPacket `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(contextData, &contextReply); err != nil {
		t.Fatal(err)
	}
	packet := contextReply.Result.StructuredContent
	if packet.SnapshotID != "fictional-snapshot" || packet.Integrity != server.IntegrityVerified || len(packet.Items) != 1 || packet.Items[0].Source == nil || !strings.Contains(packet.Items[0].Text, "14 days") || len(packet.Items[0].EvidenceIDs) != 1 || packet.Items[0].EvidenceIDs[0] != "fictional-evidence" || packet.Items[0].CitationID == "" {
		t.Fatalf("unbound context: %+v", packet)
	}
	evidenceData := post(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"rkc.get_evidence","arguments":{"evidence_id":"fictional-evidence"}}}`, 200)
	var evidenceReply struct {
		Result struct {
			StructuredContent rkcmodel.Evidence `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(evidenceData, &evidenceReply); err != nil {
		t.Fatal(err)
	}
	evidence := evidenceReply.Result.StructuredContent
	if evidence.Source == nil || *evidence.Source != *packet.Items[0].Source || evidence.InputDigest != dataset.ArtifactByID["fictional-artifact"].SHA256 || evidence.ID != packet.Items[0].EvidenceIDs[0] {
		t.Fatalf("citation/evidence mismatch: evidence=%+v context=%+v", evidence, packet.Items[0])
	}
	after, err := json.Marshal(dataset.Bundle)
	if err != nil || string(after) != string(before) {
		t.Fatal("read-only HTTP workflow changed the pinned canonical bundle")
	}
}
