package handler

import (
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
		_, ok := b.Take(1)
		require.True(t, ok, i)
		now = now.Add(10 * time.Second)
	}
	left, ok := b.Take(1)
	require.False(t, ok)
	require.Equal(t, 30*time.Second, left, "the first paste frees its place 60 s after it")
	_, ok = b.Take(2)
	require.True(t, ok, "another account has its own budget")

	now = now.Add(30 * time.Second)
	_, ok = b.Take(1)
	require.True(t, ok)
	left, ok = b.Take(1)
	require.False(t, ok)
	require.Equal(t, 10*time.Second, left)

	// Idle accounts are forgotten
	now = now.Add(2 * time.Minute)
	_, ok = b.Take(3)
	require.True(t, ok)
	require.Len(t, b.accounts, 1)

	var none *PasteBudget
	_, ok = none.Take(1)
	require.True(t, ok, "a nil budget never refuses")
	require.Nil(t, NewPasteBudget(0, time.Minute))
}
