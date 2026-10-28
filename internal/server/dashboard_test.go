package server

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/armbian/ansi-hastebin/internal/dashboard"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// zeroCSRF is the CSRF token of the sessions loginAs makes
var zeroCSRF = base64.RawURLEncoding.EncodeToString(make([]byte, 32))

// dash is a server with accounts on, built from a config that edit may change
type dash struct {
	t   *testing.T
	srv *Server
	db  *accounts.DB
	dir string
}

func newDash(t *testing.T, edit func(*config.Config)) *dash {
	t.Helper()
	dir := t.TempDir()
	cfg := config.NewConfig("nonexistent.yaml")
	cfg.Storage.FilePath = dir
	cfg.BaseURL = "http://localhost:7777"
	cfg.GitHubClientID = "Iv1.test"
	cfg.GitHubClientSecret = testSecret
	cfg.MinFreeDisk = 1 << 20
	if edit != nil {
		edit(cfg)
	}
	db, err := accounts.Open(cfg.AccountsDBPath())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	d := &dash{t: t, db: db, dir: dir}
	d.srv = d.start(cfg)
	return d
}

// start builds a server on the data dir and database, as a restart would
func (d *dash) start(cfg *config.Config) *Server {
	store := storage.NewFileStorage(d.dir, cfg.ExpirationDuration())
	store.(*storage.FileStorage).SetMinFree(cfg.MinFreeDisk)
	d.t.Cleanup(func() { store.Close() })
	srv := NewServer(cfg, store, keygenerator.NewPhoneticKeyGenerator())
	srv.Accounts = d.db
	srv.RegisterRoutes()
	return srv
}

// do sends a request from 198.51.100.7, or from the address in the
// X-Test-Remote header
func (d *dash) do(method, path string, body io.Reader, cookie *http.Cookie, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "198.51.100.7:4444"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "X-Test-Remote" {
			req.RemoteAddr = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	d.srv.mux.ServeHTTP(rec, req)
	return rec
}

// api sends a JSON request with the session cookie and its CSRF token
func (d *dash) api(method, path string, cookie *http.Cookie, body any, headers ...string) *httptest.ResponseRecorder {
	var r io.Reader
	if method != http.MethodGet {
		data, err := json.Marshal(body)
		require.NoError(d.t, err)
		r = strings.NewReader(string(data))
		headers = append([]string{"Content-Type", "application/json", "X-CSRF-Token", zeroCSRF, "Origin", "http://localhost:7777"}, headers...)
	}
	return d.do(method, path, r, cookie, headers...)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}

// newToken creates a token through the API and returns it
func (d *dash) newToken(cookie *http.Cookie, name string) (string, int64) {
	rec := d.api(http.MethodPost, "/api/v1/tokens", cookie, map[string]string{"name": name})
	require.Equal(d.t, http.StatusCreated, rec.Code, rec.Body.String())
	out := decode[struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}](d.t, rec)
	return out.Token, out.ID
}

// upload PUTs content to /log and returns the answer and the key in the link
func (d *dash) upload(content string, cookie *http.Cookie, headers ...string) (*httptest.ResponseRecorder, string) {
	rec := d.do(http.MethodPut, "/log", strings.NewReader(content), cookie, headers...)
	key := ""
	if rec.Code == http.StatusOK {
		link := strings.TrimSpace(rec.Body.String())
		key = link[strings.LastIndex(link, "/")+1:]
	}
	return rec, key
}

// stored reports whether the store has content under key
func (d *dash) stored(key string) bool {
	_, err := d.srv.storage.Size(key)
	return err == nil
}

// files counts the paste files in the data dir
func (d *dash) files() int {
	n, _, err := d.srv.storage.(*storage.FileStorage).Usage()
	require.NoError(d.t, err)
	return int(n)
}

func (d *dash) owner(key string) int64 {
	var id int64
	err := d.db.R.QueryRow("SELECT user_id FROM pastes WHERE key = ?", key).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

func (d *dash) userID(githubID int64) int64 {
	u, err := d.db.UserByGitHubID(context.Background(), githubID)
	require.NoError(d.t, err)
	return u.ID
}

func TestTokenUploadsAreOwned(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	bob := loginAs(t, d.db, 12, "bob")
	aliceID := d.userID(11)

	token, _ := d.newToken(alice, "ci")
	require.True(t, strings.HasPrefix(token, "hbp_"))

	// The token is shown once: the list has its prefix only
	rec := d.api(http.MethodGet, "/api/v1/tokens", alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), token)
	require.Contains(t, rec.Body.String(), token[:10])
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	expires := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	rec, key := d.upload("hello token", nil, "Authorization", "Bearer "+token,
		"X-Paste-Title", "build log", "X-Paste-Tags", "Build, armbian,build", "X-Paste-Visibility", "private", "X-Paste-Expires", expires)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	p, err := d.db.PasteByKey(context.Background(), aliceID, key)
	require.NoError(t, err)
	require.Equal(t, "build log", p.Title)
	require.Equal(t, []string{"armbian", "build"}, p.Tags)
	require.Equal(t, accounts.VisibilityPrivate, p.Visibility)
	require.False(t, p.ExpiresAt.IsZero())
	require.EqualValues(t, len("hello token"), p.Size)
	require.True(t, d.stored(key))

	// POST /documents too
	rec = d.do(http.MethodPost, "/documents", strings.NewReader("doc"), nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusOK, rec.Code)
	key = decode[map[string]string](t, rec)["key"]
	require.Equal(t, aliceID, d.owner(key))

	// The bearer token wins over a valid cookie with its CSRF token
	rec, key = d.upload("bearer wins", bob, "Authorization", "Bearer "+token, "X-CSRF-Token", zeroCSRF)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, aliceID, d.owner(key))

	// A bad token is 401, never an anonymous upload, even with a valid cookie
	files := d.files()
	for _, bad := range []string{token[:len(token)-1] + "x", "hbp_short", auth.NewToken(), ""} {
		rec, _ = d.upload("nope", bob, "Authorization", "Bearer "+bad, "X-CSRF-Token", zeroCSRF)
		require.Equal(t, http.StatusUnauthorized, rec.Code, bad)
		require.Equal(t, `Bearer realm="hastebin", error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))
		rec = d.do(http.MethodPost, "/documents", strings.NewReader("nope"), nil, "Authorization", "Bearer "+bad)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	require.Equal(t, files, d.files(), "nothing stored")

	// A token in the query string is ignored: the upload is anonymous
	rec, key = d.upload("query", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	for _, q := range []string{"?token=", "?access_token="} {
		rec := d.do(http.MethodPut, "/log"+q+token, strings.NewReader("query"), nil)
		require.Equal(t, http.StatusOK, rec.Code)
		link := strings.TrimSpace(rec.Body.String())
		require.Zero(t, d.owner(link[strings.LastIndex(link, "/")+1:]))
	}
	require.Zero(t, d.owner(key))

	// Paste headers without a token or login are 401, never an anonymous
	// public paste in place of the one asked for
	files = d.files()
	for _, h := range [][]string{{"X-Paste-Visibility", "private"}, {"X-Paste-Visibility", "nonsense"}, {"X-Paste-Title", "t"},
		{"X-Paste-Tags", "a"}, {"X-Paste-Expires", "2030-01-02T15:04:05Z"}, {"X-Paste-Language", "go"}} {
		rec, _ = d.upload("anon", nil, h...)
		require.Equal(t, http.StatusUnauthorized, rec.Code, h)
		require.Equal(t, "unauthenticated", decode[map[string]string](t, rec)["error"])
		rec = d.do(http.MethodPost, "/documents", strings.NewReader("anon"), nil, h...)
		require.Equal(t, http.StatusUnauthorized, rec.Code, h)
	}
	require.Equal(t, files, d.files(), "nothing stored")

	// Invalid paste headers on an owned upload: 400 and nothing stored
	files = d.files()
	for name, value := range map[string]string{"X-Paste-Visibility": "public", "X-Paste-Tags": "no spaces", "X-Paste-Expires": "2001-01-01T00:00:00Z"} {
		rec, _ = d.upload("bad header", nil, "Authorization", "Bearer "+token, name, value)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		require.Contains(t, rec.Body.String(), `"message"`)
	}
	require.Equal(t, files, d.files())

	// A revoked token is 401
	rec = d.api(http.MethodGet, "/api/v1/tokens", alice, nil)
	id := decode[struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}](t, rec).Items[0].ID
	require.Equal(t, http.StatusNoContent, d.api(http.MethodDelete, fmt.Sprintf("/api/v1/tokens/%d", id), alice, nil).Code)
	rec, _ = d.upload("revoked", nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, files, d.files())
}

func TestCookieUploadsNeedCSRF(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 21, "alice")
	aliceID := d.userID(21)

	// Without the CSRF token: anonymous as before
	rec := d.do(http.MethodPost, "/documents", strings.NewReader("web"), alice)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, d.owner(decode[map[string]string](t, rec)["key"]))

	// With a wrong one, or cross-site: 401 and nothing stored, never anonymous
	files := d.files()
	for _, headers := range [][]string{
		{"X-CSRF-Token", base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))},
		{"X-CSRF-Token", zeroCSRF, "Origin", "https://evil.example"},
		{"X-CSRF-Token", zeroCSRF, "Sec-Fetch-Site", "cross-site"},
	} {
		rec := d.do(http.MethodPost, "/documents", strings.NewReader("web"), alice, headers...)
		require.Equal(t, http.StatusUnauthorized, rec.Code, headers)
		require.Equal(t, "session_expired", decode[map[string]string](t, rec)["error"])
	}
	require.Equal(t, files, d.files(), "nothing stored")

	// With it the paste is the user's, with the default visibility
	_, err := d.db.W.Exec("UPDATE users SET default_visibility = 1 WHERE id = ?", aliceID)
	require.NoError(t, err)
	rec = d.do(http.MethodPost, "/documents", strings.NewReader("web"), alice,
		"X-CSRF-Token", zeroCSRF, "Origin", "http://localhost:7777", "Content-Type", "text/plain; charset=utf-8")
	require.Equal(t, http.StatusOK, rec.Code)
	key := decode[map[string]string](t, rec)["key"]
	p, err := d.db.PasteByKey(context.Background(), aliceID, key)
	require.NoError(t, err)
	require.Equal(t, accounts.VisibilityPrivate, p.Visibility)
}

// TestStaleSessionUploadNeverAnonymous is the audit repro: a page logged
// out in another tab, or past its session, still sends its cookie and CSRF
// token with X-Paste-Visibility: private. The upload must be 401 with
// nothing stored, on /documents and /log, and never readable anonymously.
// Without any of those headers the cookie alone stays anonymous, as in v1.1.0.
func TestStaleSessionUploadNeverAnonymous(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 22, "alice")
	aliceID := d.userID(22)
	_, err := d.db.W.Exec("UPDATE users SET default_visibility = 1 WHERE id = ?", aliceID)
	require.NoError(t, err)
	// Logged out elsewhere: the session row is gone, the browser keeps the cookie
	_, err = d.db.W.Exec("DELETE FROM sessions WHERE user_id = ?", aliceID)
	require.NoError(t, err)

	files := d.files()
	for _, path := range []string{"/documents", "/log"} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			if path == "/documents" && method == http.MethodPut {
				continue
			}
			for _, headers := range [][]string{
				{"X-CSRF-Token", zeroCSRF, "Origin", "http://localhost:7777", "X-Paste-Visibility", "private"},
				{"X-CSRF-Token", zeroCSRF, "Origin", "http://localhost:7777"},
				{"X-CSRF-Token", zeroCSRF, "X-Paste-Language", "go"},
			} {
				rec := d.do(method, path, strings.NewReader("secret"), alice, headers...)
				require.Equal(t, http.StatusUnauthorized, rec.Code, path, headers)
				require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
				out := decode[map[string]string](t, rec)
				require.Equal(t, "session_expired", out["error"])
				require.NotEmpty(t, out["message"])
			}
		}
	}
	require.Equal(t, files, d.files(), "nothing stored")

	// The cookie alone, with none of those headers, is anonymous as before
	rec := d.do(http.MethodPost, "/documents", strings.NewReader("plain"), alice)
	require.Equal(t, http.StatusOK, rec.Code)
	key := decode[map[string]string](t, rec)["key"]
	require.Zero(t, d.owner(key))
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/raw/"+key, nil, nil).Code)
}

// TestOwnedUploadRateLimit: owned uploads to /documents and /log count per
// account, whatever token or address they come from; past the limit 429
// with Retry-After and nothing stored. Anonymous uploads are not counted.
func TestOwnedUploadRateLimit(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 23, "alice")
	token, _ := d.newToken(alice, "ci")
	other, _ := d.newToken(alice, "laptop")

	for i := range dashboard.UploadLimit {
		tok, path, remote := token, "/log", "198.51.100.7:1"
		if i%2 == 1 {
			tok, path, remote = other, "/documents", fmt.Sprintf("203.0.113.%d:1", i%200+1)
		}
		method := http.MethodPut
		if path == "/documents" {
			method = http.MethodPost
		}
		rec := d.do(method, path, strings.NewReader("x"), nil, "Authorization", "Bearer "+tok, "X-Test-Remote", remote)
		require.Equal(t, http.StatusOK, rec.Code, i)
	}
	files := d.files()
	for _, path := range []string{"/log", "/documents"} {
		rec := d.do(http.MethodPost, path, strings.NewReader("x"), nil, "Authorization", "Bearer "+other, "X-Test-Remote", "192.0.2.9:1")
		require.Equal(t, http.StatusTooManyRequests, rec.Code, path)
		require.Equal(t, "60", rec.Header().Get("Retry-After"))
		require.Contains(t, rec.Body.String(), `"message"`)
	}
	// The session counts against the same account
	rec := d.do(http.MethodPost, "/documents", strings.NewReader("x"), alice, "X-CSRF-Token", zeroCSRF, "Origin", "http://localhost:7777")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, files, d.files(), "nothing stored")

	// Anonymous uploads and other accounts are not held back
	rec = d.do(http.MethodPost, "/documents", strings.NewReader("anon"), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	bob := loginAs(t, d.db, 24, "bob")
	bobToken, _ := d.newToken(bob, "ci")
	rec = d.do(http.MethodPost, "/documents", strings.NewReader("bob"), nil, "Authorization", "Bearer "+bobToken)
	require.Equal(t, http.StatusOK, rec.Code)
}

// Any Authorization header whose scheme is Bearer is a token attempt,
// whatever whitespace follows the scheme: a good token owns the paste, a
// bad one is 401 and nothing is stored. Basic stays anonymous, as in v1.1.0.
func TestBearerAnyWhitespace(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 27, "alice")
	token, _ := d.newToken(alice, "ci")

	for _, h := range []string{"Bearer\t" + token, "bearer \t" + token, "BEARER\t\t" + token + "\t"} {
		rec, key := d.upload("mine", nil, "Authorization", h)
		require.Equal(t, http.StatusOK, rec.Code, "%q: %s", h, rec.Body.String())
		require.Equal(t, d.userID(27), d.owner(key), "%q", h)
		require.Equal(t, userTools, d.mcpTools("Authorization", h), "%q", h)
	}
	files := d.files()
	for _, h := range []string{"Bearer\thbp_nope", "Bearer\t", "bearer\t\tnot-a-token"} {
		rec, _ := d.upload("leak", nil, "Authorization", h)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%q", h)
		require.Equal(t, http.StatusUnauthorized, d.mcpPost("tools/list", map[string]any{}, "Authorization", h).Code, "%q", h)
	}
	require.Equal(t, files, d.files(), "nothing stored")

	rec, key := d.upload("anon", nil, "Authorization", "Basic dXNlcjpwYXNz")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, d.owner(key))
}

// Every path that creates an owned paste shares one budget per account:
// HTTP uploads, MCP upload_log, duplicates, new versions and edits. Each
// stays under its own request limit, and the mix is still held to 120 a minute.
func TestOwnedPasteBudgetShared(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 25, "alice")
	token, _ := d.newToken(alice, "ci")
	auth := []string{"Authorization", "Bearer " + token}

	created := 0
	key := d.ownedUpload(alice, "first\n")
	created++
	for range 10 {
		rec := d.api(http.MethodPost, "/api/v1/pastes/"+key+"/duplicate", alice, map[string]any{})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		created++
	}
	for range 5 {
		rec := d.raw(http.MethodPost, "/api/v1/pastes/"+key+"/versions", "next\n", alice)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		created++
	}
	for range 4 {
		rec := d.editContent(key, "edited\n", alice)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		created++
	}
	for range 60 {
		a := d.mcpTool("upload_log", map[string]any{"content": "x"}, auth...)
		require.False(t, a.isError, a.text)
		created++
	}
	for created < handler.DefaultOwnedPasteLimit {
		rec, _ := d.upload("x", nil, auth...)
		require.Equal(t, http.StatusOK, rec.Code, created)
		created++
	}

	// The budget is used up: every path refuses, and nothing is stored
	files := d.files()
	for _, path := range []string{"/log", "/documents"} {
		rec := d.do(http.MethodPost, path, strings.NewReader("x"), nil, auth...)
		require.Equal(t, http.StatusTooManyRequests, rec.Code, path)
		require.NotEmpty(t, rec.Header().Get("Retry-After"), path)
		require.Contains(t, rec.Body.String(), "too many pastes", path)
	}
	rec := d.api(http.MethodPost, "/api/v1/pastes/"+key+"/duplicate", alice, map[string]any{})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	rec = d.raw(http.MethodPost, "/api/v1/pastes/"+key+"/versions", "next\n", alice)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	rec = d.editContent(key, "edited again\n", alice)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	a := d.mcpTool("upload_log", map[string]any{"content": "x"}, auth...)
	require.True(t, a.isError)
	require.Contains(t, a.text, "nothing was stored: retry in")
	require.Equal(t, files, d.files(), "nothing stored")

	// Other accounts and anonymous uploads are not held back
	bob := loginAs(t, d.db, 26, "bob")
	d.ownedUpload(bob, "bob")
	rec, _ = d.upload("anon", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

// revokeAtEOF is an upload body that runs a function when it is read to the end
type revokeAtEOF struct {
	r    io.Reader
	done bool
	at   func()
}

func (b *revokeAtEOF) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF && !b.done {
		b.done = true
		b.at()
	}
	return n, err
}

func TestRevokedDuringUpload(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 31, "alice")
	aliceID := d.userID(31)
	token, id := d.newToken(alice, "ci")
	files := d.files()

	body := &revokeAtEOF{r: strings.NewReader(strings.Repeat("x", 3<<20)), at: func() {
		require.NoError(t, d.db.DeleteToken(context.Background(), aliceID, id))
	}}
	rec := d.do(http.MethodPut, "/log", body, nil, "Authorization", "Bearer "+token)
	require.True(t, body.done)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	require.Equal(t, `Bearer realm="hastebin", error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))
	require.Equal(t, files, d.files(), "the content is deleted again")
	n, _, err := d.db.Usage(context.Background(), aliceID)
	require.NoError(t, err)
	require.Zero(t, n)

	// The same for a session that logs out during the upload
	sessionBody := &revokeAtEOF{r: strings.NewReader("web paste"), at: func() {
		_, _, err := d.db.LogoutAll(context.Background(), &accounts.User{ID: aliceID, Login: "alice"}, false)
		require.NoError(t, err)
	}}
	rec = d.do(http.MethodPost, "/documents", sessionBody, alice, "X-CSRF-Token", zeroCSRF)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	require.Equal(t, files, d.files())
}

func TestOwnedPastesNeverExpire(t *testing.T) {
	var cfg *config.Config
	d := newDash(t, func(c *config.Config) {
		c.Expiration = 60
		cfg = c
	})
	alice := loginAs(t, d.db, 41, "alice")
	token, _ := d.newToken(alice, "ci")
	_, owned := d.upload("owned", nil, "Authorization", "Bearer "+token)
	_, anon := d.upload("anonymous", nil)
	require.NotEmpty(t, owned)
	require.NotEmpty(t, anon)

	age := func(key string) {
		sum := md5.Sum([]byte(key))
		old := time.Now().Add(-2 * time.Hour)
		require.NoError(t, os.Chtimes(filepath.Join(d.dir, hex.EncodeToString(sum[:])), old, old))
	}
	age(owned)
	age(anon)
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/raw/"+owned, nil, nil).Code)
	require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/raw/"+anon, nil, nil).Code)

	// After a restart the pin comes back from the database
	d.srv = d.start(cfg)
	age(owned)
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/raw/"+owned, nil, nil).Code)
	require.Equal(t, "owned", d.do(http.MethodGet, "/raw/"+owned, nil, nil).Body.String())
}

// With file storage and an expiration, the older contents that same-link
// edits keep under <key>.r<n> never expire either, before or after a
// restart. An expired file would be deleted by the first read, and the
// janitor uses the same rule, so reading them after aging the files shows
// the janitor would keep them too.
func TestRevisionsNeverExpire(t *testing.T) {
	var cfg *config.Config
	d := newDash(t, func(c *config.Config) {
		c.Expiration = 60
		cfg = c
	})
	alice := loginAs(t, d.db, 43, "alice")
	key := d.ownedUpload(alice, "first\n")
	require.Equal(t, http.StatusOK, d.editContent(key, "second\n", alice).Code)

	age := func(k string) {
		sum := md5.Sum([]byte(k))
		old := time.Now().Add(-2 * time.Hour)
		require.NoError(t, os.Chtimes(filepath.Join(d.dir, hex.EncodeToString(sum[:])), old, old))
	}
	for _, restart := range []bool{false, true} {
		if restart {
			d.srv = d.start(cfg)
		}
		age(key)
		age(accounts.RevisionKey(key, 1))
		rec := d.api(http.MethodGet, "/api/v1/pastes/"+key+"/revisions/1", alice, nil)
		require.Equal(t, http.StatusOK, rec.Code, "restart %v", restart)
		require.Equal(t, "first\n", rec.Body.String())
		require.Equal(t, "second\n", d.do(http.MethodGet, "/raw/"+key, nil, alice).Body.String())
	}
	require.Equal(t, 2, d.files())
	for _, name := range []string{"hastebin.db", "hastebin.db-wal"} {
		_, err := os.Stat(filepath.Join(d.dir, name))
		require.NoError(t, err, name)
	}
}

func TestDeleteRemovesStoredContent(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 51, "alice")
	token, _ := d.newToken(alice, "ci")
	_, one := d.upload("one", nil, "Authorization", "Bearer "+token)
	_, two := d.upload("two", nil, "Authorization", "Bearer "+token)
	_, three := d.upload("three", nil, "Authorization", "Bearer "+token)

	require.Equal(t, http.StatusNoContent, d.api(http.MethodDelete, "/api/v1/pastes/"+one, alice, nil).Code)
	require.Equal(t, http.StatusNotFound, d.api(http.MethodDelete, "/api/v1/pastes/"+one, alice, nil).Code)
	rec := d.api(http.MethodPost, "/api/v1/pastes/delete", alice, map[string][]string{"keys": {two, three, "nothere"}})
	require.Equal(t, http.StatusOK, rec.Code)
	res := decode[struct {
		Deleted  []string `json:"deleted"`
		NotFound []string `json:"not_found"`
	}](t, rec)
	require.ElementsMatch(t, []string{two, three}, res.Deleted)
	require.Equal(t, []string{"nothere"}, res.NotFound)

	// Rows go at once, contents once the queue drains
	require.True(t, d.stored(one))
	d.srv.drainer.Drain(context.Background())
	for _, key := range []string{one, two, three} {
		require.False(t, d.stored(key), key)
		require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/raw/"+key, nil, nil).Code)
	}
	n, err := d.db.QueueLen(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestOtherUsersSeeNothing(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 61, "alice")
	bob := loginAs(t, d.db, 62, "bob")
	aliceToken, aliceTokenID := d.newToken(alice, "alice-ci")
	bobToken, _ := d.newToken(bob, "bob-ci")
	_, aliceKey := d.upload("alice secret", nil, "Authorization", "Bearer "+aliceToken, "X-Paste-Tags", "alicetag",
		"X-Paste-Title", "alice title")
	_, bobKey := d.upload("bob paste", nil, "Authorization", "Bearer "+bobToken)

	// Bob gets 404 on everything of Alice's, the same as for a missing key
	for _, rec := range []*httptest.ResponseRecorder{
		d.api(http.MethodGet, "/api/v1/pastes/"+aliceKey, bob, nil),
		d.api(http.MethodPatch, "/api/v1/pastes/"+aliceKey, bob, map[string]string{"title": "mine"}),
		d.api(http.MethodDelete, "/api/v1/pastes/"+aliceKey, bob, nil),
		d.api(http.MethodDelete, fmt.Sprintf("/api/v1/tokens/%d", aliceTokenID), bob, nil),
	} {
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.JSONEq(t, `{"error":"not_found","message":"Not found."}`, rec.Body.String())
	}
	missing := d.api(http.MethodGet, "/api/v1/pastes/doesnotexist", bob, nil)
	require.Equal(t, http.StatusNotFound, missing.Code)

	rec := d.api(http.MethodPost, "/api/v1/pastes/delete", bob, map[string][]string{"keys": {aliceKey, bobKey}})
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, fmt.Sprintf(`{"deleted":[%q],"not_found":[%q]}`, bobKey, aliceKey), rec.Body.String())

	for _, path := range []string{"/api/v1/pastes", "/api/v1/pastes?q=" + aliceKey, "/api/v1/pastes?tag=alicetag", "/api/v1/tags",
		"/api/v1/account/export", "/api/v1/tokens"} {
		rec := d.api(http.MethodGet, path, bob, nil)
		require.Equal(t, http.StatusOK, rec.Code, path)
		for _, secret := range []string{aliceKey, "alicetag", "alice title", "alice-ci"} {
			require.NotContains(t, rec.Body.String(), secret, path)
		}
	}

	// Alice still has everything
	rec = d.api(http.MethodGet, "/api/v1/pastes/"+aliceKey, alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "alicetag")
	require.True(t, d.stored(aliceKey))
	require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/tokens", alice, nil).Code)
	require.Contains(t, d.api(http.MethodGet, "/api/v1/tokens", alice, nil).Body.String(), "alice-ci")
}

func TestDashboardAPIRefusals(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 71, "alice")
	token, _ := d.newToken(alice, "ci")
	_, key := d.upload("x", nil, "Authorization", "Bearer "+token)

	changes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/pastes/delete", `{"keys":["` + key + `"]}`},
		{http.MethodPatch, "/api/v1/pastes/" + key, `{"title":"t"}`},
		{http.MethodDelete, "/api/v1/pastes/" + key, `{}`},
		{http.MethodPost, "/api/v1/tokens", `{"name":"n"}`},
		{http.MethodDelete, "/api/v1/tokens/1", `{}`},
		{http.MethodPatch, "/api/v1/account", `{"default_visibility":"private"}`},
		{http.MethodDelete, "/api/v1/account", `{"confirm":"alice"}`},
	}
	for _, c := range changes {
		send := func(cookie *http.Cookie, headers ...string) *httptest.ResponseRecorder {
			return d.do(c.method, c.path, strings.NewReader(c.body), cookie, headers...)
		}
		name := c.method + " " + c.path
		// Anonymous: 401
		rec := send(nil, "Content-Type", "application/json", "X-CSRF-Token", zeroCSRF)
		require.Equal(t, http.StatusUnauthorized, rec.Code, name)
		// No CSRF token, a wrong one, another origin, cross-site, not JSON: 403 csrf
		for _, headers := range [][]string{
			{"Content-Type", "application/json"},
			{"Content-Type", "application/json", "X-CSRF-Token", base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))},
			{"Content-Type", "application/json", "X-CSRF-Token", zeroCSRF, "Origin", "https://evil.example"},
			{"Content-Type", "application/json", "X-CSRF-Token", zeroCSRF, "Sec-Fetch-Site", "cross-site"},
			{"Content-Type", "text/plain", "X-CSRF-Token", zeroCSRF},
		} {
			rec := send(alice, headers...)
			require.Equal(t, http.StatusForbidden, rec.Code, name, headers)
			require.Contains(t, rec.Body.String(), `"error":"csrf"`)
		}
		// A token is for uploads only, never for the dashboard API
		rec = send(nil, "Content-Type", "application/json", "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusUnauthorized, rec.Code, name)
		rec = send(alice, "Content-Type", "application/json", "X-CSRF-Token", zeroCSRF, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusUnauthorized, rec.Code, name)
	}
	// Reads too
	for _, path := range []string{"/api/v1/pastes", "/api/v1/tokens", "/api/v1/account", "/api/v1/account/export", "/api/v1/tags"} {
		require.Equal(t, http.StatusUnauthorized, d.do(http.MethodGet, path, nil, nil).Code, path)
		require.Equal(t, http.StatusUnauthorized, d.do(http.MethodGet, path, nil, nil, "Authorization", "Bearer "+token).Code, path)
	}
	// Nothing changed
	require.True(t, d.stored(key))
	require.Equal(t, d.userID(71), d.owner(key))
}

func TestDashboardList(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 81, "alice")
	aliceID := d.userID(81)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		tags := []string{"all"}
		for j := 0; j < 9; j++ {
			tags = append(tags, fmt.Sprintf("t%03d", i*9+j))
		}
		p := accounts.NewPaste{Key: fmt.Sprintf("key%02d", i), UserID: aliceID, Title: fmt.Sprintf("paste %02d", i), Size: int64(i), Tags: tags}
		if i%2 == 0 {
			p.Visibility = accounts.VisibilityPrivate
		}
		require.NoError(t, d.db.InsertPaste(ctx, p, accounts.Limits{}))
	}

	type list struct {
		Items []struct {
			Key        string   `json:"key"`
			Visibility string   `json:"visibility"`
			Tags       []string `json:"tags"`
		} `json:"items"`
		Page  int `json:"page"`
		Pages int `json:"pages"`
		Total int `json:"total"`
	}
	rec := d.api(http.MethodGet, "/api/v1/pastes", alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	l := decode[list](t, rec)
	require.Len(t, l.Items, 25)
	require.Equal(t, 30, l.Total)
	require.Equal(t, 2, l.Pages)
	require.Equal(t, "key29", l.Items[0].Key)
	require.Len(t, l.Items[0].Tags, 10)

	l = decode[list](t, d.api(http.MethodGet, "/api/v1/pastes?page=2&sort=created", alice, nil))
	require.Len(t, l.Items, 5)
	require.Equal(t, "key25", l.Items[0].Key)
	l = decode[list](t, d.api(http.MethodGet, "/api/v1/pastes?visibility=private&tag=all&q=paste", alice, nil))
	require.Equal(t, 15, l.Total)
	l = decode[list](t, d.api(http.MethodGet, "/api/v1/pastes?tag=all&tag=t000", alice, nil))
	require.Equal(t, 1, l.Total)
	require.Equal(t, "private", l.Items[0].Visibility)

	for _, bad := range []string{"?sort=random", "?page=0", "?page=x", "?visibility=public", "?has_versions=maybe",
		"?tag=bad%20tag", "?q=" + strings.Repeat("q", 201), "?" + strings.Repeat("tag=a&", 11)} {
		rec := d.api(http.MethodGet, "/api/v1/pastes"+bad, alice, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, bad)
		require.Contains(t, rec.Body.String(), `"error":"invalid_input"`)
	}

	// Tags are bounded: 271 different ones, 200 come back
	tags := decode[struct {
		Items []accounts.TagCount `json:"items"`
	}](t, d.api(http.MethodGet, "/api/v1/tags", alice, nil))
	require.Len(t, tags.Items, 200)
	require.Equal(t, accounts.TagCount{Tag: "all", Count: 30}, tags.Items[0])

	// One paste with its revisions and versions
	rec = d.api(http.MethodGet, "/api/v1/pastes/key03", alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	item := decode[map[string]any](t, rec)
	require.Equal(t, []any{}, item["revisions"])
	require.Len(t, item["versions"], 1)
	require.NotNil(t, item["editable_until"], "new and opened by no one else: editable under the same link")
	require.Nil(t, item["edit_closed"])

	// PATCH: validated, partial
	rec = d.api(http.MethodPatch, "/api/v1/pastes/key03", alice, map[string]any{
		"title": " new title ", "language": "Go", "visibility": "link", "tags": []string{"A", "b", "a"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	item = decode[map[string]any](t, rec)
	require.Equal(t, "new title", item["title"])
	require.Equal(t, "go", item["language"])
	require.Equal(t, "link", item["visibility"])
	require.Equal(t, []any{"a", "b"}, item["tags"])
	require.NotNil(t, item["expires_at"])
	rec = d.api(http.MethodPatch, "/api/v1/pastes/key03", alice, map[string]any{"expires_at": nil})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, decode[map[string]any](t, rec)["expires_at"])
	require.Equal(t, "new title", decode[map[string]any](t, rec)["title"])
	for _, bad := range []map[string]any{
		{"title": strings.Repeat("t", 201)},
		{"tags": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}},
		{"tags": []string{"bad tag"}},
		{"tags": "notalist"},
		{"visibility": "public"},
		{"expires_at": "2001-01-01T00:00:00Z"},
		{"language": "<script>"},
		{"owner": 5},
	} {
		rec := d.api(http.MethodPatch, "/api/v1/pastes/key03", alice, bad)
		require.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}

	// Bulk delete names 1 to 100 keys
	keys := make([]string, 101)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
	}
	require.Equal(t, http.StatusBadRequest, d.api(http.MethodPost, "/api/v1/pastes/delete", alice, map[string]any{"keys": keys}).Code)
	require.Equal(t, http.StatusBadRequest, d.api(http.MethodPost, "/api/v1/pastes/delete", alice, map[string]any{"keys": []string{}}).Code)
	require.Equal(t, http.StatusOK, d.api(http.MethodPost, "/api/v1/pastes/delete", alice, map[string]any{"keys": keys[:100]}).Code)

	// Export: the metadata of every paste, as a file
	rec = d.api(http.MethodGet, "/api/v1/account/export", alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, `attachment; filename="hastebin-alice.json"`, rec.Header().Get("Content-Disposition"))
	export := decode[struct {
		Account map[string]any   `json:"account"`
		Tokens  []map[string]any `json:"tokens"`
		Pastes  []map[string]any `json:"pastes"`
	}](t, rec)
	require.Equal(t, "alice", export.Account["login"])
	require.Len(t, export.Pastes, 30)
}

func TestUserLimitsAndQuota(t *testing.T) {
	t.Run("registered max length", func(t *testing.T) {
		d := newDash(t, func(c *config.Config) {
			c.MaxLength = 10
			c.RegisteredMaxLength = 50
			c.UploadQuota = -1
		})
		token, _ := d.newToken(loginAs(t, d.db, 91, "alice"), "ci")
		rec, _ := d.upload(strings.Repeat("a", 20), nil)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		rec, _ = d.upload(strings.Repeat("a", 20), nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusOK, rec.Code)
		rec, _ = d.upload(strings.Repeat("a", 51), nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	})

	t.Run("pastes per user", func(t *testing.T) {
		d := newDash(t, func(c *config.Config) { c.UserMaxPastes = 2 })
		token, _ := d.newToken(loginAs(t, d.db, 92, "alice"), "ci")
		for i := 0; i < 2; i++ {
			rec, _ := d.upload("p", nil, "Authorization", "Bearer "+token)
			require.Equal(t, http.StatusOK, rec.Code)
		}
		files := d.files()
		rec, _ := d.upload("p", nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusForbidden, rec.Code)
		require.Contains(t, rec.Body.String(), "most pastes")
		require.Equal(t, files, d.files())
		// Others are not affected
		token2, _ := d.newToken(loginAs(t, d.db, 93, "bob"), "ci")
		rec, _ = d.upload("p", nil, "Authorization", "Bearer "+token2)
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("storage per user", func(t *testing.T) {
		d := newDash(t, func(c *config.Config) { c.UserStorageLimit = 100 })
		token, _ := d.newToken(loginAs(t, d.db, 94, "alice"), "ci")
		rec, _ := d.upload(strings.Repeat("s", 60), nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusOK, rec.Code)
		files := d.files()
		rec, _ = d.upload(strings.Repeat("s", 60), nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		require.Equal(t, files, d.files())
		rec, _ = d.upload(strings.Repeat("s", 40), nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusOK, rec.Code)
		rec, _ = d.upload("s", nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		require.Contains(t, rec.Body.String(), "storage is full")
	})

	t.Run("upload quota per user", func(t *testing.T) {
		d := newDash(t, func(c *config.Config) {
			c.UploadQuota = 50
			c.RegisteredUploadQuota = 100
		})
		token, _ := d.newToken(loginAs(t, d.db, 95, "alice"), "ci")
		bearer := []string{"Authorization", "Bearer " + token}
		// Owned uploads count per user, not against the address
		for i := 0; i < 2; i++ {
			rec, _ := d.upload(strings.Repeat("q", 40), nil, bearer...)
			require.Equal(t, http.StatusOK, rec.Code)
		}
		rec, _ := d.upload(strings.Repeat("q", 40), nil, bearer...)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		// From another address the same user is still over
		rec, _ = d.upload(strings.Repeat("q", 40), nil, append(bearer, "X-Test-Remote", "203.0.113.9:1")...)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		// The address still has its own anonymous quota
		rec, _ = d.upload(strings.Repeat("q", 40), nil)
		require.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestPerUserRateLimits(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 101, "alice")
	bob := loginAs(t, d.db, 102, "bob")

	// Writes: 30 a minute per user, whatever the address
	for i := 0; i < 30; i++ {
		rec := d.api(http.MethodPatch, "/api/v1/account", alice, map[string]string{"default_visibility": "link"},
			"X-Test-Remote", fmt.Sprintf("203.0.113.%d:1", i))
		require.Equal(t, http.StatusOK, rec.Code, i)
	}
	rec := d.api(http.MethodPatch, "/api/v1/account", alice, map[string]string{"default_visibility": "link"}, "X-Test-Remote", "192.0.2.1:1")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.JSONEq(t, `{"error":"rate_limited","message":"Too many requests, try again in a moment."}`, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.Equal(t, http.StatusOK, d.api(http.MethodPatch, "/api/v1/account", bob, map[string]string{"default_visibility": "link"}).Code)
	// Reads have their own budget
	require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/account", alice, nil).Code)

	// Token creation: 10 an hour
	for i := 0; i < 10; i++ {
		rec := d.api(http.MethodPost, "/api/v1/tokens", bob, map[string]string{"name": "t"})
		require.Equal(t, http.StatusCreated, rec.Code, i)
	}
	require.Equal(t, http.StatusTooManyRequests, d.api(http.MethodPost, "/api/v1/tokens", bob, map[string]string{"name": "t"}).Code)

	// Reads: 120 a minute
	carol := loginAs(t, d.db, 103, "carol")
	for i := 0; i < 120; i++ {
		require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/tags", carol, nil, "X-Test-Remote", fmt.Sprintf("203.0.113.%d:1", i%200)).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, d.api(http.MethodGet, "/api/v1/tags", carol, nil).Code)

	// Account deletion: 3 tries a day
	dave := loginAs(t, d.db, 104, "dave")
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusBadRequest, d.api(http.MethodDelete, "/api/v1/account", dave, map[string]string{"confirm": "wrong"}).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, d.api(http.MethodDelete, "/api/v1/account", dave, map[string]string{"confirm": "dave"}).Code)
}

func TestTokenLimit(t *testing.T) {
	d := newDash(t, func(c *config.Config) { c.MaxTokensPerUser = 2 })
	alice := loginAs(t, d.db, 111, "alice")
	d.newToken(alice, "one")
	d.newToken(alice, "two")
	rec := d.api(http.MethodPost, "/api/v1/tokens", alice, map[string]string{"name": "three"})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), `"error":"limit_reached"`)
	rec = d.api(http.MethodPost, "/api/v1/tokens", alice, map[string]string{"name": strings.Repeat("n", 65)})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccountDeletion(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 121, "alice")
	bob := loginAs(t, d.db, 122, "bob")
	aliceToken, _ := d.newToken(alice, "ci")
	bobToken, _ := d.newToken(bob, "ci")
	_, k1 := d.upload("one", nil, "Authorization", "Bearer "+aliceToken)
	_, k2 := d.upload("two", alice, "X-CSRF-Token", zeroCSRF)
	_, kb := d.upload("bob", nil, "Authorization", "Bearer "+bobToken)
	require.Equal(t, d.userID(121), d.owner(k2))

	rec := d.api(http.MethodGet, "/api/v1/account", alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	acct := decode[map[string]any](t, rec)
	require.Equal(t, float64(2), acct["usage"].(map[string]any)["pastes"])
	require.EqualValues(t, 121, acct["github_id"])

	require.Equal(t, http.StatusBadRequest, d.api(http.MethodDelete, "/api/v1/account", alice, map[string]string{"confirm": "bob"}).Code)
	rec = d.api(http.MethodDelete, "/api/v1/account", alice, map[string]string{"confirm": "Alice"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Set-Cookie"), "hb_session=;")
	d.srv.drainer.Drain(context.Background())

	require.False(t, d.stored(k1))
	require.False(t, d.stored(k2))
	require.True(t, d.stored(kb))
	// The session and the token stop working at once
	require.JSONEq(t, `null`, func() string {
		var s map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(d.do(http.MethodGet, "/api/v1/session", nil, alice).Body.Bytes(), &s))
		return string(s["user"])
	}())
	require.Equal(t, http.StatusUnauthorized, d.api(http.MethodGet, "/api/v1/pastes", alice, nil).Code)
	rec, _ = d.upload("after", nil, "Authorization", "Bearer "+aliceToken)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	// Bob is untouched
	rec, _ = d.upload("still", nil, "Authorization", "Bearer "+bobToken)
	require.Equal(t, http.StatusOK, rec.Code)
	_, err := d.db.UserByGitHubID(context.Background(), 121)
	require.ErrorIs(t, err, accounts.ErrNotFound)
}

func TestRawDownload(t *testing.T) {
	d := newDash(t, nil)
	_, key := d.upload("download me", nil)
	rec := d.do(http.MethodGet, "/raw/"+key+".js?download=1", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, `attachment; filename="`+key+`.js"`, rec.Header().Get("Content-Disposition"))
	rec = d.do(http.MethodGet, "/raw/"+key+"?download=1", nil, nil)
	require.Equal(t, `attachment; filename="`+key+`.txt"`, rec.Header().Get("Content-Disposition"))
	rec = d.do(http.MethodGet, "/raw/"+key+".a%22b?download=1", nil, nil)
	require.Equal(t, `attachment; filename="`+key+`.txt"`, rec.Header().Get("Content-Disposition"))
	require.Empty(t, d.do(http.MethodGet, "/raw/"+key, nil, nil).Header().Get("Content-Disposition"))
	require.Empty(t, d.do(http.MethodGet, "/raw/missing?download=1", nil, nil).Header().Get("Content-Disposition"))
}

func TestDashboardAccessLog(t *testing.T) {
	var logged strings.Builder
	formatter := &redactingFormatter{next: &middleware.DefaultLogFormatter{Logger: stdlog.New(&logged, "", 0), NoColor: true}}
	h := middleware.RequestLogger(formatter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	const key = "secretpastekey12"
	const token = "hbp_SECRETTOKENVALUE"
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/pastes/" + key},
		{http.MethodPatch, "/api/v1/pastes/" + key},
		{http.MethodDelete, "/api/v1/pastes/" + key},
		{http.MethodGet, "/api/v1/pastes?q=" + key + "&tag=x"},
		{http.MethodGet, "/raw/" + key + "?download=1"},
		{http.MethodPut, "/log?token=" + token},
		{http.MethodGet, "/dashboard/p/" + key},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
	}
	out := logged.String()
	require.NotContains(t, out, key)
	require.NotContains(t, out, "SECRETTOKEN")
	require.Contains(t, out, "/api/v1/pastes/#")
	require.Contains(t, out, "/api/v1/pastes ")
	require.Contains(t, out, "download=1")

	// The application log of a refused change names the route, not the key
	var applog strings.Builder
	saved := log.Logger
	log.Logger = zerolog.New(&applog)
	defer func() { log.Logger = saved }()
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 131, "alice")
	rec := d.do(http.MethodDelete, "/api/v1/pastes/"+key, strings.NewReader("{}"), alice, "Content-Type", "application/json")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, applog.String(), "/api/v1/pastes/{key}")
	require.NotContains(t, applog.String(), key)
}

func TestDashboardPage(t *testing.T) {
	d := newDash(t, nil)
	for _, path := range []string{"/dashboard", "/dashboard/tokens", "/dashboard/account"} {
		rec := d.do(http.MethodGet, path, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code, path)
		body := rec.Body.String()
		require.Contains(t, body, `id="tab-pastes"`)
		require.NotContains(t, body, "<script>")
		require.NotContains(t, body, " style=")
		require.NotContains(t, body, " onclick=")
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	}
	js := d.do(http.MethodGet, "/dashboard.js", nil, nil).Body.String()
	require.NotContains(t, js, ".html(")
	require.NotContains(t, js, "innerHTML")
}
