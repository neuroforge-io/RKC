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

	"github.com/neuroforge-io/RKC/internal/resourceguard"
	"github.com/neuroforge-io/RKC/internal/server"
)

func runServe(args []string) (resultErr error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address")
	readyFile := fs.String("ready-file", "", "atomically create a JSON readiness receipt after binding; file must not exist")
	readTimeout := fs.Duration("read-timeout", 15*time.Second, "HTTP read timeout")
	writeTimeout := fs.Duration("write-timeout", 60*time.Second, "HTTP write timeout")
	workbenchEnabled := fs.Bool("workbench", false, "enable the token-authenticated loopback command workbench")
	workspace := fs.String("workspace", ".", "workbench repository directory")
	workbenchTimeout := fs.Duration("workbench-timeout", 30*time.Minute, "maximum duration of one workbench command")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("serve does not accept positional arguments")
	}
	var workbench *server.Workbench
	if *workbenchEnabled {
		if !loopbackListenAddress(*addr) {
			return errors.New("workbench requires an explicit localhost or loopback listen address")
		}
		if err := resourceguard.RequireCurrentProcessLowPriority(); err != nil {
			return fmt.Errorf("workbench requires scripts/with-rkc-limits.sh: %w", err)
		}
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve RKC executable: %w", err)
		}
		workbench, err = server.NewWorkbench(server.WorkbenchConfig{
			Workspace: *workspace, Executable: executable, Timeout: *workbenchTimeout,
		})
		if err != nil {
			return err
		}
		defer func() {
			closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, workbench.Close(closeContext))
		}()
	}
	dataset, err := loadSelectedDataset(context.Background(), *dir, *database, *snapshotID, *repositoryID, flagWasSet(fs, "dir"))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	defer listener.Close()
	actualAddress := listener.Addr().String()
	ready := serveReadyReceipt{SchemaVersion: "1.0", Address: actualAddress, URL: "http://" + actualAddress, SnapshotID: dataset.Manifest.ID}
	if err := publishServeReadyFile(*readyFile, ready); err != nil {
		return err
	}
	httpServer := &http.Server{Addr: actualAddress, Handler: dataset.HandlerWithWorkbench(workbench), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: *readTimeout, WriteTimeout: *writeTimeout, IdleTimeout: 60 * time.Second}
	fmt.Printf("RKC snapshot %s at %s\n", dataset.Manifest.ID, ready.URL)
	if workbench != nil {
		fmt.Printf("Local command workbench enabled for %s\n", *workspace)
	}
	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	select {
	case err = <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalContext.Done():
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
}

func loopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

type serveReadyReceipt struct {
	SchemaVersion string `json:"schema_version"`
	Address       string `json:"address"`
	URL           string `json:"url"`
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
