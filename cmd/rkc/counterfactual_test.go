package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/graph"
)

func TestCounterfactualCommandReportsCitedNonAuthoritativeScenario(t *testing.T) {
	_, output, _ := makeScannedFixture(t)
	dataset, err := loadDataset(output)
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := resolveNode(dataset, "Alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := resolveNode(dataset, "Beta")
	if err != nil {
		t.Fatal(err)
	}
	edgeID := ""
	for _, edge := range dataset.Graph.Outgoing[alpha.ID] {
		if edge.To == beta.ID && edge.Kind == "calls" {
			edgeID = edge.ID
			break
		}
	}
	if edgeID == "" {
		t.Fatal("fixture call edge not found")
	}

	text, err := captureStdout(t, func() error {
		return runCounterfactual([]string{
			"--dir", output, "--from", "Alpha", "--to", "Beta", "--without-edge", edgeID,
		})
	})
	if err != nil || !strings.Contains(text, "non-authoritative") ||
		!strings.Contains(text, "No alternative route") || !strings.Contains(text, "not proof") ||
		!strings.Contains(text, edgeID) {
		t.Fatalf("counterfactual text = %q, %v", text, err)
	}

	encoded, err := captureStdout(t, func() error {
		return runCounterfactual([]string{
			"--dir", output, "--from", "Alpha", "--to", "Beta", "--without-edge", edgeID, "--json",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result graph.CounterfactualResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != graph.CounterfactualSchemaVersion || result.SnapshotID != dataset.Bundle.Snapshot.ID ||
		result.Authoritative || result.Outcome != "no_route_found" ||
		result.Baseline.Truncated || result.Counterfactual.Truncated ||
		!result.Counterfactual.SearchExhausted ||
		len(result.Assumption.WithoutEdgeIDs) != 1 || result.Assumption.WithoutEdgeIDs[0] != edgeID {
		t.Fatalf("counterfactual JSON = %+v", result)
	}

	truncatedText, err := captureStdout(t, func() error {
		return runCounterfactual([]string{
			"--dir", output, "--from", "Alpha", "--to", "Beta",
			"--without-edge", edgeID, "--limit", "1",
		})
	})
	if err != nil || !strings.Contains(truncatedText, "Outcome: search truncated") ||
		!strings.Contains(truncatedText, "Baseline: search truncated at the node limit") ||
		!strings.Contains(truncatedText, "route existence remains unresolved") ||
		strings.Contains(truncatedText, "No alternative route") {
		t.Fatalf("truncated counterfactual text = %q, %v", truncatedText, err)
	}
	truncatedJSON, err := captureStdout(t, func() error {
		return runCounterfactual([]string{
			"--dir", output, "--from", "Alpha", "--to", "Beta",
			"--without-edge", edgeID, "--limit", "1", "--json",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var truncatedResult graph.CounterfactualResult
	if err := json.Unmarshal([]byte(truncatedJSON), &truncatedResult); err != nil {
		t.Fatal(err)
	}
	if truncatedResult.Outcome != "search_truncated" || !truncatedResult.Baseline.Truncated ||
		!truncatedResult.Baseline.NodeLimitReached || truncatedResult.Baseline.SearchExhausted {
		t.Fatalf("truncated counterfactual JSON = %+v", truncatedResult)
	}

	pathText, err := captureStdout(t, func() error {
		return runPath([]string{
			"--dir", output, "--from", "Alpha", "--to", "Beta", "--limit", "1",
		})
	})
	if err != nil || !strings.Contains(pathText, "was truncated at the node limit") ||
		!strings.Contains(pathText, "path existence remains unresolved") ||
		strings.Contains(pathText, "No path found") {
		t.Fatalf("truncated path text = %q, %v", pathText, err)
	}
}

func TestCounterfactualCommandRejectsInvalidRequests(t *testing.T) {
	for name, args := range map[string][]string{
		"flags":        {"--definitely-invalid"},
		"positionals":  {"--from", "a", "--to", "b", "--without-edge", "e", "extra"},
		"endpoints":    {"--without-edge", "e"},
		"intervention": {"--from", "a", "--to", "b"},
	} {
		if err := runCounterfactual(args); err == nil {
			t.Errorf("%s request unexpectedly succeeded", name)
		}
	}
	_, output, _ := makeScannedFixture(t)
	if err := runCounterfactual([]string{
		"--dir", output, "--from", "Alpha", "--to", "Beta", "--without-edge", "missing",
	}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unknown edge error = %v", err)
	}
	if err := runCounterfactual([]string{
		"--dir", output, "--from", "Alpha", "--to", "Beta", "--without-node", "Alpha",
	}); err == nil || !strings.Contains(err.Error(), "endpoints cannot be suppressed") {
		t.Fatalf("endpoint suppression error = %v", err)
	}
}
