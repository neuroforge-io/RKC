package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/commandcatalog"
	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

func TestWorkbenchCloseCancelsActiveJobsAndRejectsSubmissions(t *testing.T) {
	var nilWorkbench *Workbench
	if err := nilWorkbench.Close(context.Background()); err != nil {
		t.Fatalf("nil Close() = %v", err)
	}

	workspace := t.TempDir()
	ready := filepath.Join(workspace, "ready")
	executable := filepath.Join(t.TempDir(), "close")
	script := "#!/bin/sh\nprintf ready > \"$2\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: executable, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := workbench.Close(nil); err == nil {
		t.Fatal("Close(nil) succeeded")
	}
	job, err := workbench.createJob([]string{"help", ready})
	if err != nil {
		t.Fatal(err)
	}
	go workbench.runJob(job.ID)
	waitForFile(t, ready, 2*time.Second)

	closeContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := workbench.Close(closeContext); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	completed := waitForWorkbenchJob(t, workbench, job.ID, time.Second)
	if completed.Status != "canceled" || completed.FinishedAt == nil {
		t.Fatalf("closed active job = %+v", completed)
	}
	if _, err := workbench.createJob([]string{"help"}); !errors.Is(err, ErrWorkbenchClosed) {
		t.Fatalf("create after Close() = %v", err)
	}
	response := httptest.NewRecorder()
	workbench.handleJobs(response, authorizedWorkbenchRequest(
		workbench, http.MethodPost, "/api/v1/workbench/jobs", `{"args":["help"]}`,
	))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed submission status = %d body=%s", response.Code, response.Body.String())
	}
	if err := workbench.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close() = %v", err)
	}
}

func TestWorkbenchCloseReportsUnprovenCleanup(t *testing.T) {
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: t.TempDir(), Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	exitCode := -1
	workbench.jobs["failed-cleanup"] = &workbenchJob{
		ID: "failed-cleanup", Status: "cleanup_failed", FinishedAt: &finished,
		ExitCode: &exitCode,
	}
	err = workbench.Close(context.Background())
	if !errors.Is(err, ErrWorkbenchCleanupUnproven) ||
		!strings.Contains(err.Error(), "failed-cleanup") {
		t.Fatalf("Close() cleanup failure = %v", err)
	}
}

func TestWorkbenchCloseHonorsCallerDeadline(t *testing.T) {
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: t.TempDir(), Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobContext, cancelJob := context.WithCancel(context.Background())
	defer cancelJob()
	workbench.jobs["stuck"] = &workbenchJob{
		ID: "stuck", Status: "running", context: jobContext,
		cancel: cancelJob, done: make(chan struct{}),
	}
	closeContext, cancelClose := context.WithCancel(context.Background())
	cancelClose()
	err = workbench.Close(closeContext)
	if !errors.Is(err, ErrWorkbenchCleanupUnproven) ||
		!strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("Close() deadline failure = %v", err)
	}
}

func TestWorkbenchManagedUnitRiskClassification(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "empty"},
		{name: "answer", args: []string{"answer"}, want: true},
		{name: "doctor", args: []string{"doctor", "--repository", "."}, want: true},
		{name: "doctor config", args: []string{"doctor", "--config", "rkc.json"}, want: true},
		{name: "doctor help", args: []string{"doctor", "--help"}},
		{name: "doctor short help", args: []string{"doctor", "-h"}},
		{name: "synthesize", args: []string{"synthesize"}, want: true},
		{name: "packet only", args: []string{"synthesize", "--packet-only"}},
		{name: "semantic", args: []string{"query", "--mode", "semantic"}, want: true},
		{name: "hybrid equals", args: []string{"query", "--mode=hybrid"}, want: true},
		{name: "lexical", args: []string{"query", "--mode", "lexical"}},
		{name: "build vector", args: []string{"query", "--build-vector-index"}, want: true},
		{name: "embedding model", args: []string{"query", "--embedding-model=x"}, want: true},
		{name: "embedding asset", args: []string{"query", "--embedding-asset", "x"}, want: true},
		{name: "embedding receipt", args: []string{"query", "--embedding-runtime-receipt=x"}, want: true},
		{name: "scan", args: []string{"scan"}, want: true},
		{name: "scan without python", args: []string{"scan", "--no-python"}},
		{name: "scan without plugins", args: []string{"scan", "--no-plugins"}},
		{name: "scan custom git despite no plugins", args: []string{"scan", "--no-plugins", "--git", "/tmp/helper", "."}, want: true},
		{name: "scan config despite no python", args: []string{"scan", "--no-python", "--config=config.json", "."}, want: true},
		{name: "scan custom python despite no plugins", args: []string{"scan", "--no-plugins", "--python=/tmp/helper", "."}, want: true},
		{name: "scan custom python plugin despite no plugins", args: []string{"scan", "--no-plugins", "--python-plugin=/tmp/helper.py", "."}, want: true},
		{name: "scan HTTPS remote", args: []string{"scan", "--no-python", "https://example.test/repo.git"}, want: true},
		{name: "scan SCP remote", args: []string{"scan", "--no-plugins", "git@example.test:repo.git"}, want: true},
		{name: "quickstart python", args: []string{"quickstart", "--python"}, want: true},
		{name: "quickstart config", args: []string{"quickstart", "--config", "rkc.json", "."}, want: true},
		{name: "quickstart", args: []string{"quickstart"}},
		{name: "history build", args: []string{"history", "build", "--dir", "."}, want: true},
		{name: "history symbol", args: []string{"history", "symbol", "--name", "main", "--dir", "."}, want: true},
		{name: "history report", args: []string{"history", "report", "--history", ".rkc-history.json"}},
		{name: "history help", args: []string{"history", "--help"}},
		{name: "trace capture", args: []string{"trace", "capture", "--dir", ".", "--", "go", "test", "./..."}, want: true},
		{name: "trace capture help", args: []string{"trace", "capture", "--help"}, want: true},
		{name: "trace report", args: []string{"trace", "report", "--dir", ".rkc"}},
		{name: "trace verify", args: []string{"trace", "verify", "--trace", ".rkc-trace.json"}},
		{name: "trace help", args: []string{"trace", "help"}},
		{name: "wizard", args: []string{"wizard"}, want: true},
		{name: "wizard folder", args: []string{"wizard", "."}, want: true},
		{name: "wizard help", args: []string{"wizard", "--help"}},
		{name: "other", args: []string{"help"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := workbenchMayLaunchManagedUnits(test.args); got != test.want {
				t.Fatalf("workbenchMayLaunchManagedUnits(%q) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}

func TestWorkbenchExecutionRejectsCommandsThatCanEscapeAggregateCeiling(t *testing.T) {
	for _, arguments := range [][]string{
		{"answer"},
		{"doctor", "--repository", "."},
		{"doctor", "--config", "rkc.json"},
		{"synthesize", "--dir", ".rkc"},
		{"synthesize", "--query", "--packet-only=true", "--dir", ".rkc"},
		{"synthesize", "--packet-only=true", "--packet-only=false", "--dir", ".rkc"},
		{"query", "--mode", "semantic", "question"},
		{"query", "-mode", "semantic", "question"},
		{"query", "-embedding-model", "model.gguf", "question"},
		{"scan", "--python", "."},
		{"scan", "--no-plugins", "--git", "/tmp/helper", "."},
		{"scan", "--no-python", "--config=rkc.json", "."},
		{"scan", "--no-plugins", "--python=/tmp/helper", "."},
		{"scan", "--no-plugins", "--python-plugin=/tmp/helper.py", "."},
		{"scan", "--no-python", "https://example.test/repo.git"},
		{"scan", "--no-plugins", "git@example.test:repo.git"},
		{"scan", "--no-python=false", "."},
		{"scan", "-no-plugins=false", "."},
		{"quickstart", "-python", "."},
		{"quickstart", "--python=true", "."},
		{"quickstart", "--config", "rkc.json", "."},
		{"history", "build", "--dir", "."},
		{"history", "symbol", "--name", "main", "--dir", "."},
		{"trace", "capture", "--dir", ".", "--", "go", "test", "./..."},
		{"trace", "capture", "--help"},
		{"wizard"},
		{"wizard", "."},
	} {
		if err := validateWorkbenchExecution(arguments); err == nil || !strings.Contains(err.Error(), "aggregate resource ceiling") {
			t.Errorf("managed-unit command %q was not rejected: %v", arguments, err)
		}
	}
	for _, arguments := range [][]string{
		{"help"},
		{"doctor", "--help"},
		{"doctor", "-h"},
		{"query", "--dir", ".rkc", "question"},
		{"query", "-mode=lexical", "question"},
		{"synthesize", "--packet-only", "--dir", ".rkc"},
		{"synthesize", "-packet-only=true", "--dir", ".rkc"},
		{"scan", "--no-python", "."},
		{"scan", "-no-plugins=true", "."},
		{"quickstart", "--python=false", "."},
		{"history", "report", "--history", ".rkc-history.json"},
		{"history", "--help"},
		{"trace", "report", "--dir", ".rkc"},
		{"trace", "verify", "--trace", ".rkc-trace.json"},
		{"trace", "help"},
		{"wizard", "--help"},
	} {
		if err := validateWorkbenchExecution(arguments); err != nil {
			t.Errorf("bounded command %q was rejected: %v", arguments, err)
		}
	}
}

func TestWorkbenchRejectsTraceCaptureSubmissionBeforeCreatingJob(t *testing.T) {
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: t.TempDir(), Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := authorizedWorkbenchRequest(
		workbench,
		http.MethodPost,
		"/api/v1/workbench/jobs",
		`{"args":["trace","capture","--dir",".","--","go","test","./..."]}`,
	)
	response := httptest.NewRecorder()
	workbench.handleJobs(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "aggregate resource ceiling") {
		t.Fatalf("trace capture submission status=%d body=%s", response.Code, response.Body.String())
	}
	workbench.mu.RLock()
	jobs := len(workbench.jobs)
	workbench.mu.RUnlock()
	if jobs != 0 {
		t.Fatalf("rejected trace capture created %d jobs", jobs)
	}
}

func TestWorkbenchRejectsExternalHelperRoutesBeforeCreatingJob(t *testing.T) {
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: t.TempDir(), Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"doctor", "--repository", "."},
		{"doctor", "--config", "rkc.json"},
		{"scan", "--no-plugins", "--git", "/tmp/helper", "."},
		{"scan", "--no-python", "--config=rkc.json", "."},
		{"scan", "--no-plugins", "--python=/tmp/helper", "."},
		{"scan", "--no-python", "https://example.test/repo.git"},
		{"scan", "--no-plugins", "git@example.test:repo.git"},
		{"quickstart", "--config", "rkc.json", "."},
		{"history", "build", "--dir", "."},
		{"history", "symbol", "--name", "main", "--dir", "."},
	} {
		encoded, err := json.Marshal(map[string]any{"args": arguments})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		workbench.handleJobs(response, authorizedWorkbenchRequest(
			workbench, http.MethodPost, "/api/v1/workbench/jobs", string(encoded),
		))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "aggregate resource ceiling") {
			t.Errorf("external-helper submission %q status=%d body=%s", arguments, response.Code, response.Body.String())
		}
	}
	workbench.mu.RLock()
	jobs := len(workbench.jobs)
	workbench.mu.RUnlock()
	if jobs != 0 {
		t.Fatalf("rejected external-helper routes created %d jobs", jobs)
	}
}

func TestWorkbenchRejectsCustomOrUnpinnedSCIPIndexerExecution(t *testing.T) {
	for _, arguments := range [][]string{
		{"scan", "--no-python", "--scip-generate", "go", "--scip-tool", "/usr/bin/setsid", "."},
		{"scan", "-scip-tool=/usr/bin/setsid", "."},
		{"scan", "--scip-no-pin-check=false", "."},
		{"quickstart", "--scip-tool=/usr/bin/setsid", "."},
		{"quickstart", "-scip-no-pin-check", "."},
		{"scip", "generate", "--language", "go", "--tool", "/usr/bin/setsid", "."},
		{"scip", "generate", "-tool=/usr/bin/setsid", "."},
		{"scip", "generate", "--no-pin-check=false", "."},
	} {
		err := validateWorkbenchExecution(arguments)
		if err == nil || !strings.Contains(err.Error(), "custom or unpinned SCIP indexer execution") {
			t.Errorf("unsafe SCIP execution %q was not rejected: %v", arguments, err)
		}
	}
	for _, arguments := range [][]string{
		{"scan", "--no-python", "--scip-index", "index.scip", "."},
		{"scip", "verify", "--index", "index.scip"},
		{"scip", "pin", "--language", "go", "--tool", "/usr/bin/scip-go"},
	} {
		if err := validateWorkbenchExecution(arguments); err != nil {
			t.Errorf("pinned/import-only SCIP workflow %q was rejected: %v", arguments, err)
		}
	}
	for _, arguments := range [][]string{
		{"scan", "--no-python", "--scip-generate", "go", "."},
		{"scan", "--no-python", "-scip-generate=go", "."},
		{"quickstart", "--scip-generate", "go", "."},
		{"scip", "generate", "--language", "go", "."},
	} {
		if err := validateWorkbenchExecution(arguments); err == nil ||
			!strings.Contains(err.Error(), "aggregate resource ceiling") {
			t.Errorf("detaching SCIP generation %q was not rejected: %v", arguments, err)
		}
	}
}

func TestWorkbenchRejectsSetsidSCIPSubmissionBeforeCreatingJob(t *testing.T) {
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: t.TempDir(), Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := authorizedWorkbenchRequest(
		workbench,
		http.MethodPost,
		"/api/v1/workbench/jobs",
		`{"args":["scip","generate","--language","go","--tool","/usr/bin/setsid","--no-pin-check","."]}`,
	)
	response := httptest.NewRecorder()
	workbench.handleJobs(response, request)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "custom or unpinned SCIP indexer execution") {
		t.Fatalf("setsid SCIP submission status=%d body=%s", response.Code, response.Body.String())
	}
	workbench.mu.RLock()
	jobs := len(workbench.jobs)
	workbench.mu.RUnlock()
	if jobs != 0 {
		t.Fatalf("rejected setsid SCIP submission created %d jobs", jobs)
	}
}

func TestWorkbenchEnvironmentRejectsUnsafeSystemdAndCGOState(t *testing.T) {
	if _, err := sanitizedWorkbenchEnvironment([]string{"CGO_ENABLED=invalid"}); err == nil {
		t.Fatal("invalid CGO_ENABLED was accepted")
	}
	for _, environment := range [][]string{
		{"XDG_RUNTIME_DIR=/run/user/1000"},
		{"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"},
		{"XDG_RUNTIME_DIR=relative", "DBUS_SESSION_BUS_ADDRESS=unix:path=relative/bus"},
		{"XDG_RUNTIME_DIR=/tmp/../tmp", "DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/bus"},
	} {
		if _, err := sanitizedWorkbenchEnvironment(environment); err == nil {
			t.Errorf("unsafe systemd environment was accepted: %q", environment)
		}
	}
	insecure := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(insecure, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(insecure, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := sanitizedWorkbenchEnvironment([]string{
		"XDG_RUNTIME_DIR=" + insecure,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.ToSlash(filepath.Join(insecure, "bus")),
	}); err == nil {
		t.Fatal("insecure runtime directory was accepted")
	}
	private := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sanitizedWorkbenchEnvironment([]string{
		"XDG_RUNTIME_DIR=" + private,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.ToSlash(filepath.Join(private, "other")),
	}); err == nil {
		t.Fatal("off-path user bus was accepted")
	}
}

func TestWorkbenchRunsOneAuthenticatedBoundedCommand(t *testing.T) {
	workspace := t.TempDir()
	executable := filepath.Join(t.TempDir(), "rkc-fixture")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'fixture:%s\\n' \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: executable, Timeout: 5 * time.Second,
		CommandContext: commandcatalog.Context{
			DatasetArgs: []string{"--dir", "/tmp/exact atlas"},
			CheckArgs:   []string{"--coverage", "/tmp/exact atlas/coverage.json"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil)
	sessionRequest.Header.Set(workbenchBootstrapHeader, workbench.bootstrap)
	sessionResponse := httptest.NewRecorder()
	workbench.handleSession(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session status = %d: %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	var session struct {
		Token           string             `json:"token"`
		AuthorityNotice string             `json:"authority_notice"`
		Commands        []workbenchCommand `json:"commands"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Token == "" || len(session.Commands) < 10 {
		t.Fatalf("incomplete session: token=%q commands=%d", session.Token, len(session.Commands))
	}
	if !strings.Contains(session.AuthorityNotice, "trusted browser profile") ||
		!strings.Contains(session.AuthorityNotice, "cannot prove") {
		t.Fatalf("workbench authority notice is incomplete: %q", session.AuthorityNotice)
	}
	byName := make(map[string]workbenchCommand, len(session.Commands))
	for _, command := range session.Commands {
		byName[command.Name] = command
	}
	if got := strings.Join(byName["query"].DefaultArgs, "|"); got != "--dir|/tmp/exact atlas|resource guard" {
		t.Fatalf("dataset-aware query defaults = %q", got)
	}
	if got := strings.Join(byName["check"].DefaultArgs, "|"); got != "--coverage|/tmp/exact atlas/coverage.json" {
		t.Fatalf("dataset-aware check defaults = %q", got)
	}
	if byName["answer"].DefaultExecutable || !strings.Contains(byName["answer"].Restriction, "aggregate resource ceiling") {
		t.Fatalf("answer workbench boundary = %+v", byName["answer"])
	}
	if byName["doctor"].DefaultExecutable || !strings.Contains(byName["doctor"].Restriction, "aggregate resource ceiling") {
		t.Fatalf("doctor workbench boundary = %+v", byName["doctor"])
	}
	if !byName["synthesize"].DefaultExecutable || byName["synthesize"].Restriction != "" {
		t.Fatalf("packet-only synthesize preset was not admitted: %+v", byName["synthesize"])
	}

	submitRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/v1/workbench/jobs",
		strings.NewReader(`{"args":["help","fixture"]}`),
	)
	submitRequest.Header.Set("Origin", "http://127.0.0.1")
	submitRequest.Header.Set("Sec-Fetch-Site", "same-origin")
	submitRequest.Header.Set("X-RKC-Workbench-Token", session.Token)
	submitResponse := httptest.NewRecorder()
	workbench.handleJobs(submitResponse, submitRequest)
	if submitResponse.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d: %s", submitResponse.Code, submitResponse.Body.String())
	}
	var submitted workbenchJob
	if err := json.Unmarshal(submitResponse.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		jobRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/jobs/"+submitted.ID, nil)
		jobRequest.SetPathValue("jobID", submitted.ID)
		jobRequest.Header.Set("X-RKC-Workbench-Token", session.Token)
		jobResponse := httptest.NewRecorder()
		workbench.handleJob(jobResponse, jobRequest)
		if jobResponse.Code != http.StatusOK {
			t.Fatalf("job status = %d: %s", jobResponse.Code, jobResponse.Body.String())
		}
		var job workbenchJob
		if err := json.Unmarshal(jobResponse.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.Status == "succeeded" {
			if job.ExitCode == nil || *job.ExitCode != 0 || job.Output != "fixture:help fixture\n" {
				t.Fatalf("unexpected completed job: %+v", job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkbenchAnalyzeActivatesValidatedDatasetAcrossReadAPIsAndDefaults(t *testing.T) {
	workspace := t.TempDir()
	goRepository := filepath.Join(workspace, "sample-go")
	typeScriptRepository := filepath.Join(workspace, "sample-typescript")
	initialAtlas := filepath.Join(goRepository, ".rkc")
	preparedAtlas := filepath.Join(typeScriptRepository, ".prepared-rkc")
	writeWorkbenchAtlasFixture(t, goRepository, initialAtlas, "sample-go", "snapshot-sample-go", "LoginService")
	writeWorkbenchAtlasFixture(t, typeScriptRepository, preparedAtlas, "sample-typescript", "snapshot-sample-typescript", "AuthService")

	initial, err := Load(initialAtlas)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "rkc-analyze-fixture")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n[ \"$1\" = quickstart ] || exit 2\nmv \"$2/.prepared-rkc\" \"$2/.rkc\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: goRepository, Executable: executable, Timeout: 10 * time.Second,
		CommandContext: workbenchCommandContextForDataset(initial),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := initial.HandlerWithWorkbench(workbench)

	before := httptest.NewRecorder()
	handler.ServeHTTP(before, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/manifest", nil))
	if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), "snapshot-sample-go") {
		t.Fatalf("initial manifest status=%d body=%s", before.Code, before.Body.String())
	}
	if got := before.Header().Get(snapshotGenerationHeader); got != "snapshot-sample-go" {
		t.Fatalf("initial snapshot generation header = %q", got)
	}

	submit := httptest.NewRecorder()
	body, err := json.Marshal(map[string]any{"args": []string{"quickstart", typeScriptRepository}})
	if err != nil {
		t.Fatal(err)
	}
	handler.ServeHTTP(submit, authorizedWorkbenchRequest(workbench, http.MethodPost, "/api/v1/workbench/jobs", string(body)))
	if submit.Code != http.StatusAccepted {
		t.Fatalf("analyze submit status=%d body=%s", submit.Code, submit.Body.String())
	}
	var submitted workbenchJob
	if err := json.Unmarshal(submit.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	completed := waitForWorkbenchJob(t, workbench, submitted.ID, 10*time.Second)
	if completed.Status != "succeeded" || completed.ActivatedDataset == nil {
		t.Fatalf("analyze job did not activate: %+v", completed)
	}
	if completed.ActivatedDataset.SnapshotID != "snapshot-sample-typescript" ||
		completed.ActivatedDataset.RepositoryRoot != typeScriptRepository ||
		completed.ActivatedDataset.AtlasRoot != filepath.Join(typeScriptRepository, ".rkc") {
		t.Fatalf("activated identity = %+v", completed.ActivatedDataset)
	}

	for name, target := range map[string]string{
		"manifest": "/api/v1/manifest",
		"search":   "/api/v1/search?q=AuthService",
		"graph":    "/api/v1/graph/neighborhood?node_id=class-sample-typescript&max_depth=1&max_nodes=8",
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+target, nil))
			if got := response.Header().Get(snapshotGenerationHeader); got != "snapshot-sample-typescript" {
				t.Fatalf("activated %s snapshot generation header = %q", name, got)
			}
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "sample-typescript") && !strings.Contains(response.Body.String(), "AuthService") {
				t.Fatalf("activated %s status=%d body=%s", name, response.Code, response.Body.String())
			}
		})
	}

	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, authorizedWorkbenchRequest(workbench, http.MethodGet, "/api/v1/workbench/session", ""))
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("activated session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	var session struct {
		ActiveDataset *workbenchDatasetIdentity `json:"active_dataset"`
		Commands      []workbenchCommand        `json:"commands"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.ActiveDataset == nil || session.ActiveDataset.SnapshotID != "snapshot-sample-typescript" {
		t.Fatalf("session active dataset = %+v", session.ActiveDataset)
	}
	commands := make(map[string]workbenchCommand, len(session.Commands))
	for _, command := range session.Commands {
		commands[command.Name] = command
	}
	wantAtlas := filepath.Join(typeScriptRepository, ".rkc")
	if got := strings.Join(commands["query"].DefaultArgs, "|"); got != "--dir|"+wantAtlas+"|resource guard" {
		t.Fatalf("activated query defaults = %q", got)
	}
	if got := strings.Join(commands["flow"].DefaultArgs, "|"); got != "report|--dir|"+wantAtlas {
		t.Fatalf("activated flow defaults = %q", got)
	}
}

func TestWorkbenchAnalyzeFailsClosedBeforePublishingInvalidDataset(t *testing.T) {
	workspace := t.TempDir()
	initialRepository := filepath.Join(workspace, "sample-go")
	targetRepository := filepath.Join(workspace, "invalid-target")
	initialAtlas := filepath.Join(initialRepository, ".rkc")
	writeWorkbenchAtlasFixture(t, initialRepository, initialAtlas, "sample-go", "snapshot-sample-go", "LoginService")
	if err := os.MkdirAll(filepath.Join(targetRepository, ".prepared-rkc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRepository, ".prepared-rkc", "bundle.json"), []byte("not a verified atlas"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := Load(initialAtlas)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "rkc-invalid-analyze-fixture")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nmv \"$2/.prepared-rkc\" \"$2/.rkc\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{Workspace: initialRepository, Executable: executable, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	handler := initial.HandlerWithWorkbench(workbench)
	job, err := workbench.createJob([]string{"quickstart", targetRepository})
	if err != nil {
		t.Fatal(err)
	}
	go workbench.runJob(job.ID)
	completed := waitForWorkbenchJob(t, workbench, job.ID, 10*time.Second)
	if completed.Status != "failed" || completed.ActivatedDataset != nil || !strings.Contains(completed.Error, "not activated") {
		t.Fatalf("invalid analyze result = %+v", completed)
	}
	manifest := httptest.NewRecorder()
	handler.ServeHTTP(manifest, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/manifest", nil))
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Body.String(), "snapshot-sample-go") || strings.Contains(manifest.Body.String(), "invalid-target") {
		t.Fatalf("invalid activation changed live manifest: status=%d body=%s", manifest.Code, manifest.Body.String())
	}
}

func TestWorkbenchRejectsCrossOriginAndUnsupportedCommands(t *testing.T) {
	if err := validateWorkbenchArgs([]string{"serve"}); err == nil {
		t.Fatal("serve was accepted")
	}
	if err := validateWorkbenchArgs([]string{"unknown"}); err == nil {
		t.Fatal("unknown command was accepted")
	}
	if err := validateWorkbenchArgs([]string{"help", "\x00"}); err == nil {
		t.Fatal("control character was accepted")
	}
	if validWorkbenchRequestHost(httptest.NewRequest(http.MethodGet, "http://example.com/", nil)) {
		t.Fatal("non-loopback Host was accepted")
	}

	workspace := t.TempDir()
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := workbench.bootstrap
	browserURL, err := workbench.BrowserURL("http://127.0.0.1:8787")
	if err != nil || !strings.Contains(browserURL, "#"+workbenchBootstrapFragment+"=") || strings.Contains(strings.Split(browserURL, "#")[0], workbench.bootstrap) {
		t.Fatalf("protected browser URL = %q, %v", browserURL, err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/workbench/jobs", strings.NewReader(`{"args":["help"]}`))
	request.Header.Set("Origin", "http://evil.example")
	request.Header.Set("X-RKC-Workbench-Token", workbench.token)
	response := httptest.NewRecorder()
	workbench.handleJobs(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", response.Code)
	}
	crossOriginSession := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil)
	crossOriginSession.Header.Set("Origin", "http://evil.example")
	crossOriginSession.Header.Set("Sec-Fetch-Site", "cross-site")
	crossOriginSession.Header.Set(workbenchBootstrapHeader, bootstrap)
	crossOriginResponse := httptest.NewRecorder()
	workbench.handleSession(crossOriginResponse, crossOriginSession)
	if crossOriginResponse.Code != http.StatusForbidden || strings.Contains(crossOriginResponse.Body.String(), workbench.token) {
		t.Fatalf("cross-origin session status=%d body=%s", crossOriginResponse.Code, crossOriginResponse.Body.String())
	}
	unauthenticated := httptest.NewRecorder()
	workbench.handleSession(unauthenticated, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil))
	if unauthenticated.Code != http.StatusForbidden || strings.Contains(unauthenticated.Body.String(), workbench.token) {
		t.Fatalf("unauthenticated loopback session status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	sameOriginSession := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil)
	sameOriginSession.Header.Set("Origin", "http://127.0.0.1")
	sameOriginSession.Header.Set("Sec-Fetch-Site", "same-origin")
	sameOriginSession.Header.Set(workbenchBootstrapHeader, bootstrap)
	sameOriginResponse := httptest.NewRecorder()
	workbench.handleSession(sameOriginResponse, sameOriginSession)
	if sameOriginResponse.Code != http.StatusOK || !strings.Contains(sameOriginResponse.Body.String(), workbench.token) {
		t.Fatalf("same-origin bootstrap status=%d body=%s", sameOriginResponse.Code, sameOriginResponse.Body.String())
	}
	replayed := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil)
	replayed.Header.Set(workbenchBootstrapHeader, bootstrap)
	replayedResponse := httptest.NewRecorder()
	workbench.handleSession(replayedResponse, replayed)
	if replayedResponse.Code != http.StatusForbidden {
		t.Fatalf("replayed bootstrap status=%d body=%s", replayedResponse.Code, replayedResponse.Body.String())
	}
	reload := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil)
	reload.Header.Set(workbenchTokenHeader, workbench.token)
	reloadResponse := httptest.NewRecorder()
	workbench.handleSession(reloadResponse, reload)
	if reloadResponse.Code != http.StatusOK {
		t.Fatalf("session-token reload status=%d body=%s", reloadResponse.Code, reloadResponse.Body.String())
	}
}

func TestWorkbenchDirectoryBrowserIsAuthenticatedBoundedAndDirectoryOnly(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"Beta", "alpha"} {
		if err := os.Mkdir(filepath.Join(workspace, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "not-a-folder.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "alpha"), filepath.Join(workspace, "linked-folder")); err != nil && !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := (&Dataset{}).HandlerWithWorkbench(workbench)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/directories", nil))
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized folder status = %d", unauthorized.Code)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authorizedWorkbenchRequest(workbench, http.MethodGet, "/api/v1/workbench/directories", ""))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("folder status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	var listing workbenchDirectoryListing
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != workspace || listing.Parent != filepath.Dir(workspace) || listing.Truncated || len(listing.Directories) != 2 {
		t.Fatalf("folder listing = %+v", listing)
	}
	if listing.Directories[0].Name != "alpha" || listing.Directories[1].Name != "Beta" ||
		listing.Directories[0].Path != filepath.Join(workspace, "alpha") {
		t.Fatalf("folder ordering = %+v", listing.Directories)
	}

	relative := httptest.NewRecorder()
	handler.ServeHTTP(relative, authorizedWorkbenchRequest(workbench, http.MethodGet, "/api/v1/workbench/directories?path=alpha", ""))
	if relative.Code != http.StatusOK {
		t.Fatalf("relative folder status=%d body=%s", relative.Code, relative.Body.String())
	}
	if err := json.Unmarshal(relative.Body.Bytes(), &listing); err != nil || listing.Path != filepath.Join(workspace, "alpha") {
		t.Fatalf("relative folder listing=%+v error=%v", listing, err)
	}

	for name, target := range map[string]string{
		"unknown query":  "/api/v1/workbench/directories?other=x",
		"duplicate path": "/api/v1/workbench/directories?path=alpha&path=Beta",
		"file":           "/api/v1/workbench/directories?path=not-a-folder.txt",
	} {
		t.Run(name, func(t *testing.T) {
			invalid := httptest.NewRecorder()
			handler.ServeHTTP(invalid, authorizedWorkbenchRequest(workbench, http.MethodGet, target, ""))
			if invalid.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", invalid.Code, invalid.Body.String())
			}
		})
	}
}

func TestWorkbenchDirectoryBrowserCapsOutputAndRejectsUnsafePaths(t *testing.T) {
	workspace := t.TempDir()
	for index := 0; index <= workbenchMaximumDirectoryEntries; index++ {
		if err := os.Mkdir(filepath.Join(workspace, fmt.Sprintf("folder-%03d", index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	listing, err := workbench.directoryListing(url.Values{"path": {workspace}})
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Directories) != workbenchMaximumDirectoryEntries || !listing.Truncated {
		t.Fatalf("bounded folder listing count=%d truncated=%t", len(listing.Directories), listing.Truncated)
	}
	for _, path := range []string{strings.Repeat("x", workbenchMaximumArgumentSize+1), "bad\npath"} {
		if _, err := workbench.directoryListing(url.Values{"path": {path}}); err == nil {
			t.Fatalf("unsafe folder path was accepted: %q", path)
		}
	}
}

func TestWorkbenchRequestValidationAndCapacity(t *testing.T) {
	workspace := t.TempDir()
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: os.Args[0], Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"empty":       nil,
		"empty value": {"help", ""},
		"large value": {"help", strings.Repeat("x", workbenchMaximumArgumentSize+1)},
		"too many":    append([]string{"help"}, make([]string, workbenchMaximumArguments)...),
		"large total": append([]string{"help"}, repeatedWorkbenchArgs(9, 4096)...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateWorkbenchArgs(args); err == nil {
				t.Fatalf("arguments were accepted: %#v", args)
			}
		})
	}
	foreignSession := httptest.NewRecorder()
	workbench.handleSession(foreignSession, httptest.NewRequest(http.MethodGet, "http://example.com/session", nil))
	if foreignSession.Code != http.StatusForbidden {
		t.Fatalf("foreign session status = %d", foreignSession.Code)
	}
	for name, body := range map[string]string{
		"malformed": `{"args":`,
		"unknown":   `{"args":["help"],"extra":true}`,
		"multiple":  `{"args":["help"]} {}`,
		"rejected":  `{"args":["unknown"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := authorizedWorkbenchRequest(workbench, http.MethodPost, "/api/v1/workbench/jobs", body)
			response := httptest.NewRecorder()
			workbench.handleJobs(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
		})
	}
	missing := httptest.NewRecorder()
	request := authorizedWorkbenchRequest(workbench, http.MethodGet, "/api/v1/workbench/jobs/missing", "")
	request.SetPathValue("jobID", "missing")
	workbench.handleJob(missing, request)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing job status = %d", missing.Code)
	}

	for index := 0; index < workbenchMaximumJobs; index++ {
		id := fmt.Sprintf("active-%d", index)
		workbench.jobs[id] = &workbenchJob{ID: id, Status: "running"}
	}
	if _, err := workbench.createJob([]string{"help"}); err == nil {
		t.Fatal("full active queue accepted another job")
	}
	workbench.jobs["active-0"].Status = "succeeded"
	if _, err := workbench.createJob([]string{"help"}); err != nil {
		t.Fatalf("completed job was not evicted: %v", err)
	}
}

func TestWorkbenchFailureTimeoutMissingJobAndBoundedOutput(t *testing.T) {
	workspace := t.TempDir()
	failing := filepath.Join(t.TempDir(), "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nprintf failure >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{Workspace: workspace, Executable: failing, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	workbench.runJob("missing")
	failed, err := workbench.createJob([]string{"help"})
	if err != nil {
		t.Fatal(err)
	}
	workbench.runJob(failed.ID)
	got := workbench.jobs[failed.ID]
	if got.Status != "failed" || got.ExitCode == nil || *got.ExitCode != 7 || got.Output != "failure" || got.Error == "" {
		t.Fatalf("unexpected failed job: %+v", got)
	}

	sleeping := filepath.Join(t.TempDir(), "sleeping")
	if err := os.WriteFile(sleeping, []byte("#!/bin/sh\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	timed, err := NewWorkbench(WorkbenchConfig{Workspace: workspace, Executable: sleeping, Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	job, err := timed.createJob([]string{"help"})
	if err != nil {
		t.Fatal(err)
	}
	timed.runJob(job.ID)
	if got := timed.jobs[job.ID]; got.Status != "timed_out" || got.ExitCode == nil || *got.ExitCode != -1 {
		t.Fatalf("unexpected timed-out job: %+v", got)
	}

	var output boundedWorkbenchBuffer
	value := bytes.Repeat([]byte("x"), workbenchMaximumOutputBytes+17)
	if written, err := output.Write(value); err != nil || written != len(value) {
		t.Fatalf("bounded write = %d, %v", written, err)
	}
	if written, err := output.Write([]byte("ignored")); err != nil || written != 7 {
		t.Fatalf("post-limit write = %d, %v", written, err)
	}
	if len(output.String()) != workbenchMaximumOutputBytes || !output.Truncated() {
		t.Fatalf("bounded output len=%d truncated=%t", len(output.String()), output.Truncated())
	}
}

func TestWorkbenchQueueWaitUsesSubmissionDeadline(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "must-not-run")
	executable := filepath.Join(t.TempDir(), "queued")
	script := "#!/bin/sh\nprintf ran > \"$2\"\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: executable, Timeout: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Hold the only execution slot without starting a process. The queued job
	// must expire based on CreatedAt rather than receiving a new run deadline.
	workbench.slot <- struct{}{}
	job, err := workbench.createJob([]string{"help", marker})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	go workbench.runJob(job.ID)
	completed := waitForWorkbenchJob(t, workbench, job.ID, 2*time.Second)
	<-workbench.slot

	if completed.Status != "timed_out" || completed.StartedAt != nil ||
		completed.FinishedAt == nil || completed.ExitCode == nil || *completed.ExitCode != -1 ||
		completed.Error != "command exceeded the workbench timeout" {
		t.Fatalf("queued timeout = %+v", completed)
	}
	if completed.DeadlineAt.Sub(completed.CreatedAt) != 40*time.Millisecond ||
		completed.FinishedAt.Before(completed.DeadlineAt) {
		t.Fatalf("submission deadline was not honored: %+v", completed)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("queued timeout took %v, want bounded by submission deadline", elapsed)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queued executable unexpectedly ran: %v", err)
	}
}

func TestWorkbenchCancelEndpointTerminatesAndReleasesSlot(t *testing.T) {
	workspace := t.TempDir()
	ready := filepath.Join(workspace, "ready")
	executable := filepath.Join(t.TempDir(), "cancel")
	script := `#!/bin/sh
if [ "$2" = "hold" ]; then
  printf ready > "$3"
  while :; do sleep 1; done
fi
printf 'quick'
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: executable, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := workbench.createJob([]string{"help", "hold", ready})
	if err != nil {
		t.Fatal(err)
	}
	go workbench.runJob(job.ID)
	waitForFile(t, ready, 2*time.Second)

	handler := (&Dataset{}).HandlerWithWorkbench(workbench)
	request := authorizedWorkbenchRequest(workbench, http.MethodDelete, "/api/v1/workbench/jobs/"+job.ID, "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", response.Code, response.Body.String())
	}
	var canceled workbenchJob
	if err := json.Unmarshal(response.Body.Bytes(), &canceled); err != nil {
		t.Fatal(err)
	}
	if canceled.Status != "canceled" || canceled.FinishedAt == nil ||
		canceled.ExitCode == nil || *canceled.ExitCode != -1 ||
		canceled.Error != "command canceled by user" {
		t.Fatalf("canceled job = %+v", canceled)
	}

	next, err := workbench.createJob([]string{"help", "quick"})
	if err != nil {
		t.Fatalf("slot was not released after cancellation: %v", err)
	}
	workbench.runJob(next.ID)
	completed, _ := workbench.jobSnapshot(next.ID)
	if completed.Status != "succeeded" || completed.Output != "quick" {
		t.Fatalf("post-cancel job = %+v", completed)
	}

	repeated := httptest.NewRecorder()
	handler.ServeHTTP(repeated, authorizedWorkbenchRequest(workbench, http.MethodDelete, "/api/v1/workbench/jobs/"+job.ID, ""))
	if repeated.Code != http.StatusOK {
		t.Fatalf("idempotent cancel status = %d body=%s", repeated.Code, repeated.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, authorizedWorkbenchRequest(workbench, http.MethodDelete, "/api/v1/workbench/jobs/missing", ""))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing cancel status = %d body=%s", missing.Code, missing.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/v1/workbench/jobs/"+job.ID, nil))
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized cancel status = %d", unauthorized.Code)
	}
}

func TestWorkbenchCancellationTerminatesDescendantProcessGroup(t *testing.T) {
	if !workbenchProcessGroupsSupported() {
		t.Skip("process groups are unavailable on this platform")
	}
	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "child.pid")
	executable := filepath.Join(t.TempDir(), "descendants")
	script := `#!/bin/sh
sleep 30 &
child=$!
printf '%s' "$child" > "$2"
wait "$child"
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: executable, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := workbench.createJob([]string{"help", pidFile})
	if err != nil {
		t.Fatal(err)
	}
	go workbench.runJob(job.ID)
	waitForFile(t, pidFile, 2*time.Second)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || childPID <= 0 {
		t.Fatalf("child pid = %q, %v", data, err)
	}
	if !workbenchProcessAlive(childPID) {
		t.Fatalf("descendant %d was not alive before cancellation", childPID)
	}

	if err := workbench.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for workbenchProcessAlive(childPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if workbenchProcessAlive(childPID) {
		t.Fatalf("descendant %d survived process-group cancellation", childPID)
	}
}

func TestWorkbenchCompletedLeaderWithSurvivingChildIsFailedAndCleaned(t *testing.T) {
	if !workbenchProcessGroupsSupported() {
		t.Skip("process groups are unavailable on this platform")
	}
	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "child.pid")
	executable := filepath.Join(t.TempDir(), "orphan-on-success")
	script := `#!/bin/sh
sleep 30 </dev/null >/dev/null 2>&1 &
child=$!
printf '%s' "$child" > "$2"
exit 0
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: executable, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := workbench.createJob([]string{"help", pidFile})
	if err != nil {
		t.Fatal(err)
	}
	workbench.runJob(job.ID)

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || childPID <= 0 {
		t.Fatalf("child pid = %q, %v", data, err)
	}
	completed, ok := workbench.jobSnapshot(job.ID)
	if !ok {
		t.Fatal("completed job disappeared")
	}
	if completed.Status != "failed" || completed.ExitCode == nil || *completed.ExitCode != 0 ||
		!strings.Contains(completed.Error, "descendant processes remained") {
		t.Fatalf("orphaning command = %+v", completed)
	}
	if workbenchProcessAlive(childPID) {
		t.Fatalf("descendant %d survived successful-leader cleanup", childPID)
	}
}

func TestWorkbenchCompletionCleanupFailureStatusIsTruthful(t *testing.T) {
	status, message := workbenchCompletionCleanupFailure(nil, true, nil)
	if status != "failed" || !strings.Contains(message, "process group was terminated") {
		t.Fatalf("proven cleanup outcome = %q, %q", status, message)
	}
	status, message = workbenchCompletionCleanupFailure(nil, true, ErrWorkbenchCleanupUnproven)
	if status != "cleanup_failed" || !strings.Contains(message, "could not be proven") {
		t.Fatalf("unproven cleanup outcome = %q, %q", status, message)
	}
}

func TestWorkbenchEnvironmentIsAllowlisted(t *testing.T) {
	t.Setenv("RKC_SECRET_TEST", "must-not-leak")
	t.Setenv("HOME", "/safe/home")
	t.Setenv("PATH", "/usr/bin:/bin")
	workspace := t.TempDir()
	executable := filepath.Join(t.TempDir(), "environment")
	script := `#!/bin/sh
printf 'secret=%s\n' "${RKC_SECRET_TEST-unset}"
printf 'home=%s\n' "${HOME-unset}"
printf 'path=%s\n' "${PATH-unset}"
printf 'workbench=%s\n' "${RKC_WORKBENCH-unset}"
printf 'no_color=%s\n' "${NO_COLOR-unset}"
printf 'term=%s\n' "${TERM-unset}"
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workbench, err := NewWorkbench(WorkbenchConfig{
		Workspace: workspace, Executable: executable, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := workbench.createJob([]string{"help"})
	if err != nil {
		t.Fatal(err)
	}
	workbench.runJob(job.ID)
	completed, _ := workbench.jobSnapshot(job.ID)
	if completed.Status != "succeeded" {
		t.Fatalf("environment job = %+v", completed)
	}
	for _, expected := range []string{
		"secret=unset", "home=/safe/home", "path=/usr/bin:/bin",
		"workbench=1", "no_color=1", "term=dumb",
	} {
		if !strings.Contains(completed.Output, expected+"\n") {
			t.Errorf("output missing %q: %q", expected, completed.Output)
		}
	}
	if strings.Contains(completed.Output, "must-not-leak") {
		t.Fatalf("ambient secret leaked: %q", completed.Output)
	}

	filtered, err := sanitizedWorkbenchEnvironment([]string{
		"PATH=/one", "RKC_SECRET_TEST=secret", "MALFORMED",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(filtered, "\n")
	for _, expected := range []string{"PATH=/one", "RKC_WORKBENCH=1", "GOMAXPROCS=1", "GOFLAGS=-p=1"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("sanitized environment missing %q: %#v", expected, filtered)
		}
	}
	if strings.Contains(joined, "RKC_SECRET_TEST") {
		t.Fatalf("sanitized environment retained a secret: %#v", filtered)
	}
}

func TestNewWorkbenchValidatesConfiguration(t *testing.T) {
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: os.Args[0]}); err == nil {
		t.Fatal("zero timeout was accepted")
	}
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: filepath.Join(t.TempDir(), "missing"), Executable: os.Args[0], Timeout: time.Second}); err == nil {
		t.Fatal("missing workspace was accepted")
	}
	workspaceFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(workspaceFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: workspaceFile, Executable: os.Args[0], Timeout: time.Second}); err == nil {
		t.Fatal("workspace file was accepted")
	}
	nonExecutable := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(nonExecutable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: nonExecutable, Timeout: time.Second}); err == nil {
		t.Fatal("non-executable tool was accepted")
	}
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: os.Args[0], Timeout: 61 * time.Minute}); err == nil {
		t.Fatal("oversized timeout was accepted")
	}
}

func authorizedWorkbenchRequest(workbench *Workbench, method, target, body string) *http.Request {
	request := httptest.NewRequest(method, "http://127.0.0.1"+target, strings.NewReader(body))
	request.Header.Set("Origin", "http://127.0.0.1")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("X-RKC-Workbench-Token", workbench.token)
	return request
}

func repeatedWorkbenchArgs(count, size int) []string {
	values := make([]string, count)
	for index := range values {
		values[index] = strings.Repeat("x", size)
	}
	return values
}

func waitForWorkbenchJob(t *testing.T, workbench *Workbench, id string, timeout time.Duration) *workbenchJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, ok := workbench.jobSnapshot(id)
		if !ok {
			t.Fatalf("workbench job %q disappeared", id)
		}
		if terminalWorkbenchStatus(job.Status) {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := workbench.jobSnapshot(id)
	t.Fatalf("workbench job did not finish within %v: %+v", timeout, job)
	return nil
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("file %q was not created within %v", path, timeout)
}

func writeWorkbenchAtlasFixture(t *testing.T, repository, output, rootName, snapshotID, symbol string) {
	t.Helper()
	sourcePath := filepath.Join(repository, "src", "auth.ts")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte("export class " + symbol + " { login(): boolean { return true } }\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(source)
	artifactID := "artifact-" + rootName
	evidenceID := "evidence-" + rootName
	classID := "class-" + rootName
	repositoryID := "repository-" + rootName
	rangeValue := &model.SourceRange{ArtifactID: artifactID, Path: "src/auth.ts", StartLine: 1, EndLine: 1}
	artifact := model.Artifact{
		ID: artifactID, Path: "src/auth.ts", Kind: "file", Language: "typescript", MediaType: "text/typescript",
		SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(source)), LineCount: 1, Text: true, Status: "syntax_parsed",
	}
	evidence := model.Evidence{
		ID: evidenceID, Kind: "syntax_inferred", Method: "workbench activation fixture", Confidence: 1,
		Source: rangeValue, Tool: "rkc-test", ToolVersion: "1",
	}
	repositoryNode := model.Node{ID: repositoryID, Kind: "repository", Name: rootName, QualifiedName: rootName, Visibility: "repository"}
	classNode := model.Node{
		ID: classID, Kind: "class", Name: symbol, QualifiedName: rootName + "." + symbol,
		Language: "typescript", Visibility: "public", PublicSurface: true,
		ArtifactID: artifactID, Source: rangeValue, EvidenceIDs: []string{evidenceID},
	}
	bundle := model.Bundle{
		Snapshot: model.Snapshot{
			SchemaVersion: model.SchemaVersion, ID: snapshotID, RepositoryID: repositoryID,
			CreatedAt: time.Unix(1, 0).UTC(), Status: "committed", RootName: rootName, RootPath: repository,
			ContentDigest: hex.EncodeToString(digest[:]), Git: model.GitInfo{Commit: strings.Repeat("a", 40)},
			Tool: model.ToolInfo{Name: "rkc", Version: "test"},
		},
		Artifacts: []model.Artifact{artifact},
		Nodes:     []model.Node{repositoryNode, classNode},
		Edges: []model.Edge{{
			ID: "edge-" + rootName, Kind: "contains", From: repositoryID, To: classID,
			Resolution: "declared", Confidence: 1, Producer: "rkc-test", EvidenceIDs: []string{evidenceID},
		}},
		Evidence: []model.Evidence{evidence},
	}
	if err := rkcexport.WriteAll(bundle, model.BuildCoverage(bundle), rkcexport.Options{
		Root: repository, Output: output, DisableStaticSite: true, DisableJSONLGraph: true,
		DisableSearchIndex: true, DisableIntegrations: true,
	}); err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(safeoutput.Marker{SchemaVersion: "1.0", Producer: "rkc", Kind: "atlas", SnapshotID: snapshotID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, safeoutput.MarkerName), marker, 0o600); err != nil {
		t.Fatal(err)
	}
}
