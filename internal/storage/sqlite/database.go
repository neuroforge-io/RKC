package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	moderncsqlite "modernc.org/sqlite"
)

// SQLite primary result codes are part of SQLite's stable public ABI. Keeping
// the small classification subset here avoids importing the generated
// modernc.org/sqlite/lib package solely for constants; that package's
// generator-only dependency graph must not become part of RKC's runtime module
// closure.
const (
	sqliteResultPermission = 3
	sqliteResultBusy       = 5
	sqliteResultLocked     = 6
	sqliteResultReadOnly   = 8
	sqliteResultInterrupt  = 9
	sqliteResultCorrupt    = 11
	sqliteResultCantOpen   = 14
	sqliteResultFormat     = 24
	sqliteResultNotADB     = 26
)

const (
	defaultBusyTimeout     = 5 * time.Second
	maximumBusyTimeout     = 60 * time.Second
	defaultReadConnections = 4
	maximumReadConnections = 32
	defaultCacheKiB        = 16 * 1024
	minimumCacheKiB        = 256
	maximumCacheKiB        = 256 * 1024
	maximumMMapBytes       = 1 << 30
)

// Options controls the bounded SQLite connection pool and connection policy.
// Path must name a database file beneath an existing, symlink-free directory.
type Options struct {
	Path            string
	BusyTimeout     time.Duration
	ReadConnections int
	Synchronous     string
	MMapBytes       int64
	CacheKiB        int
}

// DefaultOptions returns the production defaults. The caller must set Path.
func DefaultOptions() Options {
	return Options{
		BusyTimeout:     defaultBusyTimeout,
		ReadConnections: defaultReadConnections,
		Synchronous:     "NORMAL",
		CacheKiB:        defaultCacheKiB,
	}
}

// Database owns a verified RKC SQLite database and its bounded connection pool.
type Database struct {
	db        *sql.DB
	path      string
	options   Options
	closed    atomic.Bool
	closeOne  sync.Once
	closeErr  error
	binding   *pathBinding
	lifecycle sync.RWMutex
}

// Open validates the path and immutable migration assets, opens SQLite with
// per-connection safety pragmas, atomically migrates, and runs Check.
func Open(ctx context.Context, options Options) (*Database, error) {
	if err := ctx.Err(); err != nil {
		return nil, operationError("open", options.Path, ErrCanceled, err)
	}
	normalized, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	plan, err := embeddedMigrationPlan()
	if err != nil {
		return nil, err
	}
	path, created, err := secureDatabasePath(normalized.Path)
	if err != nil {
		return nil, err
	}
	normalized.Path = path
	binding, err := bindDatabasePath(path)
	if err != nil {
		return nil, err
	}

	dsn := sqliteURI(normalized)
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = binding.Close()
		return nil, operationError("open driver", path, ErrCheckFailed, err)
	}
	pool.SetMaxOpenConns(normalized.ReadConnections + 1)
	pool.SetMaxIdleConns(normalized.ReadConnections + 1)
	database := &Database{db: pool, path: path, options: normalized, binding: binding}
	fail := func(openErr error) (*Database, error) {
		_ = database.Close()
		return nil, openErr
	}

	if err := pool.PingContext(ctx); err != nil {
		return fail(operationError("connect", path, classifyDatabaseError(err), err))
	}
	if err := binding.verify(ctx, pool); err != nil {
		return fail(err)
	}
	if created {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			if err == nil {
				err = fmt.Errorf("created database mode is %o, want 600", info.Mode().Perm())
			}
			return fail(operationError("verify database permissions", path, ErrUnsafePath, err))
		}
	}
	if err := inspectOwnership(ctx, pool, path, plan); err != nil {
		return fail(err)
	}
	if err := enableWAL(ctx, pool, path, normalized.BusyTimeout); err != nil {
		return fail(err)
	}
	if err := migrate(ctx, pool, path, plan); err != nil {
		return fail(err)
	}
	if err := database.check(ctx, false); err != nil {
		return fail(err)
	}
	if err := binding.verify(ctx, pool); err != nil {
		return fail(err)
	}
	return database, nil
}

// Path returns the absolute canonical database path.
func (d *Database) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Options returns the normalized connection options in effect.
func (d *Database) Options() Options {
	if d == nil {
		return Options{}
	}
	return d.options
}

// Close is idempotent and returns the first pool-close result on every call.
func (d *Database) Close() error {
	if d == nil {
		return nil
	}
	d.closeOne.Do(func() {
		d.lifecycle.Lock()
		defer d.lifecycle.Unlock()
		d.closed.Store(true)
		if d.db != nil {
			d.closeErr = d.db.Close()
		}
		if d.binding != nil {
			if err := d.binding.Close(); d.closeErr == nil {
				d.closeErr = err
			}
		}
	})
	return d.closeErr
}

func (d *Database) requireOpen(op string) error {
	if d == nil || d.db == nil || d.closed.Load() {
		return operationError(op, d.Path(), ErrClosed, nil)
	}
	return nil
}

func normalizeOptions(options Options) (Options, error) {
	defaults := DefaultOptions()
	if options.BusyTimeout == 0 {
		options.BusyTimeout = defaults.BusyTimeout
	}
	if options.ReadConnections == 0 {
		options.ReadConnections = defaults.ReadConnections
	}
	if options.Synchronous == "" {
		options.Synchronous = defaults.Synchronous
	}
	if options.CacheKiB == 0 {
		options.CacheKiB = defaults.CacheKiB
	}
	options.Synchronous = strings.ToUpper(options.Synchronous)

	var cause error
	switch {
	case options.Path == "":
		cause = fmt.Errorf("path is required")
	case options.BusyTimeout < time.Millisecond || options.BusyTimeout > maximumBusyTimeout:
		cause = fmt.Errorf("busy timeout must be between 1ms and %s", maximumBusyTimeout)
	case options.ReadConnections < 1 || options.ReadConnections > maximumReadConnections:
		cause = fmt.Errorf("read connections must be between 1 and %d", maximumReadConnections)
	case options.Synchronous != "NORMAL" && options.Synchronous != "FULL":
		cause = fmt.Errorf("synchronous must be NORMAL or FULL")
	case options.MMapBytes < 0 || options.MMapBytes > maximumMMapBytes:
		cause = fmt.Errorf("mmap bytes must be between 0 and %d", maximumMMapBytes)
	case options.CacheKiB < minimumCacheKiB || options.CacheKiB > maximumCacheKiB:
		cause = fmt.Errorf("cache KiB must be between %d and %d", minimumCacheKiB, maximumCacheKiB)
	}
	if cause != nil {
		return Options{}, operationError("validate options", options.Path, ErrInvalidOptions, cause)
	}
	return options, nil
}

func secureDatabasePath(path string) (string, bool, error) {
	if !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return "", false, operationError("validate path", path, ErrUnsafePath, fmt.Errorf("invalid path text"))
	}
	for _, character := range path {
		if character < 0x20 || character == 0x7f {
			return "", false, operationError("validate path", path, ErrUnsafePath, fmt.Errorf("control character"))
		}
	}
	if !filepath.IsAbs(path) {
		return "", false, operationError("validate path", path, ErrUnsafePath, fmt.Errorf("path must be absolute"))
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) || clean != path {
		return "", false, operationError("validate path", path, ErrUnsafePath, fmt.Errorf("path is not canonical"))
	}
	parent := filepath.Dir(clean)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", false, operationError("inspect database parent", parent, ErrUnsafePath, err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", false, operationError("inspect database parent", parent, ErrUnsafePath, fmt.Errorf("parent is not a real directory"))
	}
	if parentInfo.Mode().Perm()&0o077 != 0 {
		return "", false, operationError(
			"inspect database parent",
			parent,
			ErrUnsafePath,
			fmt.Errorf("parent permissions are %o, want owner-only", parentInfo.Mode().Perm()),
		)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", false, operationError("resolve database parent", parent, ErrUnsafePath, err)
	}
	if resolvedParent != parent {
		return "", false, operationError("resolve database parent", parent, ErrUnsafePath, fmt.Errorf("symlinked parent"))
	}

	info, err := os.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(clean, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			if closeErr := file.Close(); closeErr != nil {
				return "", false, operationError("close created database", clean, ErrOpenFailed, closeErr)
			}
			return clean, true, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return "", false, operationError("create database", clean, ErrOpenFailed, createErr)
		}
		// Another opener won the O_EXCL race. Re-inspect its result and accept
		// only the same regular-file policy used for an existing database.
		info, err = os.Lstat(clean)
	}
	if err != nil {
		return "", false, operationError("inspect database path", clean, ErrUnsafePath, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, operationError("inspect database path", clean, ErrUnsafePath, fmt.Errorf("path is not a regular file"))
	}
	if info.Mode().Perm() != 0o600 {
		return "", false, operationError(
			"inspect database permissions",
			clean,
			ErrUnsafePath,
			fmt.Errorf("database permissions are %o, want 600", info.Mode().Perm()),
		)
	}
	return clean, false, nil
}

// pathBinding pins the file and its immediate directory throughout bootstrap.
// The two os.SameFile checks compare the opened descriptors' device/inode
// identity with the path after SQLite connects and again after migration.
type pathBinding struct {
	path       string
	parentPath string
	file       *os.File
	parent     *os.File
}

func bindDatabasePath(path string) (*pathBinding, error) {
	parentPath := filepath.Dir(path)
	parent, err := os.Open(parentPath)
	if err != nil {
		return nil, operationError("bind database parent", parentPath, ErrUnsafePath, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		_ = parent.Close()
		return nil, operationError("bind database file", path, ErrUnsafePath, err)
	}
	binding := &pathBinding{path: path, parentPath: parentPath, file: file, parent: parent}
	if err := binding.verifyFilesystem(); err != nil {
		_ = binding.Close()
		return nil, err
	}
	return binding, nil
}

func (b *pathBinding) Close() error {
	if b == nil {
		return nil
	}
	var result error
	if b.file != nil {
		result = b.file.Close()
		b.file = nil
	}
	if b.parent != nil {
		if err := b.parent.Close(); result == nil {
			result = err
		}
		b.parent = nil
	}
	return result
}

func (b *pathBinding) verifyFilesystem() error {
	if b == nil || b.file == nil || b.parent == nil {
		return operationError("verify path binding", "", ErrUnsafePath, fmt.Errorf("binding is closed"))
	}
	boundFile, err := b.file.Stat()
	if err != nil {
		return operationError("stat bound database", b.path, ErrUnsafePath, err)
	}
	pathFile, err := os.Lstat(b.path)
	if err != nil {
		return operationError("stat database path", b.path, ErrUnsafePath, err)
	}
	if !pathFile.Mode().IsRegular() || pathFile.Mode()&os.ModeSymlink != 0 ||
		pathFile.Mode().Perm() != 0o600 || !os.SameFile(boundFile, pathFile) {
		return operationError("verify database inode", b.path, ErrUnsafePath, fmt.Errorf("file identity changed"))
	}
	boundParent, err := b.parent.Stat()
	if err != nil {
		return operationError("stat bound database parent", b.parentPath, ErrUnsafePath, err)
	}
	pathParent, err := os.Lstat(b.parentPath)
	if err != nil {
		return operationError("stat database parent path", b.parentPath, ErrUnsafePath, err)
	}
	if !pathParent.IsDir() || pathParent.Mode()&os.ModeSymlink != 0 ||
		pathParent.Mode().Perm()&0o077 != 0 || !os.SameFile(boundParent, pathParent) {
		return operationError("verify database parent inode", b.parentPath, ErrUnsafePath, fmt.Errorf("parent identity changed"))
	}
	return nil
}

func (b *pathBinding) verify(ctx context.Context, database queryExecutor) error {
	if err := b.verifyFilesystem(); err != nil {
		return err
	}
	rows, err := database.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return operationError("verify SQLite database path", b.path, classifyDatabaseError(err), err)
	}
	defer rows.Close()
	foundMain := false
	for rows.Next() {
		var sequence int
		var name, openedPath string
		if err := rows.Scan(&sequence, &name, &openedPath); err != nil {
			return operationError("verify SQLite database path", b.path, classifyDatabaseError(err), err)
		}
		if name == "main" {
			foundMain = true
			if filepath.Clean(openedPath) != b.path {
				return operationError("verify SQLite database path", b.path, ErrUnsafePath, fmt.Errorf("driver opened %q", openedPath))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return operationError("verify SQLite database path", b.path, classifyDatabaseError(err), err)
	}
	if !foundMain {
		return operationError("verify SQLite database path", b.path, ErrUnsafePath, fmt.Errorf("main database is absent"))
	}
	return b.verifyFilesystem()
}

func sqliteURI(options Options) string {
	query := url.Values{}
	query.Set("mode", "rwc")
	query.Set("cache", "private")
	query.Set("nofollow", "1")
	query.Set("_txlock", "immediate")
	query.Set("_dqs", "0")
	query.Set("_error_rc", "1")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "busy_timeout("+strconv.FormatInt(options.BusyTimeout.Milliseconds(), 10)+")")
	query.Add("_pragma", "trusted_schema(OFF)")
	query.Add("_pragma", "temp_store(MEMORY)")
	query.Add("_pragma", "cell_size_check(ON)")
	query.Add("_pragma", "synchronous("+options.Synchronous+")")
	query.Add("_pragma", "cache_size(-"+strconv.Itoa(options.CacheKiB)+")")
	query.Add("_pragma", "mmap_size("+strconv.FormatInt(options.MMapBytes, 10)+")")
	return (&url.URL{Scheme: "file", Path: options.Path, RawQuery: query.Encode()}).String()
}

func inspectOwnership(ctx context.Context, database *sql.DB, path string, plan []migration) error {
	var appID int64
	if err := database.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return operationError("read application id", path, classifyDatabaseError(err), err)
	}
	var userVersion int
	if err := database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return operationError("read user version", path, classifyDatabaseError(err), err)
	}
	if appID == applicationID {
		if userVersion < 3 || userVersion > currentDatabaseVersion {
			return operationError(
				"inspect ownership",
				path,
				ErrIncompatibleSchema,
				fmt.Errorf("application id is RKC but user_version is %d", userVersion),
			)
		}
		schemaVersion, err := readSchemaVersion(ctx, database)
		if err != nil {
			return operationError("inspect RKC schema", path, classifyMigrationError(err), err)
		}
		if schemaVersion != userVersion {
			return operationError(
				"inspect RKC schema",
				path,
				ErrIncompatibleSchema,
				fmt.Errorf("schema version %d does not match user_version %d", schemaVersion, userVersion),
			)
		}
		if err := checkGovernedSchemaCatalog(ctx, database, userVersion, plan); err != nil {
			kind := classifyDatabaseError(err)
			switch {
			case errors.Is(err, errSchemaCatalogDrift):
				kind = ErrIncompatibleSchema
			case errors.Is(err, ErrMigrationTampered):
				kind = ErrMigrationTampered
			}
			return operationError("inspect RKC schema catalogue", path, kind, err)
		}
		if err := checkMigrationJournal(ctx, database, plan[:userVersion]); err != nil {
			return operationError(
				"inspect RKC migration journal",
				path,
				ErrIncompatibleSchema,
				err,
			)
		}
		return nil
	}
	if appID != 0 {
		return operationError(
			"inspect ownership",
			path,
			ErrForeignDatabase,
			fmt.Errorf("application_id is %#x", appID),
		)
	}
	if userVersion != 0 {
		return operationError(
			"inspect ownership",
			path,
			ErrForeignDatabase,
			fmt.Errorf("unowned user_version is %d", userVersion),
		)
	}

	version, err := readSchemaVersion(ctx, database)
	if err != nil {
		kind := classifyDatabaseError(err)
		if errors.Is(err, ErrIncompatibleSchema) {
			kind = ErrForeignDatabase
		}
		return operationError("inspect ownership", path, kind, err)
	}
	if version == 1 || version == 2 {
		if err := checkLegacyCatalog(ctx, database, version, plan); err != nil {
			kind := classifyDatabaseError(err)
			switch {
			case errors.Is(err, errSchemaCatalogDrift):
				kind = ErrForeignDatabase
			case errors.Is(err, ErrMigrationTampered):
				kind = ErrMigrationTampered
			case errors.Is(err, ErrIncompatibleSchema):
				kind = ErrIncompatibleSchema
			}
			return operationError("inspect legacy catalog", path, kind, err)
		}
		return nil
	}
	if version != 0 {
		return operationError("inspect ownership", path, ErrForeignDatabase, fmt.Errorf("unowned schema"))
	}
	var objects int
	if err := database.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_schema
	         WHERE name NOT LIKE 'sqlite_%'`,
	).Scan(&objects); err != nil {
		return operationError("inspect ownership", path, classifyDatabaseError(err), err)
	}
	if objects != 0 {
		return operationError(
			"inspect ownership",
			path,
			ErrForeignDatabase,
			fmt.Errorf("unowned database contains %d schema objects", objects),
		)
	}
	return nil
}

func enableWAL(
	ctx context.Context,
	database *sql.DB,
	path string,
	busyTimeout time.Duration,
) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		var mode string
		err := database.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
		if err == nil {
			if !strings.EqualFold(mode, "wal") {
				return operationError(
					"enable WAL",
					path,
					ErrCheckFailed,
					fmt.Errorf("journal mode is %q", mode),
				)
			}
			return nil
		}
		kind := classifyDatabaseError(err)
		if kind != ErrBusy || time.Now().After(deadline) {
			return operationError("enable WAL", path, kind, err)
		}
		delay := 10 * time.Millisecond
		if remaining := time.Until(deadline); remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return operationError("enable WAL", path, ErrCanceled, ctx.Err())
		case <-timer.C:
		}
	}
}

func classifyDatabaseError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrCanceled
	}
	var sqliteErr *moderncsqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case sqliteResultInterrupt:
			return ErrCanceled
		case sqliteResultBusy, sqliteResultLocked:
			return ErrBusy
		case sqliteResultCantOpen, sqliteResultPermission, sqliteResultReadOnly:
			return ErrOpenFailed
		case sqliteResultCorrupt, sqliteResultNotADB, sqliteResultFormat:
			return ErrCorruptDatabase
		}
	}
	return ErrCheckFailed
}
