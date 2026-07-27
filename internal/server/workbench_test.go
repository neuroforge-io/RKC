package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
		{name: "quickstart python", args: []string{"quickstart", "--python"}, want: true},
		{name: "quickstart", args: []string{"quickstart"}},
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
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/workbench/session", nil)
	sessionResponse := httptest.NewRecorder()
	workbench.handleSession(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session status = %d: %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	var session struct {
		Token    string             `json:"token"`
		Commands []workbenchCommand `json:"commands"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Token == "" || len(session.Commands) < 10 {
		t.Fatalf("incomplete session: token=%q commands=%d", session.Token, len(session.Commands))
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
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/workbench/jobs", strings.NewReader(`{"args":["help"]}`))
	request.Header.Set("Origin", "http://evil.example")
	request.Header.Set("X-RKC-Workbench-Token", workbench.token)
	response := httptest.NewRecorder()
	workbench.handleJobs(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", response.Code)
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
