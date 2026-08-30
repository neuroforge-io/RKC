package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestHandleProtocolMethodsResourcesAndTools(t *testing.T) {
	s := New(mcpDataset(), "test-version")
	for _, tc := range []struct {
		name       string
		method     string
		params     string
		wantRPC    int
		wantResult string
	}{
		{"initialize", "initialize", `{}`, 0, ProtocolVersion},
		{"ping", "ping", `{}`, 0, ""},
		{"tools", "tools/list", `{}`, 0, "rkc.search"},
		{"resources", "resources/list", `{}`, 0, "rkc://snapshot/manifest"},
		{"manifest", "resources/read", `{"uri":"rkc://snapshot/manifest"}`, 0, "snapshot-mcp"},
		{"coverage", "resources/read", `{"uri":"rkc://snapshot/coverage"}`, 0, "snapshot-mcp"},
		{"diagnostics", "resources/read", `{"uri":"rkc://snapshot/diagnostics"}`, 0, "diagnostic-mcp"},
		{"missing resource", "resources/read", `{"uri":"rkc://missing"}`, -32002, "resource not found"},
		{"bad resource params", "resources/read", `{`, -32602, "invalid params"},
		{"unknown method", "unknown", `{}`, -32601, "method not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, rpcErr := s.handle(context.Background(), tc.method, json.RawMessage(tc.params))
			if tc.wantRPC != 0 {
				if rpcErr == nil || rpcErr.Code != tc.wantRPC || !strings.Contains(rpcErr.Message, tc.wantResult) {
					t.Fatalf("rpcErr=%+v want code=%d text=%q", rpcErr, tc.wantRPC, tc.wantResult)
				}
				return
			}
			if rpcErr != nil {
				t.Fatalf("rpcErr=%+v", rpcErr)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantResult != "" && !strings.Contains(string(encoded), tc.wantResult) {
				t.Fatalf("result=%s want %q", encoded, tc.wantResult)
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, rpcErr := s.handle(cancelled, "ping", nil); rpcErr == nil || rpcErr.Code != -32800 {
		t.Fatalf("cancelled request error=%+v", rpcErr)
	}
}

func TestToolCallsSuccessAndBoundedFailures(t *testing.T) {
	s := New(mcpDataset(), "test")
	cases := []struct {
		name      string
		params    string
		want      string
		wantError bool
		wantRPC   int
	}{
		{"search", `{"name":"rkc.search","arguments":{"query":"Alpha","limit":1,"kinds":["function"],"languages":"go"}}`, "pkg.Alpha", false, 0},
		{"search missing", `{"name":"rkc.search","arguments":{}}`, "query is required", true, 0},
		{"symbol id", `{"name":"rkc.get_symbol","arguments":{"node":"a"}}`, "evidence-a", false, 0},
		{"symbol qualified", `{"name":"rkc.get_symbol","arguments":{"node":"pkg.Alpha"}}`, "pkg.Alpha", false, 0},
		{"symbol missing", `{"name":"rkc.get_symbol","arguments":{"node":"missing"}}`, "node not found", true, 0},
		{"symbol ambiguous", `{"name":"rkc.get_symbol","arguments":{"node":"Same"}}`, "ambiguous", true, 0},
		{"evidence", `{"name":"rkc.get_evidence","arguments":{"evidence_id":"evidence-a"}}`, "syntax_derived", false, 0},
		{"evidence empty", `{"name":"rkc.get_evidence","arguments":{}}`, "evidence_id is required", true, 0},
		{"evidence missing", `{"name":"rkc.get_evidence","arguments":{"evidence_id":"missing"}}`, "evidence not found", true, 0},
		{"neighborhood", `{"name":"rkc.neighborhood","arguments":{"node":"a","direction":"outgoing","max_depth":2,"max_nodes":20}}`, "edge-ab", false, 0},
		{"neighborhood invalid", `{"name":"rkc.neighborhood","arguments":{"node":"a","direction":"sideways"}}`, "invalid direction", true, 0},
		{"path", `{"name":"rkc.find_path","arguments":{"from":"a","to":"b"}}`, "edge-ab", false, 0},
		{"impact", `{"name":"rkc.impact","arguments":{"node":"b"}}`, "Alpha", false, 0},
		{"coverage", `{"name":"rkc.coverage","arguments":{}}`, "snapshot-mcp", false, 0},
		{"unknown", `{"name":"rkc.unknown","arguments":{}}`, "unknown tool", false, -32602},
		{"invalid params", `{`, "invalid params", false, -32602},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, rpcErr := s.callTool(context.Background(), json.RawMessage(tc.params))
			if tc.wantRPC != 0 {
				if rpcErr == nil || rpcErr.Code != tc.wantRPC || !strings.Contains(rpcErr.Message, tc.want) {
					t.Fatalf("rpcErr=%+v want %d/%q", rpcErr, tc.wantRPC, tc.want)
				}
				return
			}
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), tc.want) {
				t.Fatalf("result=%s want %q", encoded, tc.want)
			}
			var wrapper map[string]any
			if err := json.Unmarshal(encoded, &wrapper); err != nil {
				t.Fatal(err)
			}
			if got, _ := wrapper["isError"].(bool); got != tc.wantError {
				t.Fatalf("isError=%v want %v result=%s", got, tc.wantError, encoded)
			}
		})
	}
}

func TestServeJSONRPCFramingNotificationsAndErrors(t *testing.T) {
	s := New(mcpDataset(), "test")
	input := strings.Join([]string{
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"1.0","method":"ping"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"does/not/exist"}`,
	}, "\n")
	var output bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("responses=%d want 3: %s", len(lines), output.String())
	}
	if !strings.Contains(lines[0], "invalid JSON-RPC version") || !strings.Contains(lines[1], `"id":2`) || !strings.Contains(lines[2], "method not found") {
		t.Fatalf("unexpected responses: %s", output.String())
	}
	var malformed bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader("{"), &malformed); err != nil ||
		!strings.Contains(malformed.String(), "parse error") {
		t.Fatalf("malformed request response=%q err=%v", malformed.String(), err)
	}
	if err := s.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), failingWriter{}); err == nil || !strings.Contains(err.Error(), "encode MCP response") {
		t.Fatalf("expected encode failure, got %v", err)
	}
}

func TestMCPTransportAndPayloadBoundsFailClosed(t *testing.T) {
	s := New(mcpDataset(), "test")

	oversized := strings.NewReader(strings.Repeat("x", maximumRequestBytes+1))
	if err := s.Serve(context.Background(), oversized, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized request error = %v", err)
	}

	for _, payload := range []string{
		`{"jsonrpc":"2.0","jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","unknown":true}`,
	} {
		var output bytes.Buffer
		if err := s.Serve(context.Background(), strings.NewReader(payload), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `"code":-32700`) {
			t.Fatalf("strict request response = %s", output.String())
		}
	}

	var invalidID bytes.Buffer
	if err := s.Serve(
		context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":{"bad":true},"method":"ping"}`),
		&invalidID,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(invalidID.String(), `"code":-32600`) {
		t.Fatalf("invalid ID response = %s", invalidID.String())
	}

	for _, params := range []string{
		`{"name":"rkc.search","arguments":{"query":"x","unknown":true}}`,
		`{"name":"rkc.search","arguments":{"query":"x","limit":"many"}}`,
		`{"name":"rkc.neighborhood","arguments":{"node":"a","max_nodes":5001}}`,
	} {
		if _, rpcErr := s.callTool(context.Background(), json.RawMessage(params)); rpcErr == nil ||
			rpcErr.Code != -32602 {
			t.Fatalf("invalid tool arguments %s produced %+v", params, rpcErr)
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, rpcErr := s.callTool(
		cancelled,
		json.RawMessage(`{"name":"rkc.coverage","arguments":{}}`),
	); rpcErr == nil || rpcErr.Code != -32800 {
		t.Fatalf("cancelled tool call = %+v", rpcErr)
	}

	var bounded bytes.Buffer
	if err := writeResponse(&bounded, response{
		JSONRPC: "2.0",
		ID:      json.RawMessage("7"),
		Result:  strings.Repeat("x", maximumResponseBytes),
	}); err != nil {
		t.Fatal(err)
	}
	if bounded.Len() >= maximumResponseBytes ||
		!strings.Contains(bounded.String(), "response too large") {
		t.Fatalf("bounded response bytes=%d body=%s", bounded.Len(), bounded.String())
	}
}

func TestMCPResourceAndStructuredResultsAreBounded(t *testing.T) {
	dataset := mcpDataset()
	dataset.Bundle.Diagnostics = make([]rkcmodel.Diagnostic, maximumResourceItems+1)
	for index := range dataset.Bundle.Diagnostics {
		dataset.Bundle.Diagnostics[index] = rkcmodel.Diagnostic{
			ID:       fmt.Sprintf("diagnostic-%04d", index),
			Severity: "warning",
			Code:     "TEST",
			Message:  "bounded fixture",
		}
	}
	s := New(dataset, "test")
	result, rpcErr := s.handle(
		context.Background(),
		"resources/read",
		json.RawMessage(`{"uri":"rkc://snapshot/diagnostics"}`),
	)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	resource, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("bounded diagnostics resource type = %T", result)
	}
	contents, ok := resource["contents"].([]map[string]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("bounded diagnostics contents = %#v", resource["contents"])
	}
	text, _ := contents[0]["text"].(string)
	var page struct {
		Items     []rkcmodel.Diagnostic `json:"items"`
		Total     int                   `json:"total"`
		Truncated bool                  `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Truncated || page.Total != maximumResourceItems+1 ||
		len(page.Items) != maximumResourceItems {
		t.Fatalf(
			"bounded diagnostics page = total %d items %d truncated %t",
			page.Total,
			len(page.Items),
			page.Truncated,
		)
	}

	large := mcpDataset()
	evidence := large.EvidenceByID["evidence-a"]
	evidence.Detail = strings.Repeat("x", maximumStructuredBytes+1)
	large.EvidenceByID["evidence-a"] = evidence
	result, rpcErr = New(large, "test").callTool(
		context.Background(),
		json.RawMessage(`{"name":"rkc.get_evidence","arguments":{"evidence_id":"evidence-a"}}`),
	)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	wrapper, ok := result.(map[string]any)
	if !ok || wrapper["isError"] != true {
		t.Fatalf("oversized structured result = %#v", result)
	}
}

func TestMCPHelpersAndSchemas(t *testing.T) {
	t.Parallel()
	if !hasID(json.RawMessage("1")) || hasID(nil) || hasID(json.RawMessage("null")) {
		t.Fatal("hasID mismatch")
	}
	values := map[string]any{
		"space": " value ", "bool": true, "number": json.Number("12"), "float": float64(7.9), "integer": 4,
		"array": []any{"go", "", 1}, "strings": []string{"python", ""}, "csv": "go, rust,",
	}
	if stringArg(values, "space") != "value" || !boolArg(values, "bool") || boolArg(values, "missing") {
		t.Fatal("string/bool helper mismatch")
	}
	if intArg(values, "number", 1, 0, 10) != 10 || intArg(values, "float", 1, 0, 10) != 7 || intArg(values, "integer", 1, 0, 10) != 4 || intArg(values, "missing", 5, 0, 10) != 5 {
		t.Fatal("intArg mismatch")
	}
	if len(setArg(values, "array")) != 1 || len(setArg(values, "strings")) != 1 || len(setArg(values, "csv")) != 2 {
		t.Fatal("setArg mismatch")
	}
	if len(tools()) != 7 || pathSchema()["properties"] == nil || traversalSchema("node")["required"] == nil {
		t.Fatal("tool schema mismatch")
	}
	if errorResponse(json.RawMessage("1"), -1, "bad", "data").Error == nil || toolError(errors.New("bad"))["isError"] != true {
		t.Fatal("error helper mismatch")
	}
}

func TestMCPServerConcurrentReadSafety(t *testing.T) {
	s := New(mcpDataset(), "test")
	methods := []struct{ method, params string }{
		{"tools/list", `{}`}, {"resources/read", `{"uri":"rkc://snapshot/coverage"}`},
		{"tools/call", `{"name":"rkc.search","arguments":{"query":"Alpha"}}`},
		{"tools/call", `{"name":"rkc.neighborhood","arguments":{"node":"a"}}`},
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		for _, item := range methods {
			wg.Add(1)
			go func(method, params string) {
				defer wg.Done()
				if _, rpcErr := s.handle(context.Background(), method, json.RawMessage(params)); rpcErr != nil {
					t.Errorf("%s: %+v", method, rpcErr)
				}
			}(item.method, item.params)
		}
	}
	wg.Wait()
}

func TestMCPArgumentAndRequestValidatorsCoverProtocolBoundaries(t *testing.T) {
	invalidArguments := []struct {
		name  string
		value any
		rule  toolArgumentRule
	}{
		{"string type", 1, toolArgumentRule{kind: toolArgumentString, max: 4}},
		{"string length", "12345", toolArgumentRule{kind: toolArgumentString, max: 4}},
		{"integer type", 1, toolArgumentRule{kind: toolArgumentInteger, min: 1, max: 2}},
		{"integer syntax", json.Number("1.5"), toolArgumentRule{kind: toolArgumentInteger, min: 1, max: 2}},
		{"integer range", json.Number("3"), toolArgumentRule{kind: toolArgumentInteger, min: 1, max: 2}},
		{"boolean type", "true", toolArgumentRule{kind: toolArgumentBoolean}},
		{"list type", 1, toolArgumentRule{kind: toolArgumentStringList, max: 2}},
		{"list count", []any{"a", "b", "c"}, toolArgumentRule{kind: toolArgumentStringList, max: 2}},
		{"list item type", []any{1}, toolArgumentRule{kind: toolArgumentStringList, max: 2}},
		{"list item length", []any{strings.Repeat("x", maximumArgumentTextBytes+1)}, toolArgumentRule{kind: toolArgumentStringList, max: 2}},
		{"rule kind", "x", toolArgumentRule{kind: 255}},
	}
	for _, test := range invalidArguments {
		t.Run(test.name, func(t *testing.T) {
			if err := validateToolArgument("value", test.value, test.rule); err == nil {
				t.Fatalf("invalid argument was accepted: %#v", test.value)
			}
		})
	}
	for _, value := range []any{
		strings.Repeat("x", maximumArgumentTextBytes+1),
		[]any{strings.Repeat("x", maximumArgumentTextBytes+1)},
	} {
		if err := validateStringListArgument("items", value, 2); err == nil {
			t.Fatalf("oversized string list was accepted: %T", value)
		}
	}
	if err := validateToolArguments("rkc.search", map[string]any{"unknown": true}); err == nil {
		t.Fatal("unknown search argument was accepted")
	}
	if err := validateToolArguments("third.party", map[string]any{"anything": true}); err != nil {
		t.Fatalf("unknown extension tool arguments were rejected: %v", err)
	}

	requests := []request{
		{JSONRPC: "1.0", Method: "ping"},
		{JSONRPC: "2.0", Method: ""},
		{JSONRPC: "2.0", Method: " ping "},
		{JSONRPC: "2.0", Method: strings.Repeat("x", 257)},
		{JSONRPC: "2.0", ID: json.RawMessage(`{"bad":true}`), Method: "ping"},
		{JSONRPC: "2.0", ID: json.RawMessage(`"` + strings.Repeat("x", 257) + `"`), Method: "ping"},
		{JSONRPC: "2.0", Params: json.RawMessage(`[]`), Method: "ping"},
		{JSONRPC: "2.0", Params: json.RawMessage(`{"a":1,"a":2}`), Method: "ping"},
	}
	for index, request := range requests {
		if err := validateRequest(request); err == nil {
			t.Errorf("invalid request %d was accepted: %+v", index, request)
		}
	}
	for _, id := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("1"), json.RawMessage(`"id"`)} {
		if err := validateRequest(request{JSONRPC: "2.0", ID: id, Method: "ping"}); err != nil {
			t.Errorf("valid request ID %s = %v", id, err)
		}
	}
}

func TestMCPTransportToolAndDecoderResidualBoundaries(t *testing.T) {
	s := New(mcpDataset(), "test")
	if err := s.Serve(nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("nil context succeeded")
	}
	if err := s.Serve(context.Background(), nil, io.Discard); err == nil {
		t.Fatal("nil input succeeded")
	}
	if err := s.Serve(context.Background(), strings.NewReader(""), nil); err == nil {
		t.Fatal("nil output succeeded")
	}
	if err := (*Server)(nil).Serve(context.Background(), strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("nil server succeeded")
	}
	if err := New(nil, "test").Serve(context.Background(), strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("nil dataset succeeded")
	}
	if err := s.Serve(context.Background(), strings.NewReader("\n \t\n"), io.Discard); err != nil {
		t.Fatalf("blank frames = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, rpcErr := s.readResource(
		cancelled, json.RawMessage(`{"uri":"rkc://snapshot/manifest"}`),
	); rpcErr == nil || rpcErr.Code != -32800 {
		t.Fatalf("cancelled resource = %+v", rpcErr)
	}
	for _, params := range []string{
		`{"name":"rkc.neighborhood","arguments":{"node":"missing"}}`,
		`{"name":"rkc.find_path","arguments":{"from":"missing","to":"b"}}`,
		`{"name":"rkc.find_path","arguments":{"from":"a","to":"missing"}}`,
		`{"name":"rkc.impact","arguments":{"node":"missing"}}`,
		`{"name":"rkc.impact","arguments":{"node":"a","direction":"sideways"}}`,
	} {
		result, rpcErr := s.callTool(context.Background(), json.RawMessage(params))
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		wrapper, ok := result.(map[string]any)
		if !ok || wrapper["isError"] != true {
			t.Fatalf("tool failure wrapper = %#v", result)
		}
	}

	dataset := mcpDataset()
	node := dataset.NodeByID["a"]
	node.EvidenceIDs = make([]string, maximumRelatedRecords+1)
	for index := range node.EvidenceIDs {
		node.EvidenceIDs[index] = "missing"
	}
	dataset.NodeByID["a"] = node
	dataset.Bundle.Nodes[0] = node
	dataset.Graph.Incoming["a"] = make([]rkcmodel.Edge, maximumRelatedRecords+1)
	dataset.Graph.Outgoing["a"] = make([]rkcmodel.Edge, maximumRelatedRecords+1)
	result, err := New(dataset, "test").toolGetSymbol(map[string]any{"node": "a"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(encoded), `"truncated":true`) {
		t.Fatalf("truncated symbol = %s, %v", encoded, err)
	}

	for _, data := range [][]byte{
		[]byte(`{} {}`),
		[]byte(`[1,2]`),
		[]byte(`{"a":[1,{"b":2}]}`),
		[]byte(`{"a":`),
	} {
		var target map[string]any
		err := decodeStrict(data, &target)
		if string(data) == `{"a":[1,{"b":2}]}` {
			if err != nil {
				t.Errorf("valid nested JSON = %v", err)
			}
		} else if err == nil {
			t.Errorf("invalid strict JSON accepted: %s", data)
		}
	}
	if err := writeResponse(io.Discard, response{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Result: make(chan int),
	}); err == nil {
		t.Fatal("unencodable response succeeded")
	}
	if got := intArg(map[string]any{"bad": json.Number("not-a-number")}, "bad", 7, 0, 10); got != 7 {
		t.Fatalf("invalid numeric argument = %d", got)
	}
	missingNodeDataset := mcpDataset()
	delete(missingNodeDataset.NodeByID, "a")
	searchResult, err := New(missingNodeDataset, "test").toolSearch(map[string]any{"query": "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	searchJSON, err := json.Marshal(searchResult)
	if err != nil || !strings.Contains(string(searchJSON), "pkg.Alpha") || strings.Contains(string(searchJSON), `"node":`) {
		t.Fatalf("search did not retain the canonical hit without stale node enrichment: %s, %v", searchJSON, err)
	}
}

func mcpDataset() *server.Dataset {
	evidence := rkcmodel.Evidence{ID: "evidence-a", Kind: "syntax_derived", Method: "test", Confidence: 1}
	a := rkcmodel.Node{ID: "a", LogicalID: "logical-a", Kind: "function", Name: "Alpha", QualifiedName: "pkg.Alpha", Language: "go", EvidenceIDs: []string{evidence.ID}}
	b := rkcmodel.Node{ID: "b", LogicalID: "logical-b", Kind: "function", Name: "Beta", QualifiedName: "pkg.Beta", Language: "go"}
	c := rkcmodel.Node{ID: "c", LogicalID: "logical-c", Kind: "function", Name: "Same", QualifiedName: "pkg.One.Same", Language: "go"}
	d := rkcmodel.Node{ID: "d", LogicalID: "logical-d", Kind: "function", Name: "Same", QualifiedName: "pkg.Two.Same", Language: "go"}
	edge := rkcmodel.Edge{ID: "edge-ab", Kind: "calls", From: a.ID, To: b.ID, Resolution: "declared", Confidence: 1}
	diagnostic := rkcmodel.Diagnostic{ID: "diagnostic-mcp", Severity: "warning", Code: "TEST", Message: "fixture"}
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "snapshot-mcp", SchemaVersion: rkcmodel.SchemaVersion}, Nodes: []rkcmodel.Node{a, b, c, d}, Edges: []rkcmodel.Edge{edge}, Evidence: []rkcmodel.Evidence{evidence}, Diagnostics: []rkcmodel.Diagnostic{diagnostic}}
	return &server.Dataset{
		Manifest: bundle.Snapshot, Coverage: rkcmodel.BuildCoverage(bundle), Bundle: bundle,
		NodeByID: map[string]rkcmodel.Node{"a": a, "b": b, "c": c, "d": d}, ArtifactByID: map[string]rkcmodel.Artifact{}, EvidenceByID: map[string]rkcmodel.Evidence{evidence.ID: evidence},
		Graph: graphindex.Build(bundle.Nodes, bundle.Edges), Search: search.BuildFromBundle(bundle), LoadedAt: time.Unix(1, 0),
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
