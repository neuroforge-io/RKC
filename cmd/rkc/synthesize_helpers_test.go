package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSynthesisSelectionEligibilityAndOutputHelpers(t *testing.T) {
	eligible := rkcmodel.Node{ID: "function", Kind: "function", Name: "Alpha", QualifiedName: "pkg.Alpha", PublicSurface: true, EvidenceIDs: []string{"e1"}}
	private := rkcmodel.Node{ID: "private", Kind: "function", Name: "private", EvidenceIDs: []string{"e1"}}
	module := rkcmodel.Node{ID: "module", Kind: "module", Name: "pkg", Visibility: "public", EvidenceIDs: []string{"e1"}}
	forbidden := rkcmodel.Node{ID: "secret", Kind: "secret", Name: "secret", PublicSurface: true, EvidenceIDs: []string{"e1"}}
	if !eligibleSynthesisNode(eligible, nil, true) || !eligibleSynthesisNode(module, nil, true) {
		t.Fatal("eligible public nodes were rejected")
	}
	if eligibleSynthesisNode(private, nil, true) || eligibleSynthesisNode(forbidden, nil, false) {
		t.Fatal("ineligible nodes were accepted")
	}
	if eligibleSynthesisNode(eligible, map[string]struct{}{"class": {}}, false) {
		t.Fatal("kind filtering was ignored")
	}
	if eligibleSynthesisNode(rkcmodel.Node{Kind: "function", EvidenceIDs: nil}, nil, false) {
		t.Fatal("node without evidence was accepted")
	}

	if safeFileKey("***") != "item" {
		t.Fatalf("empty-safe key = %q", safeFileKey("***"))
	}
	long := safeFileKey(strings.Repeat("a", 140))
	if len(long) != 113 || !strings.Contains(long, "-") {
		t.Fatalf("long safe key = %q (%d)", long, len(long))
	}
	if parseInt("12", 3) != 12 || parseInt("bad", 3) != 3 {
		t.Fatal("parseInt fallback is incorrect")
	}

	var buffer bytes.Buffer
	writer := bufio.NewWriter(&buffer)
	if err := writeJSONLine(writer, map[string]string{"x": "<tag>"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "{\"x\":\"\\u003ctag\\u003e\"}\n" {
		t.Fatalf("JSON line = %q", got)
	}

	root := t.TempDir()
	pretty := filepath.Join(root, "nested", "value.json")
	if err := writePrettyJSONFile(pretty, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(pretty); err != nil || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("pretty JSON: data=%q err=%v", data, err)
	}
	writeTestFile(t, filepath.Join(root, "b.txt"), "b")
	writeTestFile(t, filepath.Join(root, "a.txt"), "aa")
	writeTestFile(t, filepath.Join(root, "manifest.json"), "ignored")
	writeTestFile(t, filepath.Join(root, ".rkc-generated.json"), "ignored")
	files, err := inventorySynthesisFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[0].Path != "a.txt" || files[1].Path != "b.txt" || files[2].Path != "nested/value.json" {
		t.Fatalf("synthesis inventory = %+v", files)
	}
}

func TestWriteSynthesisMarkdownEscapesModelText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "summary.md")
	subject := rkcmodel.Node{ID: "n1", Name: "A *name*", QualifiedName: "pkg.A<script>"}
	packet := modelruntime.EvidencePacket{SnapshotID: "s1", PacketID: "p1"}
	validation := modelruntime.ClaimValidation{
		AcceptedSummary:        "A [summary] with *markup* and <tag>.",
		Accepted:               []rkcmodel.Claim{{Text: "Calls `Beta` safely", EvidenceIDs: []string{"e1", "e2"}, Certainty: "observed", Category: "behavior"}},
		Rejected:               []modelruntime.RejectedClaim{{Reasons: []string{"bad citation"}}},
		SummaryRejectedReasons: []string{"extra rejected summary"},
	}
	if err := writeSynthesisMarkdown(path, subject, packet, validation, modelruntime.ModelDescriptor{ID: "model"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{`pkg.A&lt;script&gt;`, `A \[summary\] with \*markup\* and &lt;tag&gt;.`, "Calls \\`Beta\\` safely", "Evidence: `e1`, `e2`", "Summary rejected", "Claim rejected"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("Markdown missing %q:\n%s", expected, text)
		}
	}
	if markdownPlain("  two\n words  ") != "two words" {
		t.Fatal("markdownPlain did not normalize whitespace")
	}
}
