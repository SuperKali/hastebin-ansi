package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/dashboard"
	"github.com/go-chi/chi/v5"
)

// maxUserQuery is the longest user search, in bytes
const maxUserQuery = 100

// UserItem is an account in the admin API
type UserItem struct {
	ID             int64      `json:"id"`
	GitHubID       int64      `json:"github_id"`
	Login          string     `json:"login"`
	AvatarURL      *string    `json:"avatar_url"`
	CreatedAt      time.Time  `json:"created_at"`
	LastLoginAt    *time.Time `json:"last_login_at"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	DisabledAt     *time.Time `json:"disabled_at"`
	DisabledReason string     `json:"disabled_reason"`
	AbuseStrikes   int        `json:"abuse_strikes"`
	Pastes         int64      `json:"pastes"`
	Bytes          int64      `json:"bytes"`

	// Admin is set for an account in ADMIN_GITHUB_IDS: it cannot be
	// disabled, revoked or deleted here
	Admin bool `json:"admin"`
}

func (h *Handler) userItem(row accounts.UserRow) UserItem {
	u := row.User
	return UserItem{
		ID:             u.ID,
		GitHubID:       u.GitHubID,
		Login:          u.Login,
		AvatarURL:      avatar(u.AvatarURL),
		CreatedAt:      u.CreatedAt,
		LastLoginAt:    timePtr(u.LastLoginAt),
		LastSeenAt:     timePtr(u.LastSeenAt),
		DisabledAt:     timePtr(u.DisabledAt),
		DisabledReason: u.DisabledReason,
		AbuseStrikes:   u.AbuseStrikes,
		Pastes:         row.Pastes,
		Bytes:          row.Bytes,
		Admin:          h.Auth.IsAdmin(u.GitHubID),
	}
}

// users answers GET /api/v1/admin/users?q=&sort=created|last_seen|bytes|pastes|login&page=
func (h *Handler) users(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	q := accounts.UserQuery{Q: strings.TrimSpace(values.Get("q")), Sort: "created", PerPage: PerPage}
	if len(q.Q) > maxUserQuery {
		writeError(w, http.StatusBadRequest, "invalid_input", "The search text is too long.")
		return
	}
	if sort := values.Get("sort"); sort != "" {
		if _, ok := accounts.UserSort[sort]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_input", "Unknown sort. Use created, last_seen, bytes, pastes or login.")
			return
		}
		q.Sort = sort
	}
	page, ok := pageParam(w, r)
	if !ok {
		return
	}
	q.Page = page
	list, total, err := h.DB.ListUsers(r.Context(), q)
	if err != nil {
		writeDBError(w, err)
		return
	}
	items := make([]UserItem, 0, len(list))
	for _, row := range list {
		items = append(items, h.userItem(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "pages": pages(total), "total": total})
}

// PasteItem is the metadata of a registered paste in the admin API. Its
// content is never part of it.
type PasteItem struct {
	Key          string     `json:"key"`
	Title        string     `json:"title"`
	Language     string     `json:"language"`
	Visibility   string     `json:"visibility"`
	Size         int64      `json:"size"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Views        int64      `json:"views"`
	LastOpenedAt *time.Time `json:"last_opened_at"`
	Version      int        `json:"version"`
}

// userID is the account of a /users/{id} route, 0 when it is no id
func userID(r *http.Request) int64 {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// user answers GET /api/v1/admin/users/{id}?page=: the account, its
// usage, and one page of the metadata of its pastes, newest first
func (h *Handler) user(w http.ResponseWriter, r *http.Request) {
	id := userID(r)
	if id == 0 {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	page, ok := pageParam(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	d, err := h.DB.UserDetailByID(ctx, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	list, total, err := h.DB.ListPastes(ctx, id, accounts.ListQuery{Visibility: -1, Sort: "-created", Page: page, PerPage: PerPage})
	if err != nil {
		writeDBError(w, err)
		return
	}
	items := make([]PasteItem, 0, len(list))
	for _, p := range list {
		items = append(items, PasteItem{
			Key: p.Key, Title: p.Title, Language: p.Language, Visibility: dashboard.VisibilityName(p.Visibility),
			Size: p.Size, CreatedAt: p.CreatedAt, ExpiresAt: timePtr(p.ExpiresAt), Views: p.Views,
			LastOpenedAt: timePtr(p.LastOpenedAt), Version: p.Version,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		UserItem
		Sessions int64          `json:"sessions"`
		Tokens   int64          `json:"tokens"`
		Pastes   map[string]any `json:"paste_list"`
	}{
		UserItem: h.userItem(d.UserRow),
		Sessions: d.Sessions,
		Tokens:   d.Tokens,
		Pastes:   map[string]any{"items": items, "page": page, "pages": pages(total), "total": total},
	})
}

// target returns the account of a /users/{id} route for an action on it.
// It answers 404 for none and 403 protected for an admin, the one acting
// included, and returns nil then.
func (h *Handler) target(w http.ResponseWriter, r *http.Request) *accounts.User {
	id := userID(r)
	if id == 0 {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return nil
	}
	u, err := h.DB.UserByID(r.Context(), id)
	if err != nil {
		writeDBError(w, err)
		return nil
	}
	if h.Auth.IsAdmin(u.GitHubID) || u.ID == admin(r).ID {
		writeError(w, http.StatusForbidden, "protected", "Admin accounts cannot be changed from the admin page.")
		return nil
	}
	return u
}

// disable answers POST /api/v1/admin/users/{id}/disable {reason}: every
// session and token of the account stops working at its next request, an
// upload in progress included, until it is enabled again
func (h *Handler) disable(w http.ResponseWriter, r *http.Request) {
	h.setDisabled(w, r, true)
}

// enable answers POST /api/v1/admin/users/{id}/enable {reason}
func (h *Handler) enable(w http.ResponseWriter, r *http.Request) {
	h.setDisabled(w, r, false)
}

func (h *Handler) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	u := h.target(w, r)
	if u == nil {
		return
	}
	reason, ok := reasonBody(w, r)
	if !ok {
		return
	}
	action := "user.enable"
	if disabled {
		action = "user.disable"
	}
	err := h.DB.SetDisabledBy(r.Context(), u.ID, disabled, reason,
		entry(r, action, accounts.UserTarget(u.ID), map[string]any{"reason": reason, "login": u.Login}))
	if err != nil {
		writeDBError(w, err)
		return
	}
	logAction(r, action).Int64("user_id", u.ID).Msg("Admin changed an account")
	h.writeUser(w, r, u.ID)
}

// revoke answers POST /api/v1/admin/users/{id}/revoke {reason}: every
// session and token of the account is deleted
func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	u := h.target(w, r)
	if u == nil {
		return
	}
	reason, ok := reasonBody(w, r)
	if !ok {
		return
	}
	sessions, tokens, err := h.DB.RevokeAll(r.Context(), u.ID,
		entry(r, "user.revoke", accounts.UserTarget(u.ID), map[string]any{"reason": reason, "login": u.Login}))
	if err != nil {
		writeDBError(w, err)
		return
	}
	logAction(r, "user.revoke").Int64("user_id", u.ID).Int64("sessions", sessions).Int64("tokens", tokens).
		Msg("Admin revoked the sessions and tokens of an account")
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "tokens": tokens})
}

// deleteUser answers DELETE /api/v1/admin/users/{id} {confirm, reason}:
// the same deletion as the user's own, confirmed by typing the login
func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	u := h.target(w, r)
	if u == nil {
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
		Reason  string `json:"reason"`
	}
	if !decodeBody(w, r, &in, `The body must be {"confirm": "<login>", "reason": "..."}.`) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(in.Confirm), u.Login) {
		writeError(w, http.StatusBadRequest, "invalid_input", "Type the login of the account to confirm.")
		return
	}
	reason, err := cleanReason(in.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	queued, err := h.DB.DeleteUserBy(r.Context(), u,
		entry(r, "user.delete", accounts.UserTarget(u.ID), map[string]any{"reason": reason, "login": u.Login, "github_id": u.GitHubID}))
	if err != nil {
		writeDBError(w, err)
		return
	}
	h.Drainer.Kick()
	logAction(r, "user.delete").Int64("user_id", u.ID).Int64("queued", queued).Msg("Admin deleted an account")
	writeJSON(w, http.StatusAccepted, map[string]any{"deleted": true, "queued": queued})
}

// writeUser answers with the account as the list shows it
func (h *Handler) writeUser(w http.ResponseWriter, r *http.Request, id int64) {
	d, err := h.DB.UserDetailByID(r.Context(), id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.userItem(d.UserRow))
}
