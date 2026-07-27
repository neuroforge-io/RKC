package server

import (
	"encoding/json"
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

func TestNewWorkbenchValidatesConfiguration(t *testing.T) {
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: os.Args[0]}); err == nil {
		t.Fatal("zero timeout was accepted")
	}
	if _, err := NewWorkbench(WorkbenchConfig{Workspace: filepath.Join(t.TempDir(), "missing"), Executable: os.Args[0], Timeout: time.Second}); err == nil {
		t.Fatal("missing workspace was accepted")
	}
}
