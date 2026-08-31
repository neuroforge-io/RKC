package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

const (
	guardedOpenChildEnvironment     = "RKC_GUARDED_OPEN_CHILD"
	guardedOpenReadyFileEnvironment = "RKC_GUARDED_OPEN_READY_FILE"
	maximumOpenReadyBytes           = 16 * 1024
)

// runOpenWithAdmission places every Linux first-run scan inside the same
// kernel-enforced, one-core, low-priority envelope used by guarded development
// and model work. Admission happens before quickstart can create an atlas,
// cache, journal, or snapshot directory.
func runOpenWithAdmission(ctx context.Context, args []string) error {
	return runOpenWithAdmissionUsing(
		ctx,
		args,
		runtime.GOOS,
		os.Getenv(guardedOpenChildEnvironment) == "1",
		resourceguard.RequireCurrentProcessLowPriority,
		launchGuardedOpen,
		runOpenContext,
	)
}

func runOpenWithAdmissionUsing(
	ctx context.Context,
	args []string,
	platform string,
	guardedChild bool,
	requireEnvelope func() error,
	launch func(context.Context, []string) error,
	local func(context.Context, []string) error,
) error {
	if ctx == nil || requireEnvelope == nil || launch == nil || local == nil {
		return errors.New("open resource admission is not configured")
	}
	if openHelpRequest(args) || platform != "linux" {
		return local(ctx, args)
	}
	if !guardedChild {
		// Even a caller already inside a shell-created envelope is re-executed.
		// The outer Go guard supplies continuous priority-workload pre-emption
		// and proves the complete transient unit inactive on every exit path.
		return launch(ctx, args)
	}
	if err := requireEnvelope(); err != nil {
		return fmt.Errorf("protected open child is outside its required resource envelope: %w", err)
	}
	return local(ctx, args)
}

func openHelpRequest(args []string) bool {
	fs, _ := newOpenFlagSet(io.Discard)
	return errors.Is(fs.Parse(args), flag.ErrHelp)
}

type guardedOpenRunner interface {
	Run(context.Context, io.Writer, io.Writer) (int64, error)
}

type guardedOpenLaunchDependencies struct {
	executable        func() (string, error)
	makeTempDirectory func(string, string) (string, error)
	removeAll         func(string) error
	absolutePath      func(string) (string, error)
	newCommand        func(context.Context, resourceguard.Config) (guardedOpenRunner, error)
	waitAndLaunch     func(context.Context, string) error
	stdout            io.Writer
	stderr            io.Writer
}

func defaultGuardedOpenLaunchDependencies() guardedOpenLaunchDependencies {
	return guardedOpenLaunchDependencies{
		executable:        os.Executable,
		makeTempDirectory: os.MkdirTemp,
		removeAll:         os.RemoveAll,
		absolutePath:      filepath.Abs,
		newCommand: func(ctx context.Context, config resourceguard.Config) (guardedOpenRunner, error) {
			return resourceguard.NewCommand(ctx, config)
		},
		waitAndLaunch: waitForOpenReadyAndLaunch,
		stdout:        os.Stdout,
		stderr:        os.Stderr,
	}
}

func launchGuardedOpen(ctx context.Context, args []string) error {
	return launchGuardedOpenUsing(ctx, args, defaultGuardedOpenLaunchDependencies())
}

func launchGuardedOpenUsing(
	ctx context.Context,
	args []string,
	dependencies guardedOpenLaunchDependencies,
) (resultErr error) {
	if ctx == nil || dependencies.executable == nil || dependencies.makeTempDirectory == nil ||
		dependencies.removeAll == nil || dependencies.absolutePath == nil || dependencies.newCommand == nil ||
		dependencies.waitAndLaunch == nil ||
		dependencies.stdout == nil || dependencies.stderr == nil {
		return errors.New("protected open launch dependencies are not configured")
	}
	executable, err := dependencies.executable()
	if err != nil {
		return fmt.Errorf("resolve RKC executable for protected open: %w", err)
	}
	fs, options := newOpenFlagSet(io.Discard)
	parseErr := fs.Parse(args)
	if parseErr == nil {
		finalizeOpenOptions(fs, options)
	}
	openBrowser := parseErr == nil && !options.noBrowser
	readyFile := options.readyFile
	var temporaryReadyDirectory string
	if openBrowser && readyFile == "" {
		temporaryReadyDirectory, err = dependencies.makeTempDirectory("", "rkc-open-ready-")
		if err != nil {
			return fmt.Errorf("create protected open readiness directory: %w", err)
		}
		defer func() {
			if cleanupErr := dependencies.removeAll(temporaryReadyDirectory); cleanupErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove protected open readiness directory: %w", cleanupErr))
			}
		}()
		readyFile = filepath.Join(temporaryReadyDirectory, "ready.json")
	}
	if readyFile != "" {
		readyFile, err = dependencies.absolutePath(readyFile)
		if err != nil {
			return fmt.Errorf("resolve protected open readiness file: %w", err)
		}
		if err := requireOpenReadyAbsent(readyFile); err != nil {
			return err
		}
	}
	arguments := make([]string, 0, len(args)+1)
	arguments = append(arguments, "open")
	arguments = append(arguments, args...)
	command, err := dependencies.newCommand(ctx, resourceguard.Config{
		Executable:      executable,
		Arguments:       arguments,
		Environment:     guardedOpenEnvironment(readyFile),
		MaximumRSSBytes: resourceguard.LowPriorityMemoryMaxBytes,
		UnitPrefix:      "rkc-low",
	})
	if err != nil {
		return fmt.Errorf("configure protected open: %w", err)
	}
	if command == nil {
		return errors.New("configure protected open: guarded command is not configured")
	}
	watchContext, stopWatching := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	if openBrowser {
		go func() {
			defer close(watchDone)
			if err := dependencies.waitAndLaunch(watchContext, readyFile); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(dependencies.stderr, "rkc: could not open the browser: %v\n", err)
			}
		}()
	} else {
		close(watchDone)
	}
	_, runErr := command.Run(ctx, dependencies.stdout, dependencies.stderr)
	stopWatching()
	<-watchDone
	if runErr != nil {
		if ctx.Err() == context.Canceled && runErr == context.Canceled {
			return nil
		}
		return fmt.Errorf("protected open: %w", runErr)
	}
	return nil
}

func waitForOpenReadyAndLaunch(ctx context.Context, path string) error {
	if ctx == nil || path == "" {
		return errors.New("protected open browser readiness is not configured")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		receipt, err := readOpenReadyReceipt(path)
		if err == nil {
			return launchBrowserPrivately(receipt.BrowserURL)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func requireOpenReadyAbsent(path string) error {
	if path == "" {
		return errors.New("protected open readiness file is not configured")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("protected open readiness file already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect protected open readiness file: %w", err)
	}
	return nil
}

func readOpenReadyReceipt(path string) (serveReadyReceipt, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return serveReadyReceipt{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return serveReadyReceipt{}, errors.New("protected open readiness file must be an owner-private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return serveReadyReceipt{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return serveReadyReceipt{}, errors.New("protected open readiness file changed while opening")
	}
	reader := bufio.NewReader(io.LimitReader(file, maximumOpenReadyBytes+1))
	data, err := io.ReadAll(reader)
	if err != nil {
		return serveReadyReceipt{}, fmt.Errorf("read protected open readiness file: %w", err)
	}
	if len(data) == 0 || len(data) > maximumOpenReadyBytes {
		return serveReadyReceipt{}, errors.New("protected open readiness receipt is empty or too large")
	}
	var receipt serveReadyReceipt
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return serveReadyReceipt{}, fmt.Errorf("decode protected open readiness receipt: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return serveReadyReceipt{}, errors.New("protected open readiness receipt must contain one JSON object")
	}
	parsed, err := url.Parse(receipt.URL)
	if err != nil || receipt.SchemaVersion != "1.0" || receipt.SnapshotID == "" ||
		parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!loopbackListenAddress(parsed.Host) || receipt.Address != parsed.Host {
		return serveReadyReceipt{}, errors.New("protected open readiness receipt is not a canonical loopback server")
	}
	if receipt.BrowserURL == "" {
		receipt.BrowserURL = receipt.URL
		return receipt, nil
	}
	browser, err := url.Parse(receipt.BrowserURL)
	if err != nil || browser.Scheme != parsed.Scheme || browser.Host != parsed.Host ||
		browser.Path != parsed.Path || browser.RawPath != parsed.RawPath || browser.RawQuery != parsed.RawQuery ||
		browser.User != nil {
		return serveReadyReceipt{}, errors.New("protected open browser capability does not match the ready server")
	}
	values, err := url.ParseQuery(browser.Fragment)
	bootstrap := values.Get("rkc-workbench")
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(bootstrap)
	if err != nil || len(values) != 1 || len(values["rkc-workbench"]) != 1 || decodeErr != nil || len(decoded) != 32 {
		return serveReadyReceipt{}, errors.New("protected open browser capability is malformed")
	}
	return receipt, nil
}

// guardedOpenEnvironment is intentionally smaller than the caller's process
// environment. It retains only local filesystem, locale, and user-systemd
// values needed by quickstart and the opt-in workbench, then fixes common
// runtimes to one worker and disables accelerators. The outer browser launcher
// keeps its original desktop environment; none of that state enters the unit.
func guardedOpenEnvironment(readyFile string) []string {
	allowed := []string{
		"DBUS_SESSION_BUS_ADDRESS", "HOME", "LANG", "LC_ALL", "PATH",
		"SSL_CERT_DIR", "SSL_CERT_FILE", "TEMP", "TMP", "TMPDIR",
		"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"XDG_RUNTIME_DIR", "XDG_STATE_HOME",
		// Compiler-indexer tooling (scip-go, scip-python, scip-typescript)
		// needs module caches and package-manager state to resolve imports.
		"GOENV", "GOFLAGS", "GOMODCACHE", "GOPATH", "GOPROXY", "GOSUMDB", "GOCACHE",
		"npm_config_cache",
	}
	values := make(map[string]string, len(allowed)+14)
	for _, name := range allowed {
		if value, present := os.LookupEnv(name); present && !strings.ContainsRune(value, '\x00') {
			values[name] = value
		}
	}
	for name, value := range map[string]string{
		guardedOpenChildEnvironment:  "1",
		"GOMAXPROCS":                 "1",
		"OMP_NUM_THREADS":            "1",
		"OPENBLAS_NUM_THREADS":       "1",
		"MKL_NUM_THREADS":            "1",
		"NUMEXPR_NUM_THREADS":        "1",
		"CMAKE_BUILD_PARALLEL_LEVEL": "1",
		"CARGO_BUILD_JOBS":           "1",
		"CUDA_VISIBLE_DEVICES":       "-1",
		"HIP_VISIBLE_DEVICES":        "-1",
		"ROCR_VISIBLE_DEVICES":       "-1",
		"GGML_VK_VISIBLE_DEVICES":    "-1",
	} {
		values[name] = value
	}
	if readyFile != "" {
		values[guardedOpenReadyFileEnvironment] = readyFile
	}
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
