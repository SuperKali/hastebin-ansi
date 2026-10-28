package accounts

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// What the admin API reads and changes. Every change writes its audit row
// in the same transaction, so a change without its row never commits.

// PasteAny returns a registered paste whoever owns it, ErrNotFound when the
// key has no row. Only the admin API may call it.
func (db *DB) PasteAny(ctx context.Context, key string) (*Paste, error) {
	return scanPaste(db.R.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE key = ?", key))
}

// Tombstone is what is left of a paste a moderator removed for abuse
type Tombstone struct {
	Key        string
	Reason     string
	Visibility int
	RemovedAt  time.Time
	AdminID    int64
}

// TombstoneOf returns the tombstone of a key, ErrNotFound when it has none
func (db *DB) TombstoneOf(ctx context.Context, key string) (*Tombstone, error) {
	var t Tombstone
	var removed int64
	var admin sql.NullInt64
	err := db.R.QueryRowContext(ctx,
		"SELECT key, reason, visibility, removed_at, admin_user_id FROM tombstones WHERE key = ?", key).
		Scan(&t.Key, &t.Reason, &t.Visibility, &removed, &admin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.RemovedAt = time.Unix(removed, 0).UTC()
	t.AdminID = admin.Int64
	return &t, nil
}

// Moderation is the outcome of RemovePaste
type Moderation struct {
	// OwnerID is the account that owned the paste, 0 for an anonymous one
	OwnerID int64

	// Visibility is what the paste was: VisibilityLink for an anonymous one
	Visibility int
}

// RemovePaste deletes a paste for a moderator in one transaction: its row,
// if it has one, goes and its content and revisions are queued for
// deletion from the store; an anonymous paste only has its content queued.
// With abuse, a tombstone keeps the key answering 410 (404 when it was
// private) and the owner gets a strike. entry is the audit row, written in
// the same transaction.
func (db *DB) RemovePaste(ctx context.Context, key string, abuse bool, reason string, entry AuditEntry) (*Moderation, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := unix(db.now())
	m := &Moderation{Visibility: VisibilityLink}
	var owner sql.NullInt64
	var visibility sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT user_id, visibility FROM pastes WHERE key = ?", key).Scan(&owner, &visibility)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO delete_queue (store_key, enqueued_at) VALUES (?, ?) ON CONFLICT (store_key) DO NOTHING", key, now); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		m.OwnerID, m.Visibility = owner.Int64, int(visibility.Int64)
		if _, err := deletePaste(ctx, tx, key, now); err != nil {
			return nil, err
		}
	}

	if abuse {
		var admin any
		if entry.ActorID != 0 {
			admin = entry.ActorID
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tombstones (key, reason, visibility, removed_at, admin_user_id) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET reason = excluded.reason, visibility = excluded.visibility,
				removed_at = excluded.removed_at, admin_user_id = excluded.admin_user_id`,
			key, reason, m.Visibility, now, admin); err != nil {
			return nil, err
		}
		if m.OwnerID != 0 {
			if _, err := tx.ExecContext(ctx, "UPDATE users SET abuse_strikes = abuse_strikes + 1 WHERE id = ?", m.OwnerID); err != nil {
				return nil, err
			}
		}
	}
	if err := audit(ctx, tx, db.now(), entry); err != nil {
		return nil, err
	}
	return m, tx.Commit()
}

// SetDisabledBy disables an account with a reason, or enables it again, and
// writes entry to the audit log in the same transaction. A disabled
// account's sessions and tokens stop working at their next request.
func (db *DB) SetDisabledBy(ctx context.Context, userID int64, disabled bool, reason string, entry AuditEntry) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var at, why any
	if disabled {
		at, why = unix(db.now()), reason
	}
	res, err := tx.ExecContext(ctx, "UPDATE users SET disabled_at = ?, disabled_reason = ? WHERE id = ?", at, why, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := audit(ctx, tx, db.now(), entry); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeAll removes every session and token of an account and writes entry
// to the audit log, in one transaction. It returns how many it removed.
func (db *DB) RevokeAll(ctx context.Context, userID int64, entry AuditEntry) (sessions, tokens int64, err error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = ?", userID).Scan(&n); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, ErrNotFound
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID)
	if err != nil {
		return 0, 0, err
	}
	sessions, _ = res.RowsAffected()
	res, err = tx.ExecContext(ctx, "DELETE FROM api_tokens WHERE user_id = ?", userID)
	if err != nil {
		return 0, 0, err
	}
	tokens, _ = res.RowsAffected()
	if err := audit(ctx, tx, db.now(), entry); err != nil {
		return 0, 0, err
	}
	return sessions, tokens, tx.Commit()
}

// UserSort are the orders of the admin user list, each the largest or
// newest first, except login
var UserSort = map[string]string{
	"created":   "users.created_at DESC, users.id DESC",
	"last_seen": "users.last_seen_at IS NULL, users.last_seen_at DESC, users.id DESC",
	"bytes":     "n_bytes DESC, users.id DESC",
	"pastes":    "n_pastes DESC, users.id DESC",
	"login":     "users.login COLLATE NOCASE ASC, users.id ASC",
}

// UserQuery selects and orders accounts for the admin user list
type UserQuery struct {
	// Q matches the login, case insensitive; a number also matches the id
	// or the GitHub id exactly. "" matches all.
	Q string

	// Sort is one of the UserSort keys
	Sort string

	// Page counts from 1, PerPage rows each
	Page    int
	PerPage int
}

// UserRow is an account with how much it stores
type UserRow struct {
	*User
	Pastes int64
	Bytes  int64 // revisions included
}

// userUsage are the columns UserRow adds to userColumns
const userUsage = `(SELECT COUNT(*) FROM pastes p WHERE p.user_id = users.id) AS n_pastes,
	(SELECT COALESCE(SUM(p.size), 0) FROM pastes p WHERE p.user_id = users.id) +
	(SELECT COALESCE(SUM(r.size), 0) FROM paste_revisions r JOIN pastes p ON p.key = r.paste_key WHERE p.user_id = users.id) AS n_bytes`

// ListUsers returns one page of the accounts that match q, and how many
// match in all. It is a costly read on L, see ListPastes.
func (db *DB) ListUsers(ctx context.Context, q UserQuery) ([]UserRow, int64, error) {
	ctx, cancel := listContext(ctx)
	defer cancel()
	order, ok := UserSort[q.Sort]
	if !ok {
		order = UserSort["created"]
	}
	cond, args := "1 = 1", []any{}
	if q.Q != "" {
		cond = `users.login LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscaper.Replace(q.Q)+"%")
		if id, err := strconv.ParseInt(q.Q, 10, 64); err == nil {
			cond += " OR users.id = ? OR users.github_id = ?"
			args = append(args, id, id)
		}
	}
	var total int64
	if err := db.L.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE "+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	perPage := max(q.PerPage, 1)
	offset := (max(q.Page, 1) - 1) * perPage
	rows, err := db.L.QueryContext(ctx,
		"SELECT "+userColumns+", "+userUsage+" FROM users WHERE "+cond+" ORDER BY "+order+" LIMIT ? OFFSET ?",
		append(args, perPage, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := []UserRow{}
	for rows.Next() {
		var row UserRow
		u, err := scanUser(rows, &row.Pastes, &row.Bytes)
		if err != nil {
			return nil, 0, err
		}
		row.User = u
		list = append(list, row)
	}
	return list, total, rows.Err()
}

// UserDetail is an account with its usage and how many ways in it has
type UserDetail struct {
	UserRow
	Sessions int64
	Tokens   int64
}

// UserDetailByID returns an account with its usage, sessions and tokens,
// ErrNotFound when none
func (db *DB) UserDetailByID(ctx context.Context, id int64) (*UserDetail, error) {
	var d UserDetail
	u, err := scanUser(db.R.QueryRowContext(ctx,
		"SELECT "+userColumns+", "+userUsage+`,
			(SELECT COUNT(*) FROM sessions s WHERE s.user_id = users.id),
			(SELECT COUNT(*) FROM api_tokens t WHERE t.user_id = users.id)
		FROM users WHERE users.id = ?`, id),
		&d.Pastes, &d.Bytes, &d.Sessions, &d.Tokens)
	if err != nil {
		return nil, err
	}
	d.User = u
	return &d, nil
}

// IPBan is a manual ban of a client, persisted so it survives restarts
type IPBan struct {
	// Client is an IPv4 address or an IPv6 /64, in handler.ClientKey form
	Client    string
	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time // zero for never
	AdminID   int64
}

// Bans returns up to limit manual bans that have not expired at now, the
// newest first
func (db *DB) Bans(ctx context.Context, now time.Time, limit int) ([]IPBan, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT client_key, reason, created_at, expires_at, admin_user_id FROM ip_bans
		WHERE expires_at IS NULL OR expires_at > ? ORDER BY created_at DESC, client_key LIMIT ?`, unix(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []IPBan{}
	for rows.Next() {
		var b IPBan
		var created int64
		var expires, admin sql.NullInt64
		if err := rows.Scan(&b.Client, &b.Reason, &created, &expires, &admin); err != nil {
			return nil, err
		}
		b.CreatedAt = time.Unix(created, 0).UTC()
		b.ExpiresAt = fromUnix(expires)
		b.AdminID = admin.Int64
		list = append(list, b)
	}
	return list, rows.Err()
}

// DeleteExpiredBans removes the manual bans over at now and returns how many
func (db *DB) DeleteExpiredBans(ctx context.Context, now time.Time) (int64, error) {
	res, err := db.W.ExecContext(ctx, "DELETE FROM ip_bans WHERE expires_at IS NOT NULL AND expires_at <= ?", unix(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PutBan adds a manual ban, or replaces the one of the same client, and
// writes entry to the audit log in the same transaction. A new ban past
// limit live bans gives ErrTooMany; limit <= 0 means no limit.
func (db *DB) PutBan(ctx context.Context, ban IPBan, limit int, entry AuditEntry) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if limit > 0 {
		var n int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM ip_bans WHERE client_key != ? AND (expires_at IS NULL OR expires_at > ?)",
			ban.Client, unix(db.now())).Scan(&n); err != nil {
			return err
		}
		if n >= limit {
			return ErrTooMany
		}
	}
	var expires, admin any
	if !ban.ExpiresAt.IsZero() {
		expires = unix(ban.ExpiresAt)
	}
	if ban.AdminID != 0 {
		admin = ban.AdminID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ip_bans (client_key, reason, created_at, expires_at, admin_user_id) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (client_key) DO UPDATE SET reason = excluded.reason, created_at = excluded.created_at,
			expires_at = excluded.expires_at, admin_user_id = excluded.admin_user_id`,
		ban.Client, ban.Reason, unix(ban.CreatedAt), expires, admin); err != nil {
		return err
	}
	if err := audit(ctx, tx, db.now(), entry); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteBan removes the manual ban of a client and writes entry to the
// audit log in the same transaction. No ban is ErrNotFound.
func (db *DB) DeleteBan(ctx context.Context, client string, entry AuditEntry) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM ip_bans WHERE client_key = ?", client)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := audit(ctx, tx, db.now(), entry); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditCount returns how many entries the audit log holds
func (db *DB) AuditCount(ctx context.Context) (int64, error) {
	var n int64
	err := db.R.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log").Scan(&n)
	return n, err
}

// PasteTarget is the audit target naming a paste
func PasteTarget(key string) string {
	return "paste:" + key
}

// UserTarget is the audit target naming a user
func UserTarget(id int64) string {
	return userTarget(id)
}

// BanTarget is the audit target naming a banned client of a kind
func BanTarget(kind, client string) string {
	return "ban:" + kind + ":" + strings.TrimSpace(client)
}
