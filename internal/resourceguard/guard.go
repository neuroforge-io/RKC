// Package resourceguard runs RKC first-run, workbench, and local-model
// processes as strictly subordinate workloads. On Linux it uses a transient
// user-systemd service plus kernel scheduling and memory controls; unsupported
// platforms fail closed unless a caller makes the explicit test-only unsafe
// opt-in.
package resourceguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrRSSLimitExceeded identifies a process stopped after its observed RSS
	// exceeded the configured hard limit.
	ErrRSSLimitExceeded = errors.New("model process exceeded its RSS limit")
	// ErrHigherPriorityActive is returned before process spawn whenever an ERAIS
	// training or evaluation workload is visible outside RKC's own ancestry.
	ErrHigherPriorityActive         = errors.New("higher-priority ERAIS work is active")
	errLauncherExitedWithActiveUnit = errors.New("model launcher exited while its guarded unit was still active")
)

const (
	unitQuiescenceWindow = 150 * time.Millisecond
	unitQuiescencePoll   = 25 * time.Millisecond
)

// Config describes one guarded subprocess. Executable should already be an
// exact, validated path when a caller needs artifact identity guarantees.
type Config struct {
	Executable          string
	Arguments           []string
	Environment         []string
	MaximumRSSBytes     int64
	UnitPrefix          string
	UnsafeDisableCgroup bool
	// UnsafeDisablePriorityCheck is test-only and is accepted only together
	// with UnsafeDisableCgroup. Production subprocesses always yield to ERAIS.
	UnsafeDisablePriorityCheck bool
}

// Command is a single-use guarded subprocess.
type Command struct {
	cmd             *exec.Cmd
	unit            string
	controlGroup    string
	priorityCheck   func() error
	maximumRSSBytes int64
}

// NewCommand constructs but does not start a guarded command.
func NewCommand(ctx context.Context, config Config) (*Command, error) {
	priorityCheck := CheckHigherPriority
	if config.UnsafeDisablePriorityCheck {
		priorityCheck = func() error { return nil }
	}
	return newCommand(ctx, config, priorityCheck)
}

func newCommand(ctx context.Context, config Config, priorityCheck func() error) (*Command, error) {
	if ctx == nil {
		return nil, errors.New("resource guard context is required")
	}
	if strings.TrimSpace(config.Executable) == "" {
		return nil, errors.New("guarded executable is required")
	}
	if err := validateEnvironment(config.Environment); err != nil {
		return nil, err
	}
	if config.MaximumRSSBytes < 0 {
		return nil, errors.New("model RSS limit must not be negative")
	}
	if config.UnsafeDisablePriorityCheck && !config.UnsafeDisableCgroup {
		return nil, errors.New("priority checks may be disabled only with the test-only cgroup bypass")
	}
	memoryMax := config.MaximumRSSBytes
	if memoryMax == 0 {
		memoryMax = 4608 * 1024 * 1024
	}
	if memoryMax < 64*1024*1024 || memoryMax > 64*1024*1024*1024 {
		return nil, errors.New("model RSS limit must be between 64 MiB and 64 GiB")
	}
	if priorityCheck == nil {
		priorityCheck = CheckHigherPriority
	}
	if runtime.GOOS != "linux" && !config.UnsafeDisableCgroup {
		return nil, errors.New("model cgroup resource guard is unsupported on this platform")
	}
	if config.UnsafeDisableCgroup {
		command := exec.CommandContext(ctx, config.Executable, config.Arguments...)
		command.Env = append([]string(nil), config.Environment...)
		return &Command{cmd: command, priorityCheck: priorityCheck, maximumRSSBytes: memoryMax}, nil
	}
	for _, executable := range []string{"systemd-run", "systemctl", "choom", "ionice", "nice", "env"} {
		if _, err := exec.LookPath(executable); err != nil {
			return nil, fmt.Errorf("required model resource guard command %q is unavailable: %w", executable, err)
		}
	}
	// Keep the default 4.5 GiB model ceiling aligned with the outer
	// development envelope's exact 4 GiB pressure threshold. Smaller explicit
	// limits retain the same ratio, while swap remains a fixed, narrow escape
	// hatch rather than scaling with model size.
	memoryHigh := memoryMax * 8 / 9
	swapMax := int64(256 * 1024 * 1024)
	prefix := config.UnitPrefix
	if prefix == "" {
		prefix = "rkc-model"
	}
	if !validUnitPrefix(prefix) {
		return nil, errors.New("resource guard unit prefix contains unsupported characters")
	}
	unit := fmt.Sprintf("%s-%d-%d.service", prefix, os.Getpid(), time.Now().UnixNano())
	// Receipt-bound executable and model arguments are opaque bytes. Prevent
	// systemd-run from treating dollar-prefixed substrings as environment
	// references and silently changing the command after validation.
	arguments := []string{
		// Preserve the caller's working directory so argument-vector paths retain
		// their exact meaning when an installed RKC command self-reexecutes. The
		// sanitized environment and explicit executable still prevent ambient
		// shell startup or PATH rewriting inside the service.
		"--user", "--wait", "--pipe", "--collect", "--quiet", "--same-dir", "--expand-environment=no", "--service-type=exec", "--unit", unit,
		"--property", "CPUWeight=1",
		"--property", "IOWeight=1",
		"--property", "CPUQuota=100%",
		"--property", "MemoryHigh=" + strconv.FormatInt(memoryHigh, 10),
		"--property", "MemoryMax=" + strconv.FormatInt(memoryMax, 10),
		"--property", "MemorySwapMax=" + strconv.FormatInt(swapMax, 10),
		"--property", "TasksMax=128",
		"--property", "OOMPolicy=stop",
		"--property", "KillMode=control-group",
		"--property", "TimeoutStopSec=2s",
		"--",
		"choom", "-n", "750", "--", "ionice", "-c", "3", "nice", "-n", "19", "env", "-i",
	}
	arguments = append(arguments, config.Environment...)
	arguments = append(arguments, config.Executable)
	arguments = append(arguments, config.Arguments...)
	// Run owns the complete transient-service lifecycle. Binding the launcher to
	// the construction context would let os/exec kill and reap systemd-run before
	// Run can terminate a still-active service and prove its cgroup inactive.
	command := exec.Command("systemd-run", arguments...)
	command.Env = ResourceGuardEnvironment()
	return &Command{cmd: command, unit: unit, priorityCheck: priorityCheck, maximumRSSBytes: memoryMax}, nil
}

// Run checks the host-priority policy immediately before spawn, then executes
// and observes the complete transient service until it is proven inactive.
func (command *Command) Run(ctx context.Context, stdout, stderr io.Writer) (int64, error) {
	if command == nil || command.cmd == nil {
		return 0, errors.New("model command is not configured")
	}
	if ctx == nil {
		return 0, errors.New("resource guard context is required")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if command.priorityCheck != nil {
		if err := command.priorityCheck(); err != nil {
			return 0, err
		}
	}
	command.cmd.Stdout = stdout
	command.cmd.Stderr = stderr
	// The priority check can be non-trivial and cancellation can arrive while it
	// runs. Never spawn after that cancellation has already become observable.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := command.cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- command.cmd.Wait() }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var peak int64
	lastPriorityCheck := time.Now()
	for {
		select {
		case err := <-done:
			if value := command.observedRSS(); value > peak {
				peak = value
			}
			cleanedActiveUnit, cleanupErr := command.cleanupUnitAfterLauncherExit()
			if ctx.Err() != nil && cleanupErr == nil {
				return peak, ctx.Err()
			}
			if cleanupErr != nil {
				return peak, errors.Join(err, ctx.Err(), cleanupErr)
			}
			if cleanedActiveUnit {
				err = errors.Join(err, errLauncherExitedWithActiveUnit)
			}
			return peak, err
		case <-ticker.C:
			value := command.observedRSS()
			if value > peak {
				peak = value
			}
			if value > command.maximumRSSBytes {
				limitErr := fmt.Errorf("%w: observed RSS %d exceeds limit %d", ErrRSSLimitExceeded, value, command.maximumRSSBytes)
				return peak, errors.Join(limitErr, command.stop(done))
			}
			// A higher-priority workload can start after this command passes its
			// admission check. Recheck during long-lived model processes and tear
			// down the complete unit promptly when that happens.
			if command.priorityCheck != nil && time.Since(lastPriorityCheck) >= time.Second {
				lastPriorityCheck = time.Now()
				if err := command.priorityCheck(); err != nil {
					return peak, errors.Join(err, command.stop(done))
				}
			}
		case <-ctx.Done():
			if stopErr := command.stop(done); stopErr != nil {
				return peak, errors.Join(ctx.Err(), stopErr)
			}
			return peak, ctx.Err()
		}
	}
}

func (command *Command) observedRSS() int64 {
	value := ProcessRSS(command.cmd.Process.Pid)
	if command.unit == "" {
		return value
	}
	if command.controlGroup == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		show := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=ControlGroup", "--value", command.unit)
		show.Env = ResourceGuardEnvironment()
		output, err := show.Output()
		cancel()
		candidate := strings.TrimSpace(string(output))
		if err == nil && strings.HasPrefix(candidate, "/") && !strings.Contains(candidate, "..") {
			command.controlGroup = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(candidate, "/"))
		}
	}
	if command.controlGroup == "" {
		return value
	}
	for _, name := range []string{"memory.current", "memory.peak"} {
		data, err := os.ReadFile(filepath.Join(command.controlGroup, name))
		if err != nil {
			continue
		}
		observed, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err == nil && observed > value {
			value = observed
		}
	}
	return value
}

func (command *Command) stop(done <-chan error) error {
	termUnitErr := command.signalUnit(syscall.SIGTERM)
	termLauncherErr := command.signalLauncher(syscall.SIGTERM)
	launcherDone := false
	timer := time.NewTimer(250 * time.Millisecond)
	select {
	case <-done:
		launcherDone = true
	case <-timer.C:
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}

	var preReapUnitErr error
	if unitErr := command.waitUnitInactive(250 * time.Millisecond); unitErr != nil {
		killUnitErr := command.signalUnit(syscall.SIGKILL)
		if inactiveErr := command.waitUnitInactive(2 * time.Second); inactiveErr != nil {
			preReapUnitErr = errors.Join(
				wrapSignalError("terminate model unit", termUnitErr),
				wrapSignalError("kill model unit", killUnitErr),
				inactiveErr,
			)
		}
	}
	if !launcherDone {
		// Killing the unit normally releases systemd-run --wait. Give that path a
		// brief chance to reap before forcibly killing the launcher itself.
		select {
		case <-done:
			launcherDone = true
		case <-time.After(250 * time.Millisecond):
		}
	}
	var failures []error
	if !launcherDone {
		killLauncherErr := command.signalLauncher(syscall.SIGKILL)
		select {
		case <-done:
			launcherDone = true
		case <-time.After(2 * time.Second):
			failures = append(failures,
				wrapSignalError("terminate model launcher", termLauncherErr),
				wrapSignalError("kill model launcher", killLauncherErr),
				errors.New("model launcher did not reap after SIGKILL"),
			)
		}
	}
	// systemd-run can finish registering a transient unit while its own process
	// is being terminated. A pre-reap "inactive" result is therefore not a
	// lifecycle proof. Once the launcher is definitively reaped, recheck and
	// close any unit that appeared during that window. If reaping failed this is
	// still a best-effort cleanup, while the launcher failure remains explicit.
	_, finalUnitErr := command.cleanupUnitAfterLauncherExit()
	if finalUnitErr != nil {
		failures = append(failures, preReapUnitErr, finalUnitErr)
	}
	return errors.Join(failures...)
}

// cleanupUnitAfterLauncherExit closes the otherwise-dangerous gap where the
// systemd-run client exits while its transient service remains alive. The
// boolean reports that cleanup was required; a nil error proves the unit is
// inactive after bounded TERM/KILL escalation.
func (command *Command) cleanupUnitAfterLauncherExit() (bool, error) {
	if command == nil || command.unit == "" {
		return false, nil
	}
	// A single inactive/unknown response is not sufficient after systemd-run
	// exits: the manager can publish an already-delivered transient-unit request
	// just after that observation. Require repeated settled observations across a
	// short window before declaring the lifecycle closed.
	if err := command.waitUnitQuiescent(unitQuiescenceWindow); err == nil {
		return false, nil
	}
	termErr := command.signalUnit(syscall.SIGTERM)
	if err := command.waitUnitInactive(250 * time.Millisecond); err == nil {
		if quietErr := command.waitUnitQuiescent(unitQuiescenceWindow); quietErr == nil {
			return true, nil
		}
	}
	var failures []error
	for attempt := 1; attempt <= 2; attempt++ {
		killErr := command.signalUnit(syscall.SIGKILL)
		inactiveErr := command.waitUnitInactive(2 * time.Second)
		if inactiveErr == nil {
			if quietErr := command.waitUnitQuiescent(unitQuiescenceWindow); quietErr == nil {
				return true, nil
			} else {
				inactiveErr = quietErr
			}
		}
		failures = append(failures,
			wrapSignalError(fmt.Sprintf("kill model unit after launcher exit (attempt %d)", attempt), killErr),
			inactiveErr,
		)
	}
	return true, errors.Join(
		wrapSignalError("terminate model unit after launcher exit", termErr),
		errors.Join(failures...),
	)
}

func (command *Command) waitUnitQuiescent(window time.Duration) error {
	if command == nil || command.unit == "" {
		return nil
	}
	if window <= 0 {
		return errors.New("model unit quiescence window must be positive")
	}
	deadline := time.Now().Add(window)
	observations := 0
	for {
		if err := command.waitUnitInactive(unitQuiescencePoll); err != nil {
			return fmt.Errorf("prove model unit quiescent after %d settled observations: %w", observations, err)
		}
		observations++
		remaining := time.Until(deadline)
		if remaining <= 0 && observations >= 3 {
			return nil
		}
		delay := unitQuiescencePoll
		if remaining > 0 && remaining < delay {
			delay = remaining
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

func wrapSignalError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (command *Command) waitUnitInactive(timeout time.Duration) error {
	if command == nil || command.unit == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		show := exec.CommandContext(ctx, "systemctl", "--user", "is-active", command.unit)
		show.Env = ResourceGuardEnvironment()
		output, err := show.Output()
		state := strings.TrimSpace(string(output))
		switch state {
		case "inactive", "failed", "dead", "unknown":
			return nil
		case "active", "activating", "deactivating", "reloading":
		default:
			if err != nil {
				return fmt.Errorf("prove model unit inactive: %w", err)
			}
			return fmt.Errorf("prove model unit inactive: unexpected state %q", state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("prove model unit inactive: %w (last state %q)", ctx.Err(), state)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (command *Command) signalUnit(signal syscall.Signal) error {
	if command == nil || command.unit == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	name := "SIGTERM"
	if signal == syscall.SIGKILL {
		name = "SIGKILL"
	}
	stop := exec.CommandContext(ctx, "systemctl", "--user", "kill", "--kill-whom=all", "--signal="+name, command.unit)
	stop.Env = ResourceGuardEnvironment()
	return stop.Run()
}

func (command *Command) signalLauncher(signal syscall.Signal) error {
	if command == nil || command.cmd == nil || command.cmd.Process == nil {
		return nil
	}
	err := command.cmd.Process.Signal(signal)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// ProcessRSS returns the resident bytes reported by Linux procfs.
func ProcessRSS(pid int) int64 {
	if runtime.GOOS != "linux" || pid <= 0 {
		return 0
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 64*1024))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value * 1024
		}
	}
	return 0
}

// SanitizedModelEnvironment returns a minimal allowlisted model environment.
func SanitizedModelEnvironment(extra []string) []string {
	allowed := map[string]struct{}{
		"HOME": {}, "PATH": {}, "TMPDIR": {}, "TEMP": {}, "TMP": {}, "LANG": {}, "LC_ALL": {},
		"OMP_NUM_THREADS": {}, "GGML_NUMA": {},
	}
	overrides := environmentMap(os.Environ(), allowed)
	for name, value := range environmentMap(extra, allowed) {
		overrides[name] = value
	}
	overrides["CUDA_VISIBLE_DEVICES"] = "-1"
	overrides["HIP_VISIBLE_DEVICES"] = "-1"
	overrides["ROCR_VISIBLE_DEVICES"] = "-1"
	overrides["GGML_VK_VISIBLE_DEVICES"] = "-1"
	return sortedEnvironment(overrides)
}

// ResourceGuardEnvironment returns only values needed to reach user-systemd.
func ResourceGuardEnvironment() []string {
	allowed := map[string]struct{}{
		"HOME": {}, "PATH": {}, "TMPDIR": {}, "TEMP": {}, "TMP": {}, "LANG": {}, "LC_ALL": {},
		"XDG_RUNTIME_DIR": {}, "DBUS_SESSION_BUS_ADDRESS": {},
	}
	return sortedEnvironment(environmentMap(os.Environ(), allowed))
}

func environmentMap(values []string, allowed map[string]struct{}) map[string]string {
	result := make(map[string]string)
	for _, item := range values {
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, permitted := allowed[name]; permitted {
			result[name] = value
		}
	}
	return result
}

func sortedEnvironment(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

func validateEnvironment(values []string) error {
	for _, value := range values {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00 ") || strings.Contains(value, "\x00") {
			return errors.New("guarded process environment contains an invalid entry")
		}
	}
	return nil
}

func validUnitPrefix(value string) bool {
	if len(value) == 0 || len(value) > 40 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}
