package accounts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestUserIDsNeverReused: deleting the account with the highest id must
// not hand that id to the next one, or audit rows and limits counted by
// user id would mix two people
func TestUserIDsNeverReused(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")
	_, err := db.DeleteUser(ctx, bob)
	require.NoError(t, err)
	carol := newUser(t, db, 3, "carol")
	require.Greater(t, carol.ID, bob.ID)
}

// TestMigrationUserIDs upgrades a database with data written by a server
// that knew only 0001 and 0002: every row stays, the sessions and tokens
// that point at users are not cascaded away by the table rebuild, foreign
// keys work after it, and new ids start past every id the database still
// names, deleted accounts in the audit log included
func TestMigrationUserIDs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hastebin.db")
	old, err := open(path, true)
	require.NoError(t, err)
	for _, name := range []string{"0001_init.sql", "0002_avatar.sql"} {
		data, err := migrationFS.ReadFile("migrations/" + name)
		require.NoError(t, err)
		_, err = old.W.Exec(string(data))
		require.NoError(t, err)
	}
	for _, stmt := range []string{
		"PRAGMA user_version = 2",
		`INSERT INTO users (id, github_id, login, created_at, default_visibility, abuse_strikes, avatar_url, disabled_at, disabled_reason)
			VALUES (1, 41, 'one', 1700000000, 1, 2, 'https://avatars.githubusercontent.com/u/41', NULL, NULL),
			(5, 45, 'five', 1700000001, 0, 0, NULL, 1700000100, 'spam')`,
		"INSERT INTO sessions (id_hash, user_id, csrf, created_at, auth_at, last_seen_at, expires_at) VALUES (x'01', 5, x'02', 1, 1, 1, 4000000000)",
		"INSERT INTO api_tokens (user_id, name, prefix, hash, created_at) VALUES (1, 'ci', 'hbp_ab12cd', x'03', 1)",
		"INSERT INTO pastes (key, user_id, title, created_at, size, root_key) VALUES ('abc', 1, 'kept', 1700000200, 12, 'abc')",
		"INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES ('abc', 1, 'abc.r1', 3, 1)",
		"INSERT INTO paste_tags (paste_key, tag) VALUES ('abc', 'build')",
		// Account 9 was deleted: only the audit log still names it
		"INSERT INTO audit_log (at, admin_user_id, admin_login, action, target) VALUES (1, 9, 'nine', 'account.delete', 'user:9')",
	} {
		_, err := old.W.Exec(stmt)
		require.NoError(t, err, stmt)
	}
	require.NoError(t, old.Close())

	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()
	version, err := db.UserVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion(), version)

	for table, want := range map[string]int{"users": 2, "sessions": 1, "api_tokens": 1, "pastes": 1, "paste_revisions": 1,
		"paste_tags": 1, "audit_log": 1} {
		var n int
		require.NoError(t, db.R.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
		require.Equal(t, want, n, table)
	}
	one, err := db.UserByID(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, "one", one.Login)
	require.Equal(t, 1, one.DefaultVisibility)
	require.Equal(t, 2, one.AbuseStrikes)
	require.Equal(t, "https://avatars.githubusercontent.com/u/41", one.AvatarURL)
	five, err := db.UserByID(ctx, 5)
	require.NoError(t, err)
	require.True(t, five.Disabled())
	require.Equal(t, "spam", five.DisabledReason)
	_, user, err := db.SessionByHash(ctx, []byte{1})
	require.NoError(t, err)
	require.Equal(t, int64(5), user.ID)

	var schema string
	require.NoError(t, db.R.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'users'").Scan(&schema))
	require.Contains(t, schema, "AUTOINCREMENT")
	var fk int
	require.NoError(t, db.W.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	require.Equal(t, 1, fk, "the writer has its foreign keys back")
	_, err = db.W.Exec("INSERT INTO sessions (id_hash, user_id, csrf, created_at, auth_at, last_seen_at, expires_at) VALUES (x'09', 77, x'02', 1, 1, 1, 2)")
	require.Error(t, err, "a session of no user is refused")
	_, err = db.W.Exec("DELETE FROM users WHERE id = 5")
	require.NoError(t, err)
	require.NoError(t, db.R.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&fk))
	require.Zero(t, fk, "ON DELETE CASCADE still works")

	next := newUser(t, db, 50, "next")
	require.Equal(t, int64(10), next.ID, "past the deleted account 9")
}

// TestListSortsUseIndexes: every order the list offers reads its page in
// index order, so a page stops at its last row instead of sorting all of
// a user's pastes; the newer version is looked up by its own index
func TestListSortsUseIndexes(t *testing.T) {
	db := openTest(t)
	full := []string{"-created", "created", "-size", "-views", "title", "-last_opened"}
	for sort := range Sorts {
		_, page, args := listSQL(1, ListQuery{Visibility: -1, Sort: sort})
		rows, err := db.R.Query("EXPLAIN QUERY PLAN "+page, append(args, 25, 0)...)
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Close())
		text := strings.Join(plan, "\n")
		require.Contains(t, text, "pastes_prev", sort)
		require.NotContains(t, text, "SCAN pastes\n", sort)
		inner := text[:strings.Index(text, "SCAN page")]
		require.NotContains(t, inner, "USE TEMP B-TREE FOR ORDER BY", "sort %s sorts all rows:\n%s", sort, text)
		for _, f := range full {
			if f == sort {
				require.NotContains(t, inner, "TEMP B-TREE", "sort %s:\n%s", sort, text)
			}
		}
	}
}

// TestCostlyReadsUseTheirOwnPool: the list, search, tags, export and admin
// lists read on L. With every connection of R taken they still work, and
// with every connection of L taken the access gate and the logins still
// read while a list gives up at its deadline as busy.
func TestCostlyReadsUseTheirOwnPool(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	addPaste(t, db, NewPaste{Key: "k1", UserID: alice.ID, Tags: []string{"t"}, Language: "go"})

	hold := func(pool *sql.DB, n int) func() {
		var conns []*sql.Conn
		for range n {
			c, err := pool.Conn(ctx)
			require.NoError(t, err)
			conns = append(conns, c)
		}
		return func() {
			for _, c := range conns {
				c.Close()
			}
		}
	}

	release := hold(db.R, readers)
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	list, total, err := db.ListPastes(short, alice.ID, ListQuery{Q: "k", Visibility: -1, Page: 1, PerPage: 25})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, list, 1)
	_, err = db.Tags(short, alice.ID, 10)
	require.NoError(t, err)
	_, err = db.Languages(short, alice.ID, 10)
	require.NoError(t, err)
	require.NoError(t, db.EachPaste(short, alice.ID, 10, func(*Paste) error { return nil }))
	_, _, err = db.ListUsers(short, UserQuery{Page: 1, PerPage: 10})
	require.NoError(t, err)
	_, err = db.AuditLog(short, 10, 0)
	require.NoError(t, err)
	cancel()
	release()

	release = hold(db.L, listReaders)
	defer release()
	short, cancel = context.WithTimeout(ctx, time.Second)
	defer cancel()
	a, err := db.AccessOf(short, "k1")
	require.NoError(t, err)
	require.True(t, a.Registered)
	_, err = db.UserByID(short, alice.ID)
	require.NoError(t, err)
	wait, cancelWait := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelWait()
	_, _, err = db.ListPastes(wait, alice.ID, ListQuery{Visibility: -1, Page: 1, PerPage: 25})
	require.Error(t, err)
	require.True(t, IsBusy(err), err)
}

// TestDeleteUserInChunks: an account with many pastes is deleted in short
// transactions, so other writers get the database between them, and each
// chunk's keys read as pending from the moment their rows are gone
func TestDeleteUserInChunks(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")
	for i := range 35 {
		addPaste(t, db, NewPaste{Key: fmt.Sprintf("a%02d", i), UserID: alice.ID, Visibility: VisibilityPrivate})
	}
	_, err := db.W.Exec("INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES ('a07', 1, 'a07.r1', 3, 0)")
	require.NoError(t, err)
	session := sha256.Sum256([]byte("s"))
	require.NoError(t, db.CreateSession(ctx, session[:], alice.ID, make([]byte, 32), time.Now().Add(time.Hour)))

	defer func(n int) { deleteUserChunk, deleteUserChunkDone = n, nil }(deleteUserChunk)
	deleteUserChunk = 10
	var chunks []int
	deleteUserChunkDone = func(n int) {
		chunks = append(chunks, n)
		// The writer is free between chunks: another account writes now
		wctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err := db.W.ExecContext(wctx, "UPDATE users SET last_seen_at = ? WHERE id = ?", len(chunks), bob.ID)
		require.NoError(t, err)
		// Already logged out, and what this chunk deleted is hidden
		_, _, err = db.SessionByHash(ctx, session[:])
		require.ErrorIs(t, err, ErrNotFound)
		var pastes, pending int
		require.NoError(t, db.R.QueryRow("SELECT COUNT(*) FROM pastes WHERE user_id = ?", alice.ID).Scan(&pastes))
		require.NoError(t, db.R.QueryRow("SELECT COUNT(*) FROM delete_queue WHERE store_key NOT LIKE '%.r%'").Scan(&pending))
		require.Equal(t, 35, pastes+pending, "every paste is either a row or queued")
		rows, err := db.R.Query("SELECT store_key FROM delete_queue")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var key string
			require.NoError(t, rows.Scan(&key))
			a, err := db.AccessOf(ctx, key)
			require.NoError(t, err)
			require.True(t, a.Pending, key)
			require.False(t, a.Registered, key)
		}
	}
	n, err := db.DeleteUser(ctx, alice)
	require.NoError(t, err)
	require.EqualValues(t, 36, n)
	require.Equal(t, []int{10, 10, 10, 5}, chunks)
	_, err = db.UserByID(ctx, alice.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.Len(t, queued(t, db), 36)
	entries, err := db.AuditLog(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "account.delete", entries[0].Action)

	_, err = db.DeleteUser(ctx, alice)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestDrainerSettlesOncePerPass: a pass deletes its batch from the store
// first and records the outcome in one write at the end, not one write per
// key on the single writer
func TestDrainerSettlesOncePerPass(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	var keys []string
	for i := range 30 {
		key := fmt.Sprintf("k%02d", i)
		keys = append(keys, key)
		addPaste(t, db, NewPaste{Key: key, UserID: alice.ID})
	}
	_, _, err := db.DeletePastes(ctx, alice.ID, keys)
	require.NoError(t, err)

	var mu sync.Mutex
	var lens []int64
	d := NewDrainer(db, func(key string) error {
		mu.Lock()
		defer mu.Unlock()
		n, err := db.QueueLen(ctx)
		require.NoError(t, err)
		lens = append(lens, n)
		if key == "k05" {
			return fmt.Errorf("store down")
		}
		return nil
	})
	require.Equal(t, 29, d.Drain(ctx))
	require.Len(t, lens, 30)
	for _, n := range lens {
		require.EqualValues(t, 30, n, "nothing is written while the pass runs")
	}
	require.Equal(t, []string{"k05"}, queued(t, db))
	var attempts int
	require.NoError(t, db.R.QueryRow("SELECT attempts FROM delete_queue WHERE store_key = 'k05'").Scan(&attempts))
	require.Equal(t, 1, attempts)
}
