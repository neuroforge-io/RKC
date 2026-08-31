package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

// errOnCallContext makes cancellation checkpoints deterministic without
// exposing a test hook in production code. Done deliberately stays nil so the
// synchronous Err checks, rather than a select race, decide the outcome.
type errOnCallContext struct {
	mu       sync.Mutex
	calls    int
	cancelAt int
	err      error
}

func (*errOnCallContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*errOnCallContext) Done() <-chan struct{}       { return nil }
func (*errOnCallContext) Value(any) any               { return nil }

func (ctx *errOnCallContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	ctx.calls++
	if ctx.cancelAt > 0 && ctx.calls >= ctx.cancelAt {
		return ctx.err
	}
	return nil
}

func TestLoadStoreCancellationCheckpointsFailBeforePublication(t *testing.T) {
	bundle := richDataset().Bundle
	reader := &serverStoreReader{bundle: bundle, coverage: model.BuildCoverage(bundle)}
	for checkpoint := 1; checkpoint <= 5; checkpoint++ {
		t.Run(string(rune('0'+checkpoint)), func(t *testing.T) {
			ctx := &errOnCallContext{cancelAt: checkpoint, err: context.Canceled}
			dataset, err := LoadStore(ctx, reader, rkcstore.SnapshotID(bundle.Snapshot.ID))
			if dataset != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("checkpoint %d returned dataset=%p err=%v", checkpoint, dataset, err)
			}
		})
	}
}

func TestWorkbenchPointerAndBrowserCapabilityBoundaries(t *testing.T) {
	primary := richDataset()
	secondary := richDataset()
	secondary.Manifest.ID = "snapshot-secondary"

	var nilWorkbench *Workbench
	nilWorkbench.attachDataset(primary)
	if got := nilWorkbench.currentDataset(primary); got != primary {
		t.Fatalf("nil workbench current dataset = %p, want fallback %p", got, primary)
	}
	if _, err := nilWorkbench.BrowserURL("http://127.0.0.1:8080/"); err == nil {
		t.Fatal("nil workbench produced a browser URL")
	}

	workbench := &Workbench{jobs: map[string]*workbenchJob{}}
	workbench.attachDataset(nil)
	if got := workbench.currentDataset(primary); got != primary {
		t.Fatalf("unbound current dataset = %p, want fallback %p", got, primary)
	}
	workbench.attachDataset(primary)
	workbench.attachDataset(secondary)
	if got := workbench.currentDataset(secondary); got != primary {
		t.Fatalf("second attachment replaced immutable initial dataset: %p", got)
	}

	workbench.bootstrap = "single-use-capability"
	for _, rawURL := range []string{
		"https://127.0.0.1:8080/",
		"http:///missing-host",
		"http://127.0.0.1:8080/#already-fragmented",
		"://malformed",
	} {
		if _, err := workbench.BrowserURL(rawURL); err == nil {
			t.Errorf("BrowserURL(%q) accepted an invalid origin", rawURL)
		}
	}
	got, err := workbench.BrowserURL("http://127.0.0.1:8080/")
	if err != nil || !strings.Contains(got, "#rkc-workbench=single-use-capability") {
		t.Fatalf("valid BrowserURL = %q, %v", got, err)
	}
	workbench.bootstrap = ""
	if _, err := workbench.BrowserURL("http://127.0.0.1:8080/"); err == nil {
		t.Fatal("consumed bootstrap capability was reused")
	}
}

func TestNewWorkbenchRejectsDanglingAndNonRegularIdentities(t *testing.T) {
	workspace := t.TempDir()
	missingExecutable := filepath.Join(t.TempDir(), "missing-rkc")
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: workspace, Executable: missingExecutable, Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "executable links") {
		t.Fatalf("missing executable error = %v", err)
	}
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: workspace, Executable: t.TempDir(), Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "executable regular file") {
		t.Fatalf("directory executable error = %v", err)
	}

	t.Run("dangling workspace symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "workspace-link")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := NewWorkbench(WorkbenchConfig{Workspace: link, Executable: os.Args[0], Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "workspace links") {
			t.Fatalf("dangling workspace symlink error = %v", err)
		}
	})
	t.Run("dangling executable symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "executable-link")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := NewWorkbench(WorkbenchConfig{Workspace: workspace, Executable: link, Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "executable links") {
			t.Fatalf("dangling executable symlink error = %v", err)
		}
	})
}

func TestCompletedQuickstartRootsRejectAmbiguityAndInvalidFolders(t *testing.T) {
	workspace := t.TempDir()
	workbench := &Workbench{workspace: workspace}
	for _, args := range [][]string{
		nil,
		{"help"},
		{"quickstart", "--python"},
		{"quickstart", "repo", "extra"},
	} {
		if repository, atlas, requested, err := workbench.completedQuickstartRoots(args); err != nil || requested || repository != "" || atlas != "" {
			t.Errorf("manual argv %q was treated as graphical activation: %q %q %t %v", args, repository, atlas, requested, err)
		}
	}

	repository := filepath.Join(workspace, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, atlas, requested, err := workbench.completedQuickstartRoots([]string{"quickstart", "repository"})
	if err != nil || !requested || resolved != repository || atlas != filepath.Join(repository, ".rkc") {
		t.Fatalf("relative quickstart roots = %q %q %t %v", resolved, atlas, requested, err)
	}

	missing := filepath.Join(workspace, "missing")
	if _, _, requested, err := workbench.completedQuickstartRoots([]string{"quickstart", missing}); !requested || err == nil || !strings.Contains(err.Error(), "links") {
		t.Fatalf("missing selected folder = requested:%t err:%v", requested, err)
	}
	file := filepath.Join(workspace, "ordinary-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, requested, err := workbench.completedQuickstartRoots([]string{"quickstart", file}); !requested || err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("selected file = requested:%t err:%v", requested, err)
	}
}

func TestQuickstartActivationIsAtomicAcrossEveryStatePrecondition(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkbenchAtlasFixture(t, target, filepath.Join(target, ".rkc"), filepath.Base(target), "snapshot-target", "Target")
	initial := richDataset()

	tests := []struct {
		name       string
		configure  func(*Workbench, *workbenchJob)
		want       string
		wantActive *Dataset
	}{
		{name: "missing job", configure: func(workbench *Workbench, _ *workbenchJob) { delete(workbench.jobs, "job") }, want: "no longer active", wantActive: initial},
		{name: "terminal job", configure: func(_ *Workbench, job *workbenchJob) { job.Status = "succeeded" }, want: "no longer active", wantActive: initial},
		{name: "canceled job", configure: func(_ *Workbench, job *workbenchJob) { job.cancel() }, want: "activation was canceled", wantActive: initial},
		{name: "closing workbench", configure: func(workbench *Workbench, _ *workbenchJob) { workbench.closed = true }, want: "shutting down", wantActive: initial},
		{name: "unbound dataset", configure: func(workbench *Workbench, _ *workbenchJob) { workbench.activeDataset = nil }, want: "shutting down", wantActive: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobContext, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := &workbenchJob{ID: "job", Status: "running", context: jobContext, cancel: cancel, done: make(chan struct{})}
			workbench := &Workbench{
				workspace: t.TempDir(), jobs: map[string]*workbenchJob{"job": job},
				activeDataset: initial,
			}
			test.configure(workbench, job)
			err := workbench.activateCompletedQuickstart("job", []string{"quickstart", target})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("activation error = %v, want %q", err, test.want)
			}
			if workbench.activeDataset != test.wantActive || job.ActivatedDataset != nil {
				t.Fatalf("failed activation published state: dataset=%p activated=%+v", workbench.activeDataset, job.ActivatedDataset)
			}
		})
	}
}

func TestQuickstartActivationRejectsUnverifiedOrWrongRepositoryOutput(t *testing.T) {
	t.Run("missing atlas", func(t *testing.T) {
		repository := filepath.Join(t.TempDir(), "repository")
		if err := os.Mkdir(repository, 0o700); err != nil {
			t.Fatal(err)
		}
		workbench := &Workbench{workspace: t.TempDir(), jobs: map[string]*workbenchJob{}, activeDataset: richDataset()}
		if err := workbench.activateCompletedQuickstart("missing", []string{"quickstart", repository}); err == nil || !strings.Contains(err.Error(), "load analyzed atlas") {
			t.Fatalf("missing atlas error = %v", err)
		}
	})

	t.Run("wrong root name", func(t *testing.T) {
		repository := filepath.Join(t.TempDir(), "repository")
		if err := os.Mkdir(repository, 0o700); err != nil {
			t.Fatal(err)
		}
		writeWorkbenchAtlasFixture(t, repository, filepath.Join(repository, ".rkc"), "different-root", "snapshot-wrong-root", "Wrong")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		workbench := &Workbench{workspace: t.TempDir(), jobs: map[string]*workbenchJob{
			"job": {ID: "job", Status: "running", context: ctx, cancel: cancel, done: make(chan struct{})},
		}, activeDataset: richDataset()}
		if err := workbench.activateCompletedQuickstart("job", []string{"quickstart", repository}); err == nil || !strings.Contains(err.Error(), "does not match selected folder") {
			t.Fatalf("wrong-root activation error = %v", err)
		}
	})

	t.Run("legacy unverified atlas", func(t *testing.T) {
		repository := filepath.Join(t.TempDir(), "repository")
		atlas := filepath.Join(repository, ".rkc")
		graphRoot := filepath.Join(atlas, "graph")
		if err := os.MkdirAll(graphRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		bundle := richDataset().Bundle
		bundle.Snapshot.RootName = filepath.Base(repository)
		writeServerJSON(t, filepath.Join(atlas, "rkc.manifest.json"), bundle.Snapshot)
		writeServerJSONL(t, filepath.Join(graphRoot, "artifacts.jsonl"), bundle.Artifacts)
		writeServerJSONL(t, filepath.Join(graphRoot, "nodes.jsonl"), bundle.Nodes)
		writeServerJSONL(t, filepath.Join(graphRoot, "edges.jsonl"), bundle.Edges)
		writeServerJSONL(t, filepath.Join(graphRoot, "evidence.jsonl"), bundle.Evidence)
		writeServerJSONL(t, filepath.Join(graphRoot, "diagnostics.jsonl"), bundle.Diagnostics)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		workbench := &Workbench{workspace: t.TempDir(), jobs: map[string]*workbenchJob{
			"job": {ID: "job", Status: "running", context: ctx, cancel: cancel, done: make(chan struct{})},
		}, activeDataset: richDataset()}
		if err := workbench.activateCompletedQuickstart("job", []string{"quickstart", repository}); err == nil || !strings.Contains(err.Error(), "integrity") {
			t.Fatalf("legacy activation error = %v", err)
		}
	})
}

func TestRunJobCancellationCheckpointsAndStartFailure(t *testing.T) {
	writeExecutable := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "rkc-test")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, cancelAt := range []int{1, 2, 3} {
		t.Run("cancel checkpoint "+string(rune('0'+cancelAt)), func(t *testing.T) {
			executable := writeExecutable(t)
			workbench, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: executable, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			job, err := workbench.createJob([]string{"help"})
			if err != nil {
				t.Fatal(err)
			}
			ctx := &errOnCallContext{cancelAt: cancelAt, err: context.Canceled}
			workbench.mu.Lock()
			workbench.jobs[job.ID].context = ctx
			workbench.jobs[job.ID].cancel = func() {}
			workbench.mu.Unlock()
			workbench.runJob(job.ID)
			finished, ok := workbench.jobSnapshot(job.ID)
			if !ok || finished.Status != "canceled" || finished.FinishedAt == nil {
				t.Fatalf("checkpoint %d job = %+v", cancelAt, finished)
			}
		})
	}

	t.Run("executable removed before start", func(t *testing.T) {
		executable := writeExecutable(t)
		workbench, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: executable, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		job, err := workbench.createJob([]string{"help"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(executable); err != nil {
			t.Fatal(err)
		}
		workbench.runJob(job.ID)
		finished, ok := workbench.jobSnapshot(job.ID)
		if !ok || finished.Status != "failed" || finished.Error == "" {
			t.Fatalf("removed executable job = %+v", finished)
		}
	})

	t.Run("nonqueued job cannot start", func(t *testing.T) {
		workbench, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: writeExecutable(t), Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		job, err := workbench.createJob([]string{"help"})
		if err != nil {
			t.Fatal(err)
		}
		workbench.jobs[job.ID].Status = "running"
		workbench.runJob(job.ID)
		if got := workbench.jobs[job.ID].Status; got != "running" {
			t.Fatalf("nonqueued status changed to %q", got)
		}
	})
}

func TestFinishJobFromContextPreservesCleanupAndOutputTruth(t *testing.T) {
	deadlineContext, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	canceledContext, cancelCanceled := context.WithCancel(context.Background())
	cancelCanceled()

	tests := []struct {
		name       string
		ctx        context.Context
		commandErr error
		managed    bool
		wantStatus string
		wantError  string
	}{
		{name: "deadline cleanup unproven", ctx: deadlineContext, commandErr: ErrWorkbenchCleanupUnproven, wantStatus: "cleanup_failed", wantError: "timed out"},
		{name: "cancel cleanup unproven", ctx: canceledContext, commandErr: ErrWorkbenchCleanupUnproven, wantStatus: "cleanup_failed", wantError: "canceled"},
		{name: "managed service risk", ctx: canceledContext, managed: true, wantStatus: "cleanup_failed", wantError: "separately managed services"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now().UTC()
			jobContext, cancel := context.WithCancel(test.ctx)
			defer cancel()
			job := &workbenchJob{
				ID: "job", Status: "running", context: jobContext, cancel: cancel,
				done: make(chan struct{}), mayLaunchManagedUnits: test.managed,
			}
			if test.managed {
				job.StartedAt = &started
			}
			workbench := &Workbench{jobs: map[string]*workbenchJob{"job": job}}
			var output boundedWorkbenchBuffer
			output.buffer.WriteString("partial output")
			output.truncated = true
			workbench.finishJobFromContext("job", test.commandErr, &output)
			finished := workbench.jobs["job"]
			if finished.Status != test.wantStatus || !strings.Contains(finished.Error, test.wantError) || finished.Output != "partial output" || !finished.Truncated {
				t.Fatalf("finished job = %+v", finished)
			}
			select {
			case <-finished.done:
			default:
				t.Fatal("finished job did not close completion channel")
			}
		})
	}
}

func TestWorkbenchHTTPBoundariesReportCapacityAuthorizationAndCleanup(t *testing.T) {
	workbench, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: os.Args[0], Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/jobs/secret", nil)
	unauthorized.SetPathValue("jobID", "secret")
	response := httptest.NewRecorder()
	workbench.handleJob(response, unauthorized)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthorized job status = %d", response.Code)
	}

	finished := time.Now().UTC()
	workbench.jobs["cleanup"] = &workbenchJob{ID: "cleanup", Status: "cleanup_failed", FinishedAt: &finished}
	request := authorizedWorkbenchRequest(workbench, http.MethodDelete, "/api/v1/workbench/jobs/cleanup", "")
	request.SetPathValue("jobID", "cleanup")
	response = httptest.NewRecorder()
	workbench.handleCancelJob(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "cleanup could not be proven") {
		t.Fatalf("cleanup-failed cancel status=%d body=%s", response.Code, response.Body.String())
	}

	for index := 0; index < workbenchMaximumJobs; index++ {
		id := "active-" + string(rune('A'+index))
		workbench.jobs[id] = &workbenchJob{ID: id, Status: "running"}
	}
	response = httptest.NewRecorder()
	workbench.handleJobs(response, authorizedWorkbenchRequest(workbench, http.MethodPost, "/api/v1/workbench/jobs", `{"args":["help"]}`))
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "Workbench is full") {
		t.Fatalf("capacity status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSnapshotBoundFiltersRejectEveryMismatchedFacet(t *testing.T) {
	dataset := richDataset()
	handler := dataset.Handler()
	for _, target := range []string{
		"/api/v1/artifacts?language=rust",
		"/api/v1/artifacts?status=failed",
		"/api/v1/artifacts?path_prefix=vendor/",
		"/api/v1/nodes?kind=class",
		"/api/v1/nodes?language=rust",
		"/api/v1/edges?kind=imports",
		"/api/v1/edges?from=missing",
		"/api/v1/edges?to=missing",
		"/api/v1/edges?resolution=runtime_observed",
		"/api/v1/diagnostics?severity=error",
		"/api/v1/diagnostics?code=OTHER",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Errorf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
		if got := response.Header().Get(snapshotGenerationHeader); got != dataset.Manifest.ID {
			t.Errorf("%s snapshot header=%q want %q", target, got, dataset.Manifest.ID)
		}
		if !strings.Contains(response.Body.String(), `"total":0`) {
			t.Errorf("%s returned mismatched canonical records: %s", target, response.Body.String())
		}
	}
}

func TestGeneratedBrowserAndBoundedFileAdversarialInputs(t *testing.T) {
	var nilDataset *Dataset
	if err := nilDataset.PrepareWorkbenchBrowser(); err == nil {
		t.Fatal("nil dataset prepared a browser")
	}
	trusted := &Dataset{staticSiteTrusted: true}
	if err := trusted.PrepareWorkbenchBrowser(); err != nil {
		t.Fatalf("trusted browser regeneration = %v", err)
	}
	if _, err := captureGeneratedBrowser(nil); err == nil {
		t.Fatal("empty generated browser was accepted")
	}
	for _, path := range []string{"../app.js", "/absolute.js", "a/../app.js", "."} {
		if _, err := captureGeneratedBrowser(map[string][]byte{path: []byte("safe")}); err == nil {
			t.Errorf("unsafe generated browser path %q was accepted", path)
		}
	}

	root := t.TempDir()
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if file, _, err := openBoundedDatasetFile(root, "directory", 1024); err == nil || file != nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("opened directory as dataset file: file=%v err=%v", file, err)
	}
	path := filepath.Join(root, "data")
	if err := os.WriteFile(path, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, before, err := openBoundedDatasetFile(root, "data", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.WriteFile(path, []byte("changed-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBoundedDatasetFile(root, "data", file, before, int64(len("initial"))); err == nil || !strings.Contains(err.Error(), "changed while reading") {
		t.Fatalf("changed file verification error = %v", err)
	}
}
