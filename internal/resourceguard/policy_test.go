package resourceguard

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseHigherPriorityPolicy(t *testing.T) {
	for _, value := range []string{"refuse", "REFUSE", "  Refuse "} {
		policy, err := ParseHigherPriorityPolicy(value)
		if err != nil || policy != PolicyRefuse {
			t.Fatalf("ParseHigherPriorityPolicy(%q) = %v, %v; want PolicyRefuse", value, policy, err)
		}
	}
	for _, value := range []string{"yield", "YIELD", "  Yield "} {
		policy, err := ParseHigherPriorityPolicy(value)
		if err != nil || policy != PolicyYield {
			t.Fatalf("ParseHigherPriorityPolicy(%q) = %v, %v; want PolicyYield", value, policy, err)
		}
	}
	for _, value := range []string{"", "strict", "maybe", "refuse+yield"} {
		if _, err := ParseHigherPriorityPolicy(value); err == nil {
			t.Fatalf("ParseHigherPriorityPolicy(%q) unexpectedly succeeded", value)
		}
	}
}

func TestHigherPriorityPolicyFromEnvironment(t *testing.T) {
	t.Run("default is yield", func(t *testing.T) {
		t.Setenv(HigherPriorityPolicyEnvironment, "")
		if policy := HigherPriorityPolicyFromEnvironment(); policy != PolicyYield {
			t.Fatalf("policy = %v; want PolicyYield", policy)
		}
	})
	t.Run("explicit refuse", func(t *testing.T) {
		t.Setenv(HigherPriorityPolicyEnvironment, "refuse")
		if policy := HigherPriorityPolicyFromEnvironment(); policy != PolicyRefuse {
			t.Fatalf("policy = %v; want PolicyRefuse", policy)
		}
	})
	t.Run("explicit yield", func(t *testing.T) {
		t.Setenv(HigherPriorityPolicyEnvironment, "yield")
		if policy := HigherPriorityPolicyFromEnvironment(); policy != PolicyYield {
			t.Fatalf("policy = %v; want PolicyYield", policy)
		}
	})
	t.Run("invalid fails closed to refuse", func(t *testing.T) {
		t.Setenv(HigherPriorityPolicyEnvironment, "sometimes")
		if policy := HigherPriorityPolicyFromEnvironment(); policy != PolicyRefuse {
			t.Fatalf("policy = %v; want PolicyRefuse", policy)
		}
	})
}

func TestParseHigherPriorityMarkers(t *testing.T) {
	t.Run("compatibility default", func(t *testing.T) {
		t.Setenv(HigherPriorityMarkersEnvironment, "")
		markers, err := HigherPriorityMarkersFromEnvironment()
		if err != nil || strings.Join(markers, ",") != DefaultHigherPriorityMarkers {
			t.Fatalf("default markers = %v, %v", markers, err)
		}
	})
	t.Run("custom ordered classes", func(t *testing.T) {
		markers, err := ParseHigherPriorityMarkers("critical_train,batch2")
		if err != nil || strings.Join(markers, ",") != "critical_train,batch2" {
			t.Fatalf("custom markers = %v, %v", markers, err)
		}
	})
	invalid := []string{
		"",
		"critical_train,",
		",critical_train",
		"critical_train,,batch",
		"critical_train,critical_train",
		"CriticalTrain",
		"critical-train",
		"_critical",
		strings.Repeat("a", maxHigherPriorityMarkerLength+1),
		strings.Repeat("a", maxHigherPriorityMarkerBytes+1),
		strings.Repeat("a,", maxHigherPriorityMarkerCount) + "a",
	}
	for index, value := range invalid {
		t.Run(fmt.Sprintf("invalid-%d", index), func(t *testing.T) {
			if _, err := ParseHigherPriorityMarkers(value); err == nil {
				t.Fatalf("marker configuration %d unexpectedly succeeded", index)
			}
		})
	}
	t.Run("invalid value is private and fails closed", func(t *testing.T) {
		const sentinel = "SUPER_SECRET_MARKER_CONFIGURATION"
		t.Setenv(HigherPriorityMarkersEnvironment, sentinel)
		_, err := HigherPriorityMarkersFromEnvironment()
		if err == nil || strings.Contains(err.Error(), sentinel) {
			t.Fatalf("private invalid marker error = %v", err)
		}
		checkErr := CheckHigherPriority()
		if !errors.Is(checkErr, ErrHigherPriorityActive) || strings.Contains(checkErr.Error(), sentinel) {
			t.Fatalf("fail-closed marker check = %v", checkErr)
		}
	})
}

func TestLoadMaxFractionFromEnvironment(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(HigherPriorityLoadMaxEnvironment, "")
		threshold, err := LoadMaxFractionFromEnvironment()
		if err != nil || threshold != DefaultHigherPriorityLoadMaxFraction {
			t.Fatalf("threshold = %v, %v; want %v", threshold, err, DefaultHigherPriorityLoadMaxFraction)
		}
	})
	t.Run("valid", func(t *testing.T) {
		t.Setenv(HigherPriorityLoadMaxEnvironment, "0.25")
		threshold, err := LoadMaxFractionFromEnvironment()
		if err != nil || threshold != 0.25 {
			t.Fatalf("threshold = %v, %v; want 0.25", threshold, err)
		}
	})
	for _, value := range []string{"0", "-1", "17", "abc", "0.5.5", "NaN"} {
		t.Run("invalid "+value, func(t *testing.T) {
			t.Setenv(HigherPriorityLoadMaxEnvironment, value)
			if _, err := LoadMaxFractionFromEnvironment(); err == nil {
				t.Fatalf("threshold for %q unexpectedly succeeded", value)
			}
		})
	}
}

type fakeYieldProbe struct {
	processSnapshots  []processSnapshot
	processSequence   [][]processSnapshot
	processTicksByPID map[int]uint64
	starttimesByPID   map[int]uint64
	processCallsByPID map[int]int
	processFailAfter  map[int]int
	processDeltaByPID map[int]uint64
	processesErr      error
	processCalls      int
	total             uint64
	totalPerCall      uint64
	processDelta      uint64
	ncpus             int
	selfPID           int
	systemUnreadable  bool
	systemFailOnCall  int
	systemCalls       int
	sleeps            []time.Duration
}

func newFakeYieldProbe() *fakeYieldProbe {
	return &fakeYieldProbe{
		processTicksByPID: map[int]uint64{},
		starttimesByPID:   map[int]uint64{},
		processCallsByPID: map[int]int{},
		processFailAfter:  map[int]int{},
		processDeltaByPID: map[int]uint64{},
		ncpus:             4,
		selfPID:           999,
	}
}

func (probe *fakeYieldProbe) processes() ([]processSnapshot, error) {
	if probe.processesErr != nil {
		return nil, probe.processesErr
	}
	probe.processCalls++
	if len(probe.processSequence) > 0 {
		index := probe.processCalls - 1
		if index >= len(probe.processSequence) {
			index = len(probe.processSequence) - 1
		}
		return append([]processSnapshot(nil), probe.processSequence[index]...), nil
	}
	return append([]processSnapshot(nil), probe.processSnapshots...), nil
}

func (probe *fakeYieldProbe) processTicks(pid int) (uint64, uint64, bool) {
	probe.processCallsByPID[pid]++
	if maximum, configured := probe.processFailAfter[pid]; configured &&
		probe.processCallsByPID[pid] > maximum {
		return 0, 0, false
	}
	base, present := probe.processTicksByPID[pid]
	if !present {
		return 0, 0, false
	}
	delta := probe.processDelta
	if configured, ok := probe.processDeltaByPID[pid]; ok {
		delta = configured
	}
	probe.processTicksByPID[pid] = base + delta
	return probe.processTicksByPID[pid], probe.starttimesByPID[pid], true
}

func (probe *fakeYieldProbe) systemTicks() (uint64, int, bool) {
	if probe.systemUnreadable {
		return 0, 0, false
	}
	probe.systemCalls++
	if probe.systemFailOnCall > 0 && probe.systemCalls == probe.systemFailOnCall {
		return 0, 0, false
	}
	probe.total += probe.totalPerCall
	return probe.total, probe.ncpus, true
}

func (probe *fakeYieldProbe) sleep(duration time.Duration) {
	probe.sleeps = append(probe.sleeps, duration)
}

func (probe *fakeYieldProbe) checker(threshold float64) *yieldLoadChecker {
	return newYieldLoadChecker(yieldCheckProbe{
		processes:    probe.processes,
		processTicks: probe.processTicks,
		systemTicks:  probe.systemTicks,
		selfPID:      probe.selfPID,
		sleep:        probe.sleep,
	}, threshold, []string{"erais"})
}

func priorityConflictSnapshot() []processSnapshot {
	return []processSnapshot{
		{
			pid:         21,
			parentPID:   1,
			commandLine: "python /home/user/erais/train.py",
			arguments:   []string{"python", "/home/user/erais/train.py"},
		},
	}
}

func TestYieldLoadCheckerAdmitsIdleWorkloads(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 10 // 10% of one core: idle margin.
	checker := probe.checker(0.5)
	for attempt := 1; attempt <= 3; attempt++ {
		if err := checker.check(); err != nil {
			t.Fatalf("check %d rejected an idle higher-priority workload: %v", attempt, err)
		}
	}
	if len(probe.sleeps) != 1 {
		t.Fatalf("cold-start sampling happened %d times; want exactly 1", len(probe.sleeps))
	}
	if probe.sleeps[0] != loadSamplingWindow {
		t.Fatalf("cold-start window = %v; want %v", probe.sleeps[0], loadSamplingWindow)
	}
}

func TestYieldLoadCheckerRefusesBusyWorkloads(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 60 // 60% of one core: genuinely active.
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("check = %v; want ErrHigherPriorityActive", err)
	}
	message := err.Error()
	for _, expected := range []string{"60%", "50%", "pid=21", "marker=erais"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("refusal message %q does not contain %q", message, expected)
		}
	}
}

func TestYieldLoadCheckerMeasuresGenericWorkerDescendants(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = []processSnapshot{
		{
			pid:         21,
			parentPID:   1,
			commandLine: "python /home/user/erais/train.py",
			arguments:   []string{"python", "/home/user/erais/train.py"},
		},
		{pid: 22, parentPID: 21, commandLine: "worker", arguments: []string{"worker"}},
	}
	probe.processTicksByPID[21] = 1000
	probe.processTicksByPID[22] = 1000
	probe.starttimesByPID[21] = 500
	probe.starttimesByPID[22] = 501
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	// Each process consumes only 30% of one core, but the complete workload
	// consumes 60% and must preempt RKC at the 50% threshold.
	probe.processDelta = 30
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("check = %v; want descendant-inclusive ErrHigherPriorityActive", err)
	}
	for _, expected := range []string{"60%", "pid=21", "pid=22", "marker=erais"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("refusal message %q does not contain %q", err, expected)
		}
	}
}

func TestExpandHigherPriorityConflictDescendantsIsBoundedAndDeterministic(t *testing.T) {
	processes := []processSnapshot{
		{pid: 31, parentPID: 30},
		{pid: 30, parentPID: 31}, // malformed cycle
		{pid: 32, parentPID: 31},
		{pid: 40, parentPID: 30},
	}
	roots := []conflict{{pid: 31, marker: "erais"}, {pid: 40, marker: "torchrun"}}
	got := expandHigherPriorityConflictDescendants(processes, roots)
	want := []conflict{
		{pid: 30, marker: "erais"},
		{pid: 31, marker: "erais"},
		{pid: 32, marker: "erais"},
		{pid: 40, marker: "torchrun"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded conflicts = %#v, want %#v", got, want)
	}
}

func TestYieldLoadRefusalBoundsProcessDiagnostics(t *testing.T) {
	checker := &yieldLoadChecker{threshold: 0.5}
	conflicts := make([]conflict, maxHigherPriorityDiagnosticProcesses+3)
	for index := range conflicts {
		conflicts[index] = conflict{pid: index + 1, marker: "erais"}
	}
	err := checker.refusal(conflicts, 0.75)
	if !strings.Contains(err.Error(), "+3 more processes") {
		t.Fatalf("bounded refusal = %q", err)
	}
	if strings.Contains(err.Error(), fmt.Sprintf("pid=%d", len(conflicts))) {
		t.Fatalf("bounded refusal leaked diagnostics beyond its cap: %q", err)
	}
}

func TestYieldMeasurementRefusalIsBoundedAndDeterministic(t *testing.T) {
	checker := &yieldLoadChecker{threshold: 0.5}
	conflicts := make([]conflict, maxHigherPriorityDiagnosticProcesses+3)
	for index := range conflicts {
		conflicts[index] = conflict{pid: len(conflicts) - index, marker: "critical_train"}
	}
	first := checker.measurementRefusal(conflicts)
	slices.Reverse(conflicts)
	second := checker.measurementRefusal(conflicts)
	if first.Error() != second.Error() || !strings.Contains(first.Error(), "+3 more processes") {
		t.Fatalf("measurement diagnostics are not bounded and deterministic: %q versus %q", first, second)
	}
	if strings.Contains(first.Error(), fmt.Sprintf("pid=%d", len(conflicts))) {
		t.Fatalf("measurement diagnostics exceeded their process cap: %q", first)
	}
}

func TestYieldLoadCheckerRefusesAtExactThreshold(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 50 // Exactly 50% of one core: the threshold is exclusive.
	checker := probe.checker(0.5)
	if err := checker.check(); !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("check = %v; want ErrHigherPriorityActive at the exact threshold", err)
	}
}

func TestYieldLoadCheckerRefusesWhenBusyWithoutColdStart(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 10
	checker := probe.checker(0.5)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := checker.check(); err != nil {
			t.Fatalf("check %d rejected an idle workload: %v", attempt, err)
		}
	}
	probe.processDelta = 60
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("check after busy arrival = %v; want ErrHigherPriorityActive", err)
	}
}

func TestYieldLoadCheckerClearsBaselineWithoutConflicts(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 10
	checker := probe.checker(0.5)
	if err := checker.check(); err != nil {
		t.Fatalf("initial check rejected an idle workload: %v", err)
	}
	probe.processSnapshots = nil
	if err := checker.check(); err != nil {
		t.Fatalf("check without conflicts failed: %v", err)
	}
	// A returning workload cannot reuse a stale baseline: cold-start sampling
	// must run again.
	probe.processSnapshots = priorityConflictSnapshot()
	if err := checker.check(); err != nil {
		t.Fatalf("check after conflict return rejected an idle workload: %v", err)
	}
	if len(probe.sleeps) != 2 {
		t.Fatalf("cold-start sampling happened %d times; want 2", len(probe.sleeps))
	}
}

func TestYieldLoadCheckerResamplesAfterPIDChurn(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 10
	checker := probe.checker(0.5)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := checker.check(); err != nil {
			t.Fatalf("check %d rejected an idle workload: %v", attempt, err)
		}
	}
	// The process restarted under the same PID: its start time changed.
	probe.starttimesByPID[21] = 501
	probe.processDelta = 60
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("check after PID churn = %v; want ErrHigherPriorityActive", err)
	}
}

func TestYieldLoadCheckerFailsClosedWithoutSystemClock(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.systemUnreadable = true
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) || !strings.Contains(err.Error(), "CPU clock") {
		t.Fatalf("check = %v; want a fail-closed CPU-clock error", err)
	}
}

func TestYieldLoadCheckerRejectsUnconfiguredProbes(t *testing.T) {
	checker := newYieldLoadChecker(yieldCheckProbe{}, 0.5, strings.Split(DefaultHigherPriorityMarkers, ","))
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("check = %v; want a fail-closed configuration error", err)
	}
}

func TestYieldLoadCheckerRejectsInvalidThreshold(t *testing.T) {
	probe := newFakeYieldProbe()
	checker := probe.checker(0)
	if err := checker.check(); !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("check with zero threshold = %v; want ErrHigherPriorityActive", err)
	}
	checker = probe.checker(17)
	if err := checker.check(); !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("check with excessive threshold = %v; want ErrHigherPriorityActive", err)
	}
}

func TestYieldLoadCheckerExcludesOwnAncestry(t *testing.T) {
	probe := newFakeYieldProbe()
	// The checking process itself lives under /home/user/erais and its child
	// legitimately scans that repository; neither may classify itself.
	probe.processSnapshots = []processSnapshot{
		{pid: 999, parentPID: 20, commandLine: "rkc open /home/user/erais/repository", arguments: []string{"rkc", "open", "/home/user/erais/repository"}},
		{pid: 1000, parentPID: 999, commandLine: "rkc scan /home/user/erais/repository", arguments: []string{"rkc", "scan", "/home/user/erais/repository"}},
	}
	probe.processTicksByPID[999] = 1000
	probe.processTicksByPID[1000] = 1000
	probe.starttimesByPID[999] = 100
	probe.starttimesByPID[1000] = 100
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 10
	checker := probe.checker(0.5)
	if err := checker.check(); err != nil {
		t.Fatalf("own ancestry was classified as a competing workload: %v", err)
	}
	if len(probe.sleeps) != 0 {
		t.Fatalf("own ancestry triggered cold-start sampling %d times; want 0", len(probe.sleeps))
	}
}

func TestCurrentPriorityCheckSelectsPolicy(t *testing.T) {
	t.Run("refuse policy", func(t *testing.T) {
		t.Setenv(HigherPriorityPolicyEnvironment, "refuse")
		if check := CurrentPriorityCheck(); check == nil {
			t.Fatal("refuse policy produced no admission check")
		}
	})
	t.Run("yield policy", func(t *testing.T) {
		t.Setenv(HigherPriorityPolicyEnvironment, "yield")
		if check := CurrentPriorityCheck(); check == nil {
			t.Fatal("yield policy produced no admission check")
		}
	})
}

func TestCheckHigherPriorityYieldPackageEntry(t *testing.T) {
	t.Run("invalid configuration fails closed", func(t *testing.T) {
		t.Setenv(HigherPriorityLoadMaxEnvironment, "garbage")
		err := CheckHigherPriorityYield()
		if runtime.GOOS != "linux" {
			if err != nil {
				t.Fatalf("non-Linux entry failed: %v", err)
			}
			return
		}
		if !errors.Is(err, ErrHigherPriorityActive) {
			t.Fatalf("invalid load threshold = %v; want ErrHigherPriorityActive", err)
		}
	})
	t.Run("valid configuration admits or measures", func(t *testing.T) {
		t.Setenv(HigherPriorityLoadMaxEnvironment, "")
		err := CheckHigherPriorityYield()
		if runtime.GOOS != "linux" {
			if err != nil {
				t.Fatalf("non-Linux entry failed: %v", err)
			}
			return
		}
		// The package checker measures the real host: an idle or absent
		// workload is admitted, and a genuinely busy one refuses.
		if err != nil && !errors.Is(err, ErrHigherPriorityActive) {
			t.Fatalf("package yield check = %v; want nil or ErrHigherPriorityActive", err)
		}
	})
}

func TestYieldLoadCheckerFailsClosedOnProcessEnumeration(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processesErr = errors.New("procfs exploded")
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) && !strings.Contains(err.Error(), "inspect higher-priority workloads") {
		t.Fatalf("check = %v; want a fail-closed enumeration error", err)
	}
}

func TestYieldLoadCheckerFailsClosedOnSecondSampleClock(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 10
	// The first sample establishes the cold-start baseline; the second sample
	// discovers an unavailable system clock and must fail closed.
	probe.systemFailOnCall = 2
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) || !strings.Contains(err.Error(), "CPU clock") {
		t.Fatalf("check = %v; want a fail-closed CPU-clock error", err)
	}
}

func TestYieldLoadCheckerFailsClosedOnUnmeasurableExitedWorkloads(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	// The discovered process exits before any sample can be read.
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 60
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) ||
		!strings.Contains(err.Error(), "unmeasurable after one bounded retry") {
		t.Fatalf("exited workload check = %v; want a bounded fail-closed refusal", err)
	}
}

func TestYieldLoadCheckerFailsClosedOnZeroDeltaWindow(t *testing.T) {
	probe := newFakeYieldProbe()
	probe.processSnapshots = priorityConflictSnapshot()
	probe.processTicksByPID[21] = 1000
	probe.starttimesByPID[21] = 500
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 0
	checker := probe.checker(0.5)
	if err := checker.check(); err != nil {
		t.Fatalf("cold-start check failed: %v", err)
	}
	// The system clock stops advancing: the window remains unmeasurable after
	// one bounded retry and must fail closed.
	probe.totalPerCall = 0
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) ||
		!strings.Contains(err.Error(), "unmeasurable after one bounded retry") {
		t.Fatalf("zero-delta check = %v; want a bounded fail-closed refusal", err)
	}
	if len(probe.sleeps) != 2 {
		t.Fatalf("cold-start sampling happened %d times; want 2", len(probe.sleeps))
	}
}

func TestYieldLoadCheckerFailsClosedWhenDescendantExitsMidSample(t *testing.T) {
	probe := newFakeYieldProbe()
	root := processSnapshot{
		pid: 21, parentPID: 1,
		commandLine: "python /home/user/erais/train.py",
		arguments:   []string{"python", "/home/user/erais/train.py"},
	}
	worker := processSnapshot{
		pid: 22, parentPID: 21,
		commandLine: "private worker argv",
		arguments:   []string{"worker"},
	}
	probe.processSequence = [][]processSnapshot{{root, worker}, {root}}
	for pid, start := range map[int]uint64{21: 500, 22: 501} {
		probe.processTicksByPID[pid] = 1000
		probe.starttimesByPID[pid] = start
	}
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	probe.processDelta = 60
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("descendant-exit check = %v; want ErrHigherPriorityActive", err)
	}
	for _, expected := range []string{
		"unmeasurable after one bounded retry", "pid=21 marker=erais", "pid=22 marker=erais",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("bounded refusal %q does not contain %q", err, expected)
		}
	}
	if strings.Contains(err.Error(), "private worker argv") || len(probe.sleeps) != 1 ||
		probe.processCalls != 2 {
		t.Fatalf(
			"refusal leaked private process data or retried without a bound: %q calls=%d sleeps=%v",
			err,
			probe.processCalls,
			probe.sleeps,
		)
	}
}

func TestYieldLoadCheckerRediscoversWorkerSpawnedDuringSampling(t *testing.T) {
	probe := newFakeYieldProbe()
	root := processSnapshot{
		pid: 21, parentPID: 1,
		commandLine: "python /home/user/erais/train.py",
		arguments:   []string{"python", "/home/user/erais/train.py"},
	}
	worker := processSnapshot{
		pid: 22, parentPID: 21,
		commandLine: "private newly spawned worker argv",
		arguments:   []string{"worker"},
	}
	probe.processSequence = [][]processSnapshot{{root}, {root, worker}}
	for pid, start := range map[int]uint64{21: 500, 22: 501} {
		probe.processTicksByPID[pid] = 1000
		probe.starttimesByPID[pid] = start
	}
	probe.processDeltaByPID[21] = 0
	probe.processDeltaByPID[22] = 60 // The new worker alone exceeds the threshold.
	probe.totalPerCall = 100 * uint64(probe.ncpus)
	checker := probe.checker(0.5)
	err := checker.check()
	if !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("spawn-during-sample check = %v; want ErrHigherPriorityActive", err)
	}
	for _, expected := range []string{
		"unmeasurable after one bounded retry", "pid=21 marker=erais", "pid=22 marker=erais",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("spawn-during-sample refusal %q does not contain %q", err, expected)
		}
	}
	if probe.processCalls != 2 || len(probe.sleeps) != 1 ||
		strings.Contains(err.Error(), "private newly spawned worker argv") {
		t.Fatalf(
			"sampling refresh was unbounded or leaked private process data: calls=%d sleeps=%v err=%q",
			probe.processCalls,
			probe.sleeps,
			err,
		)
	}
}

func TestProcCPUTicksReadsRealProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs CPU ticks are Linux-only")
	}
	ticks, starttime, ok := processCPUTicks(os.Getpid())
	if !ok || starttime == 0 {
		t.Fatalf("processCPUTicks(self) = %d, %d, %v; want a readable process", ticks, starttime, ok)
	}
	if _, _, ok := processCPUTicks(0); ok {
		t.Fatal("processCPUTicks(0) unexpectedly succeeded")
	}
	if _, _, ok := processCPUTicks(1 << 30); ok {
		t.Fatal("processCPUTicks(nonexistent) unexpectedly succeeded")
	}
}

func TestSystemCPUTicksReadsHostClock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs system ticks are Linux-only")
	}
	total, ncpus, ok := systemCPUTicks()
	if !ok || total == 0 || ncpus <= 0 {
		t.Fatalf("systemCPUTicks = %d, %d, %v; want a readable host clock", total, ncpus, ok)
	}
}
