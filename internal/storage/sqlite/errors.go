package sqlite

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidOptions reports an unsupported or out-of-bounds option.
	ErrInvalidOptions = errors.New("sqlite: invalid options")
	// ErrUnsafePath reports a path that could escape the regular-file policy.
	ErrUnsafePath = errors.New("sqlite: unsafe database path")
	// ErrClosed reports an operation attempted after Database.Close.
	ErrClosed = errors.New("sqlite: database closed")
	// ErrMigrationTampered reports embedded migration bytes that do not match
	// the immutable manifest contract.
	ErrMigrationTampered = errors.New("sqlite: migration assets tampered")
	// ErrMigrationFailed reports a migration that could not complete atomically.
	ErrMigrationFailed = errors.New("sqlite: migration failed")
	// ErrBackfillRequired reports a populated legacy database that cannot be
	// upgraded losslessly by the current runtime.
	ErrBackfillRequired = errors.New("sqlite: deterministic legacy backfill required")
	// ErrForeignDatabase reports a valid SQLite file that is not owned by RKC.
	ErrForeignDatabase = errors.New("sqlite: foreign database")
	// ErrCorruptDatabase reports bytes SQLite cannot safely interpret.
	ErrCorruptDatabase = errors.New("sqlite: corrupt database")
	// ErrIncompatibleSchema reports an RKC database with an unsupported schema.
	ErrIncompatibleSchema = errors.New("sqlite: incompatible schema")
	// ErrCheckFailed reports a failed post-open database consistency check.
	ErrCheckFailed = errors.New("sqlite: consistency check failed")
	// ErrCanceled reports a context-cancelled SQLite operation.
	ErrCanceled = errors.New("sqlite: operation canceled")
	// ErrBusy reports that SQLite could not obtain the required lock in time.
	ErrBusy = errors.New("sqlite: database busy")
	// ErrOpenFailed reports an operating-system or VFS open failure.
	ErrOpenFailed = errors.New("sqlite: database open failed")
)

// Error is a typed operation error. Both Kind and Cause participate in
// errors.Is/errors.As matching.
type Error struct {
	Op    string
	Path  string
	Kind  error
	Cause error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	detail := e.Op
	if e.Path != "" {
		detail += " " + e.Path
	}
	if e.Cause != nil {
		return fmt.Sprintf("sqlite: %s: %v", detail, e.Cause)
	}
	if e.Kind != nil {
		return fmt.Sprintf("sqlite: %s: %v", detail, e.Kind)
	}
	return "sqlite: " + detail
}

// Unwrap returns both the stable classification and the original cause.
func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	result := make([]error, 0, 2)
	if e.Kind != nil {
		result = append(result, e.Kind)
	}
	if e.Cause != nil && !errors.Is(e.Cause, e.Kind) {
		result = append(result, e.Cause)
	}
	return result
}

func operationError(op, path string, kind, cause error) error {
	return &Error{Op: op, Path: path, Kind: kind, Cause: cause}
}
