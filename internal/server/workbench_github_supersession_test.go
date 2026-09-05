package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/githubsource"
)

func TestGitHubSupersededConnectionCannotAdmitBeforeCancellation(t *testing.T) {
	fake := &fakeWorkbenchGitHub{}
	workbench, _ := githubWorkbenchForTest(t, fake, func(context.Context, githubsource.Checkout) error { return nil })
	previous := workbench.gitHubSnapshot()
	defer previous.cancel()
	workbench.mu.Lock()
	workbench.githubConnection = newWorkbenchGitHubConnection(fake, "new-account")
	workbench.mu.Unlock()
	if previous.context.Err() != nil {
		t.Fatal("regression requires an unsignaled old connection")
	}
	job, err := workbench.createSourceJob([]string{}, "example/source", previous)
	if err == nil {
		workbench.finishJob(job.ID, "failed", 1, "", false, "test cleanup")
		t.Fatal("superseded connection admitted a job before cancellation propagation")
	}
}

func TestGitHubSupersededConnectionCannotPublishSearchBeforeCancellation(t *testing.T) {
	var workbench *Workbench
	var previous *workbenchGitHubConnection
	fake := &fakeWorkbenchGitHub{search: func(context.Context, string, int) (githubsource.SearchResult, error) {
		workbench.mu.Lock()
		previous = workbench.githubConnection
		workbench.githubConnection = newWorkbenchGitHubConnection(&fakeWorkbenchGitHub{}, "new-account")
		workbench.mu.Unlock()
		return githubsource.SearchResult{Items: []githubsource.Repository{{FullName: "example/old-account-result"}}, Total: 1}, nil
	}}
	var handler http.Handler
	workbench, handler = githubWorkbenchForTest(t, fake, func(context.Context, githubsource.Checkout) error { return nil })
	defer func() {
		if previous != nil {
			previous.cancel()
		}
	}()
	response := githubRequest(workbench, handler, http.MethodGet, "/api/v1/workbench/github/repositories?q=example", "")
	if previous == nil || previous.context.Err() != nil {
		t.Fatal("regression requires an unsignaled old connection")
	}
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "old-account-result") {
		t.Fatalf("superseded search published source data: HTTP %d", response.Code)
	}
}

func TestGitHubSupersededConnectionCannotActivateBeforeCancellation(t *testing.T) {
	fake := &fakeWorkbenchGitHub{}
	workbench, _ := githubWorkbenchForTest(t, fake, func(context.Context, githubsource.Checkout) error { return nil })
	previous := workbench.gitHubSnapshot()
	defer previous.cancel()
	job, err := workbench.createSourceJob([]string{}, "example/source", previous)
	if err != nil {
		t.Fatal(err)
	}
	defer workbench.finishJob(job.ID, "failed", 1, "", false, "test cleanup")
	root := filepath.Join(workbench.workspace, "source")
	writeWorkbenchAtlasFixture(t, root, filepath.Join(root, ".rkc"), "source", "superseded-source", "OldSource")
	workbench.mu.Lock()
	initial := workbench.activeDataset
	workbench.jobs[job.ID].Status = "running"
	workbench.githubConnection = newWorkbenchGitHubConnection(fake, "new-account")
	workbench.mu.Unlock()
	if previous.context.Err() != nil {
		t.Fatal("regression requires an unsignaled old connection")
	}
	if err := workbench.activateCompletedQuickstart(job.ID, []string{"quickstart", root}); err == nil {
		t.Fatal("superseded connection activated an atlas before cancellation propagation")
	}
	workbench.mu.RLock()
	defer workbench.mu.RUnlock()
	if workbench.activeDataset != initial || workbench.jobs[job.ID].ActivatedDataset != nil {
		t.Fatal("failed activation changed the live dataset")
	}
}

func TestGitHubCanceledAuthenticationCannotInstallLateSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeWorkbenchGitHub{user: func(context.Context) (githubsource.User, error) {
		// This models cancellation after a successful response has arrived, but
		// before its account can be installed in the workbench.
		cancel()
		return githubsource.User{Login: "late-account"}, nil
	}}
	workbench, handler := githubWorkbenchForTest(t, fake, func(context.Context, githubsource.Checkout) error { return nil })
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/workbench/github/session", strings.NewReader(`{"token":"unit-test-credential"}`)).WithContext(ctx)
	request.Header.Set(workbenchTokenHeader, workbench.token)
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if workbench.gitHubSnapshot().login != "" || response.Code == http.StatusOK {
		t.Fatalf("canceled authentication installed an account: HTTP %d", response.Code)
	}
}
