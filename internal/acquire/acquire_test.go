package acquire

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenLocal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	result, err := Open(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != KindLocal || result.Temporary {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestOpenFileGitURLAndCleanup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	base := t.TempDir()
	source := filepath.Join(base, "source")
	bare := filepath.Join(base, "repository.git")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=RKC", "GIT_AUTHOR_EMAIL=rkc@example.invalid", "GIT_COMMITTER_NAME=RKC", "GIT_COMMITTER_EMAIL=rkc@example.invalid")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	run(source, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "README.md")
	run(source, "commit", "--quiet", "-m", "fixture")
	run(base, "clone", "--quiet", "--bare", source, bare)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := Open(ctx, "file://"+bare, Options{AllowFileURLs: true, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	materialized := result.Root
	if _, err := os.Stat(filepath.Join(materialized, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := result.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(materialized); !os.IsNotExist(err) {
		t.Fatalf("materialized repository still exists: %v", err)
	}
}

func TestRejectFileURLByDefault(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), "file:///tmp/example.git", Options{}); err == nil {
		t.Fatal("expected file URL rejection")
	}
}
