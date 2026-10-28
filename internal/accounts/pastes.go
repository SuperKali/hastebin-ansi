package accounts

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Visibility of a registered paste
const (
	VisibilityLink    = 0 // anyone with the link
	VisibilityPrivate = 1 // the owner only
)

var (
	// ErrPasteLimit is returned when a user has as many pastes as allowed
	ErrPasteLimit = errors.New("paste limit reached")

	// ErrStorageLimit is returned when a paste does not fit in what a user may store
	ErrStorageLimit = errors.New("storage limit reached")

	// ErrRevoked is returned when the session or token an upload came with
	// was removed before the upload was recorded
	ErrRevoked = errors.New("session or token revoked")

	// ErrUserDisabled is returned when the account of an upload was
	// disabled before the upload was recorded
	ErrUserDisabled = errors.New("account disabled")
)

// Paste is the metadata of a registered paste. The content is in the store.
type Paste struct {
	Key          string
	UserID       int64
	Title        string
	Language     string
	Visibility   int
	CreatedAt    time.Time
	ExpiresAt    time.Time // zero for never
	Size         int64
	Views        int64
	LastOpenedAt time.Time // zero for never
	SeenAt       time.Time // zero while no one else opened it
	EditedAt     time.Time // zero for never
	EditCount    int
	RootKey      string
	Version      int
	PrevKey      string // "" for none
	NextKey      string // the newer version, "" for none
	Tags         []string
}

// pasteColumns are read by scanPaste. The tags come as one comma separated
// string: a tag never holds a comma. The newer version is looked up by
// prev_key only: left to itself the planner may walk all of a user's pastes
// for each row instead.
const pasteColumns = `pastes.key, pastes.user_id, pastes.title, pastes.language, pastes.visibility, pastes.created_at,
	pastes.expires_at, pastes.size, pastes.views, pastes.last_opened_at, pastes.seen_at, pastes.edited_at,
	pastes.edit_count, pastes.root_key, pastes.version, pastes.prev_key,
	(SELECT n.key FROM pastes AS n INDEXED BY pastes_prev WHERE n.prev_key = pastes.key AND n.user_id = pastes.user_id
		ORDER BY n.created_at, n.key LIMIT 1),
	(SELECT group_concat(tag, ',') FROM (SELECT tag FROM paste_tags WHERE paste_key = pastes.key ORDER BY tag))`

func scanPaste(row interface{ Scan(...any) error }) (*Paste, error) {
	var p Paste
	var created int64
	var expires, opened, seen, edited sql.NullInt64
	var prev, next, tags sql.NullString
	err := row.Scan(&p.Key, &p.UserID, &p.Title, &p.Language, &p.Visibility, &created, &expires, &p.Size, &p.Views,
		&opened, &seen, &edited, &p.EditCount, &p.RootKey, &p.Version, &prev, &next, &tags)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.ExpiresAt = fromUnix(expires)
	p.LastOpenedAt = fromUnix(opened)
	p.SeenAt = fromUnix(seen)
	p.EditedAt = fromUnix(edited)
	p.PrevKey = prev.String
	p.NextKey = next.String
	p.Tags = []string{}
	if tags.String != "" {
		p.Tags = strings.Split(tags.String, ",")
	}
	return &p, nil
}

// NewPaste is a registered paste to record, once its content is stored
type NewPaste struct {
	Key        string
	UserID     int64
	Title      string
	Language   string
	Visibility int
	ExpiresAt  time.Time // zero for never
	Size       int64
	Tags       []string // normalized already

	// PrevKey makes the paste the next version of that paste of the same
	// user: it joins its chain (root_key) one past the newest version. A
	// PrevKey that is gone, or someone else's, is ErrNotFound.
	PrevKey string

	// DeletePrevious deletes PrevKey in the same transaction and queues its
	// content, revisions included
	DeletePrevious bool

	// TokenID or SessionHash name what authenticated the upload; when set it
	// must still exist when the row is written, else ErrRevoked
	TokenID     int64
	SessionHash []byte
}

// Limits are what one user may keep. 0 or less means no limit.
type Limits struct {
	MaxPastes int64
	MaxBytes  int64
}

// usage returns how many pastes a user has and the bytes they and their
// revisions take
func usage(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID int64) (count, bytes int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM pastes WHERE user_id = ?),
		(SELECT COALESCE(SUM(size), 0) FROM pastes WHERE user_id = ?) +
		(SELECT COALESCE(SUM(r.size), 0) FROM paste_revisions r JOIN pastes p ON p.key = r.paste_key WHERE p.user_id = ?)`,
		userID, userID, userID).Scan(&count, &bytes)
	return count, bytes, err
}

// Usage returns how many pastes a user has and how many bytes they take,
// revisions included
func (db *DB) Usage(ctx context.Context, userID int64) (count, bytes int64, err error) {
	return usage(ctx, db.R, userID)
}

// InsertPaste records a paste whose content is stored already. Within the
// same transaction it checks that the token or session of the upload still
// exists (ErrRevoked), that the account is not disabled (ErrUserDisabled),
// and the limits of the user: ErrPasteLimit or ErrStorageLimit when the
// paste does not fit, and ErrNotFound for a PrevKey that is gone. Then
// nothing is recorded.
func (db *DB) InsertPaste(ctx context.Context, p NewPaste, limits Limits) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := checkUploader(ctx, tx, p.UserID, p.TokenID, p.SessionHash); err != nil {
		return err
	}

	count, bytes, err := usage(ctx, tx, p.UserID)
	if err != nil {
		return err
	}
	if limits.MaxPastes > 0 && count >= limits.MaxPastes {
		return ErrPasteLimit
	}
	if limits.MaxBytes > 0 && bytes+p.Size > limits.MaxBytes {
		return ErrStorageLimit
	}

	var expires, prev any
	if !p.ExpiresAt.IsZero() {
		expires = unix(p.ExpiresAt)
	}
	root, version := p.Key, 1
	if p.PrevKey != "" {
		prev = p.PrevKey
		err := tx.QueryRowContext(ctx, "SELECT root_key FROM pastes WHERE key = ? AND user_id = ?", p.PrevKey, p.UserID).Scan(&root)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT MAX(version) + 1 FROM pastes WHERE user_id = ? AND root_key = ?",
			p.UserID, root).Scan(&version); err != nil {
			return err
		}
	}
	now := unix(db.now())
	_, err = tx.ExecContext(ctx,
		`INSERT INTO pastes (key, user_id, title, language, visibility, created_at, expires_at, size, root_key, version, prev_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Key, p.UserID, p.Title, p.Language, p.Visibility, now, expires, p.Size, root, version, prev)
	if err != nil {
		return err
	}
	if err := setTags(ctx, tx, p.Key, p.Tags); err != nil {
		return err
	}
	if p.PrevKey != "" && p.DeletePrevious {
		if _, err := deletePaste(ctx, tx, p.PrevKey, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// setTags replaces the tags of a paste
func setTags(ctx context.Context, x execer, key string, tags []string) error {
	if _, err := x.ExecContext(ctx, "DELETE FROM paste_tags WHERE paste_key = ?", key); err != nil {
		return err
	}
	for _, tag := range tags {
		if _, err := x.ExecContext(ctx, "INSERT OR IGNORE INTO paste_tags (paste_key, tag) VALUES (?, ?)", key, tag); err != nil {
			return err
		}
	}
	return nil
}

// PasteByKey returns a paste of a user. A paste of another user, or none,
// is ErrNotFound.
func (db *DB) PasteByKey(ctx context.Context, userID int64, key string) (*Paste, error) {
	return scanPaste(db.R.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE key = ? AND user_id = ?", key, userID))
}

// ListQuery selects and orders the pastes of a user
type ListQuery struct {
	// Q matches the title, key or language, case insensitive. "" matches all.
	Q string

	// Visibility is VisibilityLink or VisibilityPrivate, -1 for both
	Visibility int

	// Language matches exactly, "" for any
	Language string

	// Tags must all be on a paste
	Tags []string

	// HasVersions keeps only pastes with (true) or without (false) other
	// versions, nil for both
	HasVersions *bool

	// Sort is one of the Sorts keys
	Sort string

	// Page counts from 1, PerPage rows each
	Page    int
	PerPage int
}

// Sorts are the orders a list can have: a field, with "-" in front for
// the largest or newest first. Each has an index on (user_id, field, ...)
// so a page is read in index order, see migration 0004_indexes.sql.
var Sorts = map[string]string{
	"-created":     "pastes.created_at DESC, pastes.key DESC",
	"created":      "pastes.created_at ASC, pastes.key ASC",
	"title":        "pastes.title COLLATE NOCASE ASC, pastes.created_at DESC, pastes.key DESC",
	"-title":       "pastes.title COLLATE NOCASE DESC, pastes.created_at DESC, pastes.key DESC",
	"-size":        "pastes.size DESC, pastes.created_at DESC, pastes.key DESC",
	"size":         "pastes.size ASC, pastes.created_at DESC, pastes.key DESC",
	"-views":       "pastes.views DESC, pastes.created_at DESC, pastes.key DESC",
	"views":        "pastes.views ASC, pastes.created_at DESC, pastes.key DESC",
	"-last_opened": "pastes.last_opened_at IS NULL, pastes.last_opened_at DESC, pastes.created_at DESC, pastes.key DESC",
	"last_opened":  "pastes.last_opened_at IS NULL, pastes.last_opened_at ASC, pastes.created_at DESC, pastes.key DESC",
}

// likeEscaper escapes the wildcards of a LIKE pattern, with \ as the escape
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ListPastes returns one page of the pastes of a user that match q, and how
// many match in all. It reads on L within ListTimeout: a search walks all
// of a user's pastes, so it must never hold the connections the access
// gate and the logins read on.
func (db *DB) ListPastes(ctx context.Context, userID int64, q ListQuery) ([]*Paste, int64, error) {
	ctx, cancel := listContext(ctx)
	defer cancel()
	count, page, args := listSQL(userID, q)
	var total int64
	if err := db.L.QueryRowContext(ctx, count, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	perPage := max(q.PerPage, 1)
	offset := (max(q.Page, 1) - 1) * perPage
	rows, err := db.L.QueryContext(ctx, page, append(args, perPage, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := []*Paste{}
	for rows.Next() {
		p, err := scanPaste(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, p)
	}
	return list, total, rows.Err()
}

// listSQL builds the queries of ListPastes: the count, and the page, which
// takes args then LIMIT and OFFSET. The page is picked on the bare rows
// first, in the order of an index; the columns that cost a lookup each, the
// newer version and the tags, are read for that page alone.
func listSQL(userID int64, q ListQuery) (count, page string, args []any) {
	order, ok := Sorts[q.Sort]
	if !ok {
		order = Sorts["-created"]
	}
	where := []string{"pastes.user_id = ?"}
	args = []any{userID}
	if q.Q != "" {
		pattern := "%" + likeEscaper.Replace(q.Q) + "%"
		where = append(where, `(pastes.title LIKE ? ESCAPE '\' OR pastes.key LIKE ? ESCAPE '\' OR pastes.language LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern, pattern)
	}
	if q.Visibility >= 0 {
		where = append(where, "pastes.visibility = ?")
		args = append(args, q.Visibility)
	}
	if q.Language != "" {
		where = append(where, "pastes.language = ?")
		args = append(args, q.Language)
	}
	for _, tag := range q.Tags {
		where = append(where, "EXISTS (SELECT 1 FROM paste_tags t WHERE t.paste_key = pastes.key AND t.tag = ?)")
		args = append(args, tag)
	}
	if q.HasVersions != nil {
		versions := "(pastes.version > 1 OR EXISTS (SELECT 1 FROM pastes n WHERE n.prev_key = pastes.key))"
		if !*q.HasVersions {
			versions = "NOT " + versions
		}
		where = append(where, versions)
	}
	cond := strings.Join(where, " AND ")
	count = "SELECT COUNT(*) FROM pastes WHERE " + cond
	page = "SELECT " + pasteColumns +
		" FROM (SELECT key AS page_key FROM pastes WHERE " + cond + " ORDER BY " + order + " LIMIT ? OFFSET ?) page" +
		" JOIN pastes ON pastes.key = page.page_key ORDER BY " + order
	return count, page, args
}

// EachPaste calls fn for every paste of a user, ordered by key, reading
// pageSize rows at a time so no connection is held while fn runs. Each
// page is a costly read on L, see ListPastes.
func (db *DB) EachPaste(ctx context.Context, userID int64, pageSize int, fn func(*Paste) error) error {
	after := ""
	for {
		page, err := db.pasteAfter(ctx, userID, after, pageSize)
		if err != nil {
			return err
		}
		for _, p := range page {
			if err := fn(p); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
		after = page[len(page)-1].Key
	}
}

// pasteAfter reads the next page of EachPaste
func (db *DB) pasteAfter(ctx context.Context, userID int64, after string, pageSize int) ([]*Paste, error) {
	ctx, cancel := listContext(ctx)
	defer cancel()
	rows, err := db.L.QueryContext(ctx,
		"SELECT "+pasteColumns+" FROM pastes WHERE user_id = ? AND key > ? ORDER BY key LIMIT ?", userID, after, pageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []*Paste
	for rows.Next() {
		p, err := scanPaste(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, p)
	}
	return page, rows.Err()
}

// Revision is an older content of a paste
type Revision struct {
	Rev       int
	Size      int64
	CreatedAt time.Time
}

// Revisions returns the older contents of a paste, oldest first. The caller
// checks that the paste is the user's.
func (db *DB) Revisions(ctx context.Context, key string) ([]Revision, error) {
	rows, err := db.R.QueryContext(ctx, "SELECT rev, size, created_at FROM paste_revisions WHERE paste_key = ? ORDER BY rev", key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Revision{}
	for rows.Next() {
		var r Revision
		var created int64
		if err := rows.Scan(&r.Rev, &r.Size, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		list = append(list, r)
	}
	return list, rows.Err()
}

// VersionRef is one paste in a chain of versions
type VersionRef struct {
	Key       string
	Version   int
	CreatedAt time.Time
}

// Versions returns the pastes of a user that share a root, oldest first,
// at most 100
func (db *DB) Versions(ctx context.Context, userID int64, rootKey string) ([]VersionRef, error) {
	rows, err := db.R.QueryContext(ctx,
		"SELECT key, version, created_at FROM pastes WHERE user_id = ? AND root_key = ? ORDER BY version, created_at LIMIT 100",
		userID, rootKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []VersionRef{}
	for rows.Next() {
		var v VersionRef
		var created int64
		if err := rows.Scan(&v.Key, &v.Version, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(created, 0).UTC()
		list = append(list, v)
	}
	return list, rows.Err()
}

// PastePatch changes the metadata of a paste. Nil fields stay as they are.
type PastePatch struct {
	Title      *string
	Language   *string
	Visibility *int
	// ExpiresAt sets the end of the paste, the zero time for never
	ExpiresAt *time.Time
	Tags      *[]string
}

// UpdatePaste changes the metadata of a paste of a user and returns it. A
// paste of another user, or none, is ErrNotFound.
func (db *DB) UpdatePaste(ctx context.Context, userID int64, key string, patch PastePatch) (*Paste, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var sets []string
	var args []any
	if patch.Title != nil {
		sets, args = append(sets, "title = ?"), append(args, *patch.Title)
	}
	if patch.Language != nil {
		sets, args = append(sets, "language = ?"), append(args, *patch.Language)
	}
	if patch.Visibility != nil {
		sets, args = append(sets, "visibility = ?"), append(args, *patch.Visibility)
	}
	if patch.ExpiresAt != nil {
		var expires any
		if !patch.ExpiresAt.IsZero() {
			expires = unix(*patch.ExpiresAt)
		}
		sets, args = append(sets, "expires_at = ?"), append(args, expires)
	}
	// An empty patch still checks that the paste is the user's
	if len(sets) == 0 {
		sets = append(sets, "key = key")
	}
	res, err := tx.ExecContext(ctx, "UPDATE pastes SET "+strings.Join(sets, ", ")+" WHERE key = ? AND user_id = ?",
		append(args, key, userID)...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if patch.Tags != nil {
		if err := setTags(ctx, tx, key, *patch.Tags); err != nil {
			return nil, err
		}
	}
	p, err := scanPaste(tx.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE key = ?", key))
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// DeletePastes deletes the pastes of a user among keys in one transaction
// and queues their content, revisions included, for deletion from the
// store. Keys that are not the user's, or do not exist, come back in
// notFound, in the order given.
func (db *DB) DeletePastes(ctx context.Context, userID int64, keys []string) (deleted, notFound []string, err error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	now := unix(db.now())
	deleted, notFound = []string{}, []string{}
	for _, key := range keys {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM pastes WHERE key = ? AND user_id = ?", key, userID).Scan(&n); err != nil {
			return nil, nil, err
		}
		if n == 0 {
			notFound = append(notFound, key)
			continue
		}
		if _, err := deletePaste(ctx, tx, key, now); err != nil {
			return nil, nil, err
		}
		deleted = append(deleted, key)
	}
	return deleted, notFound, tx.Commit()
}

// TagCount is a tag and how many pastes of a user carry it
type TagCount struct {
	Tag   string `json:"tag"`
	Count int64  `json:"count"`
}

// Tags returns the tags of a user's pastes with their counts, the most used
// first, at most limit. It is a costly read on L, see ListPastes.
func (db *DB) Tags(ctx context.Context, userID int64, limit int) ([]TagCount, error) {
	ctx, cancel := listContext(ctx)
	defer cancel()
	rows, err := db.L.QueryContext(ctx,
		`SELECT t.tag, COUNT(*) AS n FROM paste_tags t JOIN pastes p ON p.key = t.paste_key
		WHERE p.user_id = ? GROUP BY t.tag ORDER BY n DESC, t.tag LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []TagCount{}
	for rows.Next() {
		var t TagCount
		if err := rows.Scan(&t.Tag, &t.Count); err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// Languages returns the languages set on a user's pastes, at most limit.
// It is a costly read on L, see ListPastes.
func (db *DB) Languages(ctx context.Context, userID int64, limit int) ([]string, error) {
	ctx, cancel := listContext(ctx)
	defer cancel()
	rows, err := db.L.QueryContext(ctx,
		"SELECT DISTINCT language FROM pastes WHERE user_id = ? AND language != '' ORDER BY language LIMIT ?", userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []string{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		list = append(list, l)
	}
	return list, rows.Err()
}

// EachStoreKey calls fn with the store key of every registered paste and
// revision, the content that must never expire
func (db *DB) EachStoreKey(ctx context.Context, fn func(key string)) error {
	rows, err := db.R.QueryContext(ctx, "SELECT key FROM pastes UNION ALL SELECT store_key FROM paste_revisions")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return err
		}
		fn(key)
	}
	return rows.Err()
}

// SetDefaultVisibility sets the visibility new pastes of a user get
func (db *DB) SetDefaultVisibility(ctx context.Context, userID int64, visibility int) error {
	res, err := db.W.ExecContext(ctx, "UPDATE users SET default_visibility = ? WHERE id = ?", visibility, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser deletes an account: its sessions, tokens and paste rows, with
// every store key of its pastes and revisions queued for deletion, in short
// transactions, see DeleteUserBy. It records the deletion in the audit log and returns how
// many store keys it queued.
func (db *DB) DeleteUser(ctx context.Context, user *User) (int64, error) {
	return db.DeleteUserBy(ctx, user, AuditEntry{
		ActorID: user.ID, ActorLogin: user.Login, Action: "account.delete", Target: userTarget(user.ID),
	})
}

// deleteUserChunk is how many pastes one transaction of DeleteUserBy
// deletes. A transaction is linear in its pastes; chunks keep each one short
// whatever USER_MAX_PASTES allows, so other writers never wait past
// busy_timeout.
var deleteUserChunk = 250

// deleteUserChunkDone, when set, is called after each chunk of
// DeleteUserBy commits, with how many pastes it deleted. Tests set it.
var deleteUserChunkDone func(deleted int)

// DeleteUserBy is DeleteUser with the audit row given: an admin deleting
// someone else's account. It runs in steps, each its own short transaction:
// first the sessions and tokens go, so nothing adds a paste meanwhile; then
// the pastes, deleteUserChunk at a time, each through deletePaste, so its
// key reads as missing from the moment its row is gone; last the user row
// with whatever pastes are left and the audit row. When a step fails the
// account is still there, logged out, with the pastes not deleted yet; the
// same call finishes it.
func (db *DB) DeleteUserBy(ctx context.Context, user *User, entry AuditEntry) (int64, error) {
	if err := db.revokeAll(ctx, user.ID); err != nil {
		return 0, err
	}
	var queued int64
	for {
		n, q, err := db.deleteUserPastes(ctx, user.ID)
		queued += q
		if err != nil {
			return queued, err
		}
		if deleteUserChunkDone != nil {
			deleteUserChunkDone(n)
		}
		if n < deleteUserChunk {
			break
		}
	}

	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return queued, err
	}
	defer tx.Rollback()
	// A paste recorded by an upload that passed its checks before the
	// tokens went; normally none
	_, q, err := deletePastesOf(ctx, tx, user.ID, -1, unix(db.now()))
	if err != nil {
		return queued, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", user.ID)
	if err != nil {
		return queued, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return queued, ErrNotFound
	}
	if err := audit(ctx, tx, db.now(), entry); err != nil {
		return queued, err
	}
	if err := tx.Commit(); err != nil {
		return queued, err
	}
	return queued + q, nil
}

// revokeAll deletes the sessions and tokens of a user. A user that does not
// exist is ErrNotFound.
func (db *DB) revokeAll(ctx context.Context, userID int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id = ?", userID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	for _, query := range []string{"DELETE FROM sessions WHERE user_id = ?", "DELETE FROM api_tokens WHERE user_id = ?"} {
		if _, err := tx.ExecContext(ctx, query, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// deleteUserPastes deletes up to deleteUserChunk pastes of a user in one
// transaction. It returns how many it deleted and how many store keys it
// queued.
func (db *DB) deleteUserPastes(ctx context.Context, userID int64) (int, int64, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	n, queued, err := deletePastesOf(ctx, tx, userID, deleteUserChunk, unix(db.now()))
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return n, queued, nil
}

// deletePastesOf deletes up to limit pastes of a user inside tx, all of
// them when limit is negative, each through deletePaste. It returns how
// many it deleted and how many store keys it queued.
func deletePastesOf(ctx context.Context, tx *sql.Tx, userID int64, limit int, now int64) (int, int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT key FROM pastes WHERE user_id = ? LIMIT ?", userID, limit)
	if err != nil {
		return 0, 0, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, 0, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
	}
	var queued int64
	for _, key := range keys {
		q, err := deletePaste(ctx, tx, key, now)
		if err != nil {
			return 0, 0, err
		}
		queued += q
	}
	return len(keys), queued, nil
}
