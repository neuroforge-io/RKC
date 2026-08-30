// Package plugin validates and executes the bounded plugin protocol used by
// RKC's extractor workers.
package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/neuroforge-io/RKC/internal/model"
)

const (
	// MaximumMemoryMiB is the largest memory ceiling RunPython admits for the
	// built-in adapter.
	MaximumMemoryMiB = int64(2048)
	// MaximumSwapMiB is the largest swap ceiling RunPython admits; the configured
	// value must also be no greater than the memory ceiling.
	MaximumSwapMiB = int64(512)
	// MaximumProcesses is the exact task ceiling required while process spawning
	// is denied.
	MaximumProcesses = 1
)

// FileRef is an inventory-bound wire reference to one requested source file.
// RunPython admits it only after root confinement and exact size, identity, and
// SHA-256 verification; the built-in worker repeats content verification before
// parsing the path it reopens.
type FileRef struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Language  string `json:"language"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Request is the JSON request sent to the built-in adapter. Root becomes the
// isolated worker's working directory. An empty Files slice makes RunPython
// return an empty fragment before validating the root or execution options.
type Request struct {
	SchemaVersion string    `json:"schema_version"`
	SnapshotID    string    `json:"snapshot_id"`
	Root          string    `json:"root"`
	Files         []FileRef `json:"files"`
}

// PythonOptions configures the digest-pinned built-in Python adapter. Nonempty
// work is admitted only when sandboxing, network denial, and process-spawn denial
// are all required and the resource ceilings satisfy RunPython's bounds.
type PythonOptions struct {
	Interpreter          string
	Script               string
	Timeout              time.Duration
	MaxOutputBytes       int64
	MaxStderrBytes       int64
	MemoryLimitMiB       int64
	SwapLimitMiB         int64
	ProcessLimit         int
	RequireSandbox       bool
	DenyNetwork          bool
	DenyProcessSpawn     bool
	Builtin              bool
	ExpectedScriptSHA256 string
	runner               pythonRunner
}

// RunPython executes a nonempty request through the built-in, digest-verified
// adapter under a timeout and bounded stdout/stderr. Every requested source is
// confined beneath Root and verified against its exact inventory identity before
// launch. The production runner is a fail-closed Linux systemd sandbox with no
// shell, network, or child-process allowance. It accepts exactly one strict JSON
// Fragment; decoding success does not replace downstream semantic validation.
func RunPython(ctx context.Context, request Request, opts PythonOptions) (model.Fragment, error) {
	if len(request.Files) == 0 {
		return model.Fragment{}, nil
	}
	if opts.Interpreter == "" {
		opts.Interpreter = "python3"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.Script == "" {
		return model.Fragment{}, fmt.Errorf("python plugin script is required")
	}
	if !opts.Builtin {
		return model.Fragment{}, errors.New("external Python plugins are disabled; only the digest-pinned built-in adapter may execute")
	}
	if !opts.RequireSandbox || !opts.DenyNetwork || !opts.DenyProcessSpawn {
		return model.Fragment{}, errors.New("built-in Python adapter requires fail-closed resource, network, and process isolation")
	}
	if opts.MemoryLimitMiB < 16 || opts.MemoryLimitMiB > MaximumMemoryMiB {
		return model.Fragment{}, fmt.Errorf("python plugin memory limit must be between 16 and %d MiB", MaximumMemoryMiB)
	}
	if opts.SwapLimitMiB < 0 || opts.SwapLimitMiB > MaximumSwapMiB || opts.SwapLimitMiB > opts.MemoryLimitMiB {
		return model.Fragment{}, fmt.Errorf("python plugin swap limit must be between 0 and %d MiB and no greater than memory", MaximumSwapMiB)
	}
	if opts.ProcessLimit != MaximumProcesses {
		return model.Fragment{}, errors.New("python plugin task limit must be exactly 1 while process spawning is denied")
	}
	absScript, err := verifyBuiltinScript(opts.Script, opts.ExpectedScriptSHA256)
	if err != nil {
		return model.Fragment{}, err
	}
	pluginCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if pluginCtx.Err() != nil {
		return model.Fragment{}, pythonPluginContextError(ctx, pluginCtx, opts.Timeout)
	}
	root, err := validatePythonFileRefs(pluginCtx, request)
	if err != nil {
		if pluginCtx.Err() != nil {
			return model.Fragment{}, pythonPluginContextError(ctx, pluginCtx, opts.Timeout)
		}
		return model.Fragment{}, fmt.Errorf("validate python plugin file references: %w", err)
	}
	request.Root = root
	payload, err := json.Marshal(request)
	if err != nil {
		return model.Fragment{}, fmt.Errorf("encode plugin request: %w", err)
	}

	stdout := newLimitedBuffer(opts.MaxOutputBytes, 64*1024*1024)
	stderr := newLimitedBuffer(opts.MaxStderrBytes, 2*1024*1024)
	runner := opts.runner
	if runner == nil {
		runner = systemdPythonRunner{}
	}
	spec := pythonRunSpec{Interpreter: opts.Interpreter, Script: absScript, Root: request.Root, MemoryLimitMiB: opts.MemoryLimitMiB, SwapLimitMiB: opts.SwapLimitMiB, ProcessLimit: opts.ProcessLimit}
	if err := runner.Run(pluginCtx, spec, payload, stdout, stderr); err != nil {
		if pluginCtx.Err() != nil {
			return model.Fragment{}, pythonPluginContextError(ctx, pluginCtx, opts.Timeout)
		}
		return model.Fragment{}, fmt.Errorf("python plugin failed: %w: %s", err, stderr.String())
	}
	if stdout.Truncated() {
		return model.Fragment{}, fmt.Errorf("python plugin output exceeded %d bytes", stdout.Limit())
	}
	if stderr.Truncated() {
		return model.Fragment{}, fmt.Errorf("python plugin stderr exceeded %d bytes", stderr.Limit())
	}
	var fragment model.Fragment
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fragment); err != nil {
		return model.Fragment{}, fmt.Errorf("decode python plugin response: %w; stderr=%s", err, stderr.String())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values are not permitted")
		}
		return model.Fragment{}, fmt.Errorf("decode python plugin response: %w; stderr=%s", err, stderr.String())
	}
	return fragment, nil
}

// pythonPluginContextError preserves the distinction between the plugin's own
// wall-time ceiling and cancellation inherited from its caller.
func pythonPluginContextError(parent, plugin context.Context, timeout time.Duration) error {
	if errors.Is(plugin.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		return fmt.Errorf("python plugin timed out after %s", timeout)
	}
	return fmt.Errorf("python plugin cancelled: %w", plugin.Err())
}

// validatePythonFileRefs binds every requested path to one stable, real root
// and returns the absolute root value sent to the worker.
func validatePythonFileRefs(ctx context.Context, request Request) (string, error) {
	if request.Root == "" {
		return "", errors.New("request root is empty")
	}
	absoluteRoot, err := filepath.Abs(request.Root)
	if err != nil {
		return "", fmt.Errorf("resolve request root: %w", err)
	}
	pathRootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("inspect request root: %w", err)
	}
	if pathRootInfo.Mode()&os.ModeSymlink != 0 || !pathRootInfo.IsDir() {
		return "", errors.New("request root must be a real directory, not a symlink")
	}
	root, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("open request root: %w", err)
	}
	defer root.Close()
	openedRootInfo, err := root.Stat(".")
	if err != nil || !openedRootInfo.IsDir() || !os.SameFile(pathRootInfo, openedRootInfo) {
		return "", errors.New("request root identity changed while opening")
	}
	for _, file := range request.Files {
		if err := validatePythonFileRef(ctx, root, file); err != nil {
			return "", fmt.Errorf("file %q: %w", file.Path, err)
		}
	}
	finalRootInfo, err := os.Lstat(absoluteRoot)
	if err != nil || finalRootInfo.Mode()&os.ModeSymlink != 0 || !finalRootInfo.IsDir() || !os.SameFile(openedRootInfo, finalRootInfo) {
		return "", errors.New("request root identity changed during validation")
	}
	return absoluteRoot, nil
}

// validatePythonFileRef proves confinement, regular-file identity, exact size,
// content digest, and post-read path stability for one inventory record.
func validatePythonFileRef(ctx context.Context, root *os.Root, file FileRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if file.SizeBytes < 0 {
		return errors.New("inventoried size must be nonnegative")
	}
	expectedDigest, err := canonicalPythonFileDigest(file.SHA256)
	if err != nil {
		return err
	}
	nativePath, err := canonicalPythonFilePath(file.Path)
	if err != nil {
		return err
	}
	pathInfo, err := lstatPythonFilePath(root, file.Path)
	if err != nil {
		return err
	}
	if !pathInfo.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	source, err := root.Open(nativePath)
	if err != nil {
		return fmt.Errorf("open file beneath request root: %w", err)
	}
	defer source.Close()
	openedInfo, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat opened file: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return errors.New("file identity changed while opening")
	}
	if openedInfo.Size() != file.SizeBytes {
		return errors.New("file size does not match inventory")
	}
	readResult, err := readPythonFileBounded(ctx, source, file.SizeBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(readResult.digest[:], expectedDigest[:]) {
		return errors.New("file content does not match its inventoried SHA-256")
	}
	finalInfo := readResult.finalInfo
	if !finalInfo.Mode().IsRegular() || !os.SameFile(openedInfo, finalInfo) || finalInfo.Size() != openedInfo.Size() || !finalInfo.ModTime().Equal(openedInfo.ModTime()) {
		return errors.New("file identity changed while reading")
	}
	finalPathInfo, err := lstatPythonFilePath(root, file.Path)
	if err != nil {
		return fmt.Errorf("reinspect file path: %w", err)
	}
	if !finalPathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, finalPathInfo) {
		return errors.New("file path identity changed while reading")
	}
	return nil
}

// canonicalPythonFileDigest accepts only the canonical wire spelling of a
// SHA-256 so equivalent or malformed identities cannot enter the worker request.
func canonicalPythonFileDigest(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if len(value) != hex.EncodedLen(sha256.Size) || value != strings.ToLower(value) {
		return digest, errors.New("SHA-256 must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return digest, errors.New("SHA-256 must be 64 lowercase hexadecimal characters")
	}
	copy(digest[:], decoded)
	return digest, nil
}

// canonicalPythonFilePath converts one canonical slash-relative wire path to
// the current platform without accepting traversal, backslashes, or volumes.
func canonicalPythonFilePath(value string) (string, error) {
	if value == "" || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) || pathpkg.IsAbs(value) || pathpkg.Clean(value) != value || value == "." {
		return "", errors.New("path must be canonical, slash-separated, and repository-relative")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("path must be canonical, slash-separated, and repository-relative")
		}
	}
	native := filepath.FromSlash(value)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", errors.New("path must be canonical, slash-separated, and repository-relative")
	}
	return native, nil
}

// lstatPythonFilePath walks every already-canonical path component without
// following a symlink and returns the final component's identity.
func lstatPythonFilePath(root *os.Root, canonicalPath string) (os.FileInfo, error) {
	components := strings.Split(canonicalPath, "/")
	var info os.FileInfo
	for index := range components {
		prefix := filepath.FromSlash(strings.Join(components[:index+1], "/"))
		current, err := root.Lstat(prefix)
		if err != nil {
			return nil, fmt.Errorf("inspect path component: %w", err)
		}
		if current.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("path contains a symlink")
		}
		if index < len(components)-1 && !current.IsDir() {
			return nil, errors.New("path contains a non-directory component")
		}
		info = current
	}
	if info == nil {
		return nil, errors.New("path has no file component")
	}
	return info, nil
}

// pythonFileReadCloser is the minimum close-interruptible regular-file surface
// needed by the bounded admission reader and its blocking-I/O test double.
type pythonFileReadCloser interface {
	io.Reader
	io.Closer
	Stat() (os.FileInfo, error)
}

// pythonFileReadResult carries the digest and final identity observed by one
// complete exact-length admission read.
type pythonFileReadResult struct {
	digest    [sha256.Size]byte
	finalInfo os.FileInfo
}

// pythonFileReadSlots bounds rare kernel-backed reads that remain unwinding
// after Close so concurrent scan timeouts cannot grow process state without limit.
var pythonFileReadSlots = make(chan struct{}, 4)

// readPythonFileBounded hashes exactly size bytes, proves EOF, and returns at
// the caller's deadline even when a remote or FUSE read is slow to unwind.
func readPythonFileBounded(ctx context.Context, source pythonFileReadCloser, size int64) (pythonFileReadResult, error) {
	if err := ctx.Err(); err != nil {
		_ = source.Close()
		return pythonFileReadResult{}, err
	}
	select {
	case pythonFileReadSlots <- struct{}{}:
	case <-ctx.Done():
		_ = source.Close()
		return pythonFileReadResult{}, ctx.Err()
	}
	type outcome struct {
		result pythonFileReadResult
		err    error
	}
	completed := make(chan outcome, 1)
	go func() {
		defer func() { <-pythonFileReadSlots }()
		hash := sha256.New()
		written, err := io.CopyN(hash, source, size)
		if err != nil {
			completed <- outcome{err: fmt.Errorf("read exact inventoried file size after %d bytes: %w", written, err)}
			return
		}
		var extra [1]byte
		if count, extraErr := io.ReadFull(source, extra[:]); count != 0 {
			completed <- outcome{err: errors.New("file grew beyond its inventoried size")}
			return
		} else if !errors.Is(extraErr, io.EOF) {
			completed <- outcome{err: fmt.Errorf("prove end of inventoried file: %w", extraErr)}
			return
		}
		finalInfo, err := source.Stat()
		if err != nil {
			completed <- outcome{err: fmt.Errorf("restat opened file: %w", err)}
			return
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		completed <- outcome{result: pythonFileReadResult{digest: digest, finalInfo: finalInfo}}
	}()
	select {
	case value := <-completed:
		return value.result, value.err
	case <-ctx.Done():
		// Close is concurrency-safe for os.File and requests interruption of a
		// remote/FUSE read. The caller does not wait for a slow kernel unwind, and
		// the global slot bound prevents repeated timeouts from growing unbounded.
		_ = source.Close()
		return pythonFileReadResult{}, ctx.Err()
	}
}

type pythonRunSpec struct {
	Interpreter    string
	Script         string
	Root           string
	MemoryLimitMiB int64
	SwapLimitMiB   int64
	ProcessLimit   int
}

type pythonRunner interface {
	Run(context.Context, pythonRunSpec, []byte, io.Writer, io.Writer) error
}

type commandSpec struct {
	path        string
	arguments   []string
	environment []string
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
}

type runningCommand interface {
	Wait() error
	Signal(os.Signal) error
	Kill() error
}

type commandLauncher interface {
	Start(commandSpec) (runningCommand, error)
	Run(context.Context, commandSpec) error
}

type execCommandLauncher struct{}

func (execCommandLauncher) Start(spec commandSpec) (runningCommand, error) {
	command := exec.Command(spec.path, spec.arguments...)
	command.Env, command.Stdin, command.Stdout, command.Stderr = spec.environment, spec.stdin, spec.stdout, spec.stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &execRunningCommand{command: command}, nil
}

func (execCommandLauncher) Run(ctx context.Context, spec commandSpec) error {
	command := exec.CommandContext(ctx, spec.path, spec.arguments...)
	command.Env, command.Stdin, command.Stdout, command.Stderr = spec.environment, spec.stdin, spec.stdout, spec.stderr
	return command.Run()
}

// Wait is supplied by exec.Cmd rather than os.Process, so retain both handles.
type execRunningCommand struct {
	command *exec.Cmd
}

func (command *execRunningCommand) Wait() error { return command.command.Wait() }
func (command *execRunningCommand) Signal(signal os.Signal) error {
	return command.command.Process.Signal(signal)
}
func (command *execRunningCommand) Kill() error { return command.command.Process.Kill() }

type systemdPythonRunner struct {
	lookPath         func(string) (string, error)
	launcher         commandLauncher
	terminationGrace time.Duration
	reapTimeout      time.Duration
}

var pluginUnitCounter atomic.Uint64

func (runner systemdPythonRunner) Run(ctx context.Context, spec pythonRunSpec, payload []byte, stdout, stderr io.Writer) error {
	if runtime.GOOS != "linux" {
		return errors.New("the built-in Python adapter is disabled on this platform because its Linux isolation policy is unavailable")
	}
	lookPath := runner.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	launcher := runner.launcher
	if launcher == nil {
		launcher = execCommandLauncher{}
	}
	paths := map[string]string{}
	for _, name := range []string{"systemd-run", "systemctl", "env", spec.Interpreter} {
		path, err := lookPath(name)
		if err != nil {
			return fmt.Errorf("required Python adapter isolation command %q is unavailable: %w", name, err)
		}
		paths[name] = path
	}
	unit := fmt.Sprintf("rkc-plugin-%d-%d.service", os.Getpid(), pluginUnitCounter.Add(1))
	arguments := systemdPythonArguments(unit, paths["env"], paths[spec.Interpreter], spec)
	process, err := launcher.Start(commandSpec{path: paths["systemd-run"], arguments: arguments, environment: resourceGuardEnvironment(), stdin: bytes.NewReader(payload), stdout: stdout, stderr: stderr})
	if err != nil {
		return fmt.Errorf("start isolated Python adapter: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		termErr := runner.signalUnit(paths["systemctl"], launcher, unit, "SIGTERM")
		processTermErr := process.Signal(os.Interrupt)
		grace := runner.terminationGrace
		if grace <= 0 {
			grace = 250 * time.Millisecond
		}
		reapTimeout := runner.reapTimeout
		if reapTimeout <= 0 {
			reapTimeout = 2 * time.Second
		}
		var terminationErrors []error
		if termErr != nil {
			terminationErrors = append(terminationErrors, fmt.Errorf("signal plugin unit with SIGTERM: %w", termErr))
		}
		if processTermErr != nil && !errors.Is(processTermErr, os.ErrProcessDone) {
			terminationErrors = append(terminationErrors, fmt.Errorf("interrupt systemd-run wrapper: %w", processTermErr))
		}
		reaped := false
		select {
		case <-done:
			reaped = true
		case <-time.After(grace):
		}
		if !reaped {
			killErr := runner.signalUnit(paths["systemctl"], launcher, unit, "SIGKILL")
			if killErr != nil {
				terminationErrors = append(terminationErrors, fmt.Errorf("signal plugin unit with SIGKILL: %w", killErr))
			}
			processKillErr := process.Kill()
			if processKillErr != nil && !errors.Is(processKillErr, os.ErrProcessDone) {
				terminationErrors = append(terminationErrors, fmt.Errorf("kill systemd-run wrapper: %w", processKillErr))
			}
			select {
			case <-done:
				reaped = true
			case <-time.After(reapTimeout):
				terminationErrors = append(terminationErrors, errors.New("systemd-run wrapper did not reap after SIGKILL"))
			}
		}
		if inactiveErr := runner.waitUnitInactive(paths["systemctl"], launcher, unit, reapTimeout); inactiveErr != nil {
			terminationErrors = append(terminationErrors, inactiveErr)
		}
		if !reaped && len(terminationErrors) == 0 {
			terminationErrors = append(terminationErrors, errors.New("systemd-run wrapper termination was not proven"))
		}
		if len(terminationErrors) > 0 {
			return errors.Join(ctx.Err(), errors.Join(terminationErrors...))
		}
		return ctx.Err()
	}
}

func (runner systemdPythonRunner) signalUnit(systemctl string, launcher commandLauncher, unit, signal string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return launcher.Run(ctx, commandSpec{path: systemctl, arguments: []string{"--user", "kill", "--kill-whom=all", "--signal=" + signal, unit}, environment: resourceGuardEnvironment()})
}

func (runner systemdPythonRunner) waitUnitInactive(systemctl string, launcher commandLauncher, unit string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		var stdout bytes.Buffer
		err := launcher.Run(ctx, commandSpec{
			path: systemctl, arguments: []string{"--user", "is-active", unit},
			environment: resourceGuardEnvironment(), stdout: &stdout,
		})
		state := strings.TrimSpace(stdout.String())
		switch state {
		case "inactive", "failed", "dead", "unknown":
			return nil
		case "active", "activating", "deactivating", "reloading":
			// Keep polling until the bounded context expires.
		default:
			if err != nil {
				return fmt.Errorf("prove plugin unit inactive: %w", err)
			}
			return fmt.Errorf("prove plugin unit inactive: unexpected state %q", state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("prove plugin unit inactive: %w (last state %q)", ctx.Err(), state)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func systemdPythonArguments(unit, envPath, interpreter string, spec pythonRunSpec) []string {
	memoryMax := spec.MemoryLimitMiB * 1024 * 1024
	memoryHigh := memoryMax * 85 / 100
	swapMax := spec.SwapLimitMiB * 1024 * 1024
	return []string{
		"--user", "--wait", "--pipe", "--collect", "--quiet", "--unit", unit, "--working-directory", spec.Root,
		"--property", "Type=exec", "--property", "CPUQuota=100%", "--property", "CPUWeight=1",
		"--property", "IOWeight=1", "--property", "Nice=19", "--property", "CPUSchedulingPolicy=idle",
		"--property", "IOSchedulingClass=idle", "--property", "OOMScoreAdjust=750",
		"--property", "MemoryHigh=" + strconv.FormatInt(memoryHigh, 10), "--property", "MemoryMax=" + strconv.FormatInt(memoryMax, 10),
		"--property", "MemorySwapMax=" + strconv.FormatInt(swapMax, 10), "--property", "TasksMax=" + strconv.Itoa(spec.ProcessLimit),
		"--property", "OOMPolicy=stop", "--property", "KillMode=control-group", "--property", "TimeoutStopSec=2s",
		"--property", "NoNewPrivileges=yes", "--property", "UMask=0077", "--property", "RestrictAddressFamilies=AF_UNIX",
		// TasksMax=1 prevents fork/clone without blocking the two in-place execs
		// needed by env(1) and the interpreter. Denying @process here would also
		// deny those bootstrap execve calls and make the sandbox nonfunctional.
		"--property", "SystemCallFilter=~@network-io", "--property", "SystemCallErrorNumber=EPERM",
		"--property", "RestrictSUIDSGID=yes", "--property", "LimitNOFILE=64", "--",
		envPath, "-i", "HOME=/nonexistent", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", interpreter, "-I", "-B", spec.Script,
	}
}

func verifyBuiltinScript(path, expected string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve built-in Python adapter: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect built-in Python adapter: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > 2*1024*1024 {
		return "", errors.New("built-in Python adapter must be an owner-only regular file no larger than 2 MiB")
	}
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("built-in Python adapter requires its embedded SHA-256 digest")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read built-in Python adapter: %w", err)
	}
	digest := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), expected) {
		return "", errors.New("built-in Python adapter digest does not match the embedded worker")
	}
	return abs, nil
}

func resourceGuardEnvironment() []string {
	allowed := map[string]struct{}{"HOME": {}, "PATH": {}, "LANG": {}, "LC_ALL": {}, "XDG_RUNTIME_DIR": {}, "DBUS_SESSION_BUS_ADDRESS": {}}
	environment := make([]string, 0, len(allowed))
	for _, item := range os.Environ() {
		name, _, ok := strings.Cut(item, "=")
		if ok {
			if _, permitted := allowed[name]; permitted {
				environment = append(environment, item)
			}
		}
	}
	return environment
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func newLimitedBuffer(configured, fallback int64) *limitedBuffer {
	if configured <= 0 {
		configured = fallback
	}
	return &limitedBuffer{limit: configured}
}
func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.written += int64(len(p))
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		chunk := p
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		_, _ = b.buffer.Write(chunk)
	}
	if b.written > b.limit {
		b.truncated = true
	}
	return len(p), nil
}
func (b *limitedBuffer) Bytes() []byte   { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string  { return b.buffer.String() }
func (b *limitedBuffer) Truncated() bool { return b.truncated }
func (b *limitedBuffer) Limit() int64    { return b.limit }
