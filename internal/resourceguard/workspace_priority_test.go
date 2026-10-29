package resourceguard

import (
	"errors"
	"strings"
	"testing"
)

func TestWorkspacePriorityLeaseSelectionIsExplicitAndPrivate(t *testing.T) {
	busy := errors.New("fixture lease held")
	for _, selection := range []string{"", "erais", "ERAIS", " erais", "erais ", "/private/fixture", "unknown"} {
		t.Run(selection, func(t *testing.T) {
			calls := 0
			err := checkWorkspaceModelLease(selection, func() error { calls++; return busy })
			switch selection {
			case "":
				if err != nil || calls != 0 {
					t.Fatalf("unset selection inspected lease: %v, calls=%d", err, calls)
				}
			case "erais":
				if !errors.Is(err, busy) || calls != 1 {
					t.Fatalf("selected lease = %v, calls=%d", err, calls)
				}
			default:
				if !errors.Is(err, ErrHigherPriorityActive) || calls != 0 || strings.Contains(err.Error(), "/private/fixture") {
					t.Fatalf("invalid selection was observed or disclosed: %v, calls=%d", err, calls)
				}
			}
		})
	}
	if err := checkWorkspaceModelLease("erais", nil); !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("missing observer did not refuse: %v", err)
	}
	if err := checkWorkspaceModelLease("erais", func() error { return nil }); err != nil {
		t.Fatalf("clear selected lease = %v", err)
	}
}

func TestWorkspacePriorityLeaseVetoPrecedesOrdinaryPolicy(t *testing.T) {
	busy := errors.New("fixture lease held")
	ordinary := errors.New("ordinary policy veto")
	leaseCalls, ordinaryCalls := 0, 0
	check := workspacePriorityCheck(func() error {
		leaseCalls++
		if leaseCalls == 1 {
			return busy
		}
		return nil
	}, func() error { ordinaryCalls++; return ordinary })
	if err := check(); !errors.Is(err, busy) || ordinaryCalls != 0 {
		t.Fatalf("held lease did not short circuit policy: %v, calls=%d", err, ordinaryCalls)
	}
	if err := check(); !errors.Is(err, ordinary) || ordinaryCalls != 1 {
		t.Fatalf("clear lease lost ordinary policy: %v, calls=%d", err, ordinaryCalls)
	}
}

func TestWorkspacePrioritySelectionPropagatesWithoutChangingResourceProfile(t *testing.T) {
	clearResourceProfile(t)
	t.Setenv(WorkspacePriorityLeaseEnvironment, "erais")
	profile, err := resourceProfileFromEnvironment()
	if err != nil || profile.selected {
		t.Fatalf("workspace scheduling selection changed resource profile: %+v, %v", profile, err)
	}
	found := false
	for _, entry := range ResourceGuardEnvironment() {
		if entry == WorkspacePriorityLeaseEnvironment+"=erais" {
			found = true
		}
	}
	if !found {
		t.Fatal("workspace selection lost in guarded child environment")
	}
	for _, entry := range SanitizedModelEnvironment(nil) {
		if strings.HasPrefix(entry, WorkspacePriorityLeaseEnvironment+"=") {
			t.Fatal("workspace-only selection escaped into model worker environment")
		}
	}
}
