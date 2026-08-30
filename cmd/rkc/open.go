package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

// runOpen is the non-technical first-run path. It deliberately composes the
// existing quickstart and read-only/opt-in-workbench contracts instead of
// introducing a second scan implementation or a separate browser application.
func runOpen(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runOpenWithAdmission(ctx, args)
}

type openOptions struct {
	config       string
	output       string
	state        string
	enablePython bool
	clean        bool
	force        bool
	address      string
	readyFile    string
	noBrowser    bool
	workbench    bool
	scipIndexes  stringList
}

func newOpenFlagSet(output io.Writer) (*flag.FlagSet, *openOptions) {
	options := &openOptions{force: true, address: "127.0.0.1:0"}
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&options.config, "config", "", "optional RKC JSON configuration")
	fs.StringVar(&options.output, "out", "", "atlas directory (default <repository>/.rkc)")
	fs.StringVar(&options.state, "state-dir", "", "snapshot directory (default <repository>/.rkc-state)")
	fs.BoolVar(&options.enablePython, "python", false, "request the sandboxed Python adapter (disabled in protected open until aggregate child ceilings are proved)")
	fs.BoolVar(&options.clean, "clean", false, "disable incremental stage-cache reuse")
	fs.BoolVar(&options.force, "force", true, "replace an existing RKC-owned atlas")
	fs.StringVar(&options.address, "addr", options.address, "HTTP listen address for the local browser")
	fs.StringVar(&options.readyFile, "ready-file", "", "atomically write the server readiness receipt to this path")
	fs.BoolVar(&options.noBrowser, "no-browser", false, "serve the atlas without opening a browser (useful for headless hosts)")
	fs.BoolVar(&options.workbench, "workbench", false, "enable the trusted-user local command launcher (Linux only; not a filesystem sandbox)")
	fs.Var(&options.scipIndexes, "scip-index", "compiler-produced SCIP index to import; repeatable")
	return fs, options
}

func runOpenContext(ctx context.Context, args []string) error {
	fs, options := newOpenFlagSet(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	finalizeOpenOptions(fs, options)
	if fs.NArg() > 1 {
		return errors.New("open accepts at most one repository path")
	}
	if options.enablePython {
		return errors.New("open --python is disabled until the Python adapter can prove one aggregate resource ceiling with its parent scan; use the deterministic default")
	}
	if options.workbench && options.noBrowser && options.readyFile == "" {
		return errors.New("open --workbench --no-browser requires an owner-private --ready-file so the one-time browser capability can be transferred safely")
	}
	if _, _, err := net.SplitHostPort(options.address); err != nil {
		return fmt.Errorf("open listen address is invalid: %w", err)
	}
	if err := validateServeAddress(options.address, false, options.workbench); err != nil {
		return fmt.Errorf("open listen address: %w", err)
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
	if err := validateOpenExecutionMode(options.workbench, runtime.GOOS, resourceguard.RequireCurrentProcessLowPriority); err != nil {
		return err
	}
	atlas := filepath.Join(root, ".rkc")
	if options.output != "" {
		atlas, err = filepath.Abs(options.output)
		if err != nil {
			return fmt.Errorf("resolve open output: %w", err)
		}
	}

	quickstartArgs := make([]string, 0, 12+len(options.scipIndexes)*2)
	if options.config != "" {
		quickstartArgs = append(quickstartArgs, "--config", options.config)
	}
	quickstartArgs = append(quickstartArgs, "--out", atlas)
	if options.state != "" {
		quickstartArgs = append(quickstartArgs, "--state-dir", options.state)
	}
	if options.enablePython {
		quickstartArgs = append(quickstartArgs, "--python")
	}
	if options.clean {
		quickstartArgs = append(quickstartArgs, "--clean")
	}
	if !options.force {
		quickstartArgs = append(quickstartArgs, "--force=false")
	}
	for _, index := range options.scipIndexes {
		quickstartArgs = append(quickstartArgs, "--scip-index", index)
	}
	quickstartArgs = append(quickstartArgs, root)
	if err := runQuickstartContext(ctx, quickstartArgs); err != nil {
		return fmt.Errorf("open quickstart: %w", err)
	}

	readyFile := options.readyFile
	guardedChild := os.Getenv(guardedOpenChildEnvironment) == "1"
	if readyFile == "" && guardedChild {
		readyFile = os.Getenv(guardedOpenReadyFileEnvironment)
	}
	serveArgs := openServeArguments(
		options.address,
		readyFile,
		atlas,
		root,
		options.workbench,
		!options.noBrowser && !guardedChild,
	)
	if err := runServeContext(ctx, serveArgs); err != nil {
		return fmt.Errorf("open server: %w", err)
	}
	return nil
}

// Workbench browser capabilities must never be delivered on the historical
// fixed read-only origin. An older or imported atlas could have registered a
// persistent service worker there before current CSP protections existed.
func finalizeOpenOptions(fs *flag.FlagSet, options *openOptions) {
	if fs != nil && options != nil && options.workbench && !flagWasSet(fs, "addr") {
		options.address = "127.0.0.1:0"
	}
}

func validateOpenExecutionMode(workbench bool, platform string, requireEnvelope func() error) error {
	if !workbench {
		return nil
	}
	if platform != "linux" {
		return errors.New("the interactive workbench requires the Linux low-priority resource envelope; omit --workbench on this platform")
	}
	if requireEnvelope == nil {
		return errors.New("interactive open resource admission is not configured")
	}
	if err := requireEnvelope(); err != nil {
		return fmt.Errorf("interactive open requires the protected resource envelope: %w", err)
	}
	return nil
}

func openServeArguments(address, readyFile, atlas, workspace string, workbench, openBrowser bool) []string {
	arguments := []string{"--addr", address}
	if readyFile != "" {
		arguments = append(arguments, "--ready-file", readyFile)
	}
	arguments = append(arguments, "--dir", atlas)
	if workbench {
		arguments = append(arguments, "--workbench", "--workspace", workspace)
	}
	if openBrowser {
		arguments = append(arguments, "--open")
	}
	return arguments
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

// launchBrowserPrivately keeps a workbench bootstrap fragment out of process
// arguments. The desktop opener sees only an owner-private temporary file,
// which redirects the browser and is removed after a bounded grace period.
func launchBrowserPrivately(target string) error {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || !loopbackListenAddress(parsed.Host) {
		return errors.New("browser target must be a canonical loopback HTTP URL")
	}
	if parsed.Fragment == "" {
		return launchBrowser(target)
	}
	directory, err := os.MkdirTemp("", "rkc-browser-bootstrap-")
	if err != nil {
		return fmt.Errorf("create private browser bootstrap directory: %w", err)
	}
	page := filepath.Join(directory, "open.html")
	file, err := os.OpenFile(page, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		os.RemoveAll(directory)
		return fmt.Errorf("create private browser bootstrap page: %w", err)
	}
	content := "<!doctype html><meta charset=\"utf-8\"><meta name=\"referrer\" content=\"no-referrer\">" +
		"<meta http-equiv=\"refresh\" content=\"0;url=" + html.EscapeString(target) + "\">" +
		"<title>Opening RKC</title><p>Opening the protected local RKC workbench…</p>"
	if _, err := io.WriteString(file, content); err != nil {
		file.Close()
		os.RemoveAll(directory)
		return fmt.Errorf("write private browser bootstrap page: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.RemoveAll(directory)
		return fmt.Errorf("sync private browser bootstrap page: %w", err)
	}
	if err := file.Close(); err != nil {
		os.RemoveAll(directory)
		return fmt.Errorf("close private browser bootstrap page: %w", err)
	}
	fileURL := (&url.URL{Scheme: "file", Path: page}).String()
	command, err := browserCommand(fileURL)
	if err != nil {
		os.RemoveAll(directory)
		return err
	}
	if err := command.Start(); err != nil {
		os.RemoveAll(directory)
		return err
	}
	go func() {
		_ = command.Wait()
		timer := time.NewTimer(30 * time.Second)
		<-timer.C
		_ = os.RemoveAll(directory)
	}()
	return nil
}
