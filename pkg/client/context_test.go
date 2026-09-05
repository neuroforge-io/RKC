package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestContextAndDiscoveryAgainstRealServer(t *testing.T) {
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "snapshot-client"}, Nodes: []rkcmodel.Node{{ID: "node", Name: "Authenticate", Kind: "function"}}}
	dataset := &server.Dataset{Manifest: bundle.Snapshot, Bundle: bundle, Search: search.BuildFromBundle(bundle)}
	host := httptest.NewServer(dataset.Handler())
	defer host.Close()
	client, err := New(host.URL)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := client.Capabilities(context.Background())
	if err != nil || capabilities.SnapshotID != bundle.Snapshot.ID || capabilities.Interfaces["context"] != "/api/v1/context" {
		t.Fatalf("discovery: %+v %v", capabilities, err)
	}
	packet, err := client.Context(context.Background(), "Authenticate", 1, 1024)
	if err != nil || len(packet.Items) != 1 || packet.Items[0].ObjectID != "node" || packet.Bytes > 1024 {
		t.Fatalf("context: %+v %v", packet, err)
	}
	defaults, err := client.Context(context.Background(), "Authenticate", 0, 0)
	if err != nil || defaults.MaxBytes != 32768 {
		t.Fatalf("defaults: %+v %v", defaults, err)
	}
	if _, err := client.Context(context.Background(), "Authenticate", -1, 1024); err == nil {
		t.Fatal("invalid limit succeeded")
	}
}

func TestExchangeRejectsMixedSnapshotGeneration(t *testing.T) {
	for _, header := range []string{"", "another-snapshot"} {
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if header != "" {
				w.Header().Set("X-RKC-Snapshot-ID", header)
			}
			_, _ = w.Write([]byte(`{"snapshot_id":"expected-snapshot"}`))
		}))
		client, err := New(host.URL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Context(context.Background(), "query", 0, 0); err == nil {
			t.Error("accepted unbound context")
		}
		if _, err := client.Capabilities(context.Background()); err == nil {
			t.Error("accepted unbound capabilities")
		}
		host.Close()
	}
}
