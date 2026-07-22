package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/neuroforge-io/RKC/internal/server"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address")
	readyFile := fs.String("ready-file", "", "atomically create a JSON readiness receipt after binding; file must not exist")
	readTimeout := fs.Duration("read-timeout", 15*time.Second, "HTTP read timeout")
	writeTimeout := fs.Duration("write-timeout", 60*time.Second, "HTTP write timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dataset, err := server.Load(*dir)
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
	httpServer := &http.Server{Addr: actualAddress, Handler: dataset.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: *readTimeout, WriteTimeout: *writeTimeout, IdleTimeout: 60 * time.Second}
	fmt.Printf("RKC snapshot %s at %s\n", dataset.Manifest.ID, ready.URL)
	err = httpServer.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
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
