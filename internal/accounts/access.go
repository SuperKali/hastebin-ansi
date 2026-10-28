package accounts

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Access is what the read path needs to know about a key: the row of a
// registered paste, the tombstone of a removed one and whether its content
// waits in the delete queue, any of them missing. A key with none of them
// is an anonymous paste, or none.
type Access struct {
	// Registered is set when a paste row exists; the fields below it are its own
	Registered bool
	UserID     int64
	Visibility int
	CreatedAt  time.Time
	ExpiresAt  time.Time // zero for never
	SeenAt     time.Time // zero while no one else opened it
	EditedAt   time.Time // zero while the content never changed
	Language   string    // "" when unknown

	// Tombstoned is set when a moderator removed the paste, which was
	// TombstoneVisibility then
	Tombstoned          bool
	TombstoneVisibility int

	// Pending is set while the content under the key waits in the delete
	// queue: the paste, or the revision, was deleted and its row is gone.
	// Such a key reads as missing for everyone, never as anonymous.
	Pending bool
}

// AccessOf reads, in one query, what decides who may read key. Each part
// is a primary key lookup.
func (db *DB) AccessOf(ctx context.Context, key string) (*Access, error) {
	var a Access
	var user, visibility, created, tombVisibility sql.NullInt64
	var expires, seen, edited sql.NullInt64
	var language sql.NullString
	err := db.R.QueryRowContext(ctx, `SELECT p.user_id, p.visibility, p.created_at, p.expires_at, p.seen_at, p.edited_at,
		p.language, t.visibility, d.store_key IS NOT NULL
		FROM (SELECT ? AS k) q
		LEFT JOIN pastes p ON p.key = q.k
		LEFT JOIN tombstones t ON t.key = q.k
		LEFT JOIN delete_queue d ON d.store_key = q.k`, key).
		Scan(&user, &visibility, &created, &expires, &seen, &edited, &language, &tombVisibility, &a.Pending)
	if err != nil {
		return nil, err
	}
	if user.Valid {
		a.Registered = true
		a.UserID = user.Int64
		a.Visibility = int(visibility.Int64)
		a.CreatedAt = time.Unix(created.Int64, 0).UTC()
		a.ExpiresAt = fromUnix(expires)
		a.SeenAt = fromUnix(seen)
		a.EditedAt = fromUnix(edited)
		a.Language = language.String
	}
	if tombVisibility.Valid {
		a.Tombstoned = true
		a.TombstoneVisibility = int(tombVisibility.Int64)
	}
	return &a, nil
}

// SeenAt returns when someone other than the owner first read a paste, the
// zero time while no one did. A missing paste is ErrNotFound.
func (db *DB) SeenAt(ctx context.Context, key string) (time.Time, error) {
	var seen sql.NullInt64
	err := db.R.QueryRowContext(ctx, "SELECT seen_at FROM pastes WHERE key = ?", key).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	return fromUnix(seen), err
}

// MarkSeen records the first read of a paste by someone other than its
// owner. It reports whether it wrote: false when seen_at was set already or
// the paste is gone. The write is committed when it returns.
func (db *DB) MarkSeen(ctx context.Context, key string, at time.Time) (bool, error) {
	res, err := db.W.ExecContext(ctx, "UPDATE pastes SET seen_at = ? WHERE key = ? AND seen_at IS NULL", unix(at), key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ViewCount is the views of one paste counted since the last flush
type ViewCount struct {
	Views int64
	Last  time.Time
}

// AddViews adds counted views to their pastes in one transaction and moves
// last_opened_at forward. Pastes deleted meanwhile are skipped.
func (db *DB) AddViews(ctx context.Context, counts map[string]ViewCount) error {
	if len(counts) == 0 {
		return nil
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		"UPDATE pastes SET views = views + ?, last_opened_at = MAX(COALESCE(last_opened_at, 0), ?) WHERE key = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for key, c := range counts {
		if _, err := stmt.ExecContext(ctx, c.Views, unix(c.Last), key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ExpirePastes deletes up to limit pastes whose expiry is at or before now,
// in one transaction, and queues their content and revisions for deletion
// from the store. It returns how many it deleted. Like every delete it goes
// through deletePaste.
func (db *DB) ExpirePastes(ctx context.Context, now time.Time, limit int) (int, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		"SELECT key FROM pastes WHERE expires_at IS NOT NULL AND expires_at <= ? ORDER BY expires_at, key LIMIT ?", unix(now), limit)
	if err != nil {
		return 0, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	at := unix(db.now())
	for _, key := range keys {
		if _, err := deletePaste(ctx, tx, key, at); err != nil {
			return 0, err
		}
	}
	return len(keys), tx.Commit()
}

// deletePaste queues the content and revisions of a paste for deletion
// from the store and deletes its row, inside tx. Every way a paste row goes
// away goes through here, so the key and its revisions are in the delete
// queue from the moment the row is gone, and AccessOf reports them Pending
// until the drainer has deleted the content. Without that the gate would
// take a key with no row for an anonymous public paste and serve the
// content of a deleted private one until the drainer got to it.
func deletePaste(ctx context.Context, tx *sql.Tx, key string, now int64) (int64, error) {
	var queued int64
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO delete_queue (store_key, enqueued_at) SELECT store_key, ? FROM paste_revisions WHERE paste_key = ?
		ON CONFLICT (store_key) DO NOTHING`, []any{now, key}},
		{"INSERT INTO delete_queue (store_key, enqueued_at) VALUES (?, ?) ON CONFLICT (store_key) DO NOTHING", []any{key, now}},
	} {
		res, err := tx.ExecContext(ctx, q.query, q.args...)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		queued += n
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM pastes WHERE key = ?", key)
	return queued, err
}
