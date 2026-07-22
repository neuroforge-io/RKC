package rkcstore

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable storage error classification.
type Code string

const (
	CodeInvalidArgument  Code = "invalid_argument"
	CodeInvalidQuery     Code = "invalid_query"
	CodeInvalidCursor    Code = "invalid_cursor"
	CodeBuildNotFound    Code = "build_not_found"
	CodeBuildClosed      Code = "build_closed"
	CodeBuildCommitted   Code = "build_committed"
	CodeSnapshotNotFound Code = "snapshot_not_found"
	CodeRecordNotFound   Code = "record_not_found"
	CodeConflict         Code = "conflict"
	CodeValidation       Code = "validation_failed"
	CodeCoverageMismatch Code = "coverage_mismatch"
	CodeCanceled         Code = "canceled"
)

var (
	ErrInvalidArgument  = errors.New("rkcstore: invalid argument")
	ErrInvalidQuery     = errors.New("rkcstore: invalid query")
	ErrInvalidCursor    = errors.New("rkcstore: invalid cursor")
	ErrBuildNotFound    = errors.New("rkcstore: build not found")
	ErrBuildClosed      = errors.New("rkcstore: build is closed")
	ErrBuildCommitted   = errors.New("rkcstore: build is committed")
	ErrSnapshotNotFound = errors.New("rkcstore: snapshot not found")
	ErrRecordNotFound   = errors.New("rkcstore: record not found")
	ErrConflict         = errors.New("rkcstore: transaction conflict")
	ErrValidation       = errors.New("rkcstore: validation failed")
	ErrCoverageMismatch = errors.New("rkcstore: coverage mismatch")
	ErrCanceled         = errors.New("rkcstore: operation canceled")
)

var sentinelByCode = map[Code]error{
	CodeInvalidArgument:  ErrInvalidArgument,
	CodeInvalidQuery:     ErrInvalidQuery,
	CodeInvalidCursor:    ErrInvalidCursor,
	CodeBuildNotFound:    ErrBuildNotFound,
	CodeBuildClosed:      ErrBuildClosed,
	CodeBuildCommitted:   ErrBuildCommitted,
	CodeSnapshotNotFound: ErrSnapshotNotFound,
	CodeRecordNotFound:   ErrRecordNotFound,
	CodeConflict:         ErrConflict,
	CodeValidation:       ErrValidation,
	CodeCoverageMismatch: ErrCoverageMismatch,
	CodeCanceled:         ErrCanceled,
}

// ValidationFailure preserves the complete deterministic validation result.
// Callers can inspect diagnostics while errors.Is still classifies the failure.
type ValidationFailure struct {
	Operation string
	BuildID   BuildID
	Result    ValidationResult
}

func (failure *ValidationFailure) Error() string {
	if failure == nil {
		return "<nil>"
	}
	return fmt.Sprintf("rkcstore: %s: validation failed for build %q", failure.Operation, failure.BuildID)
}

func (failure *ValidationFailure) Unwrap() error { return ErrValidation }

// OperationError adds stable classification and storage identifiers without
// requiring callers to parse a human-readable message.
type OperationError struct {
	Code       Code
	Operation  string
	BuildID    BuildID
	SnapshotID SnapshotID
	Field      string
	Err        error
}

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

func (err *OperationError) Unwrap() error {
	if err == nil {
		return nil
	}
	if err.Err != nil {
		return err.Err
	}
	return sentinelByCode[err.Code]
}

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

func conflict(operation string, build BuildID, snapshot SnapshotID, format string, args ...any) error {
	return storeError(CodeConflict, operation, build, snapshot, "", fmt.Errorf(format, args...))
}
