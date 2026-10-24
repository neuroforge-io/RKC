package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/workspace"
)

func TestWorkspaceStructuralRedactionStillRequiresExplicitSecretReview(t *testing.T) {
	parent := workspaceTestTempDir(t)
	root, source := filepath.Join(parent, "workspace"), filepath.Join(parent, "source")
	writeTestFile(t, filepath.Join(source, "main.go"), "package sample\n\nfunc Harvest() bool { return true }\n")
	writeTestFile(t, filepath.Join(source, "empty.txt"), "")
	_, err := captureStdout(t, func() error {
		return runWorkspaceAdd([]string{"--workspace", root, "--id", "sample", source})
	})
	if err != nil {
		t.Fatal(err)
	}
	syncSource := func() error {
		_, err := captureStdout(t, func() error {
			return runWorkspaceSyncContext(t.Context(), []string{"sync", "--workspace", root})
		})
		return err
	}
	if err := syncSource(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "registry.json")
	before, err := workspace.Load(path)
	if err != nil || before.Sources[0].Active == nil {
		t.Fatal("clean control did not publish", err)
	}
	digest := sha256.Sum256(nil)
	var lines strings.Builder
	for _, value := range []string{"function", hex.EncodeToString(digest[:])} {
		body, err := json.Marshal(map[string]string{"api_key": value})
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(body)
		lines.WriteByte('\n')
	}
	writeTestFile(t, filepath.Join(source, "credentials.jsonl"), lines.String())
	err = syncSource()
	var failure *workspace.RefreshError
	if !errors.As(err, &failure) || failure.Code != "quality_failed" ||
		!strings.Contains(failure.Error(), "high_confidence_secrets=2") || !strings.Contains(failure.Error(), "reviewed_false_positives=0") {
		t.Fatalf("structural collision must reach the unchanged secret-review gate: %v", err)
	}
	after, err := workspace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Sources[0].Active.Generation != before.Sources[0].Active.Generation ||
		after.Sources[0].Freshness.ErrorCode != "quality_failed" || len(after.Sources[0].SecretReviews) != 0 {
		t.Fatal("unreviewed structural collision replaced accepted data or installed reviews")
	}
}
