package accounts

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	// SweepInterval is how often pastes past their expiry are deleted
	SweepInterval = time.Minute

	// SweepBatch is how many expired pastes one transaction deletes at most
	SweepBatch = 100
)

// Sweeper deletes registered pastes past their expiry: their rows go in
// bounded batches, their content through the delete queue. The read path
// hides them from the moment they expire; the sweeper only frees the space.
type Sweeper struct {
	DB *DB

	// Drainer deletes the queued content; nil leaves it queued
	Drainer *Drainer
}

// NewSweeper returns a sweeper of db that hands content to drainer
func NewSweeper(db *DB, drainer *Drainer) *Sweeper {
	return &Sweeper{DB: db, Drainer: drainer}
}

// Sweep deletes every paste expired by now, SweepBatch at a time, and
// kicks the drainer when it deleted any. It returns how many it deleted.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	total := 0
	defer func() {
		if total > 0 {
			s.Drainer.Kick()
		}
	}()
	now := s.DB.now()
	for ctx.Err() == nil {
		n, err := s.DB.ExpirePastes(ctx, now, SweepBatch)
		total += n
		if err != nil {
			return total, err
		}
		if n < SweepBatch {
			break
		}
	}
	return total, ctx.Err()
}

// Run sweeps every SweepInterval until ctx is done
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		n, err := s.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Msg("Failed to delete expired pastes, retrying later")
		} else if n > 0 {
			log.Info().Int("deleted", n).Msg("Expired registered pastes deleted")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
