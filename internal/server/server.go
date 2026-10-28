package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/admin"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/armbian/ansi-hastebin/internal/dashboard"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/mcpserver"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/armbian/ansi-hastebin/static"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' https://cdnjs.cloudflare.com; " +
	"style-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net; " +
	"font-src 'self' https://cdn.jsdelivr.net; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'"

type Server struct {
	config         *config.Config
	storage        storage.Storage
	keyGenerator   keygenerator.KeyGenerator
	server         *http.Server
	metricsServer  *http.Server
	mux            *chi.Mux
	trustedProxies []*net.IPNet
	realIPHeader   string
	started        time.Time

	// Accounts is the accounts database, nil when accounts are off. Set it
	// before RegisterRoutes; the server closes it at shutdown.
	Accounts *accounts.DB

	// auth logs users in, nil when accounts are off
	auth *auth.Service

	// drainer deletes the content of deleted registered pastes, nil when accounts are off
	drainer *accounts.Drainer

	// stopAccounts stops the session sweeper
	stopAccounts context.CancelFunc

	// Version is the build version, logged at start and shown in /api/info
	Version string
}

// newHTTPServer creates an http.Server with safe timeouts
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}

func NewServer(config *config.Config, storage storage.Storage, keyGenerator keygenerator.KeyGenerator) *Server {
	mux := chi.NewRouter()
	httpServer := newHTTPServer(config.Host+":"+strconv.Itoa(config.Port), mux)

	trustedProxies, err := parseTrustedProxies(config.TrustedProxies)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to parse trusted proxies")
	}
	realIPHeader := http.CanonicalHeaderKey(config.RealIPHeader)
	if realIPHeader == "" {
		realIPHeader = "X-Forwarded-For"
	}
	log.Info().Int("trusted_proxies", len(trustedProxies)).Str("real_ip_header", realIPHeader).Msg("Trusted proxies configured")
	if config.BaseURL == "" {
		log.Warn().Msg("base_url is not set: links from /log and MCP are built from the request Host header, which the client controls. Set base_url (BASE_URL) to the public address")
	}

	var metricsServer *http.Server
	if config.MetricsAddr != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		metricsServer = newHTTPServer(config.MetricsAddr, metricsMux)
	}

	return &Server{
		config:         config,
		storage:        storage,
		keyGenerator:   keyGenerator,
		server:         httpServer,
		metricsServer:  metricsServer,
		mux:            mux,
		trustedProxies: trustedProxies,
		realIPHeader:   realIPHeader,
		started:        time.Now(),
		Version:        "dev",
	}
}

// newAuth sets up login and sessions on the accounts database. A bad login
// configuration stops the server.
func (s *Server) newAuth(mcpMaxLength int) *auth.Service {
	endpoints, err := auth.GitHubEndpoints(s.config.GitHubOAuthBase, s.config.GitHubAPIBase)
	if err != nil {
		log.Fatal().Err(err).Msg("Invalid GitHub endpoint override")
	}
	if s.config.GitHubOAuthBase != "" || s.config.GitHubAPIBase != "" {
		log.Warn().Str("oauth", endpoints.AuthURL).Str("api", endpoints.APIBase).
			Msg("GitHub endpoints are overridden: this is for testing against a fake GitHub only")
	}
	svc, err := auth.New(s.Accounts, auth.Config{
		ClientID:      s.config.GitHubClientID,
		ClientSecret:  s.config.GitHubClientSecret,
		BaseURL:       s.config.BaseURL,
		Endpoints:     endpoints,
		SessionTTL:    time.Duration(s.config.SessionTTL) * time.Second,
		SessionIdle:   time.Duration(s.config.SessionIdle) * time.Second,
		AdminIDs:      s.config.AdminGitHubIDs,
		MinAccountAge: time.Duration(s.config.RegisterMinAccountAge) * time.Second,
		ClientKey:     func(r *http.Request) string { return handler.ClientKey(handler.ClientIP(r)) },
		Limits: func(p *auth.Principal) any {
			if p != nil {
				return map[string]int{"max_length": s.config.RegisteredMaxLength, "mcp_max_length": mcpMaxLength}
			}
			return map[string]int{"max_length": s.config.MaxLength, "mcp_max_length": mcpMaxLength}
		},
	})
	if err != nil {
		log.Fatal().Err(err).Msg("Invalid login configuration")
	}
	log.Info().Bool("login", svc.LoginEnabled()).Bool("secure_cookies", svc.SecureCookies()).
		Int("admins", len(s.config.AdminGitHubIDs)).Int("session_ttl", s.config.SessionTTL).
		Int("session_idle", s.config.SessionIdle).Int("register_min_account_age", s.config.RegisterMinAccountAge).
		Msg("Accounts configured")
	return svc
}

func (s *Server) RegisterRoutes() {
	// Register middlewares. The real IP middleware runs first so the in-flight
	// limit, the logger and the rate limiter see the resolved client address.
	inflight := handler.NewInflightLimiter(s.config.MaxInflightPerClient)
	s.mux.Use(realIPMiddleware(s.trustedProxies, s.realIPHeader))
	s.mux.Use(inflightMiddleware(inflight))
	s.mux.Use(accessLogger())
	s.mux.Use(middleware.Recoverer)
	s.mux.Use(securityHeadersMiddleware)

	// Rate limiter, keyed by r.RemoteAddr as resolved by realIPMiddleware
	if s.config.RateLimiting.Enable {
		limit, window := rateLimitSettings(s.config.RateLimiting)
		s.mux.Use(httprate.LimitBy(limit, window, keyByRemoteIP))
	}

	// Metrics live on the separate metrics_addr listener; answer 404 here
	// instead of the index page so old scrapers fail clearly.
	s.mux.Handle("/metrics", http.NotFoundHandler())

	// Register document handler
	documentHandler := handler.NewDocumentHandler(s.config.KeyLength, s.config.MaxLength, s.storage, s.keyGenerator)
	documentHandler.BaseURL = s.config.BaseURL
	documentHandler.LimitUploads(s.config.MaxConcurrentUploads)
	documentHandler.LimitUploadsPerIP(s.config.MaxUploadsPerIP)
	documentHandler.SetMemoryBudget(int64(s.config.MemoryBudget))
	documentHandler.Quota = handler.NewUploadQuota(int64(s.config.UploadQuota), handler.DefaultQuotaWindow)
	if s.config.UploadTimeout != 0 {
		documentHandler.UploadTimeout = time.Duration(max(s.config.UploadTimeout, 0)) * time.Second
	}
	if s.config.NotFoundLimit > 0 {
		documentHandler.NotFound = handler.NewNotFoundLimiter(s.config.NotFoundLimit,
			time.Duration(s.config.NotFoundWindow)*time.Second, time.Duration(s.config.NotFoundBan)*time.Second)
	}
	documentHandler.RegisterRoutes(s.mux)

	// MCP endpoint for AI assistants. When it is off, answer 404 instead of the index page.
	mcpMaxLength := mcpserver.EffectiveMaxLength(s.config.MCPMaxLength, documentHandler)
	if wanted := mcpserver.MaxLength(s.config.MCPMaxLength, s.config.MaxLength); s.config.MCPEnable && mcpMaxLength < wanted {
		log.Warn().Int("mcp_max_length", wanted).Int("used", mcpMaxLength).Int("memory_budget", s.config.MemoryBudget).
			Msg("mcp_max_length lowered: an upload_log request is charged 6 times its size and must fit in one client's share of memory_budget (an eighth of it)")
	}
	var mcpHandler *mcpserver.Handler
	if s.config.MCPEnable {
		mcpHandler = mcpserver.NewHandler(documentHandler, mcpserver.Options{
			MaxLength:            mcpMaxLength,
			MaxInflightPerClient: s.config.MCPMaxInflightPerClient,
		})
		s.mux.Handle("/mcp", mcpHandler)
	} else {
		s.mux.Handle("/mcp", http.NotFoundHandler())
	}
	log.Info().Bool("mcp", s.config.MCPEnable).Int("mcp_max_length", mcpMaxLength).
		Int("max_concurrent_uploads", s.config.MaxConcurrentUploads).
		Int("max_uploads_per_ip", s.config.MaxUploadsPerIP).
		Int("max_inflight_per_client", s.config.MaxInflightPerClient).
		Int("mcp_max_inflight_per_client", s.config.MCPMaxInflightPerClient).
		Dur("upload_timeout", documentHandler.UploadTimeout).Int("not_found_limit", s.config.NotFoundLimit).
		Int("not_found_window", s.config.NotFoundWindow).Int("not_found_ban", s.config.NotFoundBan).
		Int("memory_budget", s.config.MemoryBudget).Int("upload_quota", s.config.UploadQuota).
		Msg("Upload options configured")

	// Accounts: login, sessions, the dashboard and the admin page. Without
	// them all of it answers 404 instead of the index page.
	if s.Accounts != nil {
		s.auth = s.newAuth(mcpMaxLength)
		s.auth.RegisterRoutes(s.mux)
		s.pinRegistered()
		s.drainer = accounts.NewDrainer(s.Accounts, s.deleteStored)
		documentHandler.UserQuota = handler.NewUploadQuota(int64(s.config.RegisteredUploadQuota), handler.DefaultQuotaWindow)
		dash := dashboard.New(s.auth, s.Accounts, dashboard.Limits{
			MaxPastes:   int64(s.config.UserMaxPastes),
			MaxBytes:    s.config.UserStorageLimit,
			MaxLength:   s.config.RegisteredMaxLength,
			UploadQuota: int64(s.config.RegisteredUploadQuota),
			MaxTokens:   s.config.MaxTokensPerUser,
		}, s.drainer)
		documentHandler.Uploaders = dash.Uploaders
		dash.RegisterRoutes(s.mux)
		log.Info().Int("registered_max_length", s.config.RegisteredMaxLength).
			Int("registered_upload_quota", s.config.RegisteredUploadQuota).
			Int64("user_storage_limit", s.config.UserStorageLimit).Int("user_max_pastes", s.config.UserMaxPastes).
			Int("max_tokens_per_user", s.config.MaxTokensPerUser).Msg("Registered user limits configured")
		files, _ := s.storage.(*storage.FileStorage)
		(&admin.Handler{
			Auth:        s.auth,
			DB:          s.Accounts,
			Version:     s.Version,
			Started:     s.started,
			Config:      s.config.Effective(),
			StorageType: s.config.Storage.Type,
			Files:       files,
			Docs:        documentHandler,
			Inflight:    inflight,
			MCP:         mcpHandler,
		}).RegisterRoutes(s.mux)
		serveDashboard := servePage("dashboard.html")
		s.mux.Get("/dashboard", serveDashboard)
		s.mux.Head("/dashboard", serveDashboard)
		s.mux.Get("/dashboard/*", serveDashboard)
		s.mux.Head("/dashboard/*", serveDashboard)
		s.mux.HandleFunc("/api/v1/*", auth.NotFound)
	} else {
		for _, pattern := range []string{"/auth/*", "/admin", "/admin/*", "/dashboard", "/dashboard/*", "/api/v1/*"} {
			s.mux.HandleFunc(pattern, auth.NotFound)
		}
	}

	// Server features for the web page
	s.mux.Get("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{
			"version":        s.Version,
			"mcp":            s.config.MCPEnable,
			"max_length":     s.config.MaxLength,
			"mcp_max_length": mcpMaxLength,
			"login":          s.auth != nil && s.auth.LoginEnabled(),
		})
	})

	// Register health check
	s.mux.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Register static files
	fileServer := http.FileServer(http.FS(static.StaticFS))

	// HEAD gets the same answer without the body, which net/http drops
	serveWeb := func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if privatePages[path] {
			http.NotFound(w, r)
			return
		}
		if _, err := static.StaticFS.Open(path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}

		// If file does not exist, serve index.html
		index, err := static.StaticFS.Open("index.html")
		if err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		defer index.Close()

		if _, err := io.Copy(w, index); err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}
	s.mux.Get("/*", serveWeb)
	s.mux.Head("/*", serveWeb)
}

// pinRegistered pins the content of every registered paste and revision in
// a store that expires pastes on its own clock, so they never expire. Pins
// live in memory, so this runs at every start.
func (s *Server) pinRegistered() {
	keeper, ok := s.storage.(storage.Keeper)
	if !ok {
		return
	}
	n := 0
	err := s.Accounts.EachStoreKey(context.Background(), func(key string) {
		keeper.Keep(key, true)
		n++
	})
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to read the registered pastes to keep them from expiring")
	}
	log.Info().Int("pinned", n).Msg("Registered pastes pinned, they never expire")
}

// deleteStored deletes one stored paste for the delete queue
func (s *Server) deleteStored(key string) error {
	deleter, ok := s.storage.(storage.Deleter)
	if !ok {
		return fmt.Errorf("storage %s cannot delete pastes", s.config.Storage.Type)
	}
	return deleter.Delete(key)
}

// privatePages are embedded pages served only on their own routes, behind
// their checks: /admin for admins, /dashboard when accounts are on
var privatePages = map[string]bool{"admin.html": true, "admin.js": true, "dashboard.html": true}

// servePage serves one embedded HTML page
func servePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := static.StaticFS.Open(name)
		if err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		defer page.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.Copy(w, page)
	}
}

const (
	defaultRateLimit  = 500
	defaultRateWindow = 15
)

// rateLimitSettings returns the request limit and window. A limit or window of
// 0 or less would block every request, so the defaults are used instead.
func rateLimitSettings(cfg config.RateLimitingConfig) (int, time.Duration) {
	limit, window := cfg.Limit, cfg.Window
	if limit <= 0 || window <= 0 {
		if limit <= 0 {
			limit = defaultRateLimit
		}
		if window <= 0 {
			window = defaultRateWindow
		}
		log.Warn().Int("limit", cfg.Limit).Int("window", cfg.Window).
			Int("used_limit", limit).Int("used_window", window).
			Msg("Rate limiting is on but limit or window is not set, using the defaults")
	}
	return limit, time.Duration(window) * time.Second
}

func (s *Server) Start() {
	if s.metricsServer != nil {
		go func() {
			log.Info().Str("addr", s.config.MetricsAddr).Msg("Starting metrics server")

			// A metrics failure must not take the paste server down
			if err := s.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error().Err(err).Str("addr", s.config.MetricsAddr).Msg("Failed to start metrics server")
			}
		}()
	}

	if s.auth != nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.stopAccounts = cancel
		go s.auth.Run(ctx)
		go s.drainer.Run(ctx)
	}

	log.Info().Str("version", s.Version).Str("host", s.config.Host).Int("port", s.config.Port).Msg("Starting server")

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("Failed to start server")
	}
}

func (s *Server) Shutdown(ctx context.Context) {
	log.Info().Msg("Gracefully shutting down server")

	// The delete queue and session sweeper stop before the store closes
	if s.stopAccounts != nil {
		s.stopAccounts()
	}

	if err := s.storage.Close(); err != nil {
		log.Error().Err(err).Msg("Failed to close storage")
	}

	if err := s.server.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("Failed to shutdown server")
	}

	if s.metricsServer != nil {
		if err := s.metricsServer.Shutdown(ctx); err != nil {
			log.Error().Err(err).Msg("Failed to shutdown metrics server")
		}
	}

	if s.Accounts != nil {
		if err := s.Accounts.Close(); err != nil {
			log.Error().Err(err).Msg("Failed to close the accounts database")
		}
	}
}

// parseTrustedProxies parses a list of CIDRs or bare IPs
func parseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		if strings.Contains(entry, "/") {
			_, ipNet, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: %w", entry, err)
			}
			nets = append(nets, ipNet)
			continue
		}

		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("invalid trusted proxy %q", entry)
		}
		if ip4 := ip.To4(); ip4 != nil {
			nets = append(nets, &net.IPNet{IP: ip4, Mask: net.CIDRMask(32, 32)})
		} else {
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
		}
	}
	return nets, nil
}

func isTrustedProxy(ip net.IP, trusted []*net.IPNet) bool {
	for _, ipNet := range trusted {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIPFromHeaders returns the client IP set by a trusted proxy, or "" if none is valid.
// Only the configured header is read. X-Forwarded-For is walked right to left,
// skipping trusted proxies; any other header must hold a single IP.
func clientIPFromHeaders(header http.Header, name string, trusted []*net.IPNet) string {
	if name != "X-Forwarded-For" {
		if ip := net.ParseIP(strings.TrimSpace(header.Get(name))); ip != nil {
			return ip.String()
		}
		return ""
	}

	xff := strings.Join(header.Values("X-Forwarded-For"), ",")
	if xff == "" {
		return ""
	}

	entries := strings.Split(xff, ",")
	for i := len(entries) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(entries[i]))
		if ip == nil {
			return ""
		}
		if !isTrustedProxy(ip, trusted) {
			return ip.String()
		}
	}
	return ""
}

// untrustedHeaderWarnInterval is the least time between two warnings about a
// client IP header sent by a peer that is not a trusted proxy
const untrustedHeaderWarnInterval = time.Minute

// realIPMiddleware rewrites r.RemoteAddr with the client IP from the given header,
// but only when the TCP peer is a trusted proxy. Headers from any other peer are
// ignored, with a warning at most once a minute, as that usually means the proxy
// address in trusted_proxies is wrong.
func realIPMiddleware(trusted []*net.IPNet, header string) func(http.Handler) http.Handler {
	header = http.CanonicalHeaderKey(header)
	if header == "" {
		header = "X-Forwarded-For"
	}
	warn := &rateLimitedWarning{interval: untrustedHeaderWarnInterval}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, port, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host, port = r.RemoteAddr, "0"
			}

			peer := net.ParseIP(host)
			if peer != nil && isTrustedProxy(peer, trusted) {
				if ip := clientIPFromHeaders(r.Header, header, trusted); ip != "" {
					if port == "" {
						port = "0"
					}
					r.RemoteAddr = net.JoinHostPort(ip, port)
				}
			} else if name := clientIPHeader(r.Header, header); name != "" && warn.allow(time.Now()) {
				log.Warn().Str("peer", host).Str("header", name).Int("trusted_proxies", len(trusted)).
					Msg("Ignored a client IP header from a peer that is not a trusted proxy. If this peer is your reverse proxy, add its address to trusted_proxies")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIPHeader returns the name of the client IP header a request carries:
// the configured one, or CF-Connecting-IP, "" for none
func clientIPHeader(h http.Header, configured string) string {
	for _, name := range []string{configured, "CF-Connecting-IP"} {
		if h.Get(name) != "" {
			return name
		}
	}
	return ""
}

// rateLimitedWarning lets a warning through at most once per interval
type rateLimitedWarning struct {
	interval time.Duration
	last     atomic.Int64 // unix nanoseconds of the last warning
}

func (w *rateLimitedWarning) allow(now time.Time) bool {
	last := w.last.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < w.interval {
		return false
	}
	return w.last.CompareAndSwap(last, now.UnixNano())
}

// inflightRetryAfter is the Retry-After, in seconds, of a request refused for
// too many requests in progress
const inflightRetryAfter = "2"

// inflightMiddleware refuses a request with 429 when its client already has
// as many requests in progress as the limiter allows. Every route counts,
// static files too: each open request holds a connection, its buffers and,
// for a client that reads slowly, kernel socket buffers. A nil limiter lets
// everything through.
func inflightMiddleware(limiter *handler.InflightLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if limiter == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			release, ok := limiter.Acquire(handler.ClientIP(r))
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", inflightRetryAfter)
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintln(w, `{"message": "Too many requests in progress from your address, try again shortly."}`)
				return
			}
			defer release()
			next.ServeHTTP(w, r)
		})
	}
}

// keyByRemoteIP keys the rate limiter on r.RemoteAddr only, never on request
// headers, with an IPv6 /64 counted as one client like every other limit
func keyByRemoteIP(r *http.Request) (string, error) {
	return handler.ClientKey(handler.ClientIP(r)), nil
}

// securityHeadersMiddleware adds security headers to every response
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// Pastes are public by link only: keep them, and everything else, out of search engines
		h.Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

// accessLogger logs each request like chi's Logger, with paste keys replaced
// by a short hash, so the access log does not hand out links to every paste
func accessLogger() func(http.Handler) http.Handler {
	return middleware.RequestLogger(&redactingFormatter{
		next: &middleware.DefaultLogFormatter{Logger: stdlog.New(os.Stdout, "", stdlog.LstdFlags), NoColor: false},
	})
}

// redactingFormatter hides paste keys in the request line of the access log
type redactingFormatter struct {
	next middleware.LogFormatter
}

func (f *redactingFormatter) NewLogEntry(r *http.Request) middleware.LogEntry {
	path := redactPath(r.URL.Path)
	// The query of a login route holds the OAuth code and state, or a
	// return path that may be a paste link; a dashboard search may hold a
	// key; and a token sent by mistake in a query must not reach the log
	hideQuery := strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/api/v1/") ||
		strings.Contains(r.URL.RawQuery, "hbp_")
	if path == r.URL.Path && !hideQuery {
		return f.next.NewLogEntry(r)
	}
	logged := *r
	logged.RequestURI = path
	if r.URL.RawQuery != "" && !hideQuery {
		logged.RequestURI += "?" + r.URL.RawQuery
	}
	return f.next.NewLogEntry(&logged)
}

// keyRoutes are the routes whose next path segment is a paste key, which may
// be followed by more path
var keyRoutes = []string{"api/v1/admin/pastes/", "api/v1/pastes/", "dashboard/p/"}

// redactPath replaces the paste key in /raw/<key>, /documents/<key>, /<key>,
// /api/v1/pastes/<key>/..., /api/v1/admin/pastes/<key>/... and
// /dashboard/p/<key>/... with #<first 8 hex digits of its SHA-256>, keeping
// any extension and the rest of the path. The same key always gives the same
// hash, so requests can still be followed.
func redactPath(path string) string {
	rest := strings.TrimPrefix(path, "/")
	for _, route := range keyRoutes {
		after, ok := strings.CutPrefix(rest, route)
		if !ok {
			continue
		}
		segment, tail, hasTail := strings.Cut(after, "/")
		// POST /api/v1/pastes/delete is the bulk delete, not a paste
		if segment == "" || (route == "api/v1/pastes/" && segment == "delete" && !hasTail) {
			return path
		}
		redacted := "/" + route + redactKey(segment)
		if hasTail {
			redacted += "/" + tail
		}
		return redacted
	}

	prefix := "/"
	for _, route := range []string{"raw/", "documents/"} {
		if strings.HasPrefix(rest, route) {
			prefix += route
			rest = strings.TrimPrefix(rest, route)
			break
		}
	}
	if rest == "" || strings.Contains(rest, "/") {
		return path
	}
	if prefix == "/" && !isPastePage(rest) {
		return path
	}
	return prefix + redactKey(rest)
}

// redactKey turns a path segment holding a key, cut at the first dot like
// every route cuts it, into #<first 8 hex digits of its SHA-256> plus any extension
func redactKey(segment string) string {
	key, ext, hasExt := strings.Cut(segment, ".")
	sum := sha256.Sum256([]byte(key))
	redacted := "#" + hex.EncodeToString(sum[:4])
	if hasExt {
		redacted += "." + ext
	}
	return redacted
}

// isPastePage reports whether a one segment path names a paste rather than a
// route or a file of the web page
func isPastePage(name string) bool {
	switch name {
	case "documents", "log", "mcp", "health", "metrics", "api", "dashboard", "admin", "auth":
		return false
	}
	if file, err := static.StaticFS.Open(name); err == nil {
		file.Close()
		return false
	}
	return true
}
