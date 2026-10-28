// Package admin serves the admin page and its API. Every route answers 404
// to anyone but an admin with a session cookie; a request with any
// Authorization header gets 404 too, so no API token ever acts as an admin.
// State changes need the CSRF token, and the destructive ones a GitHub
// login younger than ADMIN_REAUTH. Every change writes an audit row.
//
// Moderation is report driven: a paste is found by its key or link, never
// by browsing, and its content is shown only with a reason, which is
// audited. Accounts whose GitHub id is in ADMIN_GITHUB_IDS cannot be
// disabled, revoked or deleted from here, the admin's own included: one
// admin cannot lock out another, and none can lock out themselves.
package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/abuse"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/armbian/ansi-hastebin/internal/dashboard"
	"github.com/armbian/ansi-hastebin/internal/mcpserver"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/armbian/ansi-hastebin/static"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

// scanEvery is the least time between two scans of the paste files
const scanEvery = 10 * time.Minute

// Handler serves /admin and /api/v1/admin
type Handler struct {
	Auth *auth.Service
	DB   *accounts.DB

	Version string
	Started time.Time

	// Config is the effective configuration, secrets shown only as set or unset
	Config []config.Setting

	StorageType string
	// Files is the file storage, nil for other backends
	Files *storage.FileStorage

	Docs     *handler.DocumentHandler
	Inflight *handler.InflightLimiter
	// MCP is the MCP endpoint, nil when it is off
	MCP *mcpserver.Handler

	// Gate is the access gate of registered pastes, for the content view
	Gate *dashboard.Gate

	// Drainer deletes the content of removed pastes; nil leaves it queued
	Drainer *accounts.Drainer

	// Events are the recent refusals, Bans the manual bans in memory
	Events *abuse.Events
	Bans   *abuse.Bans

	// Reauth is how old the GitHub login of a session may be for a
	// destructive action; 0 means DefaultReauth
	Reauth time.Duration

	// BaseURL is the public address: links to other hosts are refused
	BaseURL string

	// scan caches the count of paste files
	scanMu    sync.Mutex
	scanAt    time.Time
	scanCount int64
	scanBytes int64
	scanErr   error

	now func() time.Time
}

// DefaultReauth is how old a GitHub login may be for a destructive action
// when Reauth is not set, as ADMIN_REAUTH defaults
const DefaultReauth = 12 * time.Hour

// Per admin rate limits of the API
const (
	readLimit   = 300
	writeLimit  = 60
	limitWindow = time.Minute
)

// RegisterRoutes adds /admin, /admin/* and /api/v1/admin/*
func (h *Handler) RegisterRoutes(r chi.Router) {
	if h.now == nil {
		h.now = time.Now
	}
	reauth := h.Reauth
	if reauth <= 0 {
		reauth = DefaultReauth
	}
	recent := h.Auth.RequireRecentAuth(reauth, "/admin")
	limited := func(r *http.Request) {
		h.Events.Record(abuse.KindRateLimit, handler.ClientKey(handler.ClientIP(r)), "admin")
	}
	reads := dashboard.UserLimit(readLimit, limitWindow, limited)
	writes := dashboard.UserLimit(writeLimit, limitWindow, limited)

	// destructive needs a recent GitHub login on top of the CSRF check
	destructive := func(fn http.HandlerFunc) http.Handler { return recent(fn) }

	r.Group(func(r chi.Router) {
		// Every method goes through the admin check, so non-admins get 404,
		// never 405: each route takes every method and picks its own below
		r.Use(h.Auth.LoadSession, h.Auth.RequireAdmin, noStore)
		r.HandleFunc("/admin", h.page)
		r.HandleFunc("/admin/*", h.page)
		r.HandleFunc("/api/v1/admin/status", h.status)

		r.Group(func(r chi.Router) {
			r.Use(h.Auth.RequireCSRF, byMethod(reads, writes))
			r.HandleFunc("/api/v1/admin/pastes", methods{http.MethodGet: http.HandlerFunc(h.lookup)}.serve)
			r.HandleFunc("/api/v1/admin/pastes/{key}", methods{
				http.MethodGet:    http.HandlerFunc(h.lookup),
				http.MethodDelete: destructive(h.deletePaste),
			}.serve)
			r.HandleFunc("/api/v1/admin/pastes/{key}/content", methods{http.MethodPost: destructive(h.content)}.serve)
			r.HandleFunc("/api/v1/admin/pastes/{key}/abuse", methods{http.MethodPost: destructive(h.abusePaste)}.serve)

			r.HandleFunc("/api/v1/admin/users", methods{http.MethodGet: http.HandlerFunc(h.users)}.serve)
			r.HandleFunc("/api/v1/admin/users/{id}", methods{
				http.MethodGet:    http.HandlerFunc(h.user),
				http.MethodDelete: destructive(h.deleteUser),
			}.serve)
			r.HandleFunc("/api/v1/admin/users/{id}/disable", methods{http.MethodPost: destructive(h.disable)}.serve)
			r.HandleFunc("/api/v1/admin/users/{id}/enable", methods{http.MethodPost: http.HandlerFunc(h.enable)}.serve)
			r.HandleFunc("/api/v1/admin/users/{id}/revoke", methods{http.MethodPost: destructive(h.revoke)}.serve)

			r.HandleFunc("/api/v1/admin/bans", methods{
				http.MethodGet:  http.HandlerFunc(h.bans),
				http.MethodPost: destructive(h.addBan),
			}.serve)
			r.HandleFunc("/api/v1/admin/bans/{kind}/{target}", methods{http.MethodDelete: destructive(h.unban)}.serve)

			r.HandleFunc("/api/v1/admin/events", methods{http.MethodGet: http.HandlerFunc(h.events)}.serve)
			r.HandleFunc("/api/v1/admin/audit", methods{http.MethodGet: http.HandlerFunc(h.audit)}.serve)
		})
		r.HandleFunc("/api/v1/admin/*", auth.NotFound)
	})
}

// methods routes a request by its method, 405 for any other. GET also
// answers HEAD.
type methods map[string]http.Handler

func (m methods) serve(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if next, ok := m[method]; ok {
		next.ServeHTTP(w, r)
		return
	}
	allow := make([]string, 0, len(m)+1)
	for name := range m {
		allow = append(allow, name)
		if name == http.MethodGet {
			allow = append(allow, http.MethodHead)
		}
	}
	sort.Strings(allow)
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
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

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// readOnly answers 405 to anything but GET and HEAD, and reports whether it did
func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return false
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	return true
}

// serveFile writes an embedded file of the web page
func serveFile(w http.ResponseWriter, name, contentType string) {
	file, err := static.StaticFS.Open(name)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", contentType)
	io.Copy(w, file)
}

// page serves the admin page on /admin and every /admin/ path, so tabs can
// have their own address, and its script on /admin/admin.js
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	if readOnly(w, r) {
		return
	}
	if r.URL.Path == "/admin/admin.js" {
		serveFile(w, "admin.js", "text/javascript; charset=utf-8")
		return
	}
	serveFile(w, "admin.html", "text/html; charset=utf-8")
}

// Status is the answer of GET /api/v1/admin/status
type Status struct {
	Version       string    `json:"version"`
	Started       time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	ServerNow     time.Time `json:"server_now"`
	Storage       string    `json:"storage"`

	Disk       *Disk       `json:"disk"`
	FilePastes *FilePastes `json:"file_pastes"`
	Registered *Registered `json:"registered"`

	ReadBudget   Budget `json:"read_budget"`
	UploadBudget Budget `json:"upload_budget"`
	Slots        Slots  `json:"upload_slots"`
	Inflight     Load   `json:"inflight"`

	// MCP is null when the endpoint is off
	MCP *mcpserver.Stats `json:"mcp"`

	Config []config.Setting `json:"config"`
}

// Disk is the free space of the data dir of file storage
type Disk struct {
	Free        int64  `json:"free"`
	MinFreeDisk int64  `json:"min_free_disk"`
	Error       string `json:"error,omitempty"`
}

// FilePastes is the last count of the paste files, at most scanEvery old
type FilePastes struct {
	Count int64     `json:"count"`
	Bytes int64     `json:"bytes"`
	AsOf  time.Time `json:"as_of"`
	Error string    `json:"error,omitempty"`
}

// Registered counts what the accounts database holds
type Registered struct {
	accounts.Totals
	Error string `json:"error,omitempty"`
}

// Budget is the use of one half of the memory budget, 0 size for none
type Budget struct {
	InUse int64 `json:"in_use"`
	Size  int64 `json:"size"`
}

// Slots is the use of the large upload slots, 0 total for no limit
type Slots struct {
	InUse int `json:"in_use"`
	Total int `json:"total"`
}

// Load is the requests in progress and from how many clients
type Load struct {
	Requests int `json:"requests"`
	Clients  int `json:"clients"`
}

// status answers GET /api/v1/admin/status
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	if readOnly(w, r) {
		return
	}
	now := h.now()
	st := Status{
		Version:       h.Version,
		Started:       h.Started.UTC(),
		UptimeSeconds: int64(now.Sub(h.Started).Seconds()),
		ServerNow:     now.UTC(),
		Storage:       h.StorageType,
		Config:        h.Config,
	}

	if h.Files != nil {
		disk := &Disk{MinFreeDisk: h.Files.MinFree()}
		free, err := h.Files.FreeSpace()
		if err != nil {
			disk.Error = "free space unknown"
			log.Warn().Err(err).Msg("Admin status: failed to read the free disk space")
		}
		disk.Free = free
		st.Disk = disk
		st.FilePastes = h.filePastes(now)
	}

	if h.DB != nil {
		totals, err := h.DB.Totals(r.Context())
		reg := &Registered{Totals: totals}
		if err != nil {
			reg.Error = "database unavailable"
			log.Warn().Err(err).Msg("Admin status: failed to count accounts")
		}
		st.Registered = reg
	}

	if h.Docs != nil {
		st.ReadBudget = Budget{InUse: h.Docs.ReadMemory.InUse(), Size: h.Docs.ReadMemory.Size()}
		st.UploadBudget = Budget{InUse: h.Docs.UploadMemory.InUse(), Size: h.Docs.UploadMemory.Size()}
		st.Slots.InUse, st.Slots.Total = h.Docs.UploadSlots()
	}
	st.Inflight.Requests, st.Inflight.Clients = h.Inflight.Totals()
	if h.MCP != nil {
		stats := h.MCP.Stats()
		st.MCP = &stats
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

// filePastes returns the count of paste files, scanning the data dir again
// when the last scan is scanEvery old. Requests meanwhile wait for the scan.
func (h *Handler) filePastes(now time.Time) *FilePastes {
	h.scanMu.Lock()
	defer h.scanMu.Unlock()
	if h.scanAt.IsZero() || now.Sub(h.scanAt) >= scanEvery {
		h.scanCount, h.scanBytes, h.scanErr = h.Files.Usage()
		h.scanAt = now
		if h.scanErr != nil {
			log.Warn().Err(h.scanErr).Msg("Admin status: failed to count the paste files")
		}
	}
	fp := &FilePastes{Count: h.scanCount, Bytes: h.scanBytes, AsOf: h.scanAt.UTC()}
	if h.scanErr != nil {
		fp.Error = "scan failed"
	}
	return fp
}
