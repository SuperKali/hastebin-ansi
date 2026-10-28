package handler

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testQuota allows limit bytes per hour on the given clock
func testQuota(limit int64, c *fakeClock) *UploadQuota {
	q := NewUploadQuota(limit, time.Hour)
	q.now = c.now
	return q
}

func TestUploadQuota(t *testing.T) {
	clock := newFakeClock()
	q := testQuota(100, clock)

	_, ok := q.Charge("192.0.2.1", 60)
	require.True(t, ok)
	_, over := q.Exceeded("192.0.2.1")
	require.False(t, over)

	// Over what is left: refused, and nothing is counted
	left, ok := q.Charge("192.0.2.1", 41)
	require.False(t, ok)
	require.Equal(t, time.Hour, left)
	_, ok = q.Charge("192.0.2.1", 40)
	require.True(t, ok)

	clock.add(10 * time.Minute)
	left, over = q.Exceeded("192.0.2.1")
	require.True(t, over)
	require.Equal(t, 50*time.Minute, left)

	// A refund gives the bytes back
	q.Refund("192.0.2.1", 40)
	_, over = q.Exceeded("192.0.2.1")
	require.False(t, over)
	_, ok = q.Charge("192.0.2.1", 40)
	require.True(t, ok)

	// Other clients have their own quota
	_, ok = q.Charge("192.0.2.2", 100)
	require.True(t, ok)

	// A new window starts after an hour
	clock.add(50 * time.Minute)
	_, over = q.Exceeded("192.0.2.1")
	require.False(t, over)
	_, ok = q.Charge("192.0.2.1", 100)
	require.True(t, ok)
}

func TestUploadQuotaIPv6Network(t *testing.T) {
	q := testQuota(100, newFakeClock())
	_, ok := q.Charge("2001:db8::1", 60)
	require.True(t, ok)
	_, ok = q.Charge("2001:db8::2", 60)
	require.False(t, ok)
	_, ok = q.Charge("2001:db8:0:1::1", 60)
	require.True(t, ok)
}

func TestUploadQuotaOff(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		q := NewUploadQuota(limit, time.Hour)
		require.Nil(t, q)
		_, ok := q.Charge("192.0.2.1", 1<<40)
		require.True(t, ok)
		_, over := q.Exceeded("192.0.2.1")
		require.False(t, over)
		q.Refund("192.0.2.1", 1)
		require.Zero(t, q.Tracked())
	}

	// An unknown client is not counted
	q := NewUploadQuota(10, time.Hour)
	_, ok := q.Charge("", 100)
	require.True(t, ok)
	require.Zero(t, q.Tracked())
}

func TestUploadQuotaBounded(t *testing.T) {
	clock := newFakeClock()
	q := testQuota(100, clock)
	q.maxClients = 3

	for i := 0; i < 10; i++ {
		_, ok := q.Charge(fmt.Sprintf("192.0.2.%d", i), 10)
		require.True(t, ok)
	}
	require.Equal(t, 3, q.Tracked())

	// Finished windows are swept
	clock.add(2 * time.Hour)
	_, ok := q.Charge("198.51.100.1", 10)
	require.True(t, ok)
	require.Equal(t, 1, q.Tracked())
}
