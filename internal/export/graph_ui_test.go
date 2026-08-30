package export

import (
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestBrowserGraphKeepsDenseNeighborhoodsLegibleAndAccessible(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "graph.go", []byte("package graph\n"))
	assets, err := BrowserAssets(bundle, model.BuildCoverage(bundle))
	if err != nil {
		t.Fatal(err)
	}
	application := string(assets["app.js"])
	for _, marker := range []string{
		"maximumGraphNeighbors=32,maximumGraphNodesShown=16",
		"compactGraphLabel",
		"visualNeighborIDs=neighborIDs.slice(0,maximumGraphNodesShown)",
		"shown in diagram",
		"the complete bounded immediate-neighbour list remains below",
		"neighborIDs.map(id=>'<button type=\"button\" class=\"link-button\"",
	} {
		if !strings.Contains(application, marker) {
			t.Errorf("browser graph is missing legibility/accessibility marker %q", marker)
		}
	}
	if strings.Contains(application, "truncate(label(node),25)") {
		t.Fatal("browser graph still truncates the shared qualified-name prefix")
	}
}
