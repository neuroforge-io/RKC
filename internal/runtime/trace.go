// Package runtime captures and imports bounded execution claims. It separates
// static possibility, integrity-authenticated same-process capture records, and
// producer-authenticated observations. The current coverage producer is not
// authenticated, so every current trace remains an operator assertion.
//
// Capture runs an explicitly authorized command (typically `go test` with
// coverage and test-event output). The trace-import pipeline stage binds its
// digest into the snapshot identity, so a different trace always produces a
// different snapshot. A portable self-hashed trace does not authenticate its
// producer and therefore remains user-asserted.
//
// The importer is conservative: statement coverage records positive and
// negative trace-scoped assertions but never sets canonical execution truth.
// Statement coverage also never marks a call edge observed; that requires a
// future authenticated producer with actual call events.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/security/secrets"
)

// PluginID is the stable producer identity attached to imported runtime facts.
const PluginID = "rkc.runtime"

// PluginVersion identifies the trace capture/import semantics.
const PluginVersion = "1.7.0"

const maximumCurrentProcessCaptureIntegrities = 4096

var currentProcessCaptureIntegrities = struct {
	sync.Mutex
	ids   map[string]struct{}
	order []string
}{ids: map[string]struct{}{}}

func markCurrentProcessCaptureIntegrity(trace Trace) {
	if trace.ID == "" {
		return
	}
	currentProcessCaptureIntegrities.Lock()
	defer currentProcessCaptureIntegrities.Unlock()
	if _, exists := currentProcessCaptureIntegrities.ids[trace.ID]; exists {
		return
	}
	currentProcessCaptureIntegrities.ids[trace.ID] = struct{}{}
	currentProcessCaptureIntegrities.order = append(currentProcessCaptureIntegrities.order, trace.ID)
	if len(currentProcessCaptureIntegrities.order) <= maximumCurrentProcessCaptureIntegrities {
		return
	}
	oldest := currentProcessCaptureIntegrities.order[0]
	delete(currentProcessCaptureIntegrities.ids, oldest)
	currentProcessCaptureIntegrities.order = currentProcessCaptureIntegrities.order[1:]
}

func registeredCurrentProcessCaptureIntegrity(traceID string) bool {
	currentProcessCaptureIntegrities.Lock()
	defer currentProcessCaptureIntegrities.Unlock()
	_, ok := currentProcessCaptureIntegrities.ids[traceID]
	return ok
}

func authenticatedCaptureIntegrity(trace Trace) bool {
	if trace.captureIntegrityBound {
		return trace.captureIntegrityAuthenticated && trace.integrityAuthenticatedID == trace.ID
	}
	return registeredCurrentProcessCaptureIntegrity(trace.ID)
}

const (
	// SchemaVersion is the canonical trace format version.
	SchemaVersion = "1.3"
	// MaximumTraceBytes bounds one imported trace.
	MaximumTraceBytes = 64 << 20
	// MaximumTraceArtifacts bounds covered artifacts per trace.
	MaximumTraceArtifacts = 65536
	// MaximumExecutedRanges bounds recorded ranges per artifact.
	MaximumExecutedRanges = 65536
	// MaximumTotalExecutedRanges prevents per-artifact limits multiplying into
	// an unsafe trace-wide allocation or processing workload.
	MaximumTotalExecutedRanges = 65536
	// MaximumTraceTests bounds recorded test results per trace.
	MaximumTraceTests = 65536
	// MaximumEnvironmentKeys bounds recorded environment-variable names.
	MaximumEnvironmentKeys = 4096
	// MaximumTraceSources bounds provenance inputs per trace.
	MaximumTraceSources = 16
	// MaximumCaptureOutputBytes bounds captured command output.
	MaximumCaptureOutputBytes = 4 << 20
	// MaximumCommandIdentifierBytes bounds the persisted command class.
	MaximumCommandIdentifierBytes = 64
	// MaximumPackageIdentifierBytes bounds one persisted package identity.
	MaximumPackageIdentifierBytes = 512
	// MaximumTestIdentifierBytes bounds one persisted test identity.
	MaximumTestIdentifierBytes = 256
	// MaximumTestElapsedMS rejects nonsensical or hostile event durations.
	MaximumTestElapsedMS = int64((7 * 24 * time.Hour) / time.Millisecond)
)

var (
	environmentKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	packageIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~+/\-]*$`)
	testIdentityPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const (
	redactedCommand       = "custom-command"
	redactedPackage       = "redacted-package"
	redactedSubtestSuffix = "/redacted-subtests"
)

// Trace is the canonical runtime evidence record. Command arguments,
// environment values, and captured output are never persisted. Identifiers are
// bounded and control-free; dynamic subtest names are always redacted, and
// credential-shaped static identifiers are rejected or represented by an
// explicit redaction marker.
type Trace struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	// Command is an admitted executable class, never an arbitrary basename.
	Command         string `json:"command"`
	CommandRedacted bool   `json:"command_redacted,omitempty"`
	// CommandSHA256 binds a fully redacted argument shape, never raw values.
	CommandSHA256    string          `json:"command_sha256"`
	WorkingDirectory string          `json:"working_directory"`
	ExitCode         int             `json:"exit_code"`
	DurationMS       int64           `json:"duration_ms"`
	EnvironmentKeys  []string        `json:"environment_keys"`
	Repository       TraceRepository `json:"repository"`
	Artifacts        []TraceArtifact `json:"artifacts"`
	Tests            []TraceTest     `json:"tests"`
	Sources          []TraceSource   `json:"sources"`
	// Capture-integrity authentication is deliberately process-local and
	// excluded from the portable JSON contract. It proves only that this RKC
	// process produced the exact record; it does not authenticate the command or
	// the coverage/test-event producer. integrityAuthenticatedID prevents callers
	// from mutating a captured Trace, recomputing its public ID, and retaining the
	// integrity marker.
	captureIntegrityBound         bool
	captureIntegrityAuthenticated bool
	integrityAuthenticatedID      string
}

// TraceRepository binds runtime evidence to one bounded repository content
// inventory and, when Git is available, one exact commit. ContentDigest is
// computed from canonical relative inventory records; no absolute host path or
// credentialed origin is persisted.
type TraceRepository struct {
	RepositoryID   string `json:"repository_id"`
	ContentDigest  string `json:"content_digest"`
	ArtifactCount  int    `json:"artifact_count"`
	GitCommit      string `json:"git_commit,omitempty"`
	GitUnavailable bool   `json:"git_unavailable,omitempty"`
}

// TraceArtifact records a producer-unverified statement-coverage claim for one
// repository artifact. Field names preserve trace schema 1.3 compatibility;
// import assigns authority rather than trusting those names as canonical truth.
type TraceArtifact struct {
	Path               string          `json:"path"`
	SourceSHA256       string          `json:"source_sha256"`
	SourceSizeBytes    int64           `json:"source_size_bytes"`
	Statements         int             `json:"statements"`
	ExecutedStatements int             `json:"executed_statements"`
	ExecutedRanges     []ExecutedRange `json:"executed_ranges,omitempty"`
}

// ExecutedRange is one covered span in line coordinates.
type ExecutedRange struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	Count     int `json:"count,omitempty"`
}

// TraceTest records one terminal test-result claim reported by the captured
// command. Current import does not promote it to test_result evidence.
type TraceTest struct {
	Package          string `json:"package,omitempty"`
	PackageRedacted  bool   `json:"package_redacted,omitempty"`
	Name             string `json:"name"`
	SubtestsRedacted bool   `json:"subtests_redacted,omitempty"`
	Status           string `json:"status"`
	Elapsed          int64  `json:"elapsed_ms,omitempty"`
}

// TraceSource records one input that produced the trace.
type TraceSource struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Validate checks the structural invariants of a trace.
func Validate(trace Trace) error {
	if trace.SchemaVersion != SchemaVersion {
		return fmt.Errorf("trace schema_version %q is unsupported", trace.SchemaVersion)
	}
	if trace.ID == "" {
		return errors.New("trace id is missing")
	}
	if len(trace.ID) != sha256.Size*2 {
		return errors.New("trace id must be a SHA-256 digest")
	}
	if _, err := hex.DecodeString(trace.ID); err != nil {
		return errors.New("trace id must be lowercase hexadecimal")
	}
	if trace.ID != IDFor(trace) {
		return errors.New("trace id does not match its canonical content")
	}
	if len(trace.Command) == 0 || len(trace.Command) > MaximumCommandIdentifierBytes || containsControl(trace.Command) {
		return errors.New("trace command identity is invalid")
	}
	if trace.CommandRedacted {
		if trace.Command != redactedCommand {
			return errors.New("redacted trace command must use the canonical command class")
		}
	} else if trace.Command != "go" && trace.Command != "go.exe" {
		return errors.New("trace command must be an admitted command class")
	}
	if !validDigest(trace.CommandSHA256) {
		return errors.New("trace command_sha256 is invalid")
	}
	if trace.WorkingDirectory != "." {
		return errors.New("trace working_directory must be repository-relative dot")
	}
	if trace.DurationMS < 0 {
		return errors.New("trace duration_ms cannot be negative")
	}
	if !validRepositoryIdentity(trace.Repository) {
		return errors.New("trace repository affinity is invalid")
	}
	if len(trace.EnvironmentKeys) > MaximumEnvironmentKeys {
		return fmt.Errorf("trace exceeds the %d-environment-key bound", MaximumEnvironmentKeys)
	}
	seenEnvironmentKeys := map[string]struct{}{}
	for _, key := range trace.EnvironmentKeys {
		if !environmentKeyPattern.MatchString(key) {
			return fmt.Errorf("trace environment key %q is invalid", key)
		}
		if _, duplicate := seenEnvironmentKeys[key]; duplicate {
			return fmt.Errorf("trace environment key %q is duplicated", key)
		}
		seenEnvironmentKeys[key] = struct{}{}
	}
	if len(trace.Artifacts) > MaximumTraceArtifacts {
		return fmt.Errorf("trace exceeds the %d-artifact bound", MaximumTraceArtifacts)
	}
	if len(trace.Tests) > MaximumTraceTests {
		return fmt.Errorf("trace exceeds the %d-test bound", MaximumTraceTests)
	}
	if len(trace.Sources) > MaximumTraceSources {
		return fmt.Errorf("trace exceeds the %d-source bound", MaximumTraceSources)
	}
	seenArtifacts := map[string]struct{}{}
	totalExecutedRanges := 0
	for _, artifact := range trace.Artifacts {
		if !validRelativePath(artifact.Path) {
			return errors.New("trace artifact has an invalid path")
		}
		if artifact.Statements < 0 || artifact.ExecutedStatements < 0 || artifact.ExecutedStatements > artifact.Statements {
			return fmt.Errorf("trace artifact %q has invalid statement counts", artifact.Path)
		}
		if !validDigest(artifact.SourceSHA256) || artifact.SourceSizeBytes < 0 {
			return fmt.Errorf("trace artifact %q has invalid source identity", artifact.Path)
		}
		if _, duplicate := seenArtifacts[artifact.Path]; duplicate {
			return fmt.Errorf("trace artifact %q is duplicated", artifact.Path)
		}
		seenArtifacts[artifact.Path] = struct{}{}
		if len(artifact.ExecutedRanges) > MaximumExecutedRanges {
			return fmt.Errorf("trace artifact %q exceeds the range bound", artifact.Path)
		}
		if (artifact.ExecutedStatements == 0) != (len(artifact.ExecutedRanges) == 0) {
			return fmt.Errorf("trace artifact %q has inconsistent executed statements and ranges", artifact.Path)
		}
		if len(artifact.ExecutedRanges) > MaximumTotalExecutedRanges-totalExecutedRanges {
			return fmt.Errorf("trace exceeds the %d-total-executed-range bound", MaximumTotalExecutedRanges)
		}
		totalExecutedRanges += len(artifact.ExecutedRanges)
		for _, span := range artifact.ExecutedRanges {
			if span.StartLine <= 0 || span.EndLine < span.StartLine || span.Count <= 0 {
				return fmt.Errorf("trace artifact %q has an invalid executed range", artifact.Path)
			}
		}
	}
	seenTests := map[string]struct{}{}
	for _, test := range trace.Tests {
		if !validTraceTestIdentity(test) || test.Elapsed < 0 || test.Elapsed > MaximumTestElapsedMS {
			return errors.New("trace test has invalid identity or elapsed time")
		}
		identity := test.Package + "\x00" + test.Name
		if _, duplicate := seenTests[identity]; duplicate {
			return fmt.Errorf("trace test %q is duplicated", qualifiedTestName(test))
		}
		seenTests[identity] = struct{}{}
		switch test.Status {
		case "pass", "fail", "skip":
		default:
			return errors.New("trace test has an invalid status")
		}
	}
	seenSources := map[string]struct{}{}
	for _, source := range trace.Sources {
		if strings.TrimSpace(source.Kind) == "" || !validRelativePath(source.Path) || !validDigest(source.SHA256) || source.SizeBytes < 0 {
			return errors.New("trace source is invalid")
		}
		identity := source.Kind + "\x00" + source.Path
		if _, duplicate := seenSources[identity]; duplicate {
			return fmt.Errorf("trace source %q is duplicated", source.Path)
		}
		seenSources[identity] = struct{}{}
	}
	return nil
}

// IDFor computes the content-bound trace identity.
func IDFor(trace Trace) string {
	trace.ID = ""
	data, err := json.Marshal(trace)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validRelativePath(value string) bool {
	if value == "" || !utf8.ValidString(value) || containsControl(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func containsControl(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func validRepositoryIdentity(repository TraceRepository) bool {
	if len(repository.RepositoryID) == 0 || len(repository.RepositoryID) > 256 || containsControl(repository.RepositoryID) ||
		!validDigest(repository.ContentDigest) || repository.ArtifactCount < 0 || repository.ArtifactCount > captureMaximumFiles {
		return false
	}
	if repository.GitUnavailable {
		return repository.GitCommit == ""
	}
	return validGitCommit(repository.GitCommit)
}

func validGitCommit(value string) bool {
	if (len(value) != 40 && len(value) != sha256.Size*2) || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validTraceTestIdentity(test TraceTest) bool {
	if len(test.Name) == 0 || len(test.Name) > MaximumTestIdentifierBytes || containsControl(test.Name) {
		return false
	}
	name := test.Name
	if test.SubtestsRedacted {
		if !strings.HasSuffix(name, redactedSubtestSuffix) {
			return false
		}
		name = strings.TrimSuffix(name, redactedSubtestSuffix)
	} else if strings.Contains(name, "/") {
		return false
	}
	if !testIdentityPattern.MatchString(name) {
		return false
	}
	if len(secrets.Scan([]byte(name))) > 0 {
		return false
	}
	if len(test.Package) > MaximumPackageIdentifierBytes || containsControl(test.Package) {
		return false
	}
	if test.PackageRedacted {
		return test.Package == redactedPackage
	}
	return (test.Package == "" || packageIdentityPattern.MatchString(test.Package)) &&
		len(secrets.Scan([]byte(test.Package))) == 0
}

// CaptureOptions controls one authorized runtime capture.
type CaptureOptions struct {
	// Repository is the repository root the command runs in.
	Repository string
	// Command is the exact argument vector; no shell is used.
	Command []string
	// Timeout bounds the capture (default 30 minutes).
	Timeout time.Duration
	// EnvironmentKeys is the explicit, bounded list of environment-variable
	// NAMES selected for trace metadata (never values). An empty list records
	// none; Capture never enumerates the host environment implicitly.
	EnvironmentKeys []string
}

// CaptureResult is the outcome of one authorized capture.
type CaptureResult struct {
	Trace     Trace
	Stdout    []byte
	Stderr    []byte
	Truncated bool
}

// Capture runs an authorized command in the repository and records the
// deterministic runtime evidence. When the command is a Go test run, coverage
// and test-event output are captured automatically unless already requested.
func Capture(ctx context.Context, options CaptureOptions) (CaptureResult, error) {
	if ctx == nil {
		return CaptureResult{}, errors.New("trace capture context is required")
	}
	if strings.TrimSpace(options.Repository) == "" {
		return CaptureResult{}, errors.New("trace capture repository is required")
	}
	repositoryRoot, err := filepath.Abs(options.Repository)
	if err != nil {
		return CaptureResult{}, fmt.Errorf("resolve trace capture repository: %w", err)
	}
	options.Repository = repositoryRoot
	if len(options.Command) == 0 || strings.TrimSpace(options.Command[0]) == "" {
		return CaptureResult{}, errors.New("trace capture command is required")
	}
	environmentKeys, err := normalizeEnvironmentKeys(options.EnvironmentKeys)
	if err != nil {
		return CaptureResult{}, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	command, coveragePath, instrumented, err := prepareCommand(options.Command)
	if err != nil {
		return CaptureResult{}, err
	}
	if coveragePath != "" {
		defer os.Remove(coveragePath)
	}
	beforeAffinity, beforeArtifacts, err := repositoryAffinity(ctx, options.Repository)
	if err != nil {
		return CaptureResult{}, err
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	process := exec.CommandContext(runContext, command[0], command[1:]...)
	process.Dir = options.Repository
	var stdout, stderr boundedBuffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	started := time.Now()
	runErr := process.Run()
	duration := time.Since(started)
	if runContext.Err() != nil {
		if ctx.Err() != nil {
			return CaptureResult{}, ctx.Err()
		}
		return CaptureResult{}, fmt.Errorf("trace capture timed out after %s", timeout)
	}
	if runErr != nil && process.ProcessState == nil {
		return CaptureResult{}, fmt.Errorf("start trace capture command: %w", runErr)
	}
	if stdout.truncated || stderr.truncated {
		return CaptureResult{}, fmt.Errorf("trace capture output exceeded the %d-byte per-stream safety bound", MaximumCaptureOutputBytes)
	}
	afterAffinity, _, err := repositoryAffinity(ctx, options.Repository)
	if err != nil {
		return CaptureResult{}, err
	}
	if !sameRepositoryAffinity(beforeAffinity, afterAffinity) {
		return CaptureResult{}, errors.New("trace capture changed or observed a changing repository; normalize the tree and capture again")
	}
	commandName, commandRedacted := safeCommandIdentity(options.Command[0])
	commandDigest := sha256.Sum256(mustJSON(commandShape(options.Command)))
	result := CaptureResult{
		Trace: Trace{
			SchemaVersion:    SchemaVersion,
			Command:          commandName,
			CommandRedacted:  commandRedacted,
			CommandSHA256:    hex.EncodeToString(commandDigest[:]),
			WorkingDirectory: ".",
			DurationMS:       duration.Milliseconds(),
			EnvironmentKeys:  environmentKeys,
			Repository:       beforeAffinity,
		},
		Stdout: stdout.bytes(), Stderr: stderr.bytes(),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if process.ProcessState != nil {
		result.Trace.ExitCode = process.ProcessState.ExitCode()
	}
	if coveragePath != "" {
		if info, err := os.Stat(coveragePath); err != nil {
			return CaptureResult{}, fmt.Errorf("inspect captured coverage: %w", err)
		} else if info.Size() > MaximumTraceBytes {
			return CaptureResult{}, fmt.Errorf("captured coverage is %d bytes; maximum is %d", info.Size(), MaximumTraceBytes)
		}
		coverage, err := parseCoverageFile(coveragePath)
		if err != nil {
			return CaptureResult{}, fmt.Errorf("parse captured coverage: %w", err)
		}
		result.Trace.Artifacts, err = bindTraceArtifacts(options.Repository, coverage, beforeArtifacts)
		if err != nil {
			return CaptureResult{}, err
		}
		digest, size := fileDigest(coveragePath)
		if digest == "" {
			return CaptureResult{}, errors.New("digest captured coverage")
		}
		result.Trace.Sources = append(result.Trace.Sources, TraceSource{Kind: "coverage", Path: "coverage.out", SHA256: digest, SizeBytes: size})
	}
	if instrumented {
		tests, err := parseGoTestJSON(stdout.bytes())
		if err != nil {
			return CaptureResult{}, fmt.Errorf("parse captured test events: %w", err)
		}
		result.Trace.Tests = tests
		eventDigest := sha256.Sum256(stdout.bytes())
		result.Trace.Sources = append(result.Trace.Sources, TraceSource{
			Kind: "test-events", Path: "go-test.jsonl", SHA256: hex.EncodeToString(eventDigest[:]), SizeBytes: int64(len(stdout.bytes())),
		})
	}
	result.Trace.ID = IDFor(result.Trace)
	if err := Validate(result.Trace); err != nil {
		return CaptureResult{}, fmt.Errorf("captured trace is invalid: %w", err)
	}
	result.Trace.captureIntegrityBound = true
	result.Trace.captureIntegrityAuthenticated = true
	result.Trace.integrityAuthenticatedID = result.Trace.ID
	markCurrentProcessCaptureIntegrity(result.Trace)
	return result, nil
}

func normalizeEnvironmentKeys(keys []string) ([]string, error) {
	if len(keys) > MaximumEnvironmentKeys {
		return nil, fmt.Errorf("trace capture received %d environment keys; maximum is %d", len(keys), MaximumEnvironmentKeys)
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if !environmentKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("trace environment key %q is invalid", key)
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	sort.Strings(result)
	return result, nil
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// commandShape removes every argument value before persistence. The digest
// still distinguishes the admitted executable class, option/value shape,
// argument count, and positional layout without turning caller-controlled
// names or low-entropy values into hash oracles.
func commandShape(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	commandName, _ := safeCommandIdentity(command[0])
	shape := []string{commandName}
	for _, argument := range command[1:] {
		if strings.HasPrefix(argument, "-") {
			if strings.Contains(argument, "=") {
				shape = append(shape, "<option=value>")
			} else {
				shape = append(shape, "<option>")
			}
			continue
		}
		shape = append(shape, "<argument>")
	}
	return shape
}

func safeCommandIdentity(executable string) (string, bool) {
	base := strings.ToLower(filepath.Base(executable))
	if base == "go" || base == "go.exe" {
		return base, false
	}
	return redactedCommand, true
}

// prepareCommand returns the effective argument vector plus the temporary
// coverage path and an instrumentation flag. Go test runs are automatically
// instrumented with statement coverage and JSON test events unless the caller
// already requested them (which the capture refuses to overwrite).
func prepareCommand(command []string) ([]string, string, bool, error) {
	if len(command) == 0 {
		return nil, "", false, errors.New("capture command is empty")
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(command[0])), ".exe")
	if base != "go" || len(command) < 2 || command[1] != "test" {
		return append([]string(nil), command...), "", false, nil
	}
	effective := append([]string(nil), command...)
	for index := 2; index < len(effective); index++ {
		argument := effective[index]
		if argument == "-coverprofile" || strings.HasPrefix(argument, "-coverprofile=") {
			return nil, "", false, errors.New("trace capture refuses to overwrite an explicit -coverprofile; remove the flag")
		}
		if argument == "-json" {
			return nil, "", false, errors.New("trace capture refuses to overwrite explicit -json output; remove the flag")
		}
	}
	coverageFile, err := os.CreateTemp("", "rkc-trace-coverage-*.out")
	if err != nil {
		return nil, "", false, fmt.Errorf("create coverage capture file: %w", err)
	}
	coveragePath := coverageFile.Name()
	if err := coverageFile.Close(); err != nil {
		_ = os.Remove(coveragePath)
		return nil, "", false, fmt.Errorf("close coverage capture file: %w", err)
	}
	effective = append(effective,
		"-covermode=set", "-coverprofile="+coveragePath, "-json",
	)
	return effective, coveragePath, true, nil
}

type boundedBuffer struct {
	data      []byte
	truncated bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	room := MaximumCaptureOutputBytes - len(buffer.data)
	if room <= 0 {
		buffer.truncated = true
		return len(data), nil
	}
	if len(data) > room {
		buffer.data = append(buffer.data, data[:room]...)
		buffer.truncated = true
		return len(data), nil
	}
	buffer.data = append(buffer.data, data...)
	return len(data), nil
}

func (buffer *boundedBuffer) bytes() []byte {
	return append([]byte(nil), buffer.data...)
}

func fileDigest(path string) (string, int64) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0
	}
	return hex.EncodeToString(hash.Sum(nil)), size
}
