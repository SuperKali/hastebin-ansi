package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/admin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// adminDash is a server with accounts on whose admins are the GitHub ids
// 42 (boss) and 43 (other admin)
func adminDash(t *testing.T, edit func(*config.Config)) *dash {
	return newDash(t, func(cfg *config.Config) {
		cfg.AdminGitHubIDs = []int64{42, 43}
		if edit != nil {
			edit(cfg)
		}
	})
}

// adminRoute is one route of the admin API, with a body for state changes
type adminRoute struct {
	method, path string
	body         any
	destructive  bool
}

// adminRoutes are every route of the admin API, aimed at user and key
func adminRoutes(userID int64, key string) []adminRoute {
	u := fmt.Sprint("/api/v1/admin/users/", userID)
	reason := map[string]string{"reason": "test"}
	return []adminRoute{
		{http.MethodGet, "/api/v1/admin/status", nil, false},
		{http.MethodGet, "/api/v1/admin/pastes?q=" + key, nil, false},
		{http.MethodGet, "/api/v1/admin/pastes/" + key, nil, false},
		{http.MethodPost, "/api/v1/admin/pastes/" + key + "/content", reason, true},
		{http.MethodDelete, "/api/v1/admin/pastes/" + key, reason, true},
		{http.MethodPost, "/api/v1/admin/pastes/" + key + "/abuse", reason, true},
		{http.MethodGet, "/api/v1/admin/users", nil, false},
		{http.MethodGet, u, nil, false},
		{http.MethodPost, u + "/disable", reason, true},
		{http.MethodPost, u + "/enable", reason, false},
		{http.MethodPost, u + "/revoke", reason, true},
		{http.MethodDelete, u, map[string]string{"confirm": "x", "reason": "test"}, true},
		{http.MethodGet, "/api/v1/admin/bans", nil, false},
		{http.MethodPost, "/api/v1/admin/bans", map[string]any{"target": "203.0.113.50", "reason": "test", "expires_in": 0}, true},
		{http.MethodDelete, "/api/v1/admin/bans/manual/203.0.113.50", reason, true},
		{http.MethodGet, "/api/v1/admin/events", nil, false},
		{http.MethodGet, "/api/v1/admin/audit", nil, false},
		{http.MethodPut, "/api/v1/admin/users", reason, false},
		{http.MethodGet, "/api/v1/admin/nothing", nil, false},
	}
}

func TestAdminAPIHiddenFromNonAdmins(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	user := loginAs(t, d.db, 7, "user")
	_, key := d.upload("some content", nil)
	token, _ := d.newToken(user, "t")

	for _, rt := range adminRoutes(d.userID(7), key) {
		// Anonymous, a user with a valid session and CSRF token, a user's
		// API token, and the admin with any Authorization header: all 404
		for name, rec := range map[string]func() int{
			"anonymous": func() int { return d.api(rt.method, rt.path, nil, rt.body).Code },
			"user":      func() int { return d.api(rt.method, rt.path, user, rt.body).Code },
			"token":     func() int { return d.api(rt.method, rt.path, nil, rt.body, "Authorization", "Bearer "+token).Code },
			"admin+bearer": func() int {
				return d.api(rt.method, rt.path, boss, rt.body, "Authorization", "Bearer "+token).Code
			},
			"admin+basic": func() int {
				return d.api(rt.method, rt.path, boss, rt.body, "Authorization", "Basic Ym9zczpwdw==").Code
			},
		} {
			require.Equal(t, http.StatusNotFound, rec(), "%s %s %s", name, rt.method, rt.path)
		}
	}

	// Nothing happened: the paste is there, the user and their token work
	require.True(t, d.stored(key))
	rec, _ := d.upload("still works", nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusOK, rec.Code)
	n, err := d.db.AuditCount(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestAdminStateChangesNeedCSRF(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	loginAs(t, d.db, 7, "user")
	_, key := d.upload("some content", nil)

	for _, rt := range adminRoutes(d.userID(7), key) {
		if rt.method == http.MethodGet || rt.path == "/api/v1/admin/nothing" {
			continue
		}
		body := `{"reason":"test"}`
		for name, headers := range map[string][]string{
			"no token":     {"Content-Type", "application/json", "Origin", "http://localhost:7777"},
			"wrong token":  {"Content-Type", "application/json", "X-CSRF-Token", strings.Repeat("B", 43), "Origin", "http://localhost:7777"},
			"wrong origin": {"Content-Type", "application/json", "X-CSRF-Token", zeroCSRF, "Origin", "https://evil.example"},
			"cross site":   {"Content-Type", "application/json", "X-CSRF-Token", zeroCSRF, "Sec-Fetch-Site", "cross-site"},
			"form body":    {"Content-Type", "application/x-www-form-urlencoded", "X-CSRF-Token", zeroCSRF},
		} {
			rec := d.do(rt.method, rt.path, strings.NewReader(body), boss, headers...)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s %s", name, rt.method, rt.path)
			require.Contains(t, rec.Body.String(), `"error":"csrf"`, "%s %s", name, rt.path)
		}
	}
	require.True(t, d.stored(key))
	n, err := d.db.AuditCount(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "no refused change reached the audit log")
}

func TestAdminReauthRequired(t *testing.T) {
	d := adminDash(t, func(cfg *config.Config) { cfg.AdminReauth = 3600 })
	boss := loginAs(t, d.db, 42, "boss")
	loginAs(t, d.db, 7, "user")
	_, key := d.upload("some content", nil)

	// The GitHub login of the session is two hours old
	_, err := d.db.W.Exec("UPDATE sessions SET auth_at = auth_at - 7200")
	require.NoError(t, err)

	for _, rt := range adminRoutes(d.userID(7), key) {
		if rt.path == "/api/v1/admin/nothing" || rt.method == http.MethodPut {
			continue
		}
		rec := d.api(rt.method, rt.path, boss, rt.body)
		if !rt.destructive {
			require.NotEqual(t, http.StatusForbidden, rec.Code, "%s %s: %s", rt.method, rt.path, rec.Body.String())
			continue
		}
		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s", rt.method, rt.path)
		out := decode[map[string]string](t, rec)
		require.Equal(t, "reauth_required", out["error"], rt.path)
		require.Equal(t, "/auth/github/login?reauth=1&return=%2Fadmin", out["login"])
	}
	require.True(t, d.stored(key), "nothing destructive went through")

	// A fresh login lets the same action through
	_, err = d.db.W.Exec("UPDATE sessions SET auth_at = ?", time.Now().Unix())
	require.NoError(t, err)
	rec := d.api(http.MethodDelete, "/api/v1/admin/pastes/"+key, boss, map[string]string{"reason": "spam"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// auditRows returns the audit log, newest first
func auditRows(t *testing.T, db *accounts.DB) []accounts.AuditEntry {
	t.Helper()
	rows, err := db.AuditLog(context.Background(), 1000, 0)
	require.NoError(t, err)
	return rows
}

func TestAdminLookup(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "t")
	_, anon := d.upload("anonymous paste", nil)
	_, owned := d.upload("private paste", nil, "Authorization", "Bearer "+token, "X-Paste-Visibility", "private", "X-Paste-Title", "logs")

	lookup := func(q string) map[string]any {
		rec := d.api(http.MethodGet, "/api/v1/admin/pastes?q="+url.QueryEscape(q), boss, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", q, rec.Body.String())
		return decode[map[string]any](t, rec)
	}
	for _, q := range []string{owned, owned + ".log", "/raw/" + owned + ".js?x=1", "http://localhost:7777/" + owned + "#a",
		"http://localhost:7777/documents/" + owned, "localhost:7777/dashboard/p/" + owned} {
		out := lookup(q)
		require.Equal(t, owned, out["key"], q)
		require.Equal(t, true, out["exists"])
		require.Equal(t, true, out["registered"])
		require.Equal(t, "private", out["visibility"])
		require.Equal(t, "logs", out["title"])
		require.Equal(t, "alice", out["owner"].(map[string]any)["login"])
		require.NotContains(t, fmt.Sprint(out), "private paste", "a lookup never shows content")
	}
	out := lookup(anon)
	require.Equal(t, false, out["registered"])
	require.Nil(t, out["owner"])
	require.NotNil(t, out["modified_at"])
	require.EqualValues(t, len("anonymous paste"), out["size"])

	// The key route answers the same; other hosts and non-paste paths are refused
	require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/admin/pastes/"+owned, boss, nil).Code)
	for _, q := range []string{"https://evil.example/" + owned, "http://localhost:7777/admin/" + owned, "", "a b"} {
		rec := d.api(http.MethodGet, "/api/v1/admin/pastes?q="+url.QueryEscape(q), boss, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, q)
	}
	rec := d.api(http.MethodGet, "/api/v1/admin/pastes?q=nosuchpaste", boss, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Lookups change nothing and write no audit row
	require.Empty(t, auditRows(t, d.db))
}

func TestAdminContentViewAuditedAndClosesEditWindow(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "t")
	content := strings.Repeat("x", admin.ContentLimit+10)
	_, key := d.upload(content, nil, "Authorization", "Bearer "+token, "X-Paste-Visibility", "private")

	seenAt := func() *int64 {
		var seen *int64
		require.NoError(t, d.db.R.QueryRow("SELECT seen_at FROM pastes WHERE key = ?", key).Scan(&seen))
		return seen
	}
	require.Nil(t, seenAt(), "the edit window is open")

	// A reason is required
	for _, body := range []any{map[string]string{}, map[string]string{"reason": "  "}, map[string]string{"reason": strings.Repeat("r", 501)},
		map[string]any{"reason": "x", "extra": 1}} {
		rec := d.api(http.MethodPost, "/api/v1/admin/pastes/"+key+"/content", boss, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, fmt.Sprint(body))
	}
	require.Nil(t, seenAt())
	require.Empty(t, auditRows(t, d.db))

	rec := d.api(http.MethodPost, "/api/v1/admin/pastes/"+key+"/content", boss, map[string]string{"reason": "report #12"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	out := decode[struct {
		Size      int64  `json:"size"`
		Shown     int    `json:"shown"`
		Truncated bool   `json:"truncated"`
		Content   string `json:"content"`
	}](t, rec)
	require.EqualValues(t, len(content), out.Size)
	require.Equal(t, admin.ContentLimit, out.Shown)
	require.True(t, out.Truncated)
	require.Len(t, out.Content, admin.ContentLimit)

	// Audited with the reason, and it ended the edit window
	rows := auditRows(t, d.db)
	require.Len(t, rows, 1)
	require.Equal(t, "paste.view", rows[0].Action)
	require.Equal(t, "paste:"+key, rows[0].Target)
	require.Equal(t, "boss", rows[0].ActorLogin)
	require.Contains(t, rows[0].Detail, "report #12")
	require.NotNil(t, seenAt())

	// The owner still owns it and it stays private to everyone else
	require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/raw/"+key, nil, nil).Code)
}

func TestAdminAbuseLeavesTombstone(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	alice := loginAs(t, d.db, 11, "alice")
	bob := loginAs(t, d.db, 12, "bob")
	token, _ := d.newToken(alice, "t")
	_, public := d.upload("public spam", nil, "Authorization", "Bearer "+token)
	_, private := d.upload("private spam", nil, "Authorization", "Bearer "+token, "X-Paste-Visibility", "private")
	_, anon := d.upload("anonymous spam", nil)

	for _, key := range []string{public, private, anon} {
		rec := d.api(http.MethodPost, "/api/v1/admin/pastes/"+key+"/abuse", boss, map[string]string{"reason": "malware link"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.False(t, d.stored(key), "the content is deleted at once")
	}

	// A public paste answers 410 to everyone, a private one 404 to others
	for _, who := range []any{nil, bob, "token"} {
		for _, path := range []string{"/raw/" + public, "/documents/" + public, "/raw/" + anon} {
			rec := readAs(d, path, who, token)
			require.Equal(t, http.StatusGone, rec.Code, path)
			require.Contains(t, rec.Body.String(), "removed by a moderator")
		}
	}
	for _, who := range []any{nil, bob} {
		rec := readAs(d, "/raw/"+private, who, token)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.NotContains(t, rec.Body.String(), "moderator")
	}

	// The owner has two strikes; the anonymous paste gave none
	u, err := d.db.UserByGitHubID(context.Background(), 11)
	require.NoError(t, err)
	require.Equal(t, 2, u.AbuseStrikes)

	// The lookup shows the tombstone; a second abuse finds nothing to remove
	out := decode[map[string]any](t, d.api(http.MethodGet, "/api/v1/admin/pastes/"+public, boss, nil))
	require.Equal(t, "malware link", out["tombstone"].(map[string]any)["reason"])
	require.Equal(t, false, out["exists"])
	rec := d.api(http.MethodPost, "/api/v1/admin/pastes/"+public+"/abuse", boss, map[string]string{"reason": "again"})
	require.Equal(t, http.StatusNotFound, rec.Code)
	rec = d.api(http.MethodPost, "/api/v1/admin/pastes/"+public+"/content", boss, map[string]string{"reason": "look"})
	require.Equal(t, http.StatusGone, rec.Code)

	rows := auditRows(t, d.db)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.Equal(t, "paste.abuse", row.Action)
		require.Contains(t, row.Detail, "malware link")
	}

	// A plain delete leaves no tombstone: the key is simply gone
	_, other := d.upload("delete me", nil)
	rec = d.api(http.MethodDelete, "/api/v1/admin/pastes/"+other, boss, map[string]string{"reason": "asked by author"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/raw/"+other, nil, nil).Code)
	rec = d.api(http.MethodDelete, "/api/v1/admin/pastes/"+other, boss, map[string]string{"reason": "again"})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// readAs reads path anonymously (nil), with a session cookie, or with the
// token ("token")
func readAs(d *dash, path string, who any, token string) *httptest.ResponseRecorder {
	switch w := who.(type) {
	case *http.Cookie:
		return d.do(http.MethodGet, path, nil, w)
	case string:
		return d.do(http.MethodGet, path, nil, nil, "Authorization", "Bearer "+token)
	}
	return d.do(http.MethodGet, path, nil, nil)
}

func TestAdminDisableBlocksAtOnce(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "t")
	id := d.userID(11)
	users := fmt.Sprint("/api/v1/admin/users/", id)

	require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/account", alice, nil).Code)
	rec, _ := d.upload("before", nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = d.api(http.MethodPost, users+"/disable", boss, map[string]string{"reason": ""})
	require.Equal(t, http.StatusBadRequest, rec.Code, "a reason is required")
	rec = d.api(http.MethodPost, users+"/disable", boss, map[string]string{"reason": "spam account"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"disabled_reason":"spam account"`)

	// The active session and the token stop at their next request
	rec = d.api(http.MethodGet, "/api/v1/account", alice, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "disabled")
	rec, _ = d.upload("after", nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = d.do(http.MethodPost, "/mcp", strings.NewReader(`{}`), nil, "Authorization", "Bearer "+token, "Content-Type", "application/json")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "MCP answers a disabled account like a bad token")
	require.Equal(t, `Bearer realm="hastebin", error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))

	// Enabled again, both work, as nothing was deleted
	rec = d.api(http.MethodPost, users+"/enable", boss, map[string]string{"reason": "appeal accepted"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/account", alice, nil).Code)
	rec, _ = d.upload("again", nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusOK, rec.Code)

	// Revoke deletes every session and token
	rec = d.api(http.MethodPost, users+"/revoke", boss, map[string]string{"reason": "leaked token"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"sessions":1,"tokens":1}`, rec.Body.String())
	require.Equal(t, http.StatusUnauthorized, d.api(http.MethodGet, "/api/v1/account", alice, nil).Code)
	rec, _ = d.upload("revoked", nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	actions := []string{}
	for _, row := range auditRows(t, d.db) {
		actions = append(actions, row.Action)
		require.Equal(t, fmt.Sprint("user:", id), row.Target)
	}
	require.Equal(t, []string{"user.revoke", "user.enable", "user.disable"}, actions)
}

func TestAdminsAreProtected(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	loginAs(t, d.db, 43, "other")
	for _, id := range []int64{d.userID(42), d.userID(43)} {
		u := fmt.Sprint("/api/v1/admin/users/", id)
		for _, rt := range []adminRoute{
			{http.MethodPost, u + "/disable", map[string]string{"reason": "x"}, true},
			{http.MethodPost, u + "/enable", map[string]string{"reason": "x"}, false},
			{http.MethodPost, u + "/revoke", map[string]string{"reason": "x"}, true},
			{http.MethodDelete, u, map[string]string{"confirm": "other", "reason": "x"}, true},
		} {
			rec := d.api(rt.method, rt.path, boss, rt.body)
			require.Equal(t, http.StatusForbidden, rec.Code, rt.path)
			require.Contains(t, rec.Body.String(), `"error":"protected"`)
		}
	}
	require.Empty(t, auditRows(t, d.db))
	rec := d.api(http.MethodGet, "/api/v1/admin/users?sort=login", boss, nil)
	require.Contains(t, rec.Body.String(), `"admin":true`)
}

func TestAdminDeleteUser(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "t")
	_, key := d.upload("alice's paste", nil, "Authorization", "Bearer "+token)
	u := fmt.Sprint("/api/v1/admin/users/", d.userID(11))

	rec := d.api(http.MethodDelete, u, boss, map[string]string{"confirm": "bob", "reason": "spam"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = d.api(http.MethodDelete, u, boss, map[string]string{"confirm": "alice"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = d.api(http.MethodDelete, u, boss, map[string]string{"confirm": "Alice", "reason": "spam account"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	require.Equal(t, http.StatusNotFound, d.api(http.MethodGet, u, boss, nil).Code)
	require.Equal(t, http.StatusUnauthorized, d.api(http.MethodGet, "/api/v1/account", alice, nil).Code)
	require.Zero(t, d.owner(key))
	rows := auditRows(t, d.db)
	require.Len(t, rows, 1)
	require.Equal(t, "user.delete", rows[0].Action)
	require.Equal(t, "boss", rows[0].ActorLogin)
	require.Contains(t, rows[0].Detail, "spam account")
}

func TestAdminListsBounded(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	ctx := context.Background()
	for i := 0; i < 119; i++ {
		_, _, err := d.db.LoginUser(ctx, int64(1000+i), fmt.Sprintf("user%03d", i), "", true)
		require.NoError(t, err)
	}
	for i := 0; i < 120; i++ {
		require.NoError(t, d.db.Audit(ctx, accounts.AuditEntry{ActorLogin: "boss", Action: "test", Target: fmt.Sprint(i)}))
	}

	type page struct {
		Items []map[string]any `json:"items"`
		Page  int              `json:"page"`
		Pages int              `json:"pages"`
		Total int              `json:"total"`
	}
	for _, path := range []string{"/api/v1/admin/users", "/api/v1/admin/audit"} {
		p := decode[page](t, d.api(http.MethodGet, path, boss, nil))
		require.Len(t, p.Items, admin.PerPage, path)
		require.Equal(t, 120, p.Total, path)
		require.Equal(t, 3, p.Pages, path)
		p = decode[page](t, d.api(http.MethodGet, path+"?page=3", boss, nil))
		require.Len(t, p.Items, 20, path)
		p = decode[page](t, d.api(http.MethodGet, path+"?page=9", boss, nil))
		require.Empty(t, p.Items, path)
		require.Equal(t, http.StatusBadRequest, d.api(http.MethodGet, path+"?page=0", boss, nil).Code)
	}

	// Search and sort
	p := decode[page](t, d.api(http.MethodGet, "/api/v1/admin/users?q=user11&sort=login", boss, nil))
	require.Equal(t, 9, p.Total)
	require.Equal(t, "user110", p.Items[0]["login"])
	p = decode[page](t, d.api(http.MethodGet, "/api/v1/admin/users?q=1005", boss, nil))
	require.Equal(t, 1, p.Total, "a number matches the GitHub id")
	require.Equal(t, http.StatusBadRequest, d.api(http.MethodGet, "/api/v1/admin/users?sort=password", boss, nil).Code)
	for _, sort := range []string{"created", "last_seen", "bytes", "pastes"} {
		require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/admin/users?sort="+sort, boss, nil).Code)
	}

	// The user page lists paste metadata 50 at a time, never content
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "t")
	for i := 0; i < 55; i++ {
		rec, _ := d.upload(fmt.Sprintf("secret content %d", i), nil, "Authorization", "Bearer "+token)
		require.Equal(t, http.StatusOK, rec.Code)
	}
	rec := d.api(http.MethodGet, fmt.Sprint("/api/v1/admin/users/", d.userID(11)), boss, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "secret content")
	detail := decode[struct {
		Login  string `json:"login"`
		Tokens int    `json:"tokens"`
		Pastes page   `json:"paste_list"`
	}](t, rec)
	require.Equal(t, "alice", detail.Login)
	require.Equal(t, 1, detail.Tokens)
	require.Len(t, detail.Pastes.Items, admin.PerPage)
	require.Equal(t, 55, detail.Pastes.Total)
}

func TestAuditLogImmutable(t *testing.T) {
	d := adminDash(t, nil)
	require.NoError(t, d.db.Audit(context.Background(), accounts.AuditEntry{ActorLogin: "boss", Action: "test"}))
	_, err := d.db.W.Exec("UPDATE audit_log SET action = 'forged'")
	require.ErrorContains(t, err, "append-only")
	_, err = d.db.W.Exec("DELETE FROM audit_log")
	require.ErrorContains(t, err, "append-only")
	require.Len(t, auditRows(t, d.db), 1)
}

func TestManualBan(t *testing.T) {
	cfg := (*config.Config)(nil)
	d := adminDash(t, func(c *config.Config) { cfg = c })
	boss := loginAs(t, d.db, 42, "boss")
	ban := func(target string, expiresIn int) *httptest.ResponseRecorder {
		return d.api(http.MethodPost, "/api/v1/admin/bans", boss, map[string]any{"target": target, "reason": "scraping", "expires_in": expiresIn})
	}

	for _, target := range []string{"2001:db8::/48", "198.51.100.0/24", "evil", "198.51.100.7"} {
		rec := ban(target, 0)
		require.Equal(t, http.StatusBadRequest, rec.Code, target)
	}
	require.Equal(t, http.StatusBadRequest, ban("203.0.113.9", -1).Code)
	require.Equal(t, http.StatusBadRequest, d.api(http.MethodPost, "/api/v1/admin/bans", boss,
		map[string]any{"target": "203.0.113.9", "reason": " ", "expires_in": 0}).Code)

	rec := ban("2001:db8:1:2::5", 0)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"target":"2001:db8:1:2::"`)
	rec = ban("203.0.113.9", 3600)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Any address of the /64 is blocked on every route but /health; the
	// next /64 is not
	same := "[2001:db8:1:2:ffff::1]:5555"
	for _, path := range []string{"/", "/raw/abc", "/api/info", "/admin", "/mcp"} {
		rec := d.do(http.MethodGet, path, nil, nil, "X-Test-Remote", same)
		require.Equal(t, http.StatusForbidden, rec.Code, path)
		require.JSONEq(t, `{"message":"Blocked"}`, rec.Body.String())
	}
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/health", nil, nil, "X-Test-Remote", same).Code)
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/api/info", nil, nil, "X-Test-Remote", "[2001:db8:1:3::1]:5555").Code)
	require.Equal(t, http.StatusForbidden, d.do(http.MethodGet, "/api/info", nil, nil, "X-Test-Remote", "203.0.113.9:1").Code)
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/api/info", nil, nil, "X-Test-Remote", "203.0.113.10:1").Code)

	// Bans are listed and survive a restart; one that ended does not
	list := decode[struct {
		Items []admin.BanItem `json:"items"`
	}](t, d.api(http.MethodGet, "/api/v1/admin/bans", boss, nil))
	require.Len(t, list.Items, 2)
	_, err := d.db.W.Exec("INSERT INTO ip_bans (client_key, reason, created_at, expires_at) VALUES ('192.0.2.77', 'old', 1, 2)")
	require.NoError(t, err)
	d.srv = d.start(cfg)
	require.Equal(t, http.StatusForbidden, d.do(http.MethodGet, "/", nil, nil, "X-Test-Remote", same).Code)
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/api/info", nil, nil, "X-Test-Remote", "192.0.2.77:1").Code)
	var n int
	require.NoError(t, d.db.R.QueryRow("SELECT COUNT(*) FROM ip_bans").Scan(&n))
	require.Equal(t, 2, n, "the ended ban was deleted at start")

	// Unban needs a reason, then lets the client in at once
	path := "/api/v1/admin/bans/manual/" + url.PathEscape("2001:db8:1:2::")
	require.Equal(t, http.StatusBadRequest, d.api(http.MethodDelete, path, boss, map[string]string{}).Code)
	rec = d.api(http.MethodDelete, path, boss, map[string]string{"reason": "mistake"})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/api/info", nil, nil, "X-Test-Remote", same).Code)
	require.Equal(t, http.StatusNotFound, d.api(http.MethodDelete, path, boss, map[string]string{"reason": "again"}).Code)
	require.Equal(t, http.StatusNotFound, d.api(http.MethodDelete, "/api/v1/admin/bans/other/1.2.3.4", boss, map[string]string{"reason": "x"}).Code)

	actions := []string{}
	for _, row := range auditRows(t, d.db) {
		actions = append(actions, row.Action+" "+row.Target)
	}
	require.Equal(t, []string{"ban.remove ban:manual:2001:db8:1:2::", "ban.add ban:manual:203.0.113.9", "ban.add ban:manual:2001:db8:1:2::"}, actions)
}

func TestAutomaticBansListedAndLifted(t *testing.T) {
	d := adminDash(t, func(cfg *config.Config) {
		cfg.NotFoundLimit = 2
		cfg.UploadQuota = 10
	})
	boss := loginAs(t, d.db, 42, "boss")
	const client = "203.0.113.20:1"

	for i := 0; i < 3; i++ {
		d.do(http.MethodGet, fmt.Sprint("/documents/missing", i), nil, nil, "X-Test-Remote", client)
	}
	require.Equal(t, http.StatusTooManyRequests, d.do(http.MethodGet, "/documents/missing", nil, nil, "X-Test-Remote", client).Code)
	rec := d.do(http.MethodPut, "/log", strings.NewReader("more than ten bytes"), nil, "X-Test-Remote", client)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	list := decode[struct {
		Items []admin.BanItem `json:"items"`
	}](t, d.api(http.MethodGet, "/api/v1/admin/bans", boss, nil))
	kinds := map[string]string{}
	for _, b := range list.Items {
		kinds[b.Kind] = b.Target
		require.NotNil(t, b.Until)
	}
	require.Equal(t, map[string]string{"not_found": "203.0.113.20", "quota": "203.0.113.20"}, kinds)

	// The events show both refusals, once each
	events := decode[struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
		Max   int              `json:"max"`
	}](t, d.api(http.MethodGet, "/api/v1/admin/events", boss, nil))
	seen := map[string]int{}
	for _, e := range events.Items {
		if e["client"] == "203.0.113.20" {
			seen[e["kind"].(string)]++
		}
	}
	require.Equal(t, map[string]int{"not_found_ban": 1, "quota": 1}, seen)
	require.Equal(t, 500, events.Max)

	for _, kind := range []string{"not_found", "quota"} {
		rec := d.api(http.MethodDelete, "/api/v1/admin/bans/"+kind+"/203.0.113.20", boss, map[string]string{"reason": "test client"})
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		rec = d.api(http.MethodDelete, "/api/v1/admin/bans/"+kind+"/203.0.113.20", boss, map[string]string{"reason": "again"})
		require.Equal(t, http.StatusNotFound, rec.Code)
	}
	require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/documents/missing", nil, nil, "X-Test-Remote", client).Code)
	rec, _ = d.upload("tiny", nil, "X-Test-Remote", client)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, auditRows(t, d.db), 2)
}

func TestAdminLogsHashKeys(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	_, key := d.upload("to be removed", nil)
	_, other := d.upload("to be viewed", nil)

	// From here on, every log line of the admin actions
	var applog strings.Builder
	saved := log.Logger
	log.Logger = zerolog.New(&applog)
	defer func() { log.Logger = saved }()
	require.Equal(t, http.StatusOK, d.api(http.MethodPost, "/api/v1/admin/pastes/"+other+"/content", boss, map[string]string{"reason": "check"}).Code)
	require.Equal(t, http.StatusOK, d.api(http.MethodPost, "/api/v1/admin/pastes/"+key+"/abuse", boss, map[string]string{"reason": "spam"}).Code)
	require.Equal(t, http.StatusOK, d.api(http.MethodGet, "/api/v1/admin/pastes?q="+url.QueryEscape("http://localhost:7777/raw/"+key), boss, nil).Code)

	out := applog.String()
	require.NotContains(t, out, key)
	require.NotContains(t, out, other)
	require.Contains(t, out, admin.KeyHash(key))
	require.Contains(t, out, admin.KeyHash(other))
	require.Contains(t, out, `"action":"paste.abuse"`)
	require.Equal(t, redactKey(key), admin.KeyHash(key), "the same hash as the access log")
}

func TestAdminPageCSPClean(t *testing.T) {
	d := adminDash(t, nil)
	boss := loginAs(t, d.db, 42, "boss")
	for _, path := range []string{"/admin", "/admin/users", "/admin/users/7", "/admin/abuse", "/admin/status", "/admin/audit"} {
		rec := d.do(http.MethodGet, path, nil, boss)
		require.Equal(t, http.StatusOK, rec.Code, path)
		body := rec.Body.String()
		for _, id := range []string{`id="tab-moderation"`, `id="tab-users"`, `id="tab-abuse"`, `id="tab-status"`, `id="tab-audit"`} {
			require.Contains(t, body, id)
		}
		require.NotContains(t, body, "<script>")
		require.NotContains(t, body, " style=")
		require.NotContains(t, body, " onclick=")
	}
	js := d.do(http.MethodGet, "/admin/admin.js", nil, boss).Body.String()
	require.NotContains(t, js, ".html(")
	require.NotContains(t, js, "innerHTML")
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/admin.css", nil, nil).Code)
}
