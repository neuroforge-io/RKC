package resourceguard

import (
	"fmt"
	"os"
)

// WorkspacePriorityLeaseEnvironment enables an additional, workspace-only
// priority veto. Unset or empty preserves existing marker policy; "erais"
// observes the current user's canonical kernel model-workload lease on Linux.
// It does not configure serving, arbitrary shell work, or model acquisition.
const WorkspacePriorityLeaseEnvironment = "RKC_WORKSPACE_PRIORITY_LEASE"

// CheckWorkspaceModelLease checks an explicitly selected workspace lease.
// This is a read-only observation, not an acquired reservation. A held or
// unprovable lease defers workspace refresh without touching its holder.
func CheckWorkspaceModelLease() error {
	return checkWorkspaceModelLease(os.Getenv(WorkspacePriorityLeaseEnvironment), checkERAISModelLease)
}

func checkWorkspaceModelLease(selection string, observe func() error) error {
	switch selection {
	case "":
		return nil
	case "erais":
		if observe != nil {
			return observe()
		}
	}
	// Neither a configuration error nor a private path belongs in diagnostics.
	return fmt.Errorf("%w: workspace priority lease must be erais or empty and its observer must be available", ErrHigherPriorityActive)
}

// WorkspacePriorityCheck adds the selected lease to the existing marker/load
// policy. Call it only for a workspace refresh, never for its persistent watcher
// or a reader: a later model lease must not close an established serving session.
func WorkspacePriorityCheck() func() error {
	return workspacePriorityCheck(CheckWorkspaceModelLease, CurrentPriorityCheck())
}

func workspacePriorityCheck(lease, ordinary func() error) func() error {
	return func() error {
		if err := lease(); err != nil {
			return err
		}
		return ordinary()
	}
}
