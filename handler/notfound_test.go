package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// fakeClock is a settable clock for the limiter
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

// testLimiter bans after limit misses in 15 minutes, for an hour, on the given clock
func testLimiter(limit int, c *fakeClock) *NotFoundLimiter {
	l := NewNotFoundLimiter(limit, 15*time.Minute, time.Hour)
	l.now = c.now
	return l
}

func TestNotFoundLimiter(t *testing.T) {
	clock := newFakeClock()
	l := testLimiter(3, clock)

	// The limit is allowed, one more bans
	for i := 0; i < 3; i++ {
		_, banned := l.Miss("1.1.1.1")
		require.False(t, banned, i)
	}
	left, banned := l.Miss("1.1.1.1")
	require.True(t, banned)
	require.Equal(t, time.Hour, left)

	left, banned = l.Banned("1.1.1.1")
	require.True(t, banned)
	require.Equal(t, time.Hour, left)

	// Other IPs are not affected
	_, banned = l.Banned("2.2.2.2")
	require.False(t, banned)
	_, banned = l.Miss("2.2.2.2")
	require.False(t, banned)

	// The ban ends, and the count starts again
	clock.add(59 * time.Minute)
	left, banned = l.Banned("1.1.1.1")
	require.True(t, banned)
	require.Equal(t, time.Minute, left)
	clock.add(time.Minute)
	_, banned = l.Banned("1.1.1.1")
	require.False(t, banned)
	_, banned = l.Miss("1.1.1.1")
	require.False(t, banned)
}

func TestNotFoundLimiterWindow(t *testing.T) {
	clock := newFakeClock()
	l := testLimiter(3, clock)

	// Misses spread over more than the window never add up to a ban
	for i := 0; i < 10; i++ {
		_, banned := l.Miss("1.1.1.1")
		require.False(t, banned, i)
		clock.add(6 * time.Minute)
	}
}

func TestNotFoundLimiterOff(t *testing.T) {
	l := NewNotFoundLimiter(-1, time.Minute, time.Minute)
	require.Nil(t, l)
	for i := 0; i < 100; i++ {
		_, banned := l.Miss("1.1.1.1")
		require.False(t, banned)
	}
	_, banned := l.Banned("1.1.1.1")
	require.False(t, banned)
	require.Zero(t, l.Tracked())
}

func TestNotFoundLimiterBounded(t *testing.T) {
	clock := newFakeClock()
	l := testLimiter(1, clock)
	l.maxIPs = 3

	// At the cap, the IP updated longest ago is dropped
	for i := 1; i <= 5; i++ {
		l.Miss(fmt.Sprintf("10.0.0.%d", i))
	}
	require.Equal(t, 3, l.Tracked())
	_, banned := l.Miss("10.0.0.1") // forgotten, so this is its first miss again
	require.False(t, banned)
	_, banned = l.Miss("10.0.0.5") // still tracked: its second miss bans it
	require.True(t, banned)

	// Entries whose window and ban are over are swept
	clock.add(2 * time.Hour)
	l.Miss("10.0.0.9")
	require.Equal(t, 1, l.Tracked())
}

func TestDocumentLookupBan(t *testing.T) {
	store := &mockStorage{data: map[string]string{"known": "hello"}}
	h := NewDocumentHandler(6, 1<<20, store, &mockKeyGenerator{fixedKey: "newkey"})
	clock := newFakeClock()
	h.NotFound = testLimiter(3, clock)
	router := chi.NewRouter()
	h.RegisterRoutes(router)

	do := func(method, path, ip string, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Unknown keys on both routes and HEAD all count; a found key does not reset the count
	require.Equal(t, http.StatusNotFound, do(http.MethodGet, "/documents/nope1", "1.1.1.1", "").Code)
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/raw/known", "1.1.1.1", "").Code)
	require.Equal(t, http.StatusNotFound, do(http.MethodHead, "/raw/nope2", "1.1.1.1", "").Code)
	require.Equal(t, http.StatusNotFound, do(http.MethodGet, "/raw/nope3.txt", "1.1.1.1", "").Code)

	rec := do(http.MethodGet, "/documents/nope4", "1.1.1.1", "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "3600", rec.Header().Get("Retry-After"))
	require.Contains(t, rec.Body.String(), "unknown keys")

	// While banned, even known keys are refused on both routes
	for _, path := range []string{"/documents/known", "/raw/known"} {
		rec = do(http.MethodGet, path, "1.1.1.1", "")
		require.Equal(t, http.StatusTooManyRequests, rec.Code, path)
		require.NotEmpty(t, rec.Header().Get("Retry-After"), path)
		require.Equal(t, http.StatusTooManyRequests, do(http.MethodHead, path, "1.1.1.1", "").Code, path)
	}

	// Uploads still work
	rec = do(http.MethodPost, "/documents", "1.1.1.1", "data")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "data", store.data["newkey"])

	// Other clients are not affected
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/raw/known", "2.2.2.2", "").Code)
	require.Equal(t, http.StatusNotFound, do(http.MethodGet, "/raw/nope", "2.2.2.2", "").Code)

	// The ban expires
	clock.add(time.Hour)
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/documents/known", "1.1.1.1", "").Code)
}

func TestOpenDocumentFrom(t *testing.T) {
	store := &mockStorage{data: map[string]string{"known": "hello"}}
	h := NewDocumentHandler(6, 1<<20, store, &mockKeyGenerator{fixedKey: "newkey"})
	h.NotFound = testLimiter(1, newFakeClock())

	doc, err := h.OpenDocumentFrom("1.1.1.1", "known", ToolStreamMemory, ToolReadCost)
	require.NoError(t, err)
	data, err := io.ReadAll(doc)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
	doc.Close()

	_, err = h.OpenDocumentFrom("1.1.1.1", "nope", ToolStreamMemory, ToolReadCost)
	require.ErrorIs(t, err, storage.ErrNotFound)

	var banned *BannedError
	_, err = h.OpenDocumentFrom("1.1.1.1", "nope", ToolStreamMemory, ToolReadCost)
	require.ErrorAs(t, err, &banned)
	require.Equal(t, time.Hour, banned.RetryAfter)

	_, err = h.OpenDocumentFrom("1.1.1.1", "known", ToolStreamMemory, ToolReadCost)
	require.ErrorAs(t, err, &banned)

	require.Equal(t, 1, RetryAfterSeconds(time.Millisecond))
	require.Equal(t, 2, RetryAfterSeconds(1500*time.Millisecond))
}

func TestNotFoundLimiterIPv6Network(t *testing.T) {
	// Addresses in one IPv6 /64 share a count, so a client cannot dodge the ban
	// by moving through its own network
	l := testLimiter(2, newFakeClock())
	_, banned := l.Miss("2001:db8::1")
	require.False(t, banned)
	_, banned = l.Miss("2001:db8::2")
	require.False(t, banned)
	_, banned = l.Miss("2001:db8::3")
	require.True(t, banned)
	_, banned = l.Banned("2001:db8::ffff")
	require.True(t, banned)

	// The next /64 and IPv4 clients are separate
	_, banned = l.Banned("2001:db8:0:1::1")
	require.False(t, banned)
	_, banned = l.Banned("192.0.2.1")
	require.False(t, banned)
	require.Equal(t, 1, l.Tracked())
}
