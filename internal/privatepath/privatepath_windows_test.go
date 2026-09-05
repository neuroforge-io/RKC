//go:build windows

package privatepath

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsRejectsBroadenedDACLAndDoesNotRepair(t *testing.T) {
	root, err := MkdirTemp(t.TempDir(), "private-")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(root, identity); err != nil {
		t.Fatal(err)
	}
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;GR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := CheckDir(root, identity); err == nil {
			t.Fatal("broad current DACL accepted or silently repaired")
		}
	}
}

func TestWindowsRejectsInheritedDACL(t *testing.T) {
	root := t.TempDir()
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(root, identity); err == nil {
		t.Fatal("ordinary inherited temporary-directory ACL accepted as protected")
	}
}
