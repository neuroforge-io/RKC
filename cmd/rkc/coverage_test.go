package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/semanticdiff"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type commandFailWriter struct{}

func (commandFailWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func TestCommandTextViewsAndServeSetupFailures(t *testing.T) {
	if err := runServe([]string{"--definitely-invalid"}); err == nil {
		t.Fatal("runServe(invalid flag) succeeded")
	}
	if err := runServe([]string{"--dir", filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("runServe(missing dataset) succeeded")
	}

	_, output, state := makeScannedFixture(t)
	if err := runServe([]string{"--dir", output, "--addr", "not a listen address"}); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("runServe(invalid address) = %v", err)
	}
	ready := filepath.Join(t.TempDir(), "ready.json")
	writeTestFile(t, ready, "preserve")
	if err := runServe([]string{"--dir", output, "--addr", "127.0.0.1:0", "--ready-file", ready}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runServe(existing ready file) = %v", err)
	}
	if data, err := os.ReadFile(ready); err != nil || string(data) != "preserve" {
		t.Fatalf("runServe changed readiness file: %q, %v", data, err)
	}

	textCases := []struct {
		name string
		call func() error
		want string
	}{
		{"path-found", func() error {
			return runPath([]string{"--dir", output, "--from", "Alpha", "--to", "Beta", "--direction", "both"})
		}, "Path depth"},
		{"path-not-found", func() error {
			return runPath([]string{"--dir", output, "--from", "Beta", "--to", "Alpha", "--direction", "outgoing"})
		}, "No path found"},
		{"impact", func() error { return runImpact([]string{"--dir", output, "--node", "Beta", "--direction", "incoming"}) }, "Impact set"},
		{"components", func() error {
			return runComponents([]string{"--dir", output, "--edge-kinds", "calls", "--limit", "10"})
		}, "component(s) shown"},
		{"components-cycles", func() error { return runComponents([]string{"--dir", output, "--cycles-only", "--limit", "1"}) }, "component(s) shown"},
		{"snapshot-list", func() error { return runSnapshotsList([]string{"--state-dir", state}) }, "Snapshot store:"},
		{"snapshot-recover", func() error { return runSnapshotsRecover([]string{"--state-dir", state}) }, "Removed 0 abandoned build(s)."},
	}
	for _, test := range textCases {
		t.Run(test.name, func(t *testing.T) {
			stdout, err := captureStdout(t, test.call)
			if err != nil || !strings.Contains(stdout, test.want) {
				t.Fatalf("output=%q error=%v, want %q", stdout, err, test.want)
			}
		})
	}

	pluginRoot := filepath.Join(t.TempDir(), "plugins")
	if err := os.Mkdir(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(pluginRoot, "plugins.lock.json")
	for _, test := range []struct {
		name string
		call func() error
		want string
	}{
		{"list", func() error { return runPluginsList([]string{"--root", pluginRoot}) }, ""},
		{"validate", func() error { return runPluginsValidate([]string{"--root", pluginRoot}) }, "Validated 0 plugin manifest(s)."},
		{"lock", func() error { return runPluginsLock([]string{"--root", pluginRoot, "--out", lock}) }, "Locked 0 plugin(s)"},
	} {
		stdout, err := captureStdout(t, test.call)
		if err != nil || !strings.Contains(stdout, test.want) {
			t.Fatalf("plugins %s output=%q error=%v", test.name, stdout, err)
		}
	}
	stdout, err := captureStdout(t, func() error { return runPluginsVerify([]string{"--root", pluginRoot, "--lock", lock}) })
	if err != nil || !strings.Contains(stdout, "Verified 0 locked plugin(s)") {
		t.Fatalf("plugins verify output=%q error=%v", stdout, err)
	}
}

func TestDetailedDiffFormattingBranches(t *testing.T) {
	beforeQualified := rkcmodel.Node{Name: "Before", QualifiedName: "pkg.Before"}
	afterQualified := rkcmodel.Node{Name: "After", QualifiedName: "pkg.After|Cell"}
	report := semanticdiff.Report{
		FromSnapshot: "before", ToSnapshot: "after",
		Summary: semanticdiff.Summary{ArtifactsAdded: 1, ArtifactsRemoved: 2, ArtifactsModified: 3, NodesAdded: 4, NodesRemoved: 5, NodesModified: 6, EdgesAdded: 7, EdgesRemoved: 8, EdgesModified: 9, BreakingChanges: 1, RiskChanges: 2},
		Nodes: []semanticdiff.NodeChange{
			{Severity: semanticdiff.SeverityBreaking, Kind: "removed", LogicalKey: "logical-before", Before: &beforeQualified, Fields: []string{"signature"}, Reasons: []string{"public API"}},
			{Severity: semanticdiff.SeverityRisk, Kind: "added", LogicalKey: "logical-after", After: &afterQualified},
			{Severity: semanticdiff.SeverityInfo, Kind: "modified", LogicalKey: "logical-only"},
		},
	}
	text, err := captureStdout(t, func() error { printDiffText(report, 3); return nil })
	if err != nil || !strings.Contains(text, "pkg.Before") || !strings.Contains(text, "fields=signature") || !strings.Contains(text, "reasons=public API") || !strings.Contains(text, "logical-only") {
		t.Fatalf("printDiffText output=%q error=%v", text, err)
	}
	truncated, err := captureStdout(t, func() error { printDiffText(report, 1); return nil })
	if err != nil || !strings.Contains(truncated, "detail truncated") {
		t.Fatalf("printDiffText truncated output=%q error=%v", truncated, err)
	}
	markdown, err := captureStdout(t, func() error { printDiffMarkdown(report, 3); return nil })
	if err != nil || !strings.Contains(markdown, "pkg.After\\|Cell") || !strings.Contains(markdown, "logical-only") {
		t.Fatalf("printDiffMarkdown output=%q error=%v", markdown, err)
	}
}

func TestSynthesisSelectionAndWriterFailureBranches(t *testing.T) {
	nodes := []rkcmodel.Node{
		{ID: "z", Kind: "function", Name: "Same", QualifiedName: "pkg.Same", PublicSurface: true, EvidenceIDs: []string{"e"}},
		{ID: "a", Kind: "function", Name: "Same", QualifiedName: "pkg.Same", PublicSurface: true, EvidenceIDs: []string{"e"}},
		{ID: "module", Kind: "module", Name: "A", Visibility: "public", EvidenceIDs: []string{"e"}},
		{ID: "private", Kind: "function", Name: "Private", EvidenceIDs: []string{"e"}},
		{ID: "secret", Kind: "secret", Name: "Secret", PublicSurface: true, EvidenceIDs: []string{"e"}},
	}
	dataset := &server.Dataset{Bundle: rkcmodel.Bundle{Nodes: nodes}, NodeByID: map[string]rkcmodel.Node{}}
	for _, node := range nodes {
		dataset.NodeByID[node.ID] = node
	}
	items, err := selectSynthesisSubjects(dataset, nil, "", nil, true, 2)
	if err != nil || len(items) != 2 || items[0].ID != "module" || items[1].ID != "a" {
		t.Fatalf("selectSynthesisSubjects(all) = %+v, %v", items, err)
	}
	items, err = selectSynthesisSubjects(dataset, []string{"z", "private"}, "", map[string]struct{}{"function": {}}, true, 5)
	if err != nil || len(items) != 2 {
		t.Fatalf("selectSynthesisSubjects(explicit) = %+v, %v", items, err)
	}
	if _, err := selectSynthesisSubjects(dataset, []string{"missing"}, "", nil, false, 1); err == nil {
		t.Fatal("selectSynthesisSubjects(missing reference) succeeded")
	}

	writer := bufio.NewWriter(commandFailWriter{})
	if err := writeJSONLine(writer, map[string]string{"x": "y"}); err != nil {
		t.Fatalf("writeJSONLine(buffered write) = %v", err)
	}
	if err := writer.Flush(); err == nil {
		t.Fatal("buffered write failure was not surfaced by Flush")
	}
	var buffer bytes.Buffer
	if err := writeJSONLine(bufio.NewWriter(&buffer), func() {}); err == nil {
		t.Fatal("writeJSONLine(marshal failure) succeeded")
	}
	parentFile := filepath.Join(t.TempDir(), "parent-file")
	writeTestFile(t, parentFile, "not a directory")
	if err := writePrettyJSONFile(filepath.Join(parentFile, "value.json"), map[string]string{"x": "y"}); err == nil {
		t.Fatal("writePrettyJSONFile(parent file) succeeded")
	}
	if err := writePrettyJSONFile(filepath.Join(t.TempDir(), "value.json"), func() {}); err == nil {
		t.Fatal("writePrettyJSONFile(marshal failure) succeeded")
	}
	if _, err := inventorySynthesisFiles(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inventorySynthesisFiles(missing) = %v", err)
	}
	if _, err := io.Copy(io.Discard, strings.NewReader("coverage")); err != nil {
		t.Fatal(err)
	}
}

func TestAdditionalCommandValidationBranches(t *testing.T) {
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"path flags", func() error { return runPath([]string{"--bad"}) }},
		{"impact flags", func() error { return runImpact([]string{"--bad"}) }},
		{"components flags", func() error { return runComponents([]string{"--bad"}) }},
		{"plugins list flags", func() error { return runPluginsList([]string{"--bad"}) }},
		{"plugins validate flags", func() error { return runPluginsValidate([]string{"--bad"}) }},
		{"plugins lock flags", func() error { return runPluginsLock([]string{"--bad"}) }},
		{"plugins verify flags", func() error { return runPluginsVerify([]string{"--bad"}) }},
		{"snapshots list flags", func() error { return runSnapshotsList([]string{"--bad"}) }},
		{"snapshots show flags", func() error { return runSnapshotsShow([]string{"--bad"}) }},
		{"snapshots export flags", func() error { return runSnapshotsExport([]string{"--bad"}) }},
		{"snapshots recover flags", func() error { return runSnapshotsRecover([]string{"--bad"}) }},
		{"snapshots set-current flags", func() error { return runSnapshotsSetCurrent([]string{"--bad"}) }},
		{"diff flags", func() error { return runDiff([]string{"--bad"}) }},
	} {
		if err := test.call(); err == nil {
			t.Errorf("%s unexpectedly succeeded", test.name)
		}
	}
	if displayNode("name", "") != "name" || displayNode("name", "qualified") != "qualified" {
		t.Fatal("displayNode mismatch")
	}
}
