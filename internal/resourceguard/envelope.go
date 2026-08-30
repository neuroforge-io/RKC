package resourceguard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrLowPriorityEnvelope is wrapped when RKC cannot prove that the current
// process is inside an admitted low-priority cgroup and scheduler envelope.
// Callers must treat it as failed admission before expensive work, not a warning.
var ErrLowPriorityEnvelope = errors.New("current process is outside the RKC low-priority envelope")

const (
	rkcMemoryHighBytes = int64(4 * 1024 * 1024 * 1024)
	// LowPriorityMemoryMaxBytes is the hard memory ceiling shared by the
	// installed first-run launcher, workbench, development guard, and model
	// runtime. Keep it aligned with scripts/with-rkc-limits.sh.
	LowPriorityMemoryMaxBytes = int64(4608 * 1024 * 1024)
	rkcMemoryMaxBytes         = LowPriorityMemoryMaxBytes
	rkcSwapMaxBytes           = int64(256 * 1024 * 1024)
	rkcTasksMax               = int64(128)
	rkcNice                   = 19
	rkcIOClassIdle            = 3
	rkcOOMScoreAdjust         = 750
	maximumControlRead        = 4096
)

type schedulingEnvelope struct {
	nice    int
	ioClass int
}

type externalEnvelopeDependencies struct {
	verifyFilesystems func(string, string) error
	inspectScheduling func(int) (schedulingEnvelope, error)
	lowerScheduling   func(int) error
}

// RequireCurrentProcessLowPriority proves that the calling process—not merely
// a future model child—is already inside the exact low-priority envelope made
// by the installed first-run launcher or scripts/with-rkc-limits.sh.
// Constructors call this before expensive work so verification cannot compete
// with higher-priority work.
func RequireCurrentProcessLowPriority() error {
	return requireProcessLowPriority("/proc", "/sys/fs/cgroup", os.Getpid(), currentSchedulingEnvelope)
}

// PrepareCurrentProcessLowPriority admits the current Linux process without a
// sibling transient unit only when kernel state already proves the fixed RKC
// hard-resource ceiling. An exact rkc-low unit retains
// RequireCurrentProcessLowPriority's stricter 4 GiB pressure plus 4.5 GiB
// hard-memory contract. The only external exception is a
// cgroup-namespaced container whose delegated root exposes every required hard
// limit and low weight; the process's per-thread nice and I/O priorities may
// then be monotonically lowered and are re-read before this function succeeds.
func PrepareCurrentProcessLowPriority() error {
	return prepareCurrentProcessLowPriorityUsing(
		RequireCurrentProcessLowPriority,
		func() (bool, error) { return currentProcessUsesReservedLowUnit("/proc", os.Getpid()) },
		func() error {
			return requireExternalProcessLowPriority(
				"/proc",
				"/sys/fs/cgroup",
				os.Getpid(),
				externalEnvelopeDependencies{
					verifyFilesystems: currentEnvelopeFilesystems,
					inspectScheduling: currentSchedulingEnvelope,
					lowerScheduling:   lowerCurrentSchedulingEnvelope,
				},
				true,
			)
		},
	)
}

// RequireCurrentProcessReusableLowPriority re-proves a previously prepared
// current-process envelope without changing scheduling state. Long-running
// callers use it to fail closed if externally managed cgroup controls drift.
func RequireCurrentProcessReusableLowPriority() error {
	return prepareCurrentProcessLowPriorityUsing(
		RequireCurrentProcessLowPriority,
		func() (bool, error) { return currentProcessUsesReservedLowUnit("/proc", os.Getpid()) },
		func() error {
			return requireExternalProcessLowPriority(
				"/proc",
				"/sys/fs/cgroup",
				os.Getpid(),
				externalEnvelopeDependencies{
					verifyFilesystems: currentEnvelopeFilesystems,
					inspectScheduling: currentSchedulingEnvelope,
				},
				false,
			)
		},
	)
}

func prepareCurrentProcessLowPriorityUsing(
	requireExact func() error,
	reservedUnit func() (bool, error),
	requireExternal func() error,
) error {
	if requireExact == nil || reservedUnit == nil || requireExternal == nil {
		return fmt.Errorf("%w: current-process envelope inspectors are not configured", ErrLowPriorityEnvelope)
	}
	exactErr := requireExact()
	if exactErr == nil {
		return nil
	}
	reserved, reservedErr := reservedUnit()
	if reservedErr != nil {
		return fmt.Errorf("%w: inspect reserved RKC unit after exact proof failed: %v", ErrLowPriorityEnvelope, reservedErr)
	}
	if reserved {
		// A unit bearing RKC's reserved name must satisfy the exact contract.
		// Never hide launcher or policy drift behind the more permissive external
		// container proof.
		return exactErr
	}
	externalErr := requireExternal()
	if externalErr == nil {
		return nil
	}
	return fmt.Errorf(
		"%w: exact RKC proof failed (%v); external constrained-cgroup proof failed (%v)",
		ErrLowPriorityEnvelope,
		exactErr,
		externalErr,
	)
}

func currentProcessUsesReservedLowUnit(procRoot string, pid int) (bool, error) {
	if pid <= 0 {
		return false, errors.New("process identity is invalid")
	}
	record, err := readSmallControl(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return false, err
	}
	relative, err := unifiedCgroupPathAllowRoot(record)
	if err != nil {
		return false, err
	}
	unit := filepath.Base(filepath.FromSlash(relative))
	return strings.HasPrefix(unit, "rkc-low-"), nil
}

func requireProcessLowPriority(procRoot, cgroupRoot string, pid int, scheduling func(int) (schedulingEnvelope, error)) error {
	fail := func(format string, arguments ...any) error {
		return fmt.Errorf("%w: %s", ErrLowPriorityEnvelope, fmt.Sprintf(format, arguments...))
	}
	if pid <= 0 || scheduling == nil {
		return fail("process identity or scheduling inspector is invalid")
	}
	processRoot := filepath.Join(procRoot, strconv.Itoa(pid))
	cgroupRecord, err := readSmallControl(filepath.Join(processRoot, "cgroup"))
	if err != nil {
		return fail("read unified cgroup membership: %v", err)
	}
	relative, err := unifiedCgroupPath(cgroupRecord)
	if err != nil {
		return fail("parse unified cgroup membership: %v", err)
	}
	unit := filepath.Base(filepath.FromSlash(relative))
	if !validLowPriorityUnit(unit) {
		return fail("cgroup unit %q is not an rkc-low scope or service", unit)
	}
	cgroupPath, err := safeCgroupPath(cgroupRoot, relative)
	if err != nil {
		return fail("resolve cgroup path: %v", err)
	}
	info, err := os.Lstat(cgroupPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fail("cgroup directory is unavailable or indirect: %v", err)
	}
	if err := requireControlInteger(cgroupPath, "cpu.weight", 1); err != nil {
		return fail("%v", err)
	}
	if err := requireCPUQuotaAtMostOne(cgroupPath); err != nil {
		return fail("%v", err)
	}
	for name, expected := range map[string]int64{
		"memory.high":     rkcMemoryHighBytes,
		"memory.max":      rkcMemoryMaxBytes,
		"memory.swap.max": rkcSwapMaxBytes,
		"pids.max":        rkcTasksMax,
	} {
		if err := requireControlInteger(cgroupPath, name, expected); err != nil {
			return fail("%v", err)
		}
	}
	ioWeight, err := readSmallControl(filepath.Join(cgroupPath, "io.weight"))
	if err == nil {
		fields := strings.Fields(ioWeight)
		if len(fields) != 2 || fields[0] != "default" || fields[1] != "1" {
			return fail("io.weight is not exactly default 1")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail("read io.weight: %v", err)
	}
	oomAdjustment, err := readControlInteger(filepath.Join(processRoot, "oom_score_adj"))
	if err != nil || oomAdjustment != rkcOOMScoreAdjust {
		return fail("OOM score adjustment is not %d", rkcOOMScoreAdjust)
	}
	observed, err := scheduling(pid)
	if err != nil {
		return fail("inspect process scheduling: %v", err)
	}
	if observed.nice != rkcNice {
		return fail("nice value is %d, expected %d", observed.nice, rkcNice)
	}
	if observed.ioClass != rkcIOClassIdle {
		return fail("I/O scheduling class is %d, expected idle class %d", observed.ioClass, rkcIOClassIdle)
	}
	return nil
}

func requireExternalProcessLowPriority(
	procRoot, cgroupRoot string,
	pid int,
	dependencies externalEnvelopeDependencies,
	allowSchedulingLowering bool,
) error {
	fail := func(format string, arguments ...any) error {
		return fmt.Errorf("%w: %s", ErrLowPriorityEnvelope, fmt.Sprintf(format, arguments...))
	}
	if pid <= 0 || dependencies.verifyFilesystems == nil || dependencies.inspectScheduling == nil {
		return fail("external container envelope inspectors are not configured")
	}
	if allowSchedulingLowering && dependencies.lowerScheduling == nil {
		return fail("external container scheduling normalizer is not configured")
	}
	processRoot := filepath.Join(procRoot, strconv.Itoa(pid))
	requireControls := func() (string, error) {
		cgroupRecord, err := readSmallControl(filepath.Join(processRoot, "cgroup"))
		if err != nil {
			return "", fail("read unified cgroup membership: %v", err)
		}
		relative, err := unifiedCgroupPathAllowRoot(cgroupRecord)
		if err != nil {
			return "", fail("parse unified cgroup membership: %v", err)
		}
		// This exception is deliberately narrow: a cgroup namespace presents its
		// delegated container root as 0::/. Non-root generic external cgroups are
		// not admitted because proving mount roots and inherited ancestor limits is
		// a materially broader policy.
		if relative != "/" {
			return "", fail("external reuse requires the cgroup namespace root, observed %q", relative)
		}
		if err := requireAllThreadsInCgroupNamespaceRoot(processRoot); err != nil {
			return "", fail("verify thread cgroup membership: %v", err)
		}
		cgroupPath, err := safeCgroupPathAllowRoot(cgroupRoot, relative)
		if err != nil {
			return "", fail("resolve cgroup namespace root: %v", err)
		}
		info, err := os.Lstat(cgroupPath)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fail("cgroup namespace root is unavailable or indirect: %v", err)
		}
		if err := dependencies.verifyFilesystems(processRoot, cgroupPath); err != nil {
			return "", fail("verify kernel control filesystems: %v", err)
		}
		if err := requireCPUQuotaAtMostOne(cgroupPath); err != nil {
			return "", fail("%v", err)
		}
		memoryMax, err := requireControlAtMostValue(cgroupPath, "memory.max", rkcMemoryMaxBytes)
		if err != nil {
			return "", fail("%v", err)
		}
		swapMax, err := requireControlAtMostValue(cgroupPath, "memory.swap.max", rkcSwapMaxBytes)
		if err != nil {
			return "", fail("%v", err)
		}
		pidsMax, err := requireControlAtMostValue(cgroupPath, "pids.max", rkcTasksMax)
		if err != nil {
			return "", fail("%v", err)
		}
		for _, usage := range []struct {
			name        string
			policyLimit int64
			actualLimit int64
		}{
			{name: "memory.current", policyLimit: rkcMemoryMaxBytes, actualLimit: memoryMax},
			{name: "memory.swap.current", policyLimit: rkcSwapMaxBytes, actualLimit: swapMax},
			{name: "pids.current", policyLimit: rkcTasksMax, actualLimit: pidsMax},
		} {
			if err := requireCurrentControlAtMostLimit(cgroupPath, usage.name, usage.policyLimit, usage.actualLimit); err != nil {
				return "", fail("%v", err)
			}
		}
		if err := requireControlAtMost(cgroupPath, "cpu.weight", 1); err != nil {
			return "", fail("%v", err)
		}
		if err := requireOptionalLowIOWeight(cgroupPath); err != nil {
			return "", fail("%v", err)
		}
		oomAdjustment, err := readControlInteger(filepath.Join(processRoot, "oom_score_adj"))
		if err != nil || oomAdjustment < rkcOOMScoreAdjust {
			return "", fail("OOM score adjustment is below %d", rkcOOMScoreAdjust)
		}
		return cgroupRecord, nil
	}

	initialMembership, err := requireControls()
	if err != nil {
		return err
	}
	observed, err := dependencies.inspectScheduling(pid)
	if err != nil {
		return fail("inspect process scheduling: %v", err)
	}
	if observed.nice == rkcNice && observed.ioClass == rkcIOClassIdle {
		return nil
	}
	if !allowSchedulingLowering {
		return fail(
			"process scheduling is nice %d / I/O class %d, expected nice %d / idle class %d",
			observed.nice,
			observed.ioClass,
			rkcNice,
			rkcIOClassIdle,
		)
	}
	if observed.nice < -20 || observed.nice > rkcNice || observed.ioClass < 0 || observed.ioClass > rkcIOClassIdle {
		return fail("process scheduling inspector returned an invalid state")
	}
	if err := dependencies.lowerScheduling(pid); err != nil {
		return fail("lower process scheduling: %v", err)
	}
	// Re-prove both the externally managed controls and scheduling after the
	// monotonic adjustment. This catches membership/control drift during setup.
	finalMembership, err := requireControls()
	if err != nil {
		return err
	}
	if finalMembership != initialMembership {
		return fail("cgroup membership changed while preparing the process")
	}
	observed, err = dependencies.inspectScheduling(pid)
	if err != nil {
		return fail("re-inspect process scheduling: %v", err)
	}
	if observed.nice != rkcNice || observed.ioClass != rkcIOClassIdle {
		return fail(
			"lowered process scheduling is nice %d / I/O class %d, expected nice %d / idle class %d",
			observed.nice,
			observed.ioClass,
			rkcNice,
			rkcIOClassIdle,
		)
	}
	return nil
}

func requireAllThreadsInCgroupNamespaceRoot(processRoot string) error {
	entries, err := os.ReadDir(filepath.Join(processRoot, "task"))
	if err != nil {
		return err
	}
	threads := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		threadID, parseErr := strconv.ParseInt(entry.Name(), 10, 64)
		if parseErr != nil || threadID <= 0 {
			return fmt.Errorf("thread entry %q is invalid", entry.Name())
		}
		record, readErr := readSmallControl(filepath.Join(processRoot, "task", entry.Name(), "cgroup"))
		if readErr != nil {
			return fmt.Errorf("read thread %s membership: %w", entry.Name(), readErr)
		}
		relative, parseErr := unifiedCgroupPathAllowRoot(record)
		if parseErr != nil || relative != "/" {
			return fmt.Errorf("thread %s is not in the cgroup namespace root", entry.Name())
		}
		threads++
	}
	if threads == 0 {
		return errors.New("process has no inspectable cgroup threads")
	}
	return nil
}

func requireCPUQuotaAtMostOne(root string) error {
	cpuMax, err := readSmallControl(filepath.Join(root, "cpu.max"))
	if err != nil {
		return fmt.Errorf("read cpu.max: %w", err)
	}
	fields := strings.Fields(cpuMax)
	if len(fields) != 2 || fields[0] == "max" {
		return errors.New("cpu.max does not impose a one-core ceiling")
	}
	quota, quotaErr := strconv.ParseInt(fields[0], 10, 64)
	period, periodErr := strconv.ParseInt(fields[1], 10, 64)
	if quotaErr != nil || periodErr != nil || quota <= 0 || period <= 0 || quota > period {
		return errors.New("cpu.max does not impose a one-core ceiling")
	}
	burst, err := readControlInteger(filepath.Join(root, "cpu.max.burst"))
	if err == nil && burst != 0 {
		return errors.New("cpu.max.burst is not zero")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read cpu.max.burst: %w", err)
	}
	return nil
}

func requireControlAtMost(root, name string, maximum int64) error {
	_, err := requireControlAtMostValue(root, name, maximum)
	return err
}

func requireControlAtMostValue(root, name string, maximum int64) (int64, error) {
	value, err := readControlInteger(filepath.Join(root, name))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", name, err)
	}
	if value > maximum {
		return 0, fmt.Errorf("%s is %d, exceeds %d", name, value, maximum)
	}
	return value, nil
}

func requireCurrentControlAtMostLimit(root, name string, policyLimit, actualLimit int64) error {
	value, err := requireControlAtMostValue(root, name, policyLimit)
	if err != nil {
		return err
	}
	if actualLimit == 0 {
		if value != 0 {
			return fmt.Errorf("%s is %d despite a zero limit", name, value)
		}
		return nil
	}
	if value > actualLimit {
		return fmt.Errorf("%s is %d, exceeds its hard limit %d", name, value, actualLimit)
	}
	return nil
}

func requireOptionalLowIOWeight(root string) error {
	value, err := readSmallControl(filepath.Join(root, "io.weight"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read io.weight: %w", err)
	}
	fields := strings.Fields(value)
	if len(fields) == 0 || len(fields)%2 != 0 {
		return errors.New("io.weight is malformed")
	}
	for index := 0; index < len(fields); index += 2 {
		weight, parseErr := strconv.ParseInt(fields[index+1], 10, 64)
		if parseErr != nil || weight < 1 || weight > 1 {
			return fmt.Errorf("io.weight entry %q is not low weight 1", fields[index])
		}
	}
	return nil
}

func unifiedCgroupPath(record string) (string, error) {
	return parseUnifiedCgroupPath(record, false)
}

func unifiedCgroupPathAllowRoot(record string) (string, error) {
	return parseUnifiedCgroupPath(record, true)
}

func parseUnifiedCgroupPath(record string, allowRoot bool) (string, error) {
	var found string
	for _, line := range strings.Split(strings.TrimSpace(record), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 3 || fields[0] != "0" || fields[1] != "" {
			continue
		}
		if found != "" {
			return "", errors.New("multiple unified cgroup records")
		}
		found = fields[2]
	}
	if found == "" || !strings.HasPrefix(found, "/") || strings.ContainsRune(found, '\x00') {
		return "", errors.New("missing or invalid unified cgroup path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(found)))
	if clean != found || (!allowRoot && found == "/") {
		return "", errors.New("unified cgroup path is not canonical")
	}
	return found, nil
}

func validLowPriorityUnit(unit string) bool {
	if !strings.HasPrefix(unit, "rkc-low-") {
		return false
	}
	identifier := strings.TrimPrefix(unit, "rkc-low-")
	identifier = strings.TrimSuffix(strings.TrimSuffix(identifier, ".scope"), ".service")
	if identifier == "" || (unit != "rkc-low-"+identifier+".scope" && unit != "rkc-low-"+identifier+".service") {
		return false
	}
	for _, segment := range strings.Split(identifier, "-") {
		if segment == "" {
			return false
		}
		for _, character := range segment {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func safeCgroupPath(root, relative string) (string, error) {
	return resolveSafeCgroupPath(root, relative, false)
}

func safeCgroupPathAllowRoot(root, relative string) (string, error) {
	return resolveSafeCgroupPath(root, relative, true)
}

func resolveSafeCgroupPath(root, relative string, allowRoot bool) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(absoluteRoot, filepath.FromSlash(strings.TrimPrefix(relative, "/")))
	rel, err := filepath.Rel(absoluteRoot, candidate)
	if err != nil || (!allowRoot && rel == ".") || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("cgroup path escapes its root")
	}
	return candidate, nil
}

func requireControlInteger(root, name string, expected int64) error {
	value, err := readControlInteger(filepath.Join(root, name))
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if value != expected {
		return fmt.Errorf("%s is %d, expected %d", name, value, expected)
	}
	return nil
}

func readControlInteger(path string) (int64, error) {
	value, err := readSmallControl(path)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("control value is not a non-negative integer")
	}
	return parsed, nil
}

func readSmallControl(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumControlRead+1))
	if err != nil {
		return "", err
	}
	if len(data) > maximumControlRead {
		return "", errors.New("control record exceeds size limit")
	}
	return strings.TrimSpace(string(data)), nil
}
