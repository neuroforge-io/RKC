package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestToolMetadataCompatibilityAndNoAuthority(t *testing.T) {
	workspaceServer, _, _, loads := workspaceFixture(t, "alpha", "beta")
	for _, test := range []struct {
		name   string
		server *Server
		tool   string
	}{
		{"atlas", New(mcpDataset(), "test"), "rkc.coverage"},
		{"workspace", workspaceServer, "rkc.repositories"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, metadata := range []string{`{}`, `{"progressToken":"request-1"}`, `{"progressToken":3.25}`, `{"progressToken":0,"example.org/context":{"Title":"Synthetic transport metadata","flags":[true,null,3]}}`} {
				request := json.RawMessage(`{"name":"` + test.tool + `","arguments":{},"_meta":` + metadata + `}`)
				result, rpcErr := test.server.callTool(context.Background(), request)
				if rpcErr != nil || result.(map[string]any)["isError"] != false {
					t.Fatalf("valid metadata rejected: %s %#v %#v", metadata, result, rpcErr)
				}
				encoded, _ := json.Marshal(result)
				if bytes.Contains(encoded, []byte("Synthetic transport metadata")) || bytes.Contains(encoded, []byte("progressToken")) {
					t.Fatalf("transport metadata leaked to tool result: %s", encoded)
				}
			}
			for _, metadata := range []string{`null`, `[]`, `true`, `"string"`, `{"progressToken":null}`, `{"progressToken":true}`, `{"progressToken":{}}`, `{"progressToken":[]}`, `{"progressToken":1,"progressToken":2}`, `{"extension":{"a":1,"a":2}}`} {
				_, rpcErr := test.server.callTool(context.Background(), json.RawMessage(`{"name":"`+test.tool+`","_meta":`+metadata+`}`))
				if rpcErr == nil || rpcErr.Code != -32602 {
					t.Fatalf("invalid metadata accepted: %s %#v", metadata, rpcErr)
				}
			}
		})
	}
	// A repository hidden in metadata cannot silently choose a source when the
	// real tool arguments are ambiguous, nor authorize a revoked repository.
	result, rpcErr := workspaceServer.callTool(context.Background(), json.RawMessage(`{"name":"rkc.coverage","_meta":{"repository":"alpha"}}`))
	if rpcErr != nil || result.(map[string]any)["isError"] != true || *loads != 0 {
		t.Fatalf("metadata supplied authority: %#v %#v loads=%d", result, rpcErr, *loads)
	}
}

func TestToolMetadataRemainsInsideRequestByteLimit(t *testing.T) {
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rkc.coverage","_meta":{"example.org/oversize":"` + strings.Repeat("x", maximumRequestBytes) + `"}}}` + "\n"
	var out bytes.Buffer
	if err := New(mcpDataset(), "test").Serve(context.Background(), strings.NewReader(request), &out); err == nil || !strings.Contains(err.Error(), "message exceeds") || out.Len() != 0 {
		t.Fatalf("unbounded metadata admitted: error=%v bytes=%d", err, out.Len())
	}
}

func TestResourceMetadataIsOptionalAndCannotSelectURI(t *testing.T) {
	workspaceServer, _, _, _ := workspaceFixture(t, "alpha")
	for _, test := range []struct {
		server *Server
		uri    string
	}{
		{New(mcpDataset(), "test"), "rkc://snapshot/manifest"},
		{workspaceServer, "rkc://workspace/repositories"},
	} {
		result, rpcErr := test.server.handle(context.Background(), "resources/read", json.RawMessage(`{"uri":"`+test.uri+`","_meta":{"progressToken":"receipt","example.org/context":{"opaque":true}}}`))
		if rpcErr != nil || result == nil {
			t.Fatalf("resource metadata rejected: %#v %#v", result, rpcErr)
		}
		for _, raw := range []string{`{"uri":"` + test.uri + `","_meta":null}`, `{"uri":"` + test.uri + `","_meta":{"progressToken":true}}`, `{"_meta":{"uri":"` + test.uri + `"}}`} {
			if _, rpcErr := test.server.handle(context.Background(), "resources/read", json.RawMessage(raw)); rpcErr == nil {
				t.Fatalf("invalid metadata changed resource selection: %s", raw)
			}
		}
	}
}
