package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

func TestOpenForwardsOptionalScanFlagsBeforeCancellation(t *testing.T) {
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err := runOpenContext(cancelled, []string{
		"--config", filepath.Join(repository, "rkc.json"),
		"--out", filepath.Join(repository, "atlas"),
		"--state-dir", filepath.Join(repository, "state"),
		"--clean",
		"--force=false",
		"--scip-index", filepath.Join(repository, "index.scip"),
		repository,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("optional flag forwarding error = %v", err)
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

func TestLaunchBrowserPrivatelyKeepsCapabilityOutOfProcessArguments(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("desktop opener fixture is Linux-specific")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "argument")
	opener := filepath.Join(directory, "xdg-open")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$RKC_BROWSER_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("RKC_BROWSER_MARKER", marker)
	const capability = "private-bootstrap-capability"
	if err := launchBrowserPrivately("http://127.0.0.1:8787#rkc-workbench=" + capability); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var argument string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		if err == nil {
			argument = string(data)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.HasPrefix(argument, "file:") || strings.Contains(argument, capability) {
		t.Fatalf("desktop opener argument disclosed capability: %q", argument)
	}
	pageURL, err := url.Parse(argument)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(pageURL.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private bootstrap page = %v, %v", info, err)
	}
	page, err := os.ReadFile(pageURL.Path)
	if err != nil || !strings.Contains(string(page), capability) {
		t.Fatalf("private bootstrap page content = %q, %v", page, err)
	}
	if err := launchBrowserPrivately("http://example.test/#rkc-workbench=x"); err == nil {
		t.Fatal("remote browser target was accepted")
	}
}

func TestOpenBuildsServesAndStopsWithContext(t *testing.T) {
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package example\n\nfunc Hello() string { return \"hello\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	readyPath := filepath.Join(root, "ready.json")
	openArgs := []string{
		"--clean",
		"--out", filepath.Join(root, "atlas"),
		"--state-dir", filepath.Join(root, "state"),
		"--addr", "127.0.0.1:0",
		"--ready-file", readyPath,
	}
	if runtime.GOOS == "linux" {
		// Keep the browser branch deterministic and headless in CI while still
		// proving that the server attempts the portable desktop opener.
		openerDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(openerDir, "xdg-open"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", openerDir)
	} else {
		openArgs = append(openArgs, "--no-browser")
	}
	openArgs = append(openArgs, repository)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runOpenContext(ctx, openArgs)
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
