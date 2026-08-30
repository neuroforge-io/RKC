package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWorkbenchBrowserCapabilityCannotBeReissuedAfterSessionExchange(t *testing.T) {
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: t.TempDir(), Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	const serverURL = "http://127.0.0.1:8787"
	browserURL, err := workbench.BrowserURL(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(browserURL)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(parsed.Fragment)
	if err != nil || fragment.Get(workbenchBootstrapFragment) == "" || parsed.RawQuery != "" {
		t.Fatalf("protected browser URL = %q, %v", browserURL, err)
	}
	bootstrap := fragment.Get(workbenchBootstrapFragment)

	request := httptest.NewRequest(http.MethodGet, serverURL+"/api/v1/workbench/session", nil)
	request.Header.Set("Origin", serverURL)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set(workbenchBootstrapHeader, bootstrap)
	response := httptest.NewRecorder()
	workbench.handleSession(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("bootstrap exchange status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if strings.Contains(response.Body.String(), bootstrap) {
		t.Fatal("session response disclosed the consumed bootstrap capability")
	}
	if !strings.Contains(response.Body.String(), "invoking OS user's filesystem authority") ||
		!strings.Contains(response.Body.String(), "not a security sandbox") {
		t.Fatalf("session omitted trusted-user authority warning: %s", response.Body.String())
	}
	if _, err := workbench.BrowserURL(serverURL); err == nil || !strings.Contains(err.Error(), "already been consumed") {
		t.Fatalf("consumed browser capability was reissued: %v", err)
	}

	replay := httptest.NewRequest(http.MethodGet, serverURL+"/api/v1/workbench/session", nil)
	replay.Header.Set("Origin", serverURL)
	replay.Header.Set("Sec-Fetch-Site", "same-origin")
	replay.Header.Set(workbenchBootstrapHeader, bootstrap)
	replayResponse := httptest.NewRecorder()
	workbench.handleSession(replayResponse, replay)
	if replayResponse.Code != http.StatusForbidden || strings.Contains(replayResponse.Body.String(), workbench.token) {
		t.Fatalf("bootstrap replay status=%d body=%s", replayResponse.Code, replayResponse.Body.String())
	}
}
