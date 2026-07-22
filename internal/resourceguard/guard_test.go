package resourceguard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if err := checkHigherPriority(processes[:3], 10); err != nil {
		t.Fatalf("ancestors or substring caused false positive: %v", err)
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
	if err := os.WriteFile(filepath.Join(root, "not-a-pid"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	processes, err := procProcessSnapshots(root)
	if err != nil || len(processes) != 1 || processes[0].pid != 42 || processes[0].parentPID != 7 || processes[0].commandLine != "python -m ERais" {
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
	if _, err := cancelled.Run(cancelContext, nil, nil); !errors.Is(err, context.Canceled) {
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
			Executable: "/bin/true", Environment: SanitizedModelEnvironment(nil), MaximumRSSBytes: 128 << 20, UnitPrefix: "rkc-test",
		}, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		arguments := strings.Join(command.cmd.Args, " ")
		for _, required := range []string{"CPUWeight=1", "IOWeight=1", "CPUQuota=100%", "MemoryHigh=", "MemoryMax=134217728", "MemorySwapMax=", "TasksMax=128", "OOMPolicy=stop", "choom -n 750", "ionice -c 3", "nice -n 19", "env -i"} {
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
	if defaultLimit.maximumRSSBytes != 2560*1024*1024 {
		t.Fatalf("default RSS limit = %d", defaultLimit.maximumRSSBytes)
	}
	if _, err := defaultLimit.Run(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("nil run context error = %v", err)
	}
	if err := (*Command)(nil).waitUnitInactive(time.Second); err != nil {
		t.Fatalf("nil command unit wait = %v", err)
	}
}
