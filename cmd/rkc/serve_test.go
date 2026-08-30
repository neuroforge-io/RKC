package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

func TestPublishServeReadyFileIsAtomicAndNoClobber(t *testing.T) {
	receipt := serveReadyReceipt{
		SchemaVersion: "1.0",
		Address:       "127.0.0.1:12345",
		URL:           "http://127.0.0.1:12345",
		SnapshotID:    "rkc:snapshot:test",
	}
	path := filepath.Join(t.TempDir(), "ready.json")
	if err := publishServeReadyFile(path, receipt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("readiness mode = %v", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded serveReadyReceipt
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != receipt {
		t.Fatalf("readiness receipt = %+v, decode error = %v", decoded, err)
	}
	if err := publishServeReadyFile(path, serveReadyReceipt{SnapshotID: "replacement"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second publication error = %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(data) {
		t.Fatalf("readiness receipt changed: %q, error = %v", unchanged, err)
	}
}

func TestServedWorkbenchCommandContextUsesExactImmutableSelection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "atlas with spaces")
	fileContext := servedWorkbenchCommandContext(root, "snapshot-file", "")
	if got := strings.Join(fileContext.DatasetArgs, "|"); got != "--dir|"+root {
		t.Fatalf("file dataset args = %q", got)
	}
	if got := strings.Join(fileContext.CheckArgs, "|"); got != "--coverage|"+filepath.Join(root, "coverage.json") {
		t.Fatalf("file check args = %q", got)
	}

	database := filepath.Join(t.TempDir(), "catalogue.sqlite")
	databaseContext := servedWorkbenchCommandContext(database, "snapshot-exact", database)
	if got := strings.Join(databaseContext.DatasetArgs, "|"); got != "--database|"+database+"|--snapshot|snapshot-exact" {
		t.Fatalf("database dataset args = %q", got)
	}
	if got := strings.Join(databaseContext.CheckArgs, "|"); got != "--help" {
		t.Fatalf("database check args = %q", got)
	}
}

func TestPublishServeReadyFileOptionalAndRejectsExistingSymlink(t *testing.T) {
	if err := publishServeReadyFile("", serveReadyReceipt{}); err != nil {
		t.Fatalf("empty readiness path: %v", err)
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "ready.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := publishServeReadyFile(link, serveReadyReceipt{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("symlink readiness publication error = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: %q, error = %v", data, err)
	}
}

func TestLoopbackListenAddressStrictlyRejectsRemoteAndMalformedHosts(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:0", "127.1.2.3:8787", "[::1]:0", "localhost:8787", "LOCALHOST:1",
	} {
		if !loopbackListenAddress(address) {
			t.Errorf("loopback address rejected: %q", address)
		}
	}
	for _, address := range []string{
		"", "127.0.0.1", "0.0.0.0:8787", "[::]:8787", "example.com:8787", "localhost",
	} {
		if loopbackListenAddress(address) {
			t.Errorf("non-loopback or malformed address accepted: %q", address)
		}
	}
}

func TestValidateServeAddressRequiresExplicitRemoteAcknowledgement(t *testing.T) {
	if err := validateServeAddress("127.0.0.1:8787", false, false); err != nil {
		t.Fatalf("loopback listener rejected: %v", err)
	}
	if err := validateServeAddress("0.0.0.0:8787", false, false); err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Fatalf("remote listener without acknowledgement = %v", err)
	}
	if err := validateServeAddress("0.0.0.0:8787", true, false); err != nil {
		t.Fatalf("explicit read-only remote listener rejected: %v", err)
	}
	if err := validateServeAddress("0.0.0.0:8787", true, true); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("remote workbench listener = %v", err)
	}
	if err := validateServeAddress("127.0.0.1:8787", false, true); err == nil || !strings.Contains(err.Error(), "ephemeral port 0") {
		t.Fatalf("stable-origin workbench listener = %v", err)
	}
	if err := validateServeAddress("127.0.0.1:0", false, true); err != nil {
		t.Fatalf("ephemeral workbench listener = %v", err)
	}
	if err := validateServeAddress("not-an-address", false, false); err != nil {
		t.Fatalf("malformed address should be left to the listener for a precise error: %v", err)
	}
}

func TestRunServeWiresRemoteAcknowledgementBeforeDatasetLoading(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-atlas")
	err := runServe([]string{"--addr", "0.0.0.0:0", "--dir", missing})
	if err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Fatalf("remote serve without acknowledgement = %v", err)
	}
	err = runServe([]string{"--allow-remote", "--addr", "0.0.0.0:0", "--dir", missing})
	if err == nil || strings.Contains(err.Error(), "--allow-remote") {
		t.Fatalf("explicit remote acknowledgement did not reach dataset validation: %v", err)
	}
}

func TestRunServePublishesReadyServesAndShutsDownCleanly(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	writeTestFile(t, filepath.Join(repository, "main.go"), "package fixture\n\nfunc Run() bool { return true }\n")
	atlas := filepath.Join(root, "atlas")
	if err := runScan([]string{
		"--out", atlas, "--state-dir", filepath.Join(root, "state"),
		"--runs-dir", filepath.Join(root, "runs"), "--no-cache", "--no-plugins",
		"--no-frameworks", "--no-secret-scan", repository,
	}); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready.json")
	done := make(chan error, 1)
	go func() {
		done <- runServe([]string{
			"--dir", atlas, "--addr", "127.0.0.1:0", "--ready-file", ready,
			"--read-timeout", "2s", "--write-timeout", "2s",
		})
	}()
	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(ready)
		if err == nil {
			var receipt serveReadyReceipt
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
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
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("serve readiness receipt was not published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not stop after SIGINT")
	}

	workbenchReady := filepath.Join(root, "workbench-ready.json")
	workbenchDone := make(chan error, 1)
	priorityArrived := make(chan struct{})
	priorityError := fmt.Errorf("%w: deterministic workbench fixture", resourceguard.ErrHigherPriorityActive)
	admitted := false
	go func() {
		workbenchDone <- runServeWithSafety(context.Background(), []string{
			"--dir", atlas, "--ready-file", workbenchReady,
			"--workbench", "--workspace", repository, "--workbench-timeout", "2s",
		}, serveSafetyChecks{
			requireLowPriority: func() error { return nil },
			checkHigherPriority: func() error {
				if !admitted {
					admitted = true
					return nil
				}
				select {
				case <-priorityArrived:
					return priorityError
				default:
					return nil
				}
			},
			priorityCheckInterval: 5 * time.Millisecond,
		})
	}()
	var workbenchReceipt serveReadyReceipt
	waitDeadline = time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(workbenchReady)
		if err == nil {
			if err := json.Unmarshal(data, &workbenchReceipt); err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(waitDeadline) {
			select {
			case err := <-workbenchDone:
				t.Fatalf("workbench serve stopped before readiness: %v", err)
			default:
			}
			t.Fatal("workbench readiness receipt was not published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	browserURL, err := url.Parse(workbenchReceipt.BrowserURL)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := browserURL.Query().Get("rkc-workbench")
	if bootstrap != "" {
		t.Fatal("workbench bootstrap capability was placed in the HTTP query")
	}
	bootstrap = browserURL.Fragment
	values, err := url.ParseQuery(bootstrap)
	if err != nil || values.Get("rkc-workbench") == "" {
		t.Fatalf("workbench browser capability = %q, %v", workbenchReceipt.BrowserURL, err)
	}
	request, err := http.NewRequest(http.MethodGet, workbenchReceipt.URL+"/api/v1/workbench/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-RKC-Workbench-Bootstrap", values.Get("rkc-workbench"))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Enabled bool   `json:"enabled"`
		Token   string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !session.Enabled || session.Token == "" {
		t.Fatalf("workbench session = status %d, %+v", response.StatusCode, session)
	}
	close(priorityArrived)
	select {
	case err := <-workbenchDone:
		if !errors.Is(err, resourceguard.ErrHigherPriorityActive) {
			t.Fatalf("workbench priority preemption = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workbench did not stop after higher-priority work arrived")
	}

	if err := runServe([]string{"unexpected"}); err == nil {
		t.Fatal("serve positional argument succeeded")
	}
	if err := runServe([]string{"--workbench", "--addr", "0.0.0.0:0", "--dir", atlas}); err == nil ||
		!strings.Contains(err.Error(), "loopback") {
		t.Fatalf("remote workbench serve = %v", err)
	}
	if err := runServe([]string{"--dir", filepath.Join(root, "missing"), "--addr", "127.0.0.1:0"}); err == nil {
		t.Fatal("serve accepted a missing dataset")
	}
	if err := runServe([]string{"--dir", atlas, "--addr", "not-an-address"}); err == nil ||
		!strings.Contains(err.Error(), "listen") {
		t.Fatalf("malformed listen address = %v", err)
	}
	existingReady := filepath.Join(root, "existing-ready.json")
	writeTestFile(t, existingReady, "keep")
	if err := runServe([]string{
		"--dir", atlas, "--addr", "127.0.0.1:0", "--ready-file", existingReady,
	}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing readiness target = %v", err)
	}
}

func TestMonitorHigherPriorityIsTickDrivenAndCancellationSafe(t *testing.T) {
	t.Run("arrival", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ticks := make(chan time.Time)
		priorityError := fmt.Errorf("%w: fixture", resourceguard.ErrHigherPriorityActive)
		checks := 0
		done := make(chan error, 1)
		go func() {
			done <- monitorHigherPriority(ctx, ticks, func() error {
				checks++
				if checks == 2 {
					return priorityError
				}
				return nil
			})
		}()
		ticks <- time.Now()
		ticks <- time.Now()
		select {
		case err := <-done:
			if !errors.Is(err, resourceguard.ErrHigherPriorityActive) {
				t.Fatalf("monitor arrival error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("monitor did not report the injected higher-priority arrival")
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ticks := make(chan time.Time)
		called := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- monitorHigherPriority(ctx, ticks, func() error {
				called <- struct{}{}
				return nil
			})
		}()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("monitor cancellation = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("monitor did not stop after cancellation")
		}
		select {
		case <-called:
			t.Fatal("priority check ran without a tick")
		default:
		}
	})
}

func TestServePrioritySafetyIsWorkbenchOnlyAndPrecedesDatasetLoading(t *testing.T) {
	root := t.TempDir()
	missingAtlas := filepath.Join(root, "missing-atlas")
	ready := filepath.Join(root, "ready.json")
	priorityError := fmt.Errorf("%w: admission fixture", resourceguard.ErrHigherPriorityActive)
	checks := 0
	err := runServeWithSafety(context.Background(), []string{
		"--workbench", "--dir", missingAtlas, "--ready-file", ready,
	}, serveSafetyChecks{
		requireLowPriority: func() error { return nil },
		checkHigherPriority: func() error {
			checks++
			return priorityError
		},
		priorityCheckInterval: time.Second,
	})
	if !errors.Is(err, resourceguard.ErrHigherPriorityActive) {
		t.Fatalf("workbench priority admission = %v", err)
	}
	if checks != 1 {
		t.Fatalf("workbench admission checks = %d, want 1", checks)
	}
	if _, statErr := os.Stat(ready); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("priority rejection created readiness state: %v", statErr)
	}

	envelopeChecks := 0
	err = runServeWithSafety(context.Background(), []string{
		"--workbench", "--dir", missingAtlas, "--addr", "127.0.0.1:8787", "--ready-file", ready,
	}, serveSafetyChecks{
		requireLowPriority: func() error {
			envelopeChecks++
			return nil
		},
		checkHigherPriority:   func() error { return nil },
		priorityCheckInterval: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "ephemeral port 0") {
		t.Fatalf("stable-origin workbench admission = %v", err)
	}
	if envelopeChecks != 0 {
		t.Fatalf("stable-origin rejection ran %d safety checks before address admission", envelopeChecks)
	}

	// Static serving has no command execution surface, so its portable path
	// must not acquire Linux-only workbench policy dependencies.
	err = runServeWithSafety(context.Background(), []string{
		"--dir", missingAtlas, "--addr", "127.0.0.1:0",
	}, serveSafetyChecks{})
	if err == nil || strings.Contains(err.Error(), "safety checks") {
		t.Fatalf("static serve safety isolation = %v", err)
	}
}
