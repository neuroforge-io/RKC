package acquire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeGitExecutable(t *testing.T, failCall int) (string, string) {
	t.Helper()
	root := t.TempDir()
	logPath := filepath.Join(root, "calls.log")
	executable := filepath.Join(root, "fake-git")
	content := fmt.Sprintf(`#!/bin/sh
set -eu
log=%s
count=0
if [ -f "$log" ]; then
    count=$(wc -l < "$log")
fi
count=$((count + 1))
printf '%%s\n' "$*" >> "$log"
if [ "$count" -eq %d ]; then
    printf 'fatal: https://fixture:verysecret@example.test/private.git\n' >&2
    exit 19
fi
`, strconv.Quote(logPath), failCall)
	if err := os.WriteFile(executable, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable, logPath
}

func TestRemoteValidationAndRedaction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		source    string
		allowFile bool
		wantSCP   bool
		wantErr   string
	}{
		{"git@example.test:owner/repo.git", false, true, ""},
		{"https://example.test/owner/repo.git", false, false, ""},
		{"ssh://git@example.test/owner/repo.git", false, false, ""},
		{"git://example.test/owner/repo.git", false, false, ""},
		{"file:///tmp/repo.git", true, false, ""},
		{"file:///tmp/repo.git", false, false, "disabled"},
		{"ftp://example.test/repo.git", false, false, "unsupported"},
		{"https:///repo.git", false, false, "host"},
		{"not a url", false, false, "supported Git URL"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.source, func(t *testing.T) {
			parsed, scp, err := validateRemoteSource(tc.source, tc.allowFile)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || scp != tc.wantSCP {
				t.Fatalf("parsed=%v scp=%v err=%v", parsed, scp, err)
			}
		})
	}
	parsed, _, err := validateRemoteSource("https://alice:supersecret@example.test/repo.git", false)
	if err != nil {
		t.Fatal(err)
	}
	redacted := redactSource("https://alice:supersecret@example.test/repo.git", parsed, false)
	if redacted != "https://alice@example.test/repo.git" || strings.Contains(redacted, "supersecret") {
		t.Fatalf("credential was not redacted: %q", redacted)
	}
	if got := redactSecrets("fatal https://bob:password123@example.test/x"); got != "fatal https://<redacted>@example.test/x" {
		t.Fatalf("redactSecrets = %q", got)
	}
}

func TestOpenRejectsInvalidInputsAndCleansFailures(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("not a repository"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), file, Options{}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected file rejection, got %v", err)
	}
	if _, err := Open(context.Background(), "https://example.test/repo.git", Options{Depth: -1}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("expected depth rejection, got %v", err)
	}
	if _, err := Open(context.Background(), "https://example.test/repo.git", Options{GitExecutable: filepath.Join(t.TempDir(), "missing")}); err == nil || !strings.Contains(err.Error(), "find Git executable") {
		t.Fatalf("expected executable rejection, got %v", err)
	}
	temporaryRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(temporaryRoot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), "https://example.test/repo.git", Options{TemporaryRoot: temporaryRoot}); err == nil || !strings.Contains(err.Error(), "temporary root") {
		t.Fatalf("expected temporary-root rejection, got %v", err)
	}
}

func TestRunGitBoundsAndRedactsFailureOutput(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "fake-git")
	content := "#!/bin/sh\nprintf '%s' 'https://user:verysecret@example.test/repo ' >&2\nprintf '%04096d' 0 >&2\nexit 23\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	err := runGit(context.Background(), Options{GitExecutable: script, MaximumLogBytes: 128}, "https://user@example.test/repo", "clone")
	if err == nil {
		t.Fatal("expected fake Git failure")
	}
	message := err.Error()
	if strings.Contains(message, "verysecret") || !strings.Contains(message, "<redacted>") || !strings.Contains(message, "output truncated") {
		t.Fatalf("unsafe or unbounded error: %q", message)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = runGit(cancelled, Options{GitExecutable: script, MaximumLogBytes: 16}, "redacted", "clone")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestLimitedBufferAndCleanupIdempotentContract(t *testing.T) {
	t.Parallel()
	buffer := &limitedBuffer{limit: 4}
	if n, err := buffer.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := buffer.String(); got != "abcd" || !buffer.truncated {
		t.Fatalf("buffer = %q truncated=%v", got, buffer.truncated)
	}
	if n, err := buffer.Write([]byte("more")); err != nil || n != 4 || buffer.String() != "abcd" {
		t.Fatalf("second Write = %d, %v, %q", n, err, buffer.String())
	}
	if err := (Result{}).Cleanup(); err != nil {
		t.Fatal(err)
	}
	called := 0
	result := Result{cleanup: func() error { called++; return errors.New("cleanup") }}
	if err := result.Cleanup(); err == nil || called != 1 {
		t.Fatalf("cleanup err=%v called=%d", err, called)
	}
}

func TestOpenHonorsCancelledContext(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "slow-git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := Open(ctx, "https://example.test/repo.git", Options{GitExecutable: script, TemporaryRoot: base, Timeout: time.Second})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed acquisition leaked %d temporary entries", len(entries))
	}
}

func TestOpenRefSubmodulesAndKeepMaterialized(t *testing.T) {
	executable, logPath := fakeGitExecutable(t, 0)
	temporaryRoot := t.TempDir()
	source := "https://fixture:verysecret@example.test/private.git"
	result, err := Open(context.Background(), source, Options{
		GitExecutable:    executable,
		Ref:              "release-v1",
		Depth:            2,
		Submodules:       true,
		TemporaryRoot:    temporaryRoot,
		KeepMaterialized: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(result.Root)) })
	if result.Kind != KindGit || result.RequestedRef != "release-v1" || result.Temporary || result.MaterializedPath != result.Root {
		t.Fatalf("unexpected result: %+v", result)
	}
	if strings.Contains(result.RedactedSource, "verysecret") || result.RedactedSource != "https://fixture@example.test/private.git" {
		t.Fatalf("unsafe redacted source: %q", result.RedactedSource)
	}
	if err := result.Cleanup(); err != nil {
		t.Fatalf("kept result cleanup = %v", err)
	}
	if _, err := os.Stat(result.Root); err != nil {
		t.Fatalf("kept materialization missing: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	if len(lines) != 5 {
		t.Fatalf("Git call count = %d; log = %q", len(lines), logData)
	}
	for _, expected := range []string{" init --quiet", " remote add origin ", " fetch --no-tags --depth 2 origin release-v1", " checkout --quiet --detach FETCH_HEAD", " submodule update --init --recursive --depth 2"} {
		if !strings.Contains(string(logData), expected) {
			t.Fatalf("Git log does not contain %q: %s", expected, logData)
		}
	}
	if strings.Count(string(logData), "core.hooksPath=/dev/null") != 5 || strings.Count(string(logData), "protocol.file.allow=never") != 5 {
		t.Fatalf("safe Git configuration missing from calls: %s", logData)
	}
}

func TestOpenRefFailureStagesAreRedactedAndCleaned(t *testing.T) {
	for failCall := 1; failCall <= 5; failCall++ {
		failCall := failCall
		t.Run(fmt.Sprintf("call_%d", failCall), func(t *testing.T) {
			executable, _ := fakeGitExecutable(t, failCall)
			temporaryRoot := t.TempDir()
			_, err := Open(context.Background(), "https://fixture:verysecret@example.test/private.git", Options{
				GitExecutable: executable,
				Ref:           "release-v1",
				Depth:         2,
				Submodules:    true,
				TemporaryRoot: temporaryRoot,
			})
			if err == nil {
				t.Fatal("expected staged Git failure")
			}
			if strings.Contains(err.Error(), "verysecret") || !strings.Contains(err.Error(), "<redacted>") {
				t.Fatalf("unsafe Git error: %q", err)
			}
			entries, readErr := os.ReadDir(temporaryRoot)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed acquisition leaked %d temporary entries", len(entries))
			}
		})
	}
}

func TestOpenDefaultsAndFilesystemErrors(t *testing.T) {
	result, err := Open(context.Background(), "   ", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != KindLocal || result.Source != "." || !filepath.IsAbs(result.Root) {
		t.Fatalf("default local result: %+v", result)
	}

	executable, _ := fakeGitExecutable(t, 0)
	lockedParent := filepath.Join(t.TempDir(), "locked-parent")
	if err := os.Mkdir(lockedParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockedParent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedParent, 0o700) })
	_, inspectionErr := Open(context.Background(), filepath.Join(lockedParent, "missing"), Options{GitExecutable: executable})
	if inspectionErr == nil {
		t.Log("filesystem permissions did not deny traversal; inspection-error branch is unavailable")
	} else if !strings.Contains(inspectionErr.Error(), "inspect repository source") {
		t.Fatalf("inspection error = %v", inspectionErr)
	}

	lockedTemporaryRoot := filepath.Join(t.TempDir(), "locked-temporary-root")
	if err := os.Mkdir(lockedTemporaryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockedTemporaryRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedTemporaryRoot, 0o700) })
	_, temporaryErr := Open(context.Background(), "https://example.test/repo.git", Options{
		GitExecutable: executable,
		TemporaryRoot: lockedTemporaryRoot,
	})
	if temporaryErr == nil {
		t.Log("filesystem permissions permitted creation; MkdirTemp error branch is unavailable")
	} else if !strings.Contains(temporaryErr.Error(), "create acquisition directory") {
		t.Fatalf("temporary directory error = %v", temporaryErr)
	}
}

func TestOpenReportsDeletedWorkingDirectory(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	working := filepath.Join(base, "working")
	if err := os.Mkdir(working, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(working); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Remove(working); err != nil {
		t.Skipf("filesystem does not permit removal of the current directory: %v", err)
	}
	if _, err := Open(context.Background(), ".", Options{}); err == nil || !strings.Contains(err.Error(), "resolve repository source") {
		t.Fatalf("deleted working directory error = %v", err)
	}
}

func TestAdditionalURLRedactionCases(t *testing.T) {
	if parsed, scp, err := validateRemoteSource("https://example.test/%zz", false); err == nil || parsed != nil || scp {
		t.Fatalf("malformed URL = %v, %v, %v", parsed, scp, err)
	}
	if got := redactSource("git@example.test:repo.git", nil, true); got != "git@example.test:repo.git" {
		t.Fatalf("SCP-style source = %q", got)
	}
	parsed, _, err := validateRemoteSource("https://:password@example.test/repo.git", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := redactSource("https://:password@example.test/repo.git", parsed, false); got != "https://example.test/repo.git" {
		t.Fatalf("empty username redaction = %q", got)
	}
}
