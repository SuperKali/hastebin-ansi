package accounts

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccessOf(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	addPaste(t, db, NewPaste{Key: "priv", UserID: alice.ID, Visibility: VisibilityPrivate, ExpiresAt: expires, Size: 3})
	_, err := db.W.Exec("INSERT INTO tombstones (key, reason, visibility, removed_at) VALUES ('gone', 'spam', 0, 1), ('hidden', 'spam', 1, 1)")
	require.NoError(t, err)

	a, err := db.AccessOf(ctx, "priv")
	require.NoError(t, err)
	require.True(t, a.Registered)
	require.False(t, a.Tombstoned)
	require.Equal(t, alice.ID, a.UserID)
	require.Equal(t, VisibilityPrivate, a.Visibility)
	require.Equal(t, expires, a.ExpiresAt)
	require.True(t, a.SeenAt.IsZero())
	require.False(t, a.CreatedAt.IsZero())

	a, err = db.AccessOf(ctx, "gone")
	require.NoError(t, err)
	require.Equal(t, &Access{Tombstoned: true, TombstoneVisibility: VisibilityLink}, a)
	a, err = db.AccessOf(ctx, "hidden")
	require.NoError(t, err)
	require.Equal(t, &Access{Tombstoned: true, TombstoneVisibility: VisibilityPrivate}, a)

	// An anonymous paste, or none: nothing known
	a, err = db.AccessOf(ctx, "anon")
	require.NoError(t, err)
	require.Equal(t, &Access{}, a)
}

func TestMarkSeen(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	addPaste(t, db, NewPaste{Key: "k", UserID: alice.ID, Size: 1})

	seen, err := db.SeenAt(ctx, "k")
	require.NoError(t, err)
	require.True(t, seen.IsZero())
	at := time.Unix(1_800_000_000, 0).UTC()
	wrote, err := db.MarkSeen(ctx, "k", at)
	require.NoError(t, err)
	require.True(t, wrote)
	wrote, err = db.MarkSeen(ctx, "k", at.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, wrote, "the first read stays")
	seen, err = db.SeenAt(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, at, seen)

	_, err = db.SeenAt(ctx, "none")
	require.ErrorIs(t, err, ErrNotFound)
	wrote, err = db.MarkSeen(ctx, "none", at)
	require.NoError(t, err)
	require.False(t, wrote)
}

func TestViewsFlush(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	addPaste(t, db, NewPaste{Key: "a", UserID: alice.ID, Size: 1})
	addPaste(t, db, NewPaste{Key: "b", UserID: alice.ID, Size: 1})

	views := NewViews(db)
	t1 := time.Unix(1_800_000_000, 0).UTC()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views.Add("a", t1.Add(time.Duration(i)*time.Second))
		}()
	}
	wg.Wait()
	views.Add("b", t1)
	views.Add("deleted", t1)
	require.Equal(t, 3, views.Pending())

	// Nothing is written until the flush
	p, err := db.PasteByKey(ctx, alice.ID, "a")
	require.NoError(t, err)
	require.Zero(t, p.Views)

	require.NoError(t, views.Flush(ctx))
	require.Zero(t, views.Pending())
	p, err = db.PasteByKey(ctx, alice.ID, "a")
	require.NoError(t, err)
	require.EqualValues(t, 50, p.Views)
	require.Equal(t, t1.Add(49*time.Second), p.LastOpenedAt)

	// last_opened_at never goes back
	views.Add("a", t1)
	require.NoError(t, views.Flush(ctx))
	p, err = db.PasteByKey(ctx, alice.ID, "a")
	require.NoError(t, err)
	require.EqualValues(t, 51, p.Views)
	require.Equal(t, t1.Add(49*time.Second), p.LastOpenedAt)

	// A failed flush keeps the counts for the next one
	views.Add("b", t1.Add(time.Minute))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, views.Flush(cancelled))
	views.Add("b", t1.Add(2*time.Minute))
	require.Equal(t, 1, views.Pending())
	require.NoError(t, views.Flush(ctx))
	p, err = db.PasteByKey(ctx, alice.ID, "b")
	require.NoError(t, err)
	require.EqualValues(t, 3, p.Views)
	require.Equal(t, t1.Add(2*time.Minute), p.LastOpenedAt)

	// A nil counter does nothing
	var none *Views
	none.Add("a", t1)
	require.NoError(t, none.Flush(ctx))
}

func TestSweeperDeletesExpired(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	now := time.Now()
	db.now = func() time.Time { return now }
	for i, key := range []string{"old1", "old2", "fresh", "never"} {
		p := NewPaste{Key: key, UserID: alice.ID, Size: 1}
		switch key {
		case "old1", "old2":
			p.ExpiresAt = now.Add(time.Duration(-i-1) * time.Minute)
		case "fresh":
			p.ExpiresAt = now.Add(time.Hour)
		}
		addPaste(t, db, p)
	}
	// A revision of an expired paste goes with it
	_, err := db.W.Exec("INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES ('old1', 1, 'old1.r1', 1, 1)")
	require.NoError(t, err)

	var mu sync.Mutex
	deleted := map[string]bool{}
	drainer := NewDrainer(db, func(key string) error {
		mu.Lock()
		defer mu.Unlock()
		deleted[key] = true
		return nil
	})
	sweeper := NewSweeper(db, drainer)
	n, err := sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, []string{"old1", "old1.r1", "old2"}, queued(t, db))
	for _, key := range []string{"old1", "old2"} {
		_, err := db.PasteByKey(ctx, alice.ID, key)
		require.ErrorIs(t, err, ErrNotFound)
	}
	for _, key := range []string{"fresh", "never"} {
		_, err := db.PasteByKey(ctx, alice.ID, key)
		require.NoError(t, err)
	}

	require.Equal(t, 3, drainer.Drain(ctx))
	require.Equal(t, map[string]bool{"old1": true, "old1.r1": true, "old2": true}, deleted)
	require.Empty(t, queued(t, db))

	// Nothing left: the next sweep deletes nothing
	n, err = sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestSweeperBatches(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	past := time.Now().Add(-time.Minute)
	for i := range SweepBatch*2 + 5 {
		addPaste(t, db, NewPaste{Key: "k" + string(rune('a'+i/26)) + string(rune('a'+i%26)), UserID: alice.ID, Size: 1, ExpiresAt: past})
	}
	n, err := NewSweeper(db, nil).Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, SweepBatch*2+5, n)
	count, _, err := db.Usage(ctx, alice.ID)
	require.NoError(t, err)
	require.Zero(t, count)

	// A cancelled sweep stops
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = NewSweeper(db, nil).Sweep(cancelled)
	require.True(t, errors.Is(err, context.Canceled))
}
