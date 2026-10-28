package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/rs/zerolog/log"
)

// commitTimeout bounds recording an owned upload once its content is stored
const commitTimeout = 10 * time.Second

// pasteMeta is what the X-Paste-* headers of an upload set
type pasteMeta struct {
	title      string
	visibility int
	expires    time.Time
	tags       []string
}

// parseUploadHeaders reads X-Paste-Title, X-Paste-Tags, X-Paste-Visibility
// and X-Paste-Expires. A missing visibility is the user's default.
func parseUploadHeaders(r *http.Request, defaultVisibility int, now time.Time) (pasteMeta, error) {
	meta := pasteMeta{visibility: defaultVisibility}
	var err error
	if meta.title, err = NormalizeTitle(r.Header.Get("X-Paste-Title")); err != nil {
		return meta, err
	}
	if meta.tags, err = NormalizeTags(SplitTags(r.Header.Values("X-Paste-Tags"))); err != nil {
		return meta, err
	}
	if v := r.Header.Get("X-Paste-Visibility"); v != "" {
		if meta.visibility, err = ParseVisibility(v); err != nil {
			return meta, err
		}
	}
	if v := r.Header.Get("X-Paste-Expires"); v != "" {
		if meta.expires, err = ParseExpiry(v, now); err != nil {
			return meta, err
		}
	}
	return meta, nil
}

// writeMessage answers a paste route with the {"message"} error form
func writeMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// writeBusy answers a paste route whose database work timed out
func writeBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	writeMessage(w, http.StatusServiceUnavailable, "The server is busy, try again shortly.")
}

// principal resolves who sends an upload: a bearer token first, then the
// session cookie when the request passes the CSRF check. It returns nil for
// an anonymous upload, and false when it refused the request and answered.
func (d *Handler) principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	var p *auth.Principal
	var err error
	if auth.HasBearer(r) {
		// A bearer token wins over a cookie, and a bad one is never anonymous
		p, err = d.Auth.Bearer(r)
	} else {
		p, err = d.Auth.UploadSession(r)
	}
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		auth.WriteInvalidToken(w, false)
		return nil, false
	case errors.Is(err, auth.ErrDisabled):
		writeMessage(w, http.StatusForbidden, "This account is disabled.")
		return nil, false
	case err != nil:
		log.Error().Err(err).Msg("Failed to resolve the account of an upload")
		writeBusy(w)
		return nil, false
	}
	return p, true
}

// Uploaders tells who sends an upload to /documents or /log, for
// handler.DocumentHandler.Uploaders. An owned upload never expires, is held
// to the user's limits and quota, and records its row once its content is
// stored; the token or session is checked again then.
func (d *Handler) Uploaders(w http.ResponseWriter, r *http.Request) (handler.Uploader, bool) {
	anonymous := handler.AnonymousUploader(r)
	p, ok := d.principal(w, r)
	if !ok {
		return anonymous, false
	}
	if p == nil {
		return anonymous, true
	}

	meta, err := parseUploadHeaders(r, p.User.DefaultVisibility, d.now())
	if err != nil {
		writeMessage(w, http.StatusBadRequest, err.Error())
		return anonymous, false
	}

	count, bytes, err := d.DB.Usage(r.Context(), p.User.ID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to read the usage of an account")
		writeBusy(w)
		return anonymous, false
	}
	if msg, status := d.overLimits(count, bytes); status != 0 {
		writeMessage(w, status, msg)
		return anonymous, false
	}
	maxLength := d.Limits.MaxLength
	if d.Limits.MaxBytes > 0 {
		if left := d.Limits.MaxBytes - bytes; maxLength <= 0 || left < int64(maxLength) {
			maxLength = int(left)
		}
	}

	userID := p.User.ID
	return handler.Uploader{
		IP:        anonymous.IP,
		QuotaKey:  "u:" + strconv.FormatInt(userID, 10),
		Owner:     userID,
		Pin:       true,
		MaxLength: maxLength,
		Commit: func(key string, size int64) error {
			return d.commit(r, p, meta, key, size)
		},
	}, true
}

// overLimits returns the message and status that refuse an upload of a user
// with count pastes of bytes in all, status 0 when there is room left
func (d *Handler) overLimits(count, bytes int64) (string, int) {
	if d.Limits.MaxPastes > 0 && count >= d.Limits.MaxPastes {
		return fmt.Sprintf("Your account holds the most pastes it may (%d). Delete some to upload more.", d.Limits.MaxPastes),
			http.StatusForbidden
	}
	if d.Limits.MaxBytes > 0 && bytes >= d.Limits.MaxBytes {
		return "Your account storage is full. Delete some pastes to upload more.", http.StatusRequestEntityTooLarge
	}
	return "", 0
}

// commit records an owned paste whose content is stored. The request may be
// gone by now, so it runs on its own deadline.
func (d *Handler) commit(r *http.Request, p *auth.Principal, meta pasteMeta, key string, size int64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), commitTimeout)
	defer cancel()
	row := accounts.NewPaste{
		Key:        key,
		UserID:     p.User.ID,
		Title:      meta.title,
		Visibility: meta.visibility,
		ExpiresAt:  meta.expires,
		Size:       size,
		Tags:       meta.tags,
	}
	if p.Token != nil {
		row.TokenID = p.Token.ID
	} else if p.Session != nil {
		row.SessionHash = p.Session.IDHash
	}
	err := d.DB.InsertPaste(ctx, row, accounts.Limits{MaxPastes: d.Limits.MaxPastes, MaxBytes: d.Limits.MaxBytes})
	switch {
	case err == nil:
		log.Info().Int64("user_id", p.User.ID).Int64("size", size).Bool("token", p.Token != nil).Msg("Owned paste stored")
		return nil
	case errors.Is(err, accounts.ErrRevoked):
		if p.Token != nil {
			return &handler.UploadRefused{Status: http.StatusUnauthorized, Message: "The API token is invalid or revoked.",
				Header: map[string]string{"WWW-Authenticate": `Bearer realm="hastebin", error="invalid_token"`}}
		}
		return &handler.UploadRefused{Status: http.StatusUnauthorized, Message: "Your session ended during the upload. Log in again."}
	case errors.Is(err, accounts.ErrPasteLimit):
		msg, status := d.overLimits(d.Limits.MaxPastes, 0)
		return &handler.UploadRefused{Status: status, Message: msg}
	case errors.Is(err, accounts.ErrStorageLimit):
		return &handler.UploadRefused{Status: http.StatusRequestEntityTooLarge,
			Message: "Your account storage is full. Delete some pastes to upload more."}
	case accounts.IsBusy(err) || errors.Is(err, context.DeadlineExceeded):
		log.Warn().Err(err).Msg("Owned paste not recorded, the database is busy")
		return &handler.UploadRefused{Status: http.StatusServiceUnavailable, Message: "The server is busy, try again shortly.",
			Header: map[string]string{"Retry-After": "5"}}
	}
	log.Error().Err(err).Msg("Failed to record an owned paste")
	return err
}
