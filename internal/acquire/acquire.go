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
)

type Kind string

const (
	KindLocal Kind = "local"
	KindGit   Kind = "git"
)

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

type Result struct {
	Kind             Kind   `json:"kind"`
	Root             string `json:"root"`
	Source           string `json:"source"`
	RedactedSource   string `json:"redacted_source"`
	RequestedRef     string `json:"requested_ref,omitempty"`
	Temporary        bool   `json:"temporary"`
	MaterializedPath string `json:"materialized_path,omitempty"`
	cleanup          func() error
}

func (result Result) Cleanup() error {
	if result.cleanup == nil {
		return nil
	}
	return result.cleanup()
}

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
		return Result{Kind: KindLocal, Root: root, Source: source, RedactedSource: root}, nil
	} else if !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("inspect repository source: %w", err)
	}

	parsed, scpStyle, err := validateRemoteSource(source, options.AllowFileURLs)
	if err != nil {
		return Result{}, err
	}
	redacted := redactSource(source, parsed, scpStyle)
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
		if err := runGit(cloneCtx, options, redacted, arguments...); err != nil {
			return Result{}, err
		}
	} else {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return Result{}, err
		}
		if err := runGit(cloneCtx, options, redacted, "-C", root, "init", "--quiet"); err != nil {
			return Result{}, err
		}
		if err := runGit(cloneCtx, options, redacted, "-C", root, "remote", "add", "origin", source); err != nil {
			return Result{}, err
		}
		fetch := []string{"-C", root, "fetch", "--no-tags"}
		if options.Depth > 0 {
			fetch = append(fetch, "--depth", fmt.Sprint(options.Depth))
		}
		fetch = append(fetch, "origin", options.Ref)
		if err := runGit(cloneCtx, options, redacted, fetch...); err != nil {
			return Result{}, err
		}
		if err := runGit(cloneCtx, options, redacted, "-C", root, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
			return Result{}, err
		}
	}
	if options.Submodules {
		arguments := []string{"-C", root, "submodule", "update", "--init", "--recursive"}
		if options.Depth > 0 {
			arguments = append(arguments, "--depth", fmt.Sprint(options.Depth))
		}
		if err := runGit(cloneCtx, options, redacted, arguments...); err != nil {
			return Result{}, err
		}
	}
	if cloneCtx.Err() != nil {
		return Result{}, fmt.Errorf("materialise %s: %w", redacted, cloneCtx.Err())
	}
	failed = false
	result := Result{
		Kind: KindGit, Root: root, Source: source, RedactedSource: redacted, RequestedRef: options.Ref,
		Temporary: !options.KeepMaterialized, MaterializedPath: root,
	}
	if !options.KeepMaterialized {
		result.cleanup = cleanup
	}
	return result, nil
}

func runGit(ctx context.Context, options Options, redactedSource string, arguments ...string) error {
	protocolFile := "never"
	if options.AllowFileURLs {
		protocolFile = "always"
	}
	safeArguments := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "filter.lfs.smudge=",
		"-c", "filter.lfs.required=false",
		"-c", "protocol.file.allow=" + protocolFile,
	}
	safeArguments = append(safeArguments, arguments...)
	command := exec.CommandContext(ctx, options.GitExecutable, safeArguments...)
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_LFS_SKIP_SMUDGE=1",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
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
		detail = redactSecrets(detail)
		if stdout.truncated || stderr.truncated {
			detail += " [output truncated]"
		}
		if ctx.Err() != nil {
			return fmt.Errorf("Git operation for %s: %w", redactedSource, ctx.Err())
		}
		return fmt.Errorf("Git operation for %s failed: %w: %s", redactedSource, err, detail)
	}
	return nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

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
func (writer *limitedBuffer) String() string { return writer.buffer.String() }

func validateRemoteSource(source string, allowFile bool) (*url.URL, bool, error) {
	if scpPattern.MatchString(source) && !strings.Contains(source, "://") {
		return nil, true, nil
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" {
		return nil, false, fmt.Errorf("repository source does not exist locally and is not a supported Git URL: %s", redactSecrets(source))
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "ssh", "git":
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
	return parsed, false, nil
}

var scpPattern = regexp.MustCompile(`^[^/@\s]+@[^/:\s]+:.+$`)
var credentialPattern = regexp.MustCompile(`(?i)(https?://)([^/@\s]+)@`)

func redactSource(source string, parsed *url.URL, scpStyle bool) string {
	if scpStyle {
		return source
	}
	copy := *parsed
	if copy.User != nil {
		username := copy.User.Username()
		if username == "" {
			copy.User = nil
		} else {
			copy.User = url.User(username)
		}
	}
	return copy.String()
}
func redactSecrets(value string) string {
	return credentialPattern.ReplaceAllString(value, `${1}<redacted>@`)
}

var _ io.Writer = (*limitedBuffer)(nil)
