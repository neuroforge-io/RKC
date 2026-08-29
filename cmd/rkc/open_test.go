package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOpenInputContracts(t *testing.T) {
	if err := runOpenContext(context.Background(), []string{"--unknown"}); err == nil {
		t.Fatal("unknown open option succeeded")
	}
	if err := runOpenContext(context.Background(), []string{"one", "two"}); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("two repositories = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := runOpenContext(context.Background(), []string{missing}); err == nil || !strings.Contains(err.Error(), "inspect open repository") {
		t.Fatalf("missing repository = %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runOpenContext(context.Background(), []string{file}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file repository = %v", err)
	}
	if err := dispatch([]string{"open", "--help"}); err != nil && !strings.Contains(err.Error(), "help requested") {
		t.Fatalf("open help = %v", err)
	}
	if err := dispatch([]string{"start", "--help"}); err != nil && !strings.Contains(err.Error(), "help requested") {
		t.Fatalf("start help = %v", err)
	}
}

func TestBrowserCommandRejectsEmptyURL(t *testing.T) {
	if _, err := browserCommand(""); err == nil || !strings.Contains(err.Error(), "URL is empty") {
		t.Fatalf("empty browser URL error = %v", err)
	}
}

func TestLaunchBrowserUsesDesktopOpenerWithoutShell(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("desktop opener fixture is Linux-specific")
	}
	directory := t.TempDir()
	opener := filepath.Join(directory, "xdg-open")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	if err := launchBrowser("http://127.0.0.1:8787"); err != nil {
		t.Fatalf("launchBrowser = %v", err)
	}
}

func TestOpenBuildsServesAndStopsWithContext(t *testing.T) {
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package example\n\nfunc Hello() string { return \"hello\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	readyPath := filepath.Join(root, "ready.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runOpenContext(ctx, []string{
			"--clean",
			"--out", filepath.Join(root, "atlas"),
			"--state-dir", filepath.Join(root, "state"),
			"--addr", "127.0.0.1:0",
			"--ready-file", readyPath,
			"--no-browser",
			repository,
		})
	}()
	var receipt serveReadyReceipt
	deadline := time.Now().Add(8 * time.Second)
	for {
		data, err := os.ReadFile(readyPath)
		if err == nil {
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			break
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("open did not publish readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := http.Get(receipt.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("open = %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("open did not stop after context cancellation")
	}
}
