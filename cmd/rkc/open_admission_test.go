package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

type guardedOpenRunnerFunc func(context.Context, io.Writer, io.Writer) (int64, error)

func (run guardedOpenRunnerFunc) Run(ctx context.Context, stdout, stderr io.Writer) (int64, error) {
	return run(ctx, stdout, stderr)
}

func TestOpenAdmissionSelectsExactlyOneExecutionPath(t *testing.T) {
	sentinel := errors.New("outside envelope")
	tests := []struct {
		name         string
		args         []string
		platform     string
		guardedChild bool
		requireError error
		wantLocal    int
		wantLaunch   int
		wantError    string
	}{
		{name: "help stays local", args: []string{"--help"}, platform: "linux", requireError: sentinel, wantLocal: 1},
		{name: "portable platform stays local", platform: "darwin", requireError: sentinel, wantLocal: 1},
		{name: "admitted Linux parent still launches monitor", platform: "linux", wantLaunch: 1},
		{name: "unadmitted Linux launches guard", platform: "linux", requireError: sentinel, wantLaunch: 1},
		{name: "admitted protected child stays local", platform: "linux", guardedChild: true, wantLocal: 1},
		{name: "child cannot recurse", platform: "linux", guardedChild: true, requireError: sentinel, wantError: "protected open child"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			localCalls := 0
			launchCalls := 0
			args := append([]string(nil), test.args...)
			err := runOpenWithAdmissionUsing(
				context.Background(),
				args,
				test.platform,
				test.guardedChild,
				func() error { return test.requireError },
				func(_ context.Context, observed []string) error {
					launchCalls++
					if !reflect.DeepEqual(observed, args) {
						t.Fatalf("launched args = %#v, want %#v", observed, args)
					}
					return nil
				},
				func(_ context.Context, observed []string) error {
					localCalls++
					if !reflect.DeepEqual(observed, args) {
						t.Fatalf("local args = %#v, want %#v", observed, args)
					}
					return nil
				},
			)
			if test.wantError == "" && err != nil {
				t.Fatalf("admission = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError) || !errors.Is(err, sentinel)) {
				t.Fatalf("admission error = %v, want %q wrapping sentinel", err, test.wantError)
			}
			if localCalls != test.wantLocal || launchCalls != test.wantLaunch {
				t.Fatalf("execution calls = local:%d launch:%d, want local:%d launch:%d", localCalls, launchCalls, test.wantLocal, test.wantLaunch)
			}
		})
	}
}

func TestOpenAdmissionRejectsMissingDependencies(t *testing.T) {
	local := func(context.Context, []string) error { return nil }
	launch := func(context.Context, []string) error { return nil }
	require := func() error { return nil }
	for name, call := range map[string]func() error{
		"nil context": func() error {
			return runOpenWithAdmissionUsing(nil, nil, "linux", false, require, launch, local)
		},
		"nil require": func() error {
			return runOpenWithAdmissionUsing(context.Background(), nil, "linux", false, nil, launch, local)
		},
		"nil launch": func() error {
			return runOpenWithAdmissionUsing(context.Background(), nil, "linux", false, require, nil, local)
		},
		"nil local": func() error {
			return runOpenWithAdmissionUsing(context.Background(), nil, "linux", false, require, launch, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || err.Error() != "open resource admission is not configured" {
				t.Fatalf("configuration error = %v", err)
			}
		})
	}
}

func TestLaunchGuardedOpenUsesExactGuardAndCleansWatcherAndTemporaryReadiness(t *testing.T) {
	root := t.TempDir()
	temporaryDirectory := filepath.Join(root, "rkc-open-ready-test")
	watcherStarted := make(chan struct{})
	watcherStopped := make(chan struct{})
	watchedPath := make(chan string, 1)
	var capturedConfig resourceguard.Config
	var removedPath string
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	dependencies := guardedOpenLaunchDependencies{
		executable: func() (string, error) { return "/opt/rkc", nil },
		makeTempDirectory: func(parent, pattern string) (string, error) {
			if parent != "" || pattern != "rkc-open-ready-" {
				return "", fmt.Errorf("unexpected temp request %q %q", parent, pattern)
			}
			if err := os.Mkdir(temporaryDirectory, 0o700); err != nil {
				return "", err
			}
			return temporaryDirectory, nil
		},
		removeAll: func(path string) error {
			removedPath = path
			select {
			case <-watcherStopped:
			default:
				return errors.New("temporary readiness removed before watcher stopped")
			}
			return os.RemoveAll(path)
		},
		absolutePath: filepath.Abs,
		newCommand: func(_ context.Context, config resourceguard.Config) (guardedOpenRunner, error) {
			capturedConfig = config
			return guardedOpenRunnerFunc(func(_ context.Context, observedStdout, observedStderr io.Writer) (int64, error) {
				if observedStdout != &stdout || observedStderr != &stderr {
					return 0, errors.New("guarded command received unexpected output writers")
				}
				select {
				case <-watcherStarted:
					return 123, nil
				case <-time.After(time.Second):
					return 0, errors.New("readiness watcher did not start")
				}
			}), nil
		},
		waitAndLaunch: func(ctx context.Context, path string) error {
			watchedPath <- path
			close(watcherStarted)
			<-ctx.Done()
			close(watcherStopped)
			return ctx.Err()
		},
		stdout: &stdout,
		stderr: &stderr,
	}
	args := []string{"--workbench", "."}
	if err := launchGuardedOpenUsing(context.Background(), args, dependencies); err != nil {
		t.Fatalf("protected open launch = %v", err)
	}

	expectedReady := filepath.Join(temporaryDirectory, "ready.json")
	if path := <-watchedPath; path != expectedReady {
		t.Fatalf("watched readiness path = %q, want %q", path, expectedReady)
	}
	if removedPath != temporaryDirectory {
		t.Fatalf("removed readiness directory = %q, want %q", removedPath, temporaryDirectory)
	}
	if _, err := os.Stat(temporaryDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary readiness directory remains after launch: %v", err)
	}
	if capturedConfig.Executable != "/opt/rkc" || capturedConfig.MaximumRSSBytes != resourceguard.LowPriorityMemoryMaxBytes ||
		capturedConfig.UnitPrefix != "rkc-low" {
		t.Fatalf("guard configuration = %+v", capturedConfig)
	}
	wantArguments := append([]string{"open"}, args...)
	if !reflect.DeepEqual(capturedConfig.Arguments, wantArguments) {
		t.Fatalf("guard arguments = %#v, want %#v", capturedConfig.Arguments, wantArguments)
	}
	environment := strings.Join(capturedConfig.Environment, "\n")
	if !strings.Contains(environment, guardedOpenChildEnvironment+"=1") ||
		!strings.Contains(environment, guardedOpenReadyFileEnvironment+"="+expectedReady) {
		t.Fatalf("guard environment omits protected launch state: %s", environment)
	}
	if stderr.Len() != 0 {
		t.Fatalf("protected launch stderr = %q", stderr.String())
	}
}

func TestLaunchGuardedOpenReportsConfigurationAndAdmissionFailures(t *testing.T) {
	if err := launchGuardedOpen(nil, nil); err == nil || err.Error() != "protected open launch dependencies are not configured" {
		t.Fatalf("default launch nil context = %v", err)
	}
	if err := launchGuardedOpenUsing(context.Background(), nil, guardedOpenLaunchDependencies{}); err == nil ||
		err.Error() != "protected open launch dependencies are not configured" {
		t.Fatalf("missing launch dependencies = %v", err)
	}

	sentinel := errors.New("priority admission denied")
	dependencies := guardedOpenTestDependencies(guardedOpenRunnerFunc(func(context.Context, io.Writer, io.Writer) (int64, error) {
		return 0, sentinel
	}))
	err := launchGuardedOpenUsing(context.Background(), []string{"--no-browser", "."}, dependencies)
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "protected open") {
		t.Fatalf("admission failure = %v", err)
	}

	dependencies = guardedOpenTestDependencies(nil)
	dependencies.newCommand = func(context.Context, resourceguard.Config) (guardedOpenRunner, error) {
		return nil, sentinel
	}
	err = launchGuardedOpenUsing(context.Background(), []string{"--no-browser", "."}, dependencies)
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "configure protected open") {
		t.Fatalf("guard configuration failure = %v", err)
	}

	dependencies = guardedOpenTestDependencies(nil)
	err = launchGuardedOpenUsing(context.Background(), []string{"--no-browser", "."}, dependencies)
	if err == nil || !strings.Contains(err.Error(), "guarded command is not configured") {
		t.Fatalf("nil guarded command = %v", err)
	}
}

func TestDefaultGuardedOpenDependenciesWireResourceGuardWithoutStartingIt(t *testing.T) {
	dependencies := defaultGuardedOpenLaunchDependencies()
	if dependencies.executable == nil || dependencies.makeTempDirectory == nil || dependencies.removeAll == nil ||
		dependencies.absolutePath == nil || dependencies.waitAndLaunch == nil || dependencies.stdout == nil ||
		dependencies.stderr == nil || dependencies.newCommand == nil {
		t.Fatal("default protected launch dependencies are incomplete")
	}
	if _, err := dependencies.newCommand(context.Background(), resourceguard.Config{}); err == nil ||
		!strings.Contains(err.Error(), "guarded executable is required") {
		t.Fatalf("default resource guard constructor = %v", err)
	}
}

func TestLaunchGuardedOpenFailsClosedBeforeGuardConstruction(t *testing.T) {
	type failureCase struct {
		name      string
		args      []string
		configure func(*guardedOpenLaunchDependencies)
		want      string
		sentinel  error
	}
	executableFailure := errors.New("executable lookup failed")
	temporaryFailure := errors.New("temporary directory failed")
	absoluteFailure := errors.New("absolute path failed")
	existingReady := filepath.Join(t.TempDir(), "ready.json")
	if err := os.WriteFile(existingReady, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []failureCase{
		{
			name:     "executable lookup",
			args:     []string{"--no-browser", "."},
			want:     "resolve RKC executable",
			sentinel: executableFailure,
			configure: func(dependencies *guardedOpenLaunchDependencies) {
				dependencies.executable = func() (string, error) { return "", executableFailure }
			},
		},
		{
			name:     "temporary readiness creation",
			args:     []string{"."},
			want:     "create protected open readiness directory",
			sentinel: temporaryFailure,
			configure: func(dependencies *guardedOpenLaunchDependencies) {
				dependencies.makeTempDirectory = func(string, string) (string, error) { return "", temporaryFailure }
			},
		},
		{
			name:     "readiness path resolution",
			args:     []string{"--no-browser", "--ready-file", "ready.json", "."},
			want:     "resolve protected open readiness file",
			sentinel: absoluteFailure,
			configure: func(dependencies *guardedOpenLaunchDependencies) {
				dependencies.absolutePath = func(string) (string, error) { return "", absoluteFailure }
			},
		},
		{
			name:      "existing readiness receipt",
			args:      []string{"--no-browser", "--ready-file", existingReady, "."},
			want:      "protected open readiness file already exists",
			configure: func(*guardedOpenLaunchDependencies) {},
		},
	}
	for index := range tests {
		test := &tests[index]
		t.Run(test.name, func(t *testing.T) {
			dependencies := guardedOpenTestDependencies(nil)
			commandCalls := 0
			dependencies.newCommand = func(context.Context, resourceguard.Config) (guardedOpenRunner, error) {
				commandCalls++
				return nil, errors.New("guard construction must not run")
			}
			test.configure(&dependencies)
			err := launchGuardedOpenUsing(context.Background(), test.args, dependencies)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("pre-guard failure = %v, want %q", err, test.want)
			}
			if test.sentinel != nil && !errors.Is(err, test.sentinel) {
				t.Fatalf("pre-guard failure = %v, want sentinel %v", err, test.sentinel)
			}
			if commandCalls != 0 {
				t.Fatalf("guard constructed %d times after preflight failure", commandCalls)
			}
		})
	}
}

func TestLaunchGuardedOpenMapsCancellationAfterGuardCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dependencies := guardedOpenTestDependencies(guardedOpenRunnerFunc(func(context.Context, io.Writer, io.Writer) (int64, error) {
		cancel()
		return 0, context.Canceled
	}))
	if err := launchGuardedOpenUsing(ctx, []string{"--no-browser", "."}, dependencies); err != nil {
		t.Fatalf("clean protected launch cancellation = %v", err)
	}
}

func TestLaunchGuardedOpenContainsBrowserFailureAndPropagatesCleanupFailure(t *testing.T) {
	root := t.TempDir()
	readyDirectory := filepath.Join(root, "private-ready")
	browserFailure := errors.New("desktop opener failed")
	cleanupFailure := errors.New("private readiness cleanup failed")
	watcherCalled := make(chan struct{})
	var stderr bytes.Buffer
	dependencies := guardedOpenTestDependencies(guardedOpenRunnerFunc(func(context.Context, io.Writer, io.Writer) (int64, error) {
		select {
		case <-watcherCalled:
			return 0, nil
		case <-time.After(time.Second):
			return 0, errors.New("browser watcher did not run")
		}
	}))
	dependencies.makeTempDirectory = func(string, string) (string, error) {
		if err := os.Mkdir(readyDirectory, 0o700); err != nil {
			return "", err
		}
		return readyDirectory, nil
	}
	dependencies.removeAll = func(path string) error {
		if path != readyDirectory {
			return fmt.Errorf("unexpected cleanup path %q", path)
		}
		return cleanupFailure
	}
	dependencies.waitAndLaunch = func(context.Context, string) error {
		close(watcherCalled)
		return browserFailure
	}
	dependencies.stderr = &stderr

	err := launchGuardedOpenUsing(context.Background(), []string{"."}, dependencies)
	if err == nil || !errors.Is(err, cleanupFailure) || errors.Is(err, browserFailure) {
		t.Fatalf("protected launch cleanup result = %v", err)
	}
	if !strings.Contains(stderr.String(), browserFailure.Error()) {
		t.Fatalf("browser failure was not reported on injected stderr: %q", stderr.String())
	}
}

func guardedOpenTestDependencies(command guardedOpenRunner) guardedOpenLaunchDependencies {
	return guardedOpenLaunchDependencies{
		executable: func() (string, error) { return "/opt/rkc", nil },
		makeTempDirectory: func(string, string) (string, error) {
			return "", errors.New("unexpected temporary readiness request")
		},
		removeAll:    func(string) error { return nil },
		absolutePath: filepath.Abs,
		newCommand: func(context.Context, resourceguard.Config) (guardedOpenRunner, error) {
			return command, nil
		},
		waitAndLaunch: func(context.Context, string) error {
			return errors.New("unexpected browser readiness watcher")
		},
		stdout: io.Discard,
		stderr: io.Discard,
	}
}

func TestGuardedOpenEnvironmentIsMinimalDeterministicAndSubordinate(t *testing.T) {
	t.Setenv("RKC_ENV_SECRET_SENTINEL", "must-not-pass")
	t.Setenv("DISPLAY", ":42")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	first := guardedOpenEnvironment("/tmp/rkc-ready.json")
	second := guardedOpenEnvironment("/tmp/rkc-ready.json")
	if !reflect.DeepEqual(first, second) || !sort.StringsAreSorted(first) {
		t.Fatalf("guarded environment is not deterministic: %#v / %#v", first, second)
	}
	joined := strings.Join(first, "\n")
	for _, required := range []string{
		guardedOpenChildEnvironment + "=1",
		"GOMAXPROCS=1",
		"OMP_NUM_THREADS=1",
		"CUDA_VISIBLE_DEVICES=-1",
		"XDG_CACHE_HOME=" + os.Getenv("XDG_CACHE_HOME"),
		guardedOpenReadyFileEnvironment + "=/tmp/rkc-ready.json",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("guarded environment omits %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "RKC_ENV_SECRET_SENTINEL") || strings.Contains(joined, "must-not-pass") ||
		strings.Contains(joined, "DISPLAY=:42") {
		t.Fatalf("guarded environment retained unapproved caller state: %s", joined)
	}
	for _, entry := range first {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, '\x00') {
			t.Fatalf("invalid guarded environment entry: %q", entry)
		}
	}
	if os.Getenv("RKC_ENV_SECRET_SENTINEL") == "" {
		t.Fatal("test sentinel unexpectedly absent from caller environment")
	}
}

func TestOpenHelpRequest(t *testing.T) {
	if !openHelpRequest([]string{"--help"}) || !openHelpRequest([]string{"--clean", "-h"}) ||
		openHelpRequest([]string{"--out", "--help", "."}) ||
		openHelpRequest([]string{"--", "--help"}) ||
		openHelpRequest([]string{"--unknown", "--help"}) {
		t.Fatal("open help classification is incorrect")
	}
}

func TestOpenExecutionModeFailsBeforeInteractiveWorkWithoutEnvelope(t *testing.T) {
	sentinel := errors.New("envelope sentinel")
	if err := validateOpenExecutionMode(false, "windows", nil); err != nil {
		t.Fatalf("portable static mode = %v", err)
	}
	if err := validateOpenExecutionMode(true, "darwin", func() error { return nil }); err == nil || !strings.Contains(err.Error(), "--workbench") {
		t.Fatalf("portable interactive mode = %v", err)
	}
	if err := validateOpenExecutionMode(true, "linux", nil); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing Linux admission = %v", err)
	}
	if err := validateOpenExecutionMode(true, "linux", func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("rejected Linux admission = %v", err)
	}
	if err := validateOpenExecutionMode(true, "linux", func() error { return nil }); err != nil {
		t.Fatalf("admitted Linux interactive mode = %v", err)
	}
}

func TestOpenRejectsNestedPythonUnitBeforeScanning(t *testing.T) {
	err := runOpenContext(context.Background(), []string{"--python", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "aggregate resource ceiling") {
		t.Fatalf("protected Python open = %v", err)
	}
}

func TestOpenRejectsHeadlessWorkbenchWithoutPrivateReadinessBeforeScanning(t *testing.T) {
	err := runOpenContext(context.Background(), []string{
		"--workbench", "--no-browser", filepath.Join(t.TempDir(), "missing-repository"),
	})
	if err == nil || !strings.Contains(err.Error(), "owner-private --ready-file") {
		t.Fatalf("headless workbench without readiness = %v", err)
	}
}

func TestOpenRejectsStableWorkbenchOriginBeforeScanning(t *testing.T) {
	err := runOpenContext(context.Background(), []string{
		"--workbench", "--addr", "127.0.0.1:8787", filepath.Join(t.TempDir(), "missing-repository"),
	})
	if err == nil || !strings.Contains(err.Error(), "ephemeral port 0") {
		t.Fatalf("stable workbench origin = %v", err)
	}
}

func TestOpenServeArgumentsDefaultReadOnlyAndKeepWorkbenchExplicit(t *testing.T) {
	interactive := openServeArguments("127.0.0.1:0", "/tmp/ready", "/tmp/atlas", "/tmp/repository", true, true)
	wantInteractive := []string{
		"--addr", "127.0.0.1:0", "--ready-file", "/tmp/ready", "--dir", "/tmp/atlas",
		"--workbench", "--workspace", "/tmp/repository", "--open",
	}
	if !reflect.DeepEqual(interactive, wantInteractive) {
		t.Fatalf("interactive serve args = %#v, want %#v", interactive, wantInteractive)
	}
	readOnly := openServeArguments("127.0.0.1:8787", "", "/tmp/atlas", "/tmp/repository", false, false)
	wantReadOnly := []string{"--addr", "127.0.0.1:8787", "--dir", "/tmp/atlas"}
	if !reflect.DeepEqual(readOnly, wantReadOnly) {
		t.Fatalf("read-only serve args = %#v, want %#v", readOnly, wantReadOnly)
	}
}

func TestOpenWorkbenchDefaultsToFreshEphemeralOrigin(t *testing.T) {
	fs, options := newOpenFlagSet(io.Discard)
	if err := fs.Parse([]string{"--workbench", "."}); err != nil {
		t.Fatal(err)
	}
	finalizeOpenOptions(fs, options)
	if options.address != "127.0.0.1:0" {
		t.Fatalf("default workbench address = %q", options.address)
	}

	fs, options = newOpenFlagSet(io.Discard)
	if err := fs.Parse([]string{"--workbench", "--addr", "127.0.0.1:0", "."}); err != nil {
		t.Fatal(err)
	}
	finalizeOpenOptions(fs, options)
	if options.address != "127.0.0.1:0" {
		t.Fatalf("explicit ephemeral workbench address = %q", options.address)
	}

	fs, options = newOpenFlagSet(io.Discard)
	if err := fs.Parse([]string{"."}); err != nil {
		t.Fatal(err)
	}
	finalizeOpenOptions(fs, options)
	if options.address != "127.0.0.1:0" {
		t.Fatalf("static open address = %q", options.address)
	}
}

func TestReadOpenReadyReceiptRequiresPrivateCanonicalLoopbackFile(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "ready.json")
	data := `{"schema_version":"1.0","address":"127.0.0.1:8787","url":"http://127.0.0.1:8787","snapshot_id":"snapshot"}`
	if err := os.WriteFile(valid, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := readOpenReadyReceipt(valid)
	if err != nil || receipt.URL != "http://127.0.0.1:8787" || receipt.BrowserURL != receipt.URL {
		t.Fatalf("valid readiness receipt = %+v, %v", receipt, err)
	}
	capability := filepath.Join(root, "capability.json")
	capabilityURL := "http://127.0.0.1:8787#rkc-workbench=" + strings.Repeat("A", 43)
	capabilityData := `{"schema_version":"1.0","address":"127.0.0.1:8787","url":"http://127.0.0.1:8787","browser_url":"` + capabilityURL + `","snapshot_id":"snapshot"}`
	if err := os.WriteFile(capability, []byte(capabilityData), 0o600); err != nil {
		t.Fatal(err)
	}
	if receipt, err := readOpenReadyReceipt(capability); err != nil || receipt.BrowserURL != capabilityURL {
		t.Fatalf("capability readiness receipt = %+v, %v", receipt, err)
	}
	for name, body := range map[string]string{
		"remote":  `{"schema_version":"1.0","address":"example.test:8787","url":"http://example.test:8787","snapshot_id":"snapshot"}`,
		"query":   `{"schema_version":"1.0","address":"127.0.0.1:8787","url":"http://127.0.0.1:8787?token=x","snapshot_id":"snapshot"}`,
		"unknown": `{"schema_version":"1.0","address":"127.0.0.1:8787","url":"http://127.0.0.1:8787","snapshot_id":"snapshot","extra":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readOpenReadyReceipt(path); err == nil {
				t.Fatal("unsafe readiness receipt was accepted")
			}
		})
	}
	public := filepath.Join(root, "public.json")
	if err := os.WriteFile(public, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readOpenReadyReceipt(public); err == nil {
		t.Fatal("public readiness receipt was accepted")
	}
}

func TestOpenReadinessPreflightRejectsExistingButWatcherAcceptsImmediatePublication(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("desktop opener fixture is Linux-specific")
	}
	root := t.TempDir()
	ready := filepath.Join(root, "ready.json")
	if err := requireOpenReadyAbsent(ready); err != nil {
		t.Fatalf("absent readiness preflight = %v", err)
	}
	data := `{"schema_version":"1.0","address":"127.0.0.1:8787","url":"http://127.0.0.1:8787","snapshot_id":"snapshot"}`
	if err := os.WriteFile(ready, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireOpenReadyAbsent(ready); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing readiness preflight = %v", err)
	}
	opener := filepath.Join(root, "xdg-open")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	if err := waitForOpenReadyAndLaunch(context.Background(), ready); err != nil {
		t.Fatalf("immediately published readiness = %v", err)
	}
}
