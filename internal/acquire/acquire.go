// Package acquire materialises repository sources without executing project
// code. Local directories are used in place. Remote Git sources are cloned into
// an isolated temporary directory and removed after the caller completes.
package acquire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/sourceorigin"
)

// Kind identifies whether acquisition reused a caller-owned local directory or
// materialized an isolated Git source.
type Kind string

// KindLocal and KindGit are the only acquisition kinds returned by Open.
const (
	KindLocal Kind = "local"
	KindGit   Kind = "git"
)

// Options controls Git materialization, transport admission, resource bounds,
// and temporary-tree ownership. Local-directory acquisition ignores Git-only
// fields and never assumes cleanup ownership.
type Options struct {
	GitExecutable    string
	Ref              string
	Depth            int
	Submodules       bool
	Timeout          time.Duration
	TemporaryRoot    string
	KeepMaterialized bool
	AllowFileURLs    bool
	MaximumLogBytes  int64
}

// Result describes the acquired repository root and its canonical public
// origin. Temporary reports whether Cleanup owns a materialized parent tree;
// retained and local roots are never deleted by Result.
type Result struct {
	Kind             Kind   `json:"kind"`
	Root             string `json:"root"`
	Origin           string `json:"origin,omitempty"`
	RequestedRef     string `json:"requested_ref,omitempty"`
	Temporary        bool   `json:"temporary"`
	MaterializedPath string `json:"materialized_path,omitempty"`
	cleanup          func() error
}

// Cleanup removes an owned temporary Git materialization. It is a no-op for
// local directories and for materializations explicitly retained by the caller.
func (result Result) Cleanup() error {
	if result.cleanup == nil {
		return nil
	}
	return result.cleanup()
}

// Open resolves a local directory in place or clones an admitted HTTPS/SSH Git
// source (or an explicitly enabled file URL) without hooks, filters, prompting,
// LFS smudging, or project execution. Failed temporary acquisitions are removed
// unless KeepMaterialized is set.
func Open(ctx context.Context, source string, options Options) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("repository acquisition context is required")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "."
	}
	if info, err := os.Stat(source); err == nil {
		if !info.IsDir() {
			return Result{}, fmt.Errorf("repository source is not a directory: %s", source)
		}
		root, err := filepath.Abs(source)
		if err != nil {
			return Result{}, fmt.Errorf("resolve repository source: %w", err)
		}
		return Result{Kind: KindLocal, Root: root}, nil
	} else if !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("inspect repository source: %w", err)
	}

	_, _, err := validateRemoteSource(source, options.AllowFileURLs)
	if err != nil {
		return Result{}, err
	}
	canonical, err := sourceorigin.Normalize(source)
	if err != nil {
		return Result{}, errors.New("repository Git origin is not canonicalizable")
	}
	if options.GitExecutable == "" {
		options.GitExecutable = "git"
	}
	if options.Depth < 0 {
		return Result{}, errors.New("clone depth cannot be negative")
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Minute
	}
	if options.MaximumLogBytes <= 0 {
		options.MaximumLogBytes = 2 * 1024 * 1024
	}
	if _, err := exec.LookPath(options.GitExecutable); err != nil {
		return Result{}, fmt.Errorf("find Git executable %q: %w", options.GitExecutable, err)
	}

	base := options.TemporaryRoot
	if base != "" {
		if err := os.MkdirAll(base, 0o700); err != nil {
			return Result{}, fmt.Errorf("create acquisition temporary root: %w", err)
		}
	}
	parent, err := os.MkdirTemp(base, "rkc-acquire-")
	if err != nil {
		return Result{}, fmt.Errorf("create acquisition directory: %w", err)
	}
	root := filepath.Join(parent, "repository")
	cleanup := func() error { return os.RemoveAll(parent) }
	failed := true
	defer func() {
		if failed && !options.KeepMaterialized {
			_ = cleanup()
		}
	}()

	cloneCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	if options.Ref == "" {
		arguments := []string{"clone", "--no-tags"}
		if options.Depth > 0 {
			arguments = append(arguments, "--depth", fmt.Sprint(options.Depth))
		}
		arguments = append(arguments, "--", source, root)
		if err := runGit(cloneCtx, options, canonical, source, arguments...); err != nil {
			return Result{}, err
		}
	} else {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return Result{}, err
		}
		if err := runGit(cloneCtx, options, canonical, source, "-C", root, "init", "--quiet"); err != nil {
			return Result{}, err
		}
		if err := runGit(cloneCtx, options, canonical, source, "-C", root, "remote", "add", "origin", source); err != nil {
			return Result{}, err
		}
		fetch := []string{"-C", root, "fetch", "--no-tags"}
		if options.Depth > 0 {
			fetch = append(fetch, "--depth", fmt.Sprint(options.Depth))
		}
		fetch = append(fetch, "origin", options.Ref)
		if err := runGit(cloneCtx, options, canonical, source, fetch...); err != nil {
			return Result{}, err
		}
		if err := runGit(cloneCtx, options, canonical, source, "-C", root, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
			return Result{}, err
		}
	}
	if options.Submodules {
		arguments := []string{"-C", root, "submodule", "update", "--init", "--recursive"}
		if options.Depth > 0 {
			arguments = append(arguments, "--depth", fmt.Sprint(options.Depth))
		}
		if err := runGit(cloneCtx, options, canonical, source, arguments...); err != nil {
			return Result{}, err
		}
	}
	if cloneCtx.Err() != nil {
		return Result{}, fmt.Errorf("materialise %s: %w", canonical, cloneCtx.Err())
	}
	failed = false
	result := Result{
		Kind: KindGit, Root: root, Origin: canonical, RequestedRef: options.Ref,
		Temporary: !options.KeepMaterialized, MaterializedPath: root,
	}
	if !options.KeepMaterialized {
		result.cleanup = cleanup
	}
	return result, nil
}

func runGit(ctx context.Context, options Options, canonicalSource, rawSource string, arguments ...string) error {
	protocolFile := "never"
	if options.AllowFileURLs {
		protocolFile = "always"
	}
	safeArguments := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "filter.lfs.smudge=",
		"-c", "filter.lfs.required=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
		"-c", "protocol.git.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.file.allow=" + protocolFile,
	}
	safeArguments = append(safeArguments, arguments...)
	command := exec.CommandContext(ctx, options.GitExecutable, safeArguments...)
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_LFS_SKIP_SMUDGE=1",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_PROTOCOL_FROM_USER=0",
	)
	var stdout, stderr limitedBuffer
	stdout.limit = options.MaximumLogBytes
	stderr.limit = options.MaximumLogBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if rawSource != "" {
			detail = strings.ReplaceAll(detail, rawSource, canonicalSource)
		}
		detail = redactSecrets(detail)
		if stdout.truncated || stderr.truncated {
			detail += " [output truncated]"
		}
		if ctx.Err() != nil {
			return fmt.Errorf("Git operation for %s: %w", canonicalSource, ctx.Err())
		}
		return fmt.Errorf("Git operation for %s failed: %w: %s", canonicalSource, err, detail)
	}
	return nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

// Write drains the complete Git output stream while retaining only a bounded
// prefix and recording whether later bytes were discarded.
func (writer *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := writer.limit - writer.written
	if remaining <= 0 {
		writer.truncated = true
		return original, nil
	}
	if int64(len(data)) > remaining {
		data = data[:remaining]
		writer.truncated = true
	}
	n, err := writer.buffer.Write(data)
	writer.written += int64(n)
	if err != nil {
		return n, err
	}
	return original, nil
}

// String returns the retained diagnostic prefix.
func (writer *limitedBuffer) String() string { return writer.buffer.String() }

func validateRemoteSource(source string, allowFile bool) (*url.URL, bool, error) {
	if scpPattern.MatchString(source) && !strings.Contains(source, "://") {
		if strings.ContainsAny(source, "?#") {
			return nil, false, errors.New("Git URL query parameters and fragments are disabled; use a credential helper or SSH agent")
		}
		return nil, true, nil
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" {
		return nil, false, errors.New("repository source does not exist locally and is not a supported Git URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "ssh":
	case "git":
		return nil, false, errors.New("plaintext git:// transport is disabled; use HTTPS or SSH")
	case "file":
		if !allowFile {
			return nil, false, errors.New("file:// Git URLs are disabled; use a local directory or explicitly allow file URLs")
		}
	default:
		return nil, false, fmt.Errorf("unsupported Git URL scheme %q", parsed.Scheme)
	}
	if parsed.Scheme != "file" && parsed.Host == "" {
		return nil, false, errors.New("Git URL must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false, errors.New("Git URL query parameters and fragments are disabled; use a credential helper or SSH agent")
	}
	if parsed.User != nil {
		_, password := parsed.User.Password()
		if strings.EqualFold(parsed.Scheme, "https") || password {
			return nil, false, errors.New("inline Git URL credentials are disabled; use a credential helper or SSH agent")
		}
	}
	return parsed, false, nil
}

var scpPattern = regexp.MustCompile(`^[^/@\s]+@[^/:\s]+:.+$`)
var credentialPattern = regexp.MustCompile(`(?i)(https?://)([^/@\s]+)@`)

func redactSource(source string, parsed *url.URL, scpStyle bool) string {
	_ = parsed
	_ = scpStyle
	canonical, err := sourceorigin.Normalize(source)
	if err != nil {
		return "<invalid-origin>"
	}
	return canonical
}
func redactSecrets(value string) string {
	return credentialPattern.ReplaceAllString(value, `${1}<redacted>@`)
}

var _ io.Writer = (*limitedBuffer)(nil)
