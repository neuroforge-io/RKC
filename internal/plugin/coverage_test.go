package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type errorCommandLauncher struct {
	startErr error
	runErr   error
	state    string
}

func (launcher errorCommandLauncher) Start(commandSpec) (runningCommand, error) {
	return nil, launcher.startErr
}

func (launcher errorCommandLauncher) Run(_ context.Context, spec commandSpec) error {
	if spec.stdout != nil {
		_, _ = io.WriteString(spec.stdout, launcher.state)
	}
	return launcher.runErr
}

func TestExecCommandLauncherLifecycle(t *testing.T) {
	launcher := execCommandLauncher{}
	var output bytes.Buffer
	if err := launcher.Run(context.Background(), commandSpec{
		path: "/bin/sh", arguments: []string{"-c", "printf command-run"},
		environment: os.Environ(), stdout: &output, stderr: io.Discard,
	}); err != nil || output.String() != "command-run" {
		t.Fatalf("Run() output=%q error=%v", output.String(), err)
	}
	if err := launcher.Run(context.Background(), commandSpec{path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("Run(missing executable) succeeded")
	}
	if _, err := launcher.Start(commandSpec{path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("Start(missing executable) succeeded")
	}

	process, err := launcher.Start(commandSpec{path: "/bin/sh", arguments: []string{"-c", "exit 0"}, environment: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if err := process.Signal(os.Interrupt); err == nil {
		t.Fatal("Signal(completed process) unexpectedly succeeded")
	}
	if err := process.Kill(); err == nil {
		t.Fatal("Kill(completed process) unexpectedly succeeded")
	}
}

func TestSystemdRunnerSetupAndCompletionFailures(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only isolation runner")
	}
	spec := pythonRunSpec{Interpreter: "python3", Script: "/worker.py", Root: t.TempDir(), MemoryLimitMiB: 64, ProcessLimit: 1}
	runner := systemdPythonRunner{lookPath: func(name string) (string, error) {
		if name == "env" {
			return "", errors.New("not installed")
		}
		return "/fake/" + name, nil
	}}
	if err := runner.Run(context.Background(), spec, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), `"env" is unavailable`) {
		t.Fatalf("Run(missing guard tool) = %v", err)
	}

	runner = systemdPythonRunner{
		lookPath: func(name string) (string, error) { return "/fake/" + name, nil },
		launcher: errorCommandLauncher{startErr: errors.New("start refused")},
	}
	if err := runner.Run(context.Background(), spec, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "start refused") {
		t.Fatalf("Run(start failure) = %v", err)
	}

	done := make(chan struct{})
	close(done)
	launcher := &fakeCommandLauncher{process: &fakeRunningCommand{done: done}}
	runner = systemdPythonRunner{
		lookPath: func(name string) (string, error) { return "/fake/" + name, nil },
		launcher: launcher,
	}
	if err := runner.Run(context.Background(), spec, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run(completed wrapper) = %v", err)
	}
}

func TestWaitUnitInactiveRejectsUnknownAndTimesOutActive(t *testing.T) {
	runner := systemdPythonRunner{}
	if err := runner.waitUnitInactive("/fake/systemctl", errorCommandLauncher{state: "mystery\n"}, "unit", 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "unexpected state") {
		t.Fatalf("waitUnitInactive(unknown) = %v", err)
	}
	if err := runner.waitUnitInactive("/fake/systemctl", errorCommandLauncher{state: "", runErr: errors.New("status failed")}, "unit", 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "status failed") {
		t.Fatalf("waitUnitInactive(command error) = %v", err)
	}
	if err := runner.waitUnitInactive("/fake/systemctl", errorCommandLauncher{state: "active\n"}, "unit", time.Millisecond); err == nil || !strings.Contains(err.Error(), "last state \"active\"") {
		t.Fatalf("waitUnitInactive(active timeout) = %v", err)
	}
}

func TestVerifyBuiltinScriptShapeAndDigestFailures(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "worker.py")
	if err := os.WriteFile(file, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBuiltinScript(file, "not-a-digest"); err == nil || !strings.Contains(err.Error(), "embedded SHA-256") {
		t.Fatalf("verifyBuiltinScript(invalid digest) = %v", err)
	}
	if _, err := verifyBuiltinScript(filepath.Join(root, "missing"), strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("verifyBuiltinScript(missing) = %v", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBuiltinScript(file, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("verifyBuiltinScript(insecure mode) = %v", err)
	}
	alias := filepath.Join(root, "worker-link")
	if err := os.Symlink(file, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBuiltinScript(alias, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("verifyBuiltinScript(symlink) = %v", err)
	}
}
