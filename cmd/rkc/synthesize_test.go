package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/modelassets"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

func TestSynthesizeCancellationNeverPublishesPartialOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(t.TempDir(), "synthesis")
	err := runSynthesizeContext(ctx, []string{"--out", target, "--packet-only"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled synthesis error = %v", err)
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled synthesis published output: %v", statErr)
	}
}

func TestQualifiedStructuredGenerationContractIsPinned(t *testing.T) {
	if !json.Valid([]byte(qualifiedClaimResponseSchema)) {
		t.Fatal("qualified response schema is not valid JSON")
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(qualifiedClaimResponseSchema), &schema); err != nil {
		t.Fatal(err)
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 2 || required[0] != "claims" || required[1] != "unresolved_questions" || schema["additionalProperties"] != false {
		t.Fatalf("qualified response root drifted: %+v", schema)
	}
	arguments := qualifiedStructuredGenerationArguments()
	joined := strings.Join(arguments, "\x00")
	for _, requiredArgument := range []string{
		"--json-schema\x00" + qualifiedClaimResponseSchema,
		"--conversation\x00--single-turn\x00--jinja",
		"--reasoning\x00off",
		"--reasoning-budget\x000",
		"--offline",
		"--poll\x000",
	} {
		if !strings.Contains(joined, requiredArgument) {
			t.Fatalf("structured generation arguments omit %q: %v", requiredArgument, arguments)
		}
	}
	digest := sha256.Sum256([]byte(qualifiedClaimResponseSchema))
	if qualifiedResponseSchemaSHA256() != hex.EncodeToString(digest[:]) {
		t.Fatal("qualified response schema digest is invalid or unstable")
	}
}

func TestSynthesisDescriptorMustMatchResolvedProvenance(t *testing.T) {
	binding := modelassets.GenerationBinding{
		AssetID: "model", ModelSizeBytes: 8, NativeContextTokens: 4096, QuantizationBits: 4,
		ModelSHA256: strings.Repeat("a", 64), ModelRevision: strings.Repeat("b", 40), ModelLicense: "Apache-2.0",
		RuntimeSHA256: strings.Repeat("c", 64), RuntimeRevision: strings.Repeat("d", 40),
	}
	descriptor := modelruntime.ModelDescriptor{
		ID: binding.AssetID, Architecture: "gguf", WeightBytes: binding.ModelSizeBytes,
		ContextLimit: binding.NativeContextTokens, QuantizationBits: binding.QuantizationBits,
		Digest: "sha256:" + binding.ModelSHA256, Revision: binding.ModelRevision, License: binding.ModelLicense,
		Runtime: "llama.cpp-cli", RuntimeDigest: "sha256:" + binding.RuntimeSHA256, RuntimeRevision: binding.RuntimeRevision,
	}
	if err := validateSynthesisDescriptor(binding, descriptor); err != nil {
		t.Fatalf("matching descriptor was rejected: %v", err)
	}
	descriptor.RuntimeDigest = "sha256:" + strings.Repeat("0", 64)
	if err := validateSynthesisDescriptor(binding, descriptor); err == nil {
		t.Fatal("runtime provenance drift was accepted")
	}
}

func TestSynthesizePacketOnlyLeavesCanonicalBundleUntouched(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", "..", "examples", "sample-go"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output := filepath.Join(root, "output")
	if err := runScan([]string{"--out", output, "--no-python", "--no-typescript", "--force", repository}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(output, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	derived := filepath.Join(root, "synthesis")
	if err := runSynthesize([]string{"--dir", output, "--repo-root", repository, "--out", derived, "--packet-only", "--query", "Login", "--limit", "1", "--force"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(output, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("derived synthesis mutated canonical bundle")
	}
	manifestData, err := os.ReadFile(filepath.Join(derived, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest synthesisManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.PacketOnly || manifest.PacketsWritten != 1 || manifest.ResponsesReceived != 0 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	packetFiles, err := filepath.Glob(filepath.Join(derived, "packets", "*.json"))
	if err != nil || len(packetFiles) != 1 {
		t.Fatalf("packet files=%v err=%v", packetFiles, err)
	}
	var report modelruntime.PacketBuildReport
	data, err := os.ReadFile(packetFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Packet.Subject.Name != "Login" || len(report.Packet.Evidence) == 0 || len(report.Packet.SourceExcerpts) == 0 {
		t.Fatalf("incomplete packet: %+v", report)
	}
}

func TestResolveSynthesisOutputEnforcesDatasetBoundary(t *testing.T) {
	base := t.TempDir()
	dataset := filepath.Join(base, "atlas")
	if err := os.MkdirAll(filepath.Join(dataset, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveSynthesisOutput("", dataset, "", "packet-only-symbol-summary")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "atlas.rkc-derived", "synthesis", "packet-only-symbol-summary")
	if resolved != want {
		t.Fatalf("default synthesis output = %q, want %q", resolved, want)
	}

	for _, target := range []string{dataset, filepath.Join(dataset, "derived"), filepath.Join(dataset, "nested", "synthesis")} {
		if _, err := resolveSynthesisOutput(target, dataset, "", "profile"); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
			t.Errorf("resolveSynthesisOutput(%q) = %v, want ErrUnsafeTarget", target, err)
		}
	}

	alias := filepath.Join(base, "atlas-alias")
	if err := os.Symlink(dataset, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSynthesisOutput(filepath.Join(alias, "nested", "generated"), dataset, "", "profile"); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
		t.Fatalf("symlinked dataset descendant = %v, want ErrUnsafeTarget", err)
	}

	explicitSibling := filepath.Join(base, "explicit-synthesis")
	resolved, err = resolveSynthesisOutput(explicitSibling, dataset, "", "profile")
	if err != nil || resolved != explicitSibling {
		t.Fatalf("explicit sibling = %q, %v", resolved, err)
	}
}
