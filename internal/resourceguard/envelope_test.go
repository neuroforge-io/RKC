package resourceguard

import (
	"errors"
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
	for _, unit := range []string{"", "rkc-low-.scope", "rkc-low-name.scope", "rkc-low-12.slice", "other-12.scope"} {
		if validLowPriorityUnit(unit) {
			t.Fatalf("invalid low-priority unit was accepted: %q", unit)
		}
	}
	if !validLowPriorityUnit("rkc-low-12.scope") || !validLowPriorityUnit("rkc-low-99.service") {
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
	fixture.writeControl("memory.high", strconv.FormatInt(rkcMemoryHighBytes, 10)+"\n")
	fixture.writeControl("memory.max", strconv.FormatInt(rkcMemoryMaxBytes, 10)+"\n")
	fixture.writeControl("memory.swap.max", strconv.FormatInt(rkcSwapMaxBytes, 10)+"\n")
	fixture.writeControl("pids.max", strconv.FormatInt(rkcTasksMax, 10)+"\n")
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
