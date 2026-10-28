package handler

import (
	"testing"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestMemoryBudget(t *testing.T) {
	b := NewMemoryBudget(100)
	require.EqualValues(t, 100, b.Size())
	require.EqualValues(t, 25, b.ClientMax())

	// The total counts across clients, and a full budget never waits for room
	var holds []func()
	for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		hold, ok := b.Acquire(ip, 25)
		require.True(t, ok)
		holds = append(holds, hold)
	}
	_, ok := b.Acquire("192.0.2.4", 26)
	require.False(t, ok)
	last, ok := b.Acquire("192.0.2.4", 25)
	require.True(t, ok)
	require.EqualValues(t, 100, b.InUse())
	_, ok = b.Acquire("", 1)
	require.False(t, ok)

	holds[0]()
	holds[0]()
	require.EqualValues(t, 75, b.InUse())
	for _, hold := range holds[1:] {
		hold()
	}
	last()
	require.Zero(t, b.InUse())

	// No single charge may be larger than one client's share, with or without a client
	for _, ip := range []string{"", "192.0.2.1"} {
		_, ok = b.Acquire(ip, 26)
		require.False(t, ok, ip)
		_, ok = b.Acquire(ip, 1<<40)
		require.False(t, ok, ip)
	}
	require.Zero(t, b.InUse())

	// Nothing to charge
	none, ok := b.Acquire("", 0)
	require.True(t, ok)
	none()
}

func TestMemoryBudgetClientShare(t *testing.T) {
	b := NewMemoryBudget(100)

	// One client holds at most a quarter of the budget
	one, ok := b.Acquire("192.0.2.1", 20)
	require.True(t, ok)
	_, ok = b.Acquire("192.0.2.1", 6)
	require.False(t, ok)
	two, ok := b.Acquire("192.0.2.1", 5)
	require.True(t, ok)

	// Another address of the same IPv6 /64 is the same client
	v6, ok := b.Acquire("2001:db8::1", 25)
	require.True(t, ok)
	_, ok = b.Acquire("2001:db8::2", 1)
	require.False(t, ok)
	v6()

	// Other clients are not affected
	other, ok := b.Acquire("192.0.2.2", 25)
	require.True(t, ok)
	one()
	two()
	other()
	require.Zero(t, b.InUse())

	// A client holding nothing still gets no more than its share
	_, ok = b.Acquire("192.0.2.1", 60)
	require.False(t, ok)
	require.Zero(t, b.InUse())
}

func TestCharge(t *testing.T) {
	b := NewMemoryBudget(400)

	// A charge grows as long as it fits, and keeps what it has when it does not
	c := b.NewCharge("192.0.2.1")
	require.True(t, c.Grow(30))
	require.True(t, c.Grow(30))
	require.EqualValues(t, 60, c.Size())
	var holds []func()
	for _, ip := range []string{"192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		hold, ok := b.Acquire(ip, 100)
		require.True(t, ok)
		holds = append(holds, hold)
	}
	require.False(t, c.Grow(41))
	require.EqualValues(t, 60, c.Size())
	require.EqualValues(t, 360, b.InUse())
	for _, hold := range holds {
		hold()
	}

	// It stops at one client's share
	require.True(t, c.Grow(40))
	require.False(t, c.Grow(1))
	require.EqualValues(t, 100, c.Size())

	c.Release()
	c.Release()
	require.Zero(t, b.InUse())
	require.False(t, c.Grow(1), "a released charge cannot grow")

	// A charge without a client is capped the same way
	c = b.NewCharge("")
	require.True(t, c.Grow(100))
	require.False(t, c.Grow(1))
	c.Release()

	// Without a budget a charge only counts
	var off *MemoryBudget
	c = off.NewCharge("192.0.2.1")
	require.True(t, c.Grow(1<<40))
	require.EqualValues(t, 1<<40, c.Size())
	c.Release()
}

func TestLoadedLengthCap(t *testing.T) {
	// The default budget: 8 MiB
	require.EqualValues(t, MaxLoadedLength, LoadedLengthCap(256<<20))
	require.EqualValues(t, MaxLoadedLength, LoadedLengthCap(0))

	// A 300 MiB container gets a 150 MiB budget: still 8 MiB
	require.EqualValues(t, MaxLoadedLength, LoadedLengthCap(150<<20))

	// A small budget lowers it so the costliest read fits one client's share
	for _, total := range []int64{64 << 20, 32 << 20, 20 << 20} {
		limit := LoadedLengthCap(total)
		require.Less(t, limit, int64(MaxLoadedLength))
		share := NewMemoryBudget(total / 2).ClientMax()
		require.LessOrEqual(t, RawReadCost*limit+previewMemory, share, total)
		require.LessOrEqual(t, ToolReadCost*limit+ToolStreamMemory, share, total)
		require.LessOrEqual(t, UploadCost*limit, NewMemoryBudget(total-total/2).ClientMax(), total)
	}
	require.EqualValues(t, 1, LoadedLengthCap(1<<20), "never 0, which means no limit")
}

func TestStreams(t *testing.T) {
	require.True(t, Streams(storage.NewFileStorage(t.TempDir(), 0)))
	require.False(t, Streams(&mockStorage{data: map[string]string{}}))
}

func TestMemoryBudgetOff(t *testing.T) {
	b := NewMemoryBudget(0)
	require.Nil(t, b)
	release, ok := b.Acquire("192.0.2.1", 1<<40)
	require.True(t, ok)
	release()
	require.Zero(t, b.InUse())
	require.Zero(t, b.Size())
}

func TestSetMemoryBudget(t *testing.T) {
	h := setupHandler()
	h.SetMemoryBudget(101)
	require.EqualValues(t, 50, h.ReadMemory.Size())
	require.EqualValues(t, 51, h.UploadMemory.Size())

	// The two halves are apart: full uploads leave reads alone
	_, ok := h.UploadMemory.Acquire("", 12)
	require.True(t, ok)
	_, ok = h.ReadMemory.Acquire("", 12)
	require.True(t, ok)

	h.SetMemoryBudget(0)
	require.Nil(t, h.ReadMemory)
	require.Nil(t, h.UploadMemory)
}
