package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/githubsource"
	"github.com/neuroforge-io/RKC/internal/privatepath"
)

type workbenchGitHubClient interface {
	User(context.Context) (githubsource.User, error)
	Search(context.Context, string, int) (githubsource.SearchResult, error)
	Materialize(context.Context, string, string) (githubsource.Checkout, error)
}

type workbenchGitHubConnection struct {
	client  workbenchGitHubClient
	login   string
	context context.Context
	cancel  context.CancelFunc
}

type workbenchGitHubSource struct {
	Repository    string `json:"repository"`
	HTMLURL       string `json:"html_url"`
	CommitSHA     string `json:"commit_sha"`
	ArchiveSHA256 string `json:"archive_sha256"`
	ArchiveBytes  int64  `json:"archive_bytes"`
}

func newWorkbenchGitHubConnection(client workbenchGitHubClient, login string) *workbenchGitHubConnection {
	ctx, cancel := context.WithCancel(context.Background())
	return &workbenchGitHubConnection{client: client, login: login, context: ctx, cancel: cancel}
}

func (workbench *Workbench) authorizeGitHub(w http.ResponseWriter, request *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !workbench.authorize(request) {
		writeProblem(w, http.StatusForbidden, "Workbench authorization failed", "same-origin token authentication is required")
		return false
	}
	if workbench.compileGitHub == nil {
		writeProblem(w, http.StatusServiceUnavailable, "GitHub sources unavailable", "this workspace has no built-in GitHub compiler")
		return false
	}
	return true
}

func (workbench *Workbench) gitHubSnapshot() *workbenchGitHubConnection {
	workbench.mu.RLock()
	defer workbench.mu.RUnlock()
	return workbench.githubConnection
}

func (workbench *Workbench) handleGitHubSession(w http.ResponseWriter, request *http.Request) {
	if !workbench.authorizeGitHub(w, request) {
		return
	}
	if request.URL.RawQuery != "" {
		writeProblem(w, http.StatusBadRequest, "Invalid GitHub session request", "session requests do not accept query parameters")
		return
	}
	if request.Method == http.MethodGet {
		connection := workbench.gitHubSnapshot()
		if connection == nil {
			writeProblem(w, http.StatusServiceUnavailable, "Workspace is closed", "start a new workspace")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"connected": connection.login != "", "login": connection.login})
		return
	}
	var token string
	if request.Method == http.MethodPost {
		fields, err := decodeWorkbenchObject(w, request, 8192, "token")
		if err != nil || len(fields) != 1 {
			writeProblem(w, http.StatusBadRequest, "Invalid GitHub connection", "provide one JSON object containing token")
			return
		}
		if err := json.Unmarshal(fields["token"], &token); err != nil || token == "" {
			writeProblem(w, http.StatusBadRequest, "Invalid GitHub connection", "provide one nonempty token in the request body")
			return
		}
	}
	client, err := workbench.newGitHubClient(token)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Invalid GitHub connection", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	workbench.mu.Lock()
	if workbench.githubConnectCancel != nil {
		workbench.githubConnectCancel()
	}
	workbench.githubGeneration++
	generation := workbench.githubGeneration
	closed := workbench.closed
	if !closed {
		workbench.githubConnectCancel = cancel
	}
	workbench.mu.Unlock()
	defer func() {
		workbench.mu.Lock()
		if generation == workbench.githubGeneration {
			workbench.githubConnectCancel = nil
		}
		workbench.mu.Unlock()
	}()
	if closed {
		writeProblem(w, http.StatusServiceUnavailable, "Workspace is closed", "start a new workspace")
		return
	}
	login := ""
	if token != "" {
		user, err := client.User(ctx)
		if err != nil {
			workbench.mu.RLock()
			stale := workbench.closed || generation != workbench.githubGeneration
			workbench.mu.RUnlock()
			if stale {
				writeProblem(w, http.StatusConflict, "GitHub connection changed", "a newer connection request replaced this one")
				return
			}
			writeProblem(w, http.StatusBadGateway, "GitHub connection failed", err.Error())
			return
		}
		login = user.Login
	}
	workbench.mu.Lock()
	if workbench.closed || generation != workbench.githubGeneration || ctx.Err() != nil {
		workbench.mu.Unlock()
		writeProblem(w, http.StatusConflict, "GitHub connection changed", "a newer connection request has already replaced this one")
		return
	}
	previous := workbench.githubConnection
	if previous != nil {
		previous.cancel()
	}
	workbench.githubConnection = newWorkbenchGitHubConnection(client, login)
	workbench.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"connected": login != "", "login": login})
}

func (workbench *Workbench) handleGitHubRepositories(w http.ResponseWriter, request *http.Request) {
	if !workbench.authorizeGitHub(w, request) {
		return
	}
	values, err := gitHubSearchParameters(request)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Invalid GitHub search", err.Error())
		return
	}
	query := strings.TrimSpace(values.Get("q"))
	if query == "" || len(query) > 512 {
		writeProblem(w, http.StatusBadRequest, "Invalid GitHub search", "enter a repository search of 1 to 512 bytes")
		return
	}
	page := 1
	if raw, supplied := values["page"]; supplied {
		page, err = strconv.Atoi(raw[0])
		if err != nil || page < 1 || page > 40 {
			writeProblem(w, http.StatusBadRequest, "Invalid GitHub search", "page must be between 1 and 40")
			return
		}
	}
	connection := workbench.gitHubSnapshot()
	if connection == nil {
		writeProblem(w, http.StatusServiceUnavailable, "Workspace is closed", "start a new workspace")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	stop := context.AfterFunc(connection.context, cancel)
	defer stop()
	result, err := connection.client.Search(ctx, query, page)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, "GitHub search failed", err.Error())
		return
	}
	workbench.mu.RLock()
	defer workbench.mu.RUnlock()
	if ctx.Err() != nil || connection != workbench.githubConnection || connection.context.Err() != nil {
		writeProblem(w, http.StatusConflict, "GitHub connection changed", "search again using the current connection")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (workbench *Workbench) handleGitHubJob(w http.ResponseWriter, request *http.Request, repository string) {
	if !workbench.authorizeGitHub(w, request) {
		return
	}
	if !githubsource.ValidRepositoryName(repository) {
		writeProblem(w, http.StatusBadRequest, "Invalid GitHub repository", "choose a repository in owner/name form")
		return
	}
	connection := workbench.gitHubSnapshot()
	if connection == nil {
		writeProblem(w, http.StatusServiceUnavailable, "Workspace is closed", "start a new workspace")
		return
	}
	job, err := workbench.createSourceJob([]string{}, repository, connection)
	if err != nil {
		status := http.StatusTooManyRequests
		if errors.Is(err, ErrWorkbenchClosed) {
			status = http.StatusServiceUnavailable
		}
		writeProblem(w, status, "Source job could not start", err.Error())
		return
	}
	go workbench.runJob(job.ID)
	w.Header().Set("Location", "/api/v1/workbench/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

// GitHub acquisition, compilation, and activation occupy the same single job
// slot. No request-supplied executable, output path, or credential reaches a
// process argument. A completed source stays in the user's cache for reuse.
func (workbench *Workbench) runGitHubJob(ctx context.Context, id, repository string, connection *workbenchGitHubConnection, releaseSlot func()) {
	cache, err := os.UserCacheDir()
	if err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", 1, "", false, "user cache directory is unavailable")
		return
	}
	if err = os.MkdirAll(cache, 0700); err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", 1, "", false, "user cache directory cannot be created")
		return
	}
	directory, err := privatepath.MkdirTemp(cache, "rkc-github-")
	if err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", 1, "", false, "private source directory cannot be created")
		return
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", 1, "", false, "private source directory cannot be inspected")
		return
	}
	checkout, err := connection.client.Materialize(ctx, repository, directory)
	if err == nil {
		// The production client publishes one child. Recheck its returned path
		// before invoking a compiler with local filesystem authority.
		resolvedParent, parentErr := filepath.EvalSymlinks(directory)
		if parentErr != nil || filepath.Dir(checkout.Root) != resolvedParent {
			err = errors.New("GitHub archive returned an invalid source path")
		}
	}
	if err == nil {
		workbench.mu.Lock()
		if job, ok := workbench.jobs[id]; ok {
			job.GitHubSource = &workbenchGitHubSource{Repository: checkout.Repository.FullName, HTMLURL: checkout.Repository.HTMLURL, CommitSHA: checkout.CommitSHA, ArchiveSHA256: checkout.ArchiveSHA256, ArchiveBytes: checkout.ArchiveBytes}
		}
		workbench.mu.Unlock()
		err = workbench.compileGitHub(ctx, checkout)
	}
	activated := false
	if err == nil && ctx.Err() == nil {
		err = workbench.activateCompletedQuickstart(id, []string{"quickstart", checkout.Root})
		activated = err == nil
	}
	if !activated {
		cleanupErr := removeWorkbenchSource(directory, identity)
		releaseSlot()
		if cleanupErr != nil {
			workbench.finishJob(id, "cleanup_failed", 1, "", false, "source job stopped; private source cleanup could not be verified")
			return
		}
		if ctx.Err() != nil {
			workbench.finishJobFromContext(id, nil, nil)
			return
		}
		workbench.finishJob(id, "failed", 1, "", false, err.Error())
		return
	}
	releaseSlot()
	workbench.finishJob(id, "succeeded", 0, "GitHub source downloaded, compiled, and verified. The source and atlas are saved in your local cache.", false, "")
}

func removeWorkbenchSource(directory string, identity os.FileInfo) error {
	if err := privatepath.CheckDir(directory, identity); err != nil {
		return err
	}
	return os.RemoveAll(directory)
}

func gitHubSearchParameters(request *http.Request) (url.Values, error) {
	if len(request.URL.RawQuery) > 4096 {
		return nil, errors.New("GitHub search query is too large")
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return nil, errors.New("GitHub search query is malformed")
	}
	for key, entries := range values {
		if (key != "q" && key != "page") || len(entries) != 1 || !utf8.ValidString(entries[0]) || strings.ContainsAny(entries[0], "\r\n\x00") {
			return nil, errors.New("use q and page once each with valid UTF-8 values")
		}
	}
	return values, nil
}

// New source requests use exact field spellings and reject duplicates, aliases,
// null objects, and trailing JSON before any network or filesystem action.
func decodeWorkbenchObject(w http.ResponseWriter, request *http.Request, maximum int64, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maximum))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("request must contain one JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("invalid object field")
		}
		permitted := false
		for _, name := range allowed {
			if key == name {
				permitted = true
			}
		}
		if _, duplicate := fields[key]; !permitted || duplicate {
			return nil, errors.New("unknown or duplicate object field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("request contains trailing JSON")
	}
	return fields, nil
}
