//go:build windows

package scheduler

import (
	"context"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFileJournalRejectsWindowsDACLDrift(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		root := t.TempDir()
		journal := openTestJournal(t, root, testRunID)
		if err := addWorldToJournalWindowsDACL(root, true); err != nil {
			_ = journal.Close()
			t.Fatal(err)
		}
		err := journal.Append(context.Background(), JournalRecord{
			RunID: testRunID,
			Kind:  JournalKindRun,
			State: JournalStateRunning,
			Plan:  testJournalPlan("stage"),
		})
		if err == nil {
			t.Fatal("Append accepted a journal root DACL granting World access")
		}
		if restoreErr := secureJournalRoot(root, journal.rootIdentity); restoreErr != nil {
			t.Errorf("restore journal root DACL: %v", restoreErr)
		}
		_ = journal.Close()
	})

	t.Run("file", func(t *testing.T) {
		journal := openTestJournal(t, t.TempDir(), testRunID)
		if err := addWorldToJournalWindowsDACL(journal.Path(), false); err != nil {
			_ = journal.Close()
			t.Fatal(err)
		}
		err := journal.Append(context.Background(), JournalRecord{
			RunID: testRunID,
			Kind:  JournalKindRun,
			State: JournalStateRunning,
			Plan:  testJournalPlan("stage"),
		})
		if err == nil {
			t.Fatal("Append accepted a journal file DACL granting World access")
		}
		if restoreErr := secureJournalFile(journal.Path(), journal.fileIdentity); restoreErr != nil {
			t.Errorf("restore journal file DACL: %v", restoreErr)
		}
		_ = journal.Close()
	})
}

func TestWindowsDirectorySyncUnsupportedErrors(t *testing.T) {
	for _, err := range []error{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_INVALID_FUNCTION,
		windows.ERROR_INVALID_HANDLE,
		windows.ERROR_NOT_SUPPORTED,
	} {
		if !windowsDirectorySyncUnsupported(&os.PathError{Op: "sync", Path: "runs", Err: err}) {
			t.Errorf("windowsDirectorySyncUnsupported(%v) = false", err)
		}
	}
	if windowsDirectorySyncUnsupported(errors.New("unrelated failure")) {
		t.Fatal("windowsDirectorySyncUnsupported accepted an unrelated failure")
	}
}

func addWorldToJournalWindowsDACL(path string, directory bool) error {
	user, err := currentJournalWindowsUser()
	if err != nil {
		return err
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, 2)
	for _, trustee := range []*windows.SID{user, world} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(trustee),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}
