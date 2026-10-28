package accounts

import (
	"context"
	"time"
)

// Session is a login. Only the SHA-256 of its cookie is stored.
type Session struct {
	IDHash     []byte
	UserID     int64
	CSRF       []byte
	CreatedAt  time.Time
	AuthAt     time.Time // when the user last went through GitHub
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// CreateSession stores a new session of a user, valid until expires
func (db *DB) CreateSession(ctx context.Context, idHash []byte, userID int64, csrf []byte, expires time.Time) error {
	now := unix(db.now())
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO sessions (id_hash, user_id, csrf, created_at, auth_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		idHash, userID, csrf, now, now, now, unix(expires))
	return err
}

// SessionByHash returns a session and its user, ErrNotFound when the session
// does not exist. It does not check expiry or the user's state.
func (db *DB) SessionByHash(ctx context.Context, idHash []byte) (*Session, *User, error) {
	var s Session
	var created, authAt, seen, expires int64
	user, err := scanUser(db.R.QueryRowContext(ctx,
		`SELECT `+userColumns+`, sessions.csrf, sessions.created_at, sessions.auth_at, sessions.last_seen_at, sessions.expires_at
		FROM sessions JOIN users ON users.id = sessions.user_id WHERE sessions.id_hash = ?`, idHash),
		&s.CSRF, &created, &authAt, &seen, &expires)
	if err != nil {
		return nil, nil, err
	}
	s.IDHash = idHash
	s.UserID = user.ID
	s.CreatedAt = time.Unix(created, 0).UTC()
	s.AuthAt = time.Unix(authAt, 0).UTC()
	s.LastSeenAt = time.Unix(seen, 0).UTC()
	s.ExpiresAt = time.Unix(expires, 0).UTC()
	return &s, user, nil
}

// TouchSession records that a session and its user were just used
func (db *DB) TouchSession(ctx context.Context, idHash []byte, userID int64) error {
	now := unix(db.now())
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?", now, idHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET last_seen_at = ? WHERE id = ?", now, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSession removes one session. A missing one is not an error.
func (db *DB) DeleteSession(ctx context.Context, idHash []byte) error {
	_, err := db.W.ExecContext(ctx, "DELETE FROM sessions WHERE id_hash = ?", idHash)
	return err
}

// LogoutAll removes every session of a user and, with revokeTokens, every
// API token too, and records it in the audit log, all in one transaction.
// It returns how many sessions and tokens it removed.
func (db *DB) LogoutAll(ctx context.Context, user *User, revokeTokens bool) (sessions, tokens int64, err error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", user.ID)
	if err != nil {
		return 0, 0, err
	}
	sessions, _ = res.RowsAffected()
	if revokeTokens {
		res, err := tx.ExecContext(ctx, "DELETE FROM api_tokens WHERE user_id = ?", user.ID)
		if err != nil {
			return 0, 0, err
		}
		tokens, _ = res.RowsAffected()
	}
	detail := `{"revoke_tokens":false}`
	if revokeTokens {
		detail = `{"revoke_tokens":true}`
	}
	if err := audit(ctx, tx, db.now(), AuditEntry{
		ActorID: user.ID, ActorLogin: user.Login, Action: "session.logout_all", Target: userTarget(user.ID), Detail: detail,
	}); err != nil {
		return 0, 0, err
	}
	return sessions, tokens, tx.Commit()
}

// DeleteStaleSessions removes the sessions past their end, or unused for
// longer than idle, and returns how many it removed
func (db *DB) DeleteStaleSessions(ctx context.Context, idle time.Duration) (int64, error) {
	now := db.now()
	res, err := db.W.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?",
		unix(now), unix(now.Add(-idle)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
