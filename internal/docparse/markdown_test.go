package docparse

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func TestExtractMarkdownStructureLinksAndDeterminism(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeMarkdownTestFile(t, root, "docs/guide.md", strings.Join([]string{
		"# Guide",
		"See [target](../target.md#details), [again](../target.md), [web](https://example.com), and [local](#setup).",
		"## Setup",
		"Use **bold** and `code`.",
		"```markdown",
		"# Not a heading",
		"```",
		"Guide",
		"-----",
		"Done.",
	}, "\n"))
	writeMarkdownTestFile(t, root, "docs/second.md", "# Second\n")

	files := []pluginapi.FileRef{
		{ArtifactID: "artifact-second", Path: "docs/second.md", SHA256: "sha-second"},
		{ArtifactID: "artifact-guide", Path: "docs/guide.md", SHA256: "sha-guide"},
	}
	options := Options{Root: root, SnapshotID: "snapshot", Files: files, Artifacts: map[string]string{"target.md": "artifact-target"}}
	got, err := Extract(options)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	reversed, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: []pluginapi.FileRef{files[1], files[0]}, Artifacts: options.Artifacts})
	if err != nil {
		t.Fatalf("Extract(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(got, reversed) {
		t.Fatal("Extract() output depends on input file order")
	}
	if len(got.Documents) != 2 {
		t.Fatalf("documents = %d, want 2", len(got.Documents))
	}

	var guideSections int
	anchors := map[string]bool{}
	for _, document := range got.Documents {
		if document.Path != "docs/guide.md" {
			continue
		}
		if document.Title != "Guide" {
			t.Errorf("guide title = %q, want Guide", document.Title)
		}
		guideSections = len(document.Sections)
		for _, section := range document.Sections {
			anchor, _ := section.Attributes["anchor"].(string)
			anchors[anchor] = true
			if strings.Contains(section.Heading, "Not a heading") {
				t.Error("fenced heading was extracted")
			}
		}
	}
	if guideSections != 3 {
		t.Fatalf("guide sections = %d, want 3", guideSections)
	}
	for _, anchor := range []string{"guide", "setup", "guide-2"} {
		if !anchors[anchor] {
			t.Errorf("missing anchor %q in %#v", anchor, anchors)
		}
	}

	references := 0
	for _, edge := range got.Edges {
		if edge.Kind == "references" {
			references++
			if edge.To != "artifact-target" {
				t.Errorf("reference target = %q, want artifact-target", edge.To)
			}
		}
	}
	if references != 1 {
		t.Errorf("reference edges = %d, want duplicate local links coalesced to 1", references)
	}
}

func TestExtractMarkdownMalformedEmptyLinkDoesNotPanic(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeMarkdownTestFile(t, root, "README.md", "# Heading\n[empty](   )\n")

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Extract() panicked on an empty link destination: %v", recovered)
		}
	}()
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "artifact", Path: "README.md", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	for _, edge := range fragment.Edges {
		if edge.Kind == "references" {
			t.Errorf("unexpected reference edge for empty destination: %#v", edge)
		}
	}
}

func TestExtractMarkdownRejectsPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMarkdownTestFile(t, parent, "outside.md", "# Outside\n")

	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "outside", Path: "../outside.md", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Documents) != 0 || len(fragment.Nodes) != 0 {
		t.Fatalf("outside-root path was parsed: documents=%d nodes=%d", len(fragment.Documents), len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-DOC-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-DOC-1001", fragment.Diagnostics)
	}
}

func TestMarkdownHelpersBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input string
		want  bool
	}{{"===", true}, {"----", true}, {"--", false}, {"-=-", true}, {"--- ", false}, {"", false}} {
		if got := isSetext(test.input); got != test.want {
			t.Errorf("isSetext(%q) = %v, want %v", test.input, got, test.want)
		}
	}
	if got := slugify("  API / V2: Café!  "); got != "api-v2-caf" {
		t.Errorf("slugify() = %q, want api-v2-caf", got)
	}
	anchors := map[string]int{}
	if got := uniqueAnchor("", anchors); got != "section" {
		t.Errorf("first empty anchor = %q", got)
	}
	if got := uniqueAnchor("", anchors); got != "section-2" {
		t.Errorf("second empty anchor = %q", got)
	}
	if got := stripMarkdown(" ## **hello** `world`_ "); got != "hello world" {
		t.Errorf("stripMarkdown() = %q", got)
	}
}

func writeMarkdownTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
