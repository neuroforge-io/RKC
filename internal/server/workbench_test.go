package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
