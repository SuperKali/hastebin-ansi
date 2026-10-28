package abuse

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// MaxManualBans caps how many manual bans exist at once
const MaxManualBans = 10000

// banSweepEvery is the least time between two sweeps of expired bans
const banSweepEvery = time.Minute

// Ban is a manual ban of one client
type Ban struct {
	// Client is an IPv4 address or an IPv6 /64, in handler.ClientKey form
	Client    string
	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time // zero for never
	AdminID   int64
}

// Expired reports whether the ban is over at now
func (b Ban) Expired(now time.Time) bool {
	return !b.ExpiresAt.IsZero() && !now.Before(b.ExpiresAt)
}

// Bans holds the manual bans in memory, keyed by client, so the check on
// every request never touches the database. The admin API keeps it in step
// with the ip_bans table. Expired bans are ignored at once and dropped by a
// sweep at most once a minute. Methods are safe on a nil Bans, which blocks no one.
type Bans struct {
	mu        sync.RWMutex
	byClient  map[string]Ban
	lastSweep time.Time
	now       func() time.Time
}

// NewBans returns the bans of list, skipping expired ones
func NewBans(list []Ban) *Bans {
	b := &Bans{byClient: map[string]Ban{}, now: time.Now}
	now := b.now()
	for _, ban := range list {
		if !ban.Expired(now) {
			b.byClient[ban.Client] = ban
		}
	}
	return b
}

// Blocked reports whether client, in handler.ClientKey form, is banned now
func (b *Bans) Blocked(client string) bool {
	if b == nil || client == "" {
		return false
	}
	now := b.now()
	b.mu.RLock()
	ban, ok := b.byClient[client]
	sweep := now.Sub(b.lastSweep) >= banSweepEvery
	b.mu.RUnlock()
	if sweep {
		b.sweep(now)
	}
	return ok && !ban.Expired(now)
}

// sweep drops the expired bans
func (b *Bans) sweep(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.lastSweep) < banSweepEvery {
		return
	}
	b.lastSweep = now
	for client, ban := range b.byClient {
		if ban.Expired(now) {
			delete(b.byClient, client)
		}
	}
}

// Put adds or replaces a ban
func (b *Bans) Put(ban Ban) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.byClient[ban.Client] = ban
}

// Remove drops the ban of client and reports whether there was one
func (b *Bans) Remove(client string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.byClient[client]
	delete(b.byClient, client)
	return ok
}

// Len returns how many bans are held, expired ones not swept yet included
func (b *Bans) Len() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.byClient)
}

// ErrBadTarget is returned for a ban target that is not an address
var ErrBadTarget = errors.New("the target must be an IPv4 address, an IPv6 address or an IPv6 /64")

// NormalizeTarget turns what an admin typed into the client key the limits
// use: an IPv4 address as is, an IPv6 address or network as its /64. An
// IPv4 address written as IPv6 (::ffff:1.2.3.4) is the IPv4 address. A
// network must be an IPv4 /32 or an IPv6 /64 or longer; anything wider
// would ban more than one client and is refused.
func NormalizeTarget(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return "", ErrBadTarget
	}
	var addr netip.Addr
	if strings.Contains(s, "/") {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			return "", ErrBadTarget
		}
		addr = prefix.Addr()
		if addr.Is4() && prefix.Bits() != 32 || addr.Is6() && !addr.Is4In6() && prefix.Bits() < 64 {
			return "", ErrBadTarget
		}
		if addr.Is4In6() && prefix.Bits() != 128 {
			return "", ErrBadTarget
		}
	} else {
		var err error
		if addr, err = netip.ParseAddr(s); err != nil {
			return "", ErrBadTarget
		}
	}
	if addr.Zone() != "" {
		return "", ErrBadTarget
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String(), nil
	}
	network, err := addr.Prefix(64)
	if err != nil {
		return "", ErrBadTarget
	}
	// The same form httprate.CanonicalizeIP gives: the masked address, no length
	return network.Addr().String(), nil
}
