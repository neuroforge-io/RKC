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

var ErrLowPriorityEnvelope = errors.New("current process is outside the RKC low-priority envelope")

const (
	rkcMemoryHighBytes = int64(2 * 1024 * 1024 * 1024)
	rkcMemoryMaxBytes  = int64(2560 * 1024 * 1024)
	rkcSwapMaxBytes    = int64(256 * 1024 * 1024)
	rkcTasksMax        = int64(128)
	rkcNice            = 19
	rkcIOClassIdle     = 3
	rkcOOMScoreAdjust  = 750
	maximumControlRead = 4096
)

type schedulingEnvelope struct {
	nice    int
	ioClass int
}

// RequireCurrentProcessLowPriority proves that the calling process—not merely
// a future model child—is already inside the exact low-priority envelope made
// by scripts/with-rkc-limits.sh. Constructors call this before hashing large
// model assets so verification cannot compete with higher-priority work.
func RequireCurrentProcessLowPriority() error {
	return requireProcessLowPriority("/proc", "/sys/fs/cgroup", os.Getpid(), currentSchedulingEnvelope)
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
	cpuMax, err := readSmallControl(filepath.Join(cgroupPath, "cpu.max"))
	if err != nil {
		return fail("read cpu.max: %v", err)
	}
	cpuFields := strings.Fields(cpuMax)
	if len(cpuFields) != 2 || cpuFields[0] == "max" {
		return fail("cpu.max does not impose a one-core ceiling")
	}
	quota, quotaErr := strconv.ParseInt(cpuFields[0], 10, 64)
	period, periodErr := strconv.ParseInt(cpuFields[1], 10, 64)
	if quotaErr != nil || periodErr != nil || quota <= 0 || period <= 0 || quota > period {
		return fail("cpu.max does not impose a one-core ceiling")
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

func unifiedCgroupPath(record string) (string, error) {
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
	if clean != found || found == "/" {
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
	for _, character := range identifier {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func safeCgroupPath(root, relative string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(absoluteRoot, filepath.FromSlash(strings.TrimPrefix(relative, "/")))
	rel, err := filepath.Rel(absoluteRoot, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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
