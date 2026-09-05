//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	"github.com/neuroforge-io/RKC/internal/privatepath"
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

func TestWindowsJournalPrivacyAtCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	if err := createJournalRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := ValidateJournalRootPrivacy(root); err != nil {
		t.Fatalf("new root is not immediately private: %v", err)
	}
	path := filepath.Join(root, testRunID+".jsonl")
	file, err := createJournalFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := ValidateJournalFilePrivacy(path); err != nil {
		t.Fatalf("new file is not immediately private: %v", err)
	}
	if _, err := file.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first\nsecond\n" {
		t.Fatalf("journal append semantics = %q, %v", data, err)
	}
}

func TestWindowsJournalMigrationLeavesOtherObjectsUnchanged(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "runs")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := addWorldToJournalWindowsDACL(root, true); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "unrelated-cache")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	childFile := filepath.Join(child, "keep.txt")
	payload := []byte("Unrelated cache contents must survive journal migration.\n")
	if err := os.WriteFile(childFile, payload, 0600); err != nil {
		t.Fatal(err)
	}
	paths := []string{parent, child, childFile}
	before := make(map[string]string)
	for _, path := range paths {
		before[path] = journalTestSecurity(t, path)
	}
	identity, err := privatepath.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureJournalRoot(root, identity); err != nil {
		t.Fatalf("current-user root migration failed: %v", err)
	}
	if err := ValidateJournalRootPrivacy(root); err != nil {
		t.Fatal(err)
	}
	journal := openTestJournal(t, root, testRunID)
	appendTestRecord(t, journal, JournalRecord{RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning, Plan: testJournalPlan("stage")})
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileJournal(journal.Path()); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if after := journalTestSecurity(t, path); after != before[path] {
			t.Fatalf("journal migration changed unrelated ACL for %s: %s => %s", path, before[path], after)
		}
	}
	after, err := os.ReadFile(childFile)
	if err != nil || !bytes.Equal(payload, after) {
		t.Fatalf("unrelated cache bytes changed: %q, %v", after, err)
	}
}

func TestWindowsJournalSecurityRejectsReplacedIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	if err := createJournalRoot(root); err != nil {
		t.Fatal(err)
	}
	identity, err := privatepath.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	before := journalTestSecurity(t, root)
	if err := secureJournalRoot(root, identity); err == nil {
		t.Fatal("security repair accepted a replacement root")
	}
	if err := syncStableJournalDirectory(root, identity); err == nil {
		t.Fatal("directory sync accepted a replacement root")
	}
	if after := journalTestSecurity(t, root); after != before {
		t.Fatal("rejected replacement root ACL was changed")
	}
}

func journalTestSecurity(t *testing.T, path string) string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.String()
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
