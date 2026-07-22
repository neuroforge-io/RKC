package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePythonRunner struct {
	stdout         string
	stderr         string
	err            error
	waitForContext bool
	spec           pythonRunSpec
}

type fakeRunningCommand struct {
	done        chan struct{}
	once        sync.Once
	signalStops bool
	killStops   bool
	signalErr   error
	killErr     error
}

func (command *fakeRunningCommand) Wait() error { <-command.done; return nil }
func (command *fakeRunningCommand) Signal(os.Signal) error {
	if command.signalStops {
		command.once.Do(func() { close(command.done) })
	}
	return command.signalErr
}
func (command *fakeRunningCommand) Kill() error {
	if command.killStops {
		command.once.Do(func() { close(command.done) })
	}
	return command.killErr
}

type fakeCommandLauncher struct {
	process *fakeRunningCommand
	started commandSpec
	runs    []commandSpec
}

func (launcher *fakeCommandLauncher) Start(spec commandSpec) (runningCommand, error) {
	launcher.started = spec
	return launcher.process, nil
}
func (launcher *fakeCommandLauncher) Run(_ context.Context, spec commandSpec) error {
	launcher.runs = append(launcher.runs, spec)
	if len(spec.arguments) >= 2 && spec.arguments[1] == "is-active" && spec.stdout != nil {
		_, _ = io.WriteString(spec.stdout, "inactive\n")
	}
	return nil
}

func (runner *fakePythonRunner) Run(ctx context.Context, spec pythonRunSpec, _ []byte, stdout, stderr io.Writer) error {
	runner.spec = spec
	if runner.waitForContext {
		<-ctx.Done()
		return ctx.Err()
	}
	_, _ = io.WriteString(stdout, runner.stdout)
	_, _ = io.WriteString(stderr, runner.stderr)
	return runner.err
}

func sandboxedOptions(t *testing.T, script string, runner pythonRunner) PythonOptions {
	t.Helper()
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return PythonOptions{
		Interpreter: "/fake/python3", Script: script, Timeout: time.Second,
		MaxOutputBytes: 64, MaxStderrBytes: 64, MemoryLimitMiB: 256,
		SwapLimitMiB: 32, ProcessLimit: 1, RequireSandbox: true,
		DenyNetwork: true, DenyProcessSpawn: true, Builtin: true,
		ExpectedScriptSHA256: hex.EncodeToString(digest[:]), runner: runner,
	}
}

func nonEmptyRequest(root string) Request {
	return Request{
		SchemaVersion: "1.0",
		SnapshotID:    "snapshot",
		Root:          root,
		Files:         []FileRef{{ID: "artifact", Path: "sample.py", Language: "python", SHA256: strings.Repeat("a", 64)}},
	}
}

func shellPlugin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunPythonEmptyRequestIsNoOp(t *testing.T) {
	fragment, err := RunPython(context.Background(), Request{}, PythonOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Nodes) != 0 || len(fragment.Edges) != 0 || len(fragment.Diagnostics) != 0 {
		t.Fatalf("empty request fragment = %+v", fragment)
	}
}

func TestRunPythonRequiresScriptForNonEmptyRequest(t *testing.T) {
	if _, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), PythonOptions{}); err == nil || !strings.Contains(err.Error(), "script is required") {
		t.Fatalf("RunPython(no script) = %v", err)
	}
}

func TestRunPythonSuccessAndExactLimits(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	runner := &fakePythonRunner{stdout: `{"nodes":[{"id":"node-1","kind":"function","name":"Example"}]}`, stderr: "note"}
	opts := sandboxedOptions(t, script, runner)
	opts.MaxStderrBytes = 4
	fragment, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Nodes) != 1 || fragment.Nodes[0].ID != "node-1" || fragment.Nodes[0].Name != "Example" {
		t.Fatalf("RunPython() fragment = %+v", fragment)
	}
	if runner.spec.MemoryLimitMiB != 256 || runner.spec.SwapLimitMiB != 32 || runner.spec.ProcessLimit != 1 {
		t.Fatalf("runner did not receive resource policy: %+v", runner.spec)
	}
}

func TestRunPythonRejectsUntrustedOrWeakenedPolicy(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	base := sandboxedOptions(t, script, &fakePythonRunner{stdout: "{}"})
	tests := []struct {
		name string
		edit func(*PythonOptions)
		want string
	}{
		{"external", func(options *PythonOptions) { options.Builtin = false }, "external Python plugins are disabled"},
		{"sandbox", func(options *PythonOptions) { options.RequireSandbox = false }, "requires fail-closed"},
		{"network", func(options *PythonOptions) { options.DenyNetwork = false }, "requires fail-closed"},
		{"process", func(options *PythonOptions) { options.DenyProcessSpawn = false }, "requires fail-closed"},
		{"memory", func(options *PythonOptions) { options.MemoryLimitMiB = MaximumMemoryMiB + 1 }, "memory limit"},
		{"swap", func(options *PythonOptions) { options.SwapLimitMiB = MaximumSwapMiB + 1 }, "swap limit"},
		{"tasks", func(options *PythonOptions) { options.ProcessLimit = 0 }, "task limit"},
		{"digest", func(options *PythonOptions) { options.ExpectedScriptSHA256 = strings.Repeat("0", 64) }, "digest does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := base
			test.edit(&options)
			if _, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), options); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RunPython() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSystemdPythonArgumentsEncodeEnforcedPolicy(t *testing.T) {
	arguments := systemdPythonArguments("rkc-plugin-test.service", "/usr/bin/env", "/usr/bin/python3", pythonRunSpec{
		Script: "/private/worker.py", Root: "/repository", MemoryLimitMiB: 256, SwapLimitMiB: 32, ProcessLimit: 1,
	})
	joined := strings.Join(arguments, "\x00")
	for _, required := range []string{
		"CPUQuota=100%", "CPUWeight=1", "IOWeight=1", "Nice=19", "CPUSchedulingPolicy=idle",
		"MemoryMax=268435456", "MemorySwapMax=33554432", "TasksMax=1", "KillMode=control-group",
		"UMask=0077", "RestrictAddressFamilies=AF_UNIX", "SystemCallFilter=~@network-io",
		"/usr/bin/env\x00-i\x00HOME=/nonexistent", "/usr/bin/python3\x00-I\x00-B\x00/private/worker.py",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("guard arguments missing %q: %v", required, arguments)
		}
	}
}

func TestSystemdPythonRunnerCancelsWholeUnitWithFakes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only isolation runner")
	}
	launcher := &fakeCommandLauncher{process: &fakeRunningCommand{done: make(chan struct{}), killStops: true}}
	runner := systemdPythonRunner{
		lookPath: func(name string) (string, error) { return "/fake/" + filepath.Base(name), nil },
		launcher: launcher, terminationGrace: 5 * time.Millisecond, reapTimeout: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.Run(ctx, pythonRunSpec{Interpreter: "python3", Script: "/worker.py", Root: "/repository", MemoryLimitMiB: 256, SwapLimitMiB: 32, ProcessLimit: 1}, nil, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(cancelled) = %v", err)
	}
	for _, signal := range []string{"--signal=SIGTERM", "--signal=SIGKILL"} {
		found := false
		for index, call := range launcher.runs {
			joined := strings.Join(call.arguments, " ")
			if strings.Contains(joined, signal) {
				found = true
				if !strings.Contains(joined, "--kill-whom=all") || !strings.Contains(joined, "rkc-plugin-") {
					t.Fatalf("systemctl call %d = %q", index, joined)
				}
			}
		}
		if !found {
			t.Fatalf("missing systemctl %s call: %+v", signal, launcher.runs)
		}
	}
	if inherited := launcher.started.environment; inherited == nil {
		t.Fatal("outer guard environment must be explicitly sanitized, not inherited")
	}
}

func TestSystemdPythonRunnerSurfacesUnreapedFailedKill(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only isolation runner")
	}
	process := &fakeRunningCommand{done: make(chan struct{}), killErr: errors.New("kill denied")}
	defer process.once.Do(func() { close(process.done) })
	launcher := &fakeCommandLauncher{process: process}
	runner := systemdPythonRunner{
		lookPath: func(name string) (string, error) { return "/fake/" + filepath.Base(name), nil },
		launcher: launcher, terminationGrace: time.Millisecond, reapTimeout: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runner.Run(ctx, pythonRunSpec{Interpreter: "python3", Script: "/worker.py", Root: "/repository", MemoryLimitMiB: 256, SwapLimitMiB: 32, ProcessLimit: 1}, nil, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "kill denied") || !strings.Contains(err.Error(), "did not reap") {
		t.Fatalf("Run(stuck cancellation) = %v", err)
	}
}

func TestRunPythonReportsProcessFailureAndStderr(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	_, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{stderr: "specific plugin failure", err: errors.New("exit status 7")}))
	if err == nil || !strings.Contains(err.Error(), "python plugin failed") || !strings.Contains(err.Error(), "specific plugin failure") {
		t.Fatalf("RunPython(failed process) = %v", err)
	}

	_, err = RunPython(context.Background(), nonEmptyRequest(t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{err: errors.New("missing interpreter")}))
	if err == nil || !strings.Contains(err.Error(), "python plugin failed") {
		t.Fatalf("RunPython(missing interpreter) = %v", err)
	}
}

func TestRunPythonTimeoutAndParentCancellation(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	started := time.Now()
	opts := sandboxedOptions(t, script, &fakePythonRunner{waitForContext: true})
	opts.Timeout = 25 * time.Millisecond
	_, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), opts)
	if err == nil || !strings.Contains(err.Error(), "timed out after 25ms") {
		t.Fatalf("RunPython(timeout) = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("timed-out plugin took too long: %s", time.Since(started))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = RunPython(ctx, nonEmptyRequest(t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{waitForContext: true}))
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("RunPython(cancelled parent) = %v", err)
	}
}

func TestRunPythonEnforcesOutputAndStderrLimits(t *testing.T) {
	stdoutScript := shellPlugin(t, "# inert fixture")
	opts := sandboxedOptions(t, stdoutScript, &fakePythonRunner{stdout: "12345"})
	opts.MaxOutputBytes = 4
	if _, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), opts); err == nil || !strings.Contains(err.Error(), "output exceeded 4 bytes") {
		t.Fatalf("RunPython(stdout limit) = %v", err)
	}

	stderrScript := shellPlugin(t, "# inert fixture 2")
	opts = sandboxedOptions(t, stderrScript, &fakePythonRunner{stdout: "{}", stderr: "12345"})
	opts.MaxOutputBytes, opts.MaxStderrBytes = 2, 4
	if _, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), opts); err == nil || !strings.Contains(err.Error(), "stderr exceeded 4 bytes") {
		t.Fatalf("RunPython(stderr limit) = %v", err)
	}
}

func TestRunPythonRejectsInvalidJSON(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	if _, err := RunPython(context.Background(), nonEmptyRequest(t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{stdout: "not-json", stderr: "decoder context"})); err == nil || !strings.Contains(err.Error(), "decode python plugin response") || !strings.Contains(err.Error(), "decoder context") {
		t.Fatalf("RunPython(invalid JSON) = %v", err)
	}
}

func TestLimitedBufferFallbackExactLimitAndTruncation(t *testing.T) {
	buffer := newLimitedBuffer(0, 4)
	if buffer.Limit() != 4 {
		t.Fatalf("fallback limit = %d", buffer.Limit())
	}
	written, err := buffer.Write([]byte("1234"))
	if err != nil || written != 4 || buffer.Truncated() || string(buffer.Bytes()) != "1234" || buffer.String() != "1234" {
		t.Fatalf("exact Write() = written %d err %v bytes %q truncated %v", written, err, buffer.Bytes(), buffer.Truncated())
	}
	written, err = buffer.Write([]byte("567"))
	if err != nil || written != 3 || !buffer.Truncated() || string(buffer.Bytes()) != "1234" {
		t.Fatalf("overflow Write() = written %d err %v bytes %q truncated %v", written, err, buffer.Bytes(), buffer.Truncated())
	}
	empty := newLimitedBuffer(-1, 2)
	if _, err := empty.Write(nil); err != nil || empty.Truncated() {
		t.Fatalf("Write(nil) = %v, truncated=%v", err, empty.Truncated())
	}
}

func TestRunPythonHonorsAlreadyExpiredDeadline(t *testing.T) {
	script := shellPlugin(t, `printf '{}'`)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := RunPython(ctx, nonEmptyRequest(t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{waitForContext: true}))
	if err == nil {
		t.Fatal("RunPython(expired context) succeeded")
	}
	if !strings.Contains(err.Error(), "timed out") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunPython(expired context) = %v", err)
	}
}
