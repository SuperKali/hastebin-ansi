// Package abuse keeps what the admin page shows about misbehaving clients:
// a bounded ring of recent refusals, and the manual bans an admin set.
// Nothing here is written to disk except through the accounts database.
package abuse

import (
	"sync"
	"time"
)

// Kinds of events, one per way the server refuses a client
const (
	// KindNotFoundBan is a client banned, or refused while banned, for
	// looking up too many unknown keys
	KindNotFoundBan = "not_found_ban"

	// KindQuota is an upload refused because the client or user used up its upload quota
	KindQuota = "quota"

	// KindNoSpace is an upload refused because the disk is nearly full (507)
	KindNoSpace = "no_space"

	// KindInflight is a request refused because the client had too many in progress
	KindInflight = "inflight"

	// KindBusy is a request refused because the memory budget or the upload slots were full (503)
	KindBusy = "busy"

	// KindRateLimit is a request refused by a rate limit (429)
	KindRateLimit = "rate_limit"
)

const (
	// MaxEvents is how many events the ring keeps; the oldest go first
	MaxEvents = 500

	// eventEvery is the least time between two events of one kind from one client
	eventEvery = time.Minute

	// maxRecent caps how many kind and client pairs are remembered for eventEvery
	maxRecent = 10000
)

// Event is one refusal of a client
type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Client string    `json:"client"`
	Detail string    `json:"detail,omitempty"`
}

// Events is a ring of the last MaxEvents refusals. Each kind is recorded at
// most once a minute per client, so a client hammering the server cannot
// push everyone else out. Methods are safe on a nil Events, which records nothing.
type Events struct {
	mu     sync.Mutex
	ring   []Event // MaxEvents long once full
	next   int     // where the next event goes once the ring is full
	recent map[string]time.Time
	now    func() time.Time
}

// NewEvents returns an empty ring
func NewEvents() *Events {
	return &Events{recent: map[string]time.Time{}, now: time.Now}
}

// Record adds a refusal of client, in handler.ClientKey form, unless the
// same kind was recorded for it within the last minute. "" clients are
// recorded as unknown.
func (e *Events) Record(kind, client, detail string) {
	if e == nil {
		return
	}
	if client == "" {
		client = "unknown"
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	id := kind + " " + client
	if last, ok := e.recent[id]; ok && now.Sub(last) < eventEvery {
		return
	}
	if len(e.recent) >= maxRecent {
		for k, at := range e.recent {
			if now.Sub(at) >= eventEvery {
				delete(e.recent, k)
			}
		}
		if len(e.recent) >= maxRecent {
			// Every pair is recent: a flood from many clients. Keep the ring
			// going and forget who was seen, rather than grow.
			clear(e.recent)
		}
	}
	e.recent[id] = now

	ev := Event{At: now.UTC(), Kind: kind, Client: client, Detail: detail}
	if len(e.ring) < MaxEvents {
		e.ring = append(e.ring, ev)
		return
	}
	e.ring[e.next] = ev
	e.next = (e.next + 1) % MaxEvents
}

// Page returns up to perPage events, newest first, skipping (page-1)*perPage,
// and how many there are in all
func (e *Events) Page(page, perPage int) ([]Event, int) {
	out := []Event{}
	if e == nil {
		return out, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	total := len(e.ring)
	start := (max(page, 1) - 1) * max(perPage, 1)
	for i := start; i < total && len(out) < perPage; i++ {
		// The newest is just before next once full, at the end before that
		idx := (e.next - 1 - i + 2*total) % total
		if total < MaxEvents {
			idx = total - 1 - i
		}
		out = append(out, e.ring[idx])
	}
	return out, total
}

// Len returns how many events the ring holds
func (e *Events) Len() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.ring)
}
