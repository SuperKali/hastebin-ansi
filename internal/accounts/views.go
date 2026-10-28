package accounts

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ViewFlushInterval is how often counted views are written to the database
const ViewFlushInterval = 15 * time.Second

// Views counts the views of registered pastes in memory and writes them to
// the database in one transaction every ViewFlushInterval, so a read never
// waits for a write. Only reads by someone other than the owner count.
type Views struct {
	db *DB

	mu      sync.Mutex
	pending map[string]ViewCount
}

// NewViews returns a view counter that writes to db
func NewViews(db *DB) *Views {
	return &Views{db: db, pending: map[string]ViewCount{}}
}

// Add counts one view of key at the given time. It is safe on a nil Views.
func (v *Views) Add(key string, at time.Time) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.pending[key]
	c.Views++
	if at.After(c.Last) {
		c.Last = at
	}
	v.pending[key] = c
}

// Pending returns how many pastes have views not written yet
func (v *Views) Pending() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.pending)
}

// Flush writes the counted views. On failure they are kept for the next flush.
func (v *Views) Flush(ctx context.Context) error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	counts := v.pending
	v.pending = map[string]ViewCount{}
	v.mu.Unlock()
	if len(counts) == 0 {
		return nil
	}

	err := v.db.AddViews(ctx, counts)
	if err == nil {
		return nil
	}
	// Put them back, merged with the views counted meanwhile
	v.mu.Lock()
	defer v.mu.Unlock()
	for key, c := range counts {
		now := v.pending[key]
		now.Views += c.Views
		if c.Last.After(now.Last) {
			now.Last = c.Last
		}
		v.pending[key] = now
	}
	return err
}

// Run flushes the views every ViewFlushInterval until ctx is done. The last
// views are flushed by whoever stops it, before the database closes.
func (v *Views) Run(ctx context.Context) {
	ticker := time.NewTicker(ViewFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := v.Flush(ctx); err != nil && ctx.Err() == nil {
				log.Warn().Err(err).Msg("Failed to write paste views, retrying later")
			}
		}
	}
}
