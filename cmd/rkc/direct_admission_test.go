package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

type guardedDirectRunnerFunc func(context.Context, io.Writer, io.Writer) (int64, error)

func (run guardedDirectRunnerFunc) Run(ctx context.Context, stdout, stderr io.Writer) (int64, error) {
	return run(ctx, stdout, stderr)
}

func TestDirectCommandAdmissionSelectsExactlyOneExecutionPath(t *testing.T) {
	sentinel := errors.New("envelope proof failed")
	tests := []struct {
		name             string
		command          string
		args             []string
		platform         string
		guardedChild     string
		guardedOpenChild bool
		requireError     error
		prepareError     error
		wantLocal        int
		wantProtected    int
		wantLaunch       int
		wantRequire      int
		wantPrepare      int
		wantError        string
		wantWrapped      bool
	}{
		{name: "scan help stays local", command: "scan", args: []string{"--help"}, platform: "linux", wantLocal: 1},
		{name: "quickstart help stays local", command: "quickstart", args: []string{"--python", "--help"}, platform: "linux", wantLocal: 1},
		{name: "portable scan stays local", command: "scan", args: []string{"--no-python", "."}, platform: "darwin", wantLocal: 1},
		{name: "portable quickstart stays local", command: "quickstart", args: []string{"."}, platform: "windows", wantLocal: 1},
		{name: "Linux scan launches guard when current process is unbounded", command: "scan", args: []string{"--no-python", "."}, platform: "linux", prepareError: sentinel, wantPrepare: 1, wantLaunch: 1},
		{name: "Linux quickstart launches guard when current process is unbounded", command: "quickstart", args: []string{"."}, platform: "linux", prepareError: sentinel, wantPrepare: 1, wantLaunch: 1},
		{name: "Linux scan reuses an already proven current process", command: "scan", args: []string{"--no-python", "."}, platform: "linux", wantPrepare: 1, wantProtected: 1, wantLocal: 1},
		{name: "Linux quickstart reuses an already proven current process", command: "quickstart", args: []string{"."}, platform: "linux", wantPrepare: 1, wantProtected: 1, wantLocal: 1},
		{name: "scan child proves envelope", command: "scan", args: []string{"--no-plugins", "."}, platform: "linux", guardedChild: "scan", wantRequire: 1, wantProtected: 1, wantLocal: 1},
		{name: "quickstart child proves envelope", command: "quickstart", args: []string{"."}, platform: "linux", guardedChild: "quickstart", wantRequire: 1, wantProtected: 1, wantLocal: 1},
		{name: "open child proves envelope", command: "quickstart", args: []string{"."}, platform: "linux", guardedOpenChild: true, wantRequire: 1, wantProtected: 1, wantLocal: 1},
		{name: "child cannot recurse after proof failure", command: "scan", args: []string{"--no-python", "."}, platform: "linux", guardedChild: "scan", requireError: sentinel, wantRequire: 1, wantError: "protected scan child", wantWrapped: true},
		{name: "cross command marker fails closed", command: "scan", args: []string{"--no-python", "."}, platform: "linux", guardedChild: "quickstart", wantError: "cross-command admission"},
		{name: "unsafe scan fails before admission", command: "scan", args: []string{"."}, platform: "linux", wantError: "rkc scan --no-python"},
		{name: "false override fails before admission", command: "scan", args: []string{"--no-python", "--no-python=false", "."}, platform: "linux", wantError: "explicit final"},
		{name: "quickstart Python fails before admission", command: "quickstart", args: []string{"--python", "."}, platform: "linux", wantError: "rkc quickstart"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			localCalls := 0
			protectedCalls := 0
			launchCalls := 0
			requireCalls := 0
			prepareCalls := 0
			original := append([]string(nil), test.args...)
			local := func(_ context.Context, args []string) error {
				localCalls++
				if !reflect.DeepEqual(args, original) {
					t.Fatalf("local args = %#v, want %#v", args, original)
				}
				return nil
			}
			err := runDirectCommandWithAdmissionUsing(
				context.Background(),
				test.command,
				test.args,
				test.platform,
				test.guardedChild,
				test.guardedOpenChild,
				func() error {
					requireCalls++
					return test.requireError
				},
				func() error {
					prepareCalls++
					return test.prepareError
				},
				func(_ context.Context, command string, args []string) error {
					launchCalls++
					if command != test.command || !reflect.DeepEqual(args, original) {
						t.Fatalf("launch = %q %#v, want %q %#v", command, args, test.command, original)
					}
					return nil
				},
				local,
				func(ctx context.Context, args []string) error {
					protectedCalls++
					return local(ctx, args)
				},
			)
			if test.wantError == "" && err != nil {
				t.Fatalf("admission = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("admission error = %v, want %q", err, test.wantError)
			}
			if test.wantWrapped && !errors.Is(err, sentinel) {
				t.Fatalf("admission error = %v, want wrapped sentinel", err)
			}
			if localCalls != test.wantLocal || protectedCalls != test.wantProtected ||
				launchCalls != test.wantLaunch || requireCalls != test.wantRequire || prepareCalls != test.wantPrepare {
				t.Fatalf(
					"calls = local:%d protected:%d launch:%d require:%d prepare:%d, want local:%d protected:%d launch:%d require:%d prepare:%d",
					localCalls, protectedCalls, launchCalls, requireCalls, prepareCalls,
					test.wantLocal, test.wantProtected, test.wantLaunch, test.wantRequire, test.wantPrepare,
				)
			}
		})
	}
}

func TestDirectCommandAdmissionRejectsMissingDependencies(t *testing.T) {
	require := func() error { return nil }
	prepare := func() error { return nil }
	launch := func(context.Context, string, []string) error { return nil }
	local := func(context.Context, []string) error { return nil }
	for name, call := range map[string]func() error{
		"nil context": func() error {
			return runDirectCommandWithAdmissionUsing(nil, "scan", []string{"--no-python"}, "linux", "", false, require, prepare, launch, local, local)
		},
		"nil require": func() error {
			return runDirectCommandWithAdmissionUsing(context.Background(), "scan", []string{"--no-python"}, "linux", "", false, nil, prepare, launch, local, local)
		},
		"nil prepare": func() error {
			return runDirectCommandWithAdmissionUsing(context.Background(), "scan", []string{"--no-python"}, "linux", "", false, require, nil, launch, local, local)
		},
		"nil launch": func() error {
			return runDirectCommandWithAdmissionUsing(context.Background(), "scan", []string{"--no-python"}, "linux", "", false, require, prepare, nil, local, local)
		},
		"nil local": func() error {
			return runDirectCommandWithAdmissionUsing(context.Background(), "scan", []string{"--no-python"}, "linux", "", false, require, prepare, launch, nil, local)
		},
		"nil protected local": func() error {
			return runDirectCommandWithAdmissionUsing(context.Background(), "scan", []string{"--no-python"}, "linux", "", false, require, prepare, launch, local, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || err.Error() != "direct command resource admission is not configured" {
				t.Fatalf("configuration error = %v", err)
			}
		})
	}
}

func TestProtectedDirectLocalContinuouslyYieldsAndReprovesEnvelope(t *testing.T) {
	priorityActive := errors.New("priority active")
	envelopeDrift := errors.New("envelope drift")
	for _, test := range []struct {
		name              string
		checkPriority     func() error
		requireEnvelope   func() error
		triggerTick       bool
		wantError         error
		wantLocal         int
		wantTickerStarted int
	}{
		{
			name: "initial priority rejection starts no work",
			checkPriority: func() error {
				return priorityActive
			},
			requireEnvelope: func() error { return nil },
			wantError:       priorityActive,
		},
		{
			name:              "initial envelope rejection starts no work",
			checkPriority:     func() error { return nil },
			requireEnvelope:   func() error { return envelopeDrift },
			wantError:         envelopeDrift,
			wantTickerStarted: 0,
		},
		{
			name: "continuous priority rejection cancels work",
			checkPriority: func() func() error {
				calls := 0
				return func() error {
					calls++
					if calls > 1 {
						return priorityActive
					}
					return nil
				}
			}(),
			requireEnvelope:   func() error { return nil },
			triggerTick:       true,
			wantError:         priorityActive,
			wantLocal:         1,
			wantTickerStarted: 1,
		},
		{
			name:          "continuous envelope rejection cancels work",
			checkPriority: func() error { return nil },
			requireEnvelope: func() func() error {
				calls := 0
				return func() error {
					calls++
					if calls > 1 {
						return envelopeDrift
					}
					return nil
				}
			}(),
			triggerTick:       true,
			wantError:         envelopeDrift,
			wantLocal:         1,
			wantTickerStarted: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			localCalls := 0
			tickerStarts := 0
			stops := 0
			ticks := make(chan time.Time, 1)
			if test.triggerTick {
				ticks <- time.Unix(1, 0)
			}
			err := runProtectedDirectLocalUsing(
				context.Background(),
				"scan",
				[]string{"--no-python", "."},
				func(ctx context.Context, _ []string) error {
					localCalls++
					if test.triggerTick {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				},
				protectedDirectLocalDependencies{
					checkHigherPriority: test.checkPriority,
					requireEnvelope:     test.requireEnvelope,
					newTicker: func(interval time.Duration) (<-chan time.Time, func()) {
						tickerStarts++
						if interval != directCurrentProcessCheckInterval {
							t.Fatalf("monitor interval = %s", interval)
						}
						return ticks, func() { stops++ }
					},
				},
			)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("protected local error = %v, want %v", err, test.wantError)
			}
			if localCalls != test.wantLocal || tickerStarts != test.wantTickerStarted || stops != test.wantTickerStarted {
				t.Fatalf(
					"calls local=%d ticker=%d stop=%d, want local=%d ticker/stop=%d",
					localCalls,
					tickerStarts,
					stops,
					test.wantLocal,
					test.wantTickerStarted,
				)
			}
		})
	}
}

func TestProtectedDirectLocalReturnsLocalFailureWithoutMonitorNoise(t *testing.T) {
	sentinel := errors.New("local failure")
	ticks := make(chan time.Time)
	stopped := 0
	err := runProtectedDirectLocalUsing(
		context.Background(),
		"quickstart",
		nil,
		func(context.Context, []string) error { return sentinel },
		protectedDirectLocalDependencies{
			checkHigherPriority: func() error { return nil },
			requireEnvelope:     func() error { return nil },
			newTicker: func(time.Duration) (<-chan time.Time, func()) {
				return ticks, func() { stopped++ }
			},
		},
	)
	if !errors.Is(err, sentinel) || stopped != 1 {
		t.Fatalf("local result = %v, stopped=%d", err, stopped)
	}
}

func TestProtectedDirectLocalHonorsCancellationBeforeInspectionOrWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := runProtectedDirectLocalUsing(
		ctx,
		"scan",
		nil,
		func(context.Context, []string) error { calls++; return nil },
		protectedDirectLocalDependencies{
			checkHigherPriority: func() error { calls++; return nil },
			requireEnvelope:     func() error { calls++; return nil },
			newTicker: func(time.Duration) (<-chan time.Time, func()) {
				calls++
				return make(chan time.Time), func() {}
			},
		},
	)
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("pre-cancelled local result = %v, calls=%d", err, calls)
	}
}

func TestDirectAdmissionJoinsReuseAndLaunchFailuresWithoutLocalFallback(t *testing.T) {
	reuseFailure := errors.New("reuse failure")
	launchFailure := errors.New("launch failure")
	localCalls := 0
	err := runDirectCommandWithAdmissionUsing(
		context.Background(),
		"scan",
		[]string{"--no-python", "."},
		"linux",
		"",
		false,
		func() error { return nil },
		func() error { return reuseFailure },
		func(context.Context, string, []string) error { return launchFailure },
		func(context.Context, []string) error { localCalls++; return nil },
		func(context.Context, []string) error { localCalls++; return nil },
	)
	if !errors.Is(err, reuseFailure) || !errors.Is(err, launchFailure) || localCalls != 0 {
		t.Fatalf("joined admission failure = %v, local calls=%d", err, localCalls)
	}
}

func TestProtectedDirectLocalRejectsMissingMonitorDependencies(t *testing.T) {
	local := func(context.Context, []string) error { return nil }
	valid := protectedDirectLocalDependencies{
		checkHigherPriority: func() error { return nil },
		requireEnvelope:     func() error { return nil },
		newTicker: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
	}
	for name, call := range map[string]func() error{
		"nil context": func() error {
			return runProtectedDirectLocalUsing(nil, "scan", nil, local, valid)
		},
		"nil local": func() error {
			return runProtectedDirectLocalUsing(context.Background(), "scan", nil, nil, valid)
		},
		"nil priority": func() error {
			dependencies := valid
			dependencies.checkHigherPriority = nil
			return runProtectedDirectLocalUsing(context.Background(), "scan", nil, local, dependencies)
		},
		"nil envelope": func() error {
			dependencies := valid
			dependencies.requireEnvelope = nil
			return runProtectedDirectLocalUsing(context.Background(), "scan", nil, local, dependencies)
		},
		"nil ticker factory": func() error {
			dependencies := valid
			dependencies.newTicker = nil
			return runProtectedDirectLocalUsing(context.Background(), "scan", nil, local, dependencies)
		},
		"nil ticker result": func() error {
			dependencies := valid
			dependencies.newTicker = func(time.Duration) (<-chan time.Time, func()) { return nil, nil }
			return runProtectedDirectLocalUsing(context.Background(), "scan", nil, local, dependencies)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || !strings.Contains(err.Error(), "not configured") {
				t.Fatalf("monitor configuration error = %v", err)
			}
		})
	}
}

func TestLaunchGuardedDirectUsesExactArgumentsGuardAndSanitizedEnvironment(t *testing.T) {
	t.Setenv("RKC_DIRECT_SECRET_SENTINEL", "must-not-pass")
	t.Setenv("DISPLAY", ":42")
	t.Setenv("HOME", t.TempDir())
	var captured resourceguard.Config
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	dependencies := guardedDirectLaunchDependencies{
		executable: func() (string, error) { return "/opt/rkc", nil },
		newCommand: func(_ context.Context, config resourceguard.Config) (guardedOpenRunner, error) {
			captured = config
			return guardedDirectRunnerFunc(func(_ context.Context, observedStdout, observedStderr io.Writer) (int64, error) {
				if observedStdout != &stdout || observedStderr != &stderr {
					return 0, errors.New("unexpected output writers")
				}
				return 123, nil
			}), nil
		},
		stdout: &stdout,
		stderr: &stderr,
	}
	args := []string{"--out", "atlas", "--no-python=true", "repository"}
	if err := launchGuardedDirectUsing(context.Background(), "scan", args, dependencies); err != nil {
		t.Fatalf("protected scan = %v", err)
	}
	if captured.Executable != "/opt/rkc" || captured.MaximumRSSBytes != resourceguard.LowPriorityMemoryMaxBytes ||
		captured.UnitPrefix != "rkc-low" || captured.UnsafeDisableCgroup || captured.UnsafeDisablePriorityCheck {
		t.Fatalf("guard config = %+v", captured)
	}
	wantArguments := append([]string{"scan"}, args...)
	if !reflect.DeepEqual(captured.Arguments, wantArguments) {
		t.Fatalf("guard arguments = %#v, want %#v", captured.Arguments, wantArguments)
	}
	if !sort.StringsAreSorted(captured.Environment) {
		t.Fatalf("guard environment is not sorted: %#v", captured.Environment)
	}
	environment := strings.Join(captured.Environment, "\n")
	for _, required := range []string{
		guardedDirectChildEnvironment + "=scan",
		"GOMAXPROCS=1",
		"OMP_NUM_THREADS=1",
		"CUDA_VISIBLE_DEVICES=-1",
	} {
		if !strings.Contains(environment, required) {
			t.Fatalf("guard environment omits %q: %s", required, environment)
		}
	}
	for _, forbidden := range []string{
		guardedOpenChildEnvironment + "=1",
		"RKC_DIRECT_SECRET_SENTINEL",
		"must-not-pass",
		"DISPLAY=:42",
	} {
		if strings.Contains(environment, forbidden) {
			t.Fatalf("guard environment retained %q: %s", forbidden, environment)
		}
	}
}

func TestLaunchGuardedDirectReportsFailuresAndMapsCleanCancellation(t *testing.T) {
	sentinel := errors.New("guard failure")
	base := func(runner guardedOpenRunner) guardedDirectLaunchDependencies {
		return guardedDirectLaunchDependencies{
			executable: func() (string, error) { return "/opt/rkc", nil },
			newCommand: func(context.Context, resourceguard.Config) (guardedOpenRunner, error) { return runner, nil },
			stdout:     io.Discard,
			stderr:     io.Discard,
		}
	}

	if err := launchGuardedDirectUsing(nil, "scan", []string{"--no-python"}, guardedDirectLaunchDependencies{}); err == nil ||
		err.Error() != "protected direct launch dependencies are not configured" {
		t.Fatalf("nil launch dependencies = %v", err)
	}

	dependencies := base(nil)
	dependencies.executable = func() (string, error) { return "", sentinel }
	if err := launchGuardedDirectUsing(context.Background(), "scan", []string{"--no-python"}, dependencies); err == nil ||
		!errors.Is(err, sentinel) || !strings.Contains(err.Error(), "resolve RKC executable") {
		t.Fatalf("executable failure = %v", err)
	}

	dependencies = base(nil)
	dependencies.newCommand = func(context.Context, resourceguard.Config) (guardedOpenRunner, error) { return nil, sentinel }
	if err := launchGuardedDirectUsing(context.Background(), "quickstart", nil, dependencies); err == nil ||
		!errors.Is(err, sentinel) || !strings.Contains(err.Error(), "configure protected quickstart") {
		t.Fatalf("guard construction failure = %v", err)
	}

	if err := launchGuardedDirectUsing(context.Background(), "scan", []string{"--no-python"}, base(nil)); err == nil ||
		!strings.Contains(err.Error(), "guarded command is not configured") {
		t.Fatalf("nil guarded command = %v", err)
	}

	dependencies = base(guardedDirectRunnerFunc(func(context.Context, io.Writer, io.Writer) (int64, error) {
		return 0, sentinel
	}))
	if err := launchGuardedDirectUsing(context.Background(), "scan", []string{"--no-python"}, dependencies); err == nil ||
		!errors.Is(err, sentinel) || !strings.Contains(err.Error(), "protected scan") {
		t.Fatalf("guard run failure = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	dependencies = base(guardedDirectRunnerFunc(func(context.Context, io.Writer, io.Writer) (int64, error) {
		cancel()
		return 0, context.Canceled
	}))
	if err := launchGuardedDirectUsing(ctx, "quickstart", nil, dependencies); err != nil {
		t.Fatalf("clean cancellation = %v", err)
	}
}

func TestDirectAdmissionArgumentSafetyIsGrammarAware(t *testing.T) {
	for _, args := range [][]string{
		{"--no-python", "."},
		{"-no-python=true", "."},
		{"--no-plugins=true", "."},
		{"--no-python=false", "--no-python=true", "."},
		{"--no-python=false", "--no-plugins=true", "."},
		{"--out", "atlas", "--force=false", "--no-python", "."},
	} {
		help, err := validateDirectCommandAdmission("scan", args)
		if help || err != nil {
			t.Errorf("safe scan %#v = help:%t error:%v", args, help, err)
		}
	}
	for _, args := range [][]string{
		nil,
		{"."},
		{"--no-python=false", "."},
		{"--no-plugins=false", "."},
		{"--no-python", "--no-python=false", "."},
		{"--out", "--no-python", "."},
		{"--", "--no-python", "."},
		{".", "--no-python"},
	} {
		help, err := validateDirectCommandAdmission("scan", args)
		if help || err == nil || !strings.Contains(err.Error(), "rkc scan --no-python") {
			t.Errorf("unsafe scan %#v = help:%t error:%v", args, help, err)
		}
	}
	if _, err := validateDirectCommandAdmission("scan", []string{"--no-python=invalid", "."}); err == nil ||
		!strings.Contains(err.Error(), "invalid --no-python") || !strings.Contains(err.Error(), "rkc scan --no-python") {
		t.Fatalf("invalid scan safety boolean = %v", err)
	}

	for _, args := range [][]string{nil, {"."}, {"--python=false", "."}, {"--python=true", "--python=false", "."}} {
		help, err := validateDirectCommandAdmission("quickstart", args)
		if help || err != nil {
			t.Errorf("safe quickstart %#v = help:%t error:%v", args, help, err)
		}
	}
	for _, args := range [][]string{{"--python", "."}, {"--python=true", "."}, {"--python=false", "--python=true", "."}} {
		help, err := validateDirectCommandAdmission("quickstart", args)
		if help || err == nil || !strings.Contains(err.Error(), "rkc quickstart") {
			t.Errorf("unsafe quickstart %#v = help:%t error:%v", args, help, err)
		}
	}
	if _, err := validateDirectCommandAdmission("quickstart", []string{"--python=invalid", "."}); err == nil ||
		!strings.Contains(err.Error(), "invalid --python") || !strings.Contains(err.Error(), "rkc quickstart") {
		t.Fatalf("invalid quickstart safety boolean = %v", err)
	}

	for _, test := range []struct {
		command string
		args    []string
	}{
		{command: "scan", args: []string{"--help"}},
		{command: "scan", args: []string{"--force", "-h"}},
		{command: "quickstart", args: []string{"--python", "--help"}},
	} {
		help, err := validateDirectCommandAdmission(test.command, test.args)
		if !help || err != nil {
			t.Errorf("help %s %#v = help:%t error:%v", test.command, test.args, help, err)
		}
	}
	if help, err := validateDirectCommandAdmission("scan", []string{"--config", "--help", "--no-python"}); help || err != nil {
		t.Fatalf("value-position help was misclassified: help:%t error:%v", help, err)
	}
	if _, err := validateDirectCommandAdmission("serve", nil); err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("unsupported direct command = %v", err)
	}
}

func TestDefaultGuardedDirectDependenciesWireResourceGuardWithoutStartingIt(t *testing.T) {
	dependencies := defaultGuardedDirectLaunchDependencies()
	if dependencies.executable == nil || dependencies.newCommand == nil || dependencies.stdout == nil || dependencies.stderr == nil {
		t.Fatal("default protected direct dependencies are incomplete")
	}
	if _, err := dependencies.newCommand(context.Background(), resourceguard.Config{}); err == nil ||
		!strings.Contains(err.Error(), "guarded executable is required") {
		t.Fatalf("default resource guard constructor = %v", err)
	}
}
