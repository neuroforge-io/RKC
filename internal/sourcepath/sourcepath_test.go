package sourcepath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadFileAndOpenRegular(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "nested/value.txt", "bounded fixture\n")

	data, err := ReadFile(root, "nested/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "bounded fixture\n" {
		t.Fatalf("ReadFile = %q", got)
	}

	input, err := OpenRegular(root, "nested/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		t.Fatalf("opened file info = %v, %v", opened, err)
	}
}

func TestOpenRegularRejectsUnsafeRootsAndPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFixture(t, root, "plain.txt", "inside")
	writeFixture(t, outside, "secret.txt", "outside")

	rootFile := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(rootFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]string{
		"empty":        "",
		"nul":          "bad\x00root",
		"missing":      filepath.Join(t.TempDir(), "missing"),
		"regular file": rootFile,
		"symlink":      rootLink,
	} {
		t.Run("root_"+name, func(t *testing.T) {
			if input, err := OpenRegular(candidate, "plain.txt"); err == nil {
				input.Close()
				t.Fatalf("OpenRegular accepted unsafe root %q", candidate)
			}
		})
	}

	unsafePaths := []string{"", "bad\x00path", "/absolute", ".", "./plain.txt", "a/../plain.txt", "a//plain.txt", "../plain.txt"}
	for _, candidate := range unsafePaths {
		candidate := candidate
		t.Run("path_"+strings.ReplaceAll(candidate, "/", "_"), func(t *testing.T) {
			if input, err := OpenRegular(root, candidate); err == nil {
				input.Close()
				t.Fatalf("OpenRegular accepted unsafe path %q", candidate)
			}
		})
	}

	if input, err := OpenRegular(root, "missing.txt"); err == nil || !strings.Contains(err.Error(), "inspect source path") {
		if input != nil {
			input.Close()
		}
		t.Fatalf("missing path error = %v", err)
	}
	if input, err := OpenRegular(root, "plain.txt/child"); err == nil || !strings.Contains(err.Error(), "non-directory") {
		if input != nil {
			input.Close()
		}
		t.Fatalf("non-directory component error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if input, err := OpenRegular(root, "directory"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		if input != nil {
			input.Close()
		}
		t.Fatalf("directory leaf error = %v", err)
	}

	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leaf-link")); err != nil {
		t.Fatal(err)
	}
	if input, err := OpenRegular(root, "leaf-link"); err == nil || !strings.Contains(err.Error(), "symlink") {
		if input != nil {
			input.Close()
		}
		t.Fatalf("symlink leaf error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "directory-link")); err != nil {
		t.Fatal(err)
	}
	if input, err := OpenRegular(root, "directory-link/secret.txt"); err == nil || !strings.Contains(err.Error(), "symlink") {
		if input != nil {
			input.Close()
		}
		t.Fatalf("symlink component error = %v", err)
	}

	locked := writeFixture(t, root, "locked.txt", "private")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	if input, err := OpenRegular(root, "locked.txt"); err == nil {
		input.Close()
		t.Log("filesystem permissions did not deny the owner; open-error branch is unavailable")
	} else if !strings.Contains(err.Error(), "open source path") {
		t.Fatalf("permission error = %v", err)
	}
}

func TestResolveRelativeContainment(t *testing.T) {
	valid := []struct {
		base   string
		target string
		want   string
	}{
		{"docs", "guide.md", "docs/guide.md"},
		{"docs/api", "../README.md", "docs/README.md"},
		{"", "source.go", "source.go"},
		{".", "./source.go", "source.go"},
	}
	for _, tc := range valid {
		got, err := ResolveRelative(tc.base, tc.target)
		if err != nil || got != tc.want {
			t.Fatalf("ResolveRelative(%q, %q) = %q, %v; want %q", tc.base, tc.target, got, err, tc.want)
		}
	}
	invalid := []struct{ base, target string }{
		{"", ""},
		{"", "bad\x00path"},
		{"", "/absolute"},
		{"", "."},
		{"", ".."},
		{"docs", "../.."},
		{"/absolute-base", "child"},
	}
	for _, tc := range invalid {
		if got, err := ResolveRelative(tc.base, tc.target); err == nil {
			t.Fatalf("ResolveRelative(%q, %q) = %q, want error", tc.base, tc.target, got)
		}
	}
}

func unchangedFixture(t *testing.T) (string, os.FileInfo, []componentState) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository")
	file := writeFixture(t, root, "nested/value.txt", "fixture")
	rootInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(file)
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	return root, rootInfo, []componentState{{path: directory, info: directoryInfo}, {path: file, info: fileInfo}}
}

func TestVerifyUnchangedDetectsReplacementAndRemoval(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		root, rootInfo, states := unchangedFixture(t)
		if err := verifyUnchanged(root, rootInfo, states, "nested/value.txt"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("root removed", func(t *testing.T) {
		root, rootInfo, states := unchangedFixture(t)
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		if err := verifyUnchanged(root, rootInfo, states, "nested/value.txt"); err == nil || !strings.Contains(err.Error(), "reinspect repository root") {
			t.Fatalf("removed root error = %v", err)
		}
	})
	t.Run("root replaced", func(t *testing.T) {
		root, rootInfo, states := unchangedFixture(t)
		old := root + ".old"
		if err := os.Rename(root, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := verifyUnchanged(root, rootInfo, states, "nested/value.txt"); err == nil || !strings.Contains(err.Error(), "root changed") {
			t.Fatalf("replaced root error = %v", err)
		}
	})
	t.Run("component removed", func(t *testing.T) {
		root, rootInfo, states := unchangedFixture(t)
		if err := os.Remove(states[len(states)-1].path); err != nil {
			t.Fatal(err)
		}
		if err := verifyUnchanged(root, rootInfo, states, "nested/value.txt"); err == nil ||
			(!strings.Contains(err.Error(), "reinspect source path") && !strings.Contains(err.Error(), "changed while opening")) {
			t.Fatalf("removed component error = %v", err)
		}
	})
	t.Run("component replaced", func(t *testing.T) {
		root, rootInfo, states := unchangedFixture(t)
		leaf := states[len(states)-1].path
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(leaf, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyUnchanged(root, rootInfo, states, "nested/value.txt"); err == nil || !strings.Contains(err.Error(), "changed while opening") {
			t.Fatalf("replaced component error = %v", err)
		}
	})
	t.Run("component changed to symlink", func(t *testing.T) {
		root, rootInfo, states := unchangedFixture(t)
		leaf := states[len(states)-1].path
		outside := writeFixture(t, t.TempDir(), "outside.txt", "outside")
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, leaf); err != nil {
			t.Fatal(err)
		}
		if err := verifyUnchanged(root, rootInfo, states, "nested/value.txt"); err == nil || !strings.Contains(err.Error(), "changed while opening") {
			t.Fatalf("symlink component error = %v", err)
		}
	})
}

func TestErrorHelpersStripHostPaths(t *testing.T) {
	hostPath := filepath.Join(t.TempDir(), "private", "source")
	pathErr := &os.PathError{Op: "open", Path: hostPath, Err: fs.ErrPermission}
	source := sourceError("open", "public/source.go", pathErr)
	if !errors.Is(source, fs.ErrPermission) || strings.Contains(source.Error(), hostPath) || !strings.Contains(source.Error(), "public/source.go") {
		t.Fatalf("unsafe source error = %q", source)
	}
	root := rootError("inspect", pathErr)
	if !errors.Is(root, fs.ErrPermission) || strings.Contains(root.Error(), hostPath) {
		t.Fatalf("unsafe root error = %q", root)
	}
	sentinel := errors.New("sentinel")
	if got := underlyingPathError(sentinel); got != sentinel {
		t.Fatalf("underlyingPathError = %v", got)
	}
}

func TestReadFilePropagatesKernelReadFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux procfs fixture is required")
	}
	root := fmt.Sprintf("/proc/%d", os.Getpid())
	_, err := ReadFile(root, "mem")
	if err == nil {
		t.Fatal("reading procfs process memory from offset zero unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "read source path") {
		t.Skipf("kernel denied the fixture before the read stage: %v", err)
	}
}

func TestInspectRootReportsDeletedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit removal of the current directory")
	}
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
	if _, _, err := inspectRoot("."); err == nil || !strings.Contains(err.Error(), "resolve repository root") {
		t.Fatalf("deleted working directory error = %v", err)
	}
}
