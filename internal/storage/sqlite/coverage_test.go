package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestErrorAndClassificationBranches(t *testing.T) {
	var nilError *Error
	if nilError.Unwrap() != nil {
		t.Fatal("nil Error.Unwrap returned values")
	}
	if got := (&Error{Op: "plain"}).Error(); got != "sqlite: plain" {
		t.Fatalf("plain Error() = %q", got)
	}
	sameCause := &Error{Op: "same", Kind: ErrBusy, Cause: ErrBusy}
	if unwrapped := sameCause.Unwrap(); len(unwrapped) != 1 || unwrapped[0] != ErrBusy {
		t.Fatalf("same-cause Unwrap() = %#v", unwrapped)
	}

	classificationCases := []struct {
		input error
		want  error
	}{
		{ErrBackfillRequired, ErrBackfillRequired},
		{ErrIncompatibleSchema, ErrIncompatibleSchema},
		{context.Canceled, ErrCanceled},
		{errors.New("ordinary migration failure"), ErrMigrationFailed},
	}
	for _, fixture := range classificationCases {
		if got := classifyMigrationError(fixture.input); got != fixture.want {
			t.Errorf("classifyMigrationError(%v) = %v, want %v", fixture.input, got, fixture.want)
		}
	}
	if got := classifyDatabaseError(errors.New("ordinary database failure")); got != ErrCheckFailed {
		t.Fatalf("classifyDatabaseError(generic) = %v", got)
	}

	readOnly, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if _, err := readOnly.Exec("PRAGMA query_only = ON"); err != nil {
		t.Fatal(err)
	}
	_, err = readOnly.Exec("CREATE TABLE denied(value TEXT)")
	if err == nil || classifyDatabaseError(err) != ErrOpenFailed {
		t.Fatalf("read-only classification = %v, want ErrOpenFailed", err)
	}
}

func TestDirectStructuralFailureBranches(t *testing.T) {
	ctx := context.Background()
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := governedSchemaCatalogFingerprint(ctx, 0, plan); !errors.Is(err, ErrMigrationTampered) {
		t.Fatalf("version-zero catalogue = %v, want ErrMigrationTampered", err)
	}

	missingVersion, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer missingVersion.Close()
	if _, err := missingVersion.Exec(
		"CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := readSchemaVersion(ctx, missingVersion); err == nil {
		t.Fatal("readSchemaVersion accepted a missing schema_version row")
	}
	if _, err := missingVersion.Exec(
		"INSERT INTO schema_meta(key, value) VALUES ('schema_version', '0.9.0')",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := readSchemaVersion(ctx, missingVersion); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("unsupported schema version = %v, want ErrIncompatibleSchema", err)
	}

	v1 := rawDatabaseAtVersion(t, filepath.Join(privateTempDir(t), "wrong-version.db"), 1)
	if err := CheckV2UpgradeEligibility(ctx, v1); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("v1 eligibility = %v, want ErrIncompatibleSchema", err)
	}
	v2 := rawDatabaseAtVersion(t, filepath.Join(privateTempDir(t), "missing-table.db"), 2)
	if _, err := v2.Exec("DROP TABLE artifacts"); err != nil {
		t.Fatal(err)
	}
	if err := CheckV2UpgradeEligibility(ctx, v2); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("incomplete v2 eligibility = %v, want ErrIncompatibleSchema", err)
	}

	closed, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := closed.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckV2UpgradeEligibility(ctx, closed); err == nil {
		t.Fatal("closed database passed v0.2 eligibility")
	}
	if err := inspectOwnership(ctx, closed, "closed.db", plan); err == nil {
		t.Fatal("closed database passed ownership inspection")
	}
	options := DefaultOptions()
	for name, check := range map[string]func() error{
		"connection policy": func() error { return checkConnectionPolicy(ctx, connection, options) },
		"required schema":   func() error { return checkRequiredSchema(ctx, connection) },
		"migration journal": func() error { return checkMigrationJournal(ctx, connection, plan) },
		"integrity":         func() error { return checkIntegrity(ctx, connection) },
		"foreign keys":      func() error { return checkForeignKeys(ctx, connection) },
		"catalogue": func() error {
			_, err := schemaCatalogFingerprint(ctx, connection)
			return err
		},
	} {
		if err := check(); err == nil {
			t.Errorf("%s accepted a closed connection", name)
		}
	}
}

func TestConnectionPolicyAndCheckErrorBranches(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	connection, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := checkConnectionPolicy(ctx, connection, DefaultOptions()); err == nil {
		t.Fatal("default in-memory connection passed the production policy")
	}

	path := filepath.Join(privateTempDir(t), "non-wal.db")
	options := testOptions(path)
	fileDatabase, err := sql.Open("sqlite", sqliteURI(options))
	if err != nil {
		t.Fatal(err)
	}
	defer fileDatabase.Close()
	if err := fileDatabase.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	fileConnection, err := fileDatabase.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fileConnection.Close()
	if err := checkConnectionPolicy(ctx, fileConnection, options); err == nil {
		t.Fatal("non-WAL database passed the production policy")
	}

	database := &Database{path: "fixture.db"}
	checkCases := []struct {
		cause error
		want  error
	}{
		{errIntegrityResult, ErrCorruptDatabase},
		{errSchemaCatalogDrift, ErrIncompatibleSchema},
		{operationError("fixture", "", ErrMigrationTampered, nil), ErrMigrationTampered},
		{ErrIncompatibleSchema, ErrIncompatibleSchema},
	}
	for _, fixture := range checkCases {
		if err := database.checkError("fixture", fixture.cause); !errors.Is(err, fixture.want) {
			t.Errorf("checkError(%v) = %v, want %v", fixture.cause, err, fixture.want)
		}
	}
	var nilDatabase *Database
	if err := nilDatabase.Check(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Check() = %v, want ErrClosed", err)
	}
}

func TestAssetAndBindingFailureBranches(t *testing.T) {
	ctx := context.Background()
	if _, err := loadMigrationPlan(fstest.MapFS{}, embeddedManifestSHA256); !errors.Is(err, ErrMigrationTampered) {
		t.Fatalf("missing migration manifest = %v, want ErrMigrationTampered", err)
	}
	var nilBinding *pathBinding
	if err := nilBinding.verify(ctx, nil); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("nil path binding verification = %v, want ErrUnsafePath", err)
	}

	path := filepath.Join(privateTempDir(t), "bound.db")
	if _, _, err := secureDatabasePath(path); err != nil {
		t.Fatal(err)
	}
	binding, err := bindDatabasePath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	memory, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	if err := binding.verify(ctx, memory); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("mismatched SQLite path = %v, want ErrUnsafePath", err)
	}

	foreignVersion, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer foreignVersion.Close()
	if _, err := foreignVersion.Exec(
		`CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
		 INSERT INTO schema_meta(key, value) VALUES ('schema_version', '0.3.0')`,
	); err != nil {
		t.Fatal(err)
	}
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	if err := inspectOwnership(ctx, foreignVersion, "foreign.db", plan); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("unowned v0.3 schema = %v, want ErrForeignDatabase", err)
	}
}
