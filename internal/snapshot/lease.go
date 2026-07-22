package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// transactionLease is an advisory liveness proof held for the complete
// lifetime of a building transaction. It prevents cooperative recovery from
// mistaking a slow, zero-age transaction for an abandoned one.
type transactionLease struct {
	file     *os.File
	identity os.FileInfo
}

func createTransactionLease(path string) (*transactionLease, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	identity, statErr := file.Stat()
	if statErr != nil || !identity.Mode().IsRegular() {
		_ = file.Close()
		_ = os.Remove(path)
		if statErr != nil {
			return nil, statErr
		}
		return nil, errors.New("transaction lease is not a regular file")
	}
	acquired, err := lockFileExclusive(file)
	if err != nil || !acquired {
		_ = file.Close()
		_ = removeExactRegularFile(path, identity)
		if err != nil {
			return nil, err
		}
		return nil, ErrTransactionLive
	}
	return &transactionLease{file: file, identity: identity}, nil
}

// acquireAbandonedTransactionLease returns live=true when another process
// still owns the lease. Missing, replaced, or unsupported leases fail closed:
// recovery never guesses that an unprovable transaction is abandoned.
func acquireAbandonedTransactionLease(path string) (lease *transactionLease, live bool, err error) {
	before, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, fmt.Errorf("%w: transaction lease is missing", ErrBuildingUnowned)
		}
		return nil, false, err
	}
	if !before.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: transaction lease is not a regular file", ErrBuildingUnowned)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, false, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, false, fmt.Errorf("%w: transaction lease identity changed", ErrBuildingUnowned)
	}
	acquired, lockErr := lockFileExclusive(file)
	if lockErr != nil {
		_ = file.Close()
		return nil, false, lockErr
	}
	if !acquired {
		_ = file.Close()
		return nil, true, nil
	}
	return &transactionLease{file: file, identity: opened}, false, nil
}

func (lease *transactionLease) validate(path string) error {
	if lease == nil || lease.file == nil || lease.identity == nil {
		return fmt.Errorf("%w: missing transaction lease", ErrBuildingUnowned)
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(lease.identity, current) {
		return fmt.Errorf("%w: transaction lease identity changed", ErrBuildingUnowned)
	}
	opened, err := lease.file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(lease.identity, opened) {
		return fmt.Errorf("%w: opened transaction lease changed", ErrBuildingUnowned)
	}
	return nil
}

func (lease *transactionLease) Close() error {
	if lease == nil || lease.file == nil {
		return nil
	}
	err := lease.file.Close()
	lease.file = nil
	return err
}
