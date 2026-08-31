package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/history"
)

func historyRepositoryFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit := func(arguments ...string) {
		t.Helper()
		command := exec.Command("git", arguments...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@e.com")
	runGit("config", "user.name", "T")
	source := "package app\n\nfunc Greet(name string) string { return \"hi \" + name }\n"
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "initial")
	changed := "package app\n\nfunc Greet(name, title string) string { return title + \" \" + name }\n"
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "warm greeting")
	return root
}

func TestHistoryBuildReportAndSymbolExecution(t *testing.T) {
	root := historyRepositoryFixture(t)
	out := filepath.Join(t.TempDir(), "history.json")
	options, err := parseHistoryBuild([]string{"--dir", root, "--out", out})
	if err != nil {
		t.Fatal(err)
	}
	if err := executeHistoryBuild(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var compiled history.History
	if err := json.Unmarshal(data, &compiled); err != nil {
		t.Fatal(err)
	}
	if compiled.SchemaVersion != history.SchemaVersion || len(compiled.Commits) != 2 ||
		filepath.IsAbs(compiled.Repository) || compiled.SourceRevision != compiled.Commit ||
		compiled.RevisionPolicy != history.RevisionPolicyExactHead ||
		compiled.AncestryPolicy != history.AncestryPolicyFirstParent || compiled.SourceID == "" {
		t.Fatalf("history = %+v", compiled)
	}
	if err := runHistory([]string{"report", "--history", out}); err != nil {
		t.Fatal(err)
	}
	if err := runHistory([]string{"report", "--history", out, "--json"}); err != nil {
		t.Fatal(err)
	}
	symbolOptions, err := parseHistorySymbol([]string{"--name", "Greet", "--dir", root})
	if err != nil {
		t.Fatal(err)
	}
	if err := executeHistorySymbol(context.Background(), symbolOptions); err != nil {
		t.Fatal(err)
	}
	symbolOptions.jsonOutput = true
	if err := executeHistorySymbol(context.Background(), symbolOptions); err != nil {
		t.Fatal(err)
	}
	symbolOptions.name = "Absent"
	if err := executeHistorySymbol(context.Background(), symbolOptions); err == nil {
		t.Fatal("absent symbol was found")
	}
}

func TestHistoryCommandValidation(t *testing.T) {
	root := historyRepositoryFixture(t)
	defaults, err := parseHistoryBuild([]string{"--dir", root})
	if err != nil || defaults.out != ".rkc-history.json" {
		t.Fatalf("safe default history output = %q, %v", defaults.out, err)
	}
	if err := runHistory(nil); err != nil {
		t.Fatal(err)
	}
	if err := runHistory([]string{"help"}); err != nil {
		t.Fatal(err)
	}
	if err := runHistory([]string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown history subcommand") {
		t.Fatalf("unknown subcommand = %v", err)
	}
	if err := runHistory([]string{"build"}); err == nil {
		t.Fatal("build without --dir succeeded")
	}
	if err := runHistory([]string{"build", "--dir", root, "--max-commits", "0"}); err == nil {
		t.Fatal("build with zero max commits succeeded")
	}
	if err := runHistory([]string{"build", "--dir", root, "--max-commits", "10001"}); err == nil {
		t.Fatal("build with oversized max commits succeeded")
	}
	if err := runHistory([]string{"report"}); err == nil {
		t.Fatal("report without --history succeeded")
	}
	if err := runHistory([]string{"report", "--history", filepath.Join(t.TempDir(), "absent.json")}); err == nil {
		t.Fatal("report of a missing history succeeded")
	}
	if err := runHistory([]string{"symbol", "--name", "X"}); err == nil {
		t.Fatal("symbol without --dir succeeded")
	}
	if _, err := parseHistorySymbol([]string{
		"--name", strings.Repeat("x", maximumHistorySymbolQueryBytes+1), "--dir", root,
	}); err == nil {
		t.Fatal("oversized symbol query succeeded")
	}
	if _, err := parseHistoryBuild([]string{"--dir", root + "\x1b", "--out", "history.json"}); err == nil {
		t.Fatal("control-bearing repository path succeeded")
	}
	if _, err := parseHistoryBuild([]string{"--dir", root, "--out", "bad\x1b[31m.json"}); err == nil {
		t.Fatal("control-bearing output path succeeded")
	}
	if _, err := parseHistorySymbol([]string{"--name", "bad\u202e", "--dir", root}); err == nil {
		t.Fatal("format-control symbol query succeeded")
	}
	if err := runHistory([]string{"report", "--history", "bad\x1b.json"}); err == nil {
		t.Fatal("control-bearing history path succeeded")
	}
}

func TestHistoryHumanOutputEscapesUntrustedPathControls(t *testing.T) {
	root := historyRepositoryFixture(t)
	out := filepath.Join(t.TempDir(), "history\x1b[31m.json")
	output, err := captureStdout(t, func() error {
		return executeHistoryBuild(context.Background(), historyBuildOptions{
			dir: root, out: out, maxCommits: history.DefaultMaxCommits,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(output, 0x1b) || !strings.Contains(output, `\u001B`) {
		t.Fatalf("unsafe terminal output = %q", output)
	}
}

func TestHistoryResourceAdmission(t *testing.T) {
	t.Run("non linux executes locally", func(t *testing.T) {
		localCalls := 0
		err := runHistoryAdmissionUsing(
			context.Background(), []string{"build"}, "darwin", "", false,
			func() error { t.Fatal("prepare called"); return nil },
			func(context.Context, func(context.Context) error) error { t.Fatal("protected called"); return nil },
			func(context.Context, []string) error { t.Fatal("launch called"); return nil },
			func(context.Context) error { localCalls++; return nil },
		)
		if err != nil || localCalls != 1 {
			t.Fatalf("admission = %v, local calls = %d", err, localCalls)
		}
	})

	t.Run("prepared linux process is monitored", func(t *testing.T) {
		protectedCalls := 0
		localCalls := 0
		err := runHistoryAdmissionUsing(
			context.Background(), []string{"symbol"}, "linux", "", false,
			func() error { return nil },
			func(ctx context.Context, work func(context.Context) error) error {
				protectedCalls++
				return work(ctx)
			},
			func(context.Context, []string) error { t.Fatal("launch called"); return nil },
			func(context.Context) error { localCalls++; return nil },
		)
		if err != nil || protectedCalls != 1 || localCalls != 1 {
			t.Fatalf("admission = %v, protected = %d, local = %d", err, protectedCalls, localCalls)
		}
	})

	t.Run("unprepared linux process launches guarded child", func(t *testing.T) {
		launched := false
		err := runHistoryAdmissionUsing(
			context.Background(), []string{"build", "--dir", "repo"}, "linux", "", false,
			func() error { return os.ErrPermission },
			func(context.Context, func(context.Context) error) error { t.Fatal("protected called"); return nil },
			func(_ context.Context, args []string) error {
				launched = strings.Join(args, " ") == "build --dir repo"
				return nil
			},
			func(context.Context) error { t.Fatal("local called"); return nil },
		)
		if err != nil || !launched {
			t.Fatalf("admission = %v, launched = %t", err, launched)
		}
	})

	if err := runHistoryAdmissionUsing(
		context.Background(), []string{"build"}, "linux", "trace", false,
		func() error { return nil },
		func(context.Context, func(context.Context) error) error { return nil },
		func(context.Context, []string) error { return nil },
		func(context.Context) error { return nil },
	); err == nil {
		t.Fatal("cross-command guarded child marker was accepted")
	}
}
