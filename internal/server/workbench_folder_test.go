package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceStartsEmptyAndPortableFolderJobActivates(t *testing.T) {
	workspace := t.TempDir()
	folder := filepath.Join(workspace, "example")
	prepared := filepath.Join(folder, ".prepared-rkc")
	writeWorkbenchAtlasFixture(t, folder, prepared, "example", "snapshot-example", "LoginService")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	workbench, err := NewWorkbench(WorkbenchConfig{Workspace: workspace, Executable: executable, Timeout: 5 * time.Second, CompileFolder: func(ctx context.Context, path string) error {
		calls++
		if path != folder {
			return errors.New("wrong source folder")
		}
		return os.Rename(prepared, filepath.Join(path, ".rkc"))
	}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := NewWorkspaceDataset()
	if err != nil {
		t.Fatal(err)
	}
	handler := empty.HandlerWithWorkbench(workbench)
	if len(empty.Bundle.Nodes) != 0 || empty.Manifest.Metadata["rkc_workspace"] != "empty" || empty.Integrity != "workspace" {
		t.Fatal("workspace claimed compiled content")
	}
	if calls != 0 {
		t.Fatal("startup scanned a folder")
	}
	for _, path := range []string{"/", "/api/v1/health", "/api/v1/nodes", "/api/v1/diagnostics"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 200 {
			t.Fatalf("workspace %s: %d", path, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil)
	request.Header.Set(workbenchBootstrapHeader, workbench.bootstrap)
	response := httptest.NewRecorder()
	workbench.handleSession(response, request)
	var session struct {
		FolderOnly bool               `json:"folder_compilation_only"`
		Commands   []workbenchCommand `json:"commands"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if !session.FolderOnly {
		t.Fatal("portable execution profile missing")
	}
	for _, command := range session.Commands {
		if command.DefaultExecutable != (command.Name == "quickstart") {
			t.Fatalf("unsupported runnable command %s", command.Name)
		}
	}
	job, err := workbench.createJob([]string{"quickstart", "example"})
	if err != nil {
		t.Fatal(err)
	}
	workbench.runJob(job.ID)
	completed := waitForWorkbenchJob(t, workbench, job.ID, time.Second)
	if completed.Status != "succeeded" || completed.CleanupScope != "in_process" || completed.ActivatedDataset == nil || completed.ActivatedDataset.SnapshotID != "snapshot-example" {
		t.Fatalf("portable activation: %+v", completed)
	}
	if calls != 1 || len(workbench.slot) != 0 {
		t.Fatal("scan count or retained execution slot")
	}
}

func TestPortableFolderJobsRejectEveryCommandAndOptionEscape(t *testing.T) {
	workbench := &Workbench{workspace: t.TempDir(), compileFolder: func(context.Context, string) error { t.Fatal("unauthorized callback"); return nil }}
	for _, args := range [][]string{{}, {"quickstart"}, {"version"}, {"quickstart", "--python"}, {"quickstart", "--config", "custom.json", "."}, {"scan", "--no-python", "."}, {"quickstart", "https://github.com/example/repo"}, {"quickstart", "git@example.com:repo"}} {
		if _, err := workbench.createJob(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestPortableFolderCancellationWaitsForCallbackAndReleasesSlot(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	workbench, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: executable, Timeout: 5 * time.Second, CompileFolder: func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := workbench.createJob([]string{"quickstart", "."})
	if err != nil {
		t.Fatal(err)
	}
	go workbench.runJob(job.ID)
	<-entered
	done := make(chan error, 1)
	go func() { done <- workbench.CancelJob(job.ID) }()
	<-canceled
	select {
	case err := <-done:
		t.Fatalf("cancellation returned before callback: %v", err)
	default:
	}
	if len(workbench.slot) != 1 {
		t.Fatal("callback lost execution slot before returning")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not finish")
	}
	completed := waitForWorkbenchJob(t, workbench, job.ID, time.Second)
	if completed.Status != "canceled" || len(workbench.slot) != 0 {
		t.Fatalf("cancellation contract: %+v", completed)
	}
}

func TestPortableFolderFailureDoesNotActivate(t *testing.T) {
	for _, callbackErr := range []error{errors.New("compile failed"), nil} {
		workbench := &Workbench{workspace: t.TempDir(), timeout: time.Second, slot: make(chan struct{}, 1), jobs: map[string]*workbenchJob{}, compileFolder: func(context.Context, string) error { return callbackErr }}
		empty, err := NewWorkspaceDataset()
		if err != nil {
			t.Fatal(err)
		}
		workbench.attachDataset(empty)
		job, err := workbench.createJob([]string{"quickstart", "."})
		if err != nil {
			t.Fatal(err)
		}
		workbench.runJob(job.ID)
		completed := waitForWorkbenchJob(t, workbench, job.ID, time.Second)
		if completed.Status != "failed" || completed.ActivatedDataset != nil || workbench.activeDataset != empty || len(workbench.slot) != 0 {
			t.Fatalf("invalid publication: %+v", completed)
		}
		if callbackErr == nil && !strings.Contains(completed.Error, "not activated") {
			t.Fatal("missing activation failure")
		}
	}
}
