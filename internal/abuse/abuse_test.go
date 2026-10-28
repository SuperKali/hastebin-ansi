package abuse

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/go-chi/httprate"
	"github.com/stretchr/testify/require"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestEventsOncePerMinutePerKindAndClient(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	e := NewEvents()
	e.now = c.now

	e.Record(KindQuota, "1.2.3.4", "")
	e.Record(KindQuota, "1.2.3.4", "")
	e.Record(KindBusy, "1.2.3.4", "")
	e.Record(KindQuota, "5.6.7.8", "")
	require.Equal(t, 3, e.Len())

	c.add(59 * time.Second)
	e.Record(KindQuota, "1.2.3.4", "")
	require.Equal(t, 3, e.Len())
	c.add(time.Second)
	e.Record(KindQuota, "1.2.3.4", "again")
	require.Equal(t, 4, e.Len())

	// Newest first
	page, total := e.Page(1, 50)
	require.Equal(t, 4, total)
	require.Equal(t, "again", page[0].Detail)
	require.Equal(t, KindQuota, page[3].Kind)
	require.Equal(t, "1.2.3.4", page[3].Client)

	// Unknown clients are recorded as such; nil records nothing
	e.Record(KindInflight, "", "")
	page, _ = e.Page(1, 1)
	require.Equal(t, "unknown", page[0].Client)
	var none *Events
	none.Record(KindQuota, "1.2.3.4", "")
	list, n := none.Page(1, 50)
	require.Empty(t, list)
	require.Zero(t, n)
}

func TestEventsRingBounded(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	e := NewEvents()
	e.now = c.now
	for i := 0; i < MaxEvents+137; i++ {
		e.Record(KindRateLimit, fmt.Sprintf("10.0.%d.%d", i/256, i%256), fmt.Sprint(i))
	}
	require.Equal(t, MaxEvents, e.Len())

	// The oldest went first; pages walk from the newest down
	first, total := e.Page(1, 50)
	require.Equal(t, MaxEvents, total)
	require.Len(t, first, 50)
	require.Equal(t, fmt.Sprint(MaxEvents+136), first[0].Detail)
	last, _ := e.Page(10, 50)
	require.Len(t, last, 50)
	require.Equal(t, "137", last[49].Detail)
	beyond, _ := e.Page(11, 50)
	require.Empty(t, beyond)
}

func TestEventsRecentMapBounded(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	e := NewEvents()
	e.now = c.now
	for i := 0; i < maxRecent+500; i++ {
		e.Record(KindRateLimit, fmt.Sprintf("c%d", i), "")
	}
	require.LessOrEqual(t, len(e.recent), maxRecent)
	require.Equal(t, MaxEvents, e.Len())
}

func TestNormalizeTarget(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1.2.3.4", "1.2.3.4"},
		{" 1.2.3.4 ", "1.2.3.4"},
		{"1.2.3.4/32", "1.2.3.4"},
		{"::ffff:1.2.3.4", "1.2.3.4"},
		{"2001:db8:1:2::5", "2001:db8:1:2::"},
		{"2001:DB8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::"},
		{"2001:db8:1:2::/64", "2001:db8:1:2::"},
		{"2001:db8:1:2::77/128", "2001:db8:1:2::"},
	} {
		got, err := NormalizeTarget(c.in)
		require.NoError(t, err, c.in)
		require.Equal(t, c.want, got, c.in)
	}
	for _, in := range []string{"", "evil", "1.2.3", "1.2.3.0/24", "2001:db8::/48", "2001:db8::/0", "::ffff:1.2.3.4/96",
		"fe80::1%eth0", "1.2.3.4:80", "http://1.2.3.4", "1.2.3.4 5.6.7.8"} {
		_, err := NormalizeTarget(in)
		require.ErrorIs(t, err, ErrBadTarget, in)
	}

	// The same key every per client limit uses for a request from the address
	for _, ip := range []string{"203.0.113.9", "2001:db8:abcd:12::1", "2001:db8:abcd:12:ffff:ffff:ffff:ffff", "::1"} {
		got, err := NormalizeTarget(ip)
		require.NoError(t, err)
		require.Equal(t, httprate.CanonicalizeIP(net.ParseIP(ip).String()), got, ip)
	}
}

func TestBansBlockAndExpire(t *testing.T) {
	// NewBans drops what is over by the real clock
	c := &clock{t: time.Now()}
	expired := Ban{Client: "9.9.9.9", ExpiresAt: c.t.Add(-time.Second)}
	b := NewBans([]Ban{{Client: "1.2.3.4"}, {Client: "2001:db8::", ExpiresAt: c.t.Add(time.Hour)}, expired})
	b.now = c.now
	require.Equal(t, 2, b.Len(), "expired bans are not loaded")

	require.True(t, b.Blocked("1.2.3.4"))
	require.True(t, b.Blocked("2001:db8::"))
	require.False(t, b.Blocked("9.9.9.9"))
	require.False(t, b.Blocked("1.2.3.5"))
	require.False(t, b.Blocked(""))

	// Past its end a ban stops at once, and a sweep drops it
	c.add(time.Hour)
	require.False(t, b.Blocked("2001:db8::"))
	require.Equal(t, 1, b.Len())
	require.True(t, b.Blocked("1.2.3.4"), "a ban without end stays")

	b.Put(Ban{Client: "5.5.5.5"})
	require.True(t, b.Blocked("5.5.5.5"))
	require.True(t, b.Remove("5.5.5.5"))
	require.False(t, b.Remove("5.5.5.5"))
	require.False(t, b.Blocked("5.5.5.5"))

	var none *Bans
	require.False(t, none.Blocked("1.2.3.4"))
	none.Put(Ban{Client: "1.2.3.4"})
	require.False(t, none.Remove("1.2.3.4"))
}
