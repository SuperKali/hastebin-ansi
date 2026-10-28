package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/stretchr/testify/require"
)

// sameOrigin are the headers of a state change from the dashboard
var sameOrigin = []string{"X-CSRF-Token", zeroCSRF, "Origin", "http://localhost:7777"}

// ownedUpload saves content as the user of cookie and returns its key
func (d *dash) ownedUpload(cookie *http.Cookie, content string, headers ...string) string {
	rec, key := d.upload(content, cookie, append(append([]string{}, sameOrigin...), headers...)...)
	require.Equal(d.t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotZero(d.t, d.owner(key))
	return key
}

// raw sends a body to a content route with the session cookie and CSRF token
func (d *dash) raw(method, path, body string, cookie *http.Cookie, headers ...string) *httptest.ResponseRecorder {
	return d.do(method, path, strings.NewReader(body), cookie,
		append(append([]string{"Content-Type", "text/plain"}, sameOrigin...), headers...)...)
}

func (d *dash) editContent(key, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	return d.raw(http.MethodPut, "/api/v1/pastes/"+key+"/content", body, cookie)
}

// mcpRead calls read_paste as an anonymous client and returns the text
func (d *dash) mcpRead(keyOrURL string, headers ...string) string {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "read_paste", "arguments": map[string]any{"key_or_url": keyOrURL}}})
	require.NoError(d.t, err)
	rec := d.do(http.MethodPost, "/mcp", strings.NewReader(string(body)), nil,
		append([]string{"Content-Type", "application/json", "Accept", "application/json, text/event-stream"}, headers...)...)
	require.Equal(d.t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

func TestEditSameLink(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 81, "alice")
	key := d.ownedUpload(alice, "first content\n")

	// Not edited yet: no X-Paste-Edited, no edited_at
	for _, path := range []string{"/raw/", "/documents/"} {
		rec := d.do(http.MethodGet, path+key, nil, alice)
		require.Empty(t, rec.Header().Get("X-Paste-Edited"), path)
		require.NotContains(t, rec.Body.String(), "edited_at")
	}

	// The page link does not end the window: unfurlers get static HTML
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/"+key, nil, nil).Code)
	require.Equal(t, http.StatusOK, d.do(http.MethodHead, "/raw/"+key, nil, nil).Code)

	rec := d.editContent(key, "second content\n", alice)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	item := decode[map[string]any](t, rec)
	require.EqualValues(t, 1, item["edit_count"])
	require.NotNil(t, item["edited_at"])
	require.NotNil(t, item["editable_until"])

	// Everyone sees when it was edited
	for _, who := range []*http.Cookie{alice, nil} {
		rec = d.do(http.MethodHead, "/raw/"+key, nil, who)
		edited, err := time.Parse(time.RFC3339, rec.Header().Get("X-Paste-Edited"))
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), edited, time.Minute)
	}
	rec = d.do(http.MethodGet, "/documents/"+key, nil, alice)
	doc := decode[map[string]any](t, rec)
	require.Equal(t, "second content\n", doc["data"])
	require.NotEmpty(t, doc["edited_at"])
	require.Equal(t, "link", doc["visibility"])
	require.NotEmpty(t, rec.Header().Get("X-Paste-Edited"))

	// The first read by someone else ends the window, and the edited mark goes with it
	rec = d.do(http.MethodGet, "/raw/"+key, nil, nil)
	require.Equal(t, "second content\n", rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("X-Paste-Edited"))
	rec = d.editContent(key, "third\n", alice)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.JSONEq(t, `{"error":"edit_window_closed","reason":"seen",
		"message":"Someone else opened this paste, so it can no longer change under the same link. Save it as a new version."}`,
		rec.Body.String())
	rec = d.do(http.MethodGet, "/documents/"+key, nil, nil)
	doc = decode[map[string]any](t, rec)
	require.Equal(t, "second content\n", doc["data"])
	require.NotEmpty(t, doc["edited_at"])
	require.NotContains(t, doc, "owner")

	// Owner history: the old content, never under the paste routes or MCP
	rec = d.api(http.MethodGet, "/api/v1/pastes/"+key+"/revisions/1", alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "first content\n", rec.Body.String())
	for _, path := range []string{"/raw/" + key + ".r1", "/raw/" + key + "%2Er1", "/raw/" + key + "%2er1",
		"/documents/" + key + ".r1", "/documents/" + key + "%2Er1", "/raw/" + key + ".r1?download=1",
		"/raw/..%2F" + key + ".r1", "/raw/" + key + "%2F..%2F" + key + ".r1", "/raw/%2E%2E/" + key + ".r1"} {
		for _, who := range []*http.Cookie{alice, nil} {
			rec := d.do(http.MethodGet, path, nil, who)
			require.NotContains(t, rec.Body.String(), "first content", path)
		}
	}
	require.NotContains(t, d.mcpRead(key+".r1"), "first content")
	require.NotContains(t, d.mcpRead("http://localhost:7777/raw/"+key+".r1"), "first content")
	require.NotContains(t, d.mcpRead(key+"%2Er1"), "first content")
}

func TestEditRoutesOwnerOnly(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 82, "alice")
	bob := loginAs(t, d.db, 83, "bob")
	token, _ := d.newToken(alice, "ci")
	key := d.ownedUpload(alice, "alice only\n")
	require.Equal(t, http.StatusOK, d.editContent(key, "alice edit\n", alice).Code)

	type call struct{ method, path, body string }
	calls := []call{
		{http.MethodPut, "/api/v1/pastes/" + key + "/content", "bob was here"},
		{http.MethodPost, "/api/v1/pastes/" + key + "/versions", "bob was here"},
		{http.MethodPost, "/api/v1/pastes/" + key + "/versions?delete_previous=1", "bob was here"},
		{http.MethodPost, "/api/v1/pastes/" + key + "/duplicate", "{}"},
		{http.MethodGet, "/api/v1/pastes/" + key + "/revisions/1", ""},
	}
	for _, c := range calls {
		name := c.method + " " + c.path
		contentType := "text/plain"
		if strings.HasSuffix(c.path, "/duplicate") {
			contentType = "application/json"
		}
		send := func(cookie *http.Cookie, headers ...string) *httptest.ResponseRecorder {
			return d.do(c.method, c.path, strings.NewReader(c.body), cookie, append([]string{"Content-Type", contentType}, headers...)...)
		}

		// Someone else: 404 exactly as a missing key
		rec := send(bob, sameOrigin...)
		require.Equal(t, http.StatusNotFound, rec.Code, name)
		require.JSONEq(t, `{"error":"not_found","message":"Not found."}`, rec.Body.String(), name)
		missing := d.do(c.method, strings.Replace(c.path, key, "nosuchpastekey", 1), strings.NewReader(c.body), alice,
			append([]string{"Content-Type", contentType}, sameOrigin...)...)
		require.Equal(t, http.StatusNotFound, missing.Code, name)
		require.Equal(t, rec.Body.String(), missing.Body.String(), name)

		// Anonymous: 401; a token never works here
		require.Equal(t, http.StatusUnauthorized, send(nil, sameOrigin...).Code, name)
		require.Equal(t, http.StatusUnauthorized, send(nil, "Authorization", "Bearer "+token).Code, name)
		require.Equal(t, http.StatusUnauthorized, send(alice, append([]string{"Authorization", "Bearer " + token}, sameOrigin...)...).Code, name)

		if c.method == http.MethodGet {
			continue
		}
		// State changes need the CSRF token and this origin
		for _, headers := range [][]string{
			nil,
			{"X-CSRF-Token", "AAAA"},
			{"X-CSRF-Token", zeroCSRF, "Origin", "https://evil.example"},
			{"X-CSRF-Token", zeroCSRF, "Sec-Fetch-Site", "cross-site"},
		} {
			rec := send(alice, headers...)
			require.Equal(t, http.StatusForbidden, rec.Code, name, headers)
			require.Contains(t, rec.Body.String(), `"error":"csrf"`)
		}
	}

	// Nothing changed and nothing new exists
	rec := d.do(http.MethodGet, "/raw/"+key, nil, alice)
	require.Equal(t, "alice edit\n", rec.Body.String())
	list := decode[struct{ Total int }](t, d.api(http.MethodGet, "/api/v1/pastes", alice, nil))
	require.Equal(t, 1, list.Total)
	list = decode[struct{ Total int }](t, d.api(http.MethodGet, "/api/v1/pastes", bob, nil))
	require.Zero(t, list.Total)
}

func TestNewVersionsAndDuplicate(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 84, "alice")
	key := d.ownedUpload(alice, "v1 content\n", "X-Paste-Title", "build log", "X-Paste-Tags", "ci,armbian",
		"X-Paste-Visibility", "private", "X-Paste-Language", "python")

	rec := d.raw(http.MethodPost, "/api/v1/pastes/"+key+"/versions", "v2 content\n", alice)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	v2 := decode[map[string]any](t, rec)
	v2key := v2["key"].(string)
	require.NotEqual(t, key, v2key)
	require.Equal(t, key, v2["prev_key"])
	require.Equal(t, key, v2["root_key"])
	require.EqualValues(t, 2, v2["version"])
	require.Equal(t, "build log", v2["title"])
	require.Equal(t, "private", v2["visibility"])
	require.Equal(t, "python", v2["language"])
	require.ElementsMatch(t, []any{"ci", "armbian"}, v2["tags"])
	require.Equal(t, "v2 content\n", d.do(http.MethodGet, "/raw/"+v2key, nil, alice).Body.String())
	require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/raw/"+v2key, nil, nil).Code, "still private")

	detail := decode[map[string]any](t, d.api(http.MethodGet, "/api/v1/pastes/"+key, alice, nil))
	require.Equal(t, v2key, detail["next_key"])
	require.Len(t, detail["versions"], 2)

	// The next one replaces v2: v2 is gone, the chain keeps its root
	rec = d.raw(http.MethodPost, "/api/v1/pastes/"+v2key+"/versions?delete_previous=1", "v3 content\n", alice)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	v3 := decode[map[string]any](t, rec)
	require.EqualValues(t, 3, v3["version"])
	require.Equal(t, key, v3["root_key"])
	require.Nil(t, v3["prev_key"])
	require.Equal(t, http.StatusNotFound, d.api(http.MethodGet, "/api/v1/pastes/"+v2key, alice, nil).Code)
	require.True(t, d.stored(v2key), "the content goes once the queue drains")
	d.srv.drainer.Drain(context.Background())
	require.False(t, d.stored(v2key))
	require.Equal(t, http.StatusBadRequest, d.raw(http.MethodPost, "/api/v1/pastes/"+key+"/versions?delete_previous=yes", "x", alice).Code)
	require.Equal(t, http.StatusBadRequest, d.raw(http.MethodPost, "/api/v1/pastes/"+key+"/versions", "", alice).Code)

	// Duplicate: a copy under a new link, with its metadata
	rec = d.api(http.MethodPost, "/api/v1/pastes/"+key+"/duplicate", alice, map[string]any{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	dup := decode[map[string]any](t, rec)
	dupKey := dup["key"].(string)
	require.NotEqual(t, key, dupKey)
	require.Equal(t, dupKey, dup["root_key"])
	require.EqualValues(t, 1, dup["version"])
	require.Equal(t, "build log", dup["title"])
	require.Equal(t, "v1 content\n", d.do(http.MethodGet, "/raw/"+dupKey, nil, alice).Body.String())
	require.EqualValues(t, len("v1 content\n"), dup["size"])
}

func TestEditsChargeTheQuota(t *testing.T) {
	d := newDash(t, func(cfg *config.Config) { cfg.RegisteredUploadQuota = 100 })
	alice := loginAs(t, d.db, 85, "alice")
	key := d.ownedUpload(alice, strings.Repeat("a", 30))
	require.Equal(t, http.StatusOK, d.editContent(key, strings.Repeat("b", 30), alice).Code)
	rec := d.api(http.MethodPost, "/api/v1/pastes/"+key+"/duplicate", alice, map[string]any{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	// 90 used: 30 more is over 100, whichever way it comes
	rec = d.api(http.MethodPost, "/api/v1/pastes/"+key+"/duplicate", alice, map[string]any{})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.Equal(t, http.StatusTooManyRequests, d.raw(http.MethodPost, "/api/v1/pastes/"+key+"/versions", strings.Repeat("c", 30), alice).Code)
	require.Equal(t, http.StatusTooManyRequests, d.editContent(key, strings.Repeat("d", 30), alice).Code)
	require.Equal(t, strings.Repeat("b", 30), d.do(http.MethodGet, "/raw/"+key, nil, alice).Body.String())
	require.Equal(t, 2, decode[struct{ Total int }](t, d.api(http.MethodGet, "/api/v1/pastes", alice, nil)).Total)

	// The user storage limit counts revisions
	d2 := newDash(t, func(cfg *config.Config) { cfg.UserStorageLimit = 50 })
	bob := loginAs(t, d2.db, 86, "bob")
	key = d2.ownedUpload(bob, strings.Repeat("a", 20))
	require.Equal(t, http.StatusOK, d2.editContent(key, strings.Repeat("b", 20), bob).Code)
	rec = d2.editContent(key, strings.Repeat("c", 20), bob)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	require.Equal(t, strings.Repeat("b", 20), d2.do(http.MethodGet, "/raw/"+key, nil, bob).Body.String())
}

func TestLanguageAndDownload(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 87, "alice")
	token, _ := d.newToken(alice, "ci")
	language := func(key string) string {
		p, err := d.db.PasteByKey(t.Context(), d.userID(87), key)
		require.NoError(t, err)
		return p.Language
	}

	// From ?ext on /log, from the header, and none
	rec := d.do(http.MethodPut, "/log?ext=py", strings.NewReader("print(1)\n"), nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusOK, rec.Code)
	link := strings.TrimSpace(rec.Body.String())
	pyKey := link[strings.LastIndex(link, "/")+1:]
	require.Equal(t, "python", language(pyKey))
	logKey := d.ownedUpload(alice, "x\n", "X-Paste-Language", "Bash")
	require.Equal(t, "bash", language(logKey))
	_, plain := d.upload("y\n", nil, "Authorization", "Bearer "+token)
	require.Equal(t, "", language(plain))
	_, odd := d.upload("y\n", nil, "Authorization", "Bearer "+token, "X-Paste-Language", "no spaces")
	require.Empty(t, odd, "a bad language header is refused")
	rec = d.do(http.MethodPut, "/log?ext=%22%0D%0A", strings.NewReader("z\n"), nil, "Authorization", "Bearer "+token)
	require.Equal(t, http.StatusOK, rec.Code, "a bad extension is only ignored")

	// PATCH sets it
	rec = d.api(http.MethodPatch, "/api/v1/pastes/"+plain, alice, map[string]any{"language": "go"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "go", language(plain))

	// The download is named after the link's extension, else the language, else .txt
	disposition := func(path string, who *http.Cookie) string {
		rec := d.do(http.MethodGet, path, nil, who)
		require.Equal(t, http.StatusOK, rec.Code, path)
		return rec.Header().Get("Content-Disposition")
	}
	require.Equal(t, `attachment; filename="`+pyKey+`.py"`, disposition("/raw/"+pyKey+"?download=1", alice))
	require.Equal(t, `attachment; filename="`+pyKey+`.py"`, disposition("/raw/"+pyKey+"?download=1", nil))
	require.Equal(t, `attachment; filename="`+pyKey+`.sh"`, disposition("/raw/"+pyKey+".sh?download=1", nil))
	require.Equal(t, `attachment; filename="`+plain+`.go"`, disposition("/raw/"+plain+"?download=1", alice))
	_, anonKey := d.upload("anon\n", nil)
	require.Equal(t, `attachment; filename="`+anonKey+`.txt"`, disposition("/raw/"+anonKey+"?download=1", nil))
	for _, ext := range []string{"%22", "%0D%0Aset-cookie:x", "p%C3%BF", "a%22%3B%20filename%3D%22x", "%E2%80%AE", "toolongextension"} {
		got := disposition("/raw/"+pyKey+"."+ext+"?download=1", nil)
		require.Equal(t, `attachment; filename="`+pyKey+`.py"`, got, ext)
		require.NotContains(t, got, "\r")
		require.NotContains(t, got, "\n")
	}
	require.Empty(t, d.do(http.MethodGet, "/raw/"+pyKey, nil, nil).Header().Get("Content-Disposition"))
}

func TestEditWindowSettingsAndRevisionsKept(t *testing.T) {
	d := newDash(t, func(cfg *config.Config) { cfg.EditMaxRevisions = 2 })
	alice := loginAs(t, d.db, 88, "alice")
	key := d.ownedUpload(alice, "r0")
	require.Equal(t, http.StatusOK, d.editContent(key, "r1", alice).Code)
	require.Equal(t, http.StatusOK, d.editContent(key, "r2", alice).Code)
	rec := d.editContent(key, "r3", alice)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "too_many_revisions")

	// Revisions are pinned like the paste, and deleted with it
	require.True(t, d.stored(accounts.RevisionKey(key, 1)))
	require.True(t, d.stored(accounts.RevisionKey(key, 2)))
	require.Equal(t, http.StatusNoContent, d.api(http.MethodDelete, "/api/v1/pastes/"+key, alice, nil).Code)
	d.srv.drainer.Drain(context.Background())
	for _, storeKey := range []string{key, accounts.RevisionKey(key, 1), accounts.RevisionKey(key, 2)} {
		require.False(t, d.stored(storeKey), storeKey)
	}

	d0 := newDash(t, func(cfg *config.Config) { cfg.EditWindow = 0 })
	bob := loginAs(t, d0.db, 89, "bob")
	key = d0.ownedUpload(bob, "x")
	rec = d0.editContent(key, "y", bob)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), `"reason":"expired"`)
}
