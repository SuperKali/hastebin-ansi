package admin

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/dashboard"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
)

// ContentLimit is how much of a paste the content view shows
const ContentLimit = 256 << 10

// keyPattern is the form of a paste key, as every route cuts it at the first dot
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// errBadTarget is the answer to a lookup of something that is no paste link of this server
var errBadTarget = errors.New("Give a paste key, or a link to a paste on this server: /<key>, /raw/<key> or /documents/<key>.")

// ParseTarget returns the paste key in what an admin pasted: a bare key,
// with or without an extension, or a link to a paste of this server, as
// /<key>, /raw/<key>, /documents/<key> or /dashboard/p/<key>, with or
// without the scheme and host. A query or fragment is ignored. A link to
// another host than baseURL is refused, so a look-alike link never acts on
// a paste here; without a baseURL any host is taken.
func ParseTarget(s, baseURL string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxQuery || strings.ContainsAny(s, " \t\r\n\\") {
		return "", errBadTarget
	}
	path := s
	if strings.Contains(s, "/") {
		raw := s
		if !strings.Contains(raw, "://") && !strings.HasPrefix(raw, "/") {
			raw = "https://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.Opaque != "" {
			return "", errBadTarget
		}
		if u.Host != "" || u.Scheme != "" {
			if u.Scheme != "http" && u.Scheme != "https" {
				return "", errBadTarget
			}
			if base, err := url.Parse(baseURL); err == nil && base.Host != "" && !strings.EqualFold(u.Host, base.Host) {
				return "", errors.New("That link is not on this server.")
			}
		} else if strings.HasPrefix(raw, "//") {
			return "", errBadTarget
		}
		path = u.Path
	} else if cut := strings.IndexAny(s, "?#"); cut >= 0 {
		path = s[:cut]
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")
	last := ""
	switch {
	case len(segments) == 1:
		last = segments[0]
	case len(segments) == 2 && (segments[0] == "raw" || segments[0] == "documents"):
		last = segments[1]
	case len(segments) == 3 && segments[0] == "dashboard" && segments[1] == "p":
		last = segments[2]
	default:
		return "", errBadTarget
	}
	key, _, _ := strings.Cut(last, ".")
	if !keyPattern.MatchString(key) {
		return "", errBadTarget
	}
	return key, nil
}

// routeKey is the paste key of a /pastes/{key} route, cut at the first dot
// like every paste route; "" when it is not a key
func routeKey(r *http.Request) string {
	key, _, _ := strings.Cut(chi.URLParam(r, "key"), ".")
	if !keyPattern.MatchString(key) {
		return ""
	}
	return key
}

// Owner is the account of a paste in the admin API
type Owner struct {
	ID           int64   `json:"id"`
	GitHubID     int64   `json:"github_id"`
	Login        string  `json:"login"`
	AvatarURL    *string `json:"avatar_url"`
	Disabled     bool    `json:"disabled"`
	AbuseStrikes int     `json:"abuse_strikes"`
	Admin        bool    `json:"admin"`
}

// TombstoneItem is a removed paste in the admin API
type TombstoneItem struct {
	Reason     string    `json:"reason"`
	Visibility string    `json:"visibility"`
	RemovedAt  time.Time `json:"removed_at"`
	AdminID    int64     `json:"admin_id"`
}

// Lookup is what the admin page knows about a key: never its content
type Lookup struct {
	Key string `json:"key"`

	// Exists is set when the store has content under the key
	Exists     bool       `json:"exists"`
	Size       *int64     `json:"size"`
	ModifiedAt *time.Time `json:"modified_at"`

	// Registered is set when an account owns the paste; the fields up to
	// Version are its own, null or zero otherwise
	Registered bool       `json:"registered"`
	Owner      *Owner     `json:"owner"`
	Visibility string     `json:"visibility"`
	Title      string     `json:"title"`
	CreatedAt  *time.Time `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Expired    bool       `json:"expired"`
	SeenAt     *time.Time `json:"seen_at"`
	Views      int64      `json:"views"`
	Version    int        `json:"version"`

	Tombstone *TombstoneItem `json:"tombstone"`
}

// lookupKey gathers what is known about key. It returns nil when nothing
// is: no content, no row and no tombstone.
func (h *Handler) lookupKey(r *http.Request, key string) (*Lookup, error) {
	ctx := r.Context()
	out := &Lookup{Key: key, Visibility: "link"}
	if size, err := h.Docs.Store.Size(key); err == nil && size > 0 {
		out.Exists = true
		out.Size = &size
		if files, ok := h.Docs.Store.(interface {
			ModTime(string) (time.Time, error)
		}); ok {
			if t, err := files.ModTime(key); err == nil {
				t = t.UTC()
				out.ModifiedAt = &t
			}
		}
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	p, err := h.DB.PasteAny(ctx, key)
	switch {
	case err == nil:
		out.Registered = true
		out.Visibility = dashboard.VisibilityName(p.Visibility)
		out.Title = p.Title
		out.CreatedAt = timePtr(p.CreatedAt)
		out.ExpiresAt = timePtr(p.ExpiresAt)
		out.Expired = !p.ExpiresAt.IsZero() && !h.now().Before(p.ExpiresAt)
		out.SeenAt = timePtr(p.SeenAt)
		out.Views = p.Views
		out.Version = p.Version
		u, err := h.DB.UserByID(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		out.Owner = &Owner{ID: u.ID, GitHubID: u.GitHubID, Login: u.Login, AvatarURL: avatar(u.AvatarURL),
			Disabled: u.Disabled(), AbuseStrikes: u.AbuseStrikes, Admin: h.Auth.IsAdmin(u.GitHubID)}
	case !errors.Is(err, accounts.ErrNotFound):
		return nil, err
	}

	t, err := h.DB.TombstoneOf(ctx, key)
	switch {
	case err == nil:
		out.Tombstone = &TombstoneItem{Reason: t.Reason, Visibility: dashboard.VisibilityName(t.Visibility),
			RemovedAt: t.RemovedAt, AdminID: t.AdminID}
	case !errors.Is(err, accounts.ErrNotFound):
		return nil, err
	}
	if !out.Exists && !out.Registered && out.Tombstone == nil {
		return nil, nil
	}
	return out, nil
}

// lookup answers GET /api/v1/admin/pastes?q=<key or link> and GET
// /api/v1/admin/pastes/{key}: the metadata of a paste, never its content.
// A link goes in the query, which the access log leaves out, so the key
// in it is never logged in the clear.
func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) {
	var key string
	if chi.URLParam(r, "key") != "" {
		if key = routeKey(r); key == "" {
			writeError(w, http.StatusBadRequest, "invalid_input", errBadTarget.Error())
			return
		}
	} else {
		var err error
		if key, err = ParseTarget(r.URL.Query().Get("q"), h.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
	}
	out, err := h.lookupKey(r, key)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if out == nil {
		writeError(w, http.StatusNotFound, "not_found", "No paste has this key.")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// content answers POST /api/v1/admin/pastes/{key}/content {reason}: the
// first 256 KiB of a paste, private ones included. The view is written to
// the audit log first, and it counts as a read by someone other than the
// owner: inside the edit window it ends editing under the same link.
func (h *Handler) content(w http.ResponseWriter, r *http.Request) {
	key := routeKey(r)
	if key == "" {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	reason, ok := reasonBody(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := h.DB.TombstoneOf(ctx, key); err == nil {
		writeError(w, http.StatusGone, "removed", "This paste was removed for abuse; its content is gone.")
		return
	} else if !errors.Is(err, accounts.ErrNotFound) {
		writeDBError(w, err)
		return
	}
	// A paste its owner deleted is gone for admins too, while its content
	// waits in the delete queue
	if a, err := h.DB.AccessOf(ctx, key); err != nil {
		writeDBError(w, err)
		return
	} else if a.Pending {
		writeError(w, http.StatusNotFound, "not_found", "No paste has this key.")
		return
	}
	if _, err := h.Docs.Store.Size(key); errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No paste has this key.")
		return
	} else if err != nil {
		writeDBError(w, err)
		return
	}

	// Audited before anything is read: no view without its row
	if err := h.DB.Audit(ctx, entry(r, "paste.view", accounts.PasteTarget(key), map[string]any{"reason": reason})); err != nil {
		writeDBError(w, err)
		return
	}
	if h.Gate != nil {
		if err := h.Gate.ModeratorRead(ctx, key, admin(r).ID); errors.Is(err, handler.ErrDenied) {
			writeError(w, http.StatusNotFound, "not_found", "No paste has this key.")
			return
		} else if err != nil {
			writeDBError(w, err)
			return
		}
	}
	data, size, err := h.Docs.ReadForModeration(handler.ClientIP(r), key, ContentLimit)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "No paste has this key.")
		return
	case errors.Is(err, handler.ErrMemoryBusy):
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, "busy", "The server is busy, try again shortly.")
		return
	case err != nil:
		writeDBError(w, err)
		return
	}
	logAction(r, "paste.view").Str("key", KeyHash(key)).Int64("size", size).Msg("Admin viewed a paste")
	writeJSON(w, http.StatusOK, map[string]any{
		"key":       key,
		"size":      size,
		"shown":     len(data),
		"truncated": int64(len(data)) < size,
		"content":   string(data),
	})
}

// deletePaste answers DELETE /api/v1/admin/pastes/{key} {reason}
func (h *Handler) deletePaste(w http.ResponseWriter, r *http.Request) {
	h.remove(w, r, false)
}

// abusePaste answers POST /api/v1/admin/pastes/{key}/abuse {reason}: the
// paste is deleted, its key answers 410 from now on (404 when it was
// private) and its owner gets a strike
func (h *Handler) abusePaste(w http.ResponseWriter, r *http.Request) {
	h.remove(w, r, true)
}

// remove deletes a paste, and with abuse leaves a tombstone and a strike.
// The row goes in one transaction with the audit row; the content is
// deleted at once and through the delete queue, which retries on failure.
func (h *Handler) remove(w http.ResponseWriter, r *http.Request, abuse bool) {
	key := routeKey(r)
	if key == "" {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	reason, ok := reasonBody(w, r)
	if !ok {
		return
	}
	found, err := h.lookupKey(r, key)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if found == nil || (!found.Exists && !found.Registered) {
		writeError(w, http.StatusNotFound, "not_found", "No paste has this key.")
		return
	}

	action := "paste.delete"
	if abuse {
		action = "paste.abuse"
	}
	detail := map[string]any{"reason": reason}
	if found.Owner != nil {
		detail["owner_id"] = found.Owner.ID
		detail["owner_login"] = found.Owner.Login
	}
	m, err := h.DB.RemovePaste(r.Context(), key, abuse, reason, entry(r, action, accounts.PasteTarget(key), detail))
	if err != nil {
		writeDBError(w, err)
		return
	}
	// The row is gone: delete the content now so it is not readable for a
	// moment as a public paste, and let the queue retry if this fails
	if h.Drainer != nil {
		if err := h.Drainer.Delete(key); err != nil {
			logAction(r, action).Err(err).Str("key", KeyHash(key)).Msg("Removed paste content not deleted yet, the delete queue retries")
		}
		h.Drainer.Kick()
	}
	logAction(r, action).Str("key", KeyHash(key)).Int64("owner_id", m.OwnerID).Msg("Admin removed a paste")
	writeJSON(w, http.StatusOK, map[string]any{
		"key":       key,
		"deleted":   true,
		"tombstone": abuse,
		"owner_id":  m.OwnerID,
	})
}
