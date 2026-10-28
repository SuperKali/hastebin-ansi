package admin

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/abuse"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/go-chi/chi/v5"
)

// Kinds of bans in the admin API
const (
	// BanManual is a ban an admin set, kept in the database
	BanManual = "manual"

	// BanNotFound is the automatic ban for looking up too many unknown keys
	BanNotFound = "not_found"

	// BanQuota is a client that used up its upload quota
	BanQuota = "quota"

	// BanUserQuota is an account that used up its upload quota
	BanUserQuota = "user_quota"
)

const (
	// banListLimit is how many bans of each kind the list reads at most
	banListLimit = 500

	// maxBanFor is the longest a manual ban may last; 0 means for ever
	maxBanFor = 10 * 365 * 24 * time.Hour
)

// userQuotaKey is the quota key of an account, as the upload quota counts it
var userQuotaKey = regexp.MustCompile(`^u:[1-9][0-9]{0,18}$`)

// BanItem is a ban of any kind in the admin API
type BanItem struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`

	// Until is when the ban ends, null for never
	Until *time.Time `json:"until"`

	// Reason, CreatedAt and AdminID are set for manual bans
	Reason    string     `json:"reason,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	AdminID   int64      `json:"admin_id,omitempty"`

	// Used is the bytes stored in the window, for quota blocks
	Used int64 `json:"used,omitempty"`
}

// bans answers GET /api/v1/admin/bans?page=: the manual bans, newest
// first, then the automatic ones the limiters hold in memory, the longest
// first. Each kind is read up to 500; truncated says some were left out.
func (h *Handler) bans(w http.ResponseWriter, r *http.Request) {
	page, ok := pageParam(w, r)
	if !ok {
		return
	}
	now := h.now()
	manual, err := h.DB.Bans(r.Context(), now, banListLimit+1)
	if err != nil {
		writeDBError(w, err)
		return
	}
	truncated := len(manual) > banListLimit
	if truncated {
		manual = manual[:banListLimit]
	}
	items := make([]BanItem, 0, len(manual))
	for _, b := range manual {
		created := b.CreatedAt
		items = append(items, BanItem{Kind: BanManual, Target: b.Client, Until: timePtr(b.ExpiresAt),
			Reason: b.Reason, CreatedAt: &created, AdminID: b.AdminID})
	}
	add := func(kind string, list []handler.Ban) {
		if len(list) > banListLimit {
			truncated = true
			list = list[:banListLimit]
		}
		for _, b := range list {
			until := b.Until
			items = append(items, BanItem{Kind: kind, Target: b.Client, Until: &until, Used: b.Used})
		}
	}
	if h.Docs != nil {
		add(BanNotFound, h.Docs.NotFound.Snapshot(banListLimit+1))
		add(BanQuota, h.Docs.Quota.Snapshot(banListLimit+1))
		add(BanUserQuota, h.Docs.UserQuota.Snapshot(banListLimit+1))
	}

	total := int64(len(items))
	start := min((page-1)*PerPage, len(items))
	end := min(start+PerPage, len(items))
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items[start:end], "page": page, "pages": pages(total), "total": total, "truncated": truncated,
	})
}

// addBan answers POST /api/v1/admin/bans {target, reason, expires_in}:
// the client, an IPv4 address or an IPv6 /64, gets 403 on every route but
// /health from now on, until expires_in seconds pass (0 for never)
func (h *Handler) addBan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target    string `json:"target"`
		Reason    string `json:"reason"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if !decodeBody(w, r, &in, `The body must be {"target": "<IPv4, IPv6 or IPv6/64>", "reason": "...", "expires_in": <seconds, 0 for never>}.`) {
		return
	}
	client, err := abuse.NormalizeTarget(in.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "The target must be an IPv4 address, an IPv6 address or an IPv6 /64.")
		return
	}
	reason, err := cleanReason(in.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if in.ExpiresIn < 0 || in.ExpiresIn > int64(maxBanFor/time.Second) {
		writeError(w, http.StatusBadRequest, "invalid_input", "expires_in must be from 0 (never) to 10 years in seconds.")
		return
	}
	if client == handler.ClientKey(handler.ClientIP(r)) {
		writeError(w, http.StatusBadRequest, "invalid_input", "That is your own address: banning it would lock you out.")
		return
	}

	now := h.now().UTC().Truncate(time.Second)
	ban := accounts.IPBan{Client: client, Reason: reason, CreatedAt: now, AdminID: admin(r).ID}
	if in.ExpiresIn > 0 {
		ban.ExpiresAt = now.Add(time.Duration(in.ExpiresIn) * time.Second)
	}
	detail := map[string]any{"reason": reason, "expires_at": timePtr(ban.ExpiresAt)}
	err = h.DB.PutBan(r.Context(), ban, abuse.MaxManualBans, entry(r, "ban.add", accounts.BanTarget(BanManual, client), detail))
	if errors.Is(err, accounts.ErrTooMany) {
		writeError(w, http.StatusConflict, "limit_reached", "There are as many manual bans as allowed (10000). Remove some first.")
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	h.Bans.Put(abuse.Ban{Client: client, Reason: reason, CreatedAt: now, ExpiresAt: ban.ExpiresAt, AdminID: ban.AdminID})
	logAction(r, "ban.add").Str("client", client).Int64("expires_in", in.ExpiresIn).Msg("Admin banned a client")
	writeJSON(w, http.StatusCreated, BanItem{Kind: BanManual, Target: client, Until: timePtr(ban.ExpiresAt),
		Reason: reason, CreatedAt: &now, AdminID: ban.AdminID})
}

// unban answers DELETE /api/v1/admin/bans/{kind}/{target} {reason}: a manual ban
// is removed, a not-found ban lifted, a quota block reset. The target is
// the address as the list shows it, path escaped.
func (h *Handler) unban(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	raw, err := url.PathUnescape(chi.URLParam(r, "target"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "The target is not a valid address.")
		return
	}
	var client string
	switch kind {
	case BanManual, BanNotFound, BanQuota:
		if client, err = abuse.NormalizeTarget(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input", "The target must be an IPv4 address or an IPv6 /64.")
			return
		}
	case BanUserQuota:
		if !userQuotaKey.MatchString(raw) {
			writeError(w, http.StatusBadRequest, "invalid_input", "The target of a user quota is u:<user id>.")
			return
		}
		client = raw
	default:
		writeError(w, http.StatusNotFound, "not_found", "Unknown kind of ban. Use manual, not_found, quota or user_quota.")
		return
	}
	reason, ok := reasonBody(w, r)
	if !ok {
		return
	}
	audit := entry(r, "ban.remove", accounts.BanTarget(kind, client), map[string]any{"reason": reason})

	if kind == BanManual {
		if err := h.DB.DeleteBan(r.Context(), client, audit); err != nil {
			writeDBError(w, err)
			return
		}
		h.Bans.Remove(client)
	} else {
		found := false
		if h.Docs != nil {
			switch kind {
			case BanNotFound:
				found = h.Docs.NotFound.Unban(client)
			case BanQuota:
				found = h.Docs.Quota.Reset(client)
			case BanUserQuota:
				found = h.Docs.UserQuota.Reset(client)
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "not_found", "No such ban now.")
			return
		}
		// The block lives in memory and is lifted already: the row follows
		if err := h.DB.Audit(r.Context(), audit); err != nil {
			logAction(r, "ban.remove").Err(err).Str("kind", kind).Str("client", client).
				Msg("Admin lifted a ban but the audit log could not be written")
			writeDBError(w, err)
			return
		}
	}
	logAction(r, "ban.remove").Str("kind", kind).Str("client", client).Msg("Admin lifted a ban")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// events answers GET /api/v1/admin/events?page=: the recent refusals,
// newest first, each kind at most once a minute per client
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	page, ok := pageParam(w, r)
	if !ok {
		return
	}
	items, total := h.Events.Page(page, PerPage)
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "page": page, "pages": pages(int64(total)), "total": total, "max": abuse.MaxEvents,
	})
}

// audit answers GET /api/v1/admin/audit?page=: the audit log, newest
// first. It is read only: no route changes or deletes a row, and the
// database refuses it anyway.
func (h *Handler) audit(w http.ResponseWriter, r *http.Request) {
	page, ok := pageParam(w, r)
	if !ok {
		return
	}
	total, err := h.DB.AuditCount(r.Context())
	if err != nil {
		writeDBError(w, err)
		return
	}
	items, err := h.DB.AuditLog(r.Context(), PerPage, (page-1)*PerPage)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if items == nil {
		items = []accounts.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "pages": pages(total), "total": total})
}
