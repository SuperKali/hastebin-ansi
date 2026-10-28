// Package accounts keeps registered users, their sessions and the metadata of
// their pastes in one SQLite file. It is pure Go (modernc.org/sqlite), so the
// server still builds with CGO off.
package accounts

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// FileMode is the mode of the database file and its -wal and -shm files
const FileMode = 0o600

// readers is how many connections may read at the same time. Writes go
// through one connection, so they never wait on each other inside SQLite.
const readers = 4

// pragmas are set on every connection. busy_timeout comes first, so the
// switch to WAL waits for a busy database too. cache_size -8000 is 8 MiB per
// connection, outside memory_budget.
var pragmas = []string{
	"busy_timeout(5000)",
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"foreign_keys(ON)",
	"cache_size(-8000)",
}

// ErrNewerDatabase is returned by Open when the database was written by a
// newer version of the server, which this one cannot read safely
var ErrNewerDatabase = errors.New("the accounts database is newer than this server")

// DB is the accounts database: one connection for writes, a small pool for reads
type DB struct {
	path string

	// W writes, one statement or transaction at a time. R only reads.
	W *sql.DB
	R *sql.DB

	// now is the clock, replaced in tests
	now func() time.Time
}

// Exists reports whether a database file is at path
func Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Open opens the database at path, creating it with mode 0600 when missing,
// and brings its schema up to date in one transaction. A database written by
// a newer server gives ErrNewerDatabase.
func Open(path string) (*DB, error) {
	db, err := open(path, true)
	if err != nil {
		return nil, err
	}
	if err := db.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	// The -wal and -shm files exist now; keep them as private as the database
	db.tighten()
	return db, nil
}

// open opens the database at path. With create it makes a missing file, with
// mode 0600, else a missing file is an error.
func open(path string, create bool) (*DB, error) {
	if path == "" {
		return nil, errors.New("no accounts database path")
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("creating the accounts database folder: %w", err)
		}
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, FileMode)
		if err != nil {
			return nil, fmt.Errorf("creating the accounts database: %w", err)
		}
		file.Close()
	} else if !Exists(path) {
		return nil, fmt.Errorf("no accounts database at %s: %w", path, fs.ErrNotExist)
	}
	// A file created earlier with a looser mode is made private too
	if err := os.Chmod(path, FileMode); err != nil {
		return nil, fmt.Errorf("setting the mode of the accounts database: %w", err)
	}

	w, err := sql.Open("sqlite", dsn(path, "_txlock=immediate"))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	r, err := sql.Open("sqlite", dsn(path, "_pragma=query_only(1)"))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(readers)
	r.SetMaxIdleConns(readers)

	db := &DB{path: path, W: w, R: r, now: time.Now}
	if err := w.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening the accounts database: %w", err)
	}
	return db, nil
}

// dsn is the modernc.org/sqlite name of the database file at path with the
// pragmas and any extra parameters
func dsn(path string, extra ...string) string {
	params := make([]string, 0, len(pragmas)+len(extra))
	for _, pragma := range pragmas {
		params = append(params, "_pragma="+pragma)
	}
	params = append(params, extra...)
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + strings.Join(params, "&")
}

// tighten sets mode 0600 on the -wal and -shm files, when they exist
func (db *DB) tighten() {
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(db.path+suffix, FileMode)
	}
}

// Path returns the database file
func (db *DB) Path() string {
	return db.path
}

// Close closes both handles
func (db *DB) Close() error {
	return errors.Join(db.R.Close(), db.W.Close())
}

// migration is one embedded schema change, numbered by its file name
type migration struct {
	version int
	name    string
	sql     string
}

// migrations returns the embedded migrations in order
func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var list []migration
	for _, entry := range entries {
		name := entry.Name()
		number, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("migration %s: name must be NNNN_name.sql", name)
		}
		version, err := strconv.Atoi(number)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration %s: name must start with its number", name)
		}
		data, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		list = append(list, migration{version: version, name: name, sql: string(data)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })
	for i, m := range list {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: expected number %d", m.name, i+1)
		}
	}
	return list, nil
}

// SchemaVersion returns the newest schema this server knows
func SchemaVersion() int {
	list, err := migrations()
	if err != nil || len(list) == 0 {
		return 0
	}
	return list[len(list)-1].version
}

// migrate applies the migrations the database does not have yet, all in one
// transaction, and records the version in PRAGMA user_version
func (db *DB) migrate(ctx context.Context) error {
	list, err := migrations()
	if err != nil {
		return err
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting the accounts schema update: %w", err)
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading the accounts schema version: %w", err)
	}
	newest := len(list)
	if current > newest {
		return fmt.Errorf("%w: %s has schema version %d, this server knows up to %d. Run the newer server, or restore a backup made by this version",
			ErrNewerDatabase, db.path, current, newest)
	}
	if current == newest {
		return nil
	}
	for _, m := range list[current:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("accounts schema update %s: %w", m.name, err)
		}
	}
	// PRAGMA takes no bound parameters; newest is a number from this binary
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(newest)); err != nil {
		return fmt.Errorf("recording the accounts schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the accounts schema update: %w", err)
	}
	return nil
}

// UserVersion returns the schema version recorded in the database
func (db *DB) UserVersion(ctx context.Context) (int, error) {
	var version int
	err := db.R.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	return version, err
}

// Backup writes a consistent copy of the database at path to out with VACUUM
// INTO. It is safe while the server runs. out must not exist yet; it is
// created with mode 0600. The database is not migrated.
func Backup(ctx context.Context, path, out string) error {
	if out == "" {
		return errors.New("no backup file given")
	}
	db, err := open(path, false)
	if err != nil {
		return err
	}
	defer db.Close()

	// VACUUM INTO accepts an empty file, so it can be made private first
	file, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return fmt.Errorf("creating the backup file: %w", err)
	}
	file.Close()
	if _, err := db.W.ExecContext(ctx, "VACUUM INTO ?", out); err != nil {
		os.Remove(out)
		return fmt.Errorf("writing the backup: %w", err)
	}
	return os.Chmod(out, FileMode)
}

// sqliteBusy and sqliteLocked are the primary SQLite result codes of a
// database that stayed locked past busy_timeout
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// IsBusy reports whether err means the database stayed locked past the busy timeout
func IsBusy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	code := e.Code() & 0xff
	return code == sqliteBusy || code == sqliteLocked
}

// unix returns t as Unix seconds
func unix(t time.Time) int64 {
	return t.Unix()
}

// fromUnix turns Unix seconds into a UTC time, the zero time for NULL
func fromUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}
