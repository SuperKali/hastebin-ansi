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
// window after it was taken. Methods are safe on a nil budget, which never
// refuses.
type PasteBudget struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	accounts  map[int64]*budgetEntry
	lastSweep time.Time
}

// budgetEntry holds the times of the last pastes of one account, a ring of
// at most limit
type budgetEntry struct {
	times []time.Time
	next  int // the oldest time once the ring is full
	last  time.Time
}

// NewPasteBudget lets each account create limit new owned pastes per
// window. limit <= 0 returns nil, which never refuses.
func NewPasteBudget(limit int, window time.Duration) *PasteBudget {
	if limit <= 0 {
		return nil
	}
	return &PasteBudget{limit: limit, window: window, now: time.Now, accounts: make(map[int64]*budgetEntry)}
}

// Take counts one new paste of the account userID. When the account
// created its limit within the window, nothing is counted and it reports
// false with how long until it may create the next one.
func (b *PasteBudget) Take(userID int64) (time.Duration, bool) {
	if b == nil {
		return 0, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.sweepLocked(now)

	e := b.accounts[userID]
	if e == nil {
		e = &budgetEntry{times: make([]time.Time, 0, b.limit)}
		b.accounts[userID] = e
	}
	if len(e.times) < b.limit {
		e.times = append(e.times, now)
	} else {
		oldest := e.times[e.next]
		if left := oldest.Add(b.window).Sub(now); left > 0 {
			return max(left, time.Second), false
		}
		e.times[e.next] = now
		e.next = (e.next + 1) % b.limit
	}
	e.last = now
	return 0, true
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
