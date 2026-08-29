package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
)

// runOpen is the non-technical first-run path. It deliberately composes the
// existing quickstart and read-only server contracts instead of introducing a
// second scan implementation or a separate browser application.
func runOpen(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runOpenContext(ctx, args)
}

func runOpenContext(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	config := fs.String("config", "", "optional RKC JSON configuration")
	output := fs.String("out", "", "atlas directory (default <repository>/.rkc)")
	state := fs.String("state-dir", "", "snapshot directory (default <repository>/.rkc-state)")
	enablePython := fs.Bool("python", false, "enable the sandboxed Python adapter after doctor passes")
	clean := fs.Bool("clean", false, "disable incremental stage-cache reuse")
	force := fs.Bool("force", true, "replace an existing RKC-owned atlas")
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address for the local browser")
	readyFile := fs.String("ready-file", "", "atomically write the server readiness receipt to this path")
	noBrowser := fs.Bool("no-browser", false, "serve the atlas without opening a browser (useful for headless hosts)")
	scipIndexes := stringList{}
	fs.Var(&scipIndexes, "scip-index", "compiler-produced SCIP index to import; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("open accepts at most one repository path")
	}
	repository := "."
	if fs.NArg() == 1 {
		repository = fs.Arg(0)
	}
	root, err := filepath.Abs(repository)
	if err != nil {
		return fmt.Errorf("resolve open repository: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect open repository: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("open repository is not a directory: %s", root)
	}
	atlas := filepath.Join(root, ".rkc")
	if *output != "" {
		atlas, err = filepath.Abs(*output)
		if err != nil {
			return fmt.Errorf("resolve open output: %w", err)
		}
	}

	quickstartArgs := make([]string, 0, 12+len(scipIndexes)*2)
	if *config != "" {
		quickstartArgs = append(quickstartArgs, "--config", *config)
	}
	quickstartArgs = append(quickstartArgs, "--out", atlas)
	if *state != "" {
		quickstartArgs = append(quickstartArgs, "--state-dir", *state)
	}
	if *enablePython {
		quickstartArgs = append(quickstartArgs, "--python")
	}
	if *clean {
		quickstartArgs = append(quickstartArgs, "--clean")
	}
	if !*force {
		quickstartArgs = append(quickstartArgs, "--force=false")
	}
	for _, index := range scipIndexes {
		quickstartArgs = append(quickstartArgs, "--scip-index", index)
	}
	quickstartArgs = append(quickstartArgs, root)
	if err := runQuickstartContext(ctx, quickstartArgs); err != nil {
		return fmt.Errorf("open quickstart: %w", err)
	}

	serveArgs := []string{"--addr", *addr}
	if *readyFile != "" {
		serveArgs = append(serveArgs, "--ready-file", *readyFile)
	}
	serveArgs = append(serveArgs, "--dir", atlas)
	if !*noBrowser {
		serveArgs = append(serveArgs, "--open")
	}
	if err := runServeContext(ctx, serveArgs); err != nil {
		return fmt.Errorf("open server: %w", err)
	}
	return nil
}

// runServeContext exists so the composed open command can propagate a caller
// cancellation without changing the long-standing runServe API.
func runServeContext(ctx context.Context, args []string) error {
	return runServeWithContext(ctx, args)
}

// browserCommand returns an argument-vector command with no shell involved.
// It is intentionally small and portable; failure to find a desktop opener is
// reported as a non-fatal convenience error by serve.
func browserCommand(url string) (*exec.Cmd, error) {
	if url == "" {
		return nil, errors.New("browser URL is empty")
	}
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32.exe", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("find %s: %w", name, err)
	}
	return exec.Command(path, args...), nil
}

func launchBrowser(url string) error {
	command, err := browserCommand(url)
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}
	go func() { _ = command.Wait() }()
	return nil
}
