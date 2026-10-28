package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/armbian/ansi-hastebin/internal/storage"
)

// Op is what a reader does with a paste
type Op int

const (
	// OpRead delivers the content: GET /documents, GET /raw (whole or a
	// range), MCP read_paste and search_paste. A read by anyone but the
	// owner counts as a view.
	OpRead Op = iota

	// OpHead tells only whether the paste exists and its size: HEAD. It
	// never counts as a view.
	OpHead
)

var (
	// ErrDenied is returned by an AccessGate for a paste the reader may not
	// see: private and not theirs, past its expiry, or removed while
	// private. It is answered exactly like a missing paste.
	ErrDenied = errors.New("paste not found")

	// ErrRemoved is returned by an AccessGate for a public paste a
	// moderator removed: 410 Gone
	ErrRemoved = errors.New("paste removed by a moderator")
)

// Meta is what an AccessGate knows about a paste it let through
type Meta struct {
	// Owner is set when the reader owns the paste
	Owner bool

	// EditableUntil is when the owner can no longer edit the paste under
	// the same link, the zero time when they cannot now
	EditableUntil time.Time

	// ServerNow is the server time of the check
	ServerNow time.Time

	// OnDeliver, when set, runs once the content is delivered to a reader
	// who is not the owner: it counts the view
	OnDeliver func()
}

// Delivered records that the content went out. It is safe on a nil Meta.
func (m *Meta) Delivered() {
	if m != nil && m.OnDeliver != nil {
		m.OnDeliver()
	}
}

// AccessGate decides who may read a paste, before the store is touched.
// Check returns ErrDenied, ErrRemoved, any other error when it cannot tell
// (the read then fails closed with 500), or the Meta of a paste the reader
// may see. A paste the gate knows nothing about, an anonymous one, comes
// back with an empty Meta: it is public.
type AccessGate interface {
	Check(ctx context.Context, key string, p *auth.Principal, op Op) (*Meta, error)
}

// removedMessage is the body of the 410 for a public paste a moderator removed
const removedMessage = `{"message": "This paste was removed by a moderator."}`

// gate checks the reader of r against Access before key is looked up in the
// store. It returns false when the request was answered: a refused bearer
// token, a paste the reader may not see (exactly as a missing one, Miss
// counted and the store asked for its size, so neither the answer nor the
// time it takes tells them apart), a removed paste or a failed check.
func (h *DocumentHandler) gate(w http.ResponseWriter, r *http.Request, ip, key string, op Op) (*Meta, bool) {
	if h.Access == nil {
		return nil, true
	}
	var p *auth.Principal
	if h.Readers != nil {
		var ok bool
		if p, ok = h.Readers(w, r); !ok {
			return nil, false
		}
	}
	meta, err := h.Access.Check(r.Context(), key, p, op)
	if err == nil {
		return meta, true
	}
	switch {
	case errors.Is(err, ErrDenied):
		h.Store.Size(key)
		h.writeReadError(w, ip, key, storage.ErrNotFound)
	case errors.Is(err, ErrRemoved):
		http.Error(w, removedMessage, http.StatusGone)
	default:
		h.writeReadError(w, ip, key, err)
	}
	return nil, false
}

// OpenDocumentAs opens a paste for principal p, nil for anonymous, at
// client address ip, see openDocument. The AccessGate decides first; a
// paste p may not see is storage.ErrNotFound like a missing one, and counts
// against ip the same way. A banned ip gets a *BannedError, a removed public
// paste ErrRemoved. A delivered read counts as a view.
func (h *DocumentHandler) OpenDocumentAs(ctx context.Context, p *auth.Principal, ip, key string, stream, cost int64) (*Document, error) {
	if left, banned := h.NotFound.Banned(ip); banned {
		return nil, &BannedError{RetryAfter: left}
	}
	var meta *Meta
	var err error
	if h.Access != nil {
		meta, err = h.Access.Check(ctx, key, p, OpRead)
		if errors.Is(err, ErrDenied) {
			h.Store.Size(key)
			err = storage.ErrNotFound
		}
	}
	var doc *Document
	if err == nil {
		doc, err = h.openDocument(ip, key, stream, cost)
	}
	if errors.Is(err, storage.ErrNotFound) {
		if left, banned := h.NotFound.Miss(ip); banned {
			return nil, &BannedError{RetryAfter: left}
		}
	}
	if err != nil {
		return nil, err
	}
	meta.Delivered()
	return doc, nil
}

// ownerJSON is the end of the /documents answer for the owner of a paste:
// owner, editable_until (null when editing is over) and server_now, in UTC
// RFC 3339. It is empty for everyone else.
func ownerJSON(meta *Meta) string {
	if meta == nil || !meta.Owner {
		return ""
	}
	until := "null"
	if !meta.EditableUntil.IsZero() {
		until = `"` + meta.EditableUntil.UTC().Format(time.RFC3339) + `"`
	}
	return `,"owner":true,"editable_until":` + until + `,"server_now":"` + meta.ServerNow.UTC().Format(time.RFC3339) + `"`
}

// noStoreKeeper keeps Cache-Control on an answer of http.ServeContent,
// whose error answers (416 and others) drop it
type noStoreKeeper struct {
	http.ResponseWriter
	value string
}

func (k *noStoreKeeper) WriteHeader(code int) {
	if k.Header().Get("Cache-Control") == "" {
		k.Header().Set("Cache-Control", k.value)
	}
	k.ResponseWriter.WriteHeader(code)
}

// ReadFrom keeps sendfile for the body
func (k *noStoreKeeper) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(k.ResponseWriter, src)
}

// Unwrap lets http.ResponseController reach the connection
func (k *noStoreKeeper) Unwrap() http.ResponseWriter {
	return k.ResponseWriter
}

// keepNoStore wraps w so that the Cache-Control it carries now survives
// http.ServeContent
func keepNoStore(w http.ResponseWriter) http.ResponseWriter {
	value := w.Header().Get("Cache-Control")
	if value == "" {
		return w
	}
	return &noStoreKeeper{ResponseWriter: w, value: value}
}
