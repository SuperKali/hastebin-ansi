package accounts

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// openTest opens a new database in a temp dir and closes it after the test
func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "hastebin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "hastebin.db")
	require.False(t, Exists(path))

	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()
	require.True(t, Exists(path))

	// A write makes sure the WAL is in use
	_, _, err = db.LoginUser(context.Background(), 1, "octocat", true)
	require.NoError(t, err)

	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		require.NoError(t, err, name)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	var mode string
	require.NoError(t, db.R.QueryRow("PRAGMA journal_mode").Scan(&mode))
	require.Equal(t, "wal", mode)
	var fk int
	require.NoError(t, db.W.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	require.Equal(t, 1, fk)
	var timeout int
	require.NoError(t, db.R.QueryRow("PRAGMA busy_timeout").Scan(&timeout))
	require.Equal(t, 5000, timeout)

	// The reader pool cannot write
	_, err = db.R.Exec("DELETE FROM users")
	require.Error(t, err)
}

func TestOpenTightensLooseMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hastebin.db")
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hastebin.db")
	db, err := Open(path)
	require.NoError(t, err)
	version, err := db.UserVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, SchemaVersion(), version)
	require.GreaterOrEqual(t, version, 1)

	// Every table of the full schema is there
	for _, table := range []string{"users", "sessions", "api_tokens", "pastes", "paste_revisions",
		"paste_tags", "tombstones", "delete_queue", "ip_bans", "audit_log"} {
		var name string
		require.NoError(t, db.R.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name), table)
	}
	_, _, err = db.LoginUser(context.Background(), 7, "seven", true)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Opening again changes nothing and keeps the data
	for range 2 {
		db, err = Open(path)
		require.NoError(t, err)
		version, err = db.UserVersion(context.Background())
		require.NoError(t, err)
		require.Equal(t, SchemaVersion(), version)
		user, err := db.UserByGitHubID(context.Background(), 7)
		require.NoError(t, err)
		require.Equal(t, "seven", user.Login)
		require.NoError(t, db.Close())
	}
}

func TestOpenNewerDatabaseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hastebin.db")
	db, err := Open(path)
	require.NoError(t, err)
	_, err = db.W.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion()+1))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(path)
	require.ErrorIs(t, err, ErrNewerDatabase)
	require.ErrorContains(t, err, "this server knows up to")
}

func TestLoginUser(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_800_000_000, 0)
	db.now = func() time.Time { return now }

	// Without allowCreate an unknown id is not registered
	_, _, err := db.LoginUser(ctx, 100, "alice", false)
	require.ErrorIs(t, err, ErrNotFound)

	alice, created, err := db.LoginUser(ctx, 100, "alice", true)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "alice", alice.Login)
	require.Equal(t, now.UTC(), alice.CreatedAt)

	// A rename on GitHub keeps the account and refreshes the login
	now = now.Add(time.Hour)
	renamed, created, err := db.LoginUser(ctx, 100, "alice2", false)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, alice.ID, renamed.ID)
	require.Equal(t, "alice2", renamed.Login)
	require.Equal(t, now.UTC(), renamed.LastLoginAt)

	// Someone else taking the old login name gets a different account
	other, created, err := db.LoginUser(ctx, 200, "alice", true)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, alice.ID, other.ID)

	require.NoError(t, db.SetDisabled(ctx, other.ID, true, "spam"))
	other, err = db.UserByID(ctx, other.ID)
	require.NoError(t, err)
	require.True(t, other.Disabled())
	require.Equal(t, "spam", other.DisabledReason)
	require.NoError(t, db.SetDisabled(ctx, other.ID, false, ""))
	other, err = db.UserByID(ctx, other.ID)
	require.NoError(t, err)
	require.False(t, other.Disabled())
	require.ErrorIs(t, db.SetDisabled(ctx, 9999, true, "x"), ErrNotFound)
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_800_000_000, 0)
	db.now = func() time.Time { return now }

	user, _, err := db.LoginUser(ctx, 1, "octocat", true)
	require.NoError(t, err)
	hash := sha256.Sum256([]byte("cookie"))
	require.NoError(t, db.CreateSession(ctx, hash[:], user.ID, []byte("csrf"), now.Add(time.Hour)))

	s, u, err := db.SessionByHash(ctx, hash[:])
	require.NoError(t, err)
	require.Equal(t, user.ID, u.ID)
	require.Equal(t, []byte("csrf"), s.CSRF)
	require.Equal(t, now.Add(time.Hour).UTC(), s.ExpiresAt)

	now = now.Add(time.Minute)
	require.NoError(t, db.TouchSession(ctx, hash[:], user.ID))
	s, u, err = db.SessionByHash(ctx, hash[:])
	require.NoError(t, err)
	require.Equal(t, now.UTC(), s.LastSeenAt)
	require.Equal(t, now.UTC(), u.LastSeenAt)

	other := sha256.Sum256([]byte("other"))
	_, _, err = db.SessionByHash(ctx, other[:])
	require.ErrorIs(t, err, ErrNotFound)

	// Stale sessions go: past their end, or idle too long
	idle := sha256.Sum256([]byte("idle"))
	require.NoError(t, db.CreateSession(ctx, idle[:], user.ID, []byte("c"), now.Add(48*time.Hour)))
	now = now.Add(2 * time.Hour)
	require.NoError(t, db.TouchSession(ctx, idle[:], user.ID))
	n, err := db.DeleteStaleSessions(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	now = now.Add(25 * time.Hour)
	n, err = db.DeleteStaleSessions(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	require.NoError(t, db.CreateSession(ctx, hash[:], user.ID, []byte("csrf"), now.Add(time.Hour)))
	require.NoError(t, db.DeleteSession(ctx, hash[:]))
	_, _, err = db.SessionByHash(ctx, hash[:])
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLogoutAllAndAudit(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	user, _, err := db.LoginUser(ctx, 1, "octocat", true)
	require.NoError(t, err)
	other, _, err := db.LoginUser(ctx, 2, "hubot", true)
	require.NoError(t, err)
	for i, id := range []int64{user.ID, user.ID, other.ID} {
		hash := sha256.Sum256([]byte{byte(i)})
		require.NoError(t, db.CreateSession(ctx, hash[:], id, []byte("c"), time.Now().Add(time.Hour)))
	}
	_, err = db.W.Exec("INSERT INTO api_tokens (user_id, prefix, hash, created_at) VALUES (?, 'hbp_x', x'01', 0)", user.ID)
	require.NoError(t, err)

	sessions, tokens, err := db.LogoutAll(ctx, user, false)
	require.NoError(t, err)
	require.EqualValues(t, 2, sessions)
	require.Zero(t, tokens)
	sessions, tokens, err = db.LogoutAll(ctx, user, true)
	require.NoError(t, err)
	require.Zero(t, sessions)
	require.EqualValues(t, 1, tokens)

	totals, err := db.Totals(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, totals.Users)
	require.EqualValues(t, 1, totals.Sessions)
	require.Zero(t, totals.Tokens)

	entries, err := db.AuditLog(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "session.logout_all", entries[0].Action)
	require.Equal(t, `{"revoke_tokens":true}`, entries[0].Detail)
	require.Equal(t, user.ID, entries[0].ActorID)
	require.Equal(t, "user:"+fmt.Sprint(user.ID), entries[0].Target)

	require.NoError(t, db.Audit(ctx, AuditEntry{ActorLogin: "system", Action: "test"}))

	// The audit log is append-only
	_, err = db.W.Exec("UPDATE audit_log SET action = 'x'")
	require.ErrorContains(t, err, "append-only")
	_, err = db.W.Exec("DELETE FROM audit_log")
	require.ErrorContains(t, err, "append-only")
	entries, err = db.AuditLog(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Zero(t, entries[0].ActorID)
}

func TestBackupWhileWriting(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hastebin.db")
	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()

	// Writes keep going while the backup runs
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _, err := db.LoginUser(ctx, i, fmt.Sprintf("user%d", i), true)
			if err != nil {
				t.Errorf("write during backup: %v", err)
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)

	out := filepath.Join(dir, "backup.db")
	err = Backup(ctx, path, out)
	close(stop)
	wg.Wait()
	require.NoError(t, err)

	info, err := os.Stat(out)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The copy is a working database at the same schema version
	copied, err := Open(out)
	require.NoError(t, err)
	defer copied.Close()
	version, err := copied.UserVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion(), version)
	totals, err := copied.Totals(ctx)
	require.NoError(t, err)
	require.Positive(t, totals.Users)

	// An existing file is never overwritten, and a missing database is an error
	require.ErrorContains(t, Backup(ctx, path, out), "creating the backup file")
	require.Error(t, Backup(ctx, filepath.Join(dir, "missing.db"), filepath.Join(dir, "b2.db")))
	require.NoFileExists(t, filepath.Join(dir, "missing.db"))
}

func TestDSNEscapesPath(t *testing.T) {
	require.Contains(t, dsn("/data/a?b#c%d.db"), "file:/data/a%3Fb%23c%25d.db?")
	path := filepath.Join(t.TempDir(), "odd ?#% name.db")
	db, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.FileExists(t, path)
}

func TestIsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hastebin.db")
	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()

	// One process holds the write lock; another gives up after its busy timeout
	tx, err := db.W.Begin()
	require.NoError(t, err)
	_, err = tx.Exec("INSERT INTO users (github_id, login, created_at) VALUES (1, 'a', 0)")
	require.NoError(t, err)
	defer tx.Rollback()

	other, err := open(path, false)
	require.NoError(t, err)
	defer other.Close()
	_, err = other.W.Exec("PRAGMA busy_timeout = 50")
	require.NoError(t, err)
	_, err = other.W.Exec("INSERT INTO users (github_id, login, created_at) VALUES (2, 'b', 0)")
	require.Error(t, err)
	require.True(t, IsBusy(err), "%v", err)
	require.False(t, IsBusy(ErrNotFound))
}
