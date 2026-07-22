package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

const writerLeaseSuffix = ".writer.lock"

var (
	errWriterLeaseContended   = errors.New("writer lease is held by a live operation")
	errWriterLeaseManagerGone = errors.New("writer lease manager is closed")
	errWriterLeaseUnsafe      = errors.New("writer lease file is unsafe")
)

// writerLeaseManager pins one verified sibling lock-file inode for the
// lifetime of a Database. Every build uses a separately opened file
// description so advisory shared locks have independent lifetimes.
type writerLeaseManager struct {
	mu     sync.Mutex
	path   string
	anchor *os.File
	builds map[rkcstore.BuildID]*writerLease
	closed bool
}

type writerLease struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

type writerLeaseGuard struct {
	manager *writerLeaseManager
	buildID rkcstore.BuildID
	lease   *writerLease
}

func openWriterLeaseManager(databasePath string) (*writerLeaseManager, error) {
	path := databasePath + writerLeaseSuffix
	if err := writerLeasePlatformSupported(); err != nil {
		return nil, operationError("open writer lease", path, ErrOpenFailed, err)
	}
	anchor, err := openWriterLeaseAnchor(path)
	if err != nil {
		kind := ErrOpenFailed
		if errors.Is(err, errWriterLeaseUnsafe) {
			kind = ErrUnsafePath
		}
		return nil, operationError("open writer lease", path, kind, err)
	}
	return &writerLeaseManager{
		path: path, anchor: anchor,
		builds: make(map[rkcstore.BuildID]*writerLease),
	}, nil
}

func openWriterLeaseAnchor(path string) (*os.File, error) {
	for attempt := 0; attempt < 2; attempt++ {
		before, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(createErr, os.ErrExist) {
				continue
			}
			if createErr != nil {
				return nil, createErr
			}
			if verifyErr := verifyWriterLeaseFile(path, file, nil); verifyErr != nil {
				_ = file.Close()
				return nil, verifyErr
			}
			return file, nil
		case err != nil:
			return nil, err
		case !writerLeasePathInfoSafe(before):
			return nil, fmt.Errorf("%w: existing path is not an empty owner-only regular file", errWriterLeaseUnsafe)
		default:
			file, openErr := os.OpenFile(path, os.O_RDWR, 0)
			if openErr != nil {
				return nil, openErr
			}
			if verifyErr := verifyWriterLeaseFile(path, file, before); verifyErr != nil {
				_ = file.Close()
				return nil, verifyErr
			}
			return file, nil
		}
	}
	return nil, fmt.Errorf("%w: lock-file creation race did not converge", errWriterLeaseUnsafe)
}

func writerLeasePathInfoSafe(info os.FileInfo) bool {
	return info != nil && info.Mode() == 0o600 && info.Size() == 0
}

func verifyWriterLeaseFile(path string, file *os.File, expected os.FileInfo) error {
	if file == nil {
		return fmt.Errorf("%w: missing opened file", errWriterLeaseUnsafe)
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !writerLeasePathInfoSafe(opened) || !writerLeasePathInfoSafe(current) ||
		!os.SameFile(opened, current) || (expected != nil && !os.SameFile(expected, opened)) {
		return fmt.Errorf("%w: lock-file identity, type, mode, or size changed", errWriterLeaseUnsafe)
	}
	if err := writerLeaseVerifyPlatform(file); err != nil {
		return fmt.Errorf("%w: %v", errWriterLeaseUnsafe, err)
	}
	return nil
}

func (manager *writerLeaseManager) acquire(exclusive bool) (*writerLease, error) {
	if manager == nil {
		return nil, errWriterLeaseManagerGone
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed || manager.anchor == nil {
		return nil, errWriterLeaseManagerGone
	}
	anchorInfo, err := manager.anchor.Stat()
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(manager.path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*writerLease, error) {
		_ = file.Close()
		return nil, cause
	}
	if err := verifyWriterLeaseFile(manager.path, file, anchorInfo); err != nil {
		return fail(err)
	}
	acquired, err := writerLeaseTryLock(file, exclusive)
	if err != nil {
		return fail(err)
	}
	if !acquired {
		return fail(errWriterLeaseContended)
	}
	if err := verifyWriterLeaseFile(manager.path, file, anchorInfo); err != nil {
		_ = writerLeaseUnlock(file)
		return fail(err)
	}
	return &writerLease{file: file}, nil
}

func (manager *writerLeaseManager) attach(buildID rkcstore.BuildID, lease *writerLease) error {
	if manager == nil || lease == nil || lease.file == nil {
		return errWriterLeaseManagerGone
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed || manager.anchor == nil {
		return errWriterLeaseManagerGone
	}
	if _, exists := manager.builds[buildID]; exists {
		return fmt.Errorf("build %q already owns a writer lease", buildID)
	}
	manager.builds[buildID] = lease
	return nil
}

func (manager *writerLeaseManager) lockBuild(buildID rkcstore.BuildID) (*writerLeaseGuard, bool, error) {
	if manager == nil {
		return nil, false, errWriterLeaseManagerGone
	}
	manager.mu.Lock()
	if manager.closed || manager.anchor == nil {
		manager.mu.Unlock()
		return nil, false, errWriterLeaseManagerGone
	}
	lease := manager.builds[buildID]
	manager.mu.Unlock()
	if lease == nil {
		return nil, false, nil
	}
	lease.mu.Lock()
	if lease.closed || lease.file == nil {
		lease.mu.Unlock()
		return nil, false, nil
	}
	return &writerLeaseGuard{manager: manager, buildID: buildID, lease: lease}, true, nil
}

func (guard *writerLeaseGuard) unlock() {
	if guard == nil || guard.lease == nil {
		return
	}
	guard.lease.mu.Unlock()
	guard.lease = nil
}

func (guard *writerLeaseGuard) release() error {
	if guard == nil || guard.lease == nil {
		return nil
	}
	lease := guard.lease
	guard.manager.mu.Lock()
	if guard.manager.builds[guard.buildID] == lease {
		delete(guard.manager.builds, guard.buildID)
	}
	guard.manager.mu.Unlock()
	err := lease.closeLocked()
	lease.mu.Unlock()
	guard.lease = nil
	return err
}

func (lease *writerLease) close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.closeLocked()
}

func (lease *writerLease) closeLocked() error {
	if lease.closed || lease.file == nil {
		lease.closed = true
		lease.file = nil
		return nil
	}
	lease.closed = true
	file := lease.file
	lease.file = nil
	return errors.Join(writerLeaseUnlock(file), file.Close())
}

func (manager *writerLeaseManager) close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	leases := make([]*writerLease, 0, len(manager.builds))
	for _, lease := range manager.builds {
		leases = append(leases, lease)
	}
	clear(manager.builds)
	anchor := manager.anchor
	manager.anchor = nil
	manager.mu.Unlock()

	errorsFound := make([]error, 0, len(leases)+1)
	for _, lease := range leases {
		if err := lease.close(); err != nil {
			errorsFound = append(errorsFound, err)
		}
	}
	if anchor != nil {
		if err := anchor.Close(); err != nil {
			errorsFound = append(errorsFound, err)
		}
	}
	return errors.Join(errorsFound...)
}

func writerLeaseRuntimeError(operation string, buildID rkcstore.BuildID, err error) error {
	if errors.Is(err, errWriterLeaseContended) {
		return writerConflict(operation, buildID, "", err.Error())
	}
	return writerOperationError(rkcstore.CodeInternal, operation, buildID, "", "writer_lease", err)
}

func writerLeaseOwnershipError(operation string, buildID rkcstore.BuildID) error {
	return writerOperationError(
		rkcstore.CodeConflict,
		operation,
		buildID,
		"",
		"writer_lease",
		errors.New("this Database does not own the live build lease"),
	)
}

func (d *Database) writerOwnedTransaction(
	ctx context.Context,
	operation string,
	buildID rkcstore.BuildID,
	releaseOnSuccess bool,
	apply func(*sql.Tx) error,
) error {
	if d == nil {
		return writerOperationError(rkcstore.CodeConflict, operation, buildID, "", "database", ErrClosed)
	}
	d.lifecycle.RLock()
	defer d.lifecycle.RUnlock()
	if err := writerCheckContext(ctx, operation); err != nil {
		return err
	}
	if err := d.requireOpen(operation); err != nil {
		return writerOperationError(rkcstore.CodeConflict, operation, buildID, "", "database", err)
	}
	if err := writerValidIdentifier(operation, "build_id", string(buildID)); err != nil {
		return err
	}
	guard, owned, err := d.writerLeases.lockBuild(buildID)
	if err != nil {
		return writerLeaseRuntimeError(operation, buildID, err)
	}
	if !owned {
		return d.writerTransactionLocked(ctx, operation, func(transaction *sql.Tx) error {
			var state string
			err := transaction.QueryRowContext(
				ctx,
				"SELECT state FROM builds WHERE build_id = ?",
				buildID,
			).Scan(&state)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return writerOperationError(rkcstore.CodeBuildNotFound, operation, buildID, "", "", nil)
			case err != nil:
				return writerDatabaseError(operation, "build_id", buildID, "", err)
			case state == "committed":
				return writerOperationError(rkcstore.CodeBuildCommitted, operation, buildID, "", "", nil)
			case state == "aborted":
				return writerOperationError(rkcstore.CodeBuildClosed, operation, buildID, "", "", nil)
			case state == "open":
				return writerLeaseOwnershipError(operation, buildID)
			default:
				return writerOperationError(
					rkcstore.CodeBuildClosed,
					operation,
					buildID,
					"",
					"state",
					fmt.Errorf("unsupported durable build state %q", state),
				)
			}
		})
	}
	defer guard.unlock()
	if err := d.writerTransactionLocked(ctx, operation, apply); err != nil {
		return err
	}
	if releaseOnSuccess {
		if err := guard.release(); err != nil {
			return writerLeaseRuntimeError(operation, buildID, err)
		}
	}
	return nil
}
