package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	graphindex "github.com/repository-knowledge-compiler/rkc/internal/graph"
	"github.com/repository-knowledge-compiler/rkc/internal/model"
	"github.com/repository-knowledge-compiler/rkc/internal/search"
)

func testDataset() *Dataset {
	a := model.Node{ID: "a", Kind: "function", Name: "login", QualifiedName: "auth.login"}
	b := model.Node{ID: "b", Kind: "function", Name: "find_user", QualifiedName: "db.find_user"}
	e := model.Edge{ID: "e", Kind: "calls", From: "a", To: "b", Resolution: "declared"}
	bundle := model.Bundle{Snapshot: model.Snapshot{ID: "snapshot", SchemaVersion: model.SchemaVersion}, Nodes: []model.Node{a, b}, Edges: []model.Edge{e}}
	return &Dataset{
		Manifest: bundle.Snapshot, Bundle: bundle,
		NodeByID: map[string]model.Node{"a": a, "b": b}, ArtifactByID: map[string]model.Artifact{}, EvidenceByID: map[string]model.Evidence{},
		Graph: graphindex.Build(bundle.Nodes, bundle.Edges), Search: search.BuildFromBundle(bundle), LoadedAt: time.Now(),
	}
}

func TestSearch(t *testing.T) {
	d := testDataset()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=login", nil)
	res := httptest.NewRecorder()
	d.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", res.Code, res.Body.String())
	}
	if body := res.Body.String(); body == "" || !contains(body, "auth.login") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestNeighborhood(t *testing.T) {
	d := testDataset()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/graph/neighborhood?node_id=a&hops=1", nil)
	res := httptest.NewRecorder()
	d.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", res.Code, res.Body.String())
	}
	if body := res.Body.String(); !contains(body, "find_user") || !contains(body, "calls") {
		t.Fatalf("unexpected body: %s", body)
	}
}
func contains(value, part string) bool {
	return len(part) == 0 || (len(value) >= len(part) && func() bool {
		for i := 0; i+len(part) <= len(value); i++ {
			if value[i:i+len(part)] == part {
				return true
			}
		}
		return false
	}())
}
