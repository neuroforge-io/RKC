//go:build linux

package resourceguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

const maximumThreadSchedulingPasses = 8

func currentSchedulingEnvelope(pid int) (schedulingEnvelope, error) {
	threadIDs, err := linuxThreadIDs("/proc", pid)
	if err != nil {
		return schedulingEnvelope{}, err
	}
	observed := schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}
	for _, threadID := range threadIDs {
		current, err := linuxThreadSchedulingEnvelope(threadID)
		if err != nil {
			return schedulingEnvelope{}, fmt.Errorf("inspect thread %d: %w", threadID, err)
		}
		if current.nice < -20 || current.nice > rkcNice || current.ioClass < 0 || current.ioClass > rkcIOClassIdle {
			return schedulingEnvelope{}, fmt.Errorf("thread %d scheduling state is invalid", threadID)
		}
		if current.nice < observed.nice {
			observed.nice = current.nice
		}
		if current.ioClass != rkcIOClassIdle {
			// Any non-idle thread means the process must be normalized. The exact
			// non-idle class is irrelevant because the only permitted transition is
			// monotonically down to idle for every thread.
			observed.ioClass = 0
		}
	}
	return observed, nil
}

func lowerCurrentSchedulingEnvelope(pid int) error {
	for attempt := 0; attempt < maximumThreadSchedulingPasses; attempt++ {
		before, err := linuxThreadIDs("/proc", pid)
		if err != nil {
			return err
		}
		changed := false
		for _, threadID := range before {
			if err := lowerLinuxThreadScheduling(threadID); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					changed = true
					continue
				}
				return fmt.Errorf("lower thread %d scheduling: %w", threadID, err)
			}
		}
		after, err := linuxThreadIDs("/proc", pid)
		if err != nil {
			return err
		}
		if !changed && reflect.DeepEqual(before, after) {
			return nil
		}
	}
	return errors.New("process thread set did not stabilize while lowering scheduling")
}

func linuxThreadIDs(procRoot string, pid int) ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(procRoot, strconv.Itoa(pid), "task"))
	if err != nil {
		return nil, fmt.Errorf("read process threads: %w", err)
	}
	threadIDs := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		threadID, err := strconv.Atoi(entry.Name())
		if err != nil || threadID <= 0 {
			return nil, fmt.Errorf("process thread entry %q is invalid", entry.Name())
		}
		threadIDs = append(threadIDs, threadID)
	}
	if len(threadIDs) == 0 {
		return nil, errors.New("process has no inspectable threads")
	}
	sort.Ints(threadIDs)
	return threadIDs, nil
}

func linuxThreadSchedulingEnvelope(threadID int) (schedulingEnvelope, error) {
	kernelPriority, err := syscall.Getpriority(syscall.PRIO_PROCESS, threadID)
	if err != nil {
		return schedulingEnvelope{}, err
	}
	priority, _, errno := syscall.Syscall(syscall.SYS_IOPRIO_GET, 1, uintptr(threadID), 0)
	if errno != 0 {
		return schedulingEnvelope{}, errno
	}
	// Linux exposes 20-nice through the raw getpriority syscall. syscall does
	// not apply libc's conversion, so normalize it before enforcing nice 19.
	return schedulingEnvelope{nice: 20 - kernelPriority, ioClass: int(priority >> 13)}, nil
}

func lowerLinuxThreadScheduling(threadID int) error {
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, threadID, rkcNice); err != nil {
		return err
	}
	priority := uintptr(rkcIOClassIdle << 13)
	_, _, errno := syscall.Syscall(syscall.SYS_IOPRIO_SET, 1, uintptr(threadID), priority)
	if errno != 0 {
		return errno
	}
	return nil
}

func currentEnvelopeFilesystems(processRoot, cgroupRoot string) error {
	for _, check := range []struct {
		path     string
		expected int64
		name     string
	}{
		{path: processRoot, expected: unix.PROC_SUPER_MAGIC, name: "procfs"},
		{path: cgroupRoot, expected: unix.CGROUP2_SUPER_MAGIC, name: "cgroup v2"},
	} {
		var info unix.Statfs_t
		if err := unix.Statfs(check.path, &info); err != nil {
			return fmt.Errorf("inspect %s filesystem: %w", check.name, err)
		}
		if int64(info.Type) != check.expected {
			return fmt.Errorf("%s is not backed by the expected kernel filesystem", check.name)
		}
	}
	return nil
}
