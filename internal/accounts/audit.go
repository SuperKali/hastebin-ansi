package accounts

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// AuditEntry is one row of the append-only audit log. ActorID and
// ActorLogin are the admin who acted, or the user for actions users take
// on their own account (stored in admin_user_id and admin_login).
type AuditEntry struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	ActorID    int64     `json:"actor_id"`
	ActorLogin string    `json:"actor_login"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	Detail     string    `json:"detail"`
}

// execer runs a statement on a handle or in a transaction
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Audit appends an entry to the audit log. Rows can never be changed or
// deleted: triggers abort any UPDATE or DELETE.
func (db *DB) Audit(ctx context.Context, entry AuditEntry) error {
	return audit(ctx, db.W, db.now(), entry)
}

func audit(ctx context.Context, x execer, now time.Time, entry AuditEntry) error {
	var actor any
	if entry.ActorID != 0 {
		actor = entry.ActorID
	}
	_, err := x.ExecContext(ctx,
		"INSERT INTO audit_log (at, admin_user_id, admin_login, action, target, detail) VALUES (?, ?, ?, ?, ?, ?)",
		unix(now), actor, entry.ActorLogin, entry.Action, entry.Target, entry.Detail)
	return err
}

// AuditLog returns up to limit entries, newest first, skipping offset. It
// is a costly read on L, see ListPastes.
func (db *DB) AuditLog(ctx context.Context, limit, offset int) ([]AuditEntry, error) {
	ctx, cancel := listContext(ctx)
	defer cancel()
	rows, err := db.L.QueryContext(ctx,
		"SELECT id, at, admin_user_id, admin_login, action, target, detail FROM audit_log ORDER BY id DESC LIMIT ? OFFSET ?",
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var actor sql.NullInt64
		if err := rows.Scan(&e.ID, &at, &actor, &e.ActorLogin, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		e.ActorID = actor.Int64
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// userTarget is the audit target naming a user
func userTarget(id int64) string {
	return "user:" + strconv.FormatInt(id, 10)
}
