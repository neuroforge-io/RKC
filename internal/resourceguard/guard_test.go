package resourceguard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPriorityCheckIsInjectedBeforeSpawn(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "spawned")
	command, err := newCommand(context.Background(), Config{
		Executable: "/bin/sh", Arguments: []string{"-c", "printf spawned > \"$1\"", "sh", marker},
		MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true,
	}, func() error { return ErrHigherPriorityActive })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := command.Run(context.Background(), nil, nil); !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("priority error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked process spawned: %v", err)
	}
}

func TestHigherPriorityArrivalStopsRunningProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	checks := 0
	priority := func() error {
		checks++
		if checks >= 2 {
			return ErrHigherPriorityActive
		}
		return nil
	}
	command, err := newCommand(context.Background(), Config{
		Executable: "/bin/sh", Arguments: []string{"-c", "sleep 10"},
		MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true,
	}, priority)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := command.Run(context.Background(), nil, nil); !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("mid-run priority error = %v", err)
	}
	if checks < 2 || time.Since(started) > 3*time.Second {
		t.Fatalf("priority rechecks = %d after %v", checks, time.Since(started))
	}
	if command.cmd.ProcessState == nil {
		t.Fatal("preempted process was not reaped")
	}
}

func TestHigherPriorityDetectionExcludesAncestorsAndAvoidsSubstrings(t *testing.T) {
	t.Parallel()

	processes := []processSnapshot{
		{pid: 10, parentPID: 9, commandLine: "rkc test /home/user/ERAIS/self"},
		{pid: 11, parentPID: 10, commandLine: "rkc open /home/user/ERAIS/repository"},
		{pid: 9, parentPID: 1, commandLine: "codex /tmp/lm_eval/ancestor"},
		{pid: 20, parentPID: 1, commandLine: "python /tmp/noteraisworker.py"},
		{pid: 21, parentPID: 1, commandLine: "python -m LM_EVAL --tasks x"},
		{pid: 22, parentPID: 1, commandLine: "TORCHRUN train.py"},
	}
	err := checkHigherPriority(processes, 10)
	if !errors.Is(err, ErrHigherPriorityActive) || !strings.Contains(err.Error(), "pid=21 marker=lm_eval") ||
		!strings.Contains(err.Error(), "pid=22 marker=torchrun") || strings.Contains(err.Error(), "pid=20") {
		t.Fatalf("priority conflicts = %v", err)
	}
	if err := checkHigherPriority(processes[:4], 10); err != nil {
		t.Fatalf("ancestors or substring caused false positive: %v", err)
	}
	if commandHasMarker("llama-cli --offline --model model.gguf --prompt explain ERAIS safely", "erais") {
		t.Fatal("a model prompt was mistaken for a higher-priority process")
	}
	if !commandHasMarker("python /home/user/erais/train.py --config run.json", "erais") {
		t.Fatal("an ERAIS script path was not detected")
	}
	if commandHasMarker("rkc open /home/user/erais", "erais") {
		t.Fatal("an RKC repository argument was mistaken for a higher-priority process")
	}
	if !processHasMarker(processSnapshot{
		arguments: []string{".venv/bin/python", "scripts/train.py"}, cwdMarker: "erais",
	}, "erais") {
		t.Fatal("relative Python target in an ERAIS working directory was missed")
	}
	if processHasMarker(processSnapshot{
		arguments: []string{"rkc", "open", "."}, cwdMarker: "erais",
	}, "erais") {
		t.Fatal("non-interpreter RKC work in an ERAIS directory became a false conflict")
	}
	for _, arguments := range [][]string{
		{"python", "-W", "ignore", "/home/user/erais/train.py"},
		{"python3.11", "-X", "dev", "/home/user/erais/train.py"},
		{"python", "-m", "lm_eval", "--tasks", "x"},
		{"bash", "-eu", "/home/user/erais/run.sh"},
		{"bash", "+o", "errexit", "/home/user/erais/run.sh"},
		{"bash", "+O", "extglob", "/home/user/erais/run.sh"},
		{"bash", "+e", "/home/user/erais/run.sh"},
	} {
		marker := "erais"
		if strings.Contains(strings.Join(arguments, " "), "lm_eval") {
			marker = "lm_eval"
		}
		if !commandArgumentsHaveMarker(arguments, marker) {
			t.Errorf("interpreter target was not detected: %q", arguments)
		}
	}
	for _, arguments := range [][]string{
		{"llama-cli", "--prompt", "explain -m ERAIS safely"},
		{"rkc", "answer", "--question", "-m", "ERAIS"},
		{"git", "-C", "/home/user/erais", "status"},
		{"aws", "--profile", "erais", "s3", "ls"},
		{"python", "-c", "print('erais')"},
		{"python", "/tmp/tool.py", "--repository", "/home/user/erais"},
	} {
		if commandArgumentsHaveMarker(arguments, "erais") {
			t.Errorf("application argument was mistaken for an interpreter target: %q", arguments)
		}
	}
}

func TestProcCommandLinePreservesEmptyArguments(t *testing.T) {
	t.Parallel()

	arguments := splitProcCommandLine([]byte("python\x00-W\x00\x00/home/user/erais/train.py\x00\x00"))
	want := []string{"python", "-W", "", "/home/user/erais/train.py", ""}
	if !slices.Equal(arguments, want) {
		t.Fatalf("proc arguments = %#v, want %#v", arguments, want)
	}
	if !commandArgumentsHaveMarker(arguments, "erais") {
		t.Fatal("empty interpreter option value shifted or hid the ERAIS target")
	}
	if onlyEmpty := splitProcCommandLine([]byte{'\x00'}); !slices.Equal(onlyEmpty, []string{""}) {
		t.Fatalf("single empty argv entry = %#v", onlyEmpty)
	}
	if splitProcCommandLine(nil) != nil {
		t.Fatal("empty proc cmdline did not remain absent")
	}
}

func TestProcSnapshotParsing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	process := filepath.Join(root, "42")
	if err := os.Mkdir(process, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(process, "cmdline"), []byte("python\x00-m\x00ERais\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(process, "stat"), []byte("42 (name with ) paren) S 7 0 0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/home/user/erais", filepath.Join(process, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-pid"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	processes, err := procProcessSnapshots(root)
	if err != nil || len(processes) != 1 || processes[0].pid != 42 || processes[0].parentPID != 7 ||
		processes[0].commandLine != "python -m ERais" || strings.Join(processes[0].arguments, "|") != "python|-m|ERais" || processes[0].cwdMarker != "erais" {
		t.Fatalf("proc snapshots = %+v, %v", processes, err)
	}
	for _, invalid := range []string{"", "12 no-close S 1", "12 (x)", "12 (x) S", "12 (x) S nope"} {
		if _, err := parseParentPID([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid stat %q", invalid)
		}
	}
}

func TestPrioritySnapshotFailuresAreHermetic(t *testing.T) {
	if _, err := procProcessSnapshots(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing proc root was accepted")
	}
	cycle := []processSnapshot{{pid: 10, parentPID: 9}, {pid: 9, parentPID: 10}}
	if err := checkHigherPriority(cycle, 10); err != nil {
		t.Fatalf("ancestor cycle produced a false conflict: %v", err)
	}
	if err := checkHigherPriority(nil, 10); err != nil {
		t.Fatalf("missing self snapshot produced a false conflict: %v", err)
	}

	root := t.TempDir()
	makeProcess := func(pid string) string {
		t.Helper()
		path := filepath.Join(root, pid)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	makeProcess("1")
	commandDirectory := makeProcess("2")
	if err := os.Mkdir(filepath.Join(commandDirectory, "cmdline"), 0o700); err != nil {
		t.Fatal(err)
	}
	missingStat := makeProcess("3")
	if err := os.WriteFile(filepath.Join(missingStat, "cmdline"), []byte("python\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidStat := makeProcess("4")
	if err := os.WriteFile(filepath.Join(invalidStat, "cmdline"), []byte("python\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidStat, "stat"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	processes, err := procProcessSnapshots(root)
	if err != nil || len(processes) != 0 {
		t.Fatalf("unstable proc fixtures = %+v, %v", processes, err)
	}
}

func TestUnguardedCommandLifecycleAndLimits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixtures")
	}
	allow := func() error { return nil }
	command, err := newCommand(context.Background(), Config{
		Executable: "/bin/sh", Arguments: []string{"-c", "printf ok"}, MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true,
	}, allow)
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if _, err := command.Run(context.Background(), &stdout, nil); err != nil || stdout.String() != "ok" {
		t.Fatalf("short command = %q, %v", stdout.String(), err)
	}

	missing, err := newCommand(context.Background(), Config{
		Executable: filepath.Join(t.TempDir(), "missing"), MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true,
	}, allow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Run(context.Background(), nil, nil); err == nil {
		t.Fatal("unstartable command was accepted")
	}

	cancelContext, cancel := context.WithCancel(context.Background())
	cancelled, err := newCommand(cancelContext, Config{
		Executable: "/bin/sh", Arguments: []string{"-c", "sleep 10"}, MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true,
	}, allow)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, err := cancelled.Run(cancelContext, nil, nil); err != context.Canceled {
		t.Fatalf("cancellation = %v", err)
	}
	if cancelled.cmd.ProcessState == nil {
		t.Fatal("cancelled process was not reaped")
	}

	overBudget := &Command{
		cmd: exec.Command("/bin/sh", "-c", "sleep 10"), maximumRSSBytes: 1, priorityCheck: allow,
	}
	if peak, err := overBudget.Run(context.Background(), nil, nil); !errors.Is(err, ErrRSSLimitExceeded) || peak <= 1 {
		t.Fatalf("RSS limit = peak %d, err %v", peak, err)
	}
	if _, err := (*Command)(nil).Run(context.Background(), nil, nil); err == nil {
		t.Fatal("nil command was accepted")
	}
	if ProcessRSS(-1) != 0 || ProcessRSS(999999999) != 0 {
		t.Fatal("invalid process reported RSS")
	}
}

func TestGuardedCancellationEscalatesUnitAndReturnsExactContext(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux transient-service fixture")
	}
	_, signals := installFakeResourceGuardCommands(t, "exec /bin/sleep 10\n")
	ctx, cancel := context.WithCancel(context.Background())
	command, err := newCommand(ctx, Config{
		Executable: "/bin/true", MaximumRSSBytes: 64 << 20, UnitPrefix: "rkc-lifecycle-test",
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if command.cmd.Cancel != nil {
		t.Fatal("guarded launcher is still bound to os/exec context cancellation")
	}
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, err := command.Run(ctx, nil, nil); err != context.Canceled {
		t.Fatalf("guarded cancellation = %v", err)
	}
	data, err := os.ReadFile(signals)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(data); !strings.Contains(text, "--signal=SIGTERM") || !strings.Contains(text, "--signal=SIGKILL") {
		t.Fatalf("unit escalation signals = %q", text)
	}
	if command.cmd.ProcessState == nil {
		t.Fatal("guarded launcher was not reaped")
	}
}

func TestCancellationCleansUnitRegisteredDuringLauncherReap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux transient-service fixture")
	}
	launcherBody := `trap '/bin/sleep 0.35; printf "active\n" > "$TMPDIR/unit-state"; : > "$TMPDIR/unit-registered"; exit 0' TERM
while :; do /bin/sleep 0.01; done
`
	state, signals := installFakeResourceGuardCommands(t, launcherBody)
	if err := os.WriteFile(state, []byte("inactive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	command, err := newCommand(ctx, Config{
		Executable: "/bin/true", MaximumRSSBytes: 64 << 20, UnitPrefix: "rkc-registration-test",
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, err := command.Run(ctx, nil, nil); err != context.Canceled {
		t.Fatalf("delayed-registration cancellation = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(state), "unit-registered")); err != nil {
		t.Fatalf("launcher did not exercise delayed registration: %v", err)
	}
	data, err := os.ReadFile(signals)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--signal=SIGKILL") {
		t.Fatalf("post-reap unit cleanup signals = %q", data)
	}
	data, err = os.ReadFile(state)
	if err != nil || strings.TrimSpace(string(data)) != "inactive" {
		t.Fatalf("post-reap unit state = %q, %v", data, err)
	}
}

func TestCanceledContextNeverSpawnsGuardedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	marker := filepath.Join(t.TempDir(), "spawned")
	newFixture := func(ctx context.Context, priority func() error) *Command {
		t.Helper()
		command, err := newCommand(ctx, Config{
			Executable:      "/bin/sh",
			Arguments:       []string{"-c", "printf spawned > \"$1\"", "sh", marker},
			MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true,
		}, priority)
		if err != nil {
			t.Fatal(err)
		}
		return command
	}

	preCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	priorityCalled := false
	command := newFixture(preCanceled, func() error {
		priorityCalled = true
		return nil
	})
	if _, err := command.Run(preCanceled, nil, nil); err != context.Canceled {
		t.Fatalf("pre-canceled run = %v", err)
	}
	if priorityCalled || command.cmd.Process != nil {
		t.Fatal("pre-canceled run advanced toward process spawn")
	}

	lateCanceled, cancel := context.WithCancel(context.Background())
	command = newFixture(lateCanceled, func() error {
		cancel()
		return nil
	})
	if _, err := command.Run(lateCanceled, nil, nil); err != context.Canceled {
		t.Fatalf("priority-check cancellation = %v", err)
	}
	if command.cmd.Process != nil {
		t.Fatal("command spawned after cancellation became observable")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled command wrote its marker: %v", err)
	}
}

func TestLauncherExitCleansStillActiveUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux transient-service fixture")
	}
	state, signals := installFakeResourceGuardCommands(t, "exit 0\n")
	command, err := newCommand(context.Background(), Config{
		Executable: "/bin/true", MaximumRSSBytes: 64 << 20, UnitPrefix: "rkc-lifecycle-test",
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := command.Run(context.Background(), nil, nil); !errors.Is(err, errLauncherExitedWithActiveUnit) {
		t.Fatalf("early launcher exit = %v", err)
	}
	data, err := os.ReadFile(signals)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(data); !strings.Contains(text, "--signal=SIGTERM") || !strings.Contains(text, "--signal=SIGKILL") {
		t.Fatalf("unit escalation signals = %q", text)
	}
	data, err = os.ReadFile(state)
	if err != nil || strings.TrimSpace(string(data)) != "inactive" {
		t.Fatalf("unit state = %q, %v", data, err)
	}
}

func TestLauncherExitWaitsForDelayedUnitPublication(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux transient-service fixture")
	}
	launcherBody := `(/bin/sleep 0.075; printf 'active\n' > "$TMPDIR/unit-state"; : > "$TMPDIR/unit-published") >/dev/null 2>&1 &
exit 0
`
	state, signals := installFakeResourceGuardCommands(t, launcherBody)
	if err := os.WriteFile(state, []byte("inactive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := newCommand(context.Background(), Config{
		Executable: "/bin/true", MaximumRSSBytes: 64 << 20, UnitPrefix: "rkc-publication-test",
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := command.Run(context.Background(), nil, nil); !errors.Is(err, errLauncherExitedWithActiveUnit) {
		t.Fatalf("delayed unit publication = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(state), "unit-published")); err != nil {
		t.Fatalf("launcher did not publish the delayed unit: %v", err)
	}
	data, err := os.ReadFile(signals)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(data); !strings.Contains(text, "--signal=SIGTERM") || !strings.Contains(text, "--signal=SIGKILL") {
		t.Fatalf("delayed unit cleanup signals = %q", text)
	}
	data, err = os.ReadFile(state)
	if err != nil || strings.TrimSpace(string(data)) != "inactive" {
		t.Fatalf("settled delayed unit state = %q, %v", data, err)
	}
}

func installFakeResourceGuardCommands(t *testing.T, launcherBody string) (string, string) {
	t.Helper()
	directory := t.TempDir()
	state := filepath.Join(directory, "unit-state")
	signals := filepath.Join(directory, "unit-signals")
	if err := os.WriteFile(state, []byte("active\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(directory, "systemd-run")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n"+launcherBody), 0o700); err != nil {
		t.Fatal(err)
	}
	controller := filepath.Join(directory, "systemctl")
	controllerBody := `#!/bin/sh
case "$2" in
show)
    exit 0
    ;;
is-active)
    /bin/cat "$TMPDIR/unit-state"
    exit 0
    ;;
kill)
    printf '%s\n' "$*" >> "$TMPDIR/unit-signals"
    case "$*" in
    *--signal=SIGKILL*)
        printf 'inactive\n' > "$TMPDIR/unit-state"
        ;;
    esac
    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(controller, []byte(controllerBody), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", directory)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state, signals
}

func TestConfigEnvironmentAndGuardArguments(t *testing.T) {
	t.Setenv("RKC_SECRET_TEST", "secret")
	environment := SanitizedModelEnvironment([]string{"OMP_NUM_THREADS=1", "RKC_SECRET_TEST=leak", "MALFORMED", "OMP_NUM_THREADS=2"})
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "OMP_NUM_THREADS=2") || strings.Contains(joined, "RKC_SECRET_TEST") ||
		!strings.Contains(joined, "CUDA_VISIBLE_DEVICES=-1") {
		t.Fatalf("sanitized environment = %q", joined)
	}
	for _, config := range []Config{
		{MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true},
		{Executable: "/bin/true", Environment: []string{"BAD"}, MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true},
		{Executable: "/bin/true", MaximumRSSBytes: -1, UnsafeDisableCgroup: true},
		{Executable: "/bin/true", MaximumRSSBytes: 1, UnsafeDisableCgroup: true},
		{Executable: "/bin/true", MaximumRSSBytes: 64 << 20, UnsafeDisablePriorityCheck: true},
	} {
		if _, err := newCommand(context.Background(), config, func() error { return nil }); err == nil {
			t.Fatalf("accepted invalid config: %+v", config)
		}
	}
	if validUnitPrefix("") || validUnitPrefix(strings.Repeat("a", 41)) || validUnitPrefix("bad.name") || !validUnitPrefix("rkc-model_1") {
		t.Fatal("unit prefix validation mismatch")
	}
	if runtime.GOOS == "linux" {
		command, err := newCommand(context.Background(), Config{
			Executable: "/bin/true", Arguments: []string{"literal-$RKC_MODEL", "literal-${RKC_MODEL}"},
			Environment: SanitizedModelEnvironment(nil), MaximumRSSBytes: 128 << 20, UnitPrefix: "rkc-test",
		}, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if command.cmd.Cancel != nil {
			t.Fatal("guarded systemd-run launcher inherited an automatic context cancellation hook")
		}
		arguments := strings.Join(command.cmd.Args, " ")
		for _, required := range []string{"--same-dir", "--expand-environment=no", "CPUWeight=1", "IOWeight=1", "CPUQuota=100%", "MemoryHigh=", "MemoryMax=134217728", "MemorySwapMax=", "TasksMax=128", "OOMPolicy=stop", "choom -n 750", "ionice -c 3", "nice -n 19", "env -i", "literal-$RKC_MODEL", "literal-${RKC_MODEL}"} {
			if !strings.Contains(arguments, required) {
				t.Errorf("guard arguments missing %q: %s", required, arguments)
			}
		}
	}
}

func TestGuardValidationBranchesDoNotInspectLiveProcesses(t *testing.T) {
	allow := func() error { return nil }
	publicCommand, err := NewCommand(context.Background(), Config{
		Executable: "/bin/true", MaximumRSSBytes: 64 << 20,
		UnsafeDisableCgroup: true, UnsafeDisablePriorityCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publicCommand.Run(context.Background(), nil, nil); err != nil {
		t.Fatalf("test-only public command = %v", err)
	}
	if _, err := newCommand(nil, Config{Executable: "/bin/true", MaximumRSSBytes: 64 << 20, UnsafeDisableCgroup: true}, allow); err == nil {
		t.Fatal("nil construction context was accepted")
	}
	defaultLimit, err := newCommand(context.Background(), Config{
		Executable: "/bin/true", UnsafeDisableCgroup: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaultLimit.maximumRSSBytes != 4608*1024*1024 {
		t.Fatalf("default RSS limit = %d", defaultLimit.maximumRSSBytes)
	}
	if _, err := defaultLimit.Run(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("nil run context error = %v", err)
	}
	if err := (*Command)(nil).waitUnitInactive(time.Second); err != nil {
		t.Fatalf("nil command unit wait = %v", err)
	}
}
