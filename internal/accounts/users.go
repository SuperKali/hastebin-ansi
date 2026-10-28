package accounts

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound is returned when a row does not exist
var ErrNotFound = errors.New("not found")

// User is a registered account. GitHubID is the identity; Login is only for
// display and follows renames on GitHub at each login.
type User struct {
	ID                int64
	GitHubID          int64
	Login             string
	CreatedAt         time.Time
	LastLoginAt       time.Time
	LastSeenAt        time.Time
	DisabledAt        time.Time // zero while enabled
	DisabledReason    string
	DefaultVisibility int
	AbuseStrikes      int

	// AvatarURL is the GitHub avatar, checked at login, "" when none
	AvatarURL string
}

// Disabled reports whether the account is disabled
func (u *User) Disabled() bool {
	return !u.DisabledAt.IsZero()
}

const userColumns = `users.id, users.github_id, users.login, users.created_at, users.last_login_at, users.last_seen_at,
	users.disabled_at, users.disabled_reason, users.default_visibility, users.abuse_strikes, users.avatar_url`

// scanUser reads the userColumns of a row
func scanUser(row interface{ Scan(...any) error }, extra ...any) (*User, error) {
	var u User
	var created int64
	var lastLogin, lastSeen, disabled sql.NullInt64
	var reason, avatar sql.NullString
	dest := append([]any{&u.ID, &u.GitHubID, &u.Login, &created, &lastLogin, &lastSeen,
		&disabled, &reason, &u.DefaultVisibility, &u.AbuseStrikes, &avatar}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	u.LastLoginAt = fromUnix(lastLogin)
	u.LastSeenAt = fromUnix(lastSeen)
	u.DisabledAt = fromUnix(disabled)
	u.DisabledReason = reason.String
	u.AvatarURL = avatar.String
	return &u, nil
}

// UserByGitHubID returns the account of a GitHub id, ErrNotFound when none
func (db *DB) UserByGitHubID(ctx context.Context, githubID int64) (*User, error) {
	return scanUser(db.R.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE github_id = ?", githubID))
}

// UserByID returns an account, ErrNotFound when none
func (db *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(db.R.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", id))
}

// LoginUser records a login of a GitHub account: it creates the account when
// the GitHub id has none and allowCreate is true, else it refreshes the
// login name, the avatar and the login time. created tells which happened. A
// GitHub id without an account and allowCreate false gives ErrNotFound. The
// caller checks avatarURL; "" stores NULL.
func (db *DB) LoginUser(ctx context.Context, githubID int64, login, avatarURL string, allowCreate bool) (user *User, created bool, err error) {
	now := unix(db.now())
	avatar := sql.NullString{String: avatarURL, Valid: avatarURL != ""}
	res, err := db.W.ExecContext(ctx,
		"UPDATE users SET login = ?, avatar_url = ?, last_login_at = ?, last_seen_at = ? WHERE github_id = ?",
		login, avatar, now, now, githubID)
	if err != nil {
		return nil, false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, false, err
	} else if n == 0 {
		if !allowCreate {
			return nil, false, ErrNotFound
		}
		_, err := db.W.ExecContext(ctx,
			`INSERT INTO users (github_id, login, avatar_url, created_at, last_login_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (github_id) DO UPDATE SET login = excluded.login, avatar_url = excluded.avatar_url,
				last_login_at = excluded.last_login_at, last_seen_at = excluded.last_seen_at`,
			githubID, login, avatar, now, now, now)
		if err != nil {
			return nil, false, err
		}
		created = true
	}
	user, err = scanUser(db.W.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE github_id = ?", githubID))
	return user, created, err
}

// SetDisabled disables an account with a reason, or enables it again
func (db *DB) SetDisabled(ctx context.Context, userID int64, disabled bool, reason string) error {
	var at any
	var why any
	if disabled {
		at, why = unix(db.now()), reason
	}
	res, err := db.W.ExecContext(ctx, "UPDATE users SET disabled_at = ?, disabled_reason = ? WHERE id = ?", at, why, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Totals are the counts the admin status shows
type Totals struct {
	Users         int64 `json:"users"`
	Pastes        int64 `json:"pastes"`
	PasteBytes    int64 `json:"paste_bytes"`
	Revisions     int64 `json:"revisions"`
	RevisionBytes int64 `json:"revision_bytes"`
	Sessions      int64 `json:"sessions"`
	Tokens        int64 `json:"tokens"`
}

// Totals counts users, registered pastes and their bytes, revisions, sessions and tokens
func (db *DB) Totals(ctx context.Context) (Totals, error) {
	var t Totals
	err := db.R.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM pastes), (SELECT COALESCE(SUM(size), 0) FROM pastes),
		(SELECT COUNT(*) FROM paste_revisions), (SELECT COALESCE(SUM(size), 0) FROM paste_revisions),
		(SELECT COUNT(*) FROM sessions), (SELECT COUNT(*) FROM api_tokens)`).
		Scan(&t.Users, &t.Pastes, &t.PasteBytes, &t.Revisions, &t.RevisionBytes, &t.Sessions, &t.Tokens)
	return t, err
}
