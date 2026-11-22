package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

const (
	// MemoryPath is the path that selects a private in-memory database.
	MemoryPath = ":memory:"

	driverName = "sqlite"
	// busyTimeout is how long a connection waits for a lock held by another
	// connection before it gives up with SQLITE_BUSY.
	busyTimeout = 10 * time.Second
	// readerConns bounds the read-only connection pool.
	readerConns = 4
	// dbFileMode is the mode of the database files: they hold inventories
	// that are served to authenticated users only.
	dbFileMode os.FileMode = 0o600
)

// ErrSchemaVersion is returned by Start when the database file holds a
// schema this binary cannot upgrade: a newer one, or tables it did not
// create. Nothing is changed in that case.
var ErrSchemaVersion = errors.New("store: database has another schema")

// ErrNoWAL is returned by Start when the database file cannot be put into
// write-ahead-log mode.
var ErrNoWAL = errors.New("store: write-ahead log is not available")

// ErrClosed is returned by every method before Start and after Stop.
var ErrClosed = errors.New("store: not started")

// sqliteStore implements Store on modernc.org/sqlite. All writes go through
// one connection (so writers queue in Go instead of fighting over the file
// lock) and reads through a small pool of query-only connections; with WAL,
// readers never block the writer.
type sqliteStore struct {
	log  logrus.FieldLogger
	path string

	mu     sync.RWMutex
	writer *sql.DB
	reader *sql.DB
	// opened is when Start opened the database: a host's silence is not
	// counted from before it (countedValidators).
	opened time.Time

	statuses statusCache
}

var _ Store = (*sqliteStore)(nil)

// New returns a Store on the SQLite database file at path. The file and its
// directory are created by Start if they do not exist. The path ":memory:"
// selects an in-memory database that is private to this Store and discarded
// by Stop.
func New(log logrus.FieldLogger, path string) Store {
	return &sqliteStore{
		log:  log.WithField("package", "store"),
		path: path,
	}
}

// NewMemory returns a Store on a private in-memory SQLite database. It is the
// real implementation, meant for the tests of packages that need a Store.
func NewMemory(log logrus.FieldLogger) Store {
	return New(log, MemoryPath)
}

// Start opens the database and brings its schema up to date.
func (s *sqliteStore) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writer != nil {
		return errors.New("store: already started")
	}

	base, err := s.prepareDSN()
	if err != nil {
		return err
	}

	writer, err := openPool(ctx, base+"&_txlock=immediate", 1)
	if err != nil {
		return fmt.Errorf("store: open %s: %w", s.path, err)
	}

	if s.path != MemoryPath {
		// The DSN asks for WAL, but SQLite answers that pragma with the mode
		// it ended up in instead of an error.
		err = checkJournalMode(ctx, writer)
		if err != nil {
			return errors.Join(fmt.Errorf("store: open %s: %w", s.path, err), writer.Close())
		}
	}

	steps, err := loadMigrations()
	if err != nil {
		return errors.Join(err, writer.Close())
	}

	err = s.migrate(ctx, writer, steps)
	if err != nil {
		return errors.Join(err, writer.Close())
	}

	reader, err := openPool(ctx, base+"&_pragma=query_only(1)", readerConns)
	if err != nil {
		return errors.Join(fmt.Errorf("store: open %s read-only: %w", s.path, err), writer.Close())
	}

	s.writer, s.reader = writer, reader
	s.opened = time.Now()

	s.log.WithFields(logrus.Fields{"path": s.path, "schema": len(steps)}).Info("database open")

	return nil
}

// Stop closes the database. It is safe to call more than once.
func (s *sqliteStore) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writer == nil {
		return nil
	}

	// Readers first: closing the last connection checkpoints the WAL, and
	// that must be the writer.
	err := errors.Join(s.reader.Close(), s.writer.Close())
	s.writer, s.reader = nil, nil

	if err != nil {
		return fmt.Errorf("store: close: %w", err)
	}

	return nil
}

// prepareDSN returns the connection string shared by the writer and the
// readers and makes sure a database file can be created.
func (s *sqliteStore) prepareDSN() (string, error) {
	pragmas := fmt.Sprintf(
		"_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)", busyTimeout.Milliseconds())

	if s.path == MemoryPath {
		// SQLite's memdb VFS shares one in-memory database between all
		// connections of this process that open the same "/name". The name
		// is random so every Store gets its own. (A plain ":memory:" would
		// give each pooled connection a separate empty database.)
		name := make([]byte, 12)
		if _, err := rand.Read(name); err != nil {
			return "", fmt.Errorf("store: name in-memory database: %w", err)
		}

		return "file:/upgradoor-" + hex.EncodeToString(name) + "?vfs=memdb&" + pragmas, nil
	}

	if s.path == "" {
		return "", errors.New("store: database path is empty")
	}

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", fmt.Errorf("store: create database directory: %w", err)
		}
	}

	// Create the file ourselves so it is not world-readable: it holds
	// inventories that are served to authenticated users only. SQLite gives
	// new -wal and -shm files the mode of the main file.
	file, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE, dbFileMode)
	if err != nil {
		return "", fmt.Errorf("store: create database file: %w", err)
	}

	if err := file.Close(); err != nil {
		return "", fmt.Errorf("store: create database file: %w", err)
	}

	// The mode above only applies to a new file. A database that was there
	// before (restored from a backup, created by another tool) and files a
	// crash left next to it are tightened here.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := restrictFile(s.path + suffix); err != nil {
			return "", err
		}
	}

	return "file:" + escapeURIPath(s.path) + "?" + pragmas +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)", nil
}

// restrictFile removes the group and other permission bits of a database
// file, if it exists and has any.
func restrictFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("store: check mode of %s: %w", path, err)
	}

	mode := info.Mode().Perm()
	if mode&^dbFileMode == 0 {
		return nil
	}

	if err := os.Chmod(path, mode&dbFileMode); err != nil {
		return fmt.Errorf("store: %s is accessible to others (mode %#o) and cannot be "+
			"restricted: %w", path, mode, err)
	}

	return nil
}

// checkJournalMode fails unless the database is in WAL mode. The reader pool
// relies on it: with a rollback journal, readers and the writer block each
// other for up to busyTimeout.
func checkJournalMode(ctx context.Context, db *sql.DB) error {
	var mode string

	err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode)
	if err != nil {
		return fmt.Errorf("read journal mode: %w", err)
	}

	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("%w: journal mode is %q (the filesystem must support "+
			"shared memory files)", ErrNoWAL, mode)
	}

	return nil
}

// escapeURIPath escapes the characters that end the path part of an SQLite
// URI filename.
func escapeURIPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
}

// openPool opens a connection pool of at most conns connections and checks
// that the database is reachable.
func openPool(ctx context.Context, dsn string, conns int) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	// Connections are kept for the life of the pool: an in-memory database
	// lives only as long as a connection to it is open.
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := db.PingContext(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	return db, nil
}

// write runs fn in one write transaction: everything fn did is committed
// when it returns nil and rolled back otherwise.
func (s *sqliteStore) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.RLock()
	db := s.writer
	s.mu.RUnlock()

	return inTx(ctx, db, fn)
}

// read runs fn in one read transaction, which gives it a consistent snapshot
// across several queries.
func (s *sqliteStore) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.RLock()
	db := s.reader
	s.mu.RUnlock()

	return inTx(ctx, db, fn)
}

func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	if db == nil {
		return ErrClosed
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}

		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}

	return nil
}

// rowScanner is the part of *sql.Row and *sql.Rows the scan helpers need.
type rowScanner interface {
	Scan(dest ...any) error
}

// queryRows runs a query and calls scan for every row.
func queryRows(
	ctx context.Context, tx *sql.Tx, query string, args []any, scan func(row rowScanner) error,
) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}

	return rows.Err()
}

// conditions collects the WHERE terms of a query and their arguments.
type conditions struct {
	terms []string
	args  []any
}

func (c *conditions) add(term string, args ...any) {
	c.terms = append(c.terms, term)
	c.args = append(c.args, args...)
}

// where returns "" or " WHERE a AND b".
func (c *conditions) where() string {
	if len(c.terms) == 0 {
		return ""
	}

	return " WHERE " + strings.Join(c.terms, " AND ")
}

// placeholders returns "?, ?, ?" with n placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}

	return strings.Repeat("?, ", n-1) + "?"
}

// storedTime returns t as it comes back from an INTEGER millisecond column:
// in UTC, truncated to milliseconds. Times kept inside JSON columns are
// passed through it so that everything read back has one precision.
func storedTime(t time.Time) time.Time { return fromMillis(millis(t)) }

// storedTimePtr is storedTime for an optional time. It returns a new
// pointer; the caller's value is not changed.
func storedTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}

	stored := storedTime(*t)

	return &stored
}

// millis encodes a time as the schema stores it: unix milliseconds.
func millis(t time.Time) int64 { return t.UnixMilli() }

// fromMillis decodes a stored time.
func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// nullMillis encodes an optional time.
func nullMillis(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: millis(*t), Valid: true}
}

// escapeLike escapes the wildcards of a LIKE pattern for use with
// ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
