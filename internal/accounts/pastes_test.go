package accounts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newUser makes an account and returns it
func newUser(t *testing.T, db *DB, githubID int64, login string) *User {
	t.Helper()
	u, _, err := db.LoginUser(context.Background(), githubID, login, true)
	require.NoError(t, err)
	return u
}

// addPaste records a paste of a user with no limits
func addPaste(t *testing.T, db *DB, p NewPaste) {
	t.Helper()
	require.NoError(t, db.InsertPaste(context.Background(), p, Limits{}))
}

func queued(t *testing.T, db *DB) []string {
	t.Helper()
	batch, err := db.QueueBatch(context.Background(), 1000)
	require.NoError(t, err)
	var keys []string
	for _, item := range batch {
		keys = append(keys, item.StoreKey)
	}
	sort.Strings(keys)
	return keys
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")

	hash := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }
	tok, err := db.CreateToken(ctx, alice.ID, "laptop", "hbp_abcdef", hash("one"), 2)
	require.NoError(t, err)
	require.Equal(t, "laptop", tok.Name)
	require.Equal(t, "hbp_abcdef", tok.Prefix)
	require.True(t, tok.LastUsedAt.IsZero())
	_, err = db.CreateToken(ctx, alice.ID, "ci", "hbp_ghijkl", hash("two"), 2)
	require.NoError(t, err)
	_, err = db.CreateToken(ctx, alice.ID, "third", "hbp_mnopqr", hash("three"), 2)
	require.ErrorIs(t, err, ErrTooMany)

	got, user, err := db.TokenByHash(ctx, hash("one"))
	require.NoError(t, err)
	require.Equal(t, tok.ID, got.ID)
	require.Equal(t, "alice", user.Login)
	_, _, err = db.TokenByHash(ctx, hash("nope"))
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, db.TouchToken(ctx, tok.ID, alice.ID))
	got, _, err = db.TokenByHash(ctx, hash("one"))
	require.NoError(t, err)
	require.False(t, got.LastUsedAt.IsZero())

	list, err := db.Tokens(ctx, alice.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	n, err := db.CountTokens(ctx, bob.ID)
	require.NoError(t, err)
	require.Zero(t, n)

	// Bob cannot revoke Alice's token
	require.ErrorIs(t, db.DeleteToken(ctx, bob.ID, tok.ID), ErrNotFound)
	require.NoError(t, db.DeleteToken(ctx, alice.ID, tok.ID))
	require.ErrorIs(t, db.DeleteToken(ctx, alice.ID, tok.ID), ErrNotFound)
	_, _, err = db.TokenByHash(ctx, hash("one"))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestInsertPasteLimitsAndRevocation(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := newUser(t, db, 1, "alice")

	limits := Limits{MaxPastes: 2, MaxBytes: 100}
	require.NoError(t, db.InsertPaste(ctx, NewPaste{Key: "aaaa", UserID: u.ID, Size: 60, Tags: []string{"x"}}, limits))
	require.ErrorIs(t, db.InsertPaste(ctx, NewPaste{Key: "bbbb", UserID: u.ID, Size: 41}, limits), ErrStorageLimit)
	require.NoError(t, db.InsertPaste(ctx, NewPaste{Key: "bbbb", UserID: u.ID, Size: 40}, limits))
	require.ErrorIs(t, db.InsertPaste(ctx, NewPaste{Key: "cccc", UserID: u.ID, Size: 0}, limits), ErrPasteLimit)
	count, bytes, err := db.Usage(ctx, u.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.EqualValues(t, 100, bytes)

	// Revisions count against the storage
	_, err = db.W.Exec("INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES ('aaaa', 1, 'aaaa.r1', 7, 0)")
	require.NoError(t, err)
	_, bytes, err = db.Usage(ctx, u.ID)
	require.NoError(t, err)
	require.EqualValues(t, 107, bytes)

	// The token or session of the upload must still exist when the row is written
	hash := sha256.Sum256([]byte("tok"))
	tok, err := db.CreateToken(ctx, u.ID, "t", "hbp_xxxxxx", hash[:], 0)
	require.NoError(t, err)
	require.NoError(t, db.InsertPaste(ctx, NewPaste{Key: "dddd", UserID: u.ID, Size: 1, TokenID: tok.ID}, Limits{}))
	require.NoError(t, db.DeleteToken(ctx, u.ID, tok.ID))
	require.ErrorIs(t, db.InsertPaste(ctx, NewPaste{Key: "eeee", UserID: u.ID, Size: 1, TokenID: tok.ID}, Limits{}), ErrRevoked)
	session := sha256.Sum256([]byte("session"))
	require.NoError(t, db.CreateSession(ctx, session[:], u.ID, make([]byte, 32), time.Now().Add(time.Hour)))
	require.NoError(t, db.InsertPaste(ctx, NewPaste{Key: "ffff", UserID: u.ID, Size: 1, SessionHash: session[:]}, Limits{}))
	require.NoError(t, db.DeleteSession(ctx, session[:]))
	require.ErrorIs(t, db.InsertPaste(ctx, NewPaste{Key: "gggg", UserID: u.ID, Size: 1, SessionHash: session[:]}, Limits{}), ErrRevoked)
	_, err = db.PasteByKey(ctx, u.ID, "gggg")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestInsertPasteLimitsUnderRace(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := newUser(t, db, 1, "alice")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := db.InsertPaste(ctx, NewPaste{Key: fmt.Sprintf("k%02d", i), UserID: u.ID, Size: 10}, Limits{MaxPastes: 5})
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrPasteLimit) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, 5, ok)
}

func TestListPastes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		db.now = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		p := NewPaste{Key: fmt.Sprintf("a%02d", i), UserID: alice.ID, Size: int64(i), Title: fmt.Sprintf("log %02d", i)}
		if i%3 == 0 {
			p.Visibility = VisibilityPrivate
		}
		if i%2 == 0 {
			p.Tags = []string{"even"}
		}
		if i%5 == 0 {
			p.Tags = append(p.Tags, "five")
		}
		addPaste(t, db, p)
	}
	db.now = time.Now
	addPaste(t, db, NewPaste{Key: "b00", UserID: bob.ID, Title: "log of bob", Tags: []string{"even"}})
	addPaste(t, db, NewPaste{Key: "a_%", UserID: alice.ID, Title: "100% done"})
	_, err := db.UpdatePaste(ctx, alice.ID, "a07", PastePatch{Language: ptr("go")})
	require.NoError(t, err)

	all := ListQuery{Visibility: -1, Page: 1, PerPage: 25, Sort: "-created"}
	list, total, err := db.ListPastes(ctx, alice.ID, all)
	require.NoError(t, err)
	require.EqualValues(t, 31, total)
	require.Len(t, list, 25)
	for _, p := range list {
		require.Equal(t, alice.ID, p.UserID)
	}

	// Page 2 has the rest, and a page past the end is empty
	page := all
	page.Page = 2
	list, _, err = db.ListPastes(ctx, alice.ID, page)
	require.NoError(t, err)
	require.Len(t, list, 6)
	page.Page = 9
	list, _, err = db.ListPastes(ctx, alice.ID, page)
	require.NoError(t, err)
	require.Empty(t, list)

	// Search: title, key and language; LIKE wildcards are literal
	q := all
	q.Q = "%"
	list, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, "a_%", list[0].Key)
	q.Q = "GO"
	list, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, "a07", list[0].Key)
	q.Q = "bob"
	_, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.Zero(t, total, "never another user's pastes")

	// Filters: visibility, language, tags (all of them)
	q = all
	q.Visibility = VisibilityPrivate
	_, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.EqualValues(t, 10, total)
	q = all
	q.Language = "go"
	_, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	q = all
	q.Tags = []string{"even"}
	_, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.EqualValues(t, 15, total)
	q.Tags = []string{"even", "five"}
	list, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	for _, p := range list {
		require.Equal(t, []string{"even", "five"}, p.Tags)
	}
	no := false
	q = all
	q.HasVersions = &no
	_, total, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.EqualValues(t, 31, total)

	// Sorts
	q = all
	q.Sort = "-size"
	list, _, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.Equal(t, "a29", list[0].Key)
	q.Sort = "created"
	list, _, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.Equal(t, "a00", list[0].Key)
	q.Sort = "title"
	list, _, err = db.ListPastes(ctx, alice.ID, q)
	require.NoError(t, err)
	require.Equal(t, "100% done", list[0].Title)

	tags, err := db.Tags(ctx, alice.ID, 200)
	require.NoError(t, err)
	require.Equal(t, []TagCount{{"even", 15}, {"five", 6}}, tags)
	langs, err := db.Languages(ctx, alice.ID, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"go"}, langs)

	var exported []string
	require.NoError(t, db.EachPaste(ctx, alice.ID, 7, func(p *Paste) error {
		exported = append(exported, p.Key)
		return nil
	}))
	require.Len(t, exported, 31)
	require.True(t, sort.StringsAreSorted(exported))
}

func ptr[T any](v T) *T { return &v }

func TestUpdatePaste(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")
	addPaste(t, db, NewPaste{Key: "akey", UserID: alice.ID, Title: "old", Tags: []string{"a", "b"}})

	expires := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	p, err := db.UpdatePaste(ctx, alice.ID, "akey", PastePatch{
		Title: ptr("new"), Visibility: ptr(VisibilityPrivate), ExpiresAt: &expires, Tags: &[]string{"c"},
	})
	require.NoError(t, err)
	require.Equal(t, "new", p.Title)
	require.Equal(t, VisibilityPrivate, p.Visibility)
	require.Equal(t, expires, p.ExpiresAt)
	require.Equal(t, []string{"c"}, p.Tags)

	// Clearing the expiry, an empty patch, and another user's attempt
	p, err = db.UpdatePaste(ctx, alice.ID, "akey", PastePatch{ExpiresAt: &time.Time{}})
	require.NoError(t, err)
	require.True(t, p.ExpiresAt.IsZero())
	_, err = db.UpdatePaste(ctx, alice.ID, "akey", PastePatch{})
	require.NoError(t, err)
	_, err = db.UpdatePaste(ctx, bob.ID, "akey", PastePatch{Title: ptr("stolen")})
	require.ErrorIs(t, err, ErrNotFound)
	p, err = db.PasteByKey(ctx, alice.ID, "akey")
	require.NoError(t, err)
	require.Equal(t, "new", p.Title)
	_, err = db.PasteByKey(ctx, bob.ID, "akey")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeletePastesQueuesContent(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")
	addPaste(t, db, NewPaste{Key: "a1", UserID: alice.ID, Tags: []string{"x"}})
	addPaste(t, db, NewPaste{Key: "a2", UserID: alice.ID})
	addPaste(t, db, NewPaste{Key: "b1", UserID: bob.ID})
	_, err := db.W.Exec("INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES ('a1', 1, 'a1.r1', 3, 0)")
	require.NoError(t, err)

	var pinned []string
	require.NoError(t, db.EachStoreKey(ctx, func(k string) { pinned = append(pinned, k) }))
	sort.Strings(pinned)
	require.Equal(t, []string{"a1", "a1.r1", "a2", "b1"}, pinned)

	deleted, notFound, err := db.DeletePastes(ctx, alice.ID, []string{"a1", "b1", "zz"})
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, deleted)
	require.Equal(t, []string{"b1", "zz"}, notFound)
	require.Equal(t, []string{"a1", "a1.r1"}, queued(t, db))
	_, err = db.PasteByKey(ctx, bob.ID, "b1")
	require.NoError(t, err, "bob's paste stays")
	var tags int
	require.NoError(t, db.R.QueryRow("SELECT COUNT(*) FROM paste_tags").Scan(&tags))
	require.Zero(t, tags)
}

func TestDrainerRetries(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	for i := 0; i < DrainBatch+5; i++ {
		addPaste(t, db, NewPaste{Key: fmt.Sprintf("k%03d", i), UserID: alice.ID})
	}
	keys := make([]string, 0, DrainBatch+5)
	for i := 0; i < DrainBatch+5; i++ {
		keys = append(keys, fmt.Sprintf("k%03d", i))
	}
	_, _, err := db.DeletePastes(ctx, alice.ID, keys[:100])
	require.NoError(t, err)
	_, _, err = db.DeletePastes(ctx, alice.ID, keys[100:])
	require.NoError(t, err)

	var mu sync.Mutex
	removed := map[string]bool{}
	failing := true
	d := NewDrainer(db, func(key string) error {
		mu.Lock()
		defer mu.Unlock()
		if failing && key == "k003" {
			return errors.New("store down")
		}
		removed[key] = true
		return nil
	})
	// More than one batch, one key failing: everything else goes, the failed one stays
	require.Equal(t, DrainBatch+4, d.Drain(ctx))
	require.Equal(t, []string{"k003"}, queued(t, db))
	var attempts int
	require.NoError(t, db.R.QueryRow("SELECT attempts FROM delete_queue WHERE store_key = 'k003'").Scan(&attempts))
	require.Equal(t, 1, attempts)

	// The next run retries it
	failing = false
	require.Equal(t, 1, d.Drain(ctx))
	require.Empty(t, queued(t, db))
	require.True(t, removed["k003"])
	n, err := db.QueueLen(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	// Kick never blocks, even on a nil drainer
	d.Kick()
	d.Kick()
	(*Drainer)(nil).Kick()
}

func TestDeleteUser(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")
	addPaste(t, db, NewPaste{Key: "a1", UserID: alice.ID, Tags: []string{"t"}})
	addPaste(t, db, NewPaste{Key: "a2", UserID: alice.ID})
	addPaste(t, db, NewPaste{Key: "b1", UserID: bob.ID})
	_, err := db.W.Exec("UPDATE pastes SET prev_key = 'a1', version = 2, root_key = 'a1' WHERE key = 'a2'")
	require.NoError(t, err)
	_, err = db.W.Exec("INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES ('a2', 1, 'a2.r1', 3, 0)")
	require.NoError(t, err)
	session := sha256.Sum256([]byte("s"))
	require.NoError(t, db.CreateSession(ctx, session[:], alice.ID, make([]byte, 32), time.Now().Add(time.Hour)))
	tokHash := sha256.Sum256([]byte("t"))
	_, err = db.CreateToken(ctx, alice.ID, "t", "hbp_aaaaaa", tokHash[:], 0)
	require.NoError(t, err)

	n, err := db.DeleteUser(ctx, alice)
	require.NoError(t, err)
	require.EqualValues(t, 3, n)
	require.Equal(t, []string{"a1", "a2", "a2.r1"}, queued(t, db))

	_, err = db.UserByID(ctx, alice.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = db.SessionByHash(ctx, session[:])
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = db.TokenByHash(ctx, tokHash[:])
	require.ErrorIs(t, err, ErrNotFound)
	for _, table := range []string{"paste_tags", "paste_revisions"} {
		var rows int
		require.NoError(t, db.R.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&rows))
		require.Zero(t, rows, table)
	}
	_, err = db.PasteByKey(ctx, bob.ID, "b1")
	require.NoError(t, err)

	entries, err := db.AuditLog(ctx, 10, 0)
	require.NoError(t, err)
	require.Equal(t, "account.delete", entries[0].Action)
	require.Equal(t, "alice", entries[0].ActorLogin)

	_, err = db.DeleteUser(ctx, alice)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDefaultVisibility(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	u := newUser(t, db, 1, "alice")
	require.NoError(t, db.SetDefaultVisibility(ctx, u.ID, VisibilityPrivate))
	u, err := db.UserByID(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, VisibilityPrivate, u.DefaultVisibility)
	require.Error(t, db.SetDefaultVisibility(ctx, u.ID, 5), "the schema refuses other values")
	require.ErrorIs(t, db.SetDefaultVisibility(ctx, 999, VisibilityLink), ErrNotFound)
}
