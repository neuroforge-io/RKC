package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyUnchangedExportRejectsTamperingWithoutDecoding(t *testing.T) {
	for _, mutation := range []string{"none", "canonical", "derived", "missing", "extra", "manifest", "wrong pin", "wrong snapshot", "cancel"} {
		t.Run(mutation, func(t *testing.T) {
			root := writeVerifiedServerAtlas(t, richDataset().Bundle)
			dataset, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(root, exportManifestName)
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			pin, snapshot := hex.EncodeToString(digest[:]), dataset.Manifest.ID
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mutation {
			case "canonical", "derived":
				name := "bundle.json"
				if mutation == "derived" {
					name = "site/index.html"
				}
				file := filepath.Join(root, name)
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)/2] ^= 1
				if err := os.WriteFile(file, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(filepath.Join(root, "coverage.json")); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("unexpected"), 0600); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				if err := os.WriteFile(manifestPath, append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong pin":
				pin = strings.Repeat("0", 64)
			case "wrong snapshot":
				snapshot = "different"
			case "cancel":
				cancel()
			}
			err = VerifyUnchangedExport(ctx, root, snapshot, pin)
			if (err == nil) != (mutation == "none") {
				t.Fatalf("mutation %s: %v", mutation, err)
			}
			if mutation == "none" {
				_, captured, err := verifyDatasetExportManifestMode(ctx, root, manifestPath, false)
				if err != nil || len(captured) != 0 {
					t.Fatal("streaming reuse retained canonical payloads", err)
				}
			}
		})
	}
}

func TestUnchangedExportRequiresPriorVerificationInputs(t *testing.T) {
	if VerifyUnchangedExport(nil, "", "", "") == nil {
		t.Fatal("nil context accepted")
	}
	if VerifyUnchangedExport(t.Context(), "", "", "") == nil {
		t.Fatal("empty prior identity accepted")
	}
}
