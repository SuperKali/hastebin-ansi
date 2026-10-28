package dashboard

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/stretchr/testify/require"
)

// gateTest is a gate on a fresh database with alice's pastes
type gateTest struct {
	db    *accounts.DB
	gate  *Gate
	alice *auth.Principal
	bob   *auth.Principal
}

func newGateTest(t *testing.T) *gateTest {
	t.Helper()
	db, err := accounts.Open(filepath.Join(t.TempDir(), "hastebin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	alice, _, err := db.LoginUser(ctx, 1, "alice", true)
	require.NoError(t, err)
	bob, _, err := db.LoginUser(ctx, 2, "bob", true)
	require.NoError(t, err)
	return &gateTest{
		db:    db,
		gate:  NewGate(db, accounts.NewViews(db), &KeyLocks{}, 10*time.Minute),
		alice: &auth.Principal{User: alice},
		bob:   &auth.Principal{User: bob},
	}
}

func (g *gateTest) add(t *testing.T, key string, visibility int, expires time.Time) {
	t.Helper()
	require.NoError(t, g.db.InsertPaste(context.Background(), accounts.NewPaste{
		Key: key, UserID: g.alice.User.ID, Visibility: visibility, ExpiresAt: expires, Size: 1,
	}, accounts.Limits{}))
}

func (g *gateTest) seen(t *testing.T, key string) time.Time {
	t.Helper()
	seen, err := g.db.SeenAt(context.Background(), key)
	require.NoError(t, err)
	return seen
}

func TestGateRules(t *testing.T) {
	ctx := context.Background()
	g := newGateTest(t)
	g.add(t, "pub", accounts.VisibilityLink, time.Time{})
	g.add(t, "priv", accounts.VisibilityPrivate, time.Time{})
	g.add(t, "soon", accounts.VisibilityPrivate, time.Now().Add(time.Hour))
	_, err := g.db.W.Exec("INSERT INTO tombstones (key, reason, visibility, removed_at) VALUES ('gone', 'spam', 0, 1), ('hidden', 'spam', 1, 1)")
	require.NoError(t, err)

	tests := []struct {
		key    string
		reader *auth.Principal
		err    error
		owner  bool
	}{
		{"anon", nil, nil, false},
		{"anon", g.bob, nil, false},
		{"pub", nil, nil, false},
		{"pub", g.bob, nil, false},
		{"pub", g.alice, nil, true},
		{"priv", nil, handler.ErrDenied, false},
		{"priv", g.bob, handler.ErrDenied, false},
		{"priv", g.alice, nil, true},
		{"soon", g.alice, nil, true},
		{"soon", nil, handler.ErrDenied, false},
		{"gone", nil, handler.ErrRemoved, false},
		{"gone", g.alice, handler.ErrRemoved, false},
		{"hidden", nil, handler.ErrDenied, false},
		{"hidden", g.alice, handler.ErrDenied, false},
	}
	for _, op := range []handler.Op{handler.OpRead, handler.OpHead} {
		for _, tt := range tests {
			meta, err := g.gate.Check(ctx, tt.key, tt.reader, op)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err, tt.key)
				require.Nil(t, meta)
				continue
			}
			require.NoError(t, err, tt.key)
			require.Equal(t, tt.owner, meta.Owner, tt.key)
		}
	}

	// Past its expiry a paste is gone for everyone, its owner too
	now := time.Now()
	g.gate.now = func() time.Time { return now.Add(2 * time.Hour) }
	for _, reader := range []*auth.Principal{nil, g.bob, g.alice} {
		_, err := g.gate.Check(ctx, "soon", reader, handler.OpRead)
		require.ErrorIs(t, err, handler.ErrDenied)
		_, err = g.gate.Check(ctx, "soon", reader, handler.OpHead)
		require.ErrorIs(t, err, handler.ErrDenied)
	}

	// A closed database fails closed
	g.db.Close()
	_, err = g.gate.Check(ctx, "pub", nil, handler.OpRead)
	require.Error(t, err)
	require.NotErrorIs(t, err, handler.ErrDenied)
}

func TestGateEditWindow(t *testing.T) {
	ctx := context.Background()
	g := newGateTest(t)
	g.add(t, "k", accounts.VisibilityLink, time.Time{})
	created, err := g.db.PasteByKey(ctx, g.alice.User.ID, "k")
	require.NoError(t, err)
	now := created.CreatedAt.Add(time.Minute)
	g.gate.now = func() time.Time { return now }

	// The owner sees how long the window lasts, and never closes it
	meta, err := g.gate.Check(ctx, "k", g.alice, handler.OpRead)
	require.NoError(t, err)
	require.Equal(t, created.CreatedAt.Add(10*time.Minute), meta.EditableUntil)
	require.Equal(t, now, meta.ServerNow)
	meta.Delivered()
	require.True(t, g.seen(t, "k").IsZero())

	// HEAD by someone else does not close it either
	meta, err = g.gate.Check(ctx, "k", nil, handler.OpHead)
	require.NoError(t, err)
	require.False(t, meta.Owner)
	require.True(t, meta.EditableUntil.IsZero(), "only the owner learns about the window")
	meta.Delivered()
	require.True(t, g.seen(t, "k").IsZero())
	require.Zero(t, g.gate.Views.Pending())

	// The first content read by someone else closes it before the content opens
	meta, err = g.gate.Check(ctx, "k", g.bob, handler.OpRead)
	require.NoError(t, err)
	require.Equal(t, now.Truncate(time.Second), g.seen(t, "k"))
	require.Zero(t, g.gate.Views.Pending(), "a view counts once the content goes out")
	meta.Delivered()
	require.Equal(t, 1, g.gate.Views.Pending())

	meta, err = g.gate.Check(ctx, "k", g.alice, handler.OpRead)
	require.NoError(t, err)
	require.True(t, meta.EditableUntil.IsZero())

	// Past the window nothing is locked or written
	g.add(t, "late", accounts.VisibilityLink, time.Time{})
	g.gate.now = func() time.Time { return now.Add(time.Hour) }
	writes := g.gate.seenWrites.Load()
	_, err = g.gate.Check(ctx, "late", nil, handler.OpRead)
	require.NoError(t, err)
	require.Equal(t, writes, g.gate.seenWrites.Load())
	require.True(t, g.seen(t, "late").IsZero())

	// EDIT_WINDOW 0: never
	g.add(t, "off", accounts.VisibilityLink, time.Time{})
	g.gate.now = time.Now
	g.gate.EditWindow = 0
	meta, err = g.gate.Check(ctx, "off", nil, handler.OpRead)
	require.NoError(t, err)
	require.True(t, g.seen(t, "off").IsZero())
	meta, err = g.gate.Check(ctx, "off", g.alice, handler.OpRead)
	require.NoError(t, err)
	require.True(t, meta.EditableUntil.IsZero())
}

func TestGateSeenRace(t *testing.T) {
	// 100 readers at once inside the window: exactly one writes seen_at
	ctx := context.Background()
	g := newGateTest(t)
	g.add(t, "k", accounts.VisibilityLink, time.Time{})

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 100)
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reader := g.bob
			if i%2 == 0 {
				reader = nil
			}
			meta, err := g.gate.Check(ctx, "k", reader, handler.OpRead)
			if err == nil {
				meta.Delivered()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, g.gate.seenWrites.Load())
	require.False(t, g.seen(t, "k").IsZero())
	require.Zero(t, g.gate.Locks.held(), "every lock is released")

	require.NoError(t, g.gate.Views.Flush(ctx))
	p, err := g.db.PasteByKey(ctx, g.alice.User.ID, "k")
	require.NoError(t, err)
	require.EqualValues(t, 100, p.Views)
}

func TestKeyLocks(t *testing.T) {
	var locks KeyLocks
	unlockA := locks.Lock("a")
	unlockB := locks.Lock("b") // another key does not wait
	got := make(chan struct{})
	go func() {
		unlock := locks.Lock("a")
		close(got)
		unlock()
	}()
	select {
	case <-got:
		t.Fatal("the same key was locked twice")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	<-got
	unlockB()
	require.Zero(t, locks.held())
}
