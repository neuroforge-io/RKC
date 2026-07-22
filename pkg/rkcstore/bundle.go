package rkcstore

import (
	"context"
	"errors"
	"reflect"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// StageBundle writes every canonical record family and its derived coverage to
// an existing build. Large families are split into bounded, atomic batches.
//
// A returned error can follow earlier successful batches. Callers must Abort
// the build unless they can prove that retrying the same immutable records is
// safe for their Store implementation.
func StageBundle(ctx context.Context, writer SnapshotWriter, build BuildID, bundle rkcmodel.Bundle) error {
	const operation = "stage bundle"
	if ctx == nil {
		return invalidArgument(operation, "context", "context is required")
	}
	if writer == nil || reflect.ValueOf(writer).Kind() == reflect.Ptr && reflect.ValueOf(writer).IsNil() {
		return invalidArgument(operation, "writer", "snapshot writer is required")
	}
	if build == "" {
		return invalidArgument(operation, "build_id", "build identifier is required")
	}

	writes := []func() error{
		func() error {
			return stageSlice(ctx, bundle.Artifacts, func(values []rkcmodel.Artifact) error { return writer.PutArtifacts(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Evidence, func(values []rkcmodel.Evidence) error { return writer.PutEvidence(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Nodes, func(values []rkcmodel.Node) error { return writer.PutNodes(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Edges, func(values []rkcmodel.Edge) error { return writer.PutEdges(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Diagnostics, func(values []rkcmodel.Diagnostic) error { return writer.PutDiagnostics(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Claims, func(values []rkcmodel.Claim) error { return writer.PutClaims(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Conflicts, func(values []rkcmodel.Conflict) error { return writer.PutConflicts(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Documents, func(values []rkcmodel.Document) error { return writer.PutDocuments(ctx, build, values) })
		},
		func() error {
			return stageSlice(ctx, bundle.Paths, func(values []rkcmodel.ExecutionPath) error { return writer.PutPaths(ctx, build, values) })
		},
	}
	for _, write := range writes {
		if err := write(); err != nil {
			return err
		}
	}
	return writer.PutCoverage(ctx, build, rkcmodel.BuildCoverage(bundle))
}

func stageSlice[T any](ctx context.Context, values []T, put func([]T) error) error {
	for start := 0; start < len(values); start += MaxBatchSize {
		end := min(start+MaxBatchSize, len(values))
		if err := stageAdaptive(ctx, values[start:end], put); err != nil {
			return err
		}
	}
	return nil
}

func stageAdaptive[T any](ctx context.Context, values []T, put func([]T) error) error {
	if err := ctx.Err(); err != nil {
		return storeError(CodeCanceled, "stage bundle", "", "", "", err)
	}
	err := put(values)
	if err == nil || len(values) < 2 || !errors.Is(err, ErrResourceExhausted) {
		return err
	}
	middle := len(values) / 2
	if err := stageAdaptive(ctx, values[:middle], put); err != nil {
		return err
	}
	return stageAdaptive(ctx, values[middle:], put)
}
