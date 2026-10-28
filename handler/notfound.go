package handler

import (
	"container/list"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

const (
	// DefaultNotFoundMaxIPs caps how many client IPs the not-found limiter tracks
	DefaultNotFoundMaxIPs = 100000

	// notFoundSweepInterval is how often expired entries are dropped
	notFoundSweepInterval = time.Minute
)

var notFoundBans = promauto.NewCounter(prometheus.CounterOpts{
	Name: "hastebin_not_found_bans",
	Help: "The total number of client IPs banned for looking up too many unknown keys",
})

// NotFoundLimiter bans clients that look up too many keys that do not exist,
// which is how someone guesses keys. A client is an IPv4 address or an IPv6 /64
// (see ClientKey). A banned client can still upload; only its document lookups
// are refused. Methods are safe on a nil limiter, which never bans.
type NotFoundLimiter struct {
	limit  int
	window time.Duration
	ban    time.Duration
	maxIPs int
	now    func() time.Time

	mu        sync.Mutex
	entries   map[string]*list.Element
	order     *list.List // least recently updated first
	lastSweep time.Time
}

type notFoundEntry struct {
	ip          string
	start       time.Time // start of the counting window
	count       int
	bannedUntil time.Time
}

// NewNotFoundLimiter bans an IP for ban once it looks up more than limit
// unknown keys within window. limit <= 0 returns nil, which never bans.
func NewNotFoundLimiter(limit int, window, ban time.Duration) *NotFoundLimiter {
	if limit <= 0 {
		return nil
	}
	return &NotFoundLimiter{
		limit:   limit,
		window:  window,
		ban:     ban,
		maxIPs:  DefaultNotFoundMaxIPs,
		now:     time.Now,
		entries: make(map[string]*list.Element),
		order:   list.New(),
	}
}

// Banned reports whether ip is banned and how long the ban still lasts
func (l *NotFoundLimiter) Banned(ip string) (time.Duration, bool) {
	if l == nil || ip == "" {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	elem, ok := l.entries[ClientKey(ip)]
	if !ok {
		return 0, false
	}
	now := l.now()
	if left := elem.Value.(*notFoundEntry).bannedUntil.Sub(now); left > 0 {
		return left, true
	}
	return 0, false
}

// Miss records a lookup of an unknown key by ip. It reports whether ip is now
// banned, either by this lookup or before it, and how long the ban lasts.
// Lookups that find a document do not reset the count.
func (l *NotFoundLimiter) Miss(ip string) (time.Duration, bool) {
	if l == nil || ip == "" {
		return 0, false
	}
	ip = ClientKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	var e *notFoundEntry
	if elem, ok := l.entries[ip]; ok {
		e = elem.Value.(*notFoundEntry)
		l.order.MoveToBack(elem)
	} else {
		// Full: forget the IP updated longest ago
		for len(l.entries) >= l.maxIPs {
			oldest := l.order.Front()
			l.order.Remove(oldest)
			delete(l.entries, oldest.Value.(*notFoundEntry).ip)
		}
		e = &notFoundEntry{ip: ip, start: now}
		l.entries[ip] = l.order.PushBack(e)
	}

	if left := e.bannedUntil.Sub(now); left > 0 {
		return left, true
	}
	if now.Sub(e.start) > l.window {
		e.start, e.count = now, 0
	}
	e.count++
	if e.count <= l.limit {
		return 0, false
	}

	e.bannedUntil = now.Add(l.ban)
	notFoundBans.Inc()
	log.Warn().Str("ip", ip).Int("count", e.count).Dur("ban", l.ban).
		Msg("Client banned for looking up too many unknown keys")
	e.start, e.count = now, 0
	return l.ban, true
}

// Tracked returns how many client IPs are tracked
func (l *NotFoundLimiter) Tracked() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// sweepLocked drops the entries whose window and ban are over, at most once per interval
func (l *NotFoundLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < notFoundSweepInterval {
		return
	}
	l.lastSweep = now
	for ip, elem := range l.entries {
		e := elem.Value.(*notFoundEntry)
		if !now.Before(e.bannedUntil) && now.Sub(e.start) > l.window {
			l.order.Remove(elem)
			delete(l.entries, ip)
		}
	}
}

// RetryAfterSeconds rounds a ban duration up to whole seconds, at least 1
func RetryAfterSeconds(d time.Duration) int {
	return max(int(math.Ceil(d.Seconds())), 1)
}

// writeBanned answers 429 to a client banned for key guessing
func writeBanned(w http.ResponseWriter, left time.Duration) {
	w.Header().Set("Retry-After", fmt.Sprint(RetryAfterSeconds(left)))
	http.Error(w, `{"message": "Too many lookups of unknown keys, try again later."}`, http.StatusTooManyRequests)
}
