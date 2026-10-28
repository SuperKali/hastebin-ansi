package dashboard

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

// TokenItem is a personal token in the API. The token itself is never
// shown again after it is created.
type TokenItem struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func tokenItem(t *accounts.Token) TokenItem {
	return TokenItem{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt, LastUsedAt: timePtr(t.LastUsedAt)}
}

// handleTokens answers GET /api/v1/tokens
func (d *Handler) handleTokens(w http.ResponseWriter, r *http.Request) {
	list, err := d.DB.Tokens(r.Context(), user(r).ID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	items := make([]TokenItem, 0, len(list))
	for _, t := range list {
		items = append(items, tokenItem(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "max": d.Limits.MaxTokens})
}

// handleCreateToken answers POST /api/v1/tokens {name}: the new token, the
// only time it is shown
func (d *Handler) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &in, `The body must be {"name": "..."}.`) {
		return
	}
	name, err := cleanText(in.Name, MaxTokenName, "The name")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if name == "" {
		name = "token"
	}
	u := user(r)
	token := auth.NewToken()
	t, err := d.DB.CreateToken(r.Context(), u.ID, name, auth.TokenDisplay(token), auth.HashToken(token), d.Limits.MaxTokens)
	if errors.Is(err, accounts.ErrTooMany) {
		WriteError(w, http.StatusConflict, "limit_reached",
			"You have the most tokens allowed ("+strconv.Itoa(d.Limits.MaxTokens)+"). Revoke one first.")
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	log.Info().Int64("user_id", u.ID).Int64("token_id", t.ID).Str("prefix", t.Prefix).Msg("API token created")
	writeJSON(w, http.StatusCreated, struct {
		TokenItem
		Token string `json:"token"`
	}{tokenItem(t), token})
}

// handleDeleteToken answers DELETE /api/v1/tokens/{id}: the token stops
// working at once, uploads in progress with it included
func (d *Handler) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	u := user(r)
	if err := d.DB.DeleteToken(r.Context(), u.ID, id); err != nil {
		writeDBError(w, err)
		return
	}
	log.Info().Int64("user_id", u.ID).Int64("token_id", id).Msg("API token revoked")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// accountInfo is the account in the API: who, since when, the settings,
// what it uses and what it may use (0 for no limit)
func (d *Handler) accountInfo(r *http.Request, u *accounts.User) (map[string]any, error) {
	ctx := r.Context()
	count, bytes, err := d.DB.Usage(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	tokens, err := d.DB.CountTokens(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":                 u.ID,
		"login":              u.Login,
		"github_id":          u.GitHubID,
		"created_at":         u.CreatedAt,
		"last_login_at":      timePtr(u.LastLoginAt),
		"default_visibility": VisibilityName(u.DefaultVisibility),
		"usage":              map[string]any{"pastes": count, "bytes": bytes, "tokens": tokens},
		"limits": map[string]any{
			"max_pastes":   max(d.Limits.MaxPastes, 0),
			"storage":      max(d.Limits.MaxBytes, 0),
			"max_length":   max(d.Limits.MaxLength, 0),
			"upload_quota": max(d.Limits.UploadQuota, 0),
			"max_tokens":   max(d.Limits.MaxTokens, 0),
		},
	}, nil
}

// handleAccount answers GET /api/v1/account
func (d *Handler) handleAccount(w http.ResponseWriter, r *http.Request) {
	info, err := d.accountInfo(r, user(r))
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handlePatchAccount answers PATCH /api/v1/account {default_visibility}
func (d *Handler) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DefaultVisibility *string `json:"default_visibility"`
	}
	const help = `The body must be {"default_visibility": "link" or "private"}.`
	if !decodeBody(w, r, &in, help) {
		return
	}
	u := user(r)
	if in.DefaultVisibility != nil {
		v, err := ParseVisibility(*in.DefaultVisibility)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "invalid_input", help)
			return
		}
		if err := d.DB.SetDefaultVisibility(r.Context(), u.ID, v); err != nil {
			writeDBError(w, err)
			return
		}
		updated := *u
		updated.DefaultVisibility = v
		u = &updated
	}
	info, err := d.accountInfo(r, u)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleExport answers GET /api/v1/account/export: the account, its tokens
// and the metadata of every paste as one JSON file, written as it is read.
// Paste contents are not in it.
func (d *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := user(r)
	info, err := d.accountInfo(r, u)
	if err != nil {
		writeDBError(w, err)
		return
	}
	tokens, err := d.DB.Tokens(ctx, u.ID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	tokenItems := make([]TokenItem, 0, len(tokens))
	for _, t := range tokens {
		tokenItems = append(tokenItems, tokenItem(t))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="hastebin-`+safeName(u.Login)+`.json"`)
	bw := bufio.NewWriterSize(w, 32<<10)
	enc := json.NewEncoder(bw)
	write := func(prefix string, v any) error {
		bw.WriteString(prefix)
		return enc.Encode(v)
	}
	if err := write(`{"exported_at":`, d.now().UTC().Truncate(time.Second)); err != nil {
		return
	}
	write(`,"account":`, info)
	write(`,"tokens":`, tokenItems)
	bw.WriteString(`,"pastes":[`)
	first := true
	err = d.DB.EachPaste(ctx, u.ID, exportPage, func(p *accounts.Paste) error {
		prefix := ","
		if first {
			prefix, first = "", false
		}
		return write(prefix, d.item(p))
	})
	if err != nil {
		// The status is sent already: a cut file is the signal, and the log says why
		log.Warn().Err(err).Int64("user_id", u.ID).Msg("Account export cut short")
		bw.Flush()
		return
	}
	bw.WriteString("]}\n")
	bw.Flush()
}

// safeName keeps the letters, digits and - of a login, for a file name
func safeName(s string) string {
	return strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
			return c
		}
		return -1
	}, s)
}

// handleDeleteAccount answers DELETE /api/v1/account {confirm: "<login>"}:
// in one transaction the sessions, tokens and paste rows go, and every
// stored content is queued for deletion; 202, as the store catches up after
func (d *Handler) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	const help = `The body must be {"confirm": "<your login>"}.`
	if !decodeBody(w, r, &in, help) {
		return
	}
	u := user(r)
	if !strings.EqualFold(strings.TrimSpace(in.Confirm), u.Login) {
		WriteError(w, http.StatusBadRequest, "invalid_input", "Type your login to confirm.")
		return
	}
	queued, err := d.DB.DeleteUser(r.Context(), u)
	if err != nil {
		writeDBError(w, err)
		return
	}
	d.Drainer.Kick()
	d.Auth.ClearSession(w)
	log.Info().Int64("user_id", u.ID).Int64("queued", queued).Msg("Account deleted by its owner")
	writeJSON(w, http.StatusAccepted, map[string]any{"deleted": true, "queued": queued})
}
