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
	"runtime"
	"sort"
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

// ErrWorkbenchJobNotFound reports an unknown cancellation target.
var ErrWorkbenchJobNotFound = errors.New("workbench job not found")

// ErrWorkbenchClosed reports submission after shutdown has begun.
var ErrWorkbenchClosed = errors.New("workbench is closed")

// ErrWorkbenchCleanupUnproven reports that a canceled process could not be
// reaped or its supported containment boundary could not be proven empty.
var ErrWorkbenchCleanupUnproven = errors.New("workbench process cleanup could not be proven")

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
	workspace   string
	executable  string
	timeout     time.Duration
	token       string
	slot        chan struct{}
	environment []string

	mu     sync.RWMutex
	jobs   map[string]*workbenchJob
	closed bool
}

type workbenchJob struct {
	ID           string     `json:"id"`
	Args         []string   `json:"args"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	DeadlineAt   time.Time  `json:"deadline_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	Output       string     `json:"output"`
	Truncated    bool       `json:"truncated"`
	Error        string     `json:"error,omitempty"`
	CleanupScope string     `json:"cleanup_scope,omitempty"`

	context               context.Context
	cancel                context.CancelFunc
	done                  chan struct{}
	retain                int
	mayLaunchManagedUnits bool
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
	{"plan", "Preview the stage DAG, SCIP input, and cache decisions.", "read"},
	{"scan", "Compile with optional compiler-grade SCIP semantics.", "writes"},
	{"check", "Enforce coverage, integrity, and security gates.", "read"},
	{"query", "Search a compiled repository atlas.", "read"},
	{"answer", "Produce a citation-checked answer.", "model"},
	{"synthesize", "Build evidence packets or use a qualified model.", "model"},
	{"path", "Find a bounded path between graph nodes.", "read"},
	{"impact", "Traverse bounded impact relationships.", "read"},
	{"components", "List strongly connected components.", "read"},
	{"diff", "Compare two compiled snapshots.", "read"},
	{"snapshots", "Inspect, export, select, or recover snapshots.", "writes"},
	{"runs", "Inspect validated scheduler run journals.", "read"},
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
	environment, err := sanitizedWorkbenchEnvironment(os.Environ())
	if err != nil {
		return nil, fmt.Errorf("sanitize workbench environment: %w", err)
	}
	token, err := randomWorkbenchValue(32)
	if err != nil {
		return nil, fmt.Errorf("create workbench token: %w", err)
	}
	return &Workbench{
		workspace: workspace, executable: executable, timeout: config.Timeout,
		token: token, slot: make(chan struct{}, 1), environment: environment,
		jobs: make(map[string]*workbenchJob),
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
		if errors.Is(err, ErrWorkbenchClosed) {
			writeProblem(w, http.StatusServiceUnavailable, "Workbench is shutting down", err.Error())
			return
		}
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
	job, ok := workbench.jobSnapshot(id)
	if !ok {
		writeProblem(w, http.StatusNotFound, "Workbench job not found", id)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, job)
}

func (workbench *Workbench) handleCancelJob(w http.ResponseWriter, request *http.Request) {
	if !workbench.authorize(request) {
		writeProblem(w, http.StatusForbidden, "Workbench authorization failed", "same-origin token authentication is required")
		return
	}
	id := request.PathValue("jobID")
	job, err := workbench.cancelJob(id)
	if err != nil {
		if errors.Is(err, ErrWorkbenchJobNotFound) {
			writeProblem(w, http.StatusNotFound, "Workbench job not found", id)
			return
		}
		if errors.Is(err, ErrWorkbenchCleanupUnproven) {
			writeProblem(w, http.StatusConflict, "Workbench cleanup could not be proven", err.Error())
			return
		}
		writeProblem(w, http.StatusConflict, "Workbench cancellation failed", err.Error())
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
	if workbench.closed {
		return nil, ErrWorkbenchClosed
	}
	if len(workbench.jobs) >= workbenchMaximumJobs {
		workbench.evictOneTerminalJobLocked()
	}
	if len(workbench.jobs) >= workbenchMaximumJobs {
		return nil, errors.New("wait for an active job to finish before submitting more work")
	}
	created := time.Now().UTC()
	deadline := created.Add(workbench.timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	job := &workbenchJob{
		ID: id, Args: append([]string(nil), args...), Status: "queued",
		CreatedAt: created, DeadlineAt: deadline, context: ctx, cancel: cancel, done: make(chan struct{}),
		CleanupScope: workbenchCleanupScope(), mayLaunchManagedUnits: workbenchMayLaunchManagedUnits(args),
	}
	workbench.jobs[id] = job
	return copyWorkbenchJob(job), nil
}

func (workbench *Workbench) runJob(id string) {
	workbench.mu.RLock()
	job, ok := workbench.jobs[id]
	if !ok {
		workbench.mu.RUnlock()
		return
	}
	ctx := job.context
	workbench.mu.RUnlock()

	select {
	case workbench.slot <- struct{}{}:
	case <-ctx.Done():
		workbench.finishJobFromContext(id, nil, nil)
		return
	}
	slotReleased := false
	releaseSlot := func() {
		if !slotReleased {
			<-workbench.slot
			slotReleased = true
		}
	}
	defer releaseSlot()

	workbench.mu.Lock()
	job, ok = workbench.jobs[id]
	if !ok || job.Status != "queued" {
		workbench.mu.Unlock()
		return
	}
	if ctx.Err() != nil {
		workbench.mu.Unlock()
		releaseSlot()
		workbench.finishJobFromContext(id, nil, nil)
		return
	}
	started := time.Now().UTC()
	job.Status = "running"
	job.StartedAt = &started
	args := append([]string(nil), job.Args...)
	workbench.mu.Unlock()

	command := exec.Command(workbench.executable, args...)
	command.Dir = workbench.workspace
	command.Env = append([]string(nil), workbench.environment...)
	configureWorkbenchProcess(command)
	var output boundedWorkbenchBuffer
	command.Stdout = &output
	command.Stderr = &output
	if ctx.Err() != nil {
		releaseSlot()
		workbench.finishJobFromContext(id, nil, &output)
		return
	}
	if err := command.Start(); err != nil {
		releaseSlot()
		if ctx.Err() != nil {
			workbench.finishJobFromContext(id, nil, &output)
			return
		}
		workbench.finishJob(id, "failed", exitCodeFor(command), output.String(), output.Truncated(), err.Error())
		return
	}

	completed := make(chan error, 1)
	go func() { completed <- command.Wait() }()
	var (
		err                  error
		terminatedForContext bool
	)
	select {
	case err = <-completed:
	case <-ctx.Done():
		// Completion wins if it was already observable when the deadline or
		// cancellation fired. Otherwise terminate the complete process tree.
		select {
		case err = <-completed:
		default:
			terminatedForContext = true
			err = terminateWorkbenchProcess(command, completed)
		}
	}
	if terminatedForContext {
		releaseSlot()
		workbench.finishJobFromContext(id, err, &output)
		return
	}
	if err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", exitCodeFor(command), output.String(), output.Truncated(), err.Error())
		return
	}
	releaseSlot()
	workbench.finishJob(id, "succeeded", 0, output.String(), output.Truncated(), "")
}

// CancelJob explicitly cancels an active job. Cancellation waits for queue
// removal or process-tree termination so a successful return also means that
// the single execution slot was released. Repeated cancellation of a terminal
// job is idempotent.
func (workbench *Workbench) CancelJob(id string) error {
	_, err := workbench.cancelJob(id)
	return err
}

func (workbench *Workbench) cancelJob(id string) (*workbenchJob, error) {
	workbench.mu.Lock()
	job, ok := workbench.jobs[id]
	if !ok {
		workbench.mu.Unlock()
		return nil, ErrWorkbenchJobNotFound
	}
	job.retain++
	if terminalWorkbenchStatus(job.Status) {
		snapshot := copyWorkbenchJob(job)
		job.retain--
		cleanupFailed := job.Status == "cleanup_failed"
		workbench.mu.Unlock()
		if cleanupFailed {
			return snapshot, ErrWorkbenchCleanupUnproven
		}
		return snapshot, nil
	}
	cancel := job.cancel
	done := job.done
	workbench.mu.Unlock()

	cancel()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		workbench.mu.Lock()
		job, ok := workbench.jobs[id]
		if !ok {
			workbench.mu.Unlock()
			return nil, ErrWorkbenchJobNotFound
		}
		snapshot := copyWorkbenchJob(job)
		job.retain--
		cleanupFailed := job.Status == "cleanup_failed"
		workbench.mu.Unlock()
		if cleanupFailed {
			return snapshot, ErrWorkbenchCleanupUnproven
		}
		return snapshot, nil
	case <-timer.C:
		workbench.mu.Lock()
		if current, ok := workbench.jobs[id]; ok && current.retain > 0 {
			current.retain--
		}
		workbench.mu.Unlock()
		return nil, fmt.Errorf("%w: cancellation did not finish within five seconds", ErrWorkbenchCleanupUnproven)
	}
}

// Close prevents new submissions, cancels every active job, and waits until
// each job releases its execution resources or the caller's deadline expires.
func (workbench *Workbench) Close(ctx context.Context) error {
	if workbench == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("workbench close context is required")
	}
	type pendingJob struct {
		id     string
		cancel context.CancelFunc
		done   <-chan struct{}
	}
	workbench.mu.Lock()
	workbench.closed = true
	pending := make([]pendingJob, 0, len(workbench.jobs))
	var failedIDs []string
	for id, job := range workbench.jobs {
		if terminalWorkbenchStatus(job.Status) {
			if job.Status == "cleanup_failed" {
				failedIDs = append(failedIDs, id)
			}
			continue
		}
		pending = append(pending, pendingJob{id: id, cancel: job.cancel, done: job.done})
	}
	workbench.mu.Unlock()
	sort.Slice(pending, func(i, j int) bool { return pending[i].id < pending[j].id })
	sort.Strings(failedIDs)
	failures := make([]error, 0, len(failedIDs))
	for _, id := range failedIDs {
		failures = append(failures, fmt.Errorf("%w: job %s", ErrWorkbenchCleanupUnproven, id))
	}

	for _, job := range pending {
		job.cancel()
	}
	for _, job := range pending {
		select {
		case <-job.done:
			snapshot, ok := workbench.jobSnapshot(job.id)
			if !ok || snapshot.Status == "cleanup_failed" {
				failures = append(failures, fmt.Errorf("%w: job %s", ErrWorkbenchCleanupUnproven, job.id))
			}
		case <-ctx.Done():
			return errors.Join(errors.Join(failures...), fmt.Errorf("%w: %v", ErrWorkbenchCleanupUnproven, ctx.Err()))
		}
	}
	return errors.Join(failures...)
}

func (workbench *Workbench) finishJobFromContext(id string, commandErr error, output *boundedWorkbenchBuffer) {
	status := "canceled"
	message := "command canceled by user"
	workbench.mu.RLock()
	job, ok := workbench.jobs[id]
	var ctxErr error
	managedUnitRisk := false
	if ok {
		ctxErr = job.context.Err()
		managedUnitRisk = job.mayLaunchManagedUnits && job.StartedAt != nil
	}
	workbench.mu.RUnlock()
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		status = "timed_out"
		message = "command exceeded the workbench timeout"
	}
	if errors.Is(commandErr, ErrWorkbenchCleanupUnproven) || managedUnitRisk {
		status = "cleanup_failed"
		if managedUnitRisk && !errors.Is(commandErr, ErrWorkbenchCleanupUnproven) {
			message = "command stopped but cleanup of separately managed services could not be independently proven"
		} else if errors.Is(ctxErr, context.DeadlineExceeded) {
			message = "command timed out but process cleanup could not be proven"
		} else {
			message = "command was canceled but process cleanup could not be proven"
		}
	}
	outputValue := ""
	truncated := false
	if output != nil {
		outputValue = output.String()
		truncated = output.Truncated()
	}
	workbench.finishJob(id, status, -1, outputValue, truncated, message)
}

func (workbench *Workbench) finishJob(id, status string, exitCode int, output string, truncated bool, message string) {
	workbench.mu.Lock()
	defer workbench.mu.Unlock()
	if job, ok := workbench.jobs[id]; ok && !terminalWorkbenchStatus(job.Status) {
		workbench.finishJobLocked(job, status, exitCode, output, truncated, message)
	}
}

func (workbench *Workbench) finishJobLocked(job *workbenchJob, status string, exitCode int, output string, truncated bool, message string) {
	finished := time.Now().UTC()
	job.Status = status
	job.FinishedAt = &finished
	job.ExitCode = &exitCode
	job.Output = output
	job.Truncated = truncated
	job.Error = message
	job.cancel()
	close(job.done)
}

func (workbench *Workbench) jobSnapshot(id string) (*workbenchJob, bool) {
	workbench.mu.RLock()
	defer workbench.mu.RUnlock()
	job, ok := workbench.jobs[id]
	if !ok {
		return nil, false
	}
	return copyWorkbenchJob(job), true
}

func copyWorkbenchJob(job *workbenchJob) *workbenchJob {
	copy := *job
	copy.Args = append([]string(nil), job.Args...)
	copy.context = nil
	copy.cancel = nil
	copy.done = nil
	return &copy
}

func terminalWorkbenchStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "timed_out", "canceled", "cleanup_failed":
		return true
	default:
		return false
	}
}

func workbenchMayLaunchManagedUnits(args []string) bool {
	if len(args) == 0 {
		return false
	}
	hasFlag := func(name string) bool {
		for _, argument := range args[1:] {
			if argument == name || strings.HasPrefix(argument, name+"=") {
				return true
			}
		}
		return false
	}
	flagValue := func(name string) string {
		for index, argument := range args[1:] {
			if strings.HasPrefix(argument, name+"=") {
				return strings.TrimPrefix(argument, name+"=")
			}
			if argument == name && index+2 < len(args) {
				return args[index+2]
			}
		}
		return ""
	}
	switch args[0] {
	case "answer":
		return true
	case "synthesize":
		return !hasFlag("--packet-only")
	case "query":
		mode := flagValue("--mode")
		return mode == "semantic" || mode == "hybrid" ||
			hasFlag("--build-vector-index") || hasFlag("--embedding-model") ||
			hasFlag("--embedding-asset") || hasFlag("--embedding-runtime-receipt")
	case "scan":
		return !hasFlag("--no-python") && !hasFlag("--no-plugins")
	case "quickstart":
		return hasFlag("--python")
	default:
		return false
	}
}

func exitCodeFor(command *exec.Cmd) int {
	if command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}

func (workbench *Workbench) evictOneTerminalJobLocked() {
	var candidate *workbenchJob
	for _, job := range workbench.jobs {
		if !terminalWorkbenchStatus(job.Status) || job.retain != 0 {
			continue
		}
		if candidate == nil || job.CreatedAt.Before(candidate.CreatedAt) ||
			(job.CreatedAt.Equal(candidate.CreatedAt) && job.ID < candidate.ID) {
			candidate = job
		}
	}
	if candidate != nil {
		delete(workbench.jobs, candidate.ID)
	}
}

func sanitizedWorkbenchEnvironment(source []string) ([]string, error) {
	allowed := map[string]struct{}{
		"APPDATA": {}, "COMSPEC": {}, "HOME": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {},
		"LOCALAPPDATA": {}, "PATH": {}, "PATHEXT": {}, "SSL_CERT_DIR": {}, "SSL_CERT_FILE": {},
		"SYSTEMROOT": {}, "TEMP": {}, "TMP": {}, "TMPDIR": {}, "TZ": {}, "USERPROFILE": {},
		"WINDIR": {}, "XDG_CACHE_HOME": {}, "XDG_CONFIG_HOME": {}, "XDG_DATA_HOME": {},
		"XDG_STATE_HOME": {}, "XDG_RUNTIME_DIR": {}, "DBUS_SESSION_BUS_ADDRESS": {},
		"GOMAXPROCS": {}, "OMP_NUM_THREADS": {}, "OPENBLAS_NUM_THREADS": {},
		"MKL_NUM_THREADS": {}, "NUMEXPR_NUM_THREADS": {}, "CMAKE_BUILD_PARALLEL_LEVEL": {},
		"CARGO_BUILD_JOBS": {}, "GOFLAGS": {}, "CGO_ENABLED": {},
	}
	values := map[string]string{
		"NO_COLOR":      "1",
		"RKC_WORKBENCH": "1",
		"TERM":          "dumb",
	}
	for _, item := range source {
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		if _, permitted := allowed[name]; permitted {
			values[name] = value
		}
	}
	for _, name := range []string{
		"GOMAXPROCS", "OMP_NUM_THREADS", "OPENBLAS_NUM_THREADS", "MKL_NUM_THREADS",
		"NUMEXPR_NUM_THREADS", "CMAKE_BUILD_PARALLEL_LEVEL", "CARGO_BUILD_JOBS",
	} {
		values[name] = "1"
	}
	values["GOFLAGS"] = "-p=1"
	if cgo := values["CGO_ENABLED"]; cgo != "" && cgo != "0" && cgo != "1" {
		return nil, errors.New("CGO_ENABLED must be empty, 0, or 1")
	}
	if err := validateWorkbenchSystemdEnvironment(values); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment, nil
}

func validateWorkbenchSystemdEnvironment(values map[string]string) error {
	runtimeDirectory := values["XDG_RUNTIME_DIR"]
	busAddress := values["DBUS_SESSION_BUS_ADDRESS"]
	if runtimeDirectory == "" && busAddress == "" {
		return nil
	}
	if runtimeDirectory == "" || busAddress == "" {
		return errors.New("XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS must be provided together")
	}
	if strings.ContainsRune(runtimeDirectory, '\x00') || !filepath.IsAbs(runtimeDirectory) ||
		filepath.Clean(runtimeDirectory) != runtimeDirectory {
		return errors.New("XDG_RUNTIME_DIR must be a canonical absolute path")
	}
	info, err := os.Lstat(runtimeDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("XDG_RUNTIME_DIR must be an existing owner-private direct directory")
	}
	resolved, err := filepath.EvalSymlinks(runtimeDirectory)
	if err != nil || resolved != runtimeDirectory {
		return errors.New("XDG_RUNTIME_DIR must not traverse symbolic links")
	}
	expectedBus := "unix:path=" + filepath.ToSlash(filepath.Join(runtimeDirectory, "bus"))
	if busAddress != expectedBus {
		return errors.New("DBUS_SESSION_BUS_ADDRESS must name the user bus beneath XDG_RUNTIME_DIR")
	}
	return validateWorkbenchUserManagerEndpoint(runtimeDirectory)
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
