package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
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
	// Register middlewares. The real IP middleware runs first so the logger
	// and the rate limiter see the resolved client address.
	s.mux.Use(realIPMiddleware(s.trustedProxies, s.realIPHeader))
	s.mux.Use(middleware.Logger)
	s.mux.Use(middleware.Recoverer)
	s.mux.Use(securityHeadersMiddleware)

	// Rate limiter, keyed by r.RemoteAddr as resolved by realIPMiddleware
	if s.config.RateLimiting.Enable {
		s.mux.Use(httprate.LimitBy(s.config.RateLimiting.Limit, time.Duration(s.config.RateLimiting.Window)*time.Second, keyByRemoteIP))
	}

	// Metrics live on the separate metrics_addr listener; answer 404 here
	// instead of the index page so old scrapers fail clearly.
	s.mux.Handle("/metrics", http.NotFoundHandler())

	// Register document handler
	documentHandler := handler.NewDocumentHandler(s.config.KeyLength, s.config.MaxLength, s.storage, s.keyGenerator)
	documentHandler.BaseURL = s.config.BaseURL
	documentHandler.RegisterRoutes(s.mux)

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

// realIPMiddleware rewrites r.RemoteAddr with the client IP from the given header,
// but only when the TCP peer is a trusted proxy. Headers from any other peer are ignored.
func realIPMiddleware(trusted []*net.IPNet, header string) func(http.Handler) http.Handler {
	header = http.CanonicalHeaderKey(header)
	if header == "" {
		header = "X-Forwarded-For"
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(trusted) > 0 {
				host, port, err := net.SplitHostPort(r.RemoteAddr)
				if err != nil {
					host, port = r.RemoteAddr, "0"
				}

				if peer := net.ParseIP(host); peer != nil && isTrustedProxy(peer, trusted) {
					if ip := clientIPFromHeaders(r.Header, header, trusted); ip != "" {
						if port == "" {
							port = "0"
						}
						r.RemoteAddr = net.JoinHostPort(ip, port)
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// keyByRemoteIP keys the rate limiter on r.RemoteAddr only, never on request headers
func keyByRemoteIP(r *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return httprate.CanonicalizeIP(host), nil
}

// securityHeadersMiddleware adds security headers to every response
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}
