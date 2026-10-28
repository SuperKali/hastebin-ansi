// Package dashboard serves the API of the dashboard, where registered users
// manage their own pastes, tokens and account, and tells who owns an upload.
// Every query is scoped to the user of the request: a paste or token of
// someone else answers exactly like one that does not exist.
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/abuse"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/rs/zerolog/log"
)

const (
	// PerPage is how many pastes one page of the list holds
	PerPage = 25

	// MaxBulkDelete is how many keys one bulk delete may name
	MaxBulkDelete = 100

	// MaxTagList is how many tags GET /api/v1/tags returns at most
	MaxTagList = 200

	// maxLanguageList is how many languages GET /api/v1/tags returns at most
	maxLanguageList = 100

	// maxQuery is the longest search text, in bytes
	maxQuery = 200

	// maxPage is the highest page number accepted
	maxPage = 100000

	// maxBody caps the JSON body of every route here; a bulk delete of 100 keys fits
	maxBody = 16 << 10

	// exportPage is how many rows an export reads at a time
	exportPage = 500
)

// Per user rate limits, on top of the per client ones
const (
	readLimit         = 120
	readWindow        = time.Minute
	writeLimit        = 30
	writeWindow       = time.Minute
	tokenCreateLimit  = 10
	tokenCreateWindow = time.Hour
	deleteLimit       = 3
	deleteWindow      = 24 * time.Hour

	// UploadLimit is how many owned uploads to /documents and /log one
	// account may send a minute, with a token or a login; the same as MCP
	UploadLimit  = 120
	uploadWindow = time.Minute
)

// Limits are what one registered user may keep and send. 0 or less means
// no limit.
type Limits struct {
	MaxPastes   int64
	MaxBytes    int64
	MaxLength   int   // largest owned paste
	UploadQuota int64 // bytes per hour
	MaxTokens   int
}

// Handler serves the dashboard API and resolves the owner of uploads
type Handler struct {
	Auth   *auth.Service
	DB     *accounts.DB
	Limits Limits

	// Drainer deletes the store objects of deleted pastes; nil leaves them queued
	Drainer *accounts.Drainer

	// Events records the rate limited requests for the admin page; nil records nothing
	Events *abuse.Events

	// Docs stores the content of edits, new versions and duplicates, with
	// the upload limits of /documents. Locks is the key lock the Gate takes
	// to end the edit window, so an edit and that read never overlap.
	Docs  *handler.DocumentHandler
	Locks *KeyLocks

	// EditWindow is how long after creation the owner may edit a paste
	// under the same link, until someone else opens it; 0 means never.
	// MaxRevisions is how many older contents one paste keeps, 0 for no limit.
	EditWindow   time.Duration
	MaxRevisions int

	// now is the clock, replaced in tests
	now func() time.Time

	// recordEdit records an edit in the database, replaced in tests
	recordEdit func(ctx context.Context, e accounts.Edit, limits accounts.Limits) (*accounts.Paste, error)

	reads, writes, tokenCreates, deletes func(http.Handler) http.Handler

	// uploads limits the owned uploads of each account, see allowUpload
	uploads *httprate.RateLimiter
}

// New returns the handler of the dashboard API
func New(svc *auth.Service, db *accounts.DB, limits Limits, drainer *accounts.Drainer) *Handler {
	d := &Handler{Auth: svc, DB: db, Limits: limits, Drainer: drainer, now: time.Now, recordEdit: db.RecordEdit}
	d.reads = UserLimit(readLimit, readWindow, d.limited)
	d.writes = UserLimit(writeLimit, writeWindow, d.limited)
	d.tokenCreates = UserLimit(tokenCreateLimit, tokenCreateWindow, d.limited)
	d.deletes = UserLimit(deleteLimit, deleteWindow, d.limited)
	d.uploads = httprate.NewRateLimiter(UploadLimit, uploadWindow,
		httprate.WithResponseHeaders(httprate.ResponseHeaders{RetryAfter: "Retry-After"}))
	return d
}

// limited records a request refused by a per user limit
func (d *Handler) limited(r *http.Request) {
	d.Events.Record(abuse.KindRateLimit, handler.ClientKey(handler.ClientIP(r)),
		"user "+strconv.FormatInt(auth.FromContext(r.Context()).User.ID, 10))
}

// UserLimit allows each user limit requests per window, counted by user id
// whatever address they come from, and calls refused, when set, for each
// request it refuses. It needs RequireSession first.
func UserLimit(limit int, window time.Duration, refused func(*http.Request)) func(http.Handler) http.Handler {
	return httprate.LimitBy(limit, window, func(r *http.Request) (string, error) {
		return "u:" + strconv.FormatInt(auth.FromContext(r.Context()).User.ID, 10), nil
	}, httprate.WithLimitHandler(func(w http.ResponseWriter, r *http.Request) {
		if refused != nil {
			refused(r)
		}
		WriteError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests, try again in a moment.")
	}))
}

// byMethod sends reads (GET, HEAD) through one limiter and every other method through another
func byMethod(reads, writes func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		r, w := reads(next), writes(next)
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet || req.Method == http.MethodHead {
				r.ServeHTTP(rw, req)
				return
			}
			w.ServeHTTP(rw, req)
		})
	}
}

// sessionOnly refuses API tokens: they are for uploads and MCP, and the
// dashboard API, which can create tokens and delete the account, needs a
// login session with its CSRF token
func sessionOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.HasBearer(r) {
			WriteError(w, http.StatusUnauthorized, "unauthenticated",
				"API tokens work for uploads and MCP only. The dashboard API needs a login session.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RegisterRoutes adds the dashboard API. Every route needs a session; every
// state change also the CSRF token, a JSON body and, when sent, the
// server's own Origin.
func (d *Handler) RegisterRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(sessionOnly, d.Auth.LoadSession, d.Auth.RequireSession, d.Auth.RequireCSRF, byMethod(d.reads, d.writes))

		r.Get("/api/v1/pastes", d.handleList)
		r.Post("/api/v1/pastes/delete", d.handleBulkDelete)
		r.Get("/api/v1/pastes/{key}", d.handleGet)
		r.Patch("/api/v1/pastes/{key}", d.handlePatch)
		r.Delete("/api/v1/pastes/{key}", d.handleDelete)
		r.Get("/api/v1/tags", d.handleTags)
		r.Post("/api/v1/pastes/{key}/duplicate", d.handleDuplicate)
		r.Get("/api/v1/pastes/{key}/revisions/{n}", d.handleRevision)

		r.Get("/api/v1/tokens", d.handleTokens)
		r.With(d.tokenCreates).Post("/api/v1/tokens", d.handleCreateToken)
		r.Delete("/api/v1/tokens/{id}", d.handleDeleteToken)

		r.Get("/api/v1/account", d.handleAccount)
		r.Patch("/api/v1/account", d.handlePatchAccount)
		r.Get("/api/v1/account/export", d.handleExport)
		r.With(d.deletes).Delete("/api/v1/account", d.handleDeleteAccount)
	})

	// The body of these is the content of a paste, of any type, received
	// like an upload to /documents
	r.Group(func(r chi.Router) {
		r.Use(handler.NoStore, sessionOnly, d.Auth.LoadSession, d.Auth.RequireSession, d.Auth.RequireCSRFRaw, d.writes)

		r.Put("/api/v1/pastes/{key}/content", d.handleEditContent)
		r.Post("/api/v1/pastes/{key}/versions", d.handleNewVersion)
	})
}

// WriteError answers with the JSON error of the /api/v1 routes
func WriteError(w http.ResponseWriter, status int, code, message string) {
	auth.WriteError(w, status, code, message)
}

// writeJSON answers status with v as JSON, never cached
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeDBError answers a failed database call: 404 for a missing row, 503
// when the database stayed busy, 500 otherwise
func writeDBError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
	case accounts.IsBusy(err):
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "db_busy", "The server is busy, try again shortly.")
	default:
		log.Error().Err(err).Msg("Dashboard database call failed")
		WriteError(w, http.StatusInternalServerError, "internal", "Something went wrong, try again.")
	}
}

// decodeBody decodes a JSON body of at most maxBody bytes into v. Unknown
// fields are refused. It answers 400 and returns false on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, help string) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_input", help)
		return false
	}
	return true
}

// user is the account of the request; the session middlewares ran already
func user(r *http.Request) *accounts.User {
	return auth.FromContext(r.Context()).User
}

// timePtr is t for JSON, nil (null) for the zero time
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// stringPtr is s for JSON, nil (null) when empty
func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Item is a paste in the API
type Item struct {
	Key           string     `json:"key"`
	Title         string     `json:"title"`
	Language      string     `json:"language"`
	Visibility    string     `json:"visibility"`
	Size          int64      `json:"size"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	EditedAt      *time.Time `json:"edited_at"`
	EditCount     int        `json:"edit_count"`
	Views         int64      `json:"views"`
	LastOpenedAt  *time.Time `json:"last_opened_at"`
	Version       int        `json:"version"`
	RootKey       string     `json:"root_key"`
	PrevKey       *string    `json:"prev_key"`
	NextKey       *string    `json:"next_key"`
	EditableUntil *time.Time `json:"editable_until"`
	Tags          []string   `json:"tags"`
}

// item is the API form of a paste
func (d *Handler) item(p *accounts.Paste) Item {
	return Item{
		EditableUntil: timePtr(d.editableUntil(p, d.now())),
		Key:           p.Key,
		Title:         p.Title,
		Language:      p.Language,
		Visibility:    VisibilityName(p.Visibility),
		Size:          p.Size,
		CreatedAt:     p.CreatedAt,
		ExpiresAt:     timePtr(p.ExpiresAt),
		EditedAt:      timePtr(p.EditedAt),
		EditCount:     p.EditCount,
		Views:         p.Views,
		LastOpenedAt:  timePtr(p.LastOpenedAt),
		Version:       p.Version,
		RootKey:       p.RootKey,
		PrevKey:       stringPtr(p.PrevKey),
		NextKey:       stringPtr(p.NextKey),
		Tags:          p.Tags,
	}
}

// parseListQuery reads the query string of GET /api/v1/pastes
func parseListQuery(r *http.Request) (accounts.ListQuery, string) {
	values := r.URL.Query()
	q := accounts.ListQuery{Visibility: -1, Page: 1, PerPage: PerPage, Sort: "-created"}

	q.Q = strings.TrimSpace(values.Get("q"))
	if len(q.Q) > maxQuery {
		return q, "The search text is too long."
	}
	if v := values.Get("visibility"); v != "" {
		vis, err := ParseVisibility(v)
		if err != nil {
			return q, err.Error()
		}
		q.Visibility = vis
	}
	if v := values.Get("language"); v != "" {
		lang, err := NormalizeLanguage(v)
		if err != nil {
			return q, err.Error()
		}
		q.Language = lang
	}
	if tags := values["tag"]; len(tags) > 0 {
		if len(tags) > MaxTags {
			return q, "Filter by at most 10 tags."
		}
		norm, err := NormalizeTags(tags)
		if err != nil {
			return q, err.Error()
		}
		q.Tags = norm
	}
	if v := values.Get("has_versions"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return q, "has_versions must be true or false."
		}
		q.HasVersions = &b
	}
	if v := values.Get("sort"); v != "" {
		if _, ok := accounts.Sorts[v]; !ok {
			return q, "Unknown sort. Use created, title, size, views or last_opened, with - in front for the other direction."
		}
		q.Sort = v
	}
	if v := values.Get("page"); v != "" {
		page, err := strconv.Atoi(v)
		if err != nil || page < 1 || page > maxPage {
			return q, "page must be a number from 1."
		}
		q.Page = page
	}
	return q, ""
}

// handleList answers GET /api/v1/pastes: one page of the user's pastes
func (d *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	q, msg := parseListQuery(r)
	if msg != "" {
		WriteError(w, http.StatusBadRequest, "invalid_input", msg)
		return
	}
	list, total, err := d.DB.ListPastes(r.Context(), user(r).ID, q)
	if err != nil {
		writeDBError(w, err)
		return
	}
	items := make([]Item, 0, len(list))
	for _, p := range list {
		items = append(items, d.item(p))
	}
	pages := max((total+PerPage-1)/PerPage, 1)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": q.Page, "pages": pages, "total": total})
}

// pasteKey is the key of a paste route
func pasteKey(r *http.Request) string {
	return chi.URLParam(r, "key")
}

// handleGet answers GET /api/v1/pastes/{key}: the paste with its revisions,
// the chain of its versions, the server time to count the edit window down
// from, the window length, why editing under the same link is over (seen,
// expired, null while open) and how many revisions a paste may keep
func (d *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := d.now()
	u := user(r)
	p, err := d.DB.PasteByKey(ctx, u.ID, pasteKey(r))
	if err != nil {
		writeDBError(w, err)
		return
	}
	revisions, err := d.DB.Revisions(ctx, p.Key)
	if err != nil {
		writeDBError(w, err)
		return
	}
	versions, err := d.DB.Versions(ctx, u.ID, p.RootKey)
	if err != nil {
		writeDBError(w, err)
		return
	}
	type revision struct {
		Rev       int       `json:"rev"`
		Size      int64     `json:"size"`
		CreatedAt time.Time `json:"created_at"`
	}
	type version struct {
		Key       string    `json:"key"`
		Version   int       `json:"version"`
		CreatedAt time.Time `json:"created_at"`
	}
	out := struct {
		Item
		Revisions    []revision `json:"revisions"`
		Versions     []version  `json:"versions"`
		ServerNow    time.Time  `json:"server_now"`
		EditWindow   int64      `json:"edit_window"`
		EditClosed   *string    `json:"edit_closed"`
		MaxRevisions int        `json:"max_revisions"`
	}{Item: d.item(p), Revisions: []revision{}, Versions: []version{}, ServerNow: now.UTC().Truncate(time.Second),
		EditWindow: int64(d.EditWindow / time.Second), EditClosed: stringPtr(d.closedReason(p, now)), MaxRevisions: d.MaxRevisions}
	for _, rev := range revisions {
		out.Revisions = append(out.Revisions, revision{rev.Rev, rev.Size, rev.CreatedAt})
	}
	for _, v := range versions {
		out.Versions = append(out.Versions, version{v.Key, v.Version, v.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// patchHelp is the 400 message of a malformed PATCH /api/v1/pastes/{key}
const patchHelp = `The body must be JSON with any of title, language, visibility ("link" or "private"), expires_at (RFC 3339 or null) and tags.`

// handlePatch answers PATCH /api/v1/pastes/{key}: title, language,
// visibility, expiry and tags. Fields left out stay as they are.
func (d *Handler) handlePatch(w http.ResponseWriter, r *http.Request) {
	var in map[string]json.RawMessage
	if !decodeBody(w, r, &in, patchHelp) {
		return
	}
	var patch accounts.PastePatch
	for name, raw := range in {
		var err error
		switch name {
		case "title":
			var s string
			if err = json.Unmarshal(raw, &s); err == nil {
				if s, err = NormalizeTitle(s); err == nil {
					patch.Title = &s
				}
			}
		case "language":
			var s string
			if err = json.Unmarshal(raw, &s); err == nil {
				if s, err = NormalizeLanguage(s); err == nil {
					patch.Language = &s
				}
			}
		case "visibility":
			var s string
			if err = json.Unmarshal(raw, &s); err == nil {
				var v int
				if v, err = ParseVisibility(s); err == nil {
					patch.Visibility = &v
				}
			}
		case "expires_at":
			var s *string
			if err = json.Unmarshal(raw, &s); err == nil {
				t := time.Time{}
				if s != nil {
					t, err = ParseExpiry(*s, d.now())
				}
				if err == nil {
					patch.ExpiresAt = &t
				}
			}
		case "tags":
			var tags []string
			if err = json.Unmarshal(raw, &tags); err == nil {
				if tags, err = NormalizeTags(tags); err == nil {
					patch.Tags = &tags
				}
			}
		default:
			err = errors.New(patchHelp)
		}
		if err != nil {
			var typeErr *json.UnmarshalTypeError
			var syntaxErr *json.SyntaxError
			if errors.As(err, &typeErr) || errors.As(err, &syntaxErr) {
				err = errors.New(patchHelp)
			}
			WriteError(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
	}
	p, err := d.DB.UpdatePaste(r.Context(), user(r).ID, pasteKey(r), patch)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d.item(p))
}

// handleDelete answers DELETE /api/v1/pastes/{key}: the row goes at once,
// the content soon after through the delete queue
func (d *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	deleted, _, err := d.DB.DeletePastes(r.Context(), user(r).ID, []string{pasteKey(r)})
	if err != nil {
		writeDBError(w, err)
		return
	}
	if len(deleted) == 0 {
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	d.Drainer.Kick()
	log.Info().Int64("user_id", user(r).ID).Int("deleted", 1).Msg("Pastes deleted by their owner")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleBulkDelete answers POST /api/v1/pastes/delete {keys}: up to 100 keys;
// those that are not the user's come back in not_found, like missing ones
func (d *Handler) handleBulkDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Keys []string `json:"keys"`
	}
	if !decodeBody(w, r, &in, `The body must be {"keys": ["key", ...]}.`) {
		return
	}
	seen := map[string]bool{}
	keys := make([]string, 0, len(in.Keys))
	for _, key := range in.Keys {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 || len(keys) > MaxBulkDelete {
		WriteError(w, http.StatusBadRequest, "invalid_input", "Name 1 to 100 keys.")
		return
	}
	deleted, notFound, err := d.DB.DeletePastes(r.Context(), user(r).ID, keys)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if len(deleted) > 0 {
		d.Drainer.Kick()
		log.Info().Int64("user_id", user(r).ID).Int("deleted", len(deleted)).Msg("Pastes deleted by their owner")
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "not_found": notFound})
}

// handleTags answers GET /api/v1/tags: the user's tags with how many
// pastes carry each, the most used first, and the languages set on them
func (d *Handler) handleTags(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	tags, err := d.DB.Tags(r.Context(), u.ID, MaxTagList)
	if err != nil {
		writeDBError(w, err)
		return
	}
	languages, err := d.DB.Languages(r.Context(), u.ID, maxLanguageList)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tags, "languages": languages})
}
