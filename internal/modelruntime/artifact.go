package modelruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrModelArtifactIntegrity = errors.New("model runtime artifact integrity check failed")

const ggufMagic = "GGUF"

const artifactPriorityIntervalBytes = int64(64 * 1024 * 1024)

// boundArtifact keeps an open reference to the exact inode that was verified.
// On Linux the child receives a /proc reference to that descriptor, so pathname
// replacement cannot redirect inference to a different executable or model.
// Path and content checks still run immediately before and after every process
// because an already-open regular file can be modified in place.
type boundArtifact struct {
	label         string
	path          string
	expected      string
	size          int64
	executable    bool
	requireGGUF   bool
	priorityCheck func() error
	identity      os.FileInfo
	file          *os.File
}

func bindExecutable(path, expectedSHA256 string, priorityChecks ...func() error) (*boundArtifact, error) {
	if _, err := normalizeExpectedSHA256(expectedSHA256, "llama.cpp executable"); err != nil {
		return nil, err
	}
	resolved := path
	if filepath.Base(path) == path {
		var err error
		resolved, err = exec.LookPath(path)
		if err != nil {
			return nil, fmt.Errorf("resolve llama.cpp executable: %w", err)
		}
	}
	return bindArtifact("llama.cpp executable", resolved, expectedSHA256, true, false, firstPriorityCheck(priorityChecks))
}

func bindModel(path, expectedSHA256 string, priorityChecks ...func() error) (*boundArtifact, error) {
	return bindArtifact("GGUF model", path, expectedSHA256, false, true, firstPriorityCheck(priorityChecks))
}

func firstPriorityCheck(checks []func() error) func() error {
	if len(checks) == 0 {
		return nil
	}
	return checks[0]
}

func bindArtifact(label, path, expectedSHA256 string, executable, requireGGUF bool, priorityCheck func() error) (*boundArtifact, error) {
	expected, err := normalizeExpectedSHA256(expectedSHA256, label)
	if err != nil {
		return nil, err
	}
	canonical, err := resolveCanonicalArtifactPath(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", label, err)
	}
	file, err := os.Open(canonical)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	artifact := &boundArtifact{
		label:         label,
		path:          canonical,
		expected:      expected,
		executable:    executable,
		requireGGUF:   requireGGUF,
		priorityCheck: priorityCheck,
		file:          file,
	}
	identity, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect %s: %w", label, err)
	}
	artifact.identity = identity
	artifact.size = identity.Size()
	if err := artifact.verify(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return artifact, nil
}

func normalizeExpectedSHA256(value, label string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%w: expected SHA-256 for %s is required", ErrModelArtifactIntegrity, label)
	}
	if value != strings.TrimSpace(value) || value != strings.ToLower(value) {
		return "", fmt.Errorf("%w: expected SHA-256 for %s must be canonical lowercase hex", ErrModelArtifactIntegrity, label)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("%w: expected SHA-256 for %s must be 64 lowercase hex characters", ErrModelArtifactIntegrity, label)
	}
	return value, nil
}

func resolveCanonicalArtifactPath(path string) (string, error) {
	if path == "" || path != strings.TrimSpace(path) {
		return "", errors.New("artifact path is empty or has surrounding whitespace")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func (artifact *boundArtifact) referencePath() string {
	if runtime.GOOS == "linux" && artifact != nil && artifact.file != nil {
		return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), artifact.file.Fd())
	}
	if artifact == nil {
		return ""
	}
	return artifact.path
}

func (artifact *boundArtifact) verify() error {
	if artifact == nil || artifact.file == nil || artifact.identity == nil {
		return fmt.Errorf("%w: bound artifact is closed or incomplete", ErrModelArtifactIntegrity)
	}
	if err := artifact.verifyMetadata(); err != nil {
		return err
	}
	digest, size, err := digestOpenFile(artifact.file, artifact.size, artifact.priorityCheck)
	if err != nil {
		return fmt.Errorf("%w: hash %s: %w", ErrModelArtifactIntegrity, artifact.label, err)
	}
	if size != artifact.size {
		return fmt.Errorf("%w: %s size changed from %d to %d", ErrModelArtifactIntegrity, artifact.label, artifact.size, size)
	}
	if digest != artifact.expected {
		return fmt.Errorf("%w: %s SHA-256 mismatch: expected %s, got %s", ErrModelArtifactIntegrity, artifact.label, artifact.expected, digest)
	}
	if artifact.requireGGUF {
		magic := make([]byte, len(ggufMagic))
		if _, err := artifact.file.ReadAt(magic, 0); err != nil {
			return fmt.Errorf("%w: read GGUF magic: %v", ErrModelArtifactIntegrity, err)
		}
		if string(magic) != ggufMagic {
			return fmt.Errorf("%w: model does not have GGUF magic", ErrModelArtifactIntegrity)
		}
	}
	return artifact.verifyMetadata()
}

func (artifact *boundArtifact) verifyMetadata() error {
	currentCanonical, err := filepath.EvalSymlinks(artifact.path)
	if err != nil {
		return fmt.Errorf("%w: resolve current %s path: %v", ErrModelArtifactIntegrity, artifact.label, err)
	}
	currentCanonical, err = filepath.Abs(currentCanonical)
	if err != nil || filepath.Clean(currentCanonical) != artifact.path {
		return fmt.Errorf("%w: canonical %s path changed", ErrModelArtifactIntegrity, artifact.label)
	}
	pathname, err := os.Lstat(artifact.path)
	if err != nil {
		return fmt.Errorf("%w: inspect current %s path: %v", ErrModelArtifactIntegrity, artifact.label, err)
	}
	opened, err := artifact.file.Stat()
	if err != nil {
		return fmt.Errorf("%w: inspect open %s: %v", ErrModelArtifactIntegrity, artifact.label, err)
	}
	if !pathname.Mode().IsRegular() || !opened.Mode().IsRegular() {
		return fmt.Errorf("%w: %s must remain a regular file", ErrModelArtifactIntegrity, artifact.label)
	}
	if !os.SameFile(artifact.identity, pathname) || !os.SameFile(artifact.identity, opened) {
		return fmt.Errorf("%w: %s inode changed", ErrModelArtifactIntegrity, artifact.label)
	}
	if pathname.Size() != artifact.size || opened.Size() != artifact.size {
		return fmt.Errorf("%w: %s size changed", ErrModelArtifactIntegrity, artifact.label)
	}
	if pathname.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s must not be group/other writable", ErrModelArtifactIntegrity, artifact.label)
	}
	if artifact.executable && runtime.GOOS != "windows" && pathname.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: llama.cpp executable has no execute bit", ErrModelArtifactIntegrity)
	}
	return nil
}

func digestOpenFile(file *os.File, expectedSize int64, priorityChecks ...func() error) (string, int64, error) {
	return digestOpenFileWithInterval(file, expectedSize, artifactPriorityIntervalBytes, firstPriorityCheck(priorityChecks))
}

func digestOpenFileWithInterval(file *os.File, expectedSize, priorityInterval int64, priorityCheck func() error) (string, int64, error) {
	if expectedSize < 0 {
		return "", 0, errors.New("negative artifact size")
	}
	if priorityInterval <= 0 {
		return "", 0, errors.New("artifact priority interval must be positive")
	}
	checkPriority := func() error {
		if priorityCheck == nil {
			return nil
		}
		if err := priorityCheck(); err != nil {
			return fmt.Errorf("artifact hashing deferred: %w", err)
		}
		return nil
	}
	if err := checkPriority(); err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	bufferSize := int64(1024 * 1024)
	if priorityInterval < bufferSize {
		bufferSize = priorityInterval
	}
	buffer := make([]byte, int(bufferSize))
	var offset int64
	var sincePriorityCheck int64
	for offset < expectedSize {
		remaining := expectedSize - offset
		readSize := len(buffer)
		if remaining < int64(readSize) {
			readSize = int(remaining)
		}
		read, err := file.ReadAt(buffer[:readSize], offset)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			offset += int64(read)
			sincePriorityCheck += int64(read)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", offset, err
		}
		if read != readSize {
			return "", offset, errors.New("artifact size changed while hashing")
		}
		if sincePriorityCheck >= priorityInterval {
			if err := checkPriority(); err != nil {
				return "", offset, err
			}
			sincePriorityCheck = 0
		}
	}
	extra := []byte{0}
	if read, err := file.ReadAt(extra, expectedSize); read != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return "", offset + int64(read), errors.New("artifact size changed while hashing")
	}
	if err := checkPriority(); err != nil {
		return "", offset, err
	}
	return hex.EncodeToString(hash.Sum(nil)), offset, nil
}
