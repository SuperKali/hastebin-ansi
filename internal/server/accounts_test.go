package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/require"
)

const testSecret = "gh-client-secret-value"

// accountsServer returns a server with accounts on: login configured with
// BASE_URL on localhost, so cookies are hb_session without the prefix
func accountsServer(t *testing.T, login bool) (*Server, *accounts.DB) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.NewConfig("nonexistent.yaml")
	cfg.Storage.FilePath = dir
	cfg.BaseURL = "http://localhost:7777"
	cfg.AdminGitHubIDs = []int64{42}
	cfg.MinFreeDisk = 1 << 20
	if login {
		cfg.GitHubClientID = "Iv1.test"
		cfg.GitHubClientSecret = testSecret
	}
	store := storage.NewFileStorage(dir, 0)
	store.(*storage.FileStorage).SetMinFree(cfg.MinFreeDisk)
	t.Cleanup(func() { store.Close() })
	db, err := accounts.Open(cfg.AccountsDBPath())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	srv := NewServer(cfg, store, keygenerator.NewPhoneticKeyGenerator())
	srv.Version = "v9.9.9"
	srv.Accounts = db
	srv.RegisterRoutes()
	return srv, db
}

// loginAs stores a session for a GitHub user and returns its cookie
func loginAs(t *testing.T, db *accounts.DB, githubID int64, login string) *http.Cookie {
	t.Helper()
	user, _, err := db.LoginUser(context.Background(), githubID, login, true)
	require.NoError(t, err)
	id := make([]byte, 32)
	copy(id, login)
	id[31] = byte(githubID)
	sum := sha256.Sum256(id)
	require.NoError(t, db.CreateSession(context.Background(), sum[:], user.ID, make([]byte, 32), time.Now().Add(time.Hour)))
	return &http.Cookie{Name: "hb_session", Value: base64.RawURLEncoding.EncodeToString(id)}
}

func serve(srv *Server, method, path string, cookie *http.Cookie, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "198.51.100.7:4444"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func infoLogin(t *testing.T, srv *Server) bool {
	t.Helper()
	rec := serve(srv, http.MethodGet, "/api/info", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var info map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	login, ok := info["login"].(bool)
	require.True(t, ok)
	return login
}

func TestAccountsOff(t *testing.T) {
	cfg := config.NewConfig("nonexistent.yaml")
	srv := NewServer(cfg, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewPhoneticKeyGenerator())
	srv.RegisterRoutes()
	require.False(t, infoLogin(t, srv))

	// Without accounts these are 404, not the index page
	for _, path := range []string{"/auth/github/login", "/auth/github/callback?code=x", "/admin", "/admin/status",
		"/dashboard", "/dashboard/p/abc", "/api/v1/session", "/api/v1/admin/status", "/admin.html", "/admin.js", "/dashboard.html"} {
		rec := serve(srv, http.MethodGet, path, nil)
		require.Equal(t, http.StatusNotFound, rec.Code, path)
		require.NotContains(t, rec.Body.String(), "<html", path)
	}
	require.Equal(t, http.StatusOK, serve(srv, http.MethodGet, "/dashboard.css", nil).Code)
}

func TestAccountsRoutes(t *testing.T) {
	srv, db := accountsServer(t, true)
	require.True(t, infoLogin(t, srv))

	// Login starts at GitHub, with the callback under BASE_URL
	rec := serve(srv, http.MethodGet, "/auth/github/login?return=/abc", nil)
	require.Equal(t, http.StatusFound, rec.Code)
	require.True(t, strings.HasPrefix(rec.Header().Get("Location"), "https://github.com/login/oauth/authorize?"))
	require.Contains(t, rec.Header().Get("Location"), "redirect_uri=http%3A%2F%2Flocalhost%3A7777%2Fauth%2Fgithub%2Fcallback")

	// The dashboard page is served, its private file name is not
	rec = serve(srv, http.MethodGet, "/dashboard", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `id="tab-pastes"`)
	require.Equal(t, http.StatusNotFound, serve(srv, http.MethodGet, "/dashboard.html", nil).Code)

	// Session API
	rec = serve(srv, http.MethodGet, "/api/v1/session", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"user":null,"csrf":null,"limits":{"max_length":4000000,"mcp_max_length":4000000}}`, rec.Body.String())
	user := loginAs(t, db, 5, "someone")
	rec = serve(srv, http.MethodGet, "/api/v1/session", user)
	require.Contains(t, rec.Body.String(), `"login":"someone"`)
	require.Contains(t, rec.Body.String(), `"admin":false`)

	// Unknown API routes are JSON 404s
	rec = serve(srv, http.MethodGet, "/api/v1/nothing", user)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), `"error":"not_found"`)

	// The login query is never logged
	var logged strings.Builder
	formatter := &redactingFormatter{next: &middleware.DefaultLogFormatter{Logger: stdlog.New(&logged, "", 0), NoColor: true}}
	h := middleware.RequestLogger(formatter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=secretcode&state=secretstate", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/auth/github/login?return=/secretkey123", nil))
	require.NotContains(t, logged.String(), "secret")
	require.Contains(t, logged.String(), "/auth/github/callback")
}

func TestAccountsLoginOffKeepsSessions(t *testing.T) {
	srv, db := accountsServer(t, false)
	require.False(t, infoLogin(t, srv))
	require.Equal(t, http.StatusNotFound, serve(srv, http.MethodGet, "/auth/github/login", nil).Code)

	// A session from before login was turned off still works, and admins keep the admin page
	admin := loginAs(t, db, 42, "boss")
	rec := serve(srv, http.MethodGet, "/api/v1/session", admin)
	require.Contains(t, rec.Body.String(), `"admin":true`)
	require.Equal(t, http.StatusOK, serve(srv, http.MethodGet, "/admin", admin).Code)
}

func TestAdminStatus(t *testing.T) {
	srv, db := accountsServer(t, true)
	admin := loginAs(t, db, 42, "boss")
	user := loginAs(t, db, 7, "user")

	// Anonymous, non-admins and Bearer requests get 404 everywhere under admin
	for _, path := range []string{"/admin", "/admin/status", "/admin/admin.js", "/api/v1/admin/status", "/api/v1/admin/users", "/admin.html", "/admin.js"} {
		for _, cookie := range []*http.Cookie{nil, user} {
			rec := serve(srv, http.MethodGet, path, cookie)
			require.Equal(t, http.StatusNotFound, rec.Code, path)
			require.NotContains(t, rec.Body.String(), "hastebin admin", path)
		}
		rec := serve(srv, http.MethodGet, path, admin, "Authorization", "Bearer hbp_anything")
		require.Equal(t, http.StatusNotFound, rec.Code, path)
	}
	require.Equal(t, http.StatusNotFound, serve(srv, http.MethodPost, "/api/v1/admin/status", user).Code)

	// The admin gets the page, its script and the status
	rec := serve(srv, http.MethodGet, "/admin", admin)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "hastebin admin")
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.NotEmpty(t, rec.Header().Get("Content-Security-Policy"))
	require.NotContains(t, rec.Body.String(), "<script>")
	require.NotContains(t, rec.Body.String(), "style=")
	rec = serve(srv, http.MethodGet, "/admin/admin.js", admin)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "javascript")
	require.Equal(t, http.StatusMethodNotAllowed, serve(srv, http.MethodPost, "/api/v1/admin/status", admin).Code)

	require.NoError(t, srv.storage.Set("paste", "content", false))
	rec = serve(srv, http.MethodGet, "/api/v1/admin/status", admin)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, testSecret)

	var st struct {
		Version string `json:"version"`
		Storage string `json:"storage"`
		Uptime  int64  `json:"uptime_seconds"`
		Disk    *struct {
			Free        int64 `json:"free"`
			MinFreeDisk int64 `json:"min_free_disk"`
		} `json:"disk"`
		FilePastes *struct {
			Count int64     `json:"count"`
			AsOf  time.Time `json:"as_of"`
		} `json:"file_pastes"`
		Registered *struct {
			Users    int64 `json:"users"`
			Sessions int64 `json:"sessions"`
		} `json:"registered"`
		ReadBudget struct {
			Size int64 `json:"size"`
		} `json:"read_budget"`
		UploadBudget struct {
			Size int64 `json:"size"`
		} `json:"upload_budget"`
		Slots struct {
			Total int `json:"total"`
		} `json:"upload_slots"`
		Inflight struct {
			Requests int `json:"requests"`
		} `json:"inflight"`
		MCP *struct {
			ReadsMax int `json:"reads_max"`
		} `json:"mcp"`
		Config []config.Setting `json:"config"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	require.Equal(t, "v9.9.9", st.Version)
	require.Equal(t, "file", st.Storage)
	require.NotNil(t, st.Disk)
	require.EqualValues(t, 1<<20, st.Disk.MinFreeDisk)
	require.Positive(t, st.Disk.Free)
	require.NotNil(t, st.FilePastes)
	// The paste counts, the accounts database next to it does not
	require.EqualValues(t, 1, st.FilePastes.Count)
	require.False(t, st.FilePastes.AsOf.IsZero())
	require.EqualValues(t, 2, st.Registered.Users)
	require.EqualValues(t, 2, st.Registered.Sessions)
	require.Positive(t, st.ReadBudget.Size)
	require.Positive(t, st.UploadBudget.Size)
	require.Equal(t, 8, st.Slots.Total)
	require.NotNil(t, st.MCP)
	require.Positive(t, st.MCP.ReadsMax)
	values := map[string]any{}
	for _, s := range st.Config {
		values[s.Key] = s.Value
	}
	require.Equal(t, "set", values["github_client_secret"])
	require.Equal(t, "Iv1.test", values["github_client_id"])
}
