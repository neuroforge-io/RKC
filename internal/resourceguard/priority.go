package resourceguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type processSnapshot struct {
	pid         int
	parentPID   int
	commandLine string
}

// CheckHigherPriority enforces the shared-host policy used by RKC's guarded
// development wrapper. It intentionally fails closed if Linux procfs cannot be
// enumerated, because silently starting a model is unsafe on a busy host.
func CheckHigherPriority() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	processes, err := procProcessSnapshots("/proc")
	if err != nil {
		return fmt.Errorf("inspect higher-priority workloads: %w", err)
	}
	return checkHigherPriority(processes, os.Getpid())
}

func checkHigherPriority(processes []processSnapshot, self int) error {
	byPID := make(map[int]processSnapshot, len(processes))
	for _, process := range processes {
		byPID[process.pid] = process
	}
	ancestors := map[int]struct{}{}
	for pid := self; pid > 1; {
		if _, seen := ancestors[pid]; seen {
			break
		}
		ancestors[pid] = struct{}{}
		process, ok := byPID[pid]
		if !ok || process.parentPID <= 0 || process.parentPID == pid {
			break
		}
		pid = process.parentPID
	}
	type conflict struct {
		pid    int
		marker string
	}
	var conflicts []conflict
	for _, process := range processes {
		if _, ancestor := ancestors[process.pid]; ancestor {
			continue
		}
		for _, marker := range []string{"erais", "torchrun", "lm_eval"} {
			if commandHasMarker(process.commandLine, marker) {
				conflicts = append(conflicts, conflict{pid: process.pid, marker: marker})
				break
			}
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].pid < conflicts[j].pid })
	details := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		details = append(details, fmt.Sprintf("pid=%d marker=%s", conflict.pid, conflict.marker))
	}
	return fmt.Errorf("%w: %s", ErrHigherPriorityActive, strings.Join(details, ", "))
}

func commandHasMarker(commandLine, marker string) bool {
	for _, token := range strings.FieldsFunc(strings.ToLower(commandLine), func(character rune) bool {
		return !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_')
	}) {
		if token == marker {
			return true
		}
	}
	return false
}

func procProcessSnapshots(root string) ([]processSnapshot, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	processes := make([]processSnapshot, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		command, commandErr := os.ReadFile(filepath.Join(directory, "cmdline"))
		if commandErr != nil {
			if errors.Is(commandErr, os.ErrNotExist) || errors.Is(commandErr, os.ErrPermission) {
				continue
			}
			continue // Processes can exit while procfs is being enumerated.
		}
		stat, statErr := os.ReadFile(filepath.Join(directory, "stat"))
		if statErr != nil {
			continue
		}
		parentPID, err := parseParentPID(stat)
		if err != nil {
			continue
		}
		commandLine := strings.TrimSpace(strings.ReplaceAll(string(command), "\x00", " "))
		processes = append(processes, processSnapshot{pid: pid, parentPID: parentPID, commandLine: commandLine})
	}
	return processes, nil
}

func parseParentPID(stat []byte) (int, error) {
	closing := strings.LastIndexByte(string(stat), ')')
	if closing < 0 || closing+1 >= len(stat) {
		return 0, errors.New("invalid proc stat record")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	if len(fields) < 2 {
		return 0, errors.New("invalid proc stat fields")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil || parentPID < 0 {
		return 0, errors.New("invalid proc parent pid")
	}
	return parentPID, nil
}
