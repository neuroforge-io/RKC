package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOptions(path string) Options {
	options := DefaultOptions()
	options.Path = path
	options.ReadConnections = 3
	return options
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("make temporary database directory private: %v", err)
	}
	return directory
}

func openTestDatabase(t *testing.T, path string) *Database {
	t.Helper()
	database, err := Open(context.Background(), testOptions(path))
	if err != nil {
		t.Fatalf("Open(%q) error = %v", path, err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return database
}

func TestOpenFreshDatabaseAndClose(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "rkc.db")
	database := openTestDatabase(t, path)
	if database.Path() != path {
		t.Fatalf("Path() = %q, want %q", database.Path(), path)
	}
	if database.Options().BusyTimeout != defaultBusyTimeout {
		t.Fatalf("BusyTimeout = %s", database.Options().BusyTimeout)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o, want 600", info.Mode().Perm())
	}
	if err := database.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := database.Check(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Check() after Close = %v, want ErrClosed", err)
	}
	var nilDatabase *Database
	if nilDatabase.Path() != "" || nilDatabase.Options() != (Options{}) {
		t.Fatal("nil database accessors returned non-zero values")
	}
	if err := nilDatabase.Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
	var nilBinding *pathBinding
	if err := nilBinding.Close(); err != nil {
		t.Fatalf("nil path binding Close() error = %v", err)
	}
}

func TestEveryConnectionReceivesSafetyPragmas(t *testing.T) {
	database := openTestDatabase(t, filepath.Join(privateTempDir(t), "connections.db"))
	connections := make([]*sql.Conn, 0, database.options.ReadConnections+1)
	for index := 0; index < database.options.ReadConnections+1; index++ {
		connection, err := database.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
	}
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	for index, connection := range connections {
		if err := checkConnectionPolicy(context.Background(), connection, database.options); err != nil {
			t.Errorf("connection %d policy = %v", index, err)
		}
	}
}

func TestOpenRejectsInvalidOptionsAndUnsafePaths(t *testing.T) {
	base := privateTempDir(t)
	valid := filepath.Join(base, "valid.db")
	cases := []Options{
		{},
		{Path: valid, BusyTimeout: -time.Second},
		{Path: valid, BusyTimeout: maximumBusyTimeout + time.Millisecond},
		{Path: valid, ReadConnections: -1},
		{Path: valid, ReadConnections: maximumReadConnections + 1},
		{Path: valid, Synchronous: "OFF"},
		{Path: valid, MMapBytes: -1},
		{Path: valid, MMapBytes: maximumMMapBytes + 1},
		{Path: valid, CacheKiB: minimumCacheKiB - 1},
		{Path: valid, CacheKiB: maximumCacheKiB + 1},
	}
	for index, options := range cases {
		if _, err := Open(context.Background(), options); !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("invalid option case %d error = %v, want ErrInvalidOptions", index, err)
		}
	}

	directoryPath := filepath.Join(base, "directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	regularPath := filepath.Join(base, "regular.db")
	if err := os.WriteFile(regularPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(base, "linked.db")
	if err := os.Symlink(regularPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	symlinkParent := filepath.Join(base, "linked-parent")
	if err := os.Symlink(base, symlinkParent); err != nil {
		t.Fatal(err)
	}
	unsafe := []string{
		"relative.db",
		base + string(filepath.Separator) + "nested" + string(filepath.Separator) + ".." + string(filepath.Separator) + "escape.db",
		filepath.Join(base, "nul\x00.db"),
		filepath.Join(base, "line\nbreak.db"),
		filepath.Join(base, "missing", "database.db"),
		directoryPath,
		symlinkPath,
		filepath.Join(symlinkParent, "child.db"),
	}
	for _, path := range unsafe {
		if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Open(%q) error = %v, want ErrUnsafePath", path, err)
		}
	}
	permissiveParent := filepath.Join(base, "permissive")
	if err := os.Mkdir(permissiveParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(filepath.Join(permissiveParent, "database.db"))); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Open(permissive parent) error = %v, want ErrUnsafePath", err)
	}
	permissiveFile := filepath.Join(base, "permissive.db")
	if err := os.WriteFile(permissiveFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(permissiveFile)); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Open(permissive file) error = %v, want ErrUnsafePath", err)
	}

	oddPath := filepath.Join(base, "literal?#%&name.db")
	odd := openTestDatabase(t, oddPath)
	if odd.Path() != oddPath {
		t.Fatalf("odd filename Path() = %q, want %q", odd.Path(), oddPath)
	}
}

func TestSQLiteURIQuotesPathAndOptions(t *testing.T) {
	options := testOptions(filepath.Join(privateTempDir(t), "space name.db"))
	options.BusyTimeout = 1234 * time.Millisecond
	options.Synchronous = "FULL"
	options.MMapBytes = 4096
	options.CacheKiB = 2048
	parsed, err := url.Parse(sqliteURI(options))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "file" || parsed.Path != options.Path {
		t.Fatalf("URI = %q, path = %q", parsed.String(), parsed.Path)
	}
	query := parsed.Query()
	if query.Get("mode") != "rwc" || query.Get("cache") != "private" || query.Get("nofollow") != "1" {
		t.Fatalf("URI policy query = %v", query)
	}
	if query.Get("_txlock") != "immediate" || query.Get("_dqs") != "0" || query.Get("_error_rc") != "1" {
		t.Fatalf("URI driver policy query = %v", query)
	}
	pragmas := strings.Join(query["_pragma"], " ")
	for _, required := range []string{
		"foreign_keys(ON)",
		"busy_timeout(1234)",
		"trusted_schema(OFF)",
		"temp_store(MEMORY)",
		"cell_size_check(ON)",
		"synchronous(FULL)",
		"cache_size(-2048)",
		"mmap_size(4096)",
	} {
		if !strings.Contains(pragmas, required) {
			t.Errorf("URI pragmas %q do not contain %q", pragmas, required)
		}
	}
}

func TestSecurePathCreationBooleanAndInodeBinding(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "bound.db")
	resolved, created, err := secureDatabasePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path || !created {
		t.Fatalf("first secureDatabasePath = %q, %v", resolved, created)
	}
	if _, created, err := secureDatabasePath(path); err != nil || created {
		t.Fatalf("second secureDatabasePath created=%v err=%v", created, err)
	}
	binding, err := bindDatabasePath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := binding.verifyFilesystem(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("verify replaced inode = %v, want ErrUnsafePath", err)
	}
	if err := binding.Close(); err != nil {
		t.Fatal(err)
	}
	if err := binding.verifyFilesystem(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("verify closed binding = %v, want ErrUnsafePath", err)
	}
	if _, err := bindDatabasePath(filepath.Join(privateTempDir(t), "missing.db")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("bind missing database = %v, want ErrUnsafePath", err)
	}
}

func TestEnableWALRejectsUnsupportedJournalMode(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	err = enableWAL(context.Background(), database, ":memory:", time.Millisecond)
	if !errors.Is(err, ErrCheckFailed) {
		t.Fatalf("enableWAL(memory) = %v, want ErrCheckFailed", err)
	}
}

func TestParentDirectoryBindingDetectsReplacement(t *testing.T) {
	base := privateTempDir(t)
	parent := filepath.Join(base, "database")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "rkc.db")
	if _, _, err := secureDatabasePath(path); err != nil {
		t.Fatal(err)
	}
	binding, err := bindDatabasePath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	if err := os.Rename(parent, parent+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := binding.verifyFilesystem(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("verify replaced parent = %v, want ErrUnsafePath", err)
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(privateTempDir(t), "cancelled.db")
	if _, err := Open(ctx, testOptions(path)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open(cancelled) error = %v, want context.Canceled", err)
	} else if !errors.Is(err, ErrCanceled) {
		t.Fatalf("Open(cancelled) error = %v, want ErrCanceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled Open created path: %v", err)
	}

	database := openTestDatabase(t, filepath.Join(privateTempDir(t), "check-cancelled.db"))
	if err := database.Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Check(cancelled) error = %v, want context.Canceled", err)
	} else if !errors.Is(err, ErrCanceled) {
		t.Fatalf("Check(cancelled) error = %v, want ErrCanceled", err)
	}
}

func TestForeignAndCorruptFilesFailClosed(t *testing.T) {
	corrupt := filepath.Join(privateTempDir(t), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(corrupt)); !errors.Is(err, ErrCorruptDatabase) {
		t.Fatalf("Open(corrupt) error = %v, want ErrCorruptDatabase", err)
	}

	foreign := filepath.Join(privateTempDir(t), "foreign.db")
	raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: foreign}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE foreign_data(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(foreign)); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("Open(foreign) error = %v, want ErrForeignDatabase", err)
	}

	ftsPrefixed := filepath.Join(privateTempDir(t), "fts-prefixed-foreign.db")
	ftsRaw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: ftsPrefixed}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ftsRaw.Exec("CREATE TABLE search_fts_evil(value TEXT)"); err != nil {
		_ = ftsRaw.Close()
		t.Fatal(err)
	}
	if err := ftsRaw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ftsPrefixed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(ftsPrefixed)); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("Open(FTS-prefixed foreign) error = %v, want ErrForeignDatabase", err)
	}
	ftsRaw, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: ftsPrefixed}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer ftsRaw.Close()
	var appID, userVersion int
	if err := ftsRaw.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := ftsRaw.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if appID != 0 || userVersion != 0 {
		t.Fatalf("foreign database ownership was mutated: app=%#x version=%d", appID, userVersion)
	}

	spoofedLegacy := filepath.Join(privateTempDir(t), "spoofed-legacy.db")
	spoofed, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: spoofedLegacy}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spoofed.Exec(
		`CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
         INSERT INTO schema_meta(key, value) VALUES ('schema_version', '0.2.0');`,
	); err != nil {
		t.Fatal(err)
	}
	if err := spoofed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(spoofedLegacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(spoofedLegacy)); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("Open(spoofed legacy) error = %v, want ErrForeignDatabase", err)
	}
}

func TestCloseSerializesWithConcurrentChecks(t *testing.T) {
	database := openTestDatabase(t, filepath.Join(privateTempDir(t), "close-check.db"))
	start := make(chan struct{})
	results := make(chan error, 16)
	var wait sync.WaitGroup
	for index := 0; index < cap(results); index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results <- database.Check(context.Background())
		}()
	}
	close(start)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("concurrent Check returned %v", err)
		}
	}
}

func TestOwnershipMarkersFailClosedBeforeMigration(t *testing.T) {
	cases := []struct {
		name    string
		pragmas string
		want    error
	}{
		{"foreign application", "PRAGMA application_id = 1234", ErrForeignDatabase},
		{"unowned version", "PRAGMA user_version = 1", ErrForeignDatabase},
		{
			"owned unsupported version",
			fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = 2", applicationID),
			ErrIncompatibleSchema,
		},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "ownership.db")
			raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(fixture.pragmas); err != nil {
				_ = raw.Close()
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, fixture.want) {
				t.Fatalf("Open(%s) = %v, want %v", fixture.name, err, fixture.want)
			}
		})
	}
}

func TestConcurrentOpenSerializesMigrations(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "concurrent.db")
	options := testOptions(path)
	options.BusyTimeout = 15 * time.Second
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			database, err := Open(context.Background(), options)
			if err == nil {
				err = database.Close()
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent Open error = %v", err)
		}
	}
	database := openTestDatabase(t, path)
	if err := database.Check(context.Background()); err != nil {
		t.Fatalf("Check after concurrent Open = %v", err)
	}
}
