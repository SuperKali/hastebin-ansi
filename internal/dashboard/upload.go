package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/abuse"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/rs/zerolog/log"
)

// commitTimeout bounds recording an owned upload once its content is stored
const commitTimeout = 10 * time.Second

// pasteMeta is what the X-Paste-* headers of an upload set
type pasteMeta struct {
	title      string
	language   string
	visibility int
	expires    time.Time
	tags       []string
}

// parseUploadHeaders reads X-Paste-Title, X-Paste-Language, X-Paste-Tags,
// X-Paste-Visibility and X-Paste-Expires. A missing visibility is the user's
// default. Without X-Paste-Language the language comes from the extension
// in ?ext=, as curl sends a file name, when there is a usable one.
func parseUploadHeaders(r *http.Request, defaultVisibility int, now time.Time) (pasteMeta, error) {
	meta := pasteMeta{visibility: defaultVisibility}
	var err error
	if meta.title, err = NormalizeTitle(r.Header.Get("X-Paste-Title")); err != nil {
		return meta, err
	}
	if v := r.Header.Get("X-Paste-Language"); v != "" {
		if meta.language, err = NormalizeLanguage(v); err != nil {
			return meta, err
		}
	} else if ext := r.URL.Query().Get("ext"); ext != "" {
		// Only a hint: one that names no usable language is left out
		meta.language, _ = NormalizeLanguage(handler.LanguageOfExtension(ext))
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

// ownerIntent reports whether an upload asks for something only an owned
// paste has: a session (X-CSRF-Token), a token (Authorization: Bearer) or
// any X-Paste-* header. Such an upload is owned or refused, never stored
// anonymous and public in its place. An upload without any of them is
// anonymous, exactly as before accounts.
func ownerIntent(r *http.Request) bool {
	if r.Header.Get("X-CSRF-Token") != "" || auth.HasBearer(r) {
		return true
	}
	for name := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Paste-") {
			return true
		}
	}
	return false
}

// writeUploadUnauthorized answers 401 to an upload with ownerIntent that no
// account resolves: code is session_expired for a page whose login ended,
// unauthenticated for X-Paste-* headers sent without a login or token
func writeUploadUnauthorized(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// principal resolves who sends an upload: a bearer token first, then the
// session cookie when the request passes the CSRF check. It returns nil for
// an anonymous upload, and false when it refused the request and answered.
// An upload with ownerIntent is never anonymous: without an account it is
// 401 and nothing is stored.
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
	case errors.Is(err, auth.ErrSessionExpired):
		writeUploadUnauthorized(w, "session_expired",
			"Your login has ended, so the paste was not saved. Sign in again, or save it as an anonymous public paste.")
		return nil, false
	case errors.Is(err, auth.ErrDisabled):
		writeMessage(w, http.StatusForbidden, "This account is disabled.")
		return nil, false
	case err != nil:
		log.Error().Err(err).Msg("Failed to resolve the account of an upload")
		writeBusy(w)
		return nil, false
	}
	if p == nil && ownerIntent(r) {
		writeUploadUnauthorized(w, "unauthenticated",
			"X-Paste-* headers need an API token or a login, so the paste was not saved. Send your token in an Authorization: Bearer header, or leave the X-Paste-* headers out for an anonymous public paste.")
		return nil, false
	}
	return p, true
}

// Uploaders tells who sends an upload to /documents or /log, for
// handler.DocumentHandler.Uploaders. An owned upload never expires, is held
// to the user's limits, quota and upload rate, and records its row once
// its content is stored; the token or session is checked again then.
func (d *Handler) Uploaders(w http.ResponseWriter, r *http.Request) (handler.Uploader, bool) {
	anonymous := handler.AnonymousUploader(r)
	p, ok := d.principal(w, r)
	if !ok {
		return anonymous, false
	}
	if p == nil {
		return anonymous, true
	}
	if !d.allowUpload(w, r, p) {
		return anonymous, false
	}

	meta, err := parseUploadHeaders(r, p.User.DefaultVisibility, d.now())
	if err != nil {
		writeMessage(w, http.StatusBadRequest, err.Error())
		return anonymous, false
	}

	u, ok := d.ownedUploader(w, r, p, true)
	if !ok {
		return anonymous, false
	}
	u.Commit = func(key string, size int64) error {
		return d.commit(r, p, meta, key, size)
	}
	return u, true
}

// allowUpload counts an owned upload against the upload rate of its user,
// by user id whatever address or token it comes from, as MCP does. Over
// it, it answers 429 with Retry-After and returns false.
func (d *Handler) allowUpload(w http.ResponseWriter, r *http.Request, p *auth.Principal) bool {
	id := strconv.FormatInt(p.User.ID, 10)
	if !d.uploads.OnLimit(w, r, "u:"+id) {
		return true
	}
	d.Events.Record(abuse.KindRateLimit, handler.ClientKey(handler.ClientIP(r)), "upload user "+id)
	log.Info().Int64("user_id", p.User.ID).Msg("Upload rejected, the account sent too many uploads")
	writeMessage(w, http.StatusTooManyRequests, "Too many uploads from your account: wait a minute, then retry.")
	return false
}

// ownedUploader is the Uploader of content p stores in their account: the
// user's quota, pinned, at most what the account may still store. Over the
// user's limits it answers and returns false; the paste count counts only
// for a new paste. Commit is left to the caller.
func (d *Handler) ownedUploader(w http.ResponseWriter, r *http.Request, p *auth.Principal, newPaste bool) (handler.Uploader, bool) {
	count, bytes, err := d.DB.Usage(r.Context(), p.User.ID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to read the usage of an account")
		writeBusy(w)
		return handler.Uploader{}, false
	}
	if !newPaste {
		count = 0
	}
	if msg, status := d.overLimits(count, bytes); status != 0 {
		writeMessage(w, status, msg)
		return handler.Uploader{}, false
	}
	maxLength := d.Limits.MaxLength
	if d.Limits.MaxBytes > 0 {
		if left := d.Limits.MaxBytes - bytes; maxLength <= 0 || left < int64(maxLength) {
			maxLength = int(left)
		}
	}
	return handler.Uploader{
		IP:        handler.ClientIP(r),
		QuotaKey:  "u:" + strconv.FormatInt(p.User.ID, 10),
		Owner:     p.User.ID,
		Pin:       true,
		MaxLength: maxLength,
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
		Language:   meta.language,
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
	err := d.DB.InsertPaste(ctx, row, d.accountLimits())
	if err == nil {
		log.Info().Int64("user_id", p.User.ID).Int64("size", size).Bool("token", p.Token != nil).Msg("Owned paste stored")
		return nil
	}
	return d.refused(p, err, "Failed to record an owned paste")
}

// accountLimits are the limits a user's pastes are held to in the database
func (d *Handler) accountLimits() accounts.Limits {
	return accounts.Limits{MaxPastes: d.Limits.MaxPastes, MaxBytes: d.Limits.MaxBytes}
}

// refused turns a failure to record the content of p into the answer the
// upload gets: an *handler.UploadRefused, or err itself (500), logged with what
func (d *Handler) refused(p *auth.Principal, err error, what string) error {
	switch {
	case errors.Is(err, accounts.ErrRevoked):
		if p.Token != nil {
			return &handler.UploadRefused{Status: http.StatusUnauthorized, Message: "The API token is invalid or revoked.",
				Header: map[string]string{"WWW-Authenticate": `Bearer realm="hastebin", error="invalid_token"`}}
		}
		return &handler.UploadRefused{Status: http.StatusUnauthorized, Message: "Your session ended during the upload. Log in again."}
	case errors.Is(err, accounts.ErrUserDisabled):
		return &handler.UploadRefused{Status: http.StatusForbidden, Message: "This account is disabled."}
	case errors.Is(err, accounts.ErrPasteLimit):
		msg, status := d.overLimits(d.Limits.MaxPastes, 0)
		return &handler.UploadRefused{Status: status, Message: msg}
	case errors.Is(err, accounts.ErrStorageLimit):
		return &handler.UploadRefused{Status: http.StatusRequestEntityTooLarge,
			Message: "Your account storage is full. Delete some pastes to upload more."}
	case errors.Is(err, accounts.ErrNotFound):
		return &handler.UploadRefused{Status: http.StatusNotFound, Message: "Not found."}
	case accounts.IsBusy(err) || errors.Is(err, context.DeadlineExceeded):
		log.Warn().Err(err).Msg(what + ", the database is busy")
		return &handler.UploadRefused{Status: http.StatusServiceUnavailable, Message: "The server is busy, try again shortly.",
			Header: map[string]string{"Retry-After": "5"}}
	}
	log.Error().Err(err).Msg(what)
	return err
}
