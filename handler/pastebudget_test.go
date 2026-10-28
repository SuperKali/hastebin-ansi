package handler

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The window slides: each paste frees its place exactly one window after it
func TestPasteBudget(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := NewPasteBudget(3, time.Minute)
	b.now = func() time.Time { return now }

	for i := range 3 {
		_, _, ok := b.Take(1)
		require.True(t, ok, i)
		now = now.Add(10 * time.Second)
	}
	_, left, ok := b.Take(1)
	require.False(t, ok)
	require.Equal(t, 30*time.Second, left, "the first paste frees its place 60 s after it")
	_, _, ok = b.Take(2)
	require.True(t, ok, "another account has its own budget")

	now = now.Add(30 * time.Second)
	_, _, ok = b.Take(1)
	require.True(t, ok)
	_, left, ok = b.Take(1)
	require.False(t, ok)
	require.Equal(t, 10*time.Second, left)

	// Idle accounts are forgotten
	now = now.Add(2 * time.Minute)
	_, _, ok = b.Take(3)
	require.True(t, ok)
	require.Len(t, b.accounts, 1)

	var none *PasteBudget
	_, _, ok = none.Take(1)
	require.True(t, ok, "a nil budget never refuses")
	require.Nil(t, NewPasteBudget(0, time.Minute))
}

// A place given back is free at once, only that place, and only once; a
// refund after the place left the window, or of a refused take, does nothing
func TestPasteBudgetRefund(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := NewPasteBudget(3, time.Minute)
	b.now = func() time.Time { return now }

	first, _, ok := b.Take(1)
	require.True(t, ok)
	now = now.Add(10 * time.Second)
	second, _, ok := b.Take(1)
	require.True(t, ok)
	now = now.Add(10 * time.Second)
	_, _, ok = b.Take(1)
	require.True(t, ok)
	refused, _, ok := b.Take(1)
	require.False(t, ok)
	refused()
	_, _, ok = b.Take(1)
	require.False(t, ok, "the refund of a refused take frees nothing")

	second()
	second()
	_, _, ok = b.Take(1)
	require.True(t, ok, "the place given back is free")
	_, left, ok := b.Take(1)
	require.False(t, ok, "a refund run twice frees one place")
	require.Equal(t, 40*time.Second, left, "the oldest place is still the first")

	// Once the first place left the window, its refund frees nothing more
	now = now.Add(40 * time.Second)
	_, _, ok = b.Take(1)
	require.True(t, ok)
	first()
	_, _, ok = b.Take(1)
	require.False(t, ok)

	var none *PasteBudget
	refund, _, ok := none.Take(1)
	require.True(t, ok)
	refund()
}

// Parallel takes never hold more than the limit at once, refunds or not:
// taking before the work and giving back after it opens no race
func TestPasteBudgetParallel(t *testing.T) {
	b := NewPasteBudget(10, time.Hour)
	var held, most, granted atomic.Int64
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refund, _, ok := b.Take(7)
			if !ok {
				return
			}
			granted.Add(1)
			n := held.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			if i%2 == 0 {
				held.Add(-1)
				refund()
			}
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, most.Load(), int64(10))
	require.LessOrEqual(t, held.Load(), int64(10))
	require.GreaterOrEqual(t, granted.Load(), int64(10))
}
