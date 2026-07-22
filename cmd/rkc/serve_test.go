package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishServeReadyFileIsAtomicAndNoClobber(t *testing.T) {
	receipt := serveReadyReceipt{
		SchemaVersion: "1.0",
		Address:       "127.0.0.1:12345",
		URL:           "http://127.0.0.1:12345",
		SnapshotID:    "rkc:snapshot:test",
	}
	path := filepath.Join(t.TempDir(), "ready.json")
	if err := publishServeReadyFile(path, receipt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("readiness mode = %v", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded serveReadyReceipt
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != receipt {
		t.Fatalf("readiness receipt = %+v, decode error = %v", decoded, err)
	}
	if err := publishServeReadyFile(path, serveReadyReceipt{SnapshotID: "replacement"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second publication error = %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(data) {
		t.Fatalf("readiness receipt changed: %q, error = %v", unchanged, err)
	}
}

func TestPublishServeReadyFileOptionalAndRejectsExistingSymlink(t *testing.T) {
	if err := publishServeReadyFile("", serveReadyReceipt{}); err != nil {
		t.Fatalf("empty readiness path: %v", err)
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "ready.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := publishServeReadyFile(link, serveReadyReceipt{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("symlink readiness publication error = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: %q, error = %v", data, err)
	}
}
