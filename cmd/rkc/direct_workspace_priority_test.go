package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

func TestWorkspacePriorityRefusesBeforeResolvingOrLaunchingWorker(t *testing.T) {
	t.Setenv(resourceguard.WorkspacePriorityLeaseEnvironment, "/private/invalid-selection")
	calls := 0
	dependencies := guardedDirectLaunchDependencies{
		executable: func() (string, error) { calls++; return "/fixture/rkc", nil },
		newCommand: func(context.Context, resourceguard.Config) (guardedOpenRunner, error) {
			calls++
			return guardedDirectRunnerFunc(func(context.Context, io.Writer, io.Writer) (int64, error) {
				calls++
				return 0, nil
			}), nil
		},
		stdout: io.Discard,
		stderr: io.Discard,
	}
	err := launchGuardedDirectUsing(t.Context(), "workspace", []string{"sync", "--workspace", t.TempDir()}, dependencies)
	if !errors.Is(err, resourceguard.ErrHigherPriorityActive) || calls != 0 {
		t.Fatalf("workspace admission = %v, worker calls=%d", err, calls)
	}
	if strings.Contains(err.Error(), "/private/") {
		t.Fatalf("selection appeared in diagnostic: %v", err)
	}
	// A workspace-only choice must not become a veto in another launch path.
	if err := launchGuardedDirectUsing(t.Context(), "scan", []string{"--no-python"}, dependencies); err != nil || calls != 3 {
		t.Fatalf("non-workspace launch = %v, calls=%d", err, calls)
	}
}

func TestWorkspacePrioritySelectionReachesGuardedRefreshEnvironment(t *testing.T) {
	t.Setenv(resourceguard.WorkspacePriorityLeaseEnvironment, "erais")
	t.Setenv("RKC_WORKSPACE_PRIVATE_SENTINEL", "must-not-pass")
	environment := strings.Join(guardedDirectEnvironment("workspace"), "\n")
	if !strings.Contains(environment, resourceguard.WorkspacePriorityLeaseEnvironment+"=erais") {
		t.Fatal("selected workspace lease was lost at direct worker boundary")
	}
	if strings.Contains(environment, "must-not-pass") {
		t.Fatal("private ambient environment escaped direct worker boundary")
	}
}
