package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGUIWelcomeServesBeforeAnyRepositoryScan(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "example.go"), []byte("package example\nfunc Login() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(t.TempDir(), "ready.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runServeWithSafety(ctx, []string{"--welcome", "--workbench", "--workspace", workspace, "--ready-file", readyPath, "--addr", "127.0.0.1:0"}, serveSafetyChecks{
			requireLowPriority: func() error { return nil }, reproveLowPriority: func() error { return nil }, checkHigherPriority: func() error { return nil }, checkHostMemory: func() error { return nil }, priorityCheckInterval: time.Second,
		})
	}()
	var ready serveReadyReceipt
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(readyPath); err == nil {
			if err := json.Unmarshal(raw, &ready); err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case err := <-done:
			t.Fatalf("welcome failed before ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("welcome readiness timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready.SnapshotID != "rkc:workspace:empty" || ready.BrowserURL == "" {
		t.Fatalf("welcome receipt: %+v", ready)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".rkc")); !os.IsNotExist(err) {
		t.Fatal("welcome scanned the initial directory")
	}
	response, err := http.Get(ready.URL + "/api/v1/manifest")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Metadata map[string]string `json:"metadata"`
	}
	err = json.NewDecoder(response.Body).Decode(&manifest)
	response.Body.Close()
	if err != nil || manifest.Metadata["rkc_workspace"] != "empty" {
		t.Fatalf("welcome manifest: %+v %v", manifest, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("welcome server did not stop")
	}
}

func TestGUIHelpAndWelcomeAdmission(t *testing.T) {
	if err := dispatch([]string{"gui", "--help"}); err != nil && err.Error() != "flag: help requested" {
		t.Fatal(err)
	}
	if err := runOpenContext(context.Background(), []string{"--welcome", t.TempDir()}); err == nil {
		t.Fatal("unprotected welcome accepted")
	}
	if err := runServeWithSafety(context.Background(), []string{"--welcome"}, serveSafetyChecks{}); err == nil {
		t.Fatal("read-only welcome accepted")
	}
	for _, option := range []string{"--config=unused.json", "--out=unused", "--state-dir=unused", "--python=false", "--clean=false", "--force=true", "--scip-index=unused", "--trace=unused", "--history=unused"} {
		if err := runOpenContext(context.Background(), []string{"--welcome", "--workbench", option}); err == nil {
			t.Fatalf("welcome accepted unused option %s", option)
		}
	}
	for _, test := range []struct {
		command string
		args    []string
	}{{"scan", []string{"--no-python", "--no-git-metadata", "."}}, {"quickstart", []string{"--no-git-metadata", "."}}} {
		if _, err := validateDirectCommandAdmission(test.command, test.args); err != nil {
			t.Fatalf("metadata helper optout rejected: %v", err)
		}
	}
}
