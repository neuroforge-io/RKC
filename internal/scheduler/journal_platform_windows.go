//go:build windows

package scheduler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/neuroforge-io/RKC/internal/privatepath"
	"golang.org/x/sys/windows"
)

func createJournalRoot(path string) error {
	// Existing parents keep their permissions. The leaf is private from its
	// creation, before a run file or any potentially sensitive journal bytes exist.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	descriptor, err := journalWindowsDescriptor(true)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(encoded, &attributes)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil
	}
	return err
}

func createJournalFile(path string) (*os.File, error) {
	descriptor, err := journalWindowsDescriptor(false)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Match Go's append-only Windows open rights while supplying explicit owner
	// and private permissions at creation, before any bytes can be written.
	access := uint32(windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE | windows.FILE_READ_ATTRIBUTES)
	handle, err := windows.CreateFile(encoded, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		&attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func secureJournalRoot(path string, identity os.FileInfo) error {
	if err := validateJournalRootPrivacy(path, identity); err == nil {
		return nil
	}
	if err := applyJournalWindowsDACL(path, identity, true); err != nil {
		return err
	}
	return validateJournalRootPrivacy(path, identity)
}

func secureJournalFile(path string, identity os.FileInfo) error {
	if err := validateJournalFilePrivacy(path, identity); err == nil {
		return nil
	}
	if err := applyJournalWindowsDACL(path, identity, false); err != nil {
		return err
	}
	return validateJournalFilePrivacy(path, identity)
}

func validateJournalRootPrivacy(path string, identity os.FileInfo) error {
	return privatepath.CheckDir(path, identity)
}

func validateJournalFilePrivacy(path string, identity os.FileInfo) error {
	return privatepath.CheckFile(path, identity)
}

func journalWindowsDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := currentJournalWindowsUser()
	if err != nil {
		return nil, err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	// Use concrete file rights (FA), exactly like privatepath. Generic inheritable
	// rights can be expanded into separate effective/inherit-only ACEs by Windows,
	// failing the canonical one-entry privacy policy.
	return windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;" + flags + ";FA;;;" + user.String() + ")")
}

func applyJournalWindowsDACL(path string, identity os.FileInfo, directory bool) error {
	if err := validateJournalWindowsIdentity(path, identity, directory); err != nil {
		return err
	}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	access := uint32(windows.READ_CONTROL | windows.WRITE_DAC | windows.FILE_READ_ATTRIBUTES)
	if directory {
		// SetSecurityInfo explicitly does not propagate inheritable ACEs to existing
		// children when this handle was opened with MAXIMUM_ALLOWED. Migration must
		// secure this owned root alone, never recursively change an unrelated cache.
		// https://learn.microsoft.com/windows/win32/api/aclapi/nf-aclapi-setsecurityinfo
		access = windows.MAXIMUM_ALLOWED
	}
	handle, err := windows.CreateFile(encoded, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(identity, opened) || opened.IsDir() != directory || (!directory && !opened.Mode().IsRegular()) || opened.Mode()&os.ModeSymlink != 0 {
		return errors.New("scheduler journal identity changed before securing its DACL")
	}
	user, err := currentJournalWindowsUser()
	if err != nil {
		return err
	}
	previous, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read scheduler journal owner: %w", err)
	}
	owner, _, err := previous.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return errors.New("scheduler journal must already be owned by the current user before securing its DACL")
	}
	descriptor, err := journalWindowsDescriptor(directory)
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return errors.New("scheduler journal private DACL is unavailable")
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("apply scheduler journal DACL: %w", err)
	}
	return validateJournalWindowsIdentity(path, identity, directory)
}

func currentJournalWindowsUser() (*windows.SID, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read scheduler journal current-user SID: %w", err)
	}
	if tokenUser == nil || tokenUser.User.Sid == nil || !tokenUser.User.Sid.IsValid() {
		return nil, errors.New("scheduler journal current-user SID is invalid")
	}
	return tokenUser.User.Sid.Copy()
}

func validateJournalWindowsIdentity(path string, identity os.FileInfo, directory bool) error {
	current, err := privatepath.Lstat(path)
	if err != nil || identity == nil || current.Mode()&os.ModeSymlink != 0 || current.IsDir() != directory ||
		(!directory && !current.Mode().IsRegular()) || !os.SameFile(identity, current) {
		return errors.New("scheduler journal identity changed while validating its DACL")
	}
	return nil
}

func syncStableJournalDirectory(path string, identity os.FileInfo) error {
	if err := privatepath.SyncDirectoryStable(path, identity); err != nil {
		return err
	}
	return validateJournalRootPrivacy(path, identity)
}
