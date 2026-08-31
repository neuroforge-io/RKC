package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/neuroforge-io/RKC/internal/lang/scipindex"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSCIPSemanticStageCompilesAndCachesExactIndex(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "main.rs"), "fn main() {}\n")
	symbol := "rust cargo fixture 1 main()."
	occurrence := scipTestMessage(
		scipTestBytes(1, scipTestPacked(0, 3, 7)),
		scipTestString(2, symbol),
		scipTestVarint(3, 1),
		scipTestVarint(5, 16),
	)
	symbolInfo := scipTestMessage(
		scipTestString(1, symbol),
		scipTestVarint(5, 17),
		scipTestString(6, "main"),
		scipTestBytes(7, scipTestMessage(
			scipTestString(4, "rust"),
			scipTestString(5, "fn main()"),
		)),
	)
	document := scipTestMessage(
		scipTestString(1, "main.rs"),
		scipTestBytes(2, occurrence),
		scipTestBytes(3, symbolInfo),
		scipTestString(4, "Rust"),
		scipTestString(5, "fn main() {}\n"),
		scipTestVarint(6, 1),
	)
	metadata := scipTestMessage(
		scipTestBytes(2, scipTestMessage(
			scipTestString(1, "rust-analyzer"),
			scipTestString(2, "test"),
		)),
		scipTestString(3, "file:///workspace"),
		scipTestVarint(4, 1),
	)
	index := scipTestMessage(
		scipTestBytes(1, metadata),
		scipTestBytes(2, document),
	)
	indexPath := filepath.Join(t.TempDir(), "index.scip")
	if err := os.WriteFile(indexPath, index, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, _, err := scipindex.PrepareInputs(context.Background(), []string{indexPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := scipindex.MarkGeneratedByCurrentProcess(prepared[0]); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var coldEvents []scheduler.Event
	options := Options{
		Root: root, SCIPIndexes: []string{indexPath}, Cache: cache,
		DisablePythonAST: true,
		OnStageEvent: func(event scheduler.Event) {
			coldEvents = append(coldEvents, event)
		},
	}
	bundle, coverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if coverage.ArtifactsSemanticallyParsed != 1 {
		t.Fatalf("semantic coverage = %+v", coverage)
	}
	nodeID := rkcmodel.StableID("node", "scip", symbol)
	found := false
	for _, node := range bundle.Nodes {
		if node.ID == nodeID && node.Kind == "function" &&
			node.Signature == "fn main()" && node.Language == "rust" {
			found = true
		}
	}
	if !found {
		t.Fatalf("compiler node %s is missing", nodeID)
	}
	if containsStringValue(cachedStages(coldEvents), "scip-semantic") {
		t.Fatal("cold semantic stage unexpectedly used cache")
	}

	var warmEvents []scheduler.Event
	options.OnStageEvent = func(event scheduler.Event) {
		warmEvents = append(warmEvents, event)
	}
	warmBundle, warmCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStringValue(cachedStages(warmEvents), "scip-semantic") {
		t.Fatal("warm semantic stage did not use its verified cache payload")
	}
	if rkcmodel.CanonicalDigest(bundle) != rkcmodel.CanonicalDigest(warmBundle) ||
		coverage.DeterministicOutputDigest != warmCoverage.DeterministicOutputDigest {
		t.Fatal("cold and warm semantic scans differ")
	}
	plan, err := Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	stage := plannedStage(t, plan, "scip-semantic")
	if !stage.Enabled || stage.Disposition != "cache-hit" {
		t.Fatalf("semantic plan = %+v", stage)
	}
}

func scipTestMessage(fields ...[]byte) []byte {
	return bytes.Join(fields, nil)
}

func scipTestString(field int, value string) []byte {
	return scipTestBytes(field, []byte(value))
}

func scipTestBytes(field int, value []byte) []byte {
	output := scipTestEncodeVarint(uint64(field<<3 | 2))
	output = append(output, scipTestEncodeVarint(uint64(len(value)))...)
	return append(output, value...)
}

func scipTestVarint(field int, value uint64) []byte {
	output := scipTestEncodeVarint(uint64(field << 3))
	return append(output, scipTestEncodeVarint(value)...)
}

func scipTestPacked(values ...uint64) []byte {
	var output []byte
	for _, value := range values {
		output = append(output, scipTestEncodeVarint(value)...)
	}
	return output
}

func scipTestEncodeVarint(value uint64) []byte {
	var output []byte
	for value >= 0x80 {
		output = append(output, byte(value)|0x80)
		value >>= 7
	}
	return append(output, byte(value))
}
