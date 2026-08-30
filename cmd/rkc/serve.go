package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/neuroforge-io/RKC/internal/commandcatalog"
	"github.com/neuroforge-io/RKC/internal/resourceguard"
	"github.com/neuroforge-io/RKC/internal/server"
)

func runServe(args []string) (resultErr error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServeWithContext(ctx, args)
}

func runServeWithContext(ctx context.Context, args []string) (resultErr error) {
	return runServeWithSafety(ctx, args, serveSafetyChecks{
		requireLowPriority:    resourceguard.RequireCurrentProcessLowPriority,
		checkHigherPriority:   resourceguard.CheckHigherPriority,
		priorityCheckInterval: time.Second,
	})
}

type serveSafetyChecks struct {
	requireLowPriority    func() error
	checkHigherPriority   func() error
	priorityCheckInterval time.Duration
}

const (
	minimumServeHTTPTimeout = time.Millisecond
	maximumServeHTTPTimeout = time.Hour
)

func runServeWithSafety(ctx context.Context, args []string, safety serveSafetyChecks) (resultErr error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address")
	readyFile := fs.String("ready-file", "", "atomically create a JSON readiness receipt after binding; file must not exist")
	readTimeout := fs.Duration("read-timeout", 15*time.Second, "HTTP read timeout (1ms to 1h, inclusive)")
	writeTimeout := fs.Duration("write-timeout", 60*time.Second, "HTTP write timeout (1ms to 1h, inclusive)")
	openBrowser := fs.Bool("open", false, "open the loopback atlas in the default browser after binding")
	allowRemote := fs.Bool("allow-remote", false, "explicitly permit an unauthenticated non-loopback read-only listener")
	workbenchEnabled := fs.Bool("workbench", false, "enable the trusted-user token-authenticated loopback command launcher; not a filesystem sandbox")
	workspace := fs.String("workspace", ".", "workbench working directory and command defaults; not a security boundary")
	workbenchTimeout := fs.Duration("workbench-timeout", 30*time.Minute, "maximum duration of one workbench command")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("serve does not accept positional arguments")
	}
	if err := validateServeHTTPTimeouts(*readTimeout, *writeTimeout); err != nil {
		return err
	}
	// Workbench pages carry local command authority. Never deterministically
	// reuse the static server's browser origin: an OS-selected port avoids the
	// predictable origin where persisted service-worker state could remain.
	if *workbenchEnabled && !flagWasSet(fs, "addr") {
		*addr = "127.0.0.1:0"
	}
	if err := validateServeAddress(*addr, *allowRemote, *workbenchEnabled); err != nil {
		return err
	}
	if *workbenchEnabled && *openBrowser {
		return errors.New("direct serve --workbench cannot launch a browser inside the guarded service; use rkc open --workbench")
	}
	if *workbenchEnabled && *readyFile == "" {
		return errors.New("workbench requires an owner-private --ready-file for its out-of-band browser capability; use rkc open --workbench for automatic setup")
	}
	var executable string
	if *workbenchEnabled {
		if safety.requireLowPriority == nil || safety.checkHigherPriority == nil || safety.priorityCheckInterval <= 0 {
			return errors.New("workbench safety checks are unavailable")
		}
		if err := safety.requireLowPriority(); err != nil {
			return fmt.Errorf("workbench requires the RKC low-priority resource envelope (use rkc open or scripts/with-rkc-limits.sh): %w", err)
		}
		if err := safety.checkHigherPriority(); err != nil {
			return fmt.Errorf("workbench priority admission: %w", err)
		}
		var err error
		executable, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve RKC executable: %w", err)
		}
	}
	dataset, err := loadSelectedDataset(ctx, *dir, *database, *snapshotID, *repositoryID, flagWasSet(fs, "dir"))
	if err != nil {
		return err
	}
	var workbench *server.Workbench
	workbenchClosed := false
	if *workbenchEnabled {
		if err := dataset.PrepareWorkbenchBrowser(); err != nil {
			return err
		}
		// Loading and validating a large atlas can take long enough for ERAIS to
		// start after initial admission. Recheck immediately before any listener
		// or bootstrap capability is published; the outer `rkc open` guard also
		// monitors and can terminate this child throughout the load itself.
		if err := safety.checkHigherPriority(); err != nil {
			return fmt.Errorf("workbench priority changed during dataset preparation: %w", err)
		}
		commandContext := servedWorkbenchCommandContext(dataset.Root, dataset.Manifest.ID, *database)
		workbench, err = server.NewWorkbench(server.WorkbenchConfig{
			Workspace: *workspace, Executable: executable, Timeout: *workbenchTimeout,
			CommandContext: commandContext,
		})
		if err != nil {
			return err
		}
		defer func() {
			if workbenchClosed {
				return
			}
			closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, workbench.Close(closeContext))
		}()
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	defer listener.Close()
	actualAddress := listener.Addr().String()
	ready := serveReadyReceipt{SchemaVersion: "1.0", Address: actualAddress, URL: "http://" + actualAddress, SnapshotID: dataset.Manifest.ID}
	if workbench != nil {
		ready.BrowserURL, err = workbench.BrowserURL(ready.URL)
		if err != nil {
			return fmt.Errorf("create protected workbench browser URL: %w", err)
		}
	}
	if err := publishServeReadyFile(*readyFile, ready); err != nil {
		return err
	}
	httpServer := &http.Server{Addr: actualAddress, Handler: dataset.HandlerWithWorkbench(workbench), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: *readTimeout, WriteTimeout: *writeTimeout, IdleTimeout: 60 * time.Second}
	fmt.Printf("RKC snapshot %s at %s\n", dataset.Manifest.ID, ready.URL)
	if workbench != nil {
		fmt.Printf("Local command workbench enabled for %s\n", *workspace)
		fmt.Println("Security: workbench commands have the invoking user's filesystem authority; use only on a trusted local account.")
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	var priorityDone <-chan error
	if workbench != nil {
		monitorContext, stopMonitor := context.WithCancel(ctx)
		priorityTicker := time.NewTicker(safety.priorityCheckInterval)
		monitorDone := make(chan error, 1)
		go func() {
			monitorDone <- monitorHigherPriority(monitorContext, priorityTicker.C, safety.checkHigherPriority)
		}()
		defer func() {
			priorityTicker.Stop()
			stopMonitor()
		}()
		priorityDone = monitorDone
	}
	if *openBrowser {
		if !loopbackListenAddress(*addr) {
			fmt.Fprintln(os.Stderr, "rkc: --open was requested, but the listen address is not loopback; use an explicit localhost address")
		} else if err := launchBrowser(ready.URL); err != nil {
			// Browser launch is a convenience only. The server remains usable in
			// headless environments and the URL is already printed above.
			fmt.Fprintf(os.Stderr, "rkc: could not open the browser: %v\n", err)
		}
	}
	select {
	case err = <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case priorityErr := <-priorityDone:
		// Stop and reap command jobs before giving HTTP connections their grace
		// period. That makes higher-priority arrival a real compute preemption,
		// not merely a refusal to accept new browser requests.
		closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		workbenchErr := workbench.Close(closeContext)
		cancel()
		workbenchClosed = true
		return errors.Join(priorityErr, workbenchErr, shutdownHTTPServer(httpServer, serveDone))
	case <-ctx.Done():
		shutdownErr := shutdownHTTPServer(httpServer, serveDone)
		if priorityDone == nil {
			return shutdownErr
		}
		// The monitor derives from ctx, so it must report before shutdown is
		// considered complete. Waiting here preserves a simultaneously observed
		// higher-priority error instead of racing it against user cancellation.
		var priorityErr error
		select {
		case priorityErr = <-priorityDone:
		case <-time.After(2 * time.Second):
			priorityErr = errors.New("workbench priority monitor did not stop within the shutdown bound")
		}
		return errors.Join(priorityErr, shutdownErr)
	}
}

func monitorHigherPriority(ctx context.Context, ticks <-chan time.Time, check func() error) error {
	if check == nil || ticks == nil {
		return errors.New("higher-priority monitor is unavailable")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return errors.New("higher-priority monitor stopped unexpectedly")
			}
			if err := check(); err != nil {
				return err
			}
		}
	}
}

func shutdownHTTPServer(httpServer *http.Server, serveDone <-chan error) error {
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := httpServer.Shutdown(shutdownContext)
	cancel()
	var forceCloseErr error
	if shutdownErr != nil {
		forceCloseErr = httpServer.Close()
	}
	var serveErr error
	select {
	case serveErr = <-serveDone:
	case <-time.After(2 * time.Second):
		serveErr = errors.New("HTTP server did not stop within the shutdown bound")
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(shutdownErr, forceCloseErr, serveErr)
}

func servedWorkbenchCommandContext(root, snapshotID, database string) commandcatalog.Context {
	if database == "" {
		return commandcatalog.Context{
			DatasetArgs: []string{"--dir", root},
			CheckArgs:   []string{"--coverage", filepath.Join(root, "coverage.json")},
		}
	}
	return commandcatalog.Context{
		DatasetArgs: []string{"--database", root, "--snapshot", snapshotID},
		CheckArgs:   []string{"--help"},
	}
}

func loopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func validateServeAddress(address string, allowRemote, workbench bool) error {
	loopback := loopbackListenAddress(address)
	if workbench && !loopback {
		return errors.New("workbench requires an explicit localhost or loopback listen address")
	}
	if workbench {
		_, port, err := net.SplitHostPort(address)
		if err != nil || port != "0" {
			return errors.New("workbench requires ephemeral port 0 so every browser origin is fresh")
		}
	}
	if _, _, err := net.SplitHostPort(address); err == nil && !loopback && !allowRemote {
		return errors.New("non-loopback serving requires explicit --allow-remote acknowledgement")
	}
	return nil
}

func validateServeHTTPTimeouts(readTimeout, writeTimeout time.Duration) error {
	timeouts := []struct {
		name  string
		value time.Duration
	}{
		{name: "--read-timeout", value: readTimeout},
		{name: "--write-timeout", value: writeTimeout},
	}
	for _, timeout := range timeouts {
		if timeout.value < minimumServeHTTPTimeout || timeout.value > maximumServeHTTPTimeout {
			return fmt.Errorf("%s must be at least 1ms and no greater than 1h", timeout.name)
		}
	}
	return nil
}

type serveReadyReceipt struct {
	SchemaVersion string `json:"schema_version"`
	Address       string `json:"address"`
	URL           string `json:"url"`
	BrowserURL    string `json:"browser_url,omitempty"`
	SnapshotID    string `json:"snapshot_id"`
}

func publishServeReadyFile(path string, receipt serveReadyReceipt) error {
	if path == "" {
		return nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve readiness file: %w", err)
	}
	if _, err := os.Lstat(absolute); err == nil {
		return fmt.Errorf("readiness file already exists: %s", absolute)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect readiness file: %w", err)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode readiness receipt: %w", err)
	}
	data = append(data, '\n')
	parent := filepath.Dir(absolute)
	temporary, err := os.CreateTemp(parent, "."+filepath.Base(absolute)+".tmp-")
	if err != nil {
		return fmt.Errorf("create readiness staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect readiness staging file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write readiness staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync readiness staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close readiness staging file: %w", err)
	}
	// Hard-linking the fully written staging inode is an atomic, no-clobber
	// publication on the same filesystem. A concurrent writer wins cleanly.
	if err := os.Link(temporaryPath, absolute); err != nil {
		return fmt.Errorf("publish readiness file without replacement: %w", err)
	}
	return nil
}
