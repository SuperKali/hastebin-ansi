package handler

import (
	"sync"
	"time"
)

const (
	// DefaultOwnedPasteLimit is how many new owned pastes one account may
	// create per OwnedPasteWindow, by every path together
	DefaultOwnedPasteLimit = 120

	// OwnedPasteWindow is the period PasteBudget counts over
	OwnedPasteWindow = time.Minute
)

// PasteBudget caps how many new owned pastes each account creates per
// window, whatever creates them: an upload to /documents or /log, MCP
// upload_log, a duplicate, a new version or an edit that keeps a revision.
// Each path keeps its own request limit too; this one is shared, so no mix
// of paths goes past it. The window slides: a paste counts for exactly one
// window after it was taken. A path takes its place before it stores
// anything, so parallel requests never get more than the limit, and gives
// it back with the refund Take returns when nothing ends up stored. Methods
// are safe on a nil budget, which never refuses.
type PasteBudget struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	accounts  map[int64]*budgetEntry
	lastSweep time.Time
	nextID    uint64
}

// budgetEntry holds the places taken by one account, oldest first, at most limit
type budgetEntry struct {
	taken []budgetPlace
	last  time.Time
}

// budgetPlace is one paste counted at at; id tells it apart for a refund
type budgetPlace struct {
	at time.Time
	id uint64
}

// NewPasteBudget lets each account create limit new owned pastes per
// window. limit <= 0 returns nil, which never refuses.
func NewPasteBudget(limit int, window time.Duration) *PasteBudget {
	if limit <= 0 {
		return nil
	}
	return &PasteBudget{limit: limit, window: window, now: time.Now, accounts: make(map[int64]*budgetEntry)}
}

// noRefund is the refund of a place that was not taken
func noRefund() {}

// Take counts one new paste of the account userID and returns the refund
// that gives the place back, for a paste that ends up not stored. Calling
// it more than once, or after the place left the window, does nothing.
// When the account created its limit within the window, nothing is counted
// and it reports false with how long until it may create the next one.
func (b *PasteBudget) Take(userID int64) (refund func(), left time.Duration, ok bool) {
	if b == nil {
		return noRefund, 0, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.sweepLocked(now)

	e := b.accounts[userID]
	if e == nil {
		e = &budgetEntry{taken: make([]budgetPlace, 0, b.limit)}
		b.accounts[userID] = e
	}
	// Places a window old are free again
	n := 0
	for n < len(e.taken) && !e.taken[n].at.Add(b.window).After(now) {
		n++
	}
	if n > 0 {
		e.taken = append(e.taken[:0], e.taken[n:]...)
	}
	if len(e.taken) >= b.limit {
		return noRefund, max(e.taken[0].at.Add(b.window).Sub(now), time.Second), false
	}
	b.nextID++
	id := b.nextID
	e.taken = append(e.taken, budgetPlace{at: now, id: id})
	e.last = now
	return func() { b.refund(userID, id) }, 0, true
}

// refund gives back the place id of the account userID, if it still holds it
func (b *PasteBudget) refund(userID int64, id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.accounts[userID]
	if e == nil {
		return
	}
	for i, place := range e.taken {
		if place.id == id {
			e.taken = append(e.taken[:i], e.taken[i+1:]...)
			return
		}
	}
}

// sweepLocked forgets the accounts that created nothing for a window, at
// most once per window
func (b *PasteBudget) sweepLocked(now time.Time) {
	if now.Sub(b.lastSweep) < b.window {
		return
	}
	b.lastSweep = now
	for id, e := range b.accounts {
		if now.Sub(e.last) >= b.window {
			delete(b.accounts, id)
		}
	}
}
