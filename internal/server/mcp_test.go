package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/config"
	"github.com/stretchr/testify/require"
)

// mcpAnswer is the result of one MCP tool call
type mcpAnswer struct {
	status  int
	header  http.Header
	isError bool
	text    string
	out     map[string]any
}

// mcpPost sends one JSON-RPC request to /mcp with headers and returns the recorder
func (d *dash) mcpPost(method string, params any, headers ...string) *httptest.ResponseRecorder {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(d.t, err)
	return d.do(http.MethodPost, "/mcp", strings.NewReader(string(body)), nil,
		append([]string{"Content-Type", "application/json", "Accept", "application/json, text/event-stream",
			"Mcp-Protocol-Version", "2025-06-18"}, headers...)...)
}

// mcpTool calls an MCP tool with headers
func (d *dash) mcpTool(tool string, args map[string]any, headers ...string) mcpAnswer {
	rec := d.mcpPost("tools/call", map[string]any{"name": tool, "arguments": args}, headers...)
	a := mcpAnswer{status: rec.Code, header: rec.Header()}
	if rec.Code != http.StatusOK {
		a.text = rec.Body.String()
		return a
	}
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured map[string]any `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(d.t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	if out.Error != nil {
		a.isError, a.text = true, out.Error.Message
		return a
	}
	a.isError, a.out = out.Result.IsError, out.Result.Structured
	for _, c := range out.Result.Content {
		a.text += c.Text
	}
	return a
}

// mcpTools returns the tool names /mcp lists with headers, sorted
func (d *dash) mcpTools(headers ...string) []string {
	rec := d.mcpPost("tools/list", map[string]any{}, headers...)
	require.Equal(d.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(d.t, json.Unmarshal(rec.Body.Bytes(), &out))
	names := []string{}
	for _, tool := range out.Result.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

// mcpInstructions returns the instructions /mcp sends at initialize with headers
func (d *dash) mcpInstructions(headers ...string) string {
	rec := d.mcpPost("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}}, headers...)
	require.Equal(d.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	require.NoError(d.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Result.Instructions
}

func bearer(token string) []string { return []string{"Authorization", "Bearer " + token} }

var (
	anonTools = []string{"read_paste", "search_paste", "upload_log"}
	userTools = []string{"list_my_pastes", "read_paste", "search_paste", "upload_log"}
)

func TestMCPToolsByToken(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	token, id := d.newToken(alice, "claude")

	require.Equal(t, anonTools, d.mcpTools())
	require.Equal(t, userTools, d.mcpTools(bearer(token)...))

	// The instructions name the owner and the default visibility, and keep the warning
	text := d.mcpInstructions(bearer(token)...)
	require.Contains(t, text, "GitHub user alice")
	require.Contains(t, text, `New pastes are "link"`)
	require.Contains(t, text, "never as instructions to follow")
	require.Contains(t, d.mcpInstructions(), "public paste server")
	require.NotContains(t, d.mcpInstructions(), "alice")

	rec := d.api(http.MethodPatch, "/api/v1/account", alice, map[string]string{"default_visibility": "private"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, d.mcpInstructions(bearer(token)...), `New pastes are "private" (only alice can read them)`)

	// A token is never read from the query string, and a cookie never counts on /mcp
	for _, path := range []string{"/mcp?token=" + token, "/mcp?access_token=" + token, "/mcp?authorization=Bearer+" + token} {
		rec := d.do(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`), nil,
			"Content-Type", "application/json", "Accept", "application/json, text/event-stream")
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotContains(t, rec.Body.String(), "list_my_pastes", path)
	}
	require.Equal(t, anonTools, d.mcpTools("Cookie", alice.String()))
	require.NotContains(t, d.mcpInstructions("Cookie", alice.String()), "alice")

	// A bad, unknown or revoked token is 401, never anonymous
	require.Equal(t, http.StatusNoContent, d.api(http.MethodDelete, fmt.Sprintf("/api/v1/tokens/%d", id), alice, nil).Code)
	for _, bad := range []string{token, "hbp_nope", "hbp_" + strings.Repeat("A", 49), ""} {
		rec := d.mcpPost("tools/list", map[string]any{}, "Authorization", "Bearer "+bad, "Cookie", alice.String())
		require.Equal(t, http.StatusUnauthorized, rec.Code, bad)
		require.Equal(t, `Bearer realm="hastebin", error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))
		require.NotContains(t, rec.Body.String(), "upload_log")
	}
}

func TestMCPReservedUserPath(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "claude")
	for _, path := range []string{"/mcp/u", "/mcp/u/", "/mcp/u/x"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := d.do(method, path, strings.NewReader(`{}`), nil, bearer(token)...)
			require.Equal(t, http.StatusNotFound, rec.Code, method+" "+path)
			require.NotContains(t, rec.Body.String(), "<html", method+" "+path)
		}
	}
}

func TestMCPUploadOwned(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "claude")
	aliceID := d.userID(11)

	a := d.mcpTool("upload_log", map[string]any{"content": "boot log line\n", "extension": "log", "title": "  Boot of board X ",
		"visibility": "private", "tags": []string{" Armbian", "boot", "armbian", "rk3588"}, "expires_at": "2099-01-02T03:04:05Z"},
		bearer(token)...)
	require.False(t, a.isError, a.text)
	key := a.out["key"].(string)
	require.Equal(t, aliceID, d.owner(key))
	require.Equal(t, "private", a.out["visibility"])
	require.Equal(t, "Boot of board X", a.out["title"])
	require.Equal(t, []any{"armbian", "boot", "rk3588"}, a.out["tags"])
	require.Equal(t, "2099-01-02T03:04:05Z", a.out["expires_at"])
	require.True(t, strings.HasSuffix(a.out["url"].(string), "/"+key+".log"))
	require.Contains(t, a.text, "Visibility: private")

	var title, language string
	var visibility int
	var expires int64
	require.NoError(t, d.db.R.QueryRow("SELECT title, language, visibility, expires_at FROM pastes WHERE key = ?", key).
		Scan(&title, &language, &visibility, &expires))
	require.Equal(t, "Boot of board X", title)
	require.Equal(t, "log", language)
	require.Equal(t, 1, visibility)
	require.Equal(t, time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC).Unix(), expires)

	// Private: anonymous readers get a missing paste, the owner reads it
	require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/raw/"+key, nil, nil).Code)
	r := d.mcpTool("read_paste", map[string]any{"key_or_url": key})
	require.True(t, r.isError)
	r = d.mcpTool("read_paste", map[string]any{"key_or_url": key}, bearer(token)...)
	require.False(t, r.isError, r.text)
	require.Contains(t, r.text, "boot log line")

	// Without visibility the account default holds: link, then private
	a = d.mcpTool("upload_log", map[string]any{"content": "default one"}, bearer(token)...)
	require.False(t, a.isError, a.text)
	require.Equal(t, "link", a.out["visibility"])
	require.Equal(t, []any{}, a.out["tags"])
	require.Nil(t, a.out["expires_at"])
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/raw/"+a.out["key"].(string), nil, nil).Code)
	rec := d.api(http.MethodPatch, "/api/v1/account", alice, map[string]string{"default_visibility": "private"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	a = d.mcpTool("upload_log", map[string]any{"content": "default two"}, bearer(token)...)
	require.False(t, a.isError, a.text)
	require.Equal(t, "private", a.out["visibility"])
	require.Equal(t, http.StatusNotFound, d.do(http.MethodGet, "/raw/"+a.out["key"].(string), nil, nil).Code)

	// Bad parameters store nothing
	files := d.files()
	for name, args := range map[string]map[string]any{
		"visibility":   {"content": "x", "visibility": "public"},
		"past expiry":  {"content": "x", "expires_at": "2001-01-01T00:00:00Z"},
		"bad expiry":   {"content": "x", "expires_at": "tomorrow"},
		"bad tag":      {"content": "x", "tags": []string{"no spaces"}},
		"tag too long": {"content": "x", "tags": []string{strings.Repeat("a", 33)}},
		"11 tags":      {"content": "x", "tags": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}},
		"title":        {"content": "x", "title": strings.Repeat("t", 201)},
		"control":      {"content": "x", "title": "a\nb"},
		"empty":        {"content": "  "},
	} {
		a := d.mcpTool("upload_log", args, bearer(token)...)
		require.True(t, a.isError, name)
	}
	require.Equal(t, files, d.files())

	// Ten tags, duplicates dropped, are fine
	a = d.mcpTool("upload_log", map[string]any{"content": "x", "tags": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "J", " a "}},
		bearer(token)...)
	require.False(t, a.isError, a.text)
	require.Len(t, a.out["tags"], 10)

	// Anonymous uploads stay public and unowned
	a = d.mcpTool("upload_log", map[string]any{"content": "anon"})
	require.False(t, a.isError, a.text)
	require.Zero(t, d.owner(a.out["key"].(string)))
	require.Equal(t, http.StatusOK, d.do(http.MethodGet, "/raw/"+a.out["key"].(string), nil, nil).Code)
}

// Arguments the schema does not name are refused on both servers, and an
// argument only an owned paste has, sent without a token, never falls back
// to a public paste: nothing is stored
func TestMCPStrictArguments(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "claude")
	files := d.files()

	for name, args := range map[string]map[string]any{
		"visibility": {"content": "x", "visibility": "private"},
		"title":      {"content": "x", "title": "t"},
		"tags":       {"content": "x", "tags": []string{"a"}},
		"expires_at": {"content": "x", "expires_at": "2099-01-02T03:04:05Z"},
	} {
		a := d.mcpTool("upload_log", args)
		require.True(t, a.isError, name)
		require.Contains(t, a.text, name+" needs a personal token", name)
		require.Contains(t, a.text, "nothing was stored", name)
		require.NotContains(t, a.text, "public to anyone", name)
	}
	a := d.mcpTool("upload_log", map[string]any{"content": "x", "visibility": "private", "title": "t"})
	require.True(t, a.isError)
	require.Contains(t, a.text, "title and visibility need a personal token")
	for name, args := range map[string]map[string]any{
		"unknown":    {"content": "x", "foo": 1},
		"other case": {"Content": "x"},
		"null":       {"content": "x", "visibility": nil},
	} {
		a := d.mcpTool("upload_log", args)
		require.True(t, a.isError, name)
		require.Contains(t, a.text, "nothing was stored", name)
	}
	require.Equal(t, files, d.files(), "nothing stored")

	// The token server is just as strict
	for name, call := range map[string]struct {
		tool string
		args map[string]any
	}{
		"upload":     {"upload_log", map[string]any{"content": "x", "visibilty": "private"}},
		"upload key": {"upload_log", map[string]any{"content": "x", "Visibility": "private"}},
		"list":       {"list_my_pastes", map[string]any{"pages": 2}},
		"read":       {"read_paste", map[string]any{"key_or_url": "abc", "foo": 1}},
		"search":     {"search_paste", map[string]any{"key_or_url": "abc", "pattern": "x", "foo": 1}},
	} {
		a := d.mcpTool(call.tool, call.args, bearer(token)...)
		require.True(t, a.isError, name)
	}
	a = d.mcpTool("upload_log", map[string]any{"content": "x", "visibilty": "private"}, bearer(token)...)
	require.Contains(t, a.text, `unknown argument "visibilty", so nothing was stored`)
	require.Equal(t, files, d.files(), "nothing stored")

	// Anonymous reads refuse unknown arguments too, as the SDK checks their schema
	for _, tool := range []string{"read_paste", "search_paste"} {
		a := d.mcpTool(tool, map[string]any{"key_or_url": "abc", "pattern": "x", "foo": 1})
		require.True(t, a.isError, tool)
	}

	// Valid calls work as before on both servers
	a = d.mcpTool("upload_log", map[string]any{"content": "anon", "extension": "log"})
	require.False(t, a.isError, a.text)
	require.Contains(t, a.text, "public to anyone with the link")
	a = d.mcpTool("upload_log", map[string]any{"content": "mine", "visibility": "private", "title": "t"}, bearer(token)...)
	require.False(t, a.isError, a.text)
	a = d.mcpTool("list_my_pastes", map[string]any{"page": 1, "query": "", "tags": []string{}}, bearer(token)...)
	require.False(t, a.isError, a.text)
}

// The token upload_log refuses null for the arguments only an owned paste
// has, as its schema types them string or array: never the default in
// silence. content and extension take null as on the anonymous server.
func TestMCPUserNullArguments(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 12, "alice")
	token, _ := d.newToken(alice, "claude")
	files := d.files()

	for _, name := range []string{"visibility", "title", "tags", "expires_at"} {
		a := d.mcpTool("upload_log", map[string]any{"content": "x", name: nil}, bearer(token)...)
		require.True(t, a.isError, name)
		require.Contains(t, a.text, `"`+name+`" is null, so nothing was stored`, name)
	}
	a := d.mcpTool("upload_log", map[string]any{"content": "x", "title": nil, "visibility": nil}, bearer(token)...)
	require.True(t, a.isError)
	require.Contains(t, a.text, `"title" and "visibility" are null`)
	require.Equal(t, files, d.files(), "nothing stored")

	// Strings that only look like null are just values
	a = d.mcpTool("upload_log", map[string]any{"content": "x", "title": "null", "extension": nil}, bearer(token)...)
	require.False(t, a.isError, a.text)
	require.Equal(t, "null", a.out["title"])

	// The anonymous upload_log is unchanged: extension null is no extension
	a = d.mcpTool("upload_log", map[string]any{"content": "anon", "extension": nil})
	require.False(t, a.isError, a.text)
}

func TestMCPUserLimits(t *testing.T) {
	d := newDash(t, func(cfg *config.Config) {
		cfg.RegisteredUploadQuota = 100
		cfg.UserMaxPastes = 3
		cfg.UploadQuota = 1 << 20
	})
	alice := loginAs(t, d.db, 11, "alice")
	bob := loginAs(t, d.db, 12, "bob")
	aliceToken, _ := d.newToken(alice, "claude")
	bobToken, _ := d.newToken(bob, "claude")

	// The quota counts per account, whatever address the requests come from
	a := d.mcpTool("upload_log", map[string]any{"content": strings.Repeat("a", 80)}, bearer(aliceToken)...)
	require.False(t, a.isError, a.text)
	a = d.mcpTool("upload_log", map[string]any{"content": strings.Repeat("b", 30)},
		append(bearer(aliceToken), "X-Test-Remote", "203.0.113.50:1")...)
	require.True(t, a.isError)
	require.Contains(t, a.text, "upload quota of your account")

	// Other accounts and anonymous uploads from the same address are not charged with it
	a = d.mcpTool("upload_log", map[string]any{"content": strings.Repeat("c", 30)}, bearer(bobToken)...)
	require.False(t, a.isError, a.text)
	a = d.mcpTool("upload_log", map[string]any{"content": strings.Repeat("d", 300)})
	require.False(t, a.isError, a.text)

	// The paste count limit
	for i := 0; i < 2; i++ {
		a = d.mcpTool("upload_log", map[string]any{"content": "e"}, bearer(bobToken)...)
		require.False(t, a.isError, a.text)
	}
	a = d.mcpTool("upload_log", map[string]any{"content": "f"}, bearer(bobToken)...)
	require.True(t, a.isError)
	require.Contains(t, a.text, "most pastes it may (3)")
	var n int
	require.NoError(t, d.db.R.QueryRow("SELECT COUNT(*) FROM pastes WHERE user_id = ?", d.userID(12)).Scan(&n))
	require.Equal(t, 3, n)
}

func TestMCPStorageLimit(t *testing.T) {
	d := newDash(t, func(cfg *config.Config) { cfg.UserStorageLimit = 50 })
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "claude")
	a := d.mcpTool("upload_log", map[string]any{"content": strings.Repeat("a", 40)}, bearer(token)...)
	require.False(t, a.isError, a.text)
	a = d.mcpTool("upload_log", map[string]any{"content": strings.Repeat("a", 20)}, bearer(token)...)
	require.True(t, a.isError)
	require.Contains(t, a.text, "room for 10 more")
	a = d.mcpTool("upload_log", map[string]any{"content": strings.Repeat("a", 10)}, bearer(token)...)
	require.False(t, a.isError, a.text)
	a = d.mcpTool("upload_log", map[string]any{"content": "a"}, bearer(token)...)
	require.True(t, a.isError)
	require.Contains(t, a.text, "storage is full")
}

func TestMCPOtherUsersPrivate(t *testing.T) {
	a := newAccessTest(t)
	bobToken := a.bobToken
	private := a.keys["private"]

	for i, tool := range []string{"read_paste", "search_paste"} {
		args := func(key string) map[string]any {
			if tool == "search_paste" {
				return map[string]any{"key_or_url": key, "pattern": "content"}
			}
			return map[string]any{"key_or_url": key}
		}
		// From fresh addresses, so the not-found limiter tracks each one anew
		missing := a.mcpTool(tool, args(a.keys["missing"]), append(bearer(bobToken), "X-Test-Remote", fmt.Sprintf("203.0.113.%d:1", 10*i+1))...)
		tracked := a.srv.docs.NotFound.Tracked()
		got := a.mcpTool(tool, args(private), append(bearer(bobToken), "X-Test-Remote", fmt.Sprintf("203.0.113.%d:1", 10*i+2))...)
		require.Equal(t, tracked+1, a.srv.docs.NotFound.Tracked(), "Miss() counted for "+tool)
		require.True(t, got.isError)
		require.NotContains(t, got.text, "content line")
		require.Equal(t, strings.ReplaceAll(missing.text, a.keys["missing"], "KEY"), strings.ReplaceAll(got.text, private, "KEY"), tool)
		require.Equal(t, missing.status, got.status)

		// The owner's token reads it
		got = a.mcpTool(tool, args(private), bearer(a.aliceToken)...)
		require.False(t, got.isError, got.text)
		require.Contains(t, got.text, "content line")
	}
}

func TestMCPOwnReadKeepsEditWindow(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	token, _ := d.newToken(alice, "claude")
	keys := map[string]string{}
	for _, visibility := range []string{"private", "link"} {
		a := d.mcpTool("upload_log", map[string]any{"content": "mine\n", "visibility": visibility}, bearer(token)...)
		require.False(t, a.isError, a.text)
		keys[visibility] = a.out["key"].(string)
	}
	seen := func(key string) bool {
		var at *int64
		require.NoError(t, d.db.R.QueryRow("SELECT seen_at FROM pastes WHERE key = ?", key).Scan(&at))
		return at != nil
	}
	for _, key := range keys {
		d.mcpTool("read_paste", map[string]any{"key_or_url": key}, bearer(token)...)
		d.mcpTool("search_paste", map[string]any{"key_or_url": key, "pattern": "mine"}, bearer(token)...)
		require.False(t, seen(key), "reading your own paste never ends the edit window")
	}
	require.NoError(t, d.srv.views.Flush(t.Context()))
	var views int
	require.NoError(t, d.db.R.QueryRow("SELECT SUM(views) FROM pastes").Scan(&views))
	require.Zero(t, views)

	// Someone else reading the public one does
	d.mcpTool("read_paste", map[string]any{"key_or_url": keys["link"]})
	require.True(t, seen(keys["link"]))
	require.False(t, seen(keys["private"]))
}

func TestMCPListMyPastes(t *testing.T) {
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	bob := loginAs(t, d.db, 12, "bob")
	aliceToken, _ := d.newToken(alice, "claude")
	bobToken, _ := d.newToken(bob, "claude")

	mine := map[string]bool{}
	for i := 0; i < 25; i++ {
		args := map[string]any{"content": fmt.Sprintf("SECRET CONTENT %d\n", i), "title": fmt.Sprintf("build %d", i)}
		if i%5 == 0 {
			args["tags"] = []string{"nightly", "rk3588"}
			args["visibility"] = "private"
		}
		a := d.mcpTool("upload_log", args, bearer(aliceToken)...)
		require.False(t, a.isError, a.text)
		mine[a.out["key"].(string)] = true
	}
	for i := 0; i < 3; i++ {
		a := d.mcpTool("upload_log", map[string]any{"content": "bob's", "title": "build bob", "tags": []string{"nightly"}}, bearer(bobToken)...)
		require.False(t, a.isError, a.text)
	}
	_, anon := d.upload("anonymous build\n", nil)
	require.NotEmpty(t, anon)

	list := func(args map[string]any) mcpAnswer {
		a := d.mcpTool("list_my_pastes", args, bearer(aliceToken)...)
		require.False(t, a.isError, a.text)
		require.NotContains(t, a.text, "SECRET CONTENT")
		return a
	}
	items := func(a mcpAnswer) []map[string]any {
		out := []map[string]any{}
		for _, item := range a.out["items"].([]any) {
			m := item.(map[string]any)
			require.True(t, mine[m["key"].(string)], "only her own pastes")
			require.NotContains(t, m, "content")
			for _, field := range []string{"key", "url", "title", "language", "visibility", "size", "created_at", "tags", "version"} {
				require.Contains(t, m, field)
			}
			out = append(out, m)
		}
		return out
	}

	a := list(map[string]any{})
	require.Len(t, items(a), 20, "20 per page")
	require.EqualValues(t, 25, a.out["total"])
	require.EqualValues(t, 2, a.out["pages"])
	require.Contains(t, a.text, "next_page: 2")

	a = list(map[string]any{"page": 2})
	require.Len(t, items(a), 5)
	require.Contains(t, a.text, "next_page: none")
	a = list(map[string]any{"page": 9})
	require.Empty(t, items(a))

	// Tags (normalized), AND; query on the title
	a = list(map[string]any{"tags": []string{" Nightly "}})
	require.Len(t, items(a), 5)
	for _, item := range items(a) {
		require.Equal(t, "private", item["visibility"])
		require.Equal(t, []any{"nightly", "rk3588"}, item["tags"])
	}
	require.Len(t, items(list(map[string]any{"tags": []string{"nightly", "rk3588"}})), 5)
	require.Empty(t, items(list(map[string]any{"tags": []string{"nightly", "other"}})))
	require.Len(t, items(list(map[string]any{"query": "build 1"})), 11) // 1, 10-19
	require.Empty(t, items(list(map[string]any{"query": "build bob"})))
	require.Empty(t, items(list(map[string]any{"query": "anonymous build"})))

	// Bad input
	for _, args := range []map[string]any{
		{"tags": []string{"bad tag"}},
		{"query": strings.Repeat("q", 201)},
		{"page": 2_000_000},
	} {
		a := d.mcpTool("list_my_pastes", args, bearer(aliceToken)...)
		require.True(t, a.isError, args)
	}

	// Bob sees only his three, never alice's
	b := d.mcpTool("list_my_pastes", map[string]any{}, bearer(bobToken)...)
	require.False(t, b.isError)
	require.EqualValues(t, 3, b.out["total"])
	for key := range mine {
		require.NotContains(t, b.text, key)
	}

	// Without a token the tool does not exist
	n := d.mcpTool("list_my_pastes", map[string]any{})
	require.True(t, n.isError)
	require.NotContains(t, n.text, "build")
}
