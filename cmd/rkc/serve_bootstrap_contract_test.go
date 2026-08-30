package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeRejectsUnsafeWorkbenchBootstrapModesBeforeDatasetLoad(t *testing.T) {
	missingDataset := filepath.Join(t.TempDir(), "dataset-must-not-be-loaded")
	readyFile := filepath.Join(t.TempDir(), "ready-must-not-be-created.json")

	err := runServeWithContext(context.Background(), []string{
		"--workbench",
		"--open",
		"--ready-file", readyFile,
		"--addr", "127.0.0.1:0",
		"--dir", missingDataset,
	})
	if err == nil || !strings.Contains(err.Error(), "direct serve --workbench cannot launch a browser") {
		t.Fatalf("direct workbench browser error = %v", err)
	}
	if strings.Contains(err.Error(), missingDataset) {
		t.Fatalf("direct workbench browser rejection loaded the dataset: %v", err)
	}
	if _, statErr := os.Lstat(readyFile); !os.IsNotExist(statErr) {
		t.Fatalf("direct workbench browser rejection created readiness state: %v", statErr)
	}

	err = runServeWithContext(context.Background(), []string{
		"--workbench",
		"--addr", "127.0.0.1:0",
		"--dir", missingDataset,
	})
	if err == nil || !strings.Contains(err.Error(), "owner-private --ready-file") {
		t.Fatalf("missing workbench readiness error = %v", err)
	}
	if strings.Contains(err.Error(), missingDataset) {
		t.Fatalf("missing readiness rejection loaded the dataset: %v", err)
	}
}
