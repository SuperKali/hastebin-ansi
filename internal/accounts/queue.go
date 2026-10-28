package accounts

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// QueueItem is a store key waiting to be deleted
type QueueItem struct {
	StoreKey string
	Attempts int
}

// QueueBatch returns up to limit store keys waiting to be deleted, those
// tried least often and longest waiting first
func (db *DB) QueueBatch(ctx context.Context, limit int) ([]QueueItem, error) {
	rows, err := db.W.QueryContext(ctx,
		"SELECT store_key, attempts FROM delete_queue ORDER BY attempts, enqueued_at, store_key LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []QueueItem
	for rows.Next() {
		var item QueueItem
		if err := rows.Scan(&item.StoreKey, &item.Attempts); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

// QueueDone removes a deleted store key from the queue
func (db *DB) QueueDone(ctx context.Context, storeKey string) error {
	_, err := db.W.ExecContext(ctx, "DELETE FROM delete_queue WHERE store_key = ?", storeKey)
	return err
}

// QueueFailed counts a failed attempt to delete a store key; it stays queued
func (db *DB) QueueFailed(ctx context.Context, storeKey string) error {
	_, err := db.W.ExecContext(ctx, "UPDATE delete_queue SET attempts = attempts + 1 WHERE store_key = ?", storeKey)
	return err
}

// QueueLen returns how many store keys wait to be deleted
func (db *DB) QueueLen(ctx context.Context) (int64, error) {
	var n int64
	err := db.R.QueryRowContext(ctx, "SELECT COUNT(*) FROM delete_queue").Scan(&n)
	return n, err
}

const (
	// DrainBatch is how many store keys one pass of the drainer deletes at most
	DrainBatch = 100

	// DrainInterval is how often the drainer runs without being kicked
	DrainInterval = time.Minute
)

// Drainer deletes the queued store keys: after every Kick, and every
// DrainInterval to retry the ones that failed
type Drainer struct {
	DB *DB

	// Delete removes one store key; a key that does not exist is not an error
	Delete func(key string) error

	kick chan struct{}
}

// NewDrainer returns a drainer of db that deletes with del
func NewDrainer(db *DB, del func(key string) error) *Drainer {
	return &Drainer{DB: db, Delete: del, kick: make(chan struct{}, 1)}
}

// Kick asks for a pass soon. It never blocks.
func (d *Drainer) Kick() {
	if d == nil {
		return
	}
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Run drains the queue until ctx is done
func (d *Drainer) Run(ctx context.Context) {
	ticker := time.NewTicker(DrainInterval)
	defer ticker.Stop()
	d.Drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.kick:
		}
		d.Drain(ctx)
	}
}

// Drain makes passes over the queue, DrainBatch keys each, until a pass
// deletes nothing: the queue is empty, or what is left failed this time and
// waits for the next run. A key is tried once per run. It returns how many
// keys it deleted.
func (d *Drainer) Drain(ctx context.Context) int {
	total := 0
	failedNow := map[string]bool{}
	for ctx.Err() == nil {
		batch, err := d.DB.QueueBatch(ctx, DrainBatch)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn().Err(err).Msg("Failed to read the delete queue")
			}
			return total
		}
		done, failed := 0, 0
		for _, item := range batch {
			if failedNow[item.StoreKey] {
				continue
			}
			if err := d.Delete(item.StoreKey); err != nil {
				failedNow[item.StoreKey] = true
				failed++
				if err := d.DB.QueueFailed(ctx, item.StoreKey); err != nil {
					log.Warn().Err(err).Msg("Failed to record a failed delete")
				}
				continue
			}
			if err := d.DB.QueueDone(ctx, item.StoreKey); err != nil {
				log.Warn().Err(err).Msg("Failed to remove a deleted key from the queue")
				continue
			}
			done++
		}
		total += done
		if failed > 0 {
			log.Warn().Int("failed", failed).Int("deleted", done).Msg("Some pastes could not be deleted from the store, retrying later")
		}
		// A full batch that made progress may have more behind it
		if len(batch) < DrainBatch || done == 0 {
			return total
		}
	}
	return total
}
