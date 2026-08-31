package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceGitAffinityRequiresExactWorkTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	gitDirectoryBefore, gitDirectoryWasSet := os.LookupEnv("GIT_DIR")
	gitWorkTreeBefore, gitWorkTreeWasSet := os.LookupEnv("GIT_WORK_TREE")
	t.Cleanup(func() {
		if gitDirectoryWasSet {
			_ = os.Setenv("GIT_DIR", gitDirectoryBefore)
		} else {
			_ = os.Unsetenv("GIT_DIR")
		}
		if gitWorkTreeWasSet {
			_ = os.Setenv("GIT_WORK_TREE", gitWorkTreeBefore)
		} else {
			_ = os.Unsetenv("GIT_WORK_TREE")
		}
	})
	if err := os.Unsetenv("GIT_DIR"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("GIT_WORK_TREE"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, arguments := range [][]string{
		{"init", "--quiet", root},
		{"-C", root, "config", "user.name", "RKC fixture"},
		{"-C", root, "config", "user.email", "rkc@example.invalid"},
	} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"-C", root, "add", "fixture.txt"},
		{"-C", root, "commit", "--quiet", "-m", "fixture"},
	} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}

	affinity, err := traceGitAffinity(context.Background(), root)
	if err != nil || affinity.unavailable || affinity.commit == "" {
		t.Fatalf("exact work-tree affinity = %+v, err %v", affinity, err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	affinity, err = traceGitAffinity(context.Background(), nested)
	if err != nil || !affinity.unavailable || affinity.commit != "" || affinity.origin != "" {
		t.Fatalf("nested plain-folder affinity = %+v, err %v", affinity, err)
	}

	gitDirectoryOutput, err := exec.Command("git", "-C", root, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	t.Setenv("GIT_DIR", strings.TrimSpace(string(gitDirectoryOutput)))
	t.Setenv("GIT_WORK_TREE", external)
	affinity, err = traceGitAffinity(context.Background(), external)
	if err != nil || affinity.unavailable || affinity.commit == "" {
		t.Fatalf("external work-tree affinity = %+v, err %v", affinity, err)
	}

	t.Setenv("GIT_WORK_TREE", "")
	affinity, err = traceGitAffinity(context.Background(), external)
	if err != nil || !affinity.unavailable || affinity.commit != "" {
		t.Fatalf("unpaired environment affinity = %+v, err %v", affinity, err)
	}
}
