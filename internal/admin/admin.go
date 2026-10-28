// Package admin serves the admin page and its API. Every route answers 404
// to anyone but an admin with a session cookie.
package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
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

	// scan caches the count of paste files
	scanMu    sync.Mutex
	scanAt    time.Time
	scanCount int64
	scanBytes int64
	scanErr   error

	now func() time.Time
}

// RegisterRoutes adds /admin, /admin/* and /api/v1/admin/*
func (h *Handler) RegisterRoutes(r chi.Router) {
	if h.now == nil {
		h.now = time.Now
	}
	r.Group(func(r chi.Router) {
		// Every method goes through the admin check, so non-admins get 404, never 405
		r.Use(h.Auth.LoadSession, h.Auth.RequireAdmin, noStore)
		r.HandleFunc("/admin", h.page)
		r.HandleFunc("/admin/*", h.page)
		r.HandleFunc("/api/v1/admin/status", h.status)
		r.HandleFunc("/api/v1/admin/*", auth.NotFound)
	})
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
