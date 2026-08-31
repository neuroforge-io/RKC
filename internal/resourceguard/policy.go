package resourceguard

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Higher-priority workload policy.
//
// RKC treats processes matching a bounded set of workload-class markers as
// explicitly higher-priority work on shared development hosts. The
// generic default protects torchrun and lm_eval workloads; operators can
// replace that set with RKC_HIGHER_PRIORITY_MARKERS. Two policies are supported:
//
//   - PolicyRefuse (strict, the historical behavior): RKC refuses to start
//     or continue whenever any higher-priority process is visible.
//   - PolicyYield (the default): RKC runs inside its exact low-priority
//     envelope (one core, minimum CPU/I/O weight, nice 19, idle I/O,
//     4.5 GiB hard memory, OOM-first) while higher-priority processes are
//     visible, and refuses or promptly cancels only when their aggregate CPU
//     load reaches the configured fraction of one core. An idle or lightly
//     active server therefore no longer blocks RKC, while genuine compute
//     work still preempts it.
const (
	// HigherPriorityPolicyEnvironment selects the policy: "refuse" or
	// "yield" (default). An explicitly invalid value fails closed to refuse.
	HigherPriorityPolicyEnvironment = "RKC_HIGHER_PRIORITY_POLICY"
	// HigherPriorityLoadMaxEnvironment sets the aggregate fraction of one CPU
	// core that visible higher-priority workloads may consume while RKC runs
	// under the yield policy (exclusive bound, 0 < value <= 16).
	HigherPriorityLoadMaxEnvironment = "RKC_HIGHER_PRIORITY_LOAD_MAX"
	// HigherPriorityMarkersEnvironment replaces the generic marker set.
	// Its value is a comma-separated list of lower-case ASCII marker classes.
	HigherPriorityMarkersEnvironment = "RKC_HIGHER_PRIORITY_MARKERS"
	// DefaultHigherPriorityMarkers is the generic marker set used when
	// RKC_HIGHER_PRIORITY_MARKERS is unset or empty. An empty environment value
	// intentionally cannot disable protection.
	DefaultHigherPriorityMarkers = "torchrun,lm_eval"
	// Higher-priority marker configuration is deliberately small so process
	// inspection and fallback regular expressions remain bounded.
	maxHigherPriorityMarkerBytes  = 255
	maxHigherPriorityMarkerCount  = 16
	maxHigherPriorityMarkerLength = 32
	// DefaultHigherPriorityLoadMaxFraction keeps a wide margin for idle
	// servers and short request bursts while still yielding to real compute.
	DefaultHigherPriorityLoadMaxFraction = 0.5
	// Keep refusal messages bounded even when a distributed workload owns a
	// large process tree. The load calculation still includes every process.
	maxHigherPriorityDiagnosticProcesses = 16
	// loadSamplingWindow is the bounded cold-start window used to measure a
	// higher-priority workload before admitting RKC work for the first time.
	loadSamplingWindow = 250 * time.Millisecond
)

// ParseHigherPriorityMarkers validates an explicit comma-separated marker
// configuration. Markers are canonical lower-case ASCII identifiers with an
// alphanumeric first byte and only alphanumerics or underscores thereafter.
// Empty entries, duplicates, and oversized values are rejected. Errors never
// echo the raw environment value.
func ParseHigherPriorityMarkers(value string) ([]string, error) {
	if value == "" {
		return nil, fmt.Errorf("%s must not be empty", HigherPriorityMarkersEnvironment)
	}
	if len(value) > maxHigherPriorityMarkerBytes {
		return nil, fmt.Errorf(
			"%s exceeds the %d-byte limit",
			HigherPriorityMarkersEnvironment,
			maxHigherPriorityMarkerBytes,
		)
	}
	parts := strings.Split(value, ",")
	if len(parts) > maxHigherPriorityMarkerCount {
		return nil, fmt.Errorf(
			"%s exceeds the %d-marker limit",
			HigherPriorityMarkersEnvironment,
			maxHigherPriorityMarkerCount,
		)
	}
	seen := make(map[string]struct{}, len(parts))
	for index, marker := range parts {
		position := index + 1
		if marker == "" {
			return nil, fmt.Errorf(
				"%s entry %d is empty",
				HigherPriorityMarkersEnvironment,
				position,
			)
		}
		if len(marker) > maxHigherPriorityMarkerLength {
			return nil, fmt.Errorf(
				"%s entry %d exceeds the %d-byte marker limit",
				HigherPriorityMarkersEnvironment,
				position,
				maxHigherPriorityMarkerLength,
			)
		}
		for byteIndex := 0; byteIndex < len(marker); byteIndex++ {
			character := marker[byteIndex]
			alphanumeric := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
			if (!alphanumeric && character != '_') || byteIndex == 0 && !alphanumeric {
				return nil, fmt.Errorf(
					"%s entry %d is not a lower-case ASCII marker",
					HigherPriorityMarkersEnvironment,
					position,
				)
			}
		}
		if _, duplicate := seen[marker]; duplicate {
			return nil, fmt.Errorf(
				"%s entry %d duplicates an earlier marker",
				HigherPriorityMarkersEnvironment,
				position,
			)
		}
		seen[marker] = struct{}{}
	}
	return parts, nil
}

// HigherPriorityMarkersFromEnvironment returns the validated marker classes.
// Unset and empty values retain the compatibility default so an empty variable
// cannot silently disable higher-priority workload protection.
func HigherPriorityMarkersFromEnvironment() ([]string, error) {
	value, present := os.LookupEnv(HigherPriorityMarkersEnvironment)
	if !present || value == "" {
		value = DefaultHigherPriorityMarkers
	}
	return ParseHigherPriorityMarkers(value)
}

// Policy controls how visible higher-priority workloads admit RKC work.
type Policy int

const (
	// PolicyUnset defers to the RKC_HIGHER_PRIORITY_POLICY environment value.
	PolicyUnset Policy = iota
	// PolicyRefuse fails closed whenever any higher-priority process is
	// visible, regardless of its current CPU load.
	PolicyRefuse
	// PolicyYield keeps RKC subordinate inside its low-priority envelope and
	// refuses or cancels only when visible higher-priority workloads consume
	// at least the configured fraction of one CPU core.
	PolicyYield
)

// ParseHigherPriorityPolicy converts an explicit policy name.
func ParseHigherPriorityPolicy(value string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "refuse":
		return PolicyRefuse, nil
	case "yield":
		return PolicyYield, nil
	default:
		return PolicyRefuse, fmt.Errorf(
			"%s must be \"refuse\" or \"yield\"",
			HigherPriorityPolicyEnvironment,
		)
	}
}

// HigherPriorityPolicyFromEnvironment returns the configured policy. The
// default is PolicyYield so that RKC remains usable on hosts where a
// higher-priority server is merely running; hosts that require the strict
// historical refusal set RKC_HIGHER_PRIORITY_POLICY=refuse. An explicitly
// invalid value fails closed to PolicyRefuse.
func HigherPriorityPolicyFromEnvironment() Policy {
	value, present := os.LookupEnv(HigherPriorityPolicyEnvironment)
	if !present || strings.TrimSpace(value) == "" {
		return PolicyYield
	}
	policy, err := ParseHigherPriorityPolicy(value)
	if err != nil {
		return PolicyRefuse
	}
	return policy
}

// LoadMaxFractionFromEnvironment returns the yield-policy CPU threshold. An
// explicitly invalid value fails closed.
func LoadMaxFractionFromEnvironment() (float64, error) {
	value, present := os.LookupEnv(HigherPriorityLoadMaxEnvironment)
	if !present || strings.TrimSpace(value) == "" {
		return DefaultHigherPriorityLoadMaxFraction, nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || parsed <= 0 || parsed > 16 {
		return 0, fmt.Errorf(
			"%s must be a number in (0, 16]",
			HigherPriorityLoadMaxEnvironment,
		)
	}
	return parsed, nil
}

// CurrentPriorityCheck returns the admission check selected by the configured
// higher-priority policy. Production admission and periodic monitors use this
// so that one policy governs every guarded path.
func CurrentPriorityCheck() func() error {
	if HigherPriorityPolicyFromEnvironment() == PolicyYield {
		return CheckHigherPriorityYield
	}
	return CheckHigherPriority
}

// CheckHigherPriorityYield admits RKC work while higher-priority workloads are
// visible only when their aggregate CPU consumption stays below the configured
// fraction of one core. It fails closed if procfs cannot be measured. A cold
// start takes one bounded sampling window so that busy work cannot slip
// through an unmeasured admission. A changed valid configuration replaces the
// rolling checker; malformed marker or load configuration always fails closed.
func CheckHigherPriorityYield() error {
	markers, markerErr := HigherPriorityMarkersFromEnvironment()
	if markerErr != nil {
		return fmt.Errorf("%w: %v", ErrHigherPriorityActive, markerErr)
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	threshold, err := LoadMaxFractionFromEnvironment()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHigherPriorityActive, err)
	}
	yieldCheckerMu.Lock()
	defer yieldCheckerMu.Unlock()
	markerKey := strings.Join(markers, ",")
	if yieldChecker == nil || yieldChecker.threshold != threshold || yieldChecker.markerKey != markerKey {
		yieldChecker = newYieldLoadChecker(defaultYieldCheckProbe(markers), threshold, markers)
	}
	return yieldChecker.check()
}

var (
	yieldCheckerMu sync.Mutex
	yieldChecker   *yieldLoadChecker
)

type cpuSample struct {
	ticks     uint64
	starttime uint64
}

type conflict struct {
	pid    int
	marker string
}

// yieldCheckProbe isolates the kernel and process-table interfaces behind the
// load-gated check so deterministic tests can inject fixed state.
type yieldCheckProbe struct {
	processes    func() ([]processSnapshot, error)
	processTicks func(pid int) (ticks, starttime uint64, ok bool)
	systemTicks  func() (total uint64, ncpus int, ok bool)
	selfPID      int
	sleep        func(time.Duration)
}

func defaultYieldCheckProbe(markers []string) yieldCheckProbe {
	configuredMarkers := append([]string(nil), markers...)
	return yieldCheckProbe{
		processes: func() ([]processSnapshot, error) {
			return procProcessSnapshots("/proc", configuredMarkers)
		},
		processTicks: processCPUTicks,
		systemTicks:  systemCPUTicks,
		selfPID:      os.Getpid(),
		sleep:        time.Sleep,
	}
}

type yieldLoadChecker struct {
	probe     yieldCheckProbe
	threshold float64
	markers   []string
	markerKey string
	mu        sync.Mutex
	baseline  map[int]cpuSample
	system    uint64
	ncpus     int
}

func newYieldLoadChecker(probe yieldCheckProbe, threshold float64, markers []string) *yieldLoadChecker {
	configuredMarkers := append([]string(nil), markers...)
	return &yieldLoadChecker{
		probe:     probe,
		threshold: threshold,
		markers:   configuredMarkers,
		markerKey: strings.Join(configuredMarkers, ","),
	}
}

func (checker *yieldLoadChecker) check() error {
	if checker == nil || checker.probe.processes == nil || checker.probe.processTicks == nil ||
		checker.probe.systemTicks == nil || checker.probe.sleep == nil {
		return fmt.Errorf("%w: yield-policy load inspector is not configured", ErrHigherPriorityActive)
	}
	if checker.threshold <= 0 || checker.threshold > 16 {
		return fmt.Errorf("%w: yield-policy load threshold %.2f is outside (0, 16]", ErrHigherPriorityActive, checker.threshold)
	}
	processes, err := checker.probe.processes()
	if err != nil {
		return fmt.Errorf("inspect higher-priority workloads: %w", err)
	}
	roots := discoverHigherPriorityConflicts(processes, checker.probe.selfPID, checker.markers)
	if len(roots) == 0 {
		checker.mu.Lock()
		checker.baseline = nil
		checker.system = 0
		checker.ncpus = 0
		checker.mu.Unlock()
		return nil
	}
	conflicts := expandHigherPriorityConflictDescendants(processes, roots)
	diagnosticConflicts := conflicts
	load, measured, err := checker.measure(conflicts)
	if err != nil {
		return err
	}
	if !measured {
		// Cold start or a new/changed conflict set: hold one bounded sampling
		// window before admitting so busy work cannot slip through unmeasured.
		checker.probe.sleep(loadSamplingWindow)
		refreshedProcesses, refreshErr := checker.probe.processes()
		if refreshErr != nil {
			return fmt.Errorf(
				"%w: re-inspect higher-priority workloads after sampling: %v",
				ErrHigherPriorityActive,
				refreshErr,
			)
		}
		refreshedRoots := discoverHigherPriorityConflicts(
			refreshedProcesses,
			checker.probe.selfPID,
			checker.markers,
		)
		conflicts = expandHigherPriorityConflictDescendants(refreshedProcesses, refreshedRoots)
		diagnosticConflicts = mergedConflictDiagnostics(diagnosticConflicts, conflicts)
		load, measured, err = checker.measure(conflicts)
		if err != nil {
			return err
		}
	}
	if !measured {
		// A visible root or descendant that exits, changes identity, or becomes
		// unreadable during the bounded window makes its recent CPU load
		// unknowable. Admitting here would let short-lived busy workers fail the
		// policy open. Refuse this check; the next periodic check performs fresh
		// process discovery and can admit once the workload is truly absent.
		return checker.measurementRefusal(diagnosticConflicts)
	}
	if load >= checker.threshold {
		return checker.refusal(conflicts, load)
	}
	return nil
}

// measure returns the aggregate one-core CPU fraction consumed by the conflict
// processes over the window ending now and refreshes the rolling baseline. It
// reports measured=false when no valid baseline exists yet (cold start, PID
// churn, or processes exiting between samples) and fails closed when the
// system CPU clock itself is unavailable.
func (checker *yieldLoadChecker) measure(conflicts []conflict) (float64, bool, error) {
	total, ncpus, systemOK := checker.probe.systemTicks()
	if !systemOK {
		return 0, false, fmt.Errorf("%w: cannot measure higher-priority CPU load: system CPU clock is unavailable", ErrHigherPriorityActive)
	}
	sample := make(map[int]cpuSample, len(conflicts))
	for _, item := range conflicts {
		ticks, starttime, ok := checker.probe.processTicks(item.pid)
		if !ok {
			continue // The process exited between discovery and sampling.
		}
		sample[item.pid] = cpuSample{ticks: ticks, starttime: starttime}
	}
	checker.mu.Lock()
	defer checker.mu.Unlock()
	valid := checker.baseline != nil && checker.system > 0 && total > checker.system &&
		checker.ncpus == ncpus && len(sample) == len(checker.baseline)
	if valid {
		for _, item := range conflicts {
			current, ok := sample[item.pid]
			if !ok {
				valid = false
				break
			}
			base, ok := checker.baseline[item.pid]
			if !ok || base.starttime != current.starttime {
				valid = false
				break
			}
		}
	}
	if !valid || len(sample) == 0 {
		checker.baseline = sample
		checker.system = total
		checker.ncpus = ncpus
		return 0, false, nil
	}
	// Compute the delta from the retained baseline before refreshing it, so a
	// rolling window spans exactly the interval between two checks.
	var aggregate uint64
	for _, item := range conflicts {
		current := sample[item.pid]
		base := checker.baseline[item.pid]
		if current.ticks > base.ticks {
			aggregate += current.ticks - base.ticks
		}
	}
	delta := total - checker.system
	checker.baseline = sample
	checker.system = total
	checker.ncpus = ncpus
	if delta == 0 {
		return 0, false, nil
	}
	return float64(aggregate) * float64(ncpus) / float64(delta), true, nil
}

func (checker *yieldLoadChecker) refusal(conflicts []conflict, load float64) error {
	details := boundedConflictDetails(conflicts)
	return fmt.Errorf(
		"%w: visible higher-priority workloads consume %.0f%% of one core, exceeding the %.0f%% yield threshold (%s)",
		ErrHigherPriorityActive,
		load*100,
		checker.threshold*100,
		strings.Join(details, ", "),
	)
}

func (checker *yieldLoadChecker) measurementRefusal(conflicts []conflict) error {
	return fmt.Errorf(
		"%w: visible higher-priority workload CPU load remained unmeasurable after one bounded retry (%s)",
		ErrHigherPriorityActive,
		strings.Join(boundedConflictDetails(conflicts), ", "),
	)
}

func boundedConflictDetails(conflicts []conflict) []string {
	ordered := append([]conflict(nil), conflicts...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].pid == ordered[j].pid {
			return ordered[i].marker < ordered[j].marker
		}
		return ordered[i].pid < ordered[j].pid
	})
	diagnosticCount := len(ordered)
	if diagnosticCount > maxHigherPriorityDiagnosticProcesses {
		diagnosticCount = maxHigherPriorityDiagnosticProcesses
	}
	details := make([]string, 0, diagnosticCount+1)
	for _, item := range ordered[:diagnosticCount] {
		details = append(details, fmt.Sprintf("pid=%d marker=%s", item.pid, item.marker))
	}
	if remaining := len(ordered) - diagnosticCount; remaining > 0 {
		details = append(details, fmt.Sprintf("+%d more processes", remaining))
	}
	return details
}

func mergedConflictDiagnostics(left, right []conflict) []conflict {
	byPID := make(map[int]string, len(left)+len(right))
	for _, collection := range [][]conflict{left, right} {
		for _, item := range collection {
			if item.pid <= 0 || item.marker == "" {
				continue
			}
			marker, exists := byPID[item.pid]
			if !exists || item.marker < marker {
				byPID[item.pid] = item.marker
			}
		}
	}
	result := make([]conflict, 0, len(byPID))
	for pid, marker := range byPID {
		result = append(result, conflict{pid: pid, marker: marker})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].pid == result[j].pid {
			return result[i].marker < result[j].marker
		}
		return result[i].pid < result[j].pid
	})
	return result
}
