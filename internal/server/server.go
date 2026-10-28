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
	}
}

func (s *Server) RegisterRoutes() {
	// Register middlewares. The real IP middleware runs first so the in-flight
	// limit, the logger and the rate limiter see the resolved client address.
	s.mux.Use(realIPMiddleware(s.trustedProxies, s.realIPHeader))
	s.mux.Use(inflightMiddleware(handler.NewInflightLimiter(s.config.MaxInflightPerClient)))
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
	if s.config.MCPEnable {
		s.mux.Handle("/mcp", mcpserver.NewHandler(documentHandler, mcpserver.Options{
			MaxLength:            mcpMaxLength,
			MaxInflightPerClient: s.config.MCPMaxInflightPerClient,
		}))
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

	// Server features for the web page
	s.mux.Get("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{
			"mcp":            s.config.MCPEnable,
			"max_length":     s.config.MaxLength,
			"mcp_max_length": mcpMaxLength,
		})
	})

	// Register health check
	s.mux.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Register static files
	fileServer := http.FileServer(http.FS(static.StaticFS))

	s.mux.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
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
	})
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

	log.Info().Str("host", s.config.Host).Int("port", s.config.Port).Msg("Starting server")

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("Failed to start server")
	}
}

func (s *Server) Shutdown(ctx context.Context) {
	log.Info().Msg("Gracefully shutting down server")

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
	if path == r.URL.Path {
		return f.next.NewLogEntry(r)
	}
	logged := *r
	logged.RequestURI = path
	if r.URL.RawQuery != "" {
		logged.RequestURI += "?" + r.URL.RawQuery
	}
	return f.next.NewLogEntry(&logged)
}

// redactPath replaces the paste key in /raw/<key>, /documents/<key> and
// /<key> with #<first 8 hex digits of its SHA-256>, keeping any extension.
// The same key always gives the same hash, so requests can still be followed.
func redactPath(path string) string {
	rest := strings.TrimPrefix(path, "/")
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

	key, ext, hasExt := strings.Cut(rest, ".")
	sum := sha256.Sum256([]byte(key))
	redacted := prefix + "#" + hex.EncodeToString(sum[:4])
	if hasExt {
		redacted += "." + ext
	}
	return redacted
}

// isPastePage reports whether a one segment path names a paste rather than a
// route or a file of the web page
func isPastePage(name string) bool {
	switch name {
	case "documents", "log", "mcp", "health", "metrics", "api":
		return false
	}
	if file, err := static.StaticFS.Open(name); err == nil {
		file.Close()
		return false
	}
	return true
}
