package main

import (
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestStringListAndSetHelpers(t *testing.T) {
	var values stringList
	if err := values.Set(" alpha "); err != nil {
		t.Fatal(err)
	}
	if err := values.Set("beta"); err != nil {
		t.Fatal(err)
	}
	if got := values.String(); got != "alpha,beta" {
		t.Fatalf("String() = %q", got)
	}
	if err := values.Set("   "); err == nil {
		t.Fatal("empty value was accepted")
	}
	set := splitSet(" b, a, b, ,c ")
	if len(set) != 3 {
		t.Fatalf("splitSet = %#v", set)
	}
	if got := strings.Join(keys(set), ","); got != "a,b,c" {
		t.Fatalf("sorted keys = %q", got)
	}
	if displayNode("name", "qualified") != "qualified" || displayNode("name", "") != "name" {
		t.Fatal("displayNode precedence is wrong")
	}
}

func TestResolveNodeByAllReferencesAndAmbiguity(t *testing.T) {
	nodes := []rkcmodel.Node{
		{ID: "id-a", LogicalID: "logical-a", Name: "Alpha", QualifiedName: "pkg.Alpha"},
		{ID: "id-b", LogicalID: "logical-b", Name: "Shared", QualifiedName: "pkg.Shared"},
		{ID: "id-c", LogicalID: "logical-c", Name: "Shared", QualifiedName: "other.Shared"},
	}
	dataset := &server.Dataset{Bundle: rkcmodel.Bundle{Nodes: nodes}, NodeByID: map[string]rkcmodel.Node{}}
	for _, node := range nodes {
		dataset.NodeByID[node.ID] = node
	}
	for _, reference := range []string{"id-a", "logical-a", "Alpha", "pkg.Alpha"} {
		node, err := resolveNode(dataset, reference)
		if err != nil || node.ID != "id-a" {
			t.Fatalf("resolve %q: node=%+v err=%v", reference, node, err)
		}
	}
	for _, reference := range []string{"", "missing"} {
		if _, err := resolveNode(dataset, reference); err == nil {
			t.Fatalf("resolve %q unexpectedly succeeded", reference)
		}
	}
	_, err := resolveNode(dataset, "Shared")
	if err == nil || !strings.Contains(err.Error(), "id-b, id-c") {
		t.Fatalf("ambiguous resolution error = %v", err)
	}
}

func TestTraversalOptionsValidation(t *testing.T) {
	for input, want := range map[string]graph.Direction{
		"": graph.DirectionOutgoing, "outgoing": graph.DirectionOutgoing,
		"incoming": graph.DirectionIncoming, "both": graph.DirectionBoth,
	} {
		options, err := traversalOptions(input, "calls, imports", "resolved", 2, 9, true)
		if err != nil {
			t.Fatalf("direction %q: %v", input, err)
		}
		if options.Direction != want || options.MaxDepth != 2 || options.MaxNodes != 9 || !options.IncludeUnresolved {
			t.Fatalf("direction %q options = %+v", input, options)
		}
		if len(options.EdgeKinds) != 2 || len(options.Resolutions) != 1 {
			t.Fatalf("filters not parsed: %+v", options)
		}
	}
	for _, test := range []struct {
		direction string
		depth     int
		limit     int
	}{
		{"sideways", 1, 1},
		{"both", -1, 1},
		{"both", 1, 0},
	} {
		if _, err := traversalOptions(test.direction, "", "", test.depth, test.limit, false); err == nil {
			t.Fatalf("invalid traversal accepted: %+v", test)
		}
	}
}

func TestSimpleCommandHelpers(t *testing.T) {
	args := []string{"--config=one.json", "--other", "value"}
	if got := discoverFlagValue(args, "config"); got != "one.json" {
		t.Fatalf("inline config = %q", got)
	}
	if got := discoverFlagValue([]string{"--config", "two.json"}, "config"); got != "two.json" {
		t.Fatalf("separate config = %q", got)
	}
	if got := discoverFlagValue([]string{"--config"}, "config"); got != "" {
		t.Fatalf("dangling config = %q", got)
	}
	if got := firstNonBlank(" ", " second ", "third"); got != " second " {
		t.Fatalf("firstNonBlank = %q", got)
	}
	if firstNonBlank("", " ") != "" {
		t.Fatal("empty firstNonBlank should return empty")
	}
	if toolchainDigest("python3") == "" || toolchainDigest("python3") != toolchainDigest("python3") {
		t.Fatal("toolchain digest is empty or nondeterministic")
	}
	if toolchainDigest("python3") == toolchainDigest("python9") {
		t.Fatal("toolchain digest ignored Python identity")
	}
}
