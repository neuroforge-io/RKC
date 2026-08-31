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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/neuroforge-io/RKC/internal/commandcatalog"
)

const (
	workbenchMaximumRequestBytes     = 64 * 1024
	workbenchMaximumOutputBytes      = 2 * 1024 * 1024
	workbenchMaximumArguments        = 128
	workbenchMaximumArgumentSize     = 4096
	workbenchMaximumJobs             = 32
	workbenchMaximumDirectoryEntries = 256
	workbenchMaximumInspectedEntries = 4096
	workbenchTokenHeader             = "X-RKC-Workbench-Token"
	workbenchBootstrapHeader         = "X-RKC-Workbench-Bootstrap"
	workbenchBootstrapFragment       = "rkc-workbench"
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
	Workspace      string
	Executable     string
	Timeout        time.Duration
	CommandContext commandcatalog.Context
}

// Workbench runs exact RKC argv vectors without a shell. It is deliberately
// loopback-only, single-job, bounded, and token authenticated.
type Workbench struct {
	workspace   string
	executable  string
	timeout     time.Duration
	token       string
	bootstrap   string
	slot        chan struct{}
	environment []string
	commands    []workbenchCommand

	mu   sync.RWMutex
	jobs map[string]*workbenchJob
	// activeDataset and commands are replaced under the same lock after a
	// successful quickstart has been loaded and its repository identity has been
	// verified. HTTP handlers retain immutable Dataset pointers after releasing
	// the lock, so every request observes one complete atlas generation.
	activeDataset *Dataset
	closed        bool
}

type workbenchDatasetIdentity struct {
	SnapshotID     string `json:"snapshot_id"`
	RepositoryID   string `json:"repository_id,omitempty"`
	RootName       string `json:"root_name"`
	RepositoryRoot string `json:"repository_root"`
	AtlasRoot      string `json:"atlas_root"`
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
	// ActivatedDataset is present only after the server has fully loaded the
	// quickstart output, checked its snapshot/repository identity, and atomically
	// made it the dataset served by the read APIs.
	ActivatedDataset *workbenchDatasetIdentity `json:"activated_dataset,omitempty"`

	context               context.Context
	cancel                context.CancelFunc
	done                  chan struct{}
	retain                int
	mayLaunchManagedUnits bool
}

type workbenchDirectoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type workbenchDirectoryListing struct {
	Path        string                    `json:"path"`
	Parent      string                    `json:"parent,omitempty"`
	Directories []workbenchDirectoryEntry `json:"directories"`
	Truncated   bool                      `json:"truncated"`
}

type workbenchCommand struct {
	commandcatalog.Command
	DefaultExecutable bool   `json:"default_executable"`
	Restriction       string `json:"restriction,omitempty"`
}

var workbenchCommands = commandcatalog.Commands(commandcatalog.Context{})

// NewWorkbench validates and binds the workspace and executable identities,
// sanitizes the inherited environment, creates private bearer capabilities,
// and returns a bounded, single-slot command runner.
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
	bootstrap, err := randomWorkbenchValue(32)
	if err != nil {
		return nil, fmt.Errorf("create workbench bootstrap capability: %w", err)
	}
	return &Workbench{
		workspace: workspace, executable: executable, timeout: config.Timeout,
		token: token, bootstrap: bootstrap, slot: make(chan struct{}, 1), environment: environment,
		commands: workbenchCommandViews(commandcatalog.Commands(config.CommandContext)),
		jobs:     make(map[string]*workbenchJob),
	}, nil
}

func workbenchCommandViews(commands []commandcatalog.Command) []workbenchCommand {
	result := make([]workbenchCommand, 0, len(commands))
	for _, command := range commands {
		arguments := append([]string{command.Name}, command.DefaultArgs...)
		view := workbenchCommand{Command: command, DefaultExecutable: true}
		if err := validateWorkbenchExecution(arguments); err != nil {
			view.DefaultExecutable = false
			view.Restriction = err.Error()
		}
		result = append(result, view)
	}
	return result
}

// attachDataset binds the immutable dataset used when the live handler starts.
// A Workbench may be constructed before the handler, so activation deliberately
// fails closed until this binding exists.
func (workbench *Workbench) attachDataset(dataset *Dataset) {
	if workbench == nil || dataset == nil {
		return
	}
	workbench.mu.Lock()
	defer workbench.mu.Unlock()
	if workbench.activeDataset == nil {
		workbench.activeDataset = dataset
	}
}

func (workbench *Workbench) currentDataset(fallback *Dataset) *Dataset {
	if workbench == nil {
		return fallback
	}
	workbench.mu.RLock()
	dataset := workbench.activeDataset
	workbench.mu.RUnlock()
	if dataset == nil {
		return fallback
	}
	return dataset
}

func workbenchDatasetIdentityFor(dataset *Dataset) *workbenchDatasetIdentity {
	if dataset == nil {
		return nil
	}
	repositoryRoot := dataset.Manifest.RootPath
	if repositoryRoot == "" && filepath.Base(dataset.Root) == ".rkc" {
		repositoryRoot = filepath.Dir(dataset.Root)
	}
	return workbenchDatasetIdentityForRepository(dataset, repositoryRoot)
}

func workbenchDatasetIdentityForRepository(dataset *Dataset, repositoryRoot string) *workbenchDatasetIdentity {
	return &workbenchDatasetIdentity{
		SnapshotID:     dataset.Manifest.ID,
		RepositoryID:   dataset.Manifest.RepositoryID,
		RootName:       dataset.Manifest.RootName,
		RepositoryRoot: repositoryRoot,
		AtlasRoot:      dataset.Root,
	}
}

func workbenchCommandContextForDataset(dataset *Dataset) commandcatalog.Context {
	return commandcatalog.Context{
		DatasetArgs: []string{"--dir", dataset.Root},
		CheckArgs:   []string{"--coverage", filepath.Join(dataset.Root, "coverage.json")},
	}
}

// completedQuickstartRoots recognizes the exact argv emitted by the graphical
// Analyze action. Flag-bearing/manual quickstart vectors retain their ordinary
// command behavior but do not silently change the live dataset because their
// effective output path would require command-specific flag evaluation.
func (workbench *Workbench) completedQuickstartRoots(args []string) (string, string, bool, error) {
	if len(args) != 2 || args[0] != "quickstart" || strings.HasPrefix(args[1], "-") {
		return "", "", false, nil
	}
	repository := args[1]
	if !filepath.IsAbs(repository) {
		repository = filepath.Join(workbench.workspace, repository)
	}
	repository, err := filepath.Abs(repository)
	if err != nil {
		return "", "", true, fmt.Errorf("resolve analyzed repository: %w", err)
	}
	repository, err = filepath.EvalSymlinks(repository)
	if err != nil {
		return "", "", true, fmt.Errorf("resolve analyzed repository links: %w", err)
	}
	info, err := os.Stat(repository)
	if err != nil {
		return "", "", true, fmt.Errorf("inspect analyzed repository: %w", err)
	}
	if !info.IsDir() {
		return "", "", true, errors.New("analyzed repository is not a directory")
	}
	return repository, filepath.Join(repository, ".rkc"), true, nil
}

// activateCompletedQuickstart is a postcondition of graphical Analyze, not a
// second command. Load performs the complete export-manifest, canonical bundle,
// coverage, and vocabulary validation before any shared pointer changes. The
// repository root recorded in the immutable snapshot must also identify the
// exact folder supplied to quickstart.
func (workbench *Workbench) activateCompletedQuickstart(id string, args []string) error {
	repository, atlas, requested, err := workbench.completedQuickstartRoots(args)
	if err != nil || !requested {
		return err
	}
	dataset, err := Load(atlas)
	if err != nil {
		return fmt.Errorf("load analyzed atlas: %w", err)
	}
	resolvedAtlas, err := filepath.EvalSymlinks(atlas)
	if err != nil {
		return fmt.Errorf("resolve analyzed atlas links: %w", err)
	}
	resolvedAtlas, err = filepath.Abs(resolvedAtlas)
	if err != nil {
		return fmt.Errorf("resolve analyzed atlas: %w", err)
	}
	if dataset.Root != resolvedAtlas {
		return fmt.Errorf("loaded atlas root %q does not match analyzed output %q", dataset.Root, resolvedAtlas)
	}
	if dataset.Integrity != IntegrityVerified {
		return fmt.Errorf("analyzed atlas integrity is %q, expected %q", dataset.Integrity, IntegrityVerified)
	}
	if dataset.Manifest.ID == "" || dataset.Manifest.Status != "committed" {
		return fmt.Errorf("analyzed atlas snapshot is not committed (id=%q status=%q)", dataset.Manifest.ID, dataset.Manifest.Status)
	}
	// Canonical portable atlases intentionally redact host-local RootPath. The
	// selected repository is instead bound by the exact resolved <repo>/.rkc
	// location, ownership marker, export-manifest snapshot ID, and deterministic
	// root name retained in the canonical snapshot.
	if dataset.Manifest.RootName != filepath.Base(repository) {
		return fmt.Errorf("analyzed snapshot root name %q does not match selected folder %q", dataset.Manifest.RootName, filepath.Base(repository))
	}

	identity := workbenchDatasetIdentityForRepository(dataset, repository)
	commands := workbenchCommandViews(commandcatalog.Commands(workbenchCommandContextForDataset(dataset)))
	workbench.mu.Lock()
	defer workbench.mu.Unlock()
	job, ok := workbench.jobs[id]
	if !ok || job.Status != "running" {
		return errors.New("analyzed atlas job is no longer active")
	}
	if err := job.context.Err(); err != nil {
		return fmt.Errorf("analyzed atlas activation was canceled: %w", err)
	}
	if workbench.closed || workbench.activeDataset == nil {
		return errors.New("live dataset activation is unavailable while the workbench is shutting down")
	}
	workbench.activeDataset = dataset
	workbench.commands = commands
	job.ActivatedDataset = identity
	return nil
}

func randomWorkbenchValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (workbench *Workbench) handleSession(w http.ResponseWriter, request *http.Request) {
	if !validWorkbenchRequestHost(request) || !validWorkbenchOrigin(request) || !workbench.authorizeSession(request) {
		writeProblem(w, http.StatusForbidden, "Workbench authorization failed", "same-origin loopback access and the out-of-band bootstrap capability are required")
		return
	}
	workbench.mu.RLock()
	commands := append([]workbenchCommand(nil), workbench.commands...)
	activeDataset := workbenchDatasetIdentityFor(workbench.activeDataset)
	workbench.mu.RUnlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "token": workbench.token, "workspace": workbench.workspace,
		"maximum_output_bytes": workbenchMaximumOutputBytes,
		"timeout_seconds":      int(workbench.timeout.Seconds()),
		"authority_notice":     "Trusted-user launcher: commands have the invoking OS user's filesystem authority; the workspace sets command defaults and is not a security sandbox. Use a trusted browser profile: ephemeral origin allocation reduces, but cannot prove the absence of, legacy service-worker state.",
		"commands":             commands,
		"active_dataset":       activeDataset,
	})
}

// BrowserURL binds the one-time bootstrap capability to a URL fragment. URL
// fragments are never sent in HTTP requests; the generated browser removes it
// before exchanging it for the session token.
func (workbench *Workbench) BrowserURL(rawURL string) (string, error) {
	if workbench == nil {
		return "", errors.New("workbench is not configured")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Fragment != "" {
		return "", errors.New("workbench browser URL must be an unfragmented HTTP URL")
	}
	workbench.mu.RLock()
	bootstrap := workbench.bootstrap
	workbench.mu.RUnlock()
	if bootstrap == "" {
		return "", errors.New("workbench bootstrap capability has already been consumed")
	}
	parsed.Fragment = url.Values{workbenchBootstrapFragment: []string{bootstrap}}.Encode()
	return parsed.String(), nil
}

func (workbench *Workbench) authorizeSession(request *http.Request) bool {
	providedToken := request.Header.Get(workbenchTokenHeader)
	if len(providedToken) == len(workbench.token) &&
		subtle.ConstantTimeCompare([]byte(providedToken), []byte(workbench.token)) == 1 {
		return true
	}
	providedBootstrap := request.Header.Get(workbenchBootstrapHeader)
	workbench.mu.Lock()
	defer workbench.mu.Unlock()
	if workbench.bootstrap == "" || len(providedBootstrap) != len(workbench.bootstrap) ||
		subtle.ConstantTimeCompare([]byte(providedBootstrap), []byte(workbench.bootstrap)) != 1 {
		return false
	}
	workbench.bootstrap = ""
	return true
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
	if err := validateWorkbenchExecution(payload.Args); err != nil {
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

func validateWorkbenchExecution(args []string) error {
	if err := validateWorkbenchArgs(args); err != nil {
		return err
	}
	if workbenchUsesCustomOrUnpinnedSCIPIndexer(args) {
		return errors.New("custom or unpinned SCIP indexer execution is unavailable in the workbench; pin and run custom indexers from the protected terminal workflow, then import or verify the generated index here")
	}
	if workbenchMayLaunchManagedUnits(args) {
		return errors.New("this command can start a separately managed runtime and is disabled until the workbench can prove one aggregate resource ceiling")
	}
	return nil
}

// workbenchUsesCustomOrUnpinnedSCIPIndexer removes one GUI route that can select
// an arbitrary executable or bypass its digest pin before execution. A custom
// binary can call setsid and leave the per-job process group, so this capability
// remains terminal-only until cleanup can prove a stronger kernel-enforced
// containment boundary. Other helper-launching routes are classified below.
// All indexer generation, including a pinned canonical tool, is separately
// classified as managed-unit risk; imported indexes, verification, and pin-file
// maintenance remain available.
func workbenchUsesCustomOrUnpinnedSCIPIndexer(args []string) bool {
	if len(args) == 0 {
		return false
	}
	var restricted []string
	switch args[0] {
	case "scan", "quickstart":
		restricted = []string{"scip-tool", "scip-no-pin-check"}
	case "scip":
		if len(args) < 2 || args[1] != "generate" {
			return false
		}
		restricted = []string{"tool", "no-pin-check"}
	default:
		return false
	}
	for _, argument := range args[1:] {
		for _, name := range restricted {
			if _, _, matched := workbenchFlagToken(argument, name); matched {
				return true
			}
		}
	}
	return false
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

// handleDirectories provides the non-technical folder chooser with a bounded,
// directory-only view of the invoking user's filesystem. It is available only
// on the explicitly enabled, token-authenticated workbench origin. The selected
// workspace is a convenience default rather than a sandbox, so an absolute path
// can deliberately navigate elsewhere under the same OS-user authority.
func (workbench *Workbench) handleDirectories(w http.ResponseWriter, request *http.Request) {
	if !workbench.authorize(request) {
		writeProblem(w, http.StatusForbidden, "Workbench authorization failed", "same-origin token authentication is required")
		return
	}
	listing, err := workbench.directoryListing(request.URL.Query())
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Folder cannot be opened", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, listing)
}

func (workbench *Workbench) directoryListing(values url.Values) (workbenchDirectoryListing, error) {
	for key, entries := range values {
		if key != "path" {
			return workbenchDirectoryListing{}, fmt.Errorf("unsupported folder-browser parameter %q", key)
		}
		if len(entries) != 1 {
			return workbenchDirectoryListing{}, errors.New("folder browser accepts one path parameter")
		}
	}
	requested := values.Get("path")
	if requested == "" {
		requested = workbench.workspace
	}
	if len(requested) > workbenchMaximumArgumentSize || strings.IndexByte(requested, 0) >= 0 {
		return workbenchDirectoryListing{}, errors.New("folder path must be non-empty and at most 4096 bytes")
	}
	for _, character := range requested {
		if unicode.IsControl(character) {
			return workbenchDirectoryListing{}, errors.New("folder path contains unsupported control characters")
		}
	}
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(workbench.workspace, requested)
	}
	resolved, err := filepath.Abs(requested)
	if err != nil {
		return workbenchDirectoryListing{}, fmt.Errorf("resolve folder: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return workbenchDirectoryListing{}, fmt.Errorf("resolve folder links: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return workbenchDirectoryListing{}, fmt.Errorf("inspect folder: %w", err)
	}
	if !info.IsDir() {
		return workbenchDirectoryListing{}, errors.New("selected path is not a directory")
	}

	directory, err := os.Open(resolved)
	if err != nil {
		return workbenchDirectoryListing{}, fmt.Errorf("open folder: %w", err)
	}
	defer directory.Close()
	listing := workbenchDirectoryListing{Path: resolved, Directories: []workbenchDirectoryEntry{}}
	if parent := filepath.Dir(resolved); parent != resolved {
		listing.Parent = parent
	}
	inspected := 0
	for len(listing.Directories) < workbenchMaximumDirectoryEntries && inspected < workbenchMaximumInspectedEntries {
		remaining := workbenchMaximumInspectedEntries - inspected
		batchSize := 128
		if remaining < batchSize {
			batchSize = remaining
		}
		entries, readErr := directory.ReadDir(batchSize)
		inspected += len(entries)
		for _, entry := range entries {
			// Symlinks remain selectable through the explicit path box, but are not
			// followed while enumerating. This keeps traversal finite and avoids
			// surprising jumps in the ordinary point-and-click path.
			if !entry.IsDir() {
				continue
			}
			listing.Directories = append(listing.Directories, workbenchDirectoryEntry{
				Name: entry.Name(), Path: filepath.Join(resolved, entry.Name()),
			})
			if len(listing.Directories) == workbenchMaximumDirectoryEntries {
				listing.Truncated = true
				break
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return workbenchDirectoryListing{}, fmt.Errorf("read folder: %w", readErr)
		}
	}
	if inspected == workbenchMaximumInspectedEntries {
		listing.Truncated = true
	}
	sort.Slice(listing.Directories, func(i, j int) bool {
		left, right := strings.ToLower(listing.Directories[i].Name), strings.ToLower(listing.Directories[j].Name)
		if left == right {
			return listing.Directories[i].Name < listing.Directories[j].Name
		}
		return left < right
	})
	return listing, nil
}

func (workbench *Workbench) authorize(request *http.Request) bool {
	if !validWorkbenchRequestHost(request) || !validWorkbenchOrigin(request) {
		return false
	}
	provided := request.Header.Get(workbenchTokenHeader)
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
	descendantsFound, cleanupErr := verifyWorkbenchProcessCompletion(command)
	if descendantsFound || cleanupErr != nil {
		releaseSlot()
		status, message := workbenchCompletionCleanupFailure(err, descendantsFound, cleanupErr)
		workbench.finishJob(id, status, exitCodeFor(command), output.String(), output.Truncated(), message)
		return
	}
	if err != nil {
		releaseSlot()
		workbench.finishJob(id, "failed", exitCodeFor(command), output.String(), output.Truncated(), err.Error())
		return
	}
	if ctx.Err() != nil {
		releaseSlot()
		workbench.finishJobFromContext(id, nil, &output)
		return
	}
	// Keep the single execution slot through validation and pointer publication.
	// Otherwise a queued command could mutate the just-produced atlas between
	// quickstart exit and the integrity loader's immutable in-memory capture.
	if err := workbench.activateCompletedQuickstart(id, args); err != nil {
		releaseSlot()
		workbench.finishJob(
			id,
			"failed",
			0,
			output.String(),
			output.Truncated(),
			"quickstart completed but the analyzed atlas was not activated: "+err.Error(),
		)
		return
	}
	releaseSlot()
	workbench.finishJob(id, "succeeded", 0, output.String(), output.Truncated(), "")
}

func workbenchCompletionCleanupFailure(commandErr error, descendantsFound bool, cleanupErr error) (string, string) {
	status := "failed"
	detail := "command completion cleanup could not be proven"
	if descendantsFound {
		detail = "command exited while descendant processes remained; the process group was terminated"
	}
	if cleanupErr != nil {
		status = "cleanup_failed"
		if descendantsFound {
			detail = "command exited while descendant processes remained, but process-group cleanup could not be proven"
		}
	}
	if commandErr == nil {
		return status, detail
	}
	return status, errors.Join(commandErr, errors.New(detail)).Error()
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
	if job.ActivatedDataset != nil {
		identity := *job.ActivatedDataset
		copy.ActivatedDataset = &identity
	}
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
	switch args[0] {
	case "answer":
		return true
	case "doctor":
		// Doctor probes ambient Git and Python executables, and --config may name
		// a different Python interpreter. Help is parsed before any probe; every
		// executable doctor path remains terminal-only until the whole helper
		// tree shares a kernel-enforced cleanup boundary.
		return !workbenchExactHelp(args)
	case "synthesize":
		// The first option must unambiguously disable generation. Requiring the
		// safety flag first prevents another string flag from consuming it as a
		// value. Any later false override fails closed, even when it would occur
		// after a positional parsing boundary.
		return !workbenchLeadingTrueFlag(args, "packet-only") ||
			workbenchFlagCanBeFalse(args[2:], "packet-only")
	case "query":
		return workbenchQueryMayLaunchManagedUnit(args[1:])
	case "scan":
		// Remote acquisition invokes Git, while these flags can select an
		// arbitrary Git/Python executable or load helper defaults from a config
		// file. Do not infer safety merely from --no-python/--no-plugins: either
		// flag can otherwise mask a separately executable acquisition route.
		if workbenchHasAnyFlag(args[1:], "config", "git", "python", "python-plugin") ||
			workbenchScanLooksRemote(args[1:]) ||
			workbenchHasFlag(args[1:], "scip-generate") {
			return true
		}
		pythonDisabled := workbenchLeadingTrueFlag(args, "no-python") &&
			!workbenchFlagCanBeFalse(args[2:], "no-python")
		pluginsDisabled := workbenchLeadingTrueFlag(args, "no-plugins") &&
			!workbenchFlagCanBeFalse(args[2:], "no-plugins")
		return !pythonDisabled && !pluginsDisabled
	case "quickstart":
		// Guided Analyze submits only a local folder. A custom configuration can
		// redirect plugin/helper discovery, so keep that advanced route in the
		// terminal alongside Python and SCIP generation.
		return workbenchHasFlag(args[1:], "config") ||
			workbenchHasFlag(args[1:], "scip-generate") ||
			workbenchFlagCanBeTrue(args[1:], "python")
	case "history":
		// build and symbol execute Git walks. report/help consume an already
		// compiled bounded file and do not launch a helper.
		return len(args) >= 2 && (args[1] == "build" || args[1] == "symbol")
	case "scip":
		// Digest pinning authenticates the executable, not the lifecycle of
		// descendants it may detach. Generation therefore remains terminal-only
		// until the workbench has kernel-enforced descendant cleanup.
		return len(args) >= 2 && args[1] == "generate"
	case "trace":
		// Capture intentionally accepts an arbitrary command vector. A captured
		// command can create descendants outside the per-job process group, so the
		// workbench exposes only the non-executing report, verify, and help paths.
		return len(args) >= 2 && args[1] == "capture"
	case "wizard":
		// The browser catalogue may display the terminal guide's help, but an
		// interactive wizard can select open and therefore start a nested server.
		// The workbench has no terminal input and must not rely on that incidental
		// EOF behavior as its safety boundary.
		return len(args) != 2 || args[1] != "--help"
	default:
		return false
	}
}

func workbenchExactHelp(args []string) bool {
	return len(args) == 2 && (args[1] == "--help" || args[1] == "-h")
}

func workbenchHasAnyFlag(args []string, names ...string) bool {
	for _, name := range names {
		if workbenchHasFlag(args, name) {
			return true
		}
	}
	return false
}

// workbenchScanLooksRemote conservatively recognizes every remote source form
// accepted by acquire.Open. False positives are preferable to launching Git
// from the browser; local scans and the guided quickstart remain available.
func workbenchScanLooksRemote(args []string) bool {
	for _, argument := range args {
		lower := strings.ToLower(strings.TrimSpace(argument))
		for _, scheme := range []string{"https://", "ssh://", "file://", "git://"} {
			if strings.HasPrefix(lower, scheme) {
				return true
			}
		}
		at := strings.Index(argument, "@")
		if at > 0 && strings.Contains(argument[at+1:], ":") {
			return true
		}
	}
	return false
}

func workbenchHasFlag(args []string, name string) bool {
	for _, argument := range args {
		if _, _, matched := workbenchFlagToken(argument, name); matched {
			return true
		}
	}
	return false
}

// workbenchFlagToken recognizes the one- and two-dash forms accepted by Go's
// flag package without accepting abbreviations or unrelated argument text.
func workbenchFlagToken(argument, name string) (value string, hasValue, matched bool) {
	for _, prefix := range []string{"--", "-"} {
		candidate := prefix + name
		if argument == candidate {
			return "", false, true
		}
		if strings.HasPrefix(argument, candidate+"=") {
			return strings.TrimPrefix(argument, candidate+"="), true, true
		}
	}
	return "", false, false
}

func workbenchLeadingTrueFlag(args []string, name string) bool {
	if len(args) < 2 {
		return false
	}
	value, hasValue, matched := workbenchFlagToken(args[1], name)
	if !matched || !hasValue {
		return matched
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func workbenchFlagCanBeTrue(args []string, name string) bool {
	for _, argument := range args {
		value, hasValue, matched := workbenchFlagToken(argument, name)
		if !matched {
			continue
		}
		if !hasValue {
			return true
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil || parsed {
			return true
		}
	}
	return false
}

func workbenchFlagCanBeFalse(args []string, name string) bool {
	for _, argument := range args {
		value, hasValue, matched := workbenchFlagToken(argument, name)
		if !matched || !hasValue {
			continue
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil || !parsed {
			return true
		}
	}
	return false
}

func workbenchQueryMayLaunchManagedUnit(args []string) bool {
	semanticOptions := map[string]struct{}{
		"vector-index": {}, "build-vector-index": {}, "embedding-model": {},
		"llama-embedding": {}, "embedding-model-lock": {}, "embedding-asset": {},
		"embedding-runtime-receipt": {},
	}
	for index, argument := range args {
		for name := range semanticOptions {
			if _, _, matched := workbenchFlagToken(argument, name); matched {
				return true
			}
		}
		value, hasValue, matched := workbenchFlagToken(argument, "mode")
		if !matched {
			continue
		}
		if !hasValue {
			if index+1 >= len(args) {
				return true
			}
			value = args[index+1]
		}
		if value != "lexical" {
			return true
		}
	}
	return false
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

// Write accepts the caller's full byte count while retaining at most the
// configured output limit. Bytes beyond that bound set the truncation flag.
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

// String returns a synchronized copy of the retained output prefix.
func (buffer *boundedWorkbenchBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

// Truncated reports whether any accepted output bytes were discarded.
func (buffer *boundedWorkbenchBuffer) Truncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.truncated
}
