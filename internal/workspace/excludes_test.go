package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/inventory"
)

func TestExcludePatternSemantics(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		match         bool
	}{
		{"**/*.pt", "weights.pt", true}, {"**/*.pt", "runs/new/weights.pt", true},
		{"**/*.pt", "weights.pt.json", false}, {"*.pt", "runs/weights.pt", false},
		{"**/node_modules/**", "node_modules", true}, {"**/node_modules/**", "web/node_modules/package/index.js", true},
		{"**/node_modules/**", "node_modules_readme.md", false},
		{"reports/*/metrics?.json", "reports/new/metrics1.json", true},
		{"reports/*/metrics?.json", "reports/new/nested/metrics1.json", false},
		{"**/**/**/README.md", "a/b/c/README.md", true},
	} {
		t.Run(tc.pattern+"_"+tc.name, func(t *testing.T) {
			if err := ValidateExcludePatterns([]string{tc.pattern}); err != nil {
				t.Fatal(err)
			}
			if got := matchExcludePattern(strings.Split(tc.pattern, "/"), strings.Split(tc.name, "/")); got != tc.match {
				t.Fatalf("match = %v, want %v", got, tc.match)
			}
		})
	}
}

func TestExcludePatternValidationRejectsAmbiguity(t *testing.T) {
	for _, pattern := range []string{"", " ", "/tmp/**", "../**", "a/../b", "a//b", "a/", `a\b`, "C:/**", "a/**b", "***", "a/[", "a/\n"} {
		if err := ValidateExcludePatterns([]string{pattern}); err == nil {
			t.Errorf("accepted %q", pattern)
		}
	}
	if err := ValidateExcludePatterns([]string{"*.pt", "*.pt"}); err == nil {
		t.Fatal("accepted duplicate pattern")
	}
	if err := ValidateExcludePatterns(make([]string, MaximumExcludePatterns+1)); err == nil {
		t.Fatal("accepted unbounded patterns")
	}
}

func TestResolveExclusionsTracksNewArtifactsWithoutReadingThem(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte("useful source\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main.go")
	write("reports/metrics.json")
	write("web/node_modules/private.txt")
	write(".git/config")
	patterns := []string{"**/node_modules/**", "**/*.pt"}
	first, err := ResolveExclusions(context.Background(), root, []string{".git"}, patterns, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, []string{".git", "web/node_modules"}) {
		t.Fatalf("unexpected exclusions: %v", first)
	}
	write("reports/new/checkpoint.pt")
	checkpoint, err := os.OpenFile(filepath.Join(root, "reports/new/checkpoint.pt"), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse checkpoint exceeds the whole inventory budget. Successful
	// inventory below proves that expansion excluded it before any file read.
	if err := checkpoint.Truncate(64 << 20); err != nil {
		checkpoint.Close()
		t.Fatal(err)
	}
	checkpoint.Close()
	second, err := ResolveExclusions(context.Background(), root, []string{".git"}, patterns, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, []string{".git", "reports/new/checkpoint.pt", "web/node_modules"}) {
		t.Fatalf("new checkpoint was not tracked: %v", second)
	}
	result, err := inventory.ScanContext(context.Background(), inventory.Options{Root: root, Excludes: second, MaxRepositoryBytes: 1024, MaxFiles: 30})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, artifact := range result.Artifacts {
		statuses[artifact.Path] = artifact.Status
	}
	if statuses["reports/new/checkpoint.pt"] != "excluded" || statuses["reports/metrics.json"] != "text" || statuses["src/main.go"] != "text" {
		t.Fatalf("lost accounting or useful content: %v", statuses)
	}
}

func TestResolveExclusionsCancellationAndPathBounds(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := ResolveExclusions(context.Background(), root, nil, []string{"*.pt"}, 2); err == nil || result != nil {
		t.Fatalf("path bound returned partial exclusions: %v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := ResolveExclusions(ctx, root, nil, []string{"*.pt"}, 30); !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("cancellation returned partial exclusions: %v %v", result, err)
	}
}

func TestResolveExclusionsDoesNotFollowSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.pt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("native symlink unavailable: %v", err)
	}
	result, err := ResolveExclusions(context.Background(), root, nil, []string{"**/*.pt"}, 10)
	if err != nil || len(result) != 0 {
		t.Fatalf("followed source symlink: %v %v", result, err)
	}
	if _, err := ResolveExclusions(context.Background(), filepath.Join(root, "linked"), nil, []string{"**/*.pt"}, 10); err == nil {
		t.Fatal("accepted symlink source root")
	}
}
