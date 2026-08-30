package rkcstore

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable storage error classification.
type Code string

const (
	// CodeInvalidArgument reports a missing, malformed, or unsupported
	// operation argument.
	CodeInvalidArgument Code = "invalid_argument"
	// CodeInvalidQuery reports a query filter or page limit that the reader
	// cannot execute.
	CodeInvalidQuery Code = "invalid_query"
	// CodeInvalidCursor reports a malformed, expired, tampered, or
	// query-incompatible continuation cursor.
	CodeInvalidCursor Code = "invalid_cursor"
	// CodeBuildNotFound reports that a build identifier is unknown, including
	// after its bounded tombstone has been evicted.
	CodeBuildNotFound Code = "build_not_found"
	// CodeBuildClosed reports an aborted build that no longer accepts writes.
	CodeBuildClosed Code = "build_closed"
	// CodeBuildCommitted reports a committed build that no longer accepts
	// writes or aborts.
	CodeBuildCommitted Code = "build_committed"
	// CodeSnapshotNotFound reports that a requested snapshot, parent, or
	// repository head does not exist.
	CodeSnapshotNotFound Code = "snapshot_not_found"
	// CodeRecordNotFound reports that a requested record is absent from an
	// existing snapshot.
	CodeRecordNotFound Code = "record_not_found"
	// CodeConflict reports a compare-and-swap, identity-affinity, or duplicate
	// identifier conflict.
	CodeConflict Code = "conflict"
	// CodeValidation reports invalid canonical content or inconsistent stored
	// state.
	CodeValidation Code = "validation_failed"
	// CodeCoverageMismatch reports missing coverage or coverage that does not
	// exactly describe the bundle being committed.
	CodeCoverageMismatch Code = "coverage_mismatch"
	// CodeResourceExhausted reports that an explicit store safety limit would
	// be exceeded.
	CodeResourceExhausted Code = "resource_exhausted"
	// CodeCanceled reports cancellation or expiry of the supplied context.
	CodeCanceled Code = "canceled"
	// CodeInternal reports a storage or operating-system failure that is not a
	// caller validation error, transaction conflict, or cancellation. Callers
	// must not reinterpret it as a safe compare-and-swap retry.
	CodeInternal Code = "internal"
)

var (
	// ErrInvalidArgument classifies CodeInvalidArgument errors with errors.Is.
	ErrInvalidArgument = errors.New("rkcstore: invalid argument")
	// ErrInvalidQuery classifies CodeInvalidQuery errors with errors.Is.
	ErrInvalidQuery = errors.New("rkcstore: invalid query")
	// ErrInvalidCursor classifies CodeInvalidCursor errors with errors.Is.
	ErrInvalidCursor = errors.New("rkcstore: invalid cursor")
	// ErrBuildNotFound classifies CodeBuildNotFound errors with errors.Is.
	ErrBuildNotFound = errors.New("rkcstore: build not found")
	// ErrBuildClosed classifies CodeBuildClosed errors with errors.Is.
	ErrBuildClosed = errors.New("rkcstore: build is closed")
	// ErrBuildCommitted classifies CodeBuildCommitted errors with errors.Is.
	ErrBuildCommitted = errors.New("rkcstore: build is committed")
	// ErrSnapshotNotFound classifies CodeSnapshotNotFound errors with
	// errors.Is.
	ErrSnapshotNotFound = errors.New("rkcstore: snapshot not found")
	// ErrRecordNotFound classifies CodeRecordNotFound errors with errors.Is.
	ErrRecordNotFound = errors.New("rkcstore: record not found")
	// ErrConflict classifies CodeConflict errors with errors.Is.
	ErrConflict = errors.New("rkcstore: transaction conflict")
	// ErrValidation classifies CodeValidation errors with errors.Is.
	ErrValidation = errors.New("rkcstore: validation failed")
	// ErrCoverageMismatch classifies CodeCoverageMismatch errors with
	// errors.Is.
	ErrCoverageMismatch = errors.New("rkcstore: coverage mismatch")
	// ErrResourceExhausted classifies CodeResourceExhausted errors with
	// errors.Is.
	ErrResourceExhausted = errors.New("rkcstore: resource exhausted")
	// ErrCanceled classifies CodeCanceled errors with errors.Is. A canceled
	// operation also unwraps to its context cancellation error when available.
	ErrCanceled = errors.New("rkcstore: operation canceled")
	// ErrInternal classifies CodeInternal errors with errors.Is.
	ErrInternal = errors.New("rkcstore: internal storage failure")
)

var sentinelByCode = map[Code]error{
	CodeInvalidArgument:   ErrInvalidArgument,
	CodeInvalidQuery:      ErrInvalidQuery,
	CodeInvalidCursor:     ErrInvalidCursor,
	CodeBuildNotFound:     ErrBuildNotFound,
	CodeBuildClosed:       ErrBuildClosed,
	CodeBuildCommitted:    ErrBuildCommitted,
	CodeSnapshotNotFound:  ErrSnapshotNotFound,
	CodeRecordNotFound:    ErrRecordNotFound,
	CodeConflict:          ErrConflict,
	CodeValidation:        ErrValidation,
	CodeCoverageMismatch:  ErrCoverageMismatch,
	CodeResourceExhausted: ErrResourceExhausted,
	CodeCanceled:          ErrCanceled,
	CodeInternal:          ErrInternal,
}

// ValidationFailure preserves the complete deterministic validation result.
// Callers can inspect diagnostics while errors.Is still classifies the failure.
type ValidationFailure struct {
	// Operation identifies the storage operation that performed validation.
	Operation string
	// BuildID identifies the open build whose content failed validation.
	BuildID BuildID
	// Result contains the complete deterministic diagnostics and expected
	// coverage for the rejected content.
	Result ValidationResult
}

// Error implements error without embedding validation diagnostics in the
// human-readable message. Inspect Result for structured details.
func (failure *ValidationFailure) Error() string {
	if failure == nil {
		return "<nil>"
	}
	return fmt.Sprintf("rkcstore: %s: validation failed for build %q", failure.Operation, failure.BuildID)
}

// Unwrap classifies every ValidationFailure as ErrValidation.
func (failure *ValidationFailure) Unwrap() error { return ErrValidation }

// OperationError adds stable classification and storage identifiers without
// requiring callers to parse a human-readable message.
type OperationError struct {
	// Code is the stable machine-readable classification.
	Code Code
	// Operation names the storage operation that failed.
	Operation string
	// BuildID identifies the affected build when one is known.
	BuildID BuildID
	// SnapshotID identifies the affected snapshot when one is known.
	SnapshotID SnapshotID
	// Field identifies the rejected input or record family when applicable.
	Field string
	// Err retains the underlying cause. Its text is diagnostic and is not a
	// stable machine contract.
	Err error
}

// Error renders a diagnostic message. Callers should use errors.Is,
// errors.As, and Code rather than parsing this text.
func (err *OperationError) Error() string {
	if err == nil {
		return "<nil>"
	}
	detail := string(err.Code)
	if err.Operation != "" {
		detail = err.Operation + ": " + detail
	}
	if err.Field != "" {
		detail += " (" + err.Field + ")"
	}
	if err.Err != nil {
		detail += ": " + err.Err.Error()
	}
	return "rkcstore: " + detail
}

// Unwrap returns the underlying cause when present, otherwise the sentinel
// associated with Code.
func (err *OperationError) Unwrap() error {
	if err == nil {
		return nil
	}
	if err.Err != nil {
		return err.Err
	}
	return sentinelByCode[err.Code]
}

// Is reports whether target is the sentinel associated with Code, even when
// OperationError also retains a different underlying cause.
func (err *OperationError) Is(target error) bool {
	if err == nil {
		return false
	}
	sentinel := sentinelByCode[err.Code]
	return sentinel != nil && target == sentinel
}

func storeError(code Code, operation string, build BuildID, snapshot SnapshotID, field string, cause error) error {
	return &OperationError{
		Code: code, Operation: operation, BuildID: build, SnapshotID: snapshot,
		Field: field, Err: cause,
	}
}

func invalidArgument(operation, field, message string) error {
	return storeError(CodeInvalidArgument, operation, "", "", field, errors.New(message))
}

func invalidQuery(operation, field, message string) error {
	return storeError(CodeInvalidQuery, operation, "", "", field, errors.New(message))
}

func invalidCursor(operation, message string) error {
	return storeError(CodeInvalidCursor, operation, "", "", "cursor", errors.New(message))
}

func resourceExhausted(operation string, build BuildID, field, message string) error {
	return storeError(CodeResourceExhausted, operation, build, "", field, errors.New(message))
}

func conflict(operation string, build BuildID, snapshot SnapshotID, format string, args ...any) error {
	return storeError(CodeConflict, operation, build, snapshot, "", fmt.Errorf(format, args...))
}
