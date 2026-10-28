package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

// Editing, new versions, duplicates and the history of a paste. Every route
// is the owner's only: a paste of someone else answers 404 like a missing one.
//
// Same-link editing: the new content is received first under a staging key,
// without the key lock, however slow the upload. Then, under the key lock the
// reader path shares (Gate.closeWindow), the window is checked again on
// server time, the old content copied to "<key>.r<n>", the new one put in
// place in one step, and last the database records it. When that fails the
// old content goes back and the answer is 503. A reader other than the owner
// writes seen_at under the same lock before any content goes out to them, so
// no one but the owner ever reads two contents under one link.

// errorBody is the JSON of a refused edit, with the reason of a 409
type errorBody struct {
	Error   string `json:"error"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`
}

// Reasons of 409 edit_window_closed
const (
	reasonSeen    = "seen"
	reasonExpired = "expired"
)

// closedReason tells why the content of p can no longer change under the
// same link at now: "seen" once someone else opened it, "expired" past the
// window or with editing off; "" while it can
func (d *Handler) closedReason(p *accounts.Paste, now time.Time) string {
	if !p.SeenAt.IsZero() {
		return reasonSeen
	}
	if d.EditWindow <= 0 || !now.Before(p.CreatedAt.Add(d.EditWindow)) {
		return reasonExpired
	}
	return ""
}

// editableUntil is the end of the edit window of p, the zero time when it is over
func (d *Handler) editableUntil(p *accounts.Paste, now time.Time) time.Time {
	if d.closedReason(p, now) != "" {
		return time.Time{}
	}
	return p.CreatedAt.Add(d.EditWindow)
}

// writeClosed answers 409 for an edit refused for reason
func writeClosed(w http.ResponseWriter, reason string) {
	msg := "Someone else opened this paste, so it can no longer change under the same link. Save it as a new version."
	if reason == reasonExpired {
		msg = "The edit window is over. Save it as a new version."
	}
	writeJSON(w, http.StatusConflict, errorBody{Error: "edit_window_closed", Reason: reason, Message: msg})
}

// writeTooManyRevisions answers 409 for an edit of a paste that keeps as many
// older contents as it may
func writeTooManyRevisions(w http.ResponseWriter, limit int) {
	writeJSON(w, http.StatusConflict, errorBody{Error: "too_many_revisions",
		Message: fmt.Sprintf("This paste keeps %d older versions already. Save it as a new version.", limit)})
}

// writeStoreError answers a failed store operation
func writeStoreError(w http.ResponseWriter, err error) {
	var quota *handler.QuotaError
	switch {
	case errors.Is(err, storage.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, storage.ErrNoSpace):
		WriteError(w, http.StatusInsufficientStorage, "no_space", "The server is low on disk space, try again later.")
	case errors.As(err, &quota):
		w.Header().Set("Retry-After", strconv.Itoa(handler.RetryAfterSeconds(quota.RetryAfter)))
		WriteError(w, http.StatusTooManyRequests, "rate_limited", "Upload quota reached, try again later.")
	case errors.Is(err, handler.ErrMemoryBusy):
		w.Header().Set("Retry-After", "10")
		WriteError(w, http.StatusServiceUnavailable, "busy", "Server busy, try again shortly.")
	default:
		log.Error().Err(err).Msg("Store operation of the dashboard failed")
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "busy", "Storing the paste failed, try again shortly.")
	}
}

// ownPaste returns the paste of the route if it is the user's and not
// expired; else it answers 404 and returns false
func (d *Handler) ownPaste(w http.ResponseWriter, r *http.Request) (*accounts.Paste, bool) {
	p, err := d.DB.PasteByKey(r.Context(), user(r).ID, pasteKey(r))
	if err != nil {
		writeDBError(w, err)
		return nil, false
	}
	if !p.ExpiresAt.IsZero() && !d.now().Before(p.ExpiresAt) {
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	return p, true
}

// stagingKey is a fresh dotted store key, which no route reaches, to receive
// a new content of key under before it is put in place
func stagingKey(key string) string {
	var b [8]byte
	rand.Read(b[:])
	return key + ".e" + hex.EncodeToString(b[:])
}

// commitContext bounds the database and store work that must finish once
// started, even when the client goes away
func commitContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), commitTimeout)
}

// handleEditContent answers PUT /api/v1/pastes/{key}/content: the body is
// the new content, under the same link, while the edit window is open and
// no one else opened the paste. The old content is kept as a revision.
// 409 edit_window_closed {reason: seen|expired} or too_many_revisions when
// it cannot change; the upload limits and quota apply as on /documents.
func (d *Handler) handleEditContent(w http.ResponseWriter, r *http.Request) {
	paste, ok := d.ownPaste(w, r)
	if !ok {
		return
	}
	// Checked before the body is read, and again under the lock
	if reason := d.closedReason(paste, d.now()); reason != "" {
		writeClosed(w, reason)
		return
	}
	count, _, err := d.DB.NextRevision(r.Context(), paste.Key)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if d.MaxRevisions > 0 && count >= d.MaxRevisions {
		writeTooManyRevisions(w, d.MaxRevisions)
		return
	}

	p := auth.FromContext(r.Context())
	u, ok := d.ownedUploader(w, r, p, false)
	if !ok {
		return
	}
	stage := stagingKey(paste.Key)
	if _, ok := d.Docs.ReceiveInto(w, r, u, stage); !ok {
		return
	}
	defer d.Docs.DeleteStored(stage)
	size, err := d.Docs.Store.Size(stage)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	d.applyEdit(w, r, p, u, paste.Key, stage, size)
}

// applyEdit puts the content received under stage in place of key, under
// the key lock, see handleEditContent
func (d *Handler) applyEdit(w http.ResponseWriter, r *http.Request, p *auth.Principal, u handler.Uploader, key, stage string, size int64) {
	ctx, cancel := commitContext(r)
	defer cancel()
	unlock := d.Locks.Lock(key)
	defer unlock()
	kept := false
	defer func() {
		if !kept {
			d.Docs.RefundQuota(u, size)
		}
	}()

	paste, err := d.DB.PasteByKey(ctx, p.User.ID, key)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if reason := d.closedReason(paste, d.now()); reason != "" {
		writeClosed(w, reason)
		return
	}
	count, rev, err := d.DB.NextRevision(ctx, key)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if d.MaxRevisions > 0 && count >= d.MaxRevisions {
		writeTooManyRevisions(w, d.MaxRevisions)
		return
	}

	revKey := accounts.RevisionKey(key, rev)
	oldSize, err := d.Docs.CopyStored(u.IP, key, revKey, true)
	if err == nil {
		_, err = d.Docs.CopyStored(u.IP, stage, key, true)
	}
	if err != nil {
		d.Docs.DeleteStored(revKey)
		writeStoreError(w, err)
		return
	}

	edit := accounts.Edit{
		Key: key, UserID: p.User.ID, Rev: rev, StoreKey: revKey, OldSize: oldSize, NewSize: size,
		Until: paste.CreatedAt.Add(d.EditWindow), MaxRevisions: d.MaxRevisions,
	}
	if p.Session != nil {
		edit.SessionHash = p.Session.IDHash
	}
	edited, err := d.recordEdit(ctx, edit, d.accountLimits())
	if err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			// Deleted meanwhile: its content is queued for deletion, and
			// the new one written after it must not stay behind
			d.Docs.DeleteStored(key)
		} else if _, restoreErr := d.Docs.CopyStored(u.IP, revKey, key, true); restoreErr != nil {
			// Nothing records the new content: the old one goes back
			log.Error().Err(restoreErr).Int64("user_id", p.User.ID).Msg("Failed to restore a paste whose edit was not recorded")
		}
		d.Docs.DeleteStored(revKey)
		writeEditError(w, err)
		return
	}
	kept = true
	log.Info().Int64("user_id", p.User.ID).Int64("size", size).Int("rev", rev).Msg("Paste edited by its owner")
	writeJSON(w, http.StatusOK, d.item(edited))
}

// writeEditError answers an edit the database did not record
func writeEditError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, accounts.ErrEditSeen):
		writeClosed(w, reasonSeen)
	case errors.Is(err, accounts.ErrEditExpired):
		writeClosed(w, reasonExpired)
	case errors.Is(err, accounts.ErrTooManyRevisions):
		writeJSON(w, http.StatusConflict, errorBody{Error: "too_many_revisions",
			Message: "This paste keeps as many older versions as it may. Save it as a new version."})
	case errors.Is(err, accounts.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, accounts.ErrStorageLimit):
		WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "Your account storage is full. Delete some pastes to edit this one.")
	case errors.Is(err, accounts.ErrRevoked):
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "Your session ended during the upload. Log in again.")
	case errors.Is(err, accounts.ErrUserDisabled):
		WriteError(w, http.StatusForbidden, "disabled", "This account is disabled.")
	default:
		log.Warn().Err(err).Msg("Paste edit not recorded, the old content is back")
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "db_busy", "Saving failed and nothing changed. Try again shortly.")
	}
}

// copyRow is the row of a new paste of p with the title, language,
// visibility, expiry and tags of src
func copyRow(p *auth.Principal, src *accounts.Paste) accounts.NewPaste {
	row := accounts.NewPaste{
		UserID: p.User.ID, Title: src.Title, Language: src.Language, Visibility: src.Visibility,
		ExpiresAt: src.ExpiresAt, Tags: src.Tags,
	}
	if p.Session != nil {
		row.SessionHash = p.Session.IDHash
	}
	return row
}

// handleNewVersion answers POST /api/v1/pastes/{key}/versions: the body is
// a new paste under a new link, the next version of key with its title,
// language, visibility, expiry and tags. With ?delete_previous=1 the
// previous version is deleted in the same step. 201 with the new paste.
func (d *Handler) handleNewVersion(w http.ResponseWriter, r *http.Request) {
	deletePrevious := false
	switch r.URL.Query().Get("delete_previous") {
	case "", "0":
	case "1":
		deletePrevious = true
	default:
		WriteError(w, http.StatusBadRequest, "invalid_input", "delete_previous must be 0 or 1.")
		return
	}
	prev, ok := d.ownPaste(w, r)
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	u, ok := d.ownedUploader(w, r, p, true)
	if !ok {
		return
	}
	row := copyRow(p, prev)
	row.PrevKey, row.DeletePrevious = prev.Key, deletePrevious
	u.Commit = func(key string, size int64) error {
		ctx, cancel := commitContext(r)
		defer cancel()
		row.Key, row.Size = key, size
		if err := d.DB.InsertPaste(ctx, row, d.accountLimits()); err != nil {
			return d.refused(p, err, "Failed to record a new version")
		}
		return nil
	}
	key, ok := d.Docs.ReceiveInto(w, r, u, "")
	if !ok {
		return
	}
	if deletePrevious {
		d.Drainer.Kick()
	}
	log.Info().Int64("user_id", p.User.ID).Bool("delete_previous", deletePrevious).Msg("New version of a paste stored")
	d.writeCreated(w, r, key)
}

// writeCreated answers 201 with a paste just stored
func (d *Handler) writeCreated(w http.ResponseWriter, r *http.Request, key string) {
	created, err := d.DB.PasteByKey(r.Context(), user(r).ID, key)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d.item(created))
}

// handleDuplicate answers POST /api/v1/pastes/{key}/duplicate: a copy of
// the paste under a new link, with its title, language, visibility, expiry
// and tags. It counts against the quota and limits like an upload.
func (d *Handler) handleDuplicate(w http.ResponseWriter, r *http.Request) {
	src, ok := d.ownPaste(w, r)
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	u, ok := d.ownedUploader(w, r, p, true)
	if !ok {
		return
	}
	if u.MaxLength > 0 && src.Size > int64(u.MaxLength) {
		WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "This paste does not fit in what your account may still store.")
		return
	}
	if err := d.Docs.ChargeQuota(u, src.Size); err != nil {
		writeStoreError(w, err)
		return
	}
	key, size, err := d.Docs.CopyToNewKey(u.IP, src.Key, true)
	if err != nil {
		d.Docs.RefundQuota(u, src.Size)
		writeStoreError(w, err)
		return
	}
	ctx, cancel := commitContext(r)
	defer cancel()
	row := copyRow(p, src)
	row.Key, row.Size = key, size
	if err := d.DB.InsertPaste(ctx, row, d.accountLimits()); err != nil {
		d.Docs.DeleteStored(key)
		d.Docs.RefundQuota(u, src.Size)
		var refused *handler.UploadRefused
		if err = d.refused(p, err, "Failed to record a duplicate"); errors.As(err, &refused) {
			refused.Write(w)
			return
		}
		WriteError(w, http.StatusInternalServerError, "internal", "Something went wrong, try again.")
		return
	}
	log.Info().Int64("user_id", p.User.ID).Int64("size", size).Msg("Paste duplicated by its owner")
	d.writeCreated(w, r, key)
}

// handleRevision answers GET /api/v1/pastes/{key}/revisions/{n}: an older
// content of the paste, as plain text, to its owner only
func (d *Handler) handleRevision(w http.ResponseWriter, r *http.Request) {
	paste, ok := d.ownPaste(w, r)
	if !ok {
		return
	}
	rev, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || rev < 1 {
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	storeKey, err := d.DB.RevisionStoreKey(r.Context(), user(r).ID, paste.Key, rev)
	if err != nil {
		writeDBError(w, err)
		return
	}
	doc, err := d.Docs.OpenStored(handler.ClientIP(r), storeKey)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer doc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(doc.Size, 10))
	io.Copy(w, doc)
}
