package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPCLIRequiresExplicitPinnedAtlasAndRejectsOtherSelectors(t *testing.T) {
	fictionalMissing := filepath.Join(t.TempDir(), "fictional-missing-atlas")
	for _, arguments := range [][]string{
		{"--transport", "http"},
		{"--transport", "http", "--dir", ""},
		{"--transport", "http", "--dir", " fictional-atlas "},
		{"--transport", "http", "--dir", fictionalMissing, "--workspace", ""},
		{"--transport", "http", "--dir", fictionalMissing, "--database", ""},
		{"--transport", "http", "--dir", fictionalMissing, "--snapshot", ""},
		{"--transport", "http", "--dir", fictionalMissing, "--repository", ""},
	} {
		var diagnostics bytes.Buffer
		code := run(context.Background(), arguments, strings.NewReader(""), io.Discard, &diagnostics)
		if code != 1 || !strings.Contains(diagnostics.String(), "HTTP requires explicit --dir") || strings.Contains(diagnostics.String(), "load bundle") {
			t.Fatalf("selectors=%v code=%d diagnostics=%s", arguments, code, diagnostics.String())
		}
	}
	for _, arguments := range [][]string{
		{"--transport", "fictional"},
		{"--listen", "127.0.0.1:0"},
		{"--http-request-timeout", "1s"},
		{"--http-concurrency", "1"},
		{"--transport", "http", "--dir", fictionalMissing, "--listen", "0.0.0.0:0"},
		{"--transport", "http", "--dir", fictionalMissing, "--listen", "localhost:0"},
		{"--transport", "http", "--dir", fictionalMissing, "--http-request-timeout", "31s"},
		{"--transport", "http", "--dir", fictionalMissing, "--http-concurrency", "9"},
	} {
		var diagnostics bytes.Buffer
		code := run(context.Background(), arguments, strings.NewReader(""), io.Discard, &diagnostics)
		if code != 1 || strings.Contains(diagnostics.String(), "listening on") || strings.Contains(diagnostics.String(), "load bundle") {
			t.Fatalf("unsafe options=%v code=%d diagnostics=%s", arguments, code, diagnostics.String())
		}
	}
	for _, flag := range []string{"--refresh", "--force", "--write", "--delete"} {
		var diagnostics bytes.Buffer
		if code := run(context.Background(), []string{"--transport", "http", "--dir", fictionalMissing, flag}, strings.NewReader(""), io.Discard, &diagnostics); code != 2 || !strings.Contains(diagnostics.String(), "flag provided but not defined") {
			t.Fatalf("mutation flag=%s code=%d diagnostics=%s", flag, code, diagnostics.String())
		}
	}
	var diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"--transport", "http", "--dir", fictionalMissing}, strings.NewReader(""), io.Discard, &diagnostics); code != 1 || strings.Contains(diagnostics.String(), "listening on") {
		t.Fatalf("missing fictional atlas code=%d diagnostics=%s", code, diagnostics.String())
	}
}

type httpDiagnosticLines struct{ lines chan string }

func (writer httpDiagnosticLines) Write(value []byte) (int, error) {
	writer.lines <- string(value)
	return len(value), nil
}

func TestHTTPCLIStartsVerifiedFictionalAtlasAndStopsOnCancellation(t *testing.T) {
	atlas := writeTestDataset(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	diagnostics := httpDiagnosticLines{lines: make(chan string, 16)}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--transport", "http", "--dir", atlas}, strings.NewReader(""), io.Discard, diagnostics)
	}()
	var startup string
	select {
	case startup = <-diagnostics.lines:
	case code := <-done:
		t.Fatalf("HTTP exited before listening: %d", code)
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP did not announce its local address")
	}
	const prefix = "rkc-mcp: listening on "
	const suffix = " (snapshot rkc:snapshot:test; credential-free local only)\n"
	if !strings.HasPrefix(startup, prefix+"http://127.0.0.1:") || !strings.HasSuffix(startup, suffix) {
		t.Fatalf("unexpected diagnostic format: %q", startup)
	}
	endpoint := strings.TrimSuffix(strings.TrimPrefix(startup, prefix), suffix)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fictional-cli-client","version":"test"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != 200 || !strings.Contains(string(body), `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("initialize status=%d body=%s error=%v", response.StatusCode, body, readErr)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("HTTP cancellation exit=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP did not stop on context cancellation")
	}
}
