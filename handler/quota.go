package handler

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

const (
	// DefaultUploadQuota is how many bytes one client may store per DefaultQuotaWindow
	DefaultUploadQuota = 256 << 20

	// DefaultQuotaWindow is the period the upload quota counts over
	DefaultQuotaWindow = time.Hour

	// DefaultQuotaMaxClients caps how many clients the upload quota tracks
	DefaultQuotaMaxClients = 100000

	// quotaSweepInterval is how often finished windows are dropped
	quotaSweepInterval = time.Minute
)

var quotaRefused = promauto.NewCounter(prometheus.CounterOpts{
	Name: "hastebin_upload_quota_refused",
	Help: "The total number of uploads refused because the client used up its upload quota",
})

// QuotaError is returned when a client has stored its upload quota for the window
type QuotaError struct {
	// RetryAfter is how long until the window starts again
	RetryAfter time.Duration
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("upload quota used up, try again in %s", e.RetryAfter.Round(time.Second))
}

// UploadQuota caps how many bytes one client (an IPv4 address or an IPv6 /64)
// may store per window, so one client cannot fill the disk. Methods are safe
// on a nil quota, which never refuses.
type UploadQuota struct {
	limit      int64
	window     time.Duration
	maxClients int
	now        func() time.Time

	mu        sync.Mutex
	entries   map[string]*list.Element
	order     *list.List // least recently updated first
	lastSweep time.Time
}

type quotaEntry struct {
	client  string
	start   time.Time // start of the counting window
	used    int64
	refused bool // an upload was refused in this window
}

// blocked reports whether the client used up its quota, or had an upload
// refused, in a window that is not over at now
func (q *UploadQuota) blocked(e *quotaEntry, now time.Time) bool {
	return e.start.Add(q.window).After(now) && (e.used >= q.limit || e.refused)
}

// NewUploadQuota lets each client store limit bytes per window. limit <= 0 returns nil, which never refuses.
func NewUploadQuota(limit int64, window time.Duration) *UploadQuota {
	if limit <= 0 {
		return nil
	}
	return &UploadQuota{
		limit:      limit,
		window:     window,
		maxClients: DefaultQuotaMaxClients,
		now:        time.Now,
		entries:    make(map[string]*list.Element),
		order:      list.New(),
	}
}

// Exceeded reports whether the client at ip has no quota left, and how long until it has
func (q *UploadQuota) Exceeded(ip string) (time.Duration, bool) {
	if q == nil || ip == "" {
		return 0, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	elem, ok := q.entries[ClientKey(ip)]
	if !ok {
		return 0, false
	}
	e := elem.Value.(*quotaEntry)
	now := q.now()
	if left := e.start.Add(q.window).Sub(now); left > 0 && e.used >= q.limit {
		return left, true
	}
	return 0, false
}

// Charge counts n stored bytes against the client at ip. When they do not fit
// in what is left, nothing is counted and it reports false with how long until
// the window starts again.
func (q *UploadQuota) Charge(ip string, n int64) (time.Duration, bool) {
	if q == nil || ip == "" {
		return 0, true
	}
	client := ClientKey(ip)
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.now()
	q.sweepLocked(now)

	var e *quotaEntry
	if elem, ok := q.entries[client]; ok {
		e = elem.Value.(*quotaEntry)
		q.order.MoveToBack(elem)
	} else {
		// Full: forget the client updated longest ago
		for len(q.entries) >= q.maxClients {
			oldest := q.order.Front()
			q.order.Remove(oldest)
			delete(q.entries, oldest.Value.(*quotaEntry).client)
		}
		e = &quotaEntry{client: client, start: now}
		q.entries[client] = q.order.PushBack(e)
	}

	if now.Sub(e.start) >= q.window {
		e.start, e.used, e.refused = now, 0, false
	}
	if e.used+n > q.limit {
		e.refused = true
		quotaRefused.Inc()
		log.Warn().Str("client", client).Int64("used", e.used).Int64("size", n).Int64("quota", q.limit).
			Msg("Upload refused, the client used up its upload quota")
		return max(e.start.Add(q.window).Sub(now), time.Second), false
	}
	e.used += n
	return 0, true
}

// Refund gives back n bytes charged to the client at ip, for an upload that was not stored
func (q *UploadQuota) Refund(ip string, n int64) {
	if q == nil || ip == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if elem, ok := q.entries[ClientKey(ip)]; ok {
		e := elem.Value.(*quotaEntry)
		e.used = max(e.used-n, 0)
	}
}

// Tracked returns how many clients are tracked
func (q *UploadQuota) Tracked() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

// Snapshot returns up to limit clients that used up their quota, or had an
// upload refused, and wait for their window to end, the longest wait first
func (q *UploadQuota) Snapshot(limit int) []Ban {
	out := []Ban{}
	if q == nil || limit <= 0 {
		return out
	}
	q.mu.Lock()
	now := q.now()
	for _, elem := range q.entries {
		e := elem.Value.(*quotaEntry)
		if q.blocked(e, now) {
			out = append(out, Ban{Client: e.client, Until: e.start.Add(q.window).UTC(), Used: e.used})
		}
	}
	q.mu.Unlock()
	return firstBans(out, limit)
}

// Reset forgets what a client, in ClientKey form or the QuotaKey of an
// account, stored in its window, and reports whether it was blocked
func (q *UploadQuota) Reset(client string) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	elem, ok := q.entries[client]
	if !ok {
		return false
	}
	e := elem.Value.(*quotaEntry)
	blocked := q.blocked(e, q.now())
	q.order.Remove(elem)
	delete(q.entries, client)
	return blocked
}

// sweepLocked drops the entries whose window is over, at most once per interval
func (q *UploadQuota) sweepLocked(now time.Time) {
	if now.Sub(q.lastSweep) < quotaSweepInterval {
		return
	}
	q.lastSweep = now
	for client, elem := range q.entries {
		if now.Sub(elem.Value.(*quotaEntry).start) >= q.window {
			q.order.Remove(elem)
			delete(q.entries, client)
		}
	}
}
