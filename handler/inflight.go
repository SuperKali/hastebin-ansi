package handler

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DefaultMaxInflightPerClient is how many requests one client may have in
// progress at the same time, on all routes together
const DefaultMaxInflightPerClient = 16

var inflightRefused = promauto.NewCounter(prometheus.CounterOpts{
	Name: "hastebin_inflight_refused",
	Help: "The total number of requests refused because the client had too many requests in progress",
})

// InflightLimiter caps how many requests one client (an IPv4 address or an
// IPv6 /64) has in progress at the same time. Each request in progress holds
// memory the budget does not see: its goroutine, its buffers and, for a client
// that reads slowly, the socket buffers of the kernel. A nil limiter never refuses.
type InflightLimiter struct {
	max int

	mu       sync.Mutex
	byClient map[string]int
}

// NewInflightLimiter returns a limiter of max requests per client. max <= 0
// returns nil, which never refuses.
func NewInflightLimiter(max int) *InflightLimiter {
	if max <= 0 {
		return nil
	}
	return &InflightLimiter{max: max, byClient: make(map[string]int)}
}

// Acquire takes a place for the client at ip without waiting. When ok is true
// the caller must call release once the request is done; calling it more than
// once is safe. An unknown client ("") is never refused.
func (l *InflightLimiter) Acquire(ip string) (release func(), ok bool) {
	client := ClientKey(ip)
	if l == nil || client == "" {
		return func() {}, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byClient[client] >= l.max {
		inflightRefused.Inc()
		return nil, false
	}
	l.byClient[client]++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.byClient[client]--; l.byClient[client] <= 0 {
				delete(l.byClient, client)
			}
		})
	}, true
}

// InUse returns how many requests the client at ip has in progress
func (l *InflightLimiter) InUse(ip string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byClient[ClientKey(ip)]
}
