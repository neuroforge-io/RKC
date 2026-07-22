package rkcstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type stagingWriter struct {
	SnapshotWriter
	maximum int
	calls   []string
	nodes   []rkcmodel.Node
	cover   rkcmodel.Coverage
	failure error
}

func (writer *stagingWriter) record(name string, count int) error {
	writer.calls = append(writer.calls, name)
	if writer.failure != nil {
		return writer.failure
	}
	if writer.maximum > 0 && count > writer.maximum {
		return resourceExhausted("put "+name, "build", "values", "test batch limit")
	}
	return nil
}

func (writer *stagingWriter) PutArtifacts(_ context.Context, _ BuildID, values []rkcmodel.Artifact) error {
	return writer.record("artifacts", len(values))
}
func (writer *stagingWriter) PutEvidence(_ context.Context, _ BuildID, values []rkcmodel.Evidence) error {
	return writer.record("evidence", len(values))
}
func (writer *stagingWriter) PutNodes(_ context.Context, _ BuildID, values []rkcmodel.Node) error {
	if err := writer.record("nodes", len(values)); err != nil {
		return err
	}
	writer.nodes = append(writer.nodes, values...)
	return nil
}
func (writer *stagingWriter) PutEdges(_ context.Context, _ BuildID, values []rkcmodel.Edge) error {
	return writer.record("edges", len(values))
}
func (writer *stagingWriter) PutDiagnostics(_ context.Context, _ BuildID, values []rkcmodel.Diagnostic) error {
	return writer.record("diagnostics", len(values))
}
func (writer *stagingWriter) PutClaims(_ context.Context, _ BuildID, values []rkcmodel.Claim) error {
	return writer.record("claims", len(values))
}
func (writer *stagingWriter) PutConflicts(_ context.Context, _ BuildID, values []rkcmodel.Conflict) error {
	return writer.record("conflicts", len(values))
}
func (writer *stagingWriter) PutDocuments(_ context.Context, _ BuildID, values []rkcmodel.Document) error {
	return writer.record("documents", len(values))
}
func (writer *stagingWriter) PutPaths(_ context.Context, _ BuildID, values []rkcmodel.ExecutionPath) error {
	return writer.record("paths", len(values))
}
func (writer *stagingWriter) PutCoverage(_ context.Context, _ BuildID, coverage rkcmodel.Coverage) error {
	writer.calls = append(writer.calls, "coverage")
	writer.cover = coverage
	return writer.failure
}

func TestStageBundleStagesAllFamiliesAndCoverage(t *testing.T) {
	bundle := conformanceBundle("stage", "repository", "", time.Unix(10, 0).UTC())
	writer := &stagingWriter{}
	if err := StageBundle(context.Background(), writer, "build", bundle); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"artifacts", "evidence", "nodes", "edges", "diagnostics", "claims", "conflicts", "documents", "paths", "coverage"}
	if !reflect.DeepEqual(writer.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", writer.calls, wantCalls)
	}
	if !reflect.DeepEqual(writer.nodes, bundle.Nodes) {
		t.Fatalf("nodes = %+v, want %+v", writer.nodes, bundle.Nodes)
	}
	if want := rkcmodel.BuildCoverage(bundle); !reflect.DeepEqual(writer.cover, want) {
		t.Fatalf("coverage = %+v, want %+v", writer.cover, want)
	}
}

func TestStageBundleAdaptsResourceLimitedBatches(t *testing.T) {
	bundle := conformanceBundle("stage-split", "repository", "", time.Unix(20, 0).UTC())
	bundle.Nodes = make([]rkcmodel.Node, 9)
	for index := range bundle.Nodes {
		bundle.Nodes[index] = rkcmodel.Node{ID: string(rune('a' + index)), Kind: "symbol", Name: "node"}
	}
	writer := &stagingWriter{maximum: 2}
	if err := StageBundle(context.Background(), writer, "build", bundle); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(writer.nodes, bundle.Nodes) {
		t.Fatalf("adaptively staged nodes = %+v, want %+v", writer.nodes, bundle.Nodes)
	}
	if len(writer.calls) <= 10 {
		t.Fatalf("calls = %v, want adaptive retries", writer.calls)
	}
}

func TestStageBundleRejectsInvalidInputsAndStopsOnFailure(t *testing.T) {
	if err := StageBundle(nil, &stagingWriter{}, "build", rkcmodel.Bundle{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil context error = %v", err)
	}
	if err := StageBundle(context.Background(), nil, "build", rkcmodel.Bundle{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil writer error = %v", err)
	}
	var typedNil *stagingWriter
	if err := StageBundle(context.Background(), typedNil, "build", rkcmodel.Bundle{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("typed nil writer error = %v", err)
	}
	if err := StageBundle(context.Background(), &stagingWriter{}, "", rkcmodel.Bundle{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty build error = %v", err)
	}
	failure := errors.New("write failed")
	writer := &stagingWriter{failure: failure}
	bundle := conformanceBundle("stage-fail", "repository", "", time.Unix(30, 0).UTC())
	if err := StageBundle(context.Background(), writer, "build", bundle); !errors.Is(err, failure) {
		t.Fatalf("write failure = %v", err)
	}
	if !reflect.DeepEqual(writer.calls, []string{"artifacts"}) {
		t.Fatalf("calls after failure = %v", writer.calls)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stageAdaptive(canceled, []int{1}, func([]int) error { return nil }); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCanceled) {
		t.Fatalf("canceled adaptive stage = %v", err)
	}
}
