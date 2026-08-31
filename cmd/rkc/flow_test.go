package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func flowScanFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source := `package demo

import (
	"database/sql"
	"net/http"
	"os"
)

func load() string {
	return os.Getenv("DEMO_MODE")
}

func store(db *sql.DB, value string) error {
	_, err := db.Exec("INSERT INTO items (name) VALUES (?)", value)
	return err
}

func handler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	value := load()
	_ = store(db, value)
}
`
	if err := os.WriteFile(filepath.Join(root, "demo.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/demo\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "atlas")
	state := filepath.Join(t.TempDir(), "state")
	if err := runScanContext(context.Background(), []string{
		"--out", output, "--state-dir", state,
		"--no-python", "--no-typescript", "--no-frameworks",
		"--no-static-site", "--no-integrations", "--force",
		root,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var coverage rkcmodel.Coverage
	if err := json.Unmarshal(data, &coverage); err != nil {
		t.Fatal(err)
	}
	if coverage.FlowCFGBlocks == 0 || coverage.FlowCallEdges == 0 {
		t.Fatalf("flow coverage missing: %+v", coverage)
	}
	return output
}

func TestFlowReportCommand(t *testing.T) {
	output := flowScanFixture(t)
	if err := runFlow([]string{"report", "--dir", output}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"report", "--dir", output, "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"report", "--dir", output, "extra"}); err == nil {
		t.Fatal("report positional argument was accepted")
	}
}

func TestFlowNodeLabelBranches(t *testing.T) {
	output := flowScanFixture(t)
	dataset, err := loadDataset(output)
	if err != nil {
		t.Fatal(err)
	}
	bundle := dataset.Bundle
	// Rendering is independent of whether this syntax-only fixture can prove
	// the external os.Getenv binding without compiler semantics.
	envID := rkcmodel.StableID("node", "environment_variable", "DEMO_MODE")
	bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{
		ID: envID, Kind: "environment_variable", Name: "DEMO_MODE",
		QualifiedName: "DEMO_MODE",
	})
	envLabel := flowNodeLabel(bundle, envID)
	if !strings.HasPrefix(envLabel, "ENV ") {
		t.Fatalf("env label = %q", envLabel)
	}
	sinkLabel := flowNodeLabel(bundle, "missing-id-with-sink-via")
	if sinkLabel != "missing-id-with-sink-via" {
		t.Fatalf("unknown label = %q", sinkLabel)
	}
	// A synthetic value node with sink_via renders its sink provenance.
	valueNode := rkcmodel.Node{ID: "v1", Kind: "value", Name: "sink", QualifiedName: "demo.store#value1",
		Attributes: map[string]any{"flow_role": "sink", "sink_via": "database/sql.Exec"}}
	bundle.Nodes = append(bundle.Nodes, valueNode)
	label := flowNodeLabel(bundle, "v1")
	if !strings.Contains(label, "sink via database/sql.Exec") {
		t.Fatalf("sink label = %q", label)
	}
	sourceNode := rkcmodel.Node{ID: "v2", Kind: "value", Name: "read", QualifiedName: "demo.load#value0",
		Attributes: map[string]any{"flow_role": "source", "environment_variable": "DEMO_MODE"}}
	bundle.Nodes = append(bundle.Nodes, sourceNode)
	label = flowNodeLabel(bundle, "v2")
	if !strings.HasPrefix(label, "ENV DEMO_MODE") {
		t.Fatalf("env-source label = %q", label)
	}
}

func TestFlowLineageCommands(t *testing.T) {
	output := flowScanFixture(t)
	dataset, err := loadDataset(output)
	if err != nil {
		t.Fatal(err)
	}
	var handlerID string
	for _, node := range dataset.Bundle.Nodes {
		if node.Kind == "function" && node.Name == "handler" {
			handlerID = node.ID
		}
	}
	if handlerID == "" {
		t.Fatal("handler function not found in the atlas")
	}
	if err := runFlow([]string{"origins", "--dir", output, "--node", handlerID, "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"sinks", "--dir", output, "--node", handlerID}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"origins", "--dir", output, "--node", "handler"}); err != nil {
		t.Fatalf("origins by name: %v", err)
	}
	if err := runFlow([]string{"origins", "--dir", output}); err == nil {
		t.Fatal("origins without --node succeeded")
	}
}

func TestFlowEnvCommand(t *testing.T) {
	output := flowScanFixture(t)
	if err := runFlow([]string{"env", "--dir", output, "--name", "DEMO_MODE"}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"env", "--dir", output, "--name", "DEMO_MODE", "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"env", "--dir", output, "--name", "ABSENT_VAR"}); err != nil {
		t.Fatal(err)
	}
}

func TestFlowPathCommand(t *testing.T) {
	output := flowScanFixture(t)
	dataset, err := loadDataset(output)
	if err != nil {
		t.Fatal(err)
	}
	var loadID, handlerID string
	for _, node := range dataset.Bundle.Nodes {
		if node.Kind == "function" {
			switch node.Name {
			case "load":
				loadID = node.ID
			case "handler":
				handlerID = node.ID
			}
		}
	}
	if loadID == "" || handlerID == "" {
		t.Fatal("fixture functions missing")
	}
	if err := runFlow([]string{"path", "--dir", output, "--from", handlerID, "--to", loadID}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"path", "--dir", output, "--from", handlerID, "--to", loadID, "--json"}); err != nil {
		t.Fatal(err)
	}
}

func TestFlowCommandValidation(t *testing.T) {
	output := flowScanFixture(t)
	if err := runFlow(nil); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"help"}); err != nil {
		t.Fatal(err)
	}
	if err := runFlow([]string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unknown flow subcommand") {
		t.Fatalf("unknown flow subcommand = %v", err)
	}
	if err := runFlow([]string{"path", "--dir", output, "--from", "a"}); err == nil {
		t.Fatal("path without --to succeeded")
	}
	if err := runFlow([]string{"path", "--dir", output}); err == nil {
		t.Fatal("path without endpoints succeeded")
	}
	if err := runFlow([]string{"env", "--dir", output}); err == nil {
		t.Fatal("env without --name succeeded")
	}
	if err := runFlow([]string{"report", "--dir", filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Fatal("report of a missing atlas succeeded")
	}
	if err := runFlow([]string{"origins", "--dir", output}); err == nil {
		t.Fatal("origins without --node succeeded")
	}
	if err := runFlow([]string{"origins", "--dir", output, "--node", "missing-node"}); err == nil {
		t.Fatal("origins of a missing node succeeded")
	}
	if err := runFlow([]string{"origins", "--dir", output, "--node", "handler", "extra"}); err == nil {
		t.Fatal("positional arguments were accepted")
	}
}

func TestFlowCommandFlagErrors(t *testing.T) {
	output := flowScanFixture(t)
	for _, args := range [][]string{
		{"report", "--dir", output, "--badflag"},
		{"origins", "--dir", output, "--node", "x", "--badflag"},
		{"path", "--dir", output, "--from", "a", "--to", "b", "--badflag"},
		{"env", "--dir", output, "--name", "X", "--badflag"},
	} {
		if err := runFlow(args); err == nil {
			t.Fatalf("flow %v with a bad flag succeeded", args[0])
		}
	}
}
