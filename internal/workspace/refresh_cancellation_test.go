package workspace

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRefreshCancellationPreservesProducerFailureAndAcceptedGeneration(t *testing.T) {
	store := fixtureStore(t)
	source := fixtureSource(t)
	if err := store.Add(source); err != nil {
		t.Fatal(err)
	}
	accepted := fixtureActive(store, "a")
	if err := store.Refresh(t.Context(), source.ID, func(context.Context, Source, string) (*Active, error) {
		return accepted, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	captureFailure := errors.New("private capture failure")
	err := store.Refresh(ctx, source.ID, func(context.Context, Source, string) (*Active, error) {
		cancel()
		return fixtureActive(store, "b"), &RefreshError{Code: "source_changed", Cause: captureFailure}
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, captureFailure) {
		t.Fatalf("refresh lost cancellation or producer failure: %v", err)
	}
	registry, err := Load(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	after := registry.Sources[0]
	if !reflect.DeepEqual(after.Active, accepted) || after.Previous != nil ||
		after.Freshness.ErrorCode != "canceled" || after.Freshness.Status != "error" {
		t.Fatalf("canceled failed producer changed accepted state: %+v", after)
	}
}
