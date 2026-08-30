package resourceguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRequireProcessLowPriorityAcceptsExactEnvelope(t *testing.T) {
	fixture := newEnvelopeFixture(t)
	if err := requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, func(int) (schedulingEnvelope, error) {
		return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
	}); err != nil {
		t.Fatalf("exact low-priority envelope was rejected: %v", err)
	}
	if err := os.Remove(filepath.Join(fixture.unit, "io.weight")); err != nil {
		t.Fatal(err)
	}
	if err := requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, func(int) (schedulingEnvelope, error) {
		return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
	}); err != nil {
		t.Fatalf("idle-I/O fallback without delegated io.weight was rejected: %v", err)
	}
}

func TestPrepareCurrentProcessLowPriorityUsesExactThenNarrowExternalProof(t *testing.T) {
	exactFailure := fmt.Errorf("%w: exact", ErrLowPriorityEnvelope)
	externalFailure := fmt.Errorf("%w: external", ErrLowPriorityEnvelope)
	for _, test := range []struct {
		name          string
		exactError    error
		reserved      bool
		reservedError error
		externalError error
		wantError     error
		wantReserved  int
		wantExternal  int
	}{
		{name: "exact proof wins"},
		{name: "reserved name cannot fall back", exactError: exactFailure, reserved: true, wantError: exactFailure, wantReserved: 1},
		{name: "container root fallback succeeds", exactError: exactFailure, wantReserved: 1, wantExternal: 1},
		{name: "container root fallback fails closed", exactError: exactFailure, externalError: externalFailure, wantError: ErrLowPriorityEnvelope, wantReserved: 1, wantExternal: 1},
		{name: "reserved inspection fails closed", exactError: exactFailure, reservedError: externalFailure, wantError: ErrLowPriorityEnvelope, wantReserved: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			reservedCalls := 0
			externalCalls := 0
			err := prepareCurrentProcessLowPriorityUsing(
				func() error { return test.exactError },
				func() (bool, error) {
					reservedCalls++
					return test.reserved, test.reservedError
				},
				func() error {
					externalCalls++
					return test.externalError
				},
			)
			if test.wantError == nil && err != nil {
				t.Fatalf("prepare envelope = %v", err)
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("prepare envelope error = %v, want %v", err, test.wantError)
			}
			if reservedCalls != test.wantReserved || externalCalls != test.wantExternal {
				t.Fatalf("calls reserved=%d external=%d, want %d/%d", reservedCalls, externalCalls, test.wantReserved, test.wantExternal)
			}
		})
	}
	if err := prepareCurrentProcessLowPriorityUsing(nil, func() (bool, error) { return false, nil }, func() error { return nil }); !errors.Is(err, ErrLowPriorityEnvelope) {
		t.Fatalf("missing exact inspector = %v", err)
	}
}

func TestExternalContainerRootEnvelopeLowersAndReprovesScheduling(t *testing.T) {
	fixture := newExternalEnvelopeFixture(t)
	state := schedulingEnvelope{nice: 0, ioClass: 0}
	inspectCalls := 0
	lowerCalls := 0
	filesystemCalls := 0
	err := requireExternalProcessLowPriority(
		fixture.proc,
		fixture.cgroup,
		fixture.pid,
		externalEnvelopeDependencies{
			verifyFilesystems: func(processRoot, cgroupRoot string) error {
				filesystemCalls++
				if processRoot != fixture.process || cgroupRoot != fixture.cgroup {
					t.Fatalf("verified roots = %q %q", processRoot, cgroupRoot)
				}
				return nil
			},
			inspectScheduling: func(pid int) (schedulingEnvelope, error) {
				inspectCalls++
				if pid != fixture.pid {
					t.Fatalf("scheduling pid = %d", pid)
				}
				return state, nil
			},
			lowerScheduling: func(pid int) error {
				lowerCalls++
				if pid != fixture.pid {
					t.Fatalf("lower pid = %d", pid)
				}
				state = schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}
				return nil
			},
		},
		true,
	)
	if err != nil {
		t.Fatalf("bounded container envelope = %v", err)
	}
	if inspectCalls != 2 || lowerCalls != 1 || filesystemCalls != 2 {
		t.Fatalf("calls inspect=%d lower=%d filesystem=%d", inspectCalls, lowerCalls, filesystemCalls)
	}

	fixture.writeProc("oom_score_adj", "1000\n")
	fixture.writeControl("io.weight", "default 1\n8:0 1\n")
	if err := os.Remove(filepath.Join(fixture.cgroup, "io.weight")); err != nil {
		t.Fatal(err)
	}
	if err := requireExternalProcessLowPriority(
		fixture.proc,
		fixture.cgroup,
		fixture.pid,
		externalEnvelopeDependencies{
			verifyFilesystems: func(string, string) error { return nil },
			inspectScheduling: func(int) (schedulingEnvelope, error) {
				return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
			},
		},
		false,
	); err != nil {
		t.Fatalf("already-low stricter container envelope = %v", err)
	}
}

func TestExternalContainerRootEnvelopeRejectsEveryIncompleteBoundary(t *testing.T) {
	sentinel := errors.New("inspector failure")
	tests := []struct {
		name       string
		mutate     func(*envelopeFixture)
		verify     func(string, string) error
		inspect    func(int) (schedulingEnvelope, error)
		lower      func(int) error
		allowLower bool
		want       string
	}{
		{name: "non-root membership", mutate: func(f *envelopeFixture) { f.writeProc("cgroup", "0::/container/leaf\n") }, want: "namespace root"},
		{name: "thread outside root", mutate: func(f *envelopeFixture) {
			if err := os.WriteFile(filepath.Join(f.process, "task", strconv.Itoa(f.pid), "cgroup"), []byte("0::/other\n"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}, want: "thread cgroup"},
		{name: "wrong filesystems", verify: func(string, string) error { return sentinel }, want: "kernel control filesystems"},
		{name: "unlimited cpu", mutate: func(f *envelopeFixture) { f.writeControl("cpu.max", "max 100000\n") }, want: "one-core"},
		{name: "over one cpu", mutate: func(f *envelopeFixture) { f.writeControl("cpu.max", "100001 100000\n") }, want: "one-core"},
		{name: "cpu burst", mutate: func(f *envelopeFixture) { f.writeControl("cpu.max.burst", "1\n") }, want: "burst"},
		{name: "memory hard max", mutate: func(f *envelopeFixture) { f.writeControl("memory.max", strconv.FormatInt(rkcMemoryMaxBytes+1, 10)) }, want: "memory.max"},
		{name: "swap max", mutate: func(f *envelopeFixture) { f.writeControl("memory.swap.max", strconv.FormatInt(rkcSwapMaxBytes+1, 10)) }, want: "memory.swap.max"},
		{name: "tasks max", mutate: func(f *envelopeFixture) { f.writeControl("pids.max", "129\n") }, want: "pids.max"},
		{name: "memory usage above actual limit", mutate: func(f *envelopeFixture) {
			f.writeControl("memory.current", strconv.FormatInt(2560*1024*1024+1, 10))
		}, want: "hard limit"},
		{name: "swap usage above limit", mutate: func(f *envelopeFixture) {
			f.writeControl("memory.swap.current", strconv.FormatInt(rkcSwapMaxBytes+1, 10))
		}, want: "memory.swap.current"},
		{name: "tasks usage above limit", mutate: func(f *envelopeFixture) { f.writeControl("pids.current", "129\n") }, want: "pids.current"},
		{name: "cpu weight", mutate: func(f *envelopeFixture) { f.writeControl("cpu.weight", "2\n") }, want: "cpu.weight"},
		{name: "missing cpu weight", mutate: func(f *envelopeFixture) {
			if err := os.Remove(filepath.Join(f.cgroup, "cpu.weight")); err != nil {
				f.t.Fatal(err)
			}
		}, want: "cpu.weight"},
		{name: "I/O default weight", mutate: func(f *envelopeFixture) { f.writeControl("io.weight", "default 2\n") }, want: "io.weight"},
		{name: "I/O device override", mutate: func(f *envelopeFixture) { f.writeControl("io.weight", "default 1\n8:0 2\n") }, want: "io.weight"},
		{name: "OOM adjustment", mutate: func(f *envelopeFixture) { f.writeProc("oom_score_adj", "749\n") }, want: "OOM"},
		{name: "scheduling inspection", inspect: func(int) (schedulingEnvelope, error) { return schedulingEnvelope{}, sentinel }, want: "inspect process scheduling"},
		{name: "unprepared scheduling", inspect: func(int) (schedulingEnvelope, error) { return schedulingEnvelope{}, nil }, want: "expected nice"},
		{name: "invalid scheduling", inspect: func(int) (schedulingEnvelope, error) { return schedulingEnvelope{nice: 20, ioClass: 4}, nil }, allowLower: true, want: "invalid state"},
		{name: "lowering failure", inspect: func(int) (schedulingEnvelope, error) { return schedulingEnvelope{}, nil }, lower: func(int) error { return sentinel }, allowLower: true, want: "lower process scheduling"},
		{name: "lowering not proved", inspect: func(int) (schedulingEnvelope, error) { return schedulingEnvelope{}, nil }, lower: func(int) error { return nil }, allowLower: true, want: "lowered process scheduling"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExternalEnvelopeFixture(t)
			if test.mutate != nil {
				test.mutate(fixture)
			}
			verify := test.verify
			if verify == nil {
				verify = func(string, string) error { return nil }
			}
			inspect := test.inspect
			if inspect == nil {
				inspect = func(int) (schedulingEnvelope, error) {
					return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
				}
			}
			lower := test.lower
			if test.allowLower && lower == nil {
				lower = func(int) error { return nil }
			}
			err := requireExternalProcessLowPriority(
				fixture.proc,
				fixture.cgroup,
				fixture.pid,
				externalEnvelopeDependencies{
					verifyFilesystems: verify,
					inspectScheduling: inspect,
					lowerScheduling:   lower,
				},
				test.allowLower,
			)
			if !errors.Is(err, ErrLowPriorityEnvelope) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("external envelope error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRequireProcessLowPriorityRejectsPartialEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*envelopeFixture)
		nice   int
		io     int
		want   string
	}{
		{"wrong unit", func(f *envelopeFixture) { f.writeProc("cgroup", "0::/user.slice/not-rkc.scope\n") }, rkcNice, rkcIOClassIdle, "not an rkc-low"},
		{"cpu weight", func(f *envelopeFixture) { f.writeControl("cpu.weight", "100\n") }, rkcNice, rkcIOClassIdle, "cpu.weight"},
		{"unlimited cpu", func(f *envelopeFixture) { f.writeControl("cpu.max", "max 100000\n") }, rkcNice, rkcIOClassIdle, "one-core"},
		{"over one cpu", func(f *envelopeFixture) { f.writeControl("cpu.max", "100001 100000\n") }, rkcNice, rkcIOClassIdle, "one-core"},
		{"cpu burst", func(f *envelopeFixture) { f.writeControl("cpu.max.burst", "1\n") }, rkcNice, rkcIOClassIdle, "burst"},
		{"memory high", func(f *envelopeFixture) { f.writeControl("memory.high", "max\n") }, rkcNice, rkcIOClassIdle, "memory.high"},
		{"memory max", func(f *envelopeFixture) { f.writeControl("memory.max", "1\n") }, rkcNice, rkcIOClassIdle, "memory.max"},
		{"swap", func(f *envelopeFixture) { f.writeControl("memory.swap.max", "1\n") }, rkcNice, rkcIOClassIdle, "memory.swap.max"},
		{"tasks", func(f *envelopeFixture) { f.writeControl("pids.max", "129\n") }, rkcNice, rkcIOClassIdle, "pids.max"},
		{"io weight", func(f *envelopeFixture) { f.writeControl("io.weight", "default 100\n") }, rkcNice, rkcIOClassIdle, "io.weight"},
		{"oom adjustment", func(f *envelopeFixture) { f.writeProc("oom_score_adj", "0\n") }, rkcNice, rkcIOClassIdle, "OOM"},
		{"nice", func(*envelopeFixture) {}, 18, rkcIOClassIdle, "nice"},
		{"ionice", func(*envelopeFixture) {}, rkcNice, 2, "I/O scheduling"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEnvelopeFixture(t)
			test.mutate(fixture)
			err := requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, func(int) (schedulingEnvelope, error) {
				return schedulingEnvelope{nice: test.nice, ioClass: test.io}, nil
			})
			if !errors.Is(err, ErrLowPriorityEnvelope) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("partial envelope error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEnvelopeParsingRejectsAmbiguousPathsAndSchedulingFailure(t *testing.T) {
	for _, record := range []string{
		"", "1:name:/legacy", "0::/", "0::relative", "0::/a/../b", "0::/a\n0::/b",
	} {
		if _, err := unifiedCgroupPath(record); err == nil {
			t.Fatalf("invalid cgroup record was accepted: %q", record)
		}
	}
	for _, unit := range []string{"", "rkc-low-.scope", "rkc-low-name.scope", "rkc-low-12-name.service", "rkc-low-12-.service", "rkc-low-12.slice", "other-12.scope"} {
		if validLowPriorityUnit(unit) {
			t.Fatalf("invalid low-priority unit was accepted: %q", unit)
		}
	}
	if !validLowPriorityUnit("rkc-low-12.scope") || !validLowPriorityUnit("rkc-low-99.service") || !validLowPriorityUnit("rkc-low-12-345.service") {
		t.Fatal("valid low-priority unit was rejected")
	}
	fixture := newEnvelopeFixture(t)
	err := requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, func(int) (schedulingEnvelope, error) {
		return schedulingEnvelope{}, errors.New("unavailable")
	})
	if !errors.Is(err, ErrLowPriorityEnvelope) || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("scheduling inspection failure was not closed: %v", err)
	}
	if _, err := safeCgroupPath(fixture.cgroup, "/"); err == nil {
		t.Fatal("cgroup root was accepted as a workload unit")
	}
	if path, err := safeCgroupPathAllowRoot(fixture.cgroup, "/"); err != nil || path != fixture.cgroup {
		t.Fatalf("container cgroup namespace root = %q, %v", path, err)
	}
	if path, err := unifiedCgroupPathAllowRoot("0::/\n"); err != nil || path != "/" {
		t.Fatalf("container unified root = %q, %v", path, err)
	}
}

func TestReservedLowUnitDetectionCannotUseEnvironmentOrContainerNames(t *testing.T) {
	exact := newEnvelopeFixture(t)
	reserved, err := currentProcessUsesReservedLowUnit(exact.proc, exact.pid)
	if err != nil || !reserved {
		t.Fatalf("exact reserved unit = %t, %v", reserved, err)
	}
	external := newExternalEnvelopeFixture(t)
	reserved, err = currentProcessUsesReservedLowUnit(external.proc, external.pid)
	if err != nil || reserved {
		t.Fatalf("container root reserved unit = %t, %v", reserved, err)
	}
	external.writeProc("cgroup", "0::/docker-rkc-low-pretend.scope\n")
	reserved, err = currentProcessUsesReservedLowUnit(external.proc, external.pid)
	if err != nil || reserved {
		t.Fatalf("non-reserved container-like name = %t, %v", reserved, err)
	}
}

func TestEnvelopeFailuresUseOnlyHermeticControlFixtures(t *testing.T) {
	schedule := func(int) (schedulingEnvelope, error) {
		return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
	}
	if err := requireProcessLowPriority("unused", "unused", 0, schedule); !errors.Is(err, ErrLowPriorityEnvelope) {
		t.Fatalf("invalid pid error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*envelopeFixture)
		want   string
	}{
		{"missing membership", func(f *envelopeFixture) {
			if err := os.Remove(filepath.Join(f.process, "cgroup")); err != nil {
				f.t.Fatal(err)
			}
		}, "read unified cgroup"},
		{"malformed membership", func(f *envelopeFixture) { f.writeProc("cgroup", "0::relative\n") }, "parse unified cgroup"},
		{"non-directory cgroup", func(f *envelopeFixture) {
			if err := os.RemoveAll(f.unit); err != nil {
				f.t.Fatal(err)
			}
			if err := os.WriteFile(f.unit, []byte("not a directory"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}, "cgroup directory"},
		{"missing cpu weight", func(f *envelopeFixture) {
			if err := os.Remove(filepath.Join(f.unit, "cpu.weight")); err != nil {
				f.t.Fatal(err)
			}
		}, "cpu.weight"},
		{"missing cpu max", func(f *envelopeFixture) {
			if err := os.Remove(filepath.Join(f.unit, "cpu.max")); err != nil {
				f.t.Fatal(err)
			}
		}, "read cpu.max"},
		{"unreadable io weight", func(f *envelopeFixture) {
			path := filepath.Join(f.unit, "io.weight")
			if err := os.Remove(path); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				f.t.Fatal(err)
			}
		}, "read io.weight"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEnvelopeFixture(t)
			test.mutate(fixture)
			err := requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, schedule)
			if !errors.Is(err, ErrLowPriorityEnvelope) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("envelope error = %v, want %q", err, test.want)
			}
		})
	}

	root := t.TempDir()
	oversized := filepath.Join(root, "oversized")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("1", maximumControlRead+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSmallControl(oversized); err == nil {
		t.Fatal("oversized cgroup control was accepted")
	}
	missing := filepath.Join(root, "missing")
	if _, err := readControlInteger(missing); err == nil {
		t.Fatal("missing integer control was accepted")
	}
	if err := requireControlInteger(root, "missing", 1); err == nil {
		t.Fatal("missing required integer control was accepted")
	}
}

type envelopeFixture struct {
	t        *testing.T
	proc     string
	cgroup   string
	pid      int
	process  string
	unit     string
	relative string
}

func newEnvelopeFixture(t *testing.T) *envelopeFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &envelopeFixture{
		t: t, proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup"), pid: 42,
		relative: "/user.slice/rkc-low-42.scope",
	}
	fixture.process = filepath.Join(fixture.proc, strconv.Itoa(fixture.pid))
	fixture.unit = filepath.Join(fixture.cgroup, "user.slice", "rkc-low-42.scope")
	for _, directory := range []string{fixture.process, fixture.unit} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.writeProc("cgroup", "0::"+fixture.relative+"\n")
	fixture.writeProc("oom_score_adj", strconv.Itoa(rkcOOMScoreAdjust)+"\n")
	fixture.writeControl("cpu.weight", "1\n")
	fixture.writeControl("cpu.max", "100000 100000\n")
	fixture.writeControl("cpu.max.burst", "0\n")
	fixture.writeControl("memory.high", strconv.FormatInt(rkcMemoryHighBytes, 10)+"\n")
	fixture.writeControl("memory.max", strconv.FormatInt(rkcMemoryMaxBytes, 10)+"\n")
	fixture.writeControl("memory.swap.max", strconv.FormatInt(rkcSwapMaxBytes, 10)+"\n")
	fixture.writeControl("pids.max", strconv.FormatInt(rkcTasksMax, 10)+"\n")
	fixture.writeControl("io.weight", "default 1\n")
	return fixture
}

func newExternalEnvelopeFixture(t *testing.T) *envelopeFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &envelopeFixture{
		t: t, proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup"), pid: 42,
		relative: "/",
	}
	fixture.process = filepath.Join(fixture.proc, strconv.Itoa(fixture.pid))
	fixture.unit = fixture.cgroup
	threadRoot := filepath.Join(fixture.process, "task", strconv.Itoa(fixture.pid))
	for _, directory := range []string{fixture.process, threadRoot, fixture.cgroup} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.writeProc("cgroup", "0::/\n")
	if err := os.WriteFile(filepath.Join(threadRoot, "cgroup"), []byte("0::/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.writeProc("oom_score_adj", strconv.Itoa(rkcOOMScoreAdjust)+"\n")
	fixture.writeControl("cpu.weight", "1\n")
	fixture.writeControl("cpu.max", "100000 100000\n")
	fixture.writeControl("cpu.max.burst", "0\n")
	fixture.writeControl("memory.high", "max\n")
	fixture.writeControl("memory.max", strconv.FormatInt(2560*1024*1024, 10)+"\n")
	fixture.writeControl("memory.current", strconv.FormatInt(64*1024*1024, 10)+"\n")
	fixture.writeControl("memory.swap.max", strconv.FormatInt(rkcSwapMaxBytes, 10)+"\n")
	fixture.writeControl("memory.swap.current", "0\n")
	fixture.writeControl("pids.max", strconv.FormatInt(rkcTasksMax, 10)+"\n")
	fixture.writeControl("pids.current", "4\n")
	fixture.writeControl("io.weight", "default 1\n")
	return fixture
}

func (fixture *envelopeFixture) writeProc(name, value string) {
	fixture.t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.process, name), []byte(value), 0o600); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *envelopeFixture) writeControl(name, value string) {
	fixture.t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.unit, name), []byte(value), 0o600); err != nil {
		fixture.t.Fatal(err)
	}
}
