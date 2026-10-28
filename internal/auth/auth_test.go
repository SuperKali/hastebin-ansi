package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

const adminGitHubID = 7

// harness is a server with the login routes, a fake GitHub and a database
type harness struct {
	t      *testing.T
	db     *accounts.DB
	gh     *fakeGitHub
	svc    *Service
	srv    *httptest.Server
	router *chi.Mux

	// offset moves the service clock; atomic, as the server reads it in other goroutines
	offset atomic.Int64
}

// shift moves the service clock d ahead of the real time
func (h *harness) shift(d time.Duration) {
	h.offset.Store(int64(d))
}

func newHarness(t *testing.T, edit ...func(*Config)) *harness {
	t.Helper()
	db, err := accounts.Open(filepath.Join(t.TempDir(), "hastebin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	h := &harness{t: t, db: db, gh: newFakeGitHub(t), router: chi.NewRouter()}
	h.srv = httptest.NewServer(h.router)
	t.Cleanup(h.srv.Close)

	cfg := Config{
		ClientID:      h.gh.clientID,
		ClientSecret:  h.gh.clientSecret,
		BaseURL:       h.srv.URL,
		Endpoints:     h.gh.endpoints(),
		SessionTTL:    30 * 24 * time.Hour,
		SessionIdle:   7 * 24 * time.Hour,
		AdminIDs:      []int64{adminGitHubID},
		MinAccountAge: 7 * 24 * time.Hour,
		Limits:        func(*Principal) any { return map[string]int{"max_length": 100} },
	}
	for _, e := range edit {
		e(&cfg)
	}
	h.svc, err = New(db, cfg)
	require.NoError(t, err)
	h.svc.now = func() time.Time { return time.Now().Add(time.Duration(h.offset.Load())) }
	h.svc.RegisterRoutes(h.router)
	h.router.Get("/dashboard", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "dashboard") })
	h.router.Get("/p/{key}", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "paste") })
	h.router.With(h.svc.LoadSession, h.svc.RequireAdmin).Get("/admin", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "admin")
	})
	h.router.With(h.svc.LoadSession, h.svc.RequireSession, h.svc.RequireRecentAuth(time.Hour, "/admin")).
		Post("/recent", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	return h
}

// browser returns a client with its own cookie jar that does not follow redirects
func (h *harness) browser() *http.Client {
	jar, err := cookiejar.New(nil)
	require.NoError(h.t, err)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (h *harness) get(c *http.Client, target string) *http.Response {
	h.t.Helper()
	res, err := c.Get(target)
	require.NoError(h.t, err)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

// startLogin starts a login in browser c and returns the callback URL that
// GitHub would send it to
func (h *harness) startLogin(c *http.Client, query string) string {
	h.t.Helper()
	res := h.get(c, h.srv.URL+"/auth/github/login"+query)
	require.Equal(h.t, http.StatusFound, res.StatusCode)
	authorize := res.Header.Get("Location")
	require.True(h.t, strings.HasPrefix(authorize, h.gh.srv.URL+"/login/oauth/authorize?"), authorize)
	res = h.get(c, authorize)
	require.Equal(h.t, http.StatusFound, res.StatusCode)
	callback := res.Header.Get("Location")
	require.True(h.t, strings.HasPrefix(callback, h.srv.URL+CallbackPath+"?"), callback)
	return callback
}

// login runs a whole login in browser c and returns the final redirect
func (h *harness) login(c *http.Client, query string) *http.Response {
	h.t.Helper()
	return h.get(c, h.startLogin(c, query))
}

// session returns GET /api/v1/session as seen by browser c
func (h *harness) session(c *http.Client) (user map[string]any, csrf string) {
	h.t.Helper()
	res, err := c.Get(h.srv.URL + "/api/v1/session")
	require.NoError(h.t, err)
	defer res.Body.Close()
	require.Equal(h.t, http.StatusOK, res.StatusCode)
	require.Equal(h.t, "no-store", res.Header.Get("Cache-Control"))
	var body struct {
		User   map[string]any `json:"user"`
		CSRF   *string        `json:"csrf"`
		Limits map[string]any `json:"limits"`
	}
	require.NoError(h.t, json.NewDecoder(res.Body).Decode(&body))
	require.EqualValues(h.t, 100, body.Limits["max_length"])
	if body.CSRF != nil {
		csrf = *body.CSRF
	}
	return body.User, csrf
}

// post sends a JSON POST with the CSRF token from browser c
func (h *harness) post(c *http.Client, path, csrf, body string, headers ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := c.Do(req)
	require.NoError(h.t, err)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

// cookieValue returns the value of a cookie browser c holds for the server
func (h *harness) cookieValue(c *http.Client, name string) string {
	u, _ := url.Parse(h.srv.URL)
	for _, cookie := range c.Jar.Cookies(u) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func TestLoginFlow(t *testing.T) {
	h := newHarness(t)
	c := h.browser()

	res := h.login(c, "?return=/p/abc")
	require.Equal(t, http.StatusSeeOther, res.StatusCode)
	require.Equal(t, "/p/abc", res.Header.Get("Location"))

	// No scope is asked for, and PKCE is S256
	require.False(t, h.gh.authorizeQuery().Has("scope"))
	require.Equal(t, "S256", h.gh.authorizeQuery().Get("code_challenge_method"))
	require.Equal(t, h.srv.URL+CallbackPath, h.gh.authorizeQuery().Get("redirect_uri"))
	require.Equal(t, 1, h.gh.users())

	user, csrf := h.session(c)
	require.Equal(t, "octocat", user["login"])
	require.Equal(t, false, user["admin"])
	require.NotEmpty(t, csrf)

	// The oauth cookie is gone, the session cookie is set; only its hash is stored
	require.Empty(t, h.cookieValue(c, oauthCookie))
	cookie := h.cookieValue(c, sessionCookie)
	require.NotEmpty(t, cookie)
	raw, err := base64.RawURLEncoding.DecodeString(cookie)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	var n int
	require.NoError(t, h.db.R.QueryRow("SELECT COUNT(*) FROM sessions WHERE id_hash = ?", sum[:]).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, h.db.R.QueryRow("SELECT COUNT(*) FROM sessions WHERE id_hash = ?", raw).Scan(&n))
	require.Zero(t, n)

	// The GitHub token is stored nowhere
	require.NoError(t, h.db.R.QueryRow("SELECT COUNT(*) FROM api_tokens").Scan(&n))
	require.Zero(t, n)

	// A rename on GitHub follows at the next login, on the same account
	first, err := h.db.UserByGitHubID(context.Background(), 1001)
	require.NoError(t, err)
	h.gh.setUser(githubAccount{ID: 1001, Login: "octocat-renamed", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	res = h.login(c, "")
	require.Equal(t, DefaultReturn, res.Header.Get("Location"))
	user, _ = h.session(c)
	require.Equal(t, "octocat-renamed", user["login"])
	require.EqualValues(t, first.ID, user["id"])
}

func TestLoginStateReuseRefused(t *testing.T) {
	h := newHarness(t)
	c := h.browser()
	callback := h.startLogin(c, "")
	binding := h.cookieValue(c, oauthCookie)
	require.NotEmpty(t, binding)

	res := h.get(c, callback)
	require.Equal(t, http.StatusSeeOther, res.StatusCode)
	user, _ := h.session(c)
	require.NotNil(t, user)

	// The same state again, even with the original oauth cookie, is refused
	replay := h.browser()
	u, _ := url.Parse(h.srv.URL)
	replay.Jar.SetCookies(u, []*http.Cookie{{Name: oauthCookie, Value: binding, Path: "/"}})
	res = h.get(replay, callback)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	user, _ = h.session(replay)
	require.Nil(t, user)
}

func TestLoginStateFromAnotherBrowserRefused(t *testing.T) {
	h := newHarness(t)
	victim, attacker := h.browser(), h.browser()

	// The attacker starts a login and gets a victim to open its callback link
	callback := h.startLogin(attacker, "")
	res := h.get(victim, callback)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	user, _ := h.session(victim)
	require.Nil(t, user)

	// Even a victim with a login of its own in progress does not match
	h.startLogin(victim, "")
	callback = h.startLogin(attacker, "")
	require.Equal(t, http.StatusBadRequest, h.get(victim, callback).StatusCode)

	// The state is burned: the attacker cannot use it afterwards either
	require.Equal(t, http.StatusBadRequest, h.get(attacker, callback).StatusCode)
}

func TestLoginWrongPKCEVerifierRefused(t *testing.T) {
	h := newHarness(t)
	c := h.browser()
	callback := h.startLogin(c, "")

	// The server sends the verifier of the state; a different one fails at GitHub
	u, _ := url.Parse(callback)
	state := u.Query().Get("state")
	h.svc.states.mu.Lock()
	h.svc.states.entries[state].Value.(*loginState).verifier = "wrong-verifier-wrong-verifier-wrong-verifier-123"
	h.svc.states.mu.Unlock()
	res := h.get(c, callback)
	require.Equal(t, http.StatusBadGateway, res.StatusCode)
	user, _ := h.session(c)
	require.Nil(t, user)

	// The fake refuses a code without a verifier, or with the wrong one
	c2 := h.browser()
	callback = h.startLogin(c2, "")
	u, _ = url.Parse(callback)
	code := u.Query().Get("code")
	form := url.Values{"client_id": {h.gh.clientID}, "client_secret": {h.gh.clientSecret}, "code": {code},
		"redirect_uri": {h.srv.URL + CallbackPath}}
	res2, err := http.PostForm(h.gh.srv.URL+"/login/oauth/access_token", form)
	require.NoError(t, err)
	var answer map[string]string
	require.NoError(t, json.NewDecoder(res2.Body).Decode(&answer))
	res2.Body.Close()
	require.Equal(t, "bad_verification_code", answer["error"])
	require.Empty(t, answer["access_token"])
}

func TestLoginReturnTo(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"/p/abc", "/p/abc"},
		{"/dashboard?tab=tokens", "/dashboard?tab=tokens"},
		{"/", "/"},
		{"", DefaultReturn},
		{"//evil.com", DefaultReturn},
		{"/\\evil.com", DefaultReturn},
		{"/a\\b", DefaultReturn},
		{"https://evil.com", DefaultReturn},
		{"javascript:alert(1)", DefaultReturn},
		{"/\t/evil.com", DefaultReturn},
		{"/\n/evil.com", DefaultReturn},
		{"/ok path", DefaultReturn},
		{"evil.com", DefaultReturn},
		{"/" + strings.Repeat("a", 600), DefaultReturn},
	} {
		require.Equal(t, tt.want, SafeReturn(tt.in), tt.in)
	}

	h := newHarness(t)
	for _, bad := range []string{"//evil.com", "/\\evil.com", "https://evil.com", "javascript:alert(1)"} {
		c := h.browser()
		res := h.login(c, "?return="+url.QueryEscape(bad))
		require.Equal(t, http.StatusSeeOther, res.StatusCode)
		require.Equal(t, DefaultReturn, res.Header.Get("Location"), bad)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t)
	c := h.browser()
	for i := 0; i < loginLimit; i++ {
		require.Equal(t, http.StatusFound, h.get(c, h.srv.URL+"/auth/github/login").StatusCode, i)
	}
	require.Equal(t, http.StatusTooManyRequests, h.get(c, h.srv.URL+"/auth/github/login").StatusCode)
}

func TestLoginSessionFixation(t *testing.T) {
	h := newHarness(t)

	// The attacker logs in to its own account and plants that session in the victim's browser
	attacker := h.browser()
	h.gh.setUser(githubAccount{ID: 2002, Login: "mallory", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(attacker, "")
	planted := h.cookieValue(attacker, sessionCookie)
	require.NotEmpty(t, planted)

	victim := h.browser()
	u, _ := url.Parse(h.srv.URL)
	victim.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: planted, Path: "/"}})
	user, _ := h.session(victim)
	require.Equal(t, "mallory", user["login"])

	h.gh.setUser(githubAccount{ID: 1001, Login: "victim", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(victim, "")
	fresh := h.cookieValue(victim, sessionCookie)
	require.NotEqual(t, planted, fresh)
	user, _ = h.session(victim)
	require.Equal(t, "victim", user["login"])

	// The planted session is gone for the attacker too
	user, _ = h.session(attacker)
	require.Nil(t, user)

	// A made-up cookie is simply replaced
	other := h.browser()
	other.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: "attacker-chosen", Path: "/"}})
	h.login(other, "")
	require.NotEqual(t, "attacker-chosen", h.cookieValue(other, sessionCookie))
	user, _ = h.session(other)
	require.Equal(t, "victim", user["login"])
}

// findCookie returns the Set-Cookie of a response with the given name
func findCookie(t *testing.T, res *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no cookie %s in %v", name, res.Header.Values("Set-Cookie"))
	return nil
}

func TestCookieFlags(t *testing.T) {
	gh := newFakeGitHub(t)
	db, err := accounts.Open(filepath.Join(t.TempDir(), "hastebin.db"))
	require.NoError(t, err)
	defer db.Close()
	svc, err := New(db, Config{ClientID: gh.clientID, ClientSecret: gh.clientSecret, BaseURL: "https://paste.example.com",
		Endpoints: gh.endpoints(), SessionTTL: 30 * 24 * time.Hour, SessionIdle: 7 * 24 * time.Hour, MinAccountAge: -1})
	require.NoError(t, err)
	require.True(t, svc.SecureCookies())
	router := chi.NewRouter()
	svc.RegisterRoutes(router)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/github/login", nil))
	require.Equal(t, http.StatusFound, rec.Code)
	oauth := findCookie(t, rec.Result(), "__Host-hb_oauth")
	require.True(t, oauth.Secure)
	require.True(t, oauth.HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, oauth.SameSite)
	require.Equal(t, "/", oauth.Path)
	require.Empty(t, oauth.Domain)
	require.Equal(t, 600, oauth.MaxAge)

	// The redirect URI comes from BASE_URL, never from the request
	authorize, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "https://paste.example.com"+CallbackPath, authorize.Query().Get("redirect_uri"))

	// GitHub approves; its redirect goes to the https callback, which is called here directly
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).
		Get(authorize.String())
	require.NoError(t, err)
	res.Body.Close()
	callback, err := url.Parse(res.Header.Get("Location"))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, CallbackPath+"?"+callback.RawQuery, nil)
	req.AddCookie(&http.Cookie{Name: "__Host-hb_oauth", Value: oauth.Value})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())

	session := findCookie(t, rec.Result(), "__Host-hb_session")
	require.True(t, session.Secure)
	require.True(t, session.HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, session.SameSite)
	require.Equal(t, "/", session.Path)
	require.Empty(t, session.Domain)
	require.Equal(t, 30*24*3600, session.MaxAge)
	cleared := findCookie(t, rec.Result(), "__Host-hb_oauth")
	require.Negative(t, cleared.MaxAge)

	// The cookie without the prefix is not read
	req = httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	req.AddCookie(&http.Cookie{Name: "hb_session", Value: session.Value})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Contains(t, rec.Body.String(), `"user":null`)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-hb_session", Value: session.Value})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Contains(t, rec.Body.String(), `"login":"octocat"`)
}

func TestLogoutAndLogoutAll(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.browser(), h.browser(), h.browser()
	h.login(a, "")
	h.login(b, "")
	h.gh.setUser(githubAccount{ID: 3003, Login: "other", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(c, "")

	_, csrfA := h.session(a)
	res := h.post(a, "/api/v1/session/logout", csrfA, "")
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	user, _ := h.session(a)
	require.Nil(t, user)
	user, csrfB := h.session(b)
	require.NotNil(t, user)

	// A second browser of the same user logs out everywhere and revokes tokens
	id := int64(user["id"].(float64))
	_, err := h.db.W.Exec("INSERT INTO api_tokens (user_id, prefix, hash, created_at) VALUES (?, 'hbp_x', x'01', 0)", id)
	require.NoError(t, err)
	h.gh.setUser(githubAccount{ID: 1001, Login: "octocat", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(a, "")
	res = h.post(b, "/api/v1/session/logout-all", csrfB, `{"revoke_tokens": true}`)
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	for _, browser := range []*http.Client{a, b} {
		user, _ := h.session(browser)
		require.Nil(t, user)
	}
	var tokens int
	require.NoError(t, h.db.R.QueryRow("SELECT COUNT(*) FROM api_tokens").Scan(&tokens))
	require.Zero(t, tokens)

	// Other users stay logged in
	user, csrfC := h.session(c)
	require.Equal(t, "other", user["login"])

	// It is in the audit log
	entries, err := h.db.AuditLog(context.Background(), 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "session.logout_all", entries[0].Action)

	// Unknown fields are refused; an empty body means no token revocation
	res = h.post(c, "/api/v1/session/logout-all", csrfC, `{"revoke": true}`)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	res = h.post(c, "/api/v1/session/logout-all", csrfC, "")
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	// Without a session there is nothing to log out of
	res = h.post(c, "/api/v1/session/logout", csrfC, "")
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
}

func TestLogoutNeedsCSRF(t *testing.T) {
	h := newHarness(t)
	c := h.browser()
	h.login(c, "")
	_, csrf := h.session(c)
	wrong := base64.RawURLEncoding.EncodeToString(make([]byte, secretBytes))

	for _, tt := range []struct {
		name    string
		csrf    string
		headers []string
	}{
		{name: "missing token"},
		{name: "wrong token", csrf: wrong},
		{name: "malformed token", csrf: "abc"},
		{name: "wrong origin", csrf: csrf, headers: []string{"Origin", "https://evil.example"}},
		{name: "cross-site fetch", csrf: csrf, headers: []string{"Sec-Fetch-Site", "cross-site"}},
		{name: "form body", csrf: csrf, headers: []string{"Content-Type", "application/x-www-form-urlencoded"}},
		{name: "text body", csrf: csrf, headers: []string{"Content-Type", "text/plain"}},
		{name: "no content type", csrf: csrf, headers: []string{"Content-Type", ""}},
	} {
		res := h.post(c, "/api/v1/session/logout", tt.csrf, "{}", tt.headers...)
		require.Equal(t, http.StatusForbidden, res.StatusCode, tt.name)
		user, _ := h.session(c)
		require.NotNil(t, user, tt.name)
	}

	// The right token from the right origin works
	res := h.post(c, "/api/v1/session/logout", csrf, "{}", "Origin", h.srv.URL, "Sec-Fetch-Site", "same-origin")
	require.Equal(t, http.StatusNoContent, res.StatusCode)
}

func TestDisabledUserRejected(t *testing.T) {
	h := newHarness(t)
	c := h.browser()
	h.login(c, "")
	user, csrf := h.session(c)
	require.NotNil(t, user)

	require.NoError(t, h.db.SetDisabled(context.Background(), int64(user["id"].(float64)), true, "abuse"))
	user, _ = h.session(c)
	require.Nil(t, user)
	res := h.post(c, "/api/v1/session/logout", csrf, "")
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	// Logging in again is refused too
	res = h.login(h.browser(), "")
	require.Equal(t, http.StatusForbidden, res.StatusCode)
}

func TestUsernameReuseIsAnotherAccount(t *testing.T) {
	h := newHarness(t)
	first, second := h.browser(), h.browser()
	h.gh.setUser(githubAccount{ID: 4004, Login: "alice", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(first, "")
	h.gh.setUser(githubAccount{ID: 5005, Login: "alice", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(second, "")

	u1, _ := h.session(first)
	u2, _ := h.session(second)
	require.Equal(t, "alice", u1["login"])
	require.Equal(t, "alice", u2["login"])
	require.NotEqual(t, u1["id"], u2["id"])
}

func TestYoungAccountRefused(t *testing.T) {
	h := newHarness(t)
	h.gh.setUser(githubAccount{ID: 6006, Login: "fresh", CreatedAt: time.Now().Add(-6 * 24 * time.Hour)})
	res := h.login(h.browser(), "")
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	_, err := h.db.UserByGitHubID(context.Background(), 6006)
	require.ErrorIs(t, err, accounts.ErrNotFound)

	// A missing creation date is refused too
	h.gh.setUser(githubAccount{ID: 6007, Login: "nodate"})
	require.Equal(t, http.StatusForbidden, h.login(h.browser(), "").StatusCode)

	// Old enough is fine, and admins are exempt
	h.gh.setUser(githubAccount{ID: 6008, Login: "old", CreatedAt: time.Now().Add(-8 * 24 * time.Hour)})
	require.Equal(t, http.StatusSeeOther, h.login(h.browser(), "").StatusCode)
	h.gh.setUser(githubAccount{ID: adminGitHubID, Login: "newadmin", CreatedAt: time.Now().Add(-time.Hour)})
	require.Equal(t, http.StatusSeeOther, h.login(h.browser(), "").StatusCode)

	// -1 turns the check off
	off := newHarness(t, func(c *Config) { c.MinAccountAge = -1 })
	off.gh.setUser(githubAccount{ID: 6009, Login: "brandnew", CreatedAt: time.Now()})
	require.Equal(t, http.StatusSeeOther, off.login(off.browser(), "").StatusCode)
}

func TestAdminNotFoundForOthers(t *testing.T) {
	h := newHarness(t)
	anon := h.browser()
	require.Equal(t, http.StatusNotFound, h.get(anon, h.srv.URL+"/admin").StatusCode)

	user := h.browser()
	h.login(user, "")
	require.Equal(t, http.StatusNotFound, h.get(user, h.srv.URL+"/admin").StatusCode)

	admin := h.browser()
	h.gh.setUser(githubAccount{ID: adminGitHubID, Login: "boss", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(admin, "")
	u, _ := h.session(admin)
	require.Equal(t, true, u["admin"])
	require.Equal(t, http.StatusOK, h.get(admin, h.srv.URL+"/admin").StatusCode)

	// A Bearer header is never accepted there, even next to an admin cookie
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/admin", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer hbp_whatever")
	res, err := admin.Do(req)
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

// Every Authorization header counts at /admin, not only the first: an
// empty one before a Bearer, as curl -H 'Authorization;' sends it, or an
// empty one alone is 404 next to an admin cookie too
func TestAdminNotFoundWithAnyAuthorizationHeader(t *testing.T) {
	h := newHarness(t)
	admin := h.browser()
	h.gh.setUser(githubAccount{ID: adminGitHubID, Login: "boss", CreatedAt: time.Now().Add(-1000 * time.Hour)})
	h.login(admin, "")
	require.Equal(t, http.StatusOK, h.get(admin, h.srv.URL+"/admin").StatusCode)

	for _, values := range [][]string{{"", "Bearer hbp_whatever"}, {""}, {"Basic Ym9zczpwdw==", ""}} {
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/admin", nil)
		require.NoError(t, err)
		req.Header["Authorization"] = values
		res, err := admin.Do(req)
		require.NoError(t, err)
		res.Body.Close()
		require.Equal(t, http.StatusNotFound, res.StatusCode, "%q", values)
	}
}

func TestSessionExpiry(t *testing.T) {
	h := newHarness(t)
	c := h.browser()
	h.login(c, "")
	user, _ := h.session(c)
	require.NotNil(t, user)

	// Idle for longer than SessionIdle ends it, and the row is deleted
	h.shift(7*24*time.Hour + time.Minute)
	user, _ = h.session(c)
	require.Nil(t, user)
	var n int
	require.NoError(t, h.db.R.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n))
	require.Zero(t, n)

	// The absolute end applies even to a session in use
	h.shift(0)
	h.login(c, "")
	_, err := h.db.W.Exec("UPDATE sessions SET expires_at = ?", time.Now().Unix()-1)
	require.NoError(t, err)
	user, _ = h.session(c)
	require.Nil(t, user)
}

func TestRequireRecentAuth(t *testing.T) {
	h := newHarness(t)
	c := h.browser()
	h.login(c, "")
	_, csrf := h.session(c)
	require.Equal(t, http.StatusOK, h.post(c, "/recent", csrf, "{}").StatusCode)

	h.shift(2 * time.Hour)
	res, err := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/recent", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		return c.Do(req)
	}()
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	var body map[string]string
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, "reauth_required", body["error"])
	require.Equal(t, "/auth/github/login?reauth=1&return=%2Fadmin", body["login"])

	// A re-login asks GitHub to pick the account
	h.shift(0)
	_ = h.startLogin(c, "?reauth=1&return=/admin")
	require.Equal(t, "select_account", h.gh.authorizeQuery().Get("prompt"))
}

func TestLoginOff(t *testing.T) {
	db, err := accounts.Open(filepath.Join(t.TempDir(), "hastebin.db"))
	require.NoError(t, err)
	defer db.Close()
	svc, err := New(db, Config{SessionTTL: time.Hour, SessionIdle: time.Hour})
	require.NoError(t, err)
	require.False(t, svc.LoginEnabled())
	router := chi.NewRouter()
	svc.RegisterRoutes(router)
	for _, path := range []string{"/auth/github/login", CallbackPath + "?code=x&state=y", "/auth/other"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, path)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"user":null`)
}

func TestNewChecksConfig(t *testing.T) {
	db, err := accounts.Open(filepath.Join(t.TempDir(), "hastebin.db"))
	require.NoError(t, err)
	defer db.Close()
	base := Config{ClientID: "id", ClientSecret: "secret", BaseURL: "https://paste.example.com",
		SessionTTL: time.Hour, SessionIdle: time.Hour}

	svc, err := New(db, base)
	require.NoError(t, err)
	require.Equal(t, DefaultOAuthBase+"/login/oauth/authorize", svc.cfg.Endpoints.AuthURL)
	require.Equal(t, DefaultAPIBase, svc.cfg.Endpoints.APIBase)

	for _, tt := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"no secret", func(c *Config) { c.ClientSecret = "" }, "github_client_secret"},
		{"no base url", func(c *Config) { c.BaseURL = "" }, "must be set"},
		{"http base url", func(c *Config) { c.BaseURL = "http://paste.example.com" }, "must be https"},
		{"base url with path", func(c *Config) { c.BaseURL = "https://example.com/paste" }, "bare address"},
		{"no lifetime", func(c *Config) { c.SessionTTL = 0 }, "positive"},
	} {
		cfg := base
		tt.edit(&cfg)
		_, err := New(db, cfg)
		require.ErrorContains(t, err, tt.want, tt.name)
	}

	// http on localhost drops Secure and the __Host- prefix
	for _, local := range []string{"http://localhost:7777", "http://127.0.0.1:7777"} {
		cfg := base
		cfg.BaseURL = local
		svc, err := New(db, cfg)
		require.NoError(t, err, local)
		require.False(t, svc.SecureCookies())
		require.Equal(t, "hb_session", svc.cookie(sessionCookie, "", 0).Name)
	}
}

func TestGitHubEndpoints(t *testing.T) {
	e, err := GitHubEndpoints("", "")
	require.NoError(t, err)
	require.Equal(t, "https://github.com/login/oauth/access_token", e.TokenURL)
	require.Equal(t, "https://api.github.com", e.APIBase)

	e, err = GitHubEndpoints("http://127.0.0.1:9000/", "http://localhost:9000/api")
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:9000/login/oauth/authorize", e.AuthURL)
	require.Equal(t, "http://localhost:9000/api", e.APIBase)

	for _, bad := range [][2]string{
		{"http://github.example", ""},
		{"", "http://10.0.0.1"},
		{"ftp://localhost", ""},
		{"https://user:pw@github.example", ""},
		{"not a url", ""},
	} {
		_, err := GitHubEndpoints(bad[0], bad[1])
		require.Error(t, err, bad)
	}
}

func TestStateStore(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newStateStore(3, 10*time.Minute)
	s.now = func() time.Time { return now }
	for _, id := range []string{"a", "b", "c", "d"} {
		s.put(&loginState{state: id})
	}
	// Full: the oldest went
	require.Equal(t, 3, s.len())
	_, ok := s.take("a")
	require.False(t, ok)

	// Single use
	_, ok = s.take("b")
	require.True(t, ok)
	_, ok = s.take("b")
	require.False(t, ok)

	// Expired after the TTL
	now = now.Add(10 * time.Minute)
	_, ok = s.take("c")
	require.False(t, ok)
	s.put(&loginState{state: "e"})
	require.Equal(t, 1, s.len())
}
