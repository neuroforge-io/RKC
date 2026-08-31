package gitworktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsExactRoot(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, candidate := range map[string]string{
		"empty":        "",
		"relative":     ".",
		"child":        child,
		"file":         file,
		"missing":      filepath.Join(root, "missing"),
		"nul":          root + "\x00suffix",
		"over maximum": string(filepath.Separator) + strings.Repeat("x", MaximumTopLevelBytes),
	} {
		t.Run(name, func(t *testing.T) {
			if IsExactRoot(root, candidate) {
				t.Fatalf("candidate %q matched root", name)
			}
		})
	}
	if IsExactRoot(".", root) {
		t.Fatal("relative requested root matched")
	}
	if !IsExactRoot(root, filepath.Join(root, ".")) {
		t.Fatal("same directory with an equivalent spelling did not match")
	}
}

func TestIsExactRootAcceptsDirectoryAlias(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if !IsExactRoot(root, alias) {
		t.Fatal("filesystem-identical directory alias did not match")
	}
}

func TestAffinityEnvironmentIsPaired(t *testing.T) {
	t.Setenv("GIT_DIR", "")
	t.Setenv("GIT_WORK_TREE", "")
	if !AffinityEnvironmentIsPaired() {
		t.Fatal("absent affinity environment was rejected")
	}
	t.Setenv("GIT_DIR", "/repository")
	if AffinityEnvironmentIsPaired() {
		t.Fatal("unpaired GIT_DIR was accepted")
	}
	t.Setenv("GIT_WORK_TREE", "/work-tree")
	if !AffinityEnvironmentIsPaired() {
		t.Fatal("paired external work-tree environment was rejected")
	}
	t.Setenv("GIT_DIR", "")
	if AffinityEnvironmentIsPaired() {
		t.Fatal("unpaired GIT_WORK_TREE was accepted")
	}
}

func TestParseTopLevelOutput(t *testing.T) {
	for name, fixture := range map[string][]byte{
		"empty":            nil,
		"only newline":     []byte("\n"),
		"embedded LF":      []byte("/repo\nchild\n"),
		"embedded CR":      []byte("/repo\rchild\n"),
		"embedded NUL":     []byte("/repo\x00child\n"),
		"over maximum":     []byte(strings.Repeat("x", MaximumTopLevelBytes+1)),
		"oversized record": []byte(strings.Repeat("x", MaximumTopLevelBytes+2) + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if value, ok := ParseTopLevelOutput(fixture); ok {
				t.Fatalf("unsafe output parsed as %q", value)
			}
		})
	}
	for name, fixture := range map[string][]byte{
		"unix":               []byte("/repo\n"),
		"windows line end":   []byte("C:\\repo\r\n"),
		"no line end":        []byte("/repo"),
		"significant spaces": []byte(" /repo \n"),
	} {
		t.Run(name, func(t *testing.T) {
			value, ok := ParseTopLevelOutput(fixture)
			if !ok || value != strings.TrimSuffix(strings.TrimSuffix(string(fixture), "\n"), "\r") {
				t.Fatalf("parsed value = %q, ok=%t", value, ok)
			}
		})
	}
}
