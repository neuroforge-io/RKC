package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
	"github.com/neuroforge-io/RKC/internal/workspace"
)

func TestWorkspacePriorityRefusesBeforeOpeningWriter(t *testing.T) {
	t.Setenv(resourceguard.WorkspacePriorityLeaseEnvironment, "private-invalid-selection")
	parent := workspaceTestTempDir(t)
	root := filepath.Join(parent, "workspace")
	path := filepath.Join(root, "registry.json")
	store, err := workspace.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "writer.lock")
	lockBefore, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the writer held: acquiring it before the priority check would fail
	// with the unrelated writer-busy error instead of the expected veto.
	err = runWorkspaceSyncContext(t.Context(), []string{"sync", "--workspace", root})
	if !errors.Is(err, resourceguard.ErrHigherPriorityActive) || strings.Contains(err.Error(), "private-invalid-selection") {
		t.Fatalf("workspace admission result = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockAfter, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !os.SameFile(lockBefore, lockAfter) ||
		lockBefore.Mode() != lockAfter.Mode() || lockBefore.Size() != lockAfter.Size() ||
		!lockBefore.ModTime().Equal(lockAfter.ModTime()) {
		t.Fatal("priority refusal changed the registry or stable writer lease")
	}
	if _, err := workspace.Load(path); err != nil {
		t.Fatal("priority refusal blocked a registry reader", err)
	}

	absentRoot := filepath.Join(parent, "uninitialized-workspace")
	err = runWorkspaceSyncContext(t.Context(), []string{"sync", "--workspace", absentRoot})
	if !errors.Is(err, resourceguard.ErrHigherPriorityActive) {
		t.Fatalf("uninitialized workspace admission result = %v", err)
	}
	if _, err := os.Lstat(absentRoot); !os.IsNotExist(err) {
		t.Fatalf("priority refusal initialized a workspace: %v", err)
	}
}

func TestWorkspacePriorityArrivalCancelsRefreshAndRetainsGenerations(t *testing.T) {
	parent := workspaceTestTempDir(t)
	root := filepath.Join(parent, "workspace")
	store, err := workspace.Open(filepath.Join(root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	source := workspace.Source{
		ID: "sample", Label: "Synthetic source", Kind: "local",
		LocalPath: parent, Limits: workspace.DefaultLimits(), Excludes: []string{".git"},
	}
	if err := store.Add(source); err != nil {
		t.Fatal(err)
	}
	var generations []*workspace.Active
	for _, suffix := range []string{"a", "b", "c"} {
		generationRoot, err := workspace.CreateGeneration(filepath.Join(root, "generations"), source.ID)
		if err != nil {
			t.Fatal(err)
		}
		generations = append(generations, &workspace.Active{
			AtlasPath:  filepath.Join(generationRoot, "atlas"),
			SnapshotID: "synthetic-" + suffix, Generation: filepath.Base(generationRoot),
			ManifestSHA256: strings.Repeat(suffix, 64), Fingerprint: strings.Repeat(suffix, 64),
			CompilerVersion: "synthetic-test",
		})
	}
	for _, generation := range generations[:2] {
		if err := store.Refresh(t.Context(), source.ID, func(context.Context, workspace.Source, string) (*workspace.Active, error) {
			return generation, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	before := store.Registry.Sources[0]
	reader, err := workspace.AcquireActive(*before.Active)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	parentContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticks := make(chan time.Time, 1)
	checks, stops := 0, 0
	producerCanceled := false
	err = runProtectedDirectLocalUsing(parentContext, "workspace", nil,
		func(ctx context.Context, _ []string) error {
			return store.Refresh(ctx, source.ID, func(producerContext context.Context, _ workspace.Source, _ string) (*workspace.Active, error) {
				if err := producerContext.Err(); err != nil {
					t.Fatal("refresh was canceled before the model lease arrived", err)
				}
				// The monitor cannot see the veto until the refresh is in flight.
				ticks <- time.Unix(1, 0)
				<-producerContext.Done()
				producerCanceled = errors.Is(producerContext.Err(), context.Canceled)
				// A late result must not publish even if a producer forgets to
				// return the cancellation error alongside its candidate.
				return generations[2], nil
			})
		}, protectedDirectLocalDependencies{
			checkHigherPriority: func() error {
				checks++
				if checks > 1 {
					return resourceguard.ErrHigherPriorityActive
				}
				return nil
			},
			checkHostMemory: func() error { return nil },
			requireEnvelope: func() error { return nil },
			newTicker: func(time.Duration) (<-chan time.Time, func()) {
				return ticks, func() { stops++ }
			},
		})
	if !errors.Is(err, resourceguard.ErrHigherPriorityActive) || !errors.Is(err, context.Canceled) || !producerCanceled {
		t.Fatalf("mid-refresh veto result = %v, producer canceled = %t", err, producerCanceled)
	}
	if parentContext.Err() != nil || checks != 2 || stops != 1 {
		t.Fatalf("refresh cancellation escaped its worker: parent = %v, checks = %d, stops = %d", parentContext.Err(), checks, stops)
	}
	registry, err := workspace.Load(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	after := registry.Sources[0]
	if !reflect.DeepEqual(before.Active, after.Active) || !reflect.DeepEqual(before.Previous, after.Previous) ||
		!before.Freshness.UpdatedAt.Equal(after.Freshness.UpdatedAt) {
		t.Fatal("canceled refresh replaced an active or previous generation")
	}
	if after.Freshness.Status != "error" || after.Freshness.ErrorCode != "canceled" || after.Freshness.CheckedAt.IsZero() {
		t.Fatalf("canceled refresh freshness = %+v", after.Freshness)
	}
	for _, generation := range []*workspace.Active{after.Active, after.Previous} {
		available, err := workspace.AcquireActive(*generation)
		if err != nil {
			t.Fatal("canceled refresh made a retained generation unavailable to readers", err)
		}
		if err := available.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkspaceWatchRetriesAfterPriorityVeto(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	passes, reports, localCalls, stops := 0, 0, 0, 0
	err := watchWorkspace(ctx, 0, func(passContext context.Context) error {
		passes++
		return runProtectedDirectLocalUsing(passContext, "workspace", nil,
			func(workerContext context.Context, _ []string) error {
				localCalls++
				if err := workerContext.Err(); err != nil {
					t.Fatal("retried refresh inherited the previous veto's cancellation", err)
				}
				cancel()
				return nil
			}, protectedDirectLocalDependencies{
				checkHigherPriority: func() error {
					if passes == 1 {
						return resourceguard.ErrHigherPriorityActive
					}
					return nil
				},
				checkHostMemory: func() error { return nil },
				requireEnvelope: func() error { return nil },
				newTicker: func(time.Duration) (<-chan time.Time, func()) {
					return make(chan time.Time), func() { stops++ }
				},
			})
	}, func(err error) {
		reports++
		if !errors.Is(err, resourceguard.ErrHigherPriorityActive) || ctx.Err() != nil {
			t.Fatalf("watcher treated a pass veto as a watcher failure: error = %v, context = %v", err, ctx.Err())
		}
	})
	if err != nil || passes != 2 || reports != 1 || localCalls != 1 || stops != 1 {
		t.Fatalf("watch retry result = %v, passes = %d, reports = %d, local calls = %d, stops = %d", err, passes, reports, localCalls, stops)
	}
}
