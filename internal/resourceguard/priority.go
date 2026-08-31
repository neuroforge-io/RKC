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
	arguments   []string
	// cwdMarker retains only one fixed workload class, never the process's raw
	// working-directory path.
	cwdMarker string
}

// CheckHigherPriority enforces the strict shared-host policy: it refuses
// whenever any higher-priority workload is visible. It intentionally fails
// closed if Linux procfs cannot be enumerated, because silently starting a
// model is unsafe on a busy host. The default yield policy instead uses
// CheckHigherPriorityYield, which admits visible workloads whose aggregate CPU
// load stays below the configured threshold.
func CheckHigherPriority() error {
	markers, markerErr := HigherPriorityMarkersFromEnvironment()
	if markerErr != nil {
		return fmt.Errorf("%w: %v", ErrHigherPriorityActive, markerErr)
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	processes, err := procProcessSnapshots("/proc", markers)
	if err != nil {
		return fmt.Errorf("inspect higher-priority workloads: %w", err)
	}
	return checkHigherPriority(processes, os.Getpid(), markers)
}

func checkHigherPriority(processes []processSnapshot, self int, markers []string) error {
	conflicts := discoverHigherPriorityConflicts(processes, self, markers)
	if len(conflicts) == 0 {
		return nil
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].pid < conflicts[j].pid })
	diagnosticCount := len(conflicts)
	if diagnosticCount > maxHigherPriorityDiagnosticProcesses {
		diagnosticCount = maxHigherPriorityDiagnosticProcesses
	}
	details := make([]string, 0, diagnosticCount+1)
	for _, conflict := range conflicts[:diagnosticCount] {
		details = append(details, fmt.Sprintf("pid=%d marker=%s", conflict.pid, conflict.marker))
	}
	if remaining := len(conflicts) - diagnosticCount; remaining > 0 {
		details = append(details, fmt.Sprintf("+%d more processes", remaining))
	}
	return fmt.Errorf("%w: %s", ErrHigherPriorityActive, strings.Join(details, ", "))
}

// discoverHigherPriorityConflicts returns the visible higher-priority
// processes outside RKC's own ancestry, retaining only a fixed workload class
// per process rather than any raw argv or path.
func discoverHigherPriorityConflicts(processes []processSnapshot, self int, markers []string) []conflict {
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
	// Guarded workloads are descendants of the process performing the periodic
	// admission check. Exclude that complete subtree as well as ancestors: an
	// RKC child legitimately opening a marked repository must not classify its
	// own repository argument as a competing workload and kill itself.
	related := make(map[int]struct{}, len(ancestors)+1)
	for pid := range ancestors {
		related[pid] = struct{}{}
	}
	children := make(map[int][]int, len(processes))
	for _, process := range processes {
		children[process.parentPID] = append(children[process.parentPID], process.pid)
	}
	queue := []int{self}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if _, seen := related[pid]; !seen {
			related[pid] = struct{}{}
		}
		for _, child := range children[pid] {
			if _, seen := related[child]; seen {
				continue
			}
			related[child] = struct{}{}
			queue = append(queue, child)
		}
	}
	var conflicts []conflict
	for _, process := range processes {
		if _, ownProcess := related[process.pid]; ownProcess {
			continue
		}
		for _, marker := range markers {
			if processHasMarker(process, marker) {
				conflicts = append(conflicts, conflict{pid: process.pid, marker: marker})
				break
			}
		}
	}
	return conflicts
}

// expandHigherPriorityConflictDescendants returns every visible descendant of
// a directly classified higher-priority process. Worker processes frequently
// have generic executable names and arguments, so measuring only the launcher
// would undercount the workload that RKC must yield to. Direct classifications
// retain their own marker; otherwise descendants inherit the fixed marker class
// of the first sorted root that reaches them. Raw argv and paths are never
// copied into the result.
func expandHigherPriorityConflictDescendants(processes []processSnapshot, roots []conflict) []conflict {
	if len(roots) == 0 {
		return nil
	}
	sortedRoots := append([]conflict(nil), roots...)
	sort.Slice(sortedRoots, func(i, j int) bool {
		if sortedRoots[i].pid == sortedRoots[j].pid {
			return sortedRoots[i].marker < sortedRoots[j].marker
		}
		return sortedRoots[i].pid < sortedRoots[j].pid
	})
	byPID := make(map[int]conflict, len(sortedRoots))
	for _, root := range sortedRoots {
		if root.pid <= 0 {
			continue
		}
		if _, exists := byPID[root.pid]; !exists {
			byPID[root.pid] = root
		}
	}
	children := make(map[int][]int, len(processes))
	for _, process := range processes {
		if process.pid <= 0 || process.parentPID <= 0 || process.pid == process.parentPID {
			continue
		}
		children[process.parentPID] = append(children[process.parentPID], process.pid)
	}
	for parent := range children {
		sort.Ints(children[parent])
	}
	for _, root := range sortedRoots {
		if root.pid <= 0 {
			continue
		}
		queue := append([]int(nil), children[root.pid]...)
		visited := map[int]struct{}{root.pid: {}}
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			if _, seen := visited[pid]; seen {
				continue
			}
			visited[pid] = struct{}{}
			if _, directlyClassified := byPID[pid]; !directlyClassified {
				byPID[pid] = conflict{pid: pid, marker: root.marker}
			}
			queue = append(queue, children[pid]...)
		}
	}
	result := make([]conflict, 0, len(byPID))
	for _, item := range byPID {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].pid == result[j].pid {
			return result[i].marker < result[j].marker
		}
		return result[i].pid < result[j].pid
	})
	return result
}

func commandHasMarker(commandLine, marker string) bool {
	return commandArgumentsHaveMarker(strings.Fields(commandLine), marker)
}

func processHasMarker(process processSnapshot, marker string) bool {
	arguments := process.arguments
	if len(arguments) == 0 {
		arguments = strings.Fields(process.commandLine)
	}
	if commandArgumentsHaveMarker(arguments, marker) {
		return true
	}
	return process.cwdMarker == marker && interpreterHasExecutionTarget(arguments)
}

func interpreterHasExecutionTarget(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	switch interpreterKind(arguments[0]) {
	case "python":
		for index := 1; index < len(arguments); index++ {
			argument := arguments[index]
			switch {
			case argument == "--":
				return index+1 < len(arguments)
			case argument == "-c" || argument == "-m":
				return index+1 < len(arguments)
			case strings.HasPrefix(argument, "-c") && len(argument) > 2,
				strings.HasPrefix(argument, "-m") && len(argument) > 2:
				return true
			case argument == "-W" || argument == "-X" || argument == "-Q" || argument == "--check-hash-based-pycs":
				index++
			case strings.HasPrefix(argument, "-"):
				continue
			default:
				return true
			}
		}
	case "shell":
		for index := 1; index < len(arguments); index++ {
			argument := arguments[index]
			switch {
			case argument == "--":
				return index+1 < len(arguments)
			case argument == "-c" || strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") && strings.Contains(argument, "c"):
				return index+1 < len(arguments)
			case argument == "-O" || argument == "-o" || argument == "+O" || argument == "+o" || argument == "--init-file" || argument == "--rcfile":
				index++
			case strings.HasPrefix(argument, "-") || strings.HasPrefix(argument, "+"):
				continue
			default:
				return true
			}
		}
	}
	return false
}

func commandArgumentsHaveMarker(arguments []string, marker string) bool {
	if len(arguments) == 0 {
		return false
	}
	if pathHasMarker(arguments[0], marker) {
		return true
	}
	switch interpreterKind(arguments[0]) {
	case "python":
		return pythonTargetHasMarker(arguments[1:], marker)
	case "shell":
		return shellTargetHasMarker(arguments[1:], marker)
	default:
		// Never inspect arbitrary program arguments. Repository paths, model
		// prompts, cloud profiles, and questions can legitimately mention a
		// configured class without being that workload.
		return false
	}
}

func interpreterKind(executable string) string {
	base := strings.ToLower(filepath.Base(executable))
	if base == "python" || base == "python2" || base == "python3" || base == "pypy" || base == "pypy3" ||
		strings.HasPrefix(base, "python2.") || strings.HasPrefix(base, "python3.") {
		return "python"
	}
	switch base {
	case "sh", "bash", "dash", "ksh", "mksh", "zsh":
		return "shell"
	default:
		return ""
	}
}

func pythonTargetHasMarker(arguments []string, marker string) bool {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--":
			return index+1 < len(arguments) && pathHasMarker(arguments[index+1], marker)
		case argument == "-c" || strings.HasPrefix(argument, "-c") && len(argument) > 2:
			return false
		case argument == "-m":
			return index+1 < len(arguments) && pathHasMarker(arguments[index+1], marker)
		case strings.HasPrefix(argument, "-m") && len(argument) > 2:
			return pathHasMarker(strings.TrimPrefix(argument, "-m"), marker)
		case argument == "-W" || argument == "-X" || argument == "-Q" || argument == "--check-hash-based-pycs":
			index++
			continue
		case strings.HasPrefix(argument, "-"):
			continue
		default:
			return pathHasMarker(argument, marker)
		}
	}
	return false
}

func shellTargetHasMarker(arguments []string, marker string) bool {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--":
			return index+1 < len(arguments) && pathHasMarker(arguments[index+1], marker)
		case argument == "-c" || strings.Contains(argument, "c") && strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--"):
			return false
		case argument == "-O" || argument == "-o" || argument == "--init-file" || argument == "--rcfile":
			index++
			continue
		case argument == "+O" || argument == "+o":
			index++
			continue
		case strings.HasPrefix(argument, "-"):
			continue
		case strings.HasPrefix(argument, "+"):
			continue
		default:
			return pathHasMarker(argument, marker)
		}
	}
	return false
}

func pathHasMarker(value, marker string) bool {
	for _, token := range strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_')
	}) {
		if token == marker {
			return true
		}
	}
	return false
}

func procProcessSnapshots(root string, markers []string) ([]processSnapshot, error) {
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
		arguments := splitProcCommandLine(command)
		commandLine := strings.Join(arguments, " ")
		cwdMarker := ""
		if cwd, cwdErr := os.Readlink(filepath.Join(directory, "cwd")); cwdErr == nil {
			for _, marker := range markers {
				if pathHasMarker(cwd, marker) {
					cwdMarker = marker
					break
				}
			}
		}
		processes = append(processes, processSnapshot{pid: pid, parentPID: parentPID, commandLine: commandLine, arguments: arguments, cwdMarker: cwdMarker})
	}
	return processes, nil
}

func splitProcCommandLine(command []byte) []string {
	if len(command) == 0 {
		return nil
	}
	// Linux terminates /proc/<pid>/cmdline with one NUL in addition to the NUL
	// separators between argv entries. Remove exactly that terminator: trimming
	// all NULs would erase intentional empty interior or final arguments and can
	// shift interpreter option operands into the target position.
	value := string(command)
	if value[len(value)-1] == '\x00' {
		value = value[:len(value)-1]
	}
	return strings.Split(value, "\x00")
}

func parseParentPID(stat []byte) (int, error) {
	fields := procStatFields(stat)
	if len(fields) < 2 {
		return 0, errors.New("invalid proc stat fields")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil || parentPID < 0 {
		return 0, errors.New("invalid proc parent pid")
	}
	return parentPID, nil
}

// procStatFields splits /proc/<pid>/stat after the parenthesized command name,
// which is the only field that may contain arbitrary bytes. Field zero of the
// result is the process state; the kernel layout after the closing paren is
// fixed.
func procStatFields(stat []byte) []string {
	closing := strings.LastIndexByte(string(stat), ')')
	if closing < 0 || closing+1 >= len(stat) {
		return nil
	}
	return strings.Fields(string(stat[closing+1:]))
}

// processCPUTicks returns the cumulative user+system CPU ticks and the process
// start time from /proc/<pid>/stat. The start time lets callers detect PID
// reuse between samples.
func processCPUTicks(pid int) (ticks, starttime uint64, ok bool) {
	if pid <= 0 {
		return 0, 0, false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, false
	}
	fields := procStatFields(stat)
	if len(fields) < 20 {
		return 0, 0, false
	}
	utime, utimeErr := strconv.ParseUint(fields[11], 10, 64)
	stime, stimeErr := strconv.ParseUint(fields[12], 10, 64)
	start, startErr := strconv.ParseUint(fields[19], 10, 64)
	if utimeErr != nil || stimeErr != nil || startErr != nil {
		return 0, 0, false
	}
	return utime + stime, start, true
}

// systemCPUTicks returns the aggregate CPU ticks across every processor and
// the processor count from /proc/stat. Dividing process ticks by system ticks
// over the same window yields a clock-rate-independent one-core fraction.
func systemCPUTicks() (total uint64, ncpus int, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	first := data
	if newline := strings.IndexByte(string(data), '\n'); newline >= 0 {
		first = data[:newline]
	}
	fields := strings.Fields(string(first))
	if len(fields) < 2 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var sum uint64
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		sum += value
	}
	if sum == 0 {
		return 0, 0, false
	}
	ncpus = runtime.NumCPU()
	if ncpus <= 0 {
		ncpus = 1
	}
	return sum, ncpus, true
}
