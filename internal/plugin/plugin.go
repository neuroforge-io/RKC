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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/neuroforge-io/RKC/internal/model"
)

const (
	MaximumMemoryMiB = int64(2048)
	MaximumSwapMiB   = int64(512)
	MaximumProcesses = 1
)

type FileRef struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Language string `json:"language"`
	SHA256   string `json:"sha256"`
}

type Request struct {
	SchemaVersion string    `json:"schema_version"`
	SnapshotID    string    `json:"snapshot_id"`
	Root          string    `json:"root"`
	Files         []FileRef `json:"files"`
}

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
	payload, err := json.Marshal(request)
	if err != nil {
		return model.Fragment{}, fmt.Errorf("encode plugin request: %w", err)
	}

	pluginCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	stdout := newLimitedBuffer(opts.MaxOutputBytes, 64*1024*1024)
	stderr := newLimitedBuffer(opts.MaxStderrBytes, 2*1024*1024)
	runner := opts.runner
	if runner == nil {
		runner = systemdPythonRunner{}
	}
	spec := pythonRunSpec{Interpreter: opts.Interpreter, Script: absScript, Root: request.Root, MemoryLimitMiB: opts.MemoryLimitMiB, SwapLimitMiB: opts.SwapLimitMiB, ProcessLimit: opts.ProcessLimit}
	if err := runner.Run(pluginCtx, spec, payload, stdout, stderr); err != nil {
		if pluginCtx.Err() != nil {
			if errors.Is(pluginCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return model.Fragment{}, fmt.Errorf("python plugin timed out after %s", opts.Timeout)
			}
			return model.Fragment{}, fmt.Errorf("python plugin cancelled: %w", pluginCtx.Err())
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
