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
	"sync/atomic"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/githubsource"
)

type fakeWorkbenchGitHub struct {
	user        func(context.Context) (githubsource.User, error)
	search      func(context.Context, string, int) (githubsource.SearchResult, error)
	materialize func(context.Context, string, string) (githubsource.Checkout, error)
}

func (fake *fakeWorkbenchGitHub) User(ctx context.Context) (githubsource.User, error) {
	return fake.user(ctx)
}
func (fake *fakeWorkbenchGitHub) Search(ctx context.Context, q string, page int) (githubsource.SearchResult, error) {
	return fake.search(ctx, q, page)
}
func (fake *fakeWorkbenchGitHub) Materialize(ctx context.Context, repo, path string) (githubsource.Checkout, error) {
	return fake.materialize(ctx, repo, path)
}

func githubWorkbenchForTest(t *testing.T, fake *fakeWorkbenchGitHub, compile func(context.Context, githubsource.Checkout) error) (*Workbench, http.Handler) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LOCALAPPDATA", cache)
	workbench, err := NewWorkbench(WorkbenchConfig{Workspace: t.TempDir(), Executable: os.Args[0], Timeout: 5 * time.Second, CompileFolder: func(context.Context, string) error { return errors.New("unexpected local compile") }, CompileGitHub: compile})
	if err != nil {
		t.Fatal(err)
	}
	workbench.githubConnection.cancel()
	workbench.githubConnection = newWorkbenchGitHubConnection(fake, "")
	workbench.newGitHubClient = func(string) (workbenchGitHubClient, error) { return fake, nil }
	empty, err := NewWorkspaceDataset()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := workbench.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return workbench, empty.HandlerWithWorkbench(workbench)
}

func githubRequest(workbench *Workbench, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	request.Header.Set(workbenchTokenHeader, workbench.token)
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestGitHubWorkbenchPublicSearchAndStrictAdmission(t *testing.T) {
	var searches atomic.Int32
	fake := &fakeWorkbenchGitHub{search: func(ctx context.Context, q string, page int) (githubsource.SearchResult, error) {
		searches.Add(1)
		if q != "compiler" || page != 2 {
			t.Errorf("wrong search %q page %d", q, page)
		}
		return githubsource.SearchResult{Items: []githubsource.Repository{{FullName: "example/compiler"}}, Total: 26}, nil
	}}
	workbench, handler := githubWorkbenchForTest(t, fake, func(context.Context, githubsource.Checkout) error { return nil })
	for _, route := range []string{"/api/v1/workbench/github/session", "/api/v1/workbench/github/repositories?q=compiler"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+route, nil))
		if response.Code != 403 {
			t.Fatalf("unauthenticated %s: %d", route, response.Code)
		}
	}
	response := githubRequest(workbench, handler, "GET", "/api/v1/workbench/github/session", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"connected":false`) {
		t.Fatal(response.Body.String())
	}
	response = githubRequest(workbench, handler, "GET", "/api/v1/workbench/github/repositories?q=compiler&page=2", "")
	if response.Code != 200 || searches.Load() != 1 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public search: %d %s", response.Code, response.Body.String())
	}
	for _, query := range []string{"", "q=", "q=a&q=b", "q=a&page=0", "q=a&page=41", "q=a&page=x", "q=a&token=oops", "q=%ff", "q=%00", "q=%zz", "q=" + strings.Repeat("x", 513)} {
		response := githubRequest(workbench, handler, "GET", "/api/v1/workbench/github/repositories?"+query, "")
		if response.Code != 400 {
			t.Fatalf("bad query status %d", response.Code)
		}
	}
	for _, body := range []string{`null`, `{}`, `{"args":[],"github_repository":"example/repo"}`, `{"github_repository":null}`, `{"github_repository":"../repo"}`, `{"github_repository":"example/repo","github_repository":"other/repo"}`, `{"GitHub_Repository":"example/repo"}`, `{"args":["version"],"Args":["version"]}`, `{"args":["version"]} {}`} {
		response := githubRequest(workbench, handler, "POST", "/api/v1/workbench/jobs", body)
		if response.Code != 400 {
			t.Fatalf("bad source body status %d: %s", response.Code, response.Body.String())
		}
	}
	if searches.Load() != 1 || len(workbench.jobs) != 0 {
		t.Fatal("invalid request performed work")
	}
}

func TestGitHubWorkbenchConnectDisconnectAndSupersession(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	fake := &fakeWorkbenchGitHub{user: func(ctx context.Context) (githubsource.User, error) {
		close(entered)
		<-release
		return githubsource.User{Login: "example"}, nil
	}}
	workbench, handler := githubWorkbenchForTest(t, fake, func(context.Context, githubsource.Checkout) error { return nil })
	for _, body := range []string{`{"token":"one","token":"two"}`, `{"Token":"one"}`, `{"token":null}`, `{"token":""}`, `[]`} {
		response := githubRequest(workbench, handler, "POST", "/api/v1/workbench/github/session", body)
		if response.Code != 400 {
			t.Fatalf("bad credential request: %d", response.Code)
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- githubRequest(workbench, handler, "POST", "/api/v1/workbench/github/session", `{"token":"unit-test-credential"}`)
	}()
	<-entered
	old := workbench.gitHubSnapshot()
	response := githubRequest(workbench, handler, "DELETE", "/api/v1/workbench/github/session", "")
	if response.Code != 200 || old.context.Err() == nil {
		t.Fatal("disconnect did not revoke old connection")
	}
	close(release)
	if response := <-done; response.Code != 409 {
		t.Fatalf("late connect replaced disconnect: %d", response.Code)
	}
	if workbench.gitHubSnapshot().login != "" {
		t.Fatal("late connect resurrected credentials")
	}
	fake.user = func(context.Context) (githubsource.User, error) { return githubsource.User{Login: "example"}, nil }
	response = githubRequest(workbench, handler, "POST", "/api/v1/workbench/github/session", `{"token":"unit-test-credential"}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"connected":true`) || strings.Contains(response.Body.String(), "unit-test-credential") {
		t.Fatalf("connected session %d %s", response.Code, response.Body.String())
	}
}

func TestGitHubWorkbenchCompilesAndActivatesPinnedSource(t *testing.T) {
	var directory string
	fake := &fakeWorkbenchGitHub{materialize: func(ctx context.Context, repo, parent string) (githubsource.Checkout, error) {
		directory = parent
		root := filepath.Join(parent, "sample")
		if err := os.Mkdir(root, 0700); err != nil {
			return githubsource.Checkout{}, err
		}
		return githubsource.Checkout{Root: root, Repository: githubsource.Repository{FullName: repo, HTMLURL: "https://github.com/" + repo}, CommitSHA: strings.Repeat("a", 40), ArchiveSHA256: strings.Repeat("b", 64), ArchiveBytes: 300}, nil
	}}
	workbench, handler := githubWorkbenchForTest(t, fake, func(ctx context.Context, checkout githubsource.Checkout) error {
		writeWorkbenchAtlasFixture(t, checkout.Root, filepath.Join(checkout.Root, ".rkc"), "sample", "snapshot-github", "Login")
		return nil
	})
	response := githubRequest(workbench, handler, "POST", "/api/v1/workbench/jobs", `{"github_repository":"example/sample"}`)
	var submitted workbenchJob
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &submitted) != nil {
		t.Fatalf("submission %d: %s", response.Code, response.Body.String())
	}
	job := waitForWorkbenchJob(t, workbench, submitted.ID, 2*time.Second)
	if job.Status != "succeeded" || job.ActivatedDataset == nil || job.ActivatedDataset.SnapshotID != "snapshot-github" || job.GitHubSource == nil || job.GitHubSource.CommitSHA != strings.Repeat("a", 40) {
		t.Fatalf("source result: %+v", job)
	}
	if len(job.Args) != 0 || job.CleanupScope != "in_process" || job.sourceConnection != nil || len(workbench.slot) != 0 {
		t.Fatal("source job exposed executable or retained authority")
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatal("successful local checkout disappeared")
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
}

func TestGitHubWorkbenchDisconnectWaitsForSourceCleanup(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var directory string
	fake := &fakeWorkbenchGitHub{materialize: func(ctx context.Context, repo, parent string) (githubsource.Checkout, error) {
		directory = parent
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return githubsource.Checkout{}, ctx.Err()
	}}
	workbench, handler := githubWorkbenchForTest(t, fake, func(context.Context, githubsource.Checkout) error {
		t.Error("canceled acquisition compiled")
		return nil
	})
	response := githubRequest(workbench, handler, "POST", "/api/v1/workbench/jobs", `{"github_repository":"example/sample"}`)
	var submitted workbenchJob
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &submitted) != nil {
		t.Fatal(response.Body.String())
	}
	<-entered
	if response := githubRequest(workbench, handler, "DELETE", "/api/v1/workbench/github/session", ""); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	<-canceled
	job, _ := workbench.jobSnapshot(submitted.ID)
	if job.Status != "running" || len(workbench.slot) != 1 {
		t.Fatal("cancellation released the acquisition slot early")
	}
	close(release)
	job = waitForWorkbenchJob(t, workbench, submitted.ID, time.Second)
	if job.Status != "canceled" || job.ActivatedDataset != nil || len(workbench.slot) != 0 {
		t.Fatalf("canceled source: %+v", job)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("failed acquisition left private source data")
	}
}
