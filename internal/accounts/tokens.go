package accounts

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrTooMany is returned when a user already has as many rows of a kind as allowed
var ErrTooMany = errors.New("limit reached")

// Token is a personal API token. Only the SHA-256 of the token and a short
// prefix to recognise it are stored, never the token itself.
type Token struct {
	ID         int64
	UserID     int64
	Kind       string
	Name       string
	Prefix     string
	Scope      string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero when never used
	ExpiresAt  time.Time // zero for no end
}

const tokenColumns = `api_tokens.id, api_tokens.user_id, api_tokens.kind, api_tokens.name, api_tokens.prefix,
	api_tokens.scope, api_tokens.created_at, api_tokens.last_used_at, api_tokens.expires_at`

func scanToken(row interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var created int64
	var used, expires sql.NullInt64
	if err := row.Scan(&t.ID, &t.UserID, &t.Kind, &t.Name, &t.Prefix, &t.Scope, &created, &used, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	t.LastUsedAt = fromUnix(used)
	t.ExpiresAt = fromUnix(expires)
	return &t, nil
}

// CreateToken stores a new personal token of a user by its hash and display
// prefix. A user who has limit tokens already gets ErrTooMany; limit <= 0 means
// no limit. The count and the insert run in one transaction.
func (db *DB) CreateToken(ctx context.Context, userID int64, name, prefix string, hash []byte, limit int) (*Token, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if limit > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_tokens WHERE user_id = ?", userID).Scan(&n); err != nil {
			return nil, err
		}
		if n >= limit {
			return nil, ErrTooMany
		}
	}
	now := unix(db.now())
	res, err := tx.ExecContext(ctx,
		"INSERT INTO api_tokens (user_id, name, prefix, hash, created_at) VALUES (?, ?, ?, ?, ?)",
		userID, name, prefix, hash, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	token, err := scanToken(tx.QueryRowContext(ctx, "SELECT "+tokenColumns+" FROM api_tokens WHERE id = ?", id))
	if err != nil {
		return nil, err
	}
	return token, tx.Commit()
}

// Tokens returns the tokens of a user, newest first
func (db *DB) Tokens(ctx context.Context, userID int64) ([]*Token, error) {
	rows, err := db.R.QueryContext(ctx,
		"SELECT "+tokenColumns+" FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC, id DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// CountTokens returns how many tokens a user has
func (db *DB) CountTokens(ctx context.Context, userID int64) (int, error) {
	var n int
	err := db.R.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_tokens WHERE user_id = ?", userID).Scan(&n)
	return n, err
}

// DeleteToken revokes a token of a user. A token of another user, or none,
// is ErrNotFound.
func (db *DB) DeleteToken(ctx context.Context, userID, id int64) error {
	res, err := db.W.ExecContext(ctx, "DELETE FROM api_tokens WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TokenByHash returns a token and its user, ErrNotFound when no token has
// that hash. It does not check expiry or the user's state.
func (db *DB) TokenByHash(ctx context.Context, hash []byte) (*Token, *User, error) {
	var t Token
	var created int64
	var used, expires sql.NullInt64
	user, err := scanUser(db.R.QueryRowContext(ctx,
		`SELECT `+userColumns+`, api_tokens.id, api_tokens.kind, api_tokens.name, api_tokens.prefix, api_tokens.scope,
			api_tokens.created_at, api_tokens.last_used_at, api_tokens.expires_at
		FROM api_tokens JOIN users ON users.id = api_tokens.user_id WHERE api_tokens.hash = ?`, hash),
		&t.ID, &t.Kind, &t.Name, &t.Prefix, &t.Scope, &created, &used, &expires)
	if err != nil {
		return nil, nil, err
	}
	t.UserID = user.ID
	t.CreatedAt = time.Unix(created, 0).UTC()
	t.LastUsedAt = fromUnix(used)
	t.ExpiresAt = fromUnix(expires)
	return &t, user, nil
}

// TouchToken records that a token was just used, and its user
func (db *DB) TouchToken(ctx context.Context, id, userID int64) error {
	now := unix(db.now())
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE api_tokens SET last_used_at = ? WHERE id = ?", now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET last_seen_at = ? WHERE id = ?", now, userID); err != nil {
		return err
	}
	return tx.Commit()
}
