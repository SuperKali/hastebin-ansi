package server

import (
	"context"
	"encoding/json"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func TestParseTrustedProxies(t *testing.T) {
	nets, err := parseTrustedProxies([]string{"172.18.0.0/16", " 10.0.0.1 ", "", "fd00::/8", "::1"})
	require.NoError(t, err)
	require.Len(t, nets, 4)

	_, err = parseTrustedProxies([]string{"not-an-ip"})
	require.Error(t, err)

	_, err = parseTrustedProxies([]string{"10.0.0.0/33"})
	require.Error(t, err)
}

func TestRealIPMiddleware(t *testing.T) {
	trusted, err := parseTrustedProxies([]string{"172.18.0.0/16", "10.0.0.1"})
	require.NoError(t, err)

	tests := []struct {
		name       string
		noTrusted  bool
		header     string
		remoteAddr string
		headers    map[string][]string
		want       string
	}{
		{
			name:       "untrusted peer ignores X-Forwarded-For",
			remoteAddr: "203.0.113.7:4444",
			headers:    map[string][]string{"X-Forwarded-For": {"198.51.100.1"}},
			want:       "203.0.113.7:4444",
		},
		{
			name:       "untrusted peer ignores configured CF header",
			header:     "CF-Connecting-IP",
			remoteAddr: "203.0.113.7:4444",
			headers:    map[string][]string{"CF-Connecting-IP": {"198.51.100.1"}},
			want:       "203.0.113.7:4444",
		},
		{
			name:       "no trusted proxies configured ignores headers",
			noTrusted:  true,
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"X-Forwarded-For": {"198.51.100.1"}},
			want:       "172.18.0.1:4444",
		},
		{
			name:       "trusted peer without headers keeps peer",
			remoteAddr: "172.18.0.1:4444",
			want:       "172.18.0.1:4444",
		},
		{
			name:       "default ignores CF and X-Real-IP from trusted peer",
			remoteAddr: "172.18.0.1:4444",
			headers: map[string][]string{
				"CF-Connecting-IP": {"5.6.7.8"},
				"X-Real-IP":        {"5.6.7.9"},
				"X-Forwarded-For":  {"198.51.100.3"},
			},
			want: "198.51.100.3:4444",
		},
		{
			name:       "default with only spoofed CF header keeps peer",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"CF-Connecting-IP": {"5.6.7.8"}},
			want:       "172.18.0.1:4444",
		},
		{
			name:       "X-Forwarded-For right to left ignores spoofed leftmost entry",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"X-Forwarded-For": {"1.2.3.4, 198.51.100.3, 172.18.0.5, 10.0.0.1"}},
			want:       "198.51.100.3:4444",
		},
		{
			name:       "X-Forwarded-For across multiple header lines",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"X-Forwarded-For": {"1.2.3.4", "198.51.100.3"}},
			want:       "198.51.100.3:4444",
		},
		{
			name:       "X-Forwarded-For with only trusted entries keeps peer",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"X-Forwarded-For": {"172.18.0.5, 10.0.0.1"}},
			want:       "172.18.0.1:4444",
		},
		{
			name:       "invalid X-Forwarded-For candidate keeps peer",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"X-Forwarded-For": {"198.51.100.3, garbage"}},
			want:       "172.18.0.1:4444",
		},
		{
			name:       "configured CF header from trusted peer",
			header:     "CF-Connecting-IP",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"CF-Connecting-IP": {"198.51.100.1"}, "X-Forwarded-For": {"198.51.100.3"}},
			want:       "198.51.100.1:4444",
		},
		{
			name:       "configured CF header never falls back to X-Forwarded-For",
			header:     "CF-Connecting-IP",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"X-Forwarded-For": {"198.51.100.3"}, "X-Real-IP": {"198.51.100.2"}},
			want:       "172.18.0.1:4444",
		},
		{
			name:       "configured header with invalid value keeps peer",
			header:     "CF-Connecting-IP",
			remoteAddr: "172.18.0.1:4444",
			headers:    map[string][]string{"CF-Connecting-IP": {"not-an-ip"}},
			want:       "172.18.0.1:4444",
		},
		{
			name:       "configured X-Real-IP, case insensitive name",
			header:     "x-real-ip",
			remoteAddr: "10.0.0.1",
			headers:    map[string][]string{"X-Real-IP": {"198.51.100.2"}, "CF-Connecting-IP": {"5.6.7.8"}},
			want:       "198.51.100.2:0",
		},
		{
			name:       "IPv6 client from trusted peer",
			header:     "CF-Connecting-IP",
			remoteAddr: "10.0.0.1:4444",
			headers:    map[string][]string{"CF-Connecting-IP": {"2001:db8::1"}},
			want:       "[2001:db8::1]:4444",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxies := trusted
			if tt.noTrusted {
				proxies = nil
			}

			var got string
			h := realIPMiddleware(proxies, tt.header)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.RemoteAddr
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			for name, values := range tt.headers {
				for _, value := range values {
					req.Header.Add(name, value)
				}
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			require.Equal(t, tt.want, got)
		})
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	h := securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	require.Equal(t, contentSecurityPolicy, rec.Header().Get("Content-Security-Policy"))
	require.Equal(t, "noindex, nofollow", rec.Header().Get("X-Robots-Tag"))
}

func TestKeyByRemoteIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.1:4444"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "1.2.3.4")

	key, err := keyByRemoteIP(req)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.1", key)

	// Addresses of one IPv6 /64 share a bucket
	keys := map[string]bool{}
	for _, addr := range []string{"[2001:db8::1]:1", "[2001:db8::2]:2"} {
		req.RemoteAddr = addr
		key, err := keyByRemoteIP(req)
		require.NoError(t, err)
		keys[key] = true
	}
	require.Len(t, keys, 1)
}

func TestOldShortKeysStillWork(t *testing.T) {
	// New keys are 16 characters by default; a paste stored under an old 10 character key still reads
	cfg := config.NewConfig("nonexistent.yaml")
	require.Equal(t, 16, cfg.KeyLength)
	cfg.MCPEnable = true
	store := storage.NewFileStorage(t.TempDir(), 0)
	srv := NewServer(cfg, store, keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	require.NoError(t, store.Set("abcdefghij", "old paste", false))

	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	rec := get(http.MethodGet, "/raw/abcdefghij")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "old paste", rec.Body.String())
	rec = get(http.MethodGet, "/documents/abcdefghij.log")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"key":"abcdefghij"`)
	require.Equal(t, http.StatusOK, get(http.MethodHead, "/raw/abcdefghij").Code)
	// The web page for the key is the index, which loads it through /documents
	require.Contains(t, get(http.MethodGet, "/abcdefghij").Body.String(), `src="app-init.js`)

	// MCP accepts the old key too
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_paste","arguments":{"key_or_url":"https://paste.example.com/abcdefghij"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "old paste")

	// New pastes get 16 character keys
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/documents", strings.NewReader("new paste")))
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Key string `json:"key"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out.Key, 16)
}

func TestNotFoundBanConfigured(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024,
		NotFoundLimit: 2, NotFoundWindow: 60, NotFoundBan: 120}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	get := func(path, ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":1000"
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusNotFound, get("/raw/nope1", "203.0.113.5").Code)
	require.Equal(t, http.StatusNotFound, get("/raw/nope2", "203.0.113.5").Code)
	rec := get("/raw/nope3", "203.0.113.5")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "120", rec.Header().Get("Retry-After"))

	// The web page and static files stay reachable, and other clients are not affected
	require.Equal(t, http.StatusOK, get("/", "203.0.113.5").Code)
	require.Equal(t, http.StatusOK, get("/app-init.js", "203.0.113.5").Code)
	require.Equal(t, http.StatusOK, get("/api/info", "203.0.113.5").Code)
	require.Equal(t, http.StatusNotFound, get("/raw/nope1", "203.0.113.6").Code)
}

func TestServerRoutes(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	require.Equal(t, 10*time.Second, srv.server.ReadHeaderTimeout)
	require.Equal(t, 1<<16, srv.server.MaxHeaderBytes)
	require.Nil(t, srv.metricsServer)

	// /metrics is not served on the public listener
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NotContains(t, rec.Body.String(), "go_goroutines")
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	// The index loads the init script instead of an inline one
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Contains(t, rec.Body.String(), `src="app-init.js`)
	require.NotContains(t, rec.Body.String(), "<script type=\"text/javascript\">")

	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app-init.js", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.Contains(rec.Body.String(), "handlePop"))
}

func TestMetricsServer(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, MetricsAddr: "127.0.0.1:0"}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	require.NotNil(t, srv.metricsServer)
	require.Equal(t, 60*time.Second, srv.metricsServer.WriteTimeout)

	rec := httptest.NewRecorder()
	srv.metricsServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "go_goroutines")
}

func TestMetricsServerBindFailure(t *testing.T) {
	// A bad metrics address must not stop the paste server
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, MetricsAddr: "256.0.0.1:1"}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	done := make(chan struct{})
	go func() {
		srv.Start()
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

// mcpRequest is an MCP tools/list request
func mcpRequest() *http.Request {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return req
}

func TestMCPRoute(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024, MCPEnable: enabled}
		srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
		srv.RegisterRoutes()

		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, mcpRequest())
		if enabled {
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), "upload_log")
			require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		} else {
			require.Equal(t, http.StatusNotFound, rec.Code)
			require.NotContains(t, rec.Body.String(), "<html")

			// GET must not fall through to the index page either
			rec = httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
			require.Equal(t, http.StatusNotFound, rec.Code)
		}

		rec = httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/info", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var info struct {
			MCP          bool `json:"mcp"`
			MaxLength    int  `json:"max_length"`
			MCPMaxLength int  `json:"mcp_max_length"`
		}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
		require.Equal(t, enabled, info.MCP)
		require.Equal(t, 1024, info.MaxLength)
		// The 4 MiB MCP default never goes above max_length
		require.Equal(t, 1024, info.MCPMaxLength)
	}
}

func TestMCPBodyLimit(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024, MCPEnable: true}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	post := func(size int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("a", size)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		return rec
	}

	// The limit is twice the MCP upload limit, for the JSON escaping, plus 1 MiB
	rec := post(2*1024 + (1 << 20) + 1)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "curl --data-binary @file")

	// A smaller mcp_max_length lowers it
	cfg = &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024, MCPEnable: true, MCPMaxLength: 100}
	srv = NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()
	rec = post(2*100 + (1 << 20) + 1)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestLLMsTxt(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/llms.txt", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	require.Contains(t, rec.Body.String(), "/mcp")
}

func TestRateLimitSettings(t *testing.T) {
	limit, window := rateLimitSettings(config.RateLimitingConfig{Enable: true, Limit: 100, Window: 60})
	require.Equal(t, 100, limit)
	require.Equal(t, time.Minute, window)

	for _, rl := range []config.RateLimitingConfig{
		{Enable: true},
		{Enable: true, Limit: -1, Window: 15},
		{Enable: true, Limit: 500, Window: 0},
	} {
		limit, window = rateLimitSettings(rl)
		require.Equal(t, defaultRateLimit, limit)
		require.Equal(t, defaultRateWindow*time.Second, window)
	}

	// Rate limiting on without numbers still serves requests
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024,
		RateLimiting: config.RateLimitingConfig{Enable: true}}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		require.Equal(t, http.StatusOK, rec.Code)
	}
}

func TestRedactPath(t *testing.T) {
	hash := redactPath("/raw/abcdefghijklmnop")
	require.Regexp(t, `^/raw/#[0-9a-f]{8}$`, hash)
	require.NotContains(t, hash, "abcdefghijklmnop")

	// The same key gives the same hash on every route, and the extension stays
	id := strings.TrimPrefix(hash, "/raw/")
	require.Equal(t, "/documents/"+id, redactPath("/documents/abcdefghijklmnop"))
	require.Equal(t, "/"+id+".log", redactPath("/abcdefghijklmnop.log"))
	require.NotEqual(t, hash, redactPath("/raw/otherkey"))

	// Dashboard and API routes hide the key and keep the rest of the path
	require.Equal(t, "/api/v1/pastes/"+id, redactPath("/api/v1/pastes/abcdefghijklmnop"))
	require.Equal(t, "/api/v1/pastes/"+id+"/revisions/2", redactPath("/api/v1/pastes/abcdefghijklmnop/revisions/2"))
	require.Equal(t, "/api/v1/pastes/"+id+"/content", redactPath("/api/v1/pastes/abcdefghijklmnop/content"))
	require.Equal(t, "/api/v1/admin/pastes/"+id+"/abuse", redactPath("/api/v1/admin/pastes/abcdefghijklmnop/abuse"))
	require.Equal(t, "/dashboard/p/"+id, redactPath("/dashboard/p/abcdefghijklmnop"))
	require.Equal(t, "/dashboard/p/"+id+".log", redactPath("/dashboard/p/abcdefghijklmnop.log"))
	for _, path := range []string{"/api/v1/pastes/abcdefghijklmnop/versions", "/api/v1/admin/pastes/abcdefghijklmnop",
		"/dashboard/p/abcdefghijklmnop/"} {
		require.NotContains(t, redactPath(path), "abcdefghijklmnop", path)
	}

	// Routes and web page files are kept
	for _, path := range []string{"/", "/documents", "/log", "/mcp", "/health", "/metrics", "/api/info",
		"/application.js", "/favicon.ico", "/llms.txt", "/robots.txt", "/raw/", "/raw/a/b",
		"/api/v1/pastes", "/api/v1/pastes/", "/api/v1/pastes/delete", "/api/v1/admin/pastes/",
		"/api/v1/session", "/api/v1/tokens/3", "/dashboard", "/admin", "/dashboard/p/"} {
		require.Equal(t, path, redactPath(path))
	}
}

func TestHeadWebPage(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)

	// The index, a paste page and a file of the web page answer HEAD like GET,
	// without the body, which net/http drops
	for _, path := range []string{"/", "/abcdefghij", "/abcdefghij.log", "/application.js", "/robots.txt"} {
		get, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		body, err := io.ReadAll(get.Body)
		get.Body.Close()
		require.NoError(t, err)
		head, err := http.Head(ts.URL + path)
		require.NoError(t, err)
		head.Body.Close()

		require.Equal(t, http.StatusOK, get.StatusCode, path)
		require.Equal(t, http.StatusOK, head.StatusCode, path)
		require.NotEmpty(t, body, path)
		get.Header.Del("Date")
		head.Header.Del("Date")
		require.Equal(t, get.Header, head.Header, path)
	}
}

func TestVersionAndNoStore(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024, MCPEnable: true}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	require.Equal(t, "dev", srv.Version)
	srv.Version = "v1.2.3"
	srv.RegisterRoutes()

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/info", nil))
	var info struct {
		Version string `json:"version"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
	require.Equal(t, "v1.2.3", info.Version)

	// MCP answers carry paste content and are never cached
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, mcpRequest())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))

	// Paste routes are never cached either; the web page keeps its caching
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/raw/abcdefghij.js", nil))
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/application.js", nil))
	require.Empty(t, rec.Header().Get("Cache-Control"))
}

func TestAccessLogRedactsKeys(t *testing.T) {
	var logged strings.Builder
	formatter := &redactingFormatter{next: &middleware.DefaultLogFormatter{Logger: stdlog.New(&logged, "", 0), NoColor: true}}
	h := middleware.RequestLogger(formatter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handlers still see the real path
		require.Equal(t, "/raw/secretkey123", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/raw/secretkey123?x=1", nil)
	req.RemoteAddr = "198.51.100.1:4444"
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := logged.String()
	require.NotContains(t, line, "secretkey123")
	require.Regexp(t, `"GET http://example.com/raw/#[0-9a-f]{8}\?x=1 HTTP/1.1" from 198.51.100.1:4444 - 418`, line)
}

func TestUntrustedHeaderWarning(t *testing.T) {
	var logged strings.Builder
	prev := log.Logger
	log.Logger = zerolog.New(&logged)
	t.Cleanup(func() { log.Logger = prev })

	trusted, err := parseTrustedProxies([]string{"172.18.0.1"})
	require.NoError(t, err)
	h := realIPMiddleware(trusted, "CF-Connecting-IP")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	send := func(remoteAddr string, headers map[string]string) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remoteAddr
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	// From the trusted proxy, or without the header: nothing to say
	send("172.18.0.1:1", map[string]string{"CF-Connecting-IP": "198.51.100.1"})
	send("203.0.113.9:1", nil)
	require.Empty(t, logged.String())

	// From any other peer: one warning, then quiet for a minute
	send("172.18.0.2:1", map[string]string{"CF-Connecting-IP": "198.51.100.1"})
	send("172.18.0.3:1", map[string]string{"CF-Connecting-IP": "198.51.100.1"})
	require.Equal(t, 1, strings.Count(logged.String(), "not a trusted proxy"))
	require.Contains(t, logged.String(), `"peer":"172.18.0.2"`)

	w := &rateLimitedWarning{interval: time.Minute}
	now := time.Now()
	require.True(t, w.allow(now))
	require.False(t, w.allow(now.Add(59*time.Second)))
	require.True(t, w.allow(now.Add(61*time.Second)))
}

func TestMemoryBudgetConfigured(t *testing.T) {
	// A full budget answers 503 on reads
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 1024, MemoryBudget: 100, UploadQuota: 10}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/documents", strings.NewReader(body)))
		return rec
	}
	rec := post("0123456789")
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Key string `json:"key"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))

	// The quota of 10 bytes per hour is used up
	rec = post("x")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))

	// A paste under 64 KiB is read without charging the budget
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/documents/"+out.Key, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "noindex, nofollow", rec.Header().Get("X-Robots-Tag"))
}

func TestInflightMiddleware(t *testing.T) {
	// Requests block until released, like answers to clients that read slowly
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := inflightMiddleware(handler.NewInflightLimiter(2))(slow)
	serve := func(remoteAddr string) chan *httptest.ResponseRecorder {
		out := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/raw/x", nil)
			req.RemoteAddr = remoteAddr
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			out <- rec
		}()
		return out
	}

	first, second := serve("192.0.2.1:1"), serve("192.0.2.1:2")
	<-started
	<-started

	// A third from the same client is refused at once, another client goes through
	rec := <-serve("192.0.2.1:3")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "2", rec.Header().Get("Retry-After"))
	require.Contains(t, rec.Body.String(), "Too many requests in progress")
	other := serve("192.0.2.2:1")
	<-started

	close(release)
	for _, done := range []chan *httptest.ResponseRecorder{first, second, other} {
		require.Equal(t, http.StatusOK, (<-done).Code)
	}
	require.Equal(t, http.StatusOK, (<-serve("192.0.2.1:4")).Code)

	// A nil limiter (max_inflight_per_client: -1) lets everything through
	pass := inflightMiddleware(nil)(http.NotFoundHandler())
	rec = httptest.NewRecorder()
	pass.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestInflightLimitConfigured(t *testing.T) {
	// The limit covers every route, behind the real IP middleware
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, KeyLength: 10, MaxLength: 4 << 20, MaxInflightPerClient: 1,
		TrustedProxies: []string{"192.0.2.100"}, RealIPHeader: "CF-Connecting-IP"}
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()

	post := httptest.NewRecorder()
	srv.mux.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/documents", strings.NewReader(strings.Repeat("a", 2<<20))))
	require.Equal(t, http.StatusOK, post.Code)
	var out struct {
		Key string `json:"key"`
	}
	require.NoError(t, json.NewDecoder(post.Body).Decode(&out))

	request := func(path, client string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.0.2.100:1"
		req.Header.Set("CF-Connecting-IP", client)
		return req
	}

	// A /raw answer to a client that does not read holds its one place
	w := &stalledWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		srv.mux.ServeHTTP(w, request("/raw/"+out.Key, "198.51.100.1"))
		close(done)
	}()
	<-w.started

	for _, path := range []string{"/health", "/app-init.js", "/documents/" + out.Key} {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, request(path, "198.51.100.1"))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, path)
		rec = httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, request(path, "198.51.100.2"))
		require.Equal(t, http.StatusOK, rec.Code, path)
	}
	close(w.release)
	<-done
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, request("/health", "198.51.100.1"))
	require.Equal(t, http.StatusOK, rec.Code)
}

// stalledWriter is a response writer whose writes wait until release is closed
type stalledWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    bool
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	if !w.once {
		w.once = true
		close(w.started)
	}
	<-w.release
	return w.ResponseRecorder.Write(p)
}
