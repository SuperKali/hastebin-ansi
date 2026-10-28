package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/stretchr/testify/require"
)

// reader is who sends a read in the access matrix
type reader struct {
	name    string
	headers func(a *accessTest) []string
	owner   bool // owns alice's pastes
	http    bool // counts as owner on the HTTP routes only (a cookie)
	revoked bool
}

var readers = []reader{
	{name: "anonymous", headers: func(*accessTest) []string { return nil }},
	{name: "owner cookie", headers: func(a *accessTest) []string { return []string{"Cookie", a.aliceCookie} }, owner: true, http: true},
	{name: "owner bearer", headers: func(a *accessTest) []string { return []string{"Authorization", "Bearer " + a.aliceToken} }, owner: true},
	{name: "other cookie", headers: func(a *accessTest) []string { return []string{"Cookie", a.bobCookie} }},
	{name: "other bearer", headers: func(a *accessTest) []string { return []string{"Authorization", "Bearer " + a.bobToken} }},
	{name: "revoked token", headers: func(a *accessTest) []string { return []string{"Authorization", "Bearer " + a.revoked} }, revoked: true},
	{name: "owner cookie, revoked token", headers: func(a *accessTest) []string {
		return []string{"Cookie", a.aliceCookie, "Authorization", "Bearer " + a.revoked}
	}, revoked: true},
}

// route is one way to read a paste; %s is the key
type route struct {
	name    string
	method  string
	path    string
	headers []string
	tool    string // an MCP tool instead of a path
}

var routes = []route{
	{name: "documents GET", method: http.MethodGet, path: "/documents/%s"},
	{name: "documents HEAD", method: http.MethodHead, path: "/documents/%s"},
	{name: "documents ext", method: http.MethodGet, path: "/documents/%s.log"},
	{name: "raw GET", method: http.MethodGet, path: "/raw/%s"},
	{name: "raw HEAD", method: http.MethodHead, path: "/raw/%s"},
	{name: "raw ext", method: http.MethodGet, path: "/raw/%s.js"},
	{name: "raw range", method: http.MethodGet, path: "/raw/%s", headers: []string{"Range", "bytes=0-3"}},
	{name: "raw multi range", method: http.MethodGet, path: "/raw/%s", headers: []string{"Range", "bytes=0-1,3-4"}},
	{name: "raw suffix range", method: http.MethodGet, path: "/raw/%s", headers: []string{"Range", "bytes=-5"}},
	{name: "raw if-none-match", method: http.MethodGet, path: "/raw/%s", headers: []string{"If-None-Match", "*"}},
	{name: "raw if-modified-since", method: http.MethodGet, path: "/raw/%s", headers: []string{"If-Modified-Since", "Mon, 02 Jan 2090 15:04:05 GMT"}},
	{name: "raw download", method: http.MethodGet, path: "/raw/%s?download=1"},
	{name: "mcp read_paste", tool: "read_paste"},
	{name: "mcp search_paste", tool: "search_paste"},
}

// accessTest is a server with alice's pastes in every state
type accessTest struct {
	*dash
	aliceCookie, bobCookie string
	alice                  *http.Cookie
	aliceToken, bobToken   string
	revoked                string
	keys                   map[string]string // state -> key
	ip                     int
}

func newAccessTest(t *testing.T) *accessTest {
	t.Helper()
	d := newDash(t, nil)
	alice := loginAs(t, d.db, 11, "alice")
	bob := loginAs(t, d.db, 12, "bob")
	a := &accessTest{dash: d, aliceCookie: alice.String(), bobCookie: bob.String(), alice: alice, keys: map[string]string{}}
	a.aliceToken, _ = d.newToken(alice, "ci")
	a.bobToken, _ = d.newToken(bob, "ci")
	var id int64
	a.revoked, id = d.newToken(alice, "old")
	require.Equal(t, http.StatusNoContent, d.api(http.MethodDelete, fmt.Sprintf("/api/v1/tokens/%d", id), alice, nil).Code)

	owned := func(state, visibility string) {
		rec, key := d.upload(state+" content line\n", nil, "Authorization", "Bearer "+a.aliceToken, "X-Paste-Visibility", visibility)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		a.keys[state] = key
	}
	owned("public", "link")
	owned("private", "private")
	owned("expired", "private")
	owned("expiredpublic", "link")
	_, err := d.db.W.Exec("UPDATE pastes SET expires_at = ? WHERE key IN (?, ?)", time.Now().Add(-time.Minute).Unix(),
		a.keys["expired"], a.keys["expiredpublic"])
	require.NoError(t, err)
	for state, visibility := range map[string]int{"tombpublic": 0, "tombprivate": 1} {
		rec, key := d.upload(state+" content line\n", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		_, err := d.db.W.Exec("INSERT INTO tombstones (key, reason, visibility, removed_at) VALUES (?, 'abuse', ?, ?)",
			key, visibility, time.Now().Unix())
		require.NoError(t, err)
		a.keys[state] = key
	}
	a.keys["missing"] = "nosuchpastekeyzz"

	// An older content of the private paste, under its dotted store key
	priv := a.keys["private"]
	require.NoError(t, d.srv.storage.Set(priv+".r1", "REVISION SECRET\n", true))
	_, err = d.db.W.Exec("INSERT INTO paste_revisions (paste_key, rev, store_key, size, created_at) VALUES (?, 1, ?, 16, 1)", priv, priv+".r1")
	require.NoError(t, err)
	a.keys["revision"] = priv + ".r1"
	return a
}

// result is one answer, HTTP or MCP, with the key in it replaced by KEY
type result struct {
	status  int
	header  http.Header
	body    string
	toolErr bool
	text    string
}

// read sends route for key as who, from a client address of its own
func (a *accessTest) read(rt route, key string, who reader) result {
	a.ip++
	headers := append([]string{"X-Test-Remote", fmt.Sprintf("203.0.%d.%d:5555", a.ip/250, a.ip%250+1)}, who.headers(a)...)
	var rec *httptest.ResponseRecorder
	if rt.tool == "" {
		rec = a.do(rt.method, fmt.Sprintf(rt.path, key), nil, nil, append(headers, rt.headers...)...)
	} else {
		args := map[string]any{"key_or_url": key}
		if rt.tool == "search_paste" {
			args["pattern"] = "content"
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": rt.tool, "arguments": args}})
		require.NoError(a.t, err)
		rec = a.do(http.MethodPost, "/mcp", strings.NewReader(string(body)), nil,
			append(headers, "Content-Type", "application/json", "Accept", "application/json, text/event-stream")...)
	}
	base, _, _ := strings.Cut(key, ".")
	res := result{status: rec.Code, header: rec.Header().Clone(), body: strings.ReplaceAll(rec.Body.String(), base, "KEY")}
	if rt.tool != "" && rec.Code == http.StatusOK {
		var out struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		require.NoError(a.t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
		res.toolErr = out.Result.IsError
		for _, c := range out.Result.Content {
			res.text += strings.ReplaceAll(c.Text, base, "KEY")
		}
	}
	return res
}

// expect is what a reader should get for a paste state on a route:
// "served", "missing" (exactly as a missing key), "removed" or "unauthorized"
func expect(state string, who reader, rt route) string {
	owner := who.owner && (!who.http || rt.tool == "")
	switch {
	case who.revoked:
		return "unauthorized"
	case state == "public":
		return "served"
	case state == "private" || state == "revision":
		if owner {
			return "served"
		}
		return "missing"
	case state == "tombpublic":
		return "removed"
	}
	return "missing" // expired, tombstoned private, missing
}

func TestAccessMatrix(t *testing.T) {
	a := newAccessTest(t)
	states := []string{"public", "private", "expired", "expiredpublic", "tombpublic", "tombprivate", "missing", "revision"}
	for _, rt := range routes {
		for _, who := range readers {
			missing := a.read(rt, a.keys["missing"], who)
			for _, state := range states {
				t.Run(rt.name+"/"+who.name+"/"+state, func(t *testing.T) {
					tracked := a.srv.docs.NotFound.Tracked()
					got := a.read(rt, a.keys[state], who)
					want := expect(state, who, rt)

					require.Equal(t, "private, no-store", got.header.Get("Cache-Control"))
					require.NotContains(t, got.body, "REVISION SECRET")
					require.NotContains(t, got.text, "REVISION SECRET")
					if want != "served" {
						require.NotContains(t, got.body, "content line")
						require.NotContains(t, got.text, "content line")
					}

					switch want {
					case "served":
						require.Equal(t, tracked, a.srv.docs.NotFound.Tracked(), "no miss counted")
						if rt.tool != "" {
							require.Equal(t, http.StatusOK, got.status)
							require.False(t, got.toolErr, got.text)
							require.Contains(t, got.text, "content line")
							return
						}
						require.Contains(t, []int{http.StatusOK, http.StatusPartialContent, http.StatusNotModified}, got.status, got.body)
						if rt.method == http.MethodGet && rt.headers == nil {
							require.Contains(t, got.body, "content line")
						}
					case "missing":
						// Byte for byte the answer to a missing key, and counted against the client the same way
						require.Equal(t, missing.status, got.status)
						require.Equal(t, missing.body, got.body)
						require.Equal(t, missing.header, got.header)
						require.Equal(t, missing.text, got.text)
						require.Equal(t, missing.toolErr, got.toolErr)
						require.Equal(t, tracked+1, a.srv.docs.NotFound.Tracked(), "Miss() called")
						if rt.tool == "" {
							require.Equal(t, http.StatusNotFound, got.status)
						} else {
							require.True(t, got.toolErr)
							require.Contains(t, got.text, "not found")
						}
					case "removed":
						if rt.tool == "" {
							require.Equal(t, http.StatusGone, got.status)
						} else {
							require.True(t, got.toolErr)
							require.Contains(t, got.text, "removed by a moderator")
						}
					case "unauthorized":
						require.Equal(t, http.StatusUnauthorized, got.status)
						require.Equal(t, `Bearer realm="hastebin", error="invalid_token"`, got.header.Get("WWW-Authenticate"))
					}
				})
			}
		}
	}
}

func TestRevisionKeysUnreachable(t *testing.T) {
	a := newAccessTest(t)
	priv := a.keys["private"]
	owner := readers[2]
	for _, path := range []string{"/raw/%s", "/documents/%s", "/raw/%s?download=1"} {
		for _, key := range []string{priv + "%2Er1", priv + "%2er1", priv + ".r1", priv + ".r1.txt"} {
			got := a.read(route{method: http.MethodGet, path: path}, key, owner)
			require.NotContains(t, got.body, "REVISION SECRET", path+" "+key)
		}
	}
	got := a.read(route{tool: "read_paste"}, "https://host/raw/"+priv+".r1", owner)
	require.NotContains(t, got.text, "REVISION SECRET")
}

func TestOwnerDocumentJSON(t *testing.T) {
	a := newAccessTest(t)
	cookie := readers[1]
	for _, state := range []string{"public", "private"} {
		got := a.read(route{method: http.MethodGet, path: "/documents/%s"}, a.keys[state], cookie)
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(got.body), &doc))
		require.Equal(t, true, doc["owner"])
		require.NotNil(t, doc["editable_until"], "inside the edit window, opened by no one else")
		now, err := time.Parse(time.RFC3339, doc["server_now"].(string))
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), now, time.Minute)
		until, err := time.Parse(time.RFC3339, doc["editable_until"].(string))
		require.NoError(t, err)
		require.WithinDuration(t, time.Now().Add(10*time.Minute), until, time.Minute)
	}

	// Someone else opened the public one: the window is over
	got := a.read(route{method: http.MethodGet, path: "/raw/%s"}, a.keys["public"], readers[0])
	require.Equal(t, http.StatusOK, got.status)
	got = a.read(route{method: http.MethodGet, path: "/documents/%s"}, a.keys["public"], cookie)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(got.body), &doc))
	require.Contains(t, doc, "editable_until")
	require.Nil(t, doc["editable_until"])

	// Others get the answer of today
	got = a.read(route{method: http.MethodGet, path: "/documents/%s"}, a.keys["public"], readers[3])
	require.Equal(t, `{"data":"public content line\n","key":"KEY"}`+"\n", got.body)
}

func TestPrivateBanParity(t *testing.T) {
	// 31 lookups of a private key by a stranger ban like 31 of missing keys
	a := newAccessTest(t)
	bob := readers[3]
	for _, rt := range []route{{method: http.MethodGet, path: "/raw/%s"}, {tool: "read_paste"}} {
		var missing, private []string
		for i := range 31 {
			for _, k := range []struct {
				key, ip string
				out     *[]string
			}{{a.keys["missing"], "192.0.2.10", &missing}, {a.keys["private"], "192.0.2.11", &private}} {
				headers := append([]string{"X-Test-Remote", k.ip + ":1"}, bob.headers(a)...)
				var rec *httptest.ResponseRecorder
				if rt.tool == "" {
					rec = a.do(rt.method, fmt.Sprintf(rt.path, k.key), nil, nil, headers...)
					*k.out = append(*k.out, fmt.Sprint(rec.Code))
				} else {
					body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"read_paste","arguments":{"key_or_url":%q}}}`, i, k.key)
					rec = a.do(http.MethodPost, "/mcp", strings.NewReader(body), nil,
						append(headers, "Content-Type", "application/json", "Accept", "application/json, text/event-stream")...)
					text := strings.ReplaceAll(rec.Body.String(), k.key, "KEY")
					*k.out = append(*k.out, text)
				}
			}
		}
		require.Equal(t, missing, private)
		if rt.tool == "" {
			require.Equal(t, "404", private[29])
			require.Equal(t, "429", private[30])
		} else {
			require.Contains(t, private[30], "too many lookups")
		}
		// The next route starts clean
		a.srv.docs.NotFound = handler.NewNotFoundLimiter(30, 15*time.Minute, time.Hour)
	}
}

func TestViewsCounted(t *testing.T) {
	a := newAccessTest(t)
	key := a.keys["public"]
	anon, cookie, bearer, bob := readers[0], readers[1], readers[2], readers[4]
	views := func() (int64, bool) {
		var n int64
		var opened *int64
		require.NoError(t, a.db.R.QueryRow("SELECT views, last_opened_at FROM pastes WHERE key = ?", key).Scan(&n, &opened))
		return n, opened != nil
	}

	// The owner and HEAD never count. MCP takes no cookie: the owner reads it with a token.
	for _, who := range []reader{cookie, bearer} {
		for _, rt := range routes {
			if rt.tool == "" || who.name == bearer.name {
				a.read(rt, key, who)
			}
		}
	}
	a.read(route{method: http.MethodHead, path: "/raw/%s"}, key, anon)
	a.read(route{method: http.MethodHead, path: "/documents/%s"}, key, anon)
	require.NoError(t, a.srv.views.Flush(context.Background()))
	n, opened := views()
	require.Zero(t, n)
	require.False(t, opened)

	// Every content delivery to someone else does, once flushed
	for _, rt := range []route{
		{method: http.MethodGet, path: "/documents/%s"},
		{method: http.MethodGet, path: "/raw/%s"},
		{method: http.MethodGet, path: "/raw/%s", headers: []string{"Range", "bytes=0-3"}},
		{tool: "read_paste"},
		{tool: "search_paste"},
	} {
		a.read(rt, key, anon)
		a.read(rt, key, bob)
	}
	n, _ = views()
	require.Zero(t, n, "counted in memory until the flush")
	require.NoError(t, a.srv.views.Flush(context.Background()))
	n, opened = views()
	require.EqualValues(t, 10, n)
	require.True(t, opened)

	// The dashboard shows them
	rec := a.api(http.MethodGet, "/api/v1/pastes/"+key, a.alice, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	item := decode[map[string]any](t, rec)
	require.EqualValues(t, 10, item["views"])
	require.NotNil(t, item["last_opened_at"])
}

func TestExpiryHidesThenDeletes(t *testing.T) {
	a := newAccessTest(t)
	cookie := a.alice

	// A past expiry is refused
	rec := a.api(http.MethodPatch, "/api/v1/pastes/"+a.keys["private"], cookie,
		map[string]any{"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "future")
	rec, _ = a.upload("x", nil, "Authorization", "Bearer "+a.aliceToken,
		"X-Paste-Expires", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// A future one is kept, and hides the paste from everyone once it passes
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = a.api(http.MethodPatch, "/api/v1/pastes/"+a.keys["public"], cookie, map[string]any{"expires_at": expires})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	key := a.keys["expired"]
	require.True(t, a.stored(key))
	for _, who := range readers[:5] {
		got := a.read(route{method: http.MethodGet, path: "/raw/%s"}, key, who)
		require.Equal(t, http.StatusNotFound, got.status, who.name)
	}

	// The sweeper deletes the row and, through the queue, the content
	n, err := a.srv.sweeper.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n, "the expired private and public pastes")
	require.Zero(t, a.owner(key))
	require.Zero(t, a.owner(a.keys["expiredpublic"]))
	require.NotZero(t, a.owner(a.keys["public"]), "a future expiry stays")
	a.srv.drainer.Drain(context.Background())
	require.False(t, a.stored(key))
	require.False(t, a.stored(a.keys["expiredpublic"]))
	require.True(t, a.stored(a.keys["public"]))

	// Gone from the dashboard too
	rec = a.api(http.MethodGet, "/api/v1/pastes/"+key, cookie, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
