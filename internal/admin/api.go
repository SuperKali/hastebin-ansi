package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	// PerPage is how many rows one page of every admin list holds
	PerPage = 50

	// MaxReason is the longest reason, in characters
	MaxReason = 500

	// maxBody caps the JSON body of every admin route
	maxBody = 16 << 10

	// maxPage is the highest page number accepted
	maxPage = 100000

	// maxQuery is the longest search text or paste link, in bytes
	maxQuery = 2048
)

// writeError answers with the JSON error of the /api/v1 routes
func writeError(w http.ResponseWriter, status int, code, message string) {
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
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
	case accounts.IsBusy(err):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "db_busy", "The server is busy, try again shortly.")
	default:
		log.Error().Err(err).Msg("Admin database call failed")
		writeError(w, http.StatusInternalServerError, "internal", "Something went wrong, try again.")
	}
}

// decodeBody decodes a JSON body of at most maxBody bytes into v. Unknown
// fields are refused. It answers 400 and returns false on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, help string) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", help)
		return false
	}
	return true
}

// cleanReason checks the reason of an action: required, valid UTF-8,
// without control characters, at most MaxReason characters, trimmed
func cleanReason(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("Give a reason: it goes in the audit log.")
	case !utf8.ValidString(s):
		return "", errors.New("The reason must be valid UTF-8.")
	case utf8.RuneCountInString(s) > MaxReason:
		return "", errors.New("The reason is too long: at most 500 characters.")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", errors.New("The reason must not hold control characters.")
		}
	}
	return s, nil
}

// reasonBody reads a body of just {"reason"}. It answers 400 and returns
// false when the body or the reason is not right.
func reasonBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &in, `The body must be {"reason": "..."}.`) {
		return "", false
	}
	reason, err := cleanReason(in.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return "", false
	}
	return reason, true
}

// admin is the account of the request; RequireAdmin let it through
func admin(r *http.Request) *accounts.User {
	return auth.FromContext(r.Context()).User
}

// entry is the audit row of an action of the admin of r. detail is
// written as JSON.
func entry(r *http.Request, action, target string, detail map[string]any) accounts.AuditEntry {
	a := admin(r)
	text := ""
	if len(detail) > 0 {
		data, err := json.Marshal(detail)
		if err == nil {
			text = string(data)
		}
	}
	return accounts.AuditEntry{ActorID: a.ID, ActorLogin: a.Login, Action: action, Target: target, Detail: text}
}

// logAction starts the info line of an admin action. Paste keys are only
// ever logged hashed (KeyHash), like in the access log.
func logAction(r *http.Request, action string) *zerolog.Event {
	return log.Info().Int64("admin_id", admin(r).ID).Str("action", action)
}

// KeyHash is how a paste key appears in the logs: # and the first 8 hex
// digits of its SHA-256, the same form the access log uses
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "#" + hex.EncodeToString(sum[:4])
}

// pageParam reads ?page=, 1 when missing. It answers 400 and returns false
// when it is not a number from 1.
func pageParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("page")
	if v == "" {
		return 1, true
	}
	page, err := strconv.Atoi(v)
	if err != nil || page < 1 || page > maxPage {
		writeError(w, http.StatusBadRequest, "invalid_input", "page must be a number from 1.")
		return 0, false
	}
	return page, true
}

// pages is how many pages of PerPage total rows make, at least 1
func pages(total int64) int64 {
	return max((total+PerPage-1)/PerPage, 1)
}

// timePtr is t for JSON, nil (null) for the zero time
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// avatar is the stored avatar of a user, checked again and sized like the
// account menu asks for it, nil when there is none
func avatar(stored string) *string {
	clean := auth.CheckAvatarURL(stored)
	if clean == "" {
		return nil
	}
	u, err := url.Parse(clean)
	if err != nil {
		return nil
	}
	query := u.Query()
	query.Set("s", "64")
	u.RawQuery = query.Encode()
	sized := u.String()
	return &sized
}
