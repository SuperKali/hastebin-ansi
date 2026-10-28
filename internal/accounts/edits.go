package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrEditSeen is returned for an edit of a paste someone other than the
	// owner opened already
	ErrEditSeen = errors.New("paste opened by someone else")

	// ErrTooManyRevisions is returned for an edit of a paste that keeps as
	// many older contents as allowed
	ErrTooManyRevisions = errors.New("too many revisions")
)

// RevisionKey is the store key of an older content of a paste: a dotted key,
// which no route reaches, since every route cuts a key at its first dot
func RevisionKey(key string, rev int) string {
	return fmt.Sprintf("%s.r%d", key, rev)
}

// EditMarks returns when someone other than the owner first read a paste
// and when its content last changed, each the zero time for never. A
// missing paste is ErrNotFound.
func (db *DB) EditMarks(ctx context.Context, key string) (seen, edited time.Time, err error) {
	var s, e sql.NullInt64
	err = db.R.QueryRowContext(ctx, "SELECT seen_at, edited_at FROM pastes WHERE key = ?", key).Scan(&s, &e)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, ErrNotFound
	}
	return fromUnix(s), fromUnix(e), err
}

// NextRevision returns how many older contents a paste keeps and the number
// the next one gets
func (db *DB) NextRevision(ctx context.Context, key string) (count, next int, err error) {
	err = db.R.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(rev), 0) + 1 FROM paste_revisions WHERE paste_key = ?", key).
		Scan(&count, &next)
	return count, next, err
}

// RevisionStoreKey returns the store key of revision rev of a paste of a
// user. A paste or revision that is not there, or not the user's, is ErrNotFound.
func (db *DB) RevisionStoreKey(ctx context.Context, userID int64, key string, rev int) (string, error) {
	var storeKey string
	err := db.R.QueryRowContext(ctx, `SELECT r.store_key FROM paste_revisions r JOIN pastes p ON p.key = r.paste_key
		WHERE r.paste_key = ? AND r.rev = ? AND p.user_id = ?`, key, rev, userID).Scan(&storeKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return storeKey, err
}

// Edit is a new content of a paste, stored already, whose old content was
// copied to the store under StoreKey
type Edit struct {
	Key      string
	UserID   int64
	Rev      int
	StoreKey string
	OldSize  int64
	NewSize  int64

	// Until is the end of the edit window: an edit at or past it is ErrEditExpired
	Until time.Time

	// MaxRevisions is how many older contents a paste may keep, 0 for no limit
	MaxRevisions int

	// TokenID or SessionHash name what authenticated the edit, see NewPaste
	TokenID     int64
	SessionHash []byte
}

// ErrEditExpired is returned for an edit at or past the end of the edit window
var ErrEditExpired = errors.New("edit window over")

// RecordEdit records an edit in one transaction: the old content as
// revision Rev, the new size, edited_at and edit_count. Within it the paste
// must still be the user's (ErrNotFound), unseen (ErrEditSeen), inside its
// window (ErrEditExpired) and under MaxRevisions (ErrTooManyRevisions); the
// session or token must still exist (ErrRevoked), the account be enabled
// (ErrUserDisabled), and the user's bytes, the old content now counted as a
// revision, fit in limits.MaxBytes (ErrStorageLimit). Then nothing is recorded.
func (db *DB) RecordEdit(ctx context.Context, e Edit, limits Limits) (*Paste, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := checkUploader(ctx, tx, e.UserID, e.TokenID, e.SessionHash); err != nil {
		return nil, err
	}
	var seen sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT seen_at FROM pastes WHERE key = ? AND user_id = ?", e.Key, e.UserID).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	now := db.now()
	switch {
	case seen.Valid:
		return nil, ErrEditSeen
	case !now.Before(e.Until):
		return nil, ErrEditExpired
	}
	var revisions int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM paste_revisions WHERE paste_key = ?", e.Key).Scan(&revisions); err != nil {
		return nil, err
	}
	if e.MaxRevisions > 0 && revisions >= e.MaxRevisions {
		return nil, ErrTooManyRevisions
	}
	_, bytes, err := usage(ctx, tx, e.UserID)
	if err != nil {
		return nil, err
	}
	if limits.MaxBytes > 0 && bytes+e.NewSize > limits.MaxBytes {
		return nil, ErrStorageLimit
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES (?, ?, ?, ?, ?)",
		e.Key, e.Rev, e.StoreKey, e.OldSize, unix(now)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE pastes SET size = ?, edited_at = ?, edit_count = edit_count + 1 WHERE key = ?",
		e.NewSize, unix(now), e.Key); err != nil {
		return nil, err
	}
	p, err := scanPaste(tx.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE key = ?", e.Key))
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// checkUploader checks, inside tx, that the token or session of a request
// still exists (ErrRevoked) and that its account is enabled (ErrUserDisabled).
// Without either it checks only the account.
func checkUploader(ctx context.Context, tx *sql.Tx, userID, tokenID int64, sessionHash []byte) error {
	if tokenID != 0 || sessionHash != nil {
		var n int
		query, arg := "SELECT COUNT(*) FROM api_tokens WHERE id = ? AND user_id = ?", any(tokenID)
		if tokenID == 0 {
			query, arg = "SELECT COUNT(*) FROM sessions WHERE id_hash = ? AND user_id = ?", sessionHash
		}
		if err := tx.QueryRowContext(ctx, query, arg, userID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrRevoked
		}
	}
	var disabled sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT disabled_at FROM users WHERE id = ?", userID).Scan(&disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRevoked
		}
		return err
	}
	if disabled.Valid {
		return ErrUserDisabled
	}
	return nil
}
