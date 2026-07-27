//go:build windows

package scheduler

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const journalWindowsFileAllAccess = windows.ACCESS_MASK(
	windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff,
)

func createJournalRoot(path string) error {
	return os.MkdirAll(path, 0o700)
}

func secureJournalRoot(path string, identity os.FileInfo) error {
	if err := applyJournalWindowsDACL(path, identity, true); err != nil {
		return err
	}
	return validateJournalRootPrivacy(path, identity)
}

func secureJournalFile(path string, identity os.FileInfo) error {
	if err := applyJournalWindowsDACL(path, identity, false); err != nil {
		return err
	}
	return validateJournalFilePrivacy(path, identity)
}

func validateJournalRootPrivacy(path string, identity os.FileInfo) error {
	return validateJournalWindowsDACL(path, identity, true)
}

func validateJournalFilePrivacy(path string, identity os.FileInfo) error {
	return validateJournalWindowsDACL(path, identity, false)
}

func applyJournalWindowsDACL(path string, identity os.FileInfo, directory bool) error {
	if err := validateJournalWindowsIdentity(path, identity, directory); err != nil {
		return err
	}
	user, err := currentJournalWindowsUser()
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if directory {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("build scheduler journal DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("apply scheduler journal DACL: %w", err)
	}
	if err := validateJournalWindowsIdentity(path, identity, directory); err != nil {
		return err
	}
	return nil
}

func validateJournalWindowsDACL(path string, identity os.FileInfo, directory bool) error {
	if err := validateJournalWindowsIdentity(path, identity, directory); err != nil {
		return err
	}
	user, err := currentJournalWindowsUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read scheduler journal DACL: %w", err)
	}
	if descriptor == nil || !descriptor.IsValid() {
		return errors.New("scheduler journal has no valid security descriptor")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read scheduler journal DACL control: %w", err)
	}
	if control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("scheduler journal DACL is not present and protected")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read scheduler journal access list: %w", err)
	}
	if dacl == nil || defaulted || dacl.AceCount != 1 {
		return errors.New("scheduler journal DACL must contain one explicit current-user entry")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read scheduler journal access entry: %w", err)
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return errors.New("scheduler journal DACL entry must explicitly allow access")
	}
	expectedFlags := uint8(windows.NO_INHERITANCE)
	if directory {
		expectedFlags = uint8(
			windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		)
	}
	if ace.Header.AceFlags != expectedFlags {
		return errors.New("scheduler journal DACL has unexpected inheritance flags")
	}
	mask := ace.Mask
	if mask&windows.GENERIC_ALL == 0 &&
		mask&journalWindowsFileAllAccess != journalWindowsFileAllAccess {
		return errors.New("scheduler journal DACL does not grant the current user full control")
	}
	entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !entrySID.IsValid() || !user.Equals(entrySID) {
		return errors.New("scheduler journal DACL grants access to a principal other than the current user")
	}
	if err := validateJournalWindowsIdentity(path, identity, directory); err != nil {
		return err
	}
	return nil
}

func currentJournalWindowsUser() (*windows.SID, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read scheduler journal current-user SID: %w", err)
	}
	if tokenUser == nil || tokenUser.User.Sid == nil || !tokenUser.User.Sid.IsValid() {
		return nil, errors.New("scheduler journal current-user SID is invalid")
	}
	user, err := tokenUser.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("copy scheduler journal current-user SID: %w", err)
	}
	return user, nil
}

func validateJournalWindowsIdentity(
	path string,
	identity os.FileInfo,
	directory bool,
) error {
	current, err := os.Lstat(path)
	if err != nil || identity == nil ||
		current.Mode()&os.ModeSymlink != 0 ||
		current.IsDir() != directory ||
		!os.SameFile(identity, current) {
		return errors.New("scheduler journal identity changed while validating its DACL")
	}
	return nil
}

func syncStableJournalDirectory(path string, identity os.FileInfo) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil || identity == nil || !opened.IsDir() || !os.SameFile(identity, opened) {
		return errors.New("scheduler journal directory identity changed")
	}
	if err := directory.Sync(); err != nil && !windowsDirectorySyncUnsupported(err) {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || !os.SameFile(identity, current) {
		return errors.New("scheduler journal directory identity changed")
	}
	return validateJournalRootPrivacy(path, current)
}

func windowsDirectorySyncUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_INVALID_HANDLE) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}
