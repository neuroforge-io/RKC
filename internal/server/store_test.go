package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

type serverStoreReader struct {
	rkcstore.SnapshotReader
	bundle      rkcmodel.Bundle
	coverage    rkcmodel.Coverage
	bundleError error
	coverError  error
}

func (reader *serverStoreReader) Bundle(context.Context, rkcstore.SnapshotID) (rkcmodel.Bundle, error) {
	return reader.bundle, reader.bundleError
}

func (reader *serverStoreReader) Coverage(context.Context, rkcstore.SnapshotID) (rkcmodel.Coverage, error) {
	return reader.coverage, reader.coverError
}

func TestLoadStoreBuildsVerifiedDataset(t *testing.T) {
	bundle := richDataset().Bundle
	bundle.Snapshot.CreatedAt = time.Unix(123, 0).UTC()
	bundle.Snapshot.RootPath = "/private/repository"
	bundle.Snapshot.Metadata = map[string]string{"host": "ignored", "stable": "retained"}
	reader := &serverStoreReader{bundle: bundle, coverage: model.BuildCoverage(bundle)}
	dataset, err := LoadStore(context.Background(), reader, rkcstore.SnapshotID(bundle.Snapshot.ID))
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Root != "" || dataset.Integrity != IntegrityVerifiedStore || dataset.Manifest.ID != bundle.Snapshot.ID {
		t.Fatalf("dataset identity = root:%q integrity:%q snapshot:%q", dataset.Root, dataset.Integrity, dataset.Manifest.ID)
	}
	if dataset.Search == nil || dataset.Graph == nil || len(dataset.NodeByID) != len(bundle.Nodes) || len(dataset.ArtifactByID) != len(bundle.Artifacts) || len(dataset.EvidenceByID) != len(bundle.Evidence) {
		t.Fatalf("dataset indexes are incomplete: %+v", dataset)
	}
	if !reflect.DeepEqual(dataset.Coverage, reader.coverage) {
		t.Fatal("dataset coverage changed")
	}
	for _, path := range []string{"/", "/styles.css", "/app.js", "/data/atlas.json"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		dataset.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, response.Code)
		}
		if path == "/data/atlas.json" {
			var payload struct {
				Bundle rkcmodel.Bundle `json:"bundle"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Bundle.Snapshot.ID != bundle.Snapshot.ID {
				t.Fatalf("browser atlas payload = %q, %v", response.Body.String(), err)
			}
		}
	}
}

func TestLoadStoreRejectsInvalidInputsAndReaderFailures(t *testing.T) {
	bundle := richDataset().Bundle
	coverage := model.BuildCoverage(bundle)
	valid := &serverStoreReader{bundle: bundle, coverage: coverage}
	if _, err := LoadStore(nil, valid, "snapshot-rich"); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("nil context error = %v", err)
	}
	if _, err := LoadStore(context.Background(), nil, "snapshot-rich"); err == nil || !strings.Contains(err.Error(), "reader") {
		t.Fatalf("nil reader error = %v", err)
	}
	var typedNil *serverStoreReader
	if _, err := LoadStore(context.Background(), typedNil, "snapshot-rich"); err == nil || !strings.Contains(err.Error(), "reader") {
		t.Fatalf("typed nil reader error = %v", err)
	}
	if _, err := LoadStore(context.Background(), valid, ""); err == nil || !strings.Contains(err.Error(), "snapshot ID") {
		t.Fatalf("empty snapshot error = %v", err)
	}
	bundleFailure := errors.New("bundle unavailable")
	if _, err := LoadStore(context.Background(), &serverStoreReader{bundleError: bundleFailure}, "snapshot-rich"); !errors.Is(err, bundleFailure) {
		t.Fatalf("bundle failure = %v", err)
	}
	coverageFailure := errors.New("coverage unavailable")
	if _, err := LoadStore(context.Background(), &serverStoreReader{bundle: bundle, coverError: coverageFailure}, "snapshot-rich"); !errors.Is(err, coverageFailure) {
		t.Fatalf("coverage failure = %v", err)
	}
}

func TestLoadStoreRejectsMismatchedCanonicalAndCoverageData(t *testing.T) {
	bundle := richDataset().Bundle
	coverage := model.BuildCoverage(bundle)
	tests := []struct {
		name     string
		reader   *serverStoreReader
		id       rkcstore.SnapshotID
		contains string
	}{
		{"snapshot mismatch", &serverStoreReader{bundle: bundle, coverage: coverage}, "other", "does not match requested"},
		{"noncanonical", &serverStoreReader{bundle: func() rkcmodel.Bundle {
			value := model.CanonicalBundle(bundle)
			value.Nodes[0], value.Nodes[1] = value.Nodes[1], value.Nodes[0]
			return value
		}(), coverage: coverage}, "snapshot-rich", "canonical form"},
		{"invalid", &serverStoreReader{bundle: func() rkcmodel.Bundle {
			value := model.CanonicalBundle(bundle)
			value.Edges[0].To = "missing"
			value = model.CanonicalBundle(value)
			return value
		}(), coverage: coverage}, "snapshot-rich", "validate bundle"},
		{"coverage mismatch", &serverStoreReader{bundle: bundle, coverage: func() rkcmodel.Coverage { value := coverage; value.NodesTotal++; return value }()}, "snapshot-rich", "coverage does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LoadStore(context.Background(), test.reader, test.id); err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("LoadStore error = %v, want %q", err, test.contains)
			}
		})
	}
}
