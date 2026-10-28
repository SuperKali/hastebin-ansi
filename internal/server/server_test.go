package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
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
}

func TestKeyByRemoteIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.1:4444"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "1.2.3.4")

	key, err := keyByRemoteIP(req)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.1", key)
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
