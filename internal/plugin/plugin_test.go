package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	payload        []byte
	calls          int
}

type fakeRunningCommand struct {
	done        chan struct{}
	once        sync.Once
	signalStops bool
	killStops   bool
	signalErr   error
	killErr     error
}

type blockingPythonFile struct {
	closed chan struct{}
	once   sync.Once
}

func (file *blockingPythonFile) Read([]byte) (int, error) {
	<-file.closed
	return 0, os.ErrClosed
}

func (file *blockingPythonFile) Close() error {
	file.once.Do(func() { close(file.closed) })
	return nil
}

func (file *blockingPythonFile) Stat() (os.FileInfo, error) {
	return nil, errors.New("unexpected stat")
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

func (runner *fakePythonRunner) Run(ctx context.Context, spec pythonRunSpec, payload []byte, stdout, stderr io.Writer) error {
	runner.calls++
	runner.spec = spec
	runner.payload = append([]byte(nil), payload...)
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

func nonEmptyRequest(t *testing.T, root string) Request {
	t.Helper()
	data := []byte("def sample():\n    return True\n")
	if err := os.WriteFile(filepath.Join(root, "sample.py"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return Request{
		SchemaVersion: "1.0",
		SnapshotID:    "snapshot",
		Root:          root,
		Files: []FileRef{{
			ID: "artifact", Path: "sample.py", Language: "python",
			SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(data)),
		}},
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
	if _, err := RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), PythonOptions{}); err == nil || !strings.Contains(err.Error(), "script is required") {
		t.Fatalf("RunPython(no script) = %v", err)
	}
}

func TestRunPythonSuccessAndExactLimits(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	runner := &fakePythonRunner{stdout: `{"nodes":[{"id":"node-1","kind":"function","name":"Example"}]}`, stderr: "note"}
	opts := sandboxedOptions(t, script, runner)
	opts.MaxStderrBytes = 4
	request := nonEmptyRequest(t, t.TempDir())
	fragment, err := RunPython(context.Background(), request, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragment.Nodes) != 1 || fragment.Nodes[0].ID != "node-1" || fragment.Nodes[0].Name != "Example" {
		t.Fatalf("RunPython() fragment = %+v", fragment)
	}
	if runner.spec.MemoryLimitMiB != 256 || runner.spec.SwapLimitMiB != 32 || runner.spec.ProcessLimit != 1 {
		t.Fatalf("runner did not receive resource policy: %+v", runner.spec)
	}
	if runner.calls != 1 || !filepath.IsAbs(runner.spec.Root) {
		t.Fatalf("runner admission = calls %d root %q", runner.calls, runner.spec.Root)
	}
	var payload Request
	if err := json.Unmarshal(runner.payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 1 || payload.Files[0].SizeBytes != request.Files[0].SizeBytes || payload.Files[0].SHA256 != request.Files[0].SHA256 {
		t.Fatalf("runner payload lost file identity: %+v", payload.Files)
	}
}

func TestRunPythonRejectsUnsafeFileRefsBeforeRunner(t *testing.T) {
	tests := []struct {
		name string
		edit func(*testing.T, string, *Request)
		want string
	}{
		{name: "backslash", edit: func(_ *testing.T, _ string, request *Request) {
			request.Files[0].Path = "..\\outside.py"
		}, want: "canonical, slash-separated"},
		{name: "traversal", edit: func(_ *testing.T, _ string, request *Request) {
			request.Files[0].Path = "../outside.py"
		}, want: "canonical, slash-separated"},
		{name: "noncanonical", edit: func(_ *testing.T, _ string, request *Request) {
			request.Files[0].Path = "pkg/../sample.py"
		}, want: "canonical, slash-separated"},
		{name: "absolute", edit: func(_ *testing.T, root string, request *Request) {
			request.Files[0].Path = filepath.Join(root, "sample.py")
		}, want: "canonical, slash-separated"},
		{name: "root symlink", edit: func(t *testing.T, root string, request *Request) {
			link := filepath.Join(filepath.Dir(root), "root-link")
			if err := os.Symlink(root, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			request.Root = link
		}, want: "root must be a real directory"},
		{name: "negative size", edit: func(_ *testing.T, _ string, request *Request) {
			request.Files[0].SizeBytes = -1
		}, want: "size must be nonnegative"},
		{name: "maximum size does not wrap", edit: func(_ *testing.T, _ string, request *Request) {
			request.Files[0].SizeBytes = int64(^uint64(0) >> 1)
		}, want: "size does not match inventory"},
		{name: "noncanonical digest", edit: func(_ *testing.T, _ string, request *Request) {
			request.Files[0].SHA256 = strings.Repeat("A", 64)
		}, want: "64 lowercase hexadecimal"},
		{name: "changed content", edit: func(t *testing.T, root string, _ *Request) {
			data, err := os.ReadFile(filepath.Join(root, "sample.py"))
			if err != nil {
				t.Fatal(err)
			}
			for index := range data {
				data[index] = 'x'
			}
			if err := os.WriteFile(filepath.Join(root, "sample.py"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "content does not match"},
		{name: "directory", edit: func(t *testing.T, root string, request *Request) {
			if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
				t.Fatal(err)
			}
			request.Files[0].Path = "directory"
		}, want: "not a regular file"},
		{name: "missing", edit: func(_ *testing.T, _ string, request *Request) {
			request.Files[0].Path = "missing.py"
		}, want: "inspect path component"},
		{name: "final symlink", edit: func(t *testing.T, root string, _ *Request) {
			if err := os.Rename(filepath.Join(root, "sample.py"), filepath.Join(root, "target.py")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("target.py", filepath.Join(root, "sample.py")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, want: "contains a symlink"},
		{name: "intermediate symlink", edit: func(t *testing.T, root string, request *Request) {
			realDirectory := filepath.Join(root, "real")
			if err := os.Mkdir(realDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "sample.py"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(realDirectory, "sample.py"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real", filepath.Join(root, "linked")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			request.Files[0].Path = "linked/sample.py"
		}, want: "contains a symlink"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			request := nonEmptyRequest(t, root)
			test.edit(t, root, &request)
			runner := &fakePythonRunner{stdout: "{}"}
			options := sandboxedOptions(t, shellPlugin(t, "# inert fixture"), runner)
			_, err := RunPython(context.Background(), request, options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RunPython() error = %v, want %q", err, test.want)
			}
			if runner.calls != 0 {
				t.Fatalf("unsafe file reference started runner %d time(s)", runner.calls)
			}
		})
	}
}

func TestRunPythonAcceptsExactEmptyFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "empty.py")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nil)
	request := Request{Root: root, Files: []FileRef{{
		ID: "empty", Path: "empty.py", Language: "python",
		SHA256: hex.EncodeToString(digest[:]), SizeBytes: 0,
	}}}
	runner := &fakePythonRunner{stdout: "{}"}
	if _, err := RunPython(context.Background(), request, sandboxedOptions(t, shellPlugin(t, "# inert fixture"), runner)); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("empty file runner calls = %d", runner.calls)
	}
}

func TestPythonFileReadDeadlineClosesBlockingSource(t *testing.T) {
	source := &blockingPythonFile{closed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := readPythonFileBounded(ctx, source, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readPythonFileBounded() error = %v", err)
	}
	select {
	case <-source.closed:
	default:
		t.Fatal("blocking source was not closed at its deadline")
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
			if _, err := RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), options); err == nil || !strings.Contains(err.Error(), test.want) {
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
	_, err := RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{stderr: "specific plugin failure", err: errors.New("exit status 7")}))
	if err == nil || !strings.Contains(err.Error(), "python plugin failed") || !strings.Contains(err.Error(), "specific plugin failure") {
		t.Fatalf("RunPython(failed process) = %v", err)
	}

	_, err = RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{err: errors.New("missing interpreter")}))
	if err == nil || !strings.Contains(err.Error(), "python plugin failed") {
		t.Fatalf("RunPython(missing interpreter) = %v", err)
	}
}

func TestRunPythonTimeoutAndParentCancellation(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	started := time.Now()
	opts := sandboxedOptions(t, script, &fakePythonRunner{waitForContext: true})
	opts.Timeout = 25 * time.Millisecond
	_, err := RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), opts)
	if err == nil || !strings.Contains(err.Error(), "timed out after 25ms") {
		t.Fatalf("RunPython(timeout) = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("timed-out plugin took too long: %s", time.Since(started))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = RunPython(ctx, nonEmptyRequest(t, t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{waitForContext: true}))
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("RunPython(cancelled parent) = %v", err)
	}
}

func TestRunPythonEnforcesOutputAndStderrLimits(t *testing.T) {
	stdoutScript := shellPlugin(t, "# inert fixture")
	opts := sandboxedOptions(t, stdoutScript, &fakePythonRunner{stdout: "12345"})
	opts.MaxOutputBytes = 4
	if _, err := RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), opts); err == nil || !strings.Contains(err.Error(), "output exceeded 4 bytes") {
		t.Fatalf("RunPython(stdout limit) = %v", err)
	}

	stderrScript := shellPlugin(t, "# inert fixture 2")
	opts = sandboxedOptions(t, stderrScript, &fakePythonRunner{stdout: "{}", stderr: "12345"})
	opts.MaxOutputBytes, opts.MaxStderrBytes = 2, 4
	if _, err := RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), opts); err == nil || !strings.Contains(err.Error(), "stderr exceeded 4 bytes") {
		t.Fatalf("RunPython(stderr limit) = %v", err)
	}
}

func TestRunPythonRejectsInvalidJSON(t *testing.T) {
	script := shellPlugin(t, "# inert fixture")
	if _, err := RunPython(context.Background(), nonEmptyRequest(t, t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{stdout: "not-json", stderr: "decoder context"})); err == nil || !strings.Contains(err.Error(), "decode python plugin response") || !strings.Contains(err.Error(), "decoder context") {
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
	_, err := RunPython(ctx, nonEmptyRequest(t, t.TempDir()), sandboxedOptions(t, script, &fakePythonRunner{waitForContext: true}))
	if err == nil {
		t.Fatal("RunPython(expired context) succeeded")
	}
	if !strings.Contains(err.Error(), "timed out") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunPython(expired context) = %v", err)
	}
}
