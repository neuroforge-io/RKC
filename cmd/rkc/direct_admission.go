package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

const guardedDirectChildEnvironment = "RKC_GUARDED_DIRECT_CHILD"

type guardedDirectLaunchDependencies struct {
	executable func() (string, error)
	newCommand func(context.Context, resourceguard.Config) (guardedOpenRunner, error)
	stdout     io.Writer
	stderr     io.Writer
}

func defaultGuardedDirectLaunchDependencies() guardedDirectLaunchDependencies {
	return guardedDirectLaunchDependencies{
		executable: os.Executable,
		newCommand: func(ctx context.Context, config resourceguard.Config) (guardedOpenRunner, error) {
			return resourceguard.NewCommand(ctx, config)
		},
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
}

func runDirectCommandWithAdmission(
	ctx context.Context,
	command string,
	args []string,
	local func(context.Context, []string) error,
) error {
	return runDirectCommandWithAdmissionUsing(
		ctx,
		command,
		args,
		runtime.GOOS,
		os.Getenv(guardedDirectChildEnvironment),
		os.Getenv(guardedOpenChildEnvironment) == "1",
		resourceguard.RequireCurrentProcessLowPriority,
		resourceguard.PrepareCurrentProcessLowPriority,
		launchGuardedDirect,
		local,
		func(protectedContext context.Context, protectedArguments []string) error {
			return runProtectedDirectLocal(protectedContext, command, protectedArguments, local)
		},
	)
}

func runDirectCommandWithAdmissionUsing(
	ctx context.Context,
	command string,
	args []string,
	platform string,
	guardedChild string,
	guardedOpenChild bool,
	requireEnvelope func() error,
	prepareCurrent func() error,
	launch func(context.Context, string, []string) error,
	local func(context.Context, []string) error,
	protectedLocal func(context.Context, []string) error,
) error {
	if ctx == nil || requireEnvelope == nil || prepareCurrent == nil || launch == nil || local == nil || protectedLocal == nil {
		return errors.New("direct command resource admission is not configured")
	}
	help, err := validateDirectCommandAdmission(command, args)
	if help {
		return local(ctx, args)
	}
	if err != nil {
		return err
	}
	if platform != "linux" {
		return local(ctx, args)
	}
	if guardedChild != "" && guardedChild != command {
		return fmt.Errorf(
			"protected %s child marker names %q; refusing recursive or cross-command admission",
			command,
			guardedChild,
		)
	}
	if guardedChild == command || guardedOpenChild {
		if err := requireEnvelope(); err != nil {
			return fmt.Errorf("protected %s child is outside its required resource envelope: %w", command, err)
		}
		return protectedLocal(ctx, args)
	}
	prepareErr := prepareCurrent()
	if prepareErr == nil {
		return protectedLocal(ctx, args)
	}
	launchErr := launch(ctx, command, args)
	if launchErr != nil {
		return errors.Join(
			fmt.Errorf("current process cannot safely host direct %s work: %w", command, prepareErr),
			launchErr,
		)
	}
	return nil
}

const directCurrentProcessCheckInterval = time.Second

type protectedDirectLocalDependencies struct {
	checkHigherPriority func() error
	checkHostMemory     func() error
	requireEnvelope     func() error
	newTicker           func(time.Duration) (<-chan time.Time, func())
}

func runProtectedDirectLocal(
	ctx context.Context,
	command string,
	args []string,
	local func(context.Context, []string) error,
) error {
	return runProtectedDirectLocalUsing(
		ctx,
		command,
		args,
		local,
		protectedDirectLocalDependencies{
			checkHigherPriority: resourceguard.CurrentPriorityCheck(),
			checkHostMemory:     resourceguard.CheckHostAvailableMemory,
			requireEnvelope:     resourceguard.RequireCurrentProcessReusableLowPriority,
			newTicker: func(interval time.Duration) (<-chan time.Time, func()) {
				ticker := time.NewTicker(interval)
				return ticker.C, ticker.Stop
			},
		},
	)
}

func runProtectedDirectLocalUsing(
	ctx context.Context,
	command string,
	args []string,
	local func(context.Context, []string) error,
	dependencies protectedDirectLocalDependencies,
) error {
	if ctx == nil || local == nil || dependencies.checkHigherPriority == nil || dependencies.checkHostMemory == nil ||
		dependencies.requireEnvelope == nil || dependencies.newTicker == nil {
		return errors.New("protected direct local monitoring is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dependencies.checkHigherPriority(); err != nil {
		return fmt.Errorf("protected %s yielded before local work: %w", command, err)
	}
	if err := dependencies.checkHostMemory(); err != nil {
		return fmt.Errorf("protected %s host-memory reserve blocked local work: %w", command, err)
	}
	if err := dependencies.requireEnvelope(); err != nil {
		return fmt.Errorf("protected %s current-process envelope is not reusable: %w", command, err)
	}
	ticks, stopTicker := dependencies.newTicker(directCurrentProcessCheckInterval)
	if ticks == nil || stopTicker == nil {
		return errors.New("protected direct local monitor ticker is not configured")
	}
	workContext, cancelWork := context.WithCancel(ctx)
	monitorResult := make(chan error, 1)
	workFinished := make(chan struct{})
	go func() {
		for {
			select {
			case <-workFinished:
				monitorResult <- nil
				return
			case <-workContext.Done():
				monitorResult <- nil
				return
			case _, ok := <-ticks:
				if !ok {
					monitorResult <- fmt.Errorf("protected %s local monitor ticker stopped unexpectedly", command)
					cancelWork()
					return
				}
				if err := dependencies.checkHigherPriority(); err != nil {
					reportDirectLocalMonitorFailure(
						workFinished,
						monitorResult,
						cancelWork,
						fmt.Errorf("protected %s yielded during local work: %w", command, err),
					)
					return
				}
				if err := dependencies.checkHostMemory(); err != nil {
					reportDirectLocalMonitorFailure(
						workFinished,
						monitorResult,
						cancelWork,
						fmt.Errorf("protected %s yielded to the host-memory reserve during local work: %w", command, err),
					)
					return
				}
				if err := dependencies.requireEnvelope(); err != nil {
					reportDirectLocalMonitorFailure(
						workFinished,
						monitorResult,
						cancelWork,
						fmt.Errorf("protected %s current-process envelope changed during local work: %w", command, err),
					)
					return
				}
			}
		}
	}()
	localErr := local(workContext, args)
	close(workFinished)
	stopTicker()
	cancelWork()
	monitorErr := <-monitorResult
	return errors.Join(localErr, monitorErr)
}

func directLocalWorkFinished(finished <-chan struct{}) bool {
	select {
	case <-finished:
		return true
	default:
		return false
	}
}

func reportDirectLocalMonitorFailure(
	finished <-chan struct{},
	result chan<- error,
	cancel context.CancelFunc,
	failure error,
) {
	if directLocalWorkFinished(finished) {
		result <- nil
		return
	}
	result <- failure
	cancel()
}

func launchGuardedDirect(ctx context.Context, command string, args []string) error {
	return launchGuardedDirectUsing(ctx, command, args, defaultGuardedDirectLaunchDependencies())
}

func launchGuardedDirectUsing(
	ctx context.Context,
	command string,
	args []string,
	dependencies guardedDirectLaunchDependencies,
) error {
	if ctx == nil || dependencies.executable == nil || dependencies.newCommand == nil ||
		dependencies.stdout == nil || dependencies.stderr == nil {
		return errors.New("protected direct launch dependencies are not configured")
	}
	help, err := validateDirectCommandAdmission(command, args)
	if help {
		return errors.New("protected direct launch cannot execute a help request")
	}
	if err != nil {
		return err
	}
	executable, err := dependencies.executable()
	if err != nil {
		return fmt.Errorf("resolve RKC executable for protected %s: %w", command, err)
	}
	arguments := make([]string, 0, len(args)+1)
	arguments = append(arguments, command)
	arguments = append(arguments, args...)
	runner, err := dependencies.newCommand(ctx, resourceguard.Config{
		Executable:      executable,
		Arguments:       arguments,
		Environment:     guardedDirectEnvironment(command),
		MaximumRSSBytes: resourceguard.LowPriorityMemoryMaxBytes,
		UnitPrefix:      "rkc-low",
	})
	if err != nil {
		return fmt.Errorf("configure protected %s: %w", command, err)
	}
	if runner == nil {
		return fmt.Errorf("configure protected %s: guarded command is not configured", command)
	}
	_, runErr := runner.Run(ctx, dependencies.stdout, dependencies.stderr)
	if runErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) && errors.Is(runErr, context.Canceled) {
			return nil
		}
		return fmt.Errorf("protected %s: %w", command, runErr)
	}
	return nil
}

func guardedDirectEnvironment(command string) []string {
	base := guardedOpenEnvironment("")
	result := make([]string, 0, len(base)+1)
	openMarker := guardedOpenChildEnvironment + "="
	for _, entry := range base {
		if !strings.HasPrefix(entry, openMarker) {
			result = append(result, entry)
		}
	}
	result = append(result, guardedDirectChildEnvironment+"="+command)
	sort.Strings(result)
	return result
}

var scanAdmissionBooleanFlags = map[string]struct{}{
	"allow-file-url": {}, "fail-on-errors": {}, "force": {}, "include-sources": {},
	"json": {}, "keep-materialized": {}, "no-cache": {}, "no-env-keys": {},
	"no-frameworks": {}, "no-go": {}, "no-integrations": {}, "no-json-schema": {},
	"no-jsonl-graph": {}, "no-manifests": {}, "no-markdown": {}, "no-openapi": {},
	"no-plugins": {}, "no-python": {}, "no-search-index": {}, "no-secret-scan": {},
	"no-static-site": {}, "no-typescript": {}, "submodules": {},
	"scip-no-pin-check":            {},
	"unsafe-include-secret-values": {},
}

// scanAdmissionValueFlags mirrors the value-taking flags registered by
// runScanContext. Keeping their grammar explicit lets the safety preflight
// preserve standard unknown-flag and missing-value errors without parsing a
// repository or loading configuration before resource admission.
var scanAdmissionValueFlags = map[string]struct{}{
	"acquire-temp": {}, "cache-dir": {}, "clone-depth": {}, "config": {},
	"database": {}, "exclude": {}, "git": {}, "git-timeout": {},
	"max-file-bytes": {}, "max-files": {}, "max-repository-bytes": {},
	"max-text-bytes": {}, "notebook-pack-bytes": {}, "out": {},
	"plugin-output-bytes": {}, "plugin-timeout": {}, "python": {},
	"python-plugin": {}, "ref": {}, "runs-dir": {}, "scip-generate": {},
	"scip-index": {}, "scip-lock": {}, "scip-tool": {},
	"stage-memory-mib": {}, "stage-workers": {}, "state-dir": {}, "trace": {}, "history": {},
}

var quickstartAdmissionBooleanFlags = map[string]struct{}{
	"clean": {}, "force": {}, "python": {}, "scip-no-pin-check": {},
}

var quickstartAdmissionValueFlags = map[string]struct{}{
	"config": {}, "out": {}, "scip-generate": {}, "scip-index": {},
	"scip-lock": {}, "scip-tool": {}, "state-dir": {}, "trace": {}, "history": {},
}

var scipAdmissionBooleanFlags = map[string]struct{}{
	"json": {}, "no-pin-check": {},
}

var scipAdmissionValueFlags = map[string]struct{}{
	"language": {}, "lock": {}, "out": {}, "output": {},
	"timeout": {}, "tool": {}, "tool-args": {},
}

var traceAdmissionBooleanFlags = map[string]struct{}{
	"json": {},
}

var traceAdmissionValueFlags = map[string]struct{}{
	"dir": {}, "environment-key": {}, "out": {}, "timeout": {},
}

var planAdmissionBooleanFlags = map[string]struct{}{
	"json": {}, "no-cache": {}, "no-env-keys": {}, "no-frameworks": {},
	"no-go": {}, "no-json-schema": {}, "no-manifests": {}, "no-markdown": {},
	"no-openapi": {}, "no-plugins": {}, "no-python": {}, "no-secret-scan": {},
	"no-typescript": {},
}

var planAdmissionValueFlags = map[string]struct{}{
	"cache-dir": {}, "config": {}, "exclude": {}, "history": {},
	"max-file-bytes": {}, "max-files": {}, "max-repository-bytes": {},
	"max-text-bytes": {}, "plugin-output-bytes": {}, "plugin-timeout": {},
	"python": {}, "scip-index": {}, "trace": {},
}

func validateDirectCommandAdmission(command string, args []string) (bool, error) {
	var booleanFlags map[string]struct{}
	var valueFlags map[string]struct{}
	switch command {
	case "scan":
		booleanFlags = scanAdmissionBooleanFlags
		valueFlags = scanAdmissionValueFlags
	case "quickstart":
		booleanFlags = quickstartAdmissionBooleanFlags
		valueFlags = quickstartAdmissionValueFlags
	case "scip":
		booleanFlags = scipAdmissionBooleanFlags
		valueFlags = scipAdmissionValueFlags
	case "trace":
		booleanFlags = traceAdmissionBooleanFlags
		valueFlags = traceAdmissionValueFlags
	case "plan":
		booleanFlags = planAdmissionBooleanFlags
		valueFlags = planAdmissionValueFlags
	default:
		return false, fmt.Errorf("direct resource admission does not support command %q", command)
	}
	if command == "scip" || command == "trace" {
		expected := "generate"
		if command == "trace" {
			expected = "capture"
		}
		if len(args) == 0 || args[0] != expected {
			return false, fmt.Errorf("direct %s admission requires the %s subcommand", command, expected)
		}
		args = args[1:]
	}

	values := map[string]bool{}
	present := map[string]bool{}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			break
		}
		name, rawValue, hasValue, isFlag := directAdmissionFlag(argument)
		if !isFlag {
			break
		}
		if name == "h" || name == "help" {
			return true, nil
		}
		if _, isBoolean := booleanFlags[name]; !isBoolean {
			if _, isValue := valueFlags[name]; !isValue {
				return false, fmt.Errorf("flag provided but not defined: -%s", name)
			}
			// The standard flag parser consumes the following token for a known
			// non-boolean option even when it starts with a dash. Match that grammar
			// so a value can never smuggle a safety flag into admission.
			if !hasValue {
				if index+1 >= len(args) {
					return false, fmt.Errorf("flag needs an argument: -%s", name)
				}
				index++
			}
			continue
		}
		value := true
		if hasValue {
			parsed, err := strconv.ParseBool(rawValue)
			if err != nil {
				if name == "no-python" || name == "no-plugins" || name == "python" {
					if command == "scan" {
						return false, fmt.Errorf(
							"invalid --%s value %q: %w; safe command: rkc scan --no-python [options] <repository>",
							name,
							rawValue,
							err,
						)
					}
					return false, fmt.Errorf(
						"invalid --python value %q: %w; safe command: rkc quickstart [options] <repository> (Python is disabled by default)",
						rawValue,
						err,
					)
				}
				continue
			}
			value = parsed
		}
		if name == "no-python" || name == "no-plugins" || name == "python" {
			values[name] = value
			present[name] = true
		}
	}

	if command == "scan" {
		pythonDisabled := present["no-python"] && values["no-python"]
		pluginsDisabled := present["no-plugins"] && values["no-plugins"]
		if !pythonDisabled && !pluginsDisabled {
			return false, errors.New("direct scan requires an explicit final --no-python=true or --no-plugins=true before the repository path until the Python adapter and parent scan can prove one aggregate resource ceiling; use: rkc scan --no-python [options] <repository>")
		}
		return false, nil
	}
	if present["python"] && values["python"] {
		return false, errors.New("direct quickstart --python is disabled until the Python adapter and parent scan can prove one aggregate resource ceiling; use: rkc quickstart [options] <repository> (Python is disabled by default)")
	}
	return false, nil
}

func directAdmissionFlag(argument string) (name, value string, hasValue, ok bool) {
	if argument == "" || argument == "-" || argument[0] != '-' {
		return "", "", false, false
	}
	trimmed := strings.TrimPrefix(argument, "-")
	if strings.HasPrefix(trimmed, "-") {
		trimmed = strings.TrimPrefix(trimmed, "-")
	}
	if trimmed == "" || strings.HasPrefix(trimmed, "-") {
		return "", "", false, false
	}
	name, value, hasValue = strings.Cut(trimmed, "=")
	return name, value, hasValue, name != ""
}
