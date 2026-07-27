package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	workbenchMaximumRequestBytes = 64 * 1024
	workbenchMaximumOutputBytes  = 2 * 1024 * 1024
	workbenchMaximumArguments    = 128
	workbenchMaximumArgumentSize = 4096
	workbenchMaximumJobs         = 32
)

// WorkbenchConfig enables the explicitly opt-in local command surface. The
// read-only atlas handler remains the default and is portable across all
// supported platforms.
type WorkbenchConfig struct {
	Workspace  string
	Executable string
	Timeout    time.Duration
}

// Workbench runs exact RKC argv vectors without a shell. It is deliberately
// loopback-only, single-job, bounded, and token authenticated.
type Workbench struct {
	workspace  string
	executable string
	timeout    time.Duration
	token      string
	slot       chan struct{}

	mu   sync.RWMutex
	jobs map[string]*workbenchJob
}

type workbenchJob struct {
	ID         string     `json:"id"`
	Args       []string   `json:"args"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Output     string     `json:"output"`
	Truncated  bool       `json:"truncated"`
	Error      string     `json:"error,omitempty"`
}

type workbenchCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
}

var workbenchCommands = []workbenchCommand{
	{"quickstart", "Build and verify a ready-to-search atlas.", "writes"},
	{"init", "Create a complete local configuration.", "writes"},
	{"doctor", "Diagnose repository and optional capabilities.", "read"},
	{"plan", "Preview the stage DAG and cache decisions.", "read"},
	{"scan", "Compile a local or remote repository.", "writes"},
	{"check", "Enforce coverage, integrity, and security gates.", "read"},
	{"query", "Search a compiled repository atlas.", "read"},
	{"answer", "Produce a citation-checked answer.", "model"},
	{"synthesize", "Build evidence packets or use a qualified model.", "model"},
	{"path", "Find a bounded path between graph nodes.", "read"},
	{"impact", "Traverse bounded impact relationships.", "read"},
	{"components", "List strongly connected components.", "read"},
	{"diff", "Compare two compiled snapshots.", "read"},
	{"snapshots", "Inspect, export, select, or recover snapshots.", "writes"},
	{"plugins", "Inspect, validate, lock, or verify plugins.", "writes"},
	{"cache", "Inspect, verify, or prune the stage cache.", "writes"},
	{"version", "Print the RKC version.", "read"},
	{"help", "Show command help.", "read"},
}

func NewWorkbench(config WorkbenchConfig) (*Workbench, error) {
	workspace, err := filepath.Abs(config.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workbench workspace: %w", err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workbench workspace links: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return nil, errors.New("workbench workspace must be an existing directory")
	}
	executable, err := filepath.Abs(config.Executable)
	if err != nil {
		return nil, fmt.Errorf("resolve workbench executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve workbench executable links: %w", err)
	}
	info, err = os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, errors.New("workbench executable must be an executable regular file")
	}
	if config.Timeout <= 0 || config.Timeout > 60*time.Minute {
		return nil, errors.New("workbench timeout must be between zero and 60 minutes")
	}
	token, err := randomWorkbenchValue(32)
	if err != nil {
		return nil, fmt.Errorf("create workbench token: %w", err)
	}
	return &Workbench{
		workspace: workspace, executable: executable, timeout: config.Timeout,
		token: token, slot: make(chan struct{}, 1), jobs: make(map[string]*workbenchJob),
	}, nil
}

func randomWorkbenchValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (workbench *Workbench) handleSession(w http.ResponseWriter, request *http.Request) {
	if !validWorkbenchRequestHost(request) {
		writeProblem(w, http.StatusForbidden, "Loopback required", "workbench requests require a loopback Host")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "token": workbench.token, "workspace": workbench.workspace,
		"maximum_output_bytes": workbenchMaximumOutputBytes,
		"timeout_seconds":      int(workbench.timeout.Seconds()),
		"commands":             workbenchCommands,
	})
}

func (workbench *Workbench) handleJobs(w http.ResponseWriter, request *http.Request) {
	if !workbench.authorize(request) {
		writeProblem(w, http.StatusForbidden, "Workbench authorization failed", "same-origin token authentication is required")
		return
	}
	var payload struct {
		Args []string `json:"args"`
	}
	request.Body = http.MaxBytesReader(w, request.Body, workbenchMaximumRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeProblem(w, http.StatusBadRequest, "Invalid command request", err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeProblem(w, http.StatusBadRequest, "Invalid command request", "request must contain one JSON object")
		return
	}
	if err := validateWorkbenchArgs(payload.Args); err != nil {
		writeProblem(w, http.StatusBadRequest, "Command rejected", err.Error())
		return
	}
	job, err := workbench.createJob(payload.Args)
	if err != nil {
		writeProblem(w, http.StatusTooManyRequests, "Workbench is full", err.Error())
		return
	}
	go workbench.runJob(job.ID)
	w.Header().Set("Location", "/api/v1/workbench/jobs/"+url.PathEscape(job.ID))
	writeJSON(w, http.StatusAccepted, job)
}

func (workbench *Workbench) handleJob(w http.ResponseWriter, request *http.Request) {
	if !workbench.authorize(request) {
		writeProblem(w, http.StatusForbidden, "Workbench authorization failed", "same-origin token authentication is required")
		return
	}
	id := request.PathValue("jobID")
	workbench.mu.RLock()
	job, ok := workbench.jobs[id]
	if ok {
		copy := *job
		copy.Args = append([]string(nil), job.Args...)
		job = &copy
	}
	workbench.mu.RUnlock()
	if !ok {
		writeProblem(w, http.StatusNotFound, "Workbench job not found", id)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, job)
}

func (workbench *Workbench) authorize(request *http.Request) bool {
	if !validWorkbenchRequestHost(request) || !validWorkbenchOrigin(request) {
		return false
	}
	provided := request.Header.Get("X-RKC-Workbench-Token")
	return len(provided) == len(workbench.token) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(workbench.token)) == 1
}

func validWorkbenchRequestHost(request *http.Request) bool {
	host := request.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func validWorkbenchOrigin(request *http.Request) bool {
	site := request.Header.Get("Sec-Fetch-Site")
	if site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "http" && parsed.Host == request.Host
}

func validateWorkbenchArgs(args []string) error {
	if len(args) == 0 || len(args) > workbenchMaximumArguments {
		return fmt.Errorf("command must contain between 1 and %d arguments", workbenchMaximumArguments)
	}
	allowed := false
	for _, command := range workbenchCommands {
		if args[0] == command.Name {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("command %q is not available in the workbench", args[0])
	}
	if args[0] == "serve" {
		return errors.New("nested servers are not available in the workbench")
	}
	total := 0
	for _, argument := range args {
		total += len(argument)
		if argument == "" || len(argument) > workbenchMaximumArgumentSize {
			return errors.New("command arguments must be non-empty and at most 4096 bytes")
		}
		for _, character := range argument {
			if character == 0 || (unicode.IsControl(character) && character != '\t') {
				return errors.New("command arguments contain unsupported control characters")
			}
		}
	}
	if total > workbenchMaximumRequestBytes/2 {
		return errors.New("command argument payload is too large")
	}
	return nil
}

func (workbench *Workbench) createJob(args []string) (*workbenchJob, error) {
	id, err := randomWorkbenchValue(12)
	if err != nil {
		return nil, err
	}
	workbench.mu.Lock()
	defer workbench.mu.Unlock()
	if len(workbench.jobs) >= workbenchMaximumJobs {
		for key, job := range workbench.jobs {
			if job.Status == "succeeded" || job.Status == "failed" || job.Status == "timed_out" {
				delete(workbench.jobs, key)
				break
			}
		}
	}
	if len(workbench.jobs) >= workbenchMaximumJobs {
		return nil, errors.New("wait for an active job to finish before submitting more work")
	}
	job := &workbenchJob{
		ID: id, Args: append([]string(nil), args...), Status: "queued",
		CreatedAt: time.Now().UTC(),
	}
	workbench.jobs[id] = job
	copy := *job
	copy.Args = append([]string(nil), job.Args...)
	return &copy, nil
}

func (workbench *Workbench) runJob(id string) {
	workbench.slot <- struct{}{}
	defer func() { <-workbench.slot }()

	workbench.mu.Lock()
	job, ok := workbench.jobs[id]
	if !ok {
		workbench.mu.Unlock()
		return
	}
	started := time.Now().UTC()
	job.Status = "running"
	job.StartedAt = &started
	args := append([]string(nil), job.Args...)
	workbench.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), workbench.timeout)
	defer cancel()
	command := exec.CommandContext(ctx, workbench.executable, args...)
	command.Dir = workbench.workspace
	command.Env = append(os.Environ(), "RKC_WORKBENCH=1")
	var output boundedWorkbenchBuffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	finished := time.Now().UTC()
	exitCode := 0
	status := "succeeded"
	message := ""
	if ctx.Err() == context.DeadlineExceeded {
		exitCode = -1
		status = "timed_out"
		message = "command exceeded the workbench timeout"
	} else if err != nil {
		status = "failed"
		exitCode = -1
		if command.ProcessState != nil {
			exitCode = command.ProcessState.ExitCode()
		}
		message = err.Error()
	}

	workbench.mu.Lock()
	if job, ok := workbench.jobs[id]; ok {
		job.Status = status
		job.FinishedAt = &finished
		job.ExitCode = &exitCode
		job.Output = output.String()
		job.Truncated = output.Truncated()
		job.Error = message
	}
	workbench.mu.Unlock()
}

type boundedWorkbenchBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (buffer *boundedWorkbenchBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	original := len(value)
	remaining := workbenchMaximumOutputBytes - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.buffer.Write(value)
	return original, nil
}

func (buffer *boundedWorkbenchBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func (buffer *boundedWorkbenchBuffer) Truncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.truncated
}
