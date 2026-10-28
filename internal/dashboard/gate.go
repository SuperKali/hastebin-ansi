package dashboard

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/rs/zerolog/log"
)

// KeyLocks hands out one lock per paste key. A reader that ends the edit
// window and the editor that swaps the content hold the same lock, so no
// one other than the owner ever reads two contents under one link.
type KeyLocks struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	refs int
}

// Lock takes the lock of key and returns the function that releases it
func (l *KeyLocks) Lock(key string) (unlock func()) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*keyLock{}
	}
	k := l.locks[key]
	if k == nil {
		k = &keyLock{}
		l.locks[key] = k
	}
	k.refs++
	l.mu.Unlock()

	k.Lock()
	return func() {
		k.Unlock()
		l.mu.Lock()
		if k.refs--; k.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

// held returns how many keys have a lock taken or waited for
func (l *KeyLocks) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}

// Gate is the handler.AccessGate of registered pastes: a private paste only
// for its owner, an expired one for no one, a removed one as missing (410
// when it was public). A paste without a row is anonymous and public.
type Gate struct {
	DB    *accounts.DB
	Views *accounts.Views
	Locks *KeyLocks

	// EditWindow is how long after creation the owner may edit a paste
	// under the same link, until someone else opens it. 0 means never.
	EditWindow time.Duration

	// now is the clock, replaced in tests
	now func() time.Time

	// seenWrites counts the seen_at writes, for tests
	seenWrites atomic.Int64
}

// NewGate returns the access gate of db
func NewGate(db *accounts.DB, views *accounts.Views, locks *KeyLocks, editWindow time.Duration) *Gate {
	return &Gate{DB: db, Views: views, Locks: locks, EditWindow: editWindow, now: time.Now}
}

// Check decides whether p, nil for anonymous, may read key, see
// handler.AccessGate. The first content read by someone other than the
// owner inside the edit window ends it: seen_at is committed under the key
// lock before the content opens.
func (g *Gate) Check(ctx context.Context, key string, p *auth.Principal, op handler.Op) (*handler.Meta, error) {
	a, err := g.DB.AccessOf(ctx, key)
	if err != nil {
		return nil, err
	}
	if a.Tombstoned {
		if a.TombstoneVisibility == accounts.VisibilityPrivate {
			return nil, handler.ErrDenied
		}
		return nil, handler.ErrRemoved
	}
	if !a.Registered {
		return &handler.Meta{}, nil
	}

	now := g.now()
	if !a.ExpiresAt.IsZero() && !now.Before(a.ExpiresAt) {
		return nil, handler.ErrDenied
	}
	owner := p != nil && p.User != nil && p.User.ID == a.UserID
	if a.Visibility == accounts.VisibilityPrivate && !owner {
		return nil, handler.ErrDenied
	}

	meta := &handler.Meta{Owner: owner, ServerNow: now}
	if owner {
		meta.EditableUntil = g.editableUntil(a, now)
		return meta, nil
	}
	if op != handler.OpRead {
		return meta, nil
	}
	if err := g.markSeen(ctx, key, a, now); err != nil {
		return nil, err
	}
	views := g.Views
	meta.OnDeliver = func() { views.Add(key, g.now()) }
	return meta, nil
}

// editableUntil is the end of the edit window of a paste, the zero time
// when it is over or never opened
func (g *Gate) editableUntil(a *accounts.Access, now time.Time) time.Time {
	if g.EditWindow <= 0 || !a.SeenAt.IsZero() {
		return time.Time{}
	}
	until := a.CreatedAt.Add(g.EditWindow)
	if !now.Before(until) {
		return time.Time{}
	}
	return until
}

// markSeen ends the edit window of a paste on its first read by someone
// other than the owner: under the key lock, seen_at is checked again and
// written. Past the window nothing is locked or written.
func (g *Gate) markSeen(ctx context.Context, key string, a *accounts.Access, now time.Time) error {
	if g.editableUntil(a, now).IsZero() {
		return nil
	}
	unlock := g.Locks.Lock(key)
	defer unlock()
	seen, err := g.DB.SeenAt(ctx, key)
	if errors.Is(err, accounts.ErrNotFound) {
		// Deleted meanwhile: the store read answers for it
		return nil
	}
	if err != nil || !seen.IsZero() {
		return err
	}
	g.seenWrites.Add(1)
	if _, err := g.DB.MarkSeen(ctx, key, now); err != nil {
		return err
	}
	return nil
}

// Readers tells who reads a paste, for handler.DocumentHandler.Readers: a
// bearer token first, then the session cookie. A read changes nothing, so
// the cookie needs no CSRF token. A bad token is 401, never anonymous; a
// disabled account reads as anonymous with its cookie and gets 403 with a
// token, as uploads do. When the database cannot tell, the read fails closed.
func (d *Handler) Readers(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	var p *auth.Principal
	var err error
	if auth.HasBearer(r) {
		p, err = d.Auth.Bearer(r)
	} else {
		p, err = d.Auth.Session(r)
		if errors.Is(err, auth.ErrDisabled) {
			p, err = nil, nil
		}
	}
	switch {
	case err == nil:
		return p, true
	case errors.Is(err, auth.ErrInvalidToken):
		auth.WriteInvalidToken(w, false)
	case errors.Is(err, auth.ErrDisabled):
		writeMessage(w, http.StatusForbidden, "This account is disabled.")
	case accounts.IsBusy(err):
		writeBusy(w)
	default:
		log.Error().Err(err).Msg("Failed to resolve the reader of a paste")
		writeMessage(w, http.StatusInternalServerError, "Error retrieving document.")
	}
	return nil, false
}
