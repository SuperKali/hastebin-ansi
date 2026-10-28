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
// tried least often and longest waiting first. It reads on R: the writer
// stays free for the requests.
func (db *DB) QueueBatch(ctx context.Context, limit int) ([]QueueItem, error) {
	rows, err := db.R.QueryContext(ctx,
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

// QueueSettle records the outcome of one pass in one transaction: the
// store keys in done were deleted and leave the queue, those in failed
// count one more attempt and stay queued
func (db *DB) QueueSettle(ctx context.Context, done, failed []string) error {
	if len(done) == 0 && len(failed) == 0 {
		return nil
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		query string
		keys  []string
	}{
		{"DELETE FROM delete_queue WHERE store_key = ?", done},
		{"UPDATE delete_queue SET attempts = attempts + 1 WHERE store_key = ?", failed},
	} {
		if len(q.keys) == 0 {
			continue
		}
		stmt, err := tx.PrepareContext(ctx, q.query)
		if err != nil {
			return err
		}
		for _, key := range q.keys {
			if _, err := stmt.ExecContext(ctx, key); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}
	return tx.Commit()
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
		var done, failed []string
		for _, item := range batch {
			if failedNow[item.StoreKey] {
				continue
			}
			if err := d.Delete(item.StoreKey); err != nil {
				failedNow[item.StoreKey] = true
				failed = append(failed, item.StoreKey)
				continue
			}
			done = append(done, item.StoreKey)
		}
		// One write for the whole pass. Should it fail, the deleted keys
		// stay queued and hidden; deleting them again is harmless.
		if err := d.DB.QueueSettle(ctx, done, failed); err != nil {
			log.Warn().Err(err).Msg("Failed to record a pass of the delete queue")
			return total
		}
		total += len(done)
		if len(failed) > 0 {
			log.Warn().Int("failed", len(failed)).Int("deleted", len(done)).Msg("Some pastes could not be deleted from the store, retrying later")
		}
		// A full batch that made progress may have more behind it
		if len(batch) < DrainBatch || len(done) == 0 {
			return total
		}
	}
	return total
}
