package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// fakeGate answers Check from a table: an error per key, ErrDenied for a
// private key read by anyone but its owner, or a Meta that marks the
// reader owner when there is one
type fakeGate struct {
	errs      map[string]error
	private   map[string]bool
	now       time.Time
	until     time.Time
	checks    atomic.Int64
	delivered atomic.Int64
}

func (g *fakeGate) Check(_ context.Context, key string, p *auth.Principal, _ Op) (*Meta, error) {
	g.checks.Add(1)
	if err := g.errs[key]; err != nil {
		return nil, err
	}
	if g.private[key] && p == nil {
		return nil, ErrDenied
	}
	meta := &Meta{Owner: p != nil, ServerNow: g.now}
	if meta.Owner {
		meta.EditableUntil = g.until
	} else {
		meta.OnDeliver = func() { g.delivered.Add(1) }
	}
	return meta, nil
}

// sizeCounter counts the Size calls a store gets
type sizeCounter struct{ sizes atomic.Int64 }

// countedFile is file storage that counts Size calls; it keeps Open
type countedFile struct {
	*storage.FileStorage
	sizeCounter
}

func (c *countedFile) Size(key string) (int64, error) {
	c.sizes.Add(1)
	return c.FileStorage.Size(key)
}

// countedMemory is a store without Open that counts Size calls
type countedMemory struct {
	mockStorage
	sizeCounter
}

func (c *countedMemory) Size(key string) (int64, error) {
	c.sizes.Add(1)
	return c.mockStorage.Size(key)
}

// gatedHandler serves a gated paste route on a streaming store (file) or a
// loaded one (memory), with "pub" and "priv" stored and "priv" denied to
// everyone but the owner
func gatedHandler(t *testing.T, file bool) (*DocumentHandler, *fakeGate, *sizeCounter, http.Handler) {
	t.Helper()
	var store storage.Storage
	var counter *sizeCounter
	if file {
		fs := storage.NewFileStorage(t.TempDir(), 0).(*storage.FileStorage)
		t.Cleanup(func() { fs.Close() })
		c := &countedFile{FileStorage: fs}
		store, counter = c, &c.sizeCounter
	} else {
		c := &countedMemory{mockStorage: mockStorage{data: map[string]string{}}}
		store, counter = c, &c.sizeCounter
	}
	for _, key := range []string{"pub", "priv", "gone", "broken"} {
		require.NoError(t, store.Set(key, "0123456789 paste "+key, false))
	}
	h := NewDocumentHandler(6, 1<<20, store, &sequenceKeyGenerator{})
	h.NotFound = NewNotFoundLimiter(1000, time.Hour, time.Hour)
	gate := &fakeGate{
		private: map[string]bool{"priv": true},
		errs: map[string]error{
			"gone":   ErrRemoved,
			"broken": errors.New("database is locked"),
		},
		now:   time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		until: time.Date(2026, 10, 28, 12, 10, 0, 0, time.UTC),
	}
	h.Access = gate
	h.Readers = func(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
		switch r.Header.Get("X-Reader") {
		case "owner":
			return &auth.Principal{User: &accounts.User{ID: 1}}, true
		case "revoked":
			auth.WriteInvalidToken(w, false)
			return nil, false
		}
		return nil, true
	}
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	return h, gate, counter, router
}

// contentRoute is one way to read a paste over HTTP; %s is the key
type contentRoute struct {
	name    string
	method  string
	path    string
	headers []string
}

var contentRoutes = []contentRoute{
	{"documents GET", http.MethodGet, "/documents/%s", nil},
	{"documents HEAD", http.MethodHead, "/documents/%s", nil},
	{"documents ext", http.MethodGet, "/documents/%s.log", nil},
	{"raw GET", http.MethodGet, "/raw/%s", nil},
	{"raw HEAD", http.MethodHead, "/raw/%s", nil},
	{"raw ext", http.MethodGet, "/raw/%s.js", nil},
	{"raw range", http.MethodGet, "/raw/%s", []string{"Range", "bytes=0-3"}},
	{"raw multi range", http.MethodGet, "/raw/%s", []string{"Range", "bytes=0-1,4-5"}},
	{"raw suffix range", http.MethodGet, "/raw/%s", []string{"Range", "bytes=-4"}},
	{"raw bad range", http.MethodGet, "/raw/%s", []string{"Range", "bytes=500-600"}},
	{"raw if-none-match", http.MethodGet, "/raw/%s", []string{"If-None-Match", "*"}},
	{"raw if-modified-since", http.MethodGet, "/raw/%s", []string{"If-Modified-Since", "Mon, 02 Jan 2090 15:04:05 GMT"}},
	{"raw download", http.MethodGet, "/raw/%s?download=1", nil},
	{"raw range HEAD", http.MethodHead, "/raw/%s", []string{"Range", "bytes=0-3"}},
}

// get sends one request of a route for key from ip
func (c contentRoute) get(h http.Handler, key, ip string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, fmt.Sprintf(c.path, key), nil)
	req.RemoteAddr = ip + ":1234"
	all := append(append([]string{}, c.headers...), headers...)
	for i := 0; i+1 < len(all); i += 2 {
		req.Header.Set(all[i], all[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGateDeniedIsMissing(t *testing.T) {
	for _, file := range []bool{true, false} {
		h, _, counter, router := gatedHandler(t, file)
		ip := 0
		next := func() string { ip++; return fmt.Sprintf("192.0.2.%d", ip) }

		for _, route := range contentRoutes {
			t.Run(fmt.Sprintf("file=%v/%s", file, route.name), func(t *testing.T) {
				tracked, sizes := h.NotFound.Tracked(), counter.sizes.Load()
				missing := route.get(router, "nokey", next())
				require.Equal(t, http.StatusNotFound, missing.Code)
				require.Equal(t, tracked+1, h.NotFound.Tracked(), "a missing key counts against the client")

				tracked = h.NotFound.Tracked()
				sizes = counter.sizes.Load()
				denied := route.get(router, "priv", next())
				require.Equal(t, missing.Code, denied.Code)
				require.Equal(t, missing.Body.String(), denied.Body.String())
				require.Equal(t, missing.Header(), denied.Header())
				require.Equal(t, "private, no-store", denied.Header().Get("Cache-Control"))
				require.Equal(t, tracked+1, h.NotFound.Tracked(), "a denied key counts like a missing one")
				require.Equal(t, sizes+1, counter.sizes.Load(), "the store is asked for the size, as for a missing key")
				require.NotContains(t, denied.Body.String(), "paste priv")

				// The owner gets it, with every route working as without a gate
				owner := route.get(router, "priv", next(), "X-Reader", "owner")
				require.NotEqual(t, http.StatusNotFound, owner.Code)
				require.Equal(t, "private, no-store", owner.Header().Get("Cache-Control"), route.name)

				// A refused token is 401 before any lookup
				sizes = counter.sizes.Load()
				revoked := route.get(router, "priv", next(), "X-Reader", "revoked")
				require.Equal(t, http.StatusUnauthorized, revoked.Code)
				require.Equal(t, `Bearer realm="hastebin", error="invalid_token"`, revoked.Header().Get("WWW-Authenticate"))
				require.Equal(t, "private, no-store", revoked.Header().Get("Cache-Control"))
				require.Equal(t, sizes, counter.sizes.Load())

				// A removed public paste is 410, a failed check 500, neither touches the store
				gone := route.get(router, "gone", next())
				require.Equal(t, http.StatusGone, gone.Code)
				require.Equal(t, "private, no-store", gone.Header().Get("Cache-Control"))
				broken := route.get(router, "broken", next())
				require.Equal(t, http.StatusInternalServerError, broken.Code)
				require.Equal(t, "private, no-store", broken.Header().Get("Cache-Control"))
				require.Equal(t, sizes, counter.sizes.Load())
				if route.method == http.MethodGet {
					require.Contains(t, gone.Body.String(), "removed by a moderator")
					require.NotContains(t, gone.Body.String(), "paste gone")
					require.NotContains(t, broken.Body.String(), "paste broken")
				}
			})
		}
	}
}

func TestGateOwnerAnswers(t *testing.T) {
	for _, file := range []bool{true, false} {
		_, gate, _, router := gatedHandler(t, file)
		get := func(path string, headers ...string) *httptest.ResponseRecorder {
			return contentRoute{method: http.MethodGet, path: path, headers: headers}.get(router, "priv", "192.0.2.1", "X-Reader", "owner")
		}

		var doc map[string]any
		rec := get("/documents/%s")
		require.Equal(t, http.StatusOK, rec.Code)
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
		require.Equal(t, "0123456789 paste priv", doc["data"])
		require.Equal(t, true, doc["owner"])
		require.Equal(t, "2026-10-28T12:10:00Z", doc["editable_until"])
		require.Equal(t, "2026-10-28T12:00:00Z", doc["server_now"])

		gate.until = time.Time{}
		rec = get("/documents/%s")
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
		require.Contains(t, doc, "editable_until")
		require.Nil(t, doc["editable_until"])

		// A reader who is not the owner gets the JSON of today, nothing more
		rec = contentRoute{method: http.MethodGet, path: "/documents/%s"}.get(router, "pub", "192.0.2.2")
		require.Equal(t, `{"data":"0123456789 paste pub","key":"pub"}`+"\n", rec.Body.String())

		if !file {
			continue
		}
		rec = get("/raw/%s", "Range", "bytes=0-3")
		require.Equal(t, http.StatusPartialContent, rec.Code)
		require.Equal(t, "0123", rec.Body.String())
		rec = get("/raw/%s", "Range", "bytes=-4")
		require.Equal(t, http.StatusPartialContent, rec.Code)
		require.Equal(t, "priv", rec.Body.String())
		rec = get("/raw/%s", "Range", "bytes=0-1,4-5")
		require.Equal(t, http.StatusPartialContent, rec.Code)
		require.Contains(t, rec.Header().Get("Content-Type"), "multipart/byteranges")
		rec = get("/raw/%s", "If-None-Match", "*")
		require.Equal(t, http.StatusNotModified, rec.Code)
		rec = get("/raw/%s", "Range", "bytes=500-600")
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
		require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"), "kept on the errors of ServeContent")
		rec = get("/raw/%s.LOG?download=1")
		require.Equal(t, `attachment; filename="priv.log"`, rec.Header().Get("Content-Disposition"))
	}
}

func TestGateCountsDeliveries(t *testing.T) {
	_, gate, _, router := gatedHandler(t, true)
	read := func(method, path string, headers ...string) {
		rec := contentRoute{method: method, path: path}.get(router, "pub", "192.0.2.1", headers...)
		require.Less(t, rec.Code, 400)
	}
	read(http.MethodHead, "/documents/%s")
	read(http.MethodHead, "/raw/%s")
	read(http.MethodGet, "/documents/%s", "X-Reader", "owner")
	read(http.MethodGet, "/raw/%s", "X-Reader", "owner")
	require.Zero(t, gate.delivered.Load(), "HEAD and the owner never count")

	read(http.MethodGet, "/documents/%s")
	read(http.MethodGet, "/raw/%s")
	read(http.MethodGet, "/raw/%s", "Range", "bytes=0-1")
	require.EqualValues(t, 3, gate.delivered.Load())
}

func TestOpenDocumentAs(t *testing.T) {
	for _, file := range []bool{true, false} {
		h, gate, counter, _ := gatedHandler(t, file)
		ctx := context.Background()

		_, missErr := h.OpenDocumentAs(ctx, nil, "192.0.2.1", "nokey", ResponseMemory, RawReadCost)
		require.ErrorIs(t, missErr, storage.ErrNotFound)
		sizes := counter.sizes.Load()
		_, err := h.OpenDocumentAs(ctx, nil, "192.0.2.2", "priv", ResponseMemory, RawReadCost)
		require.ErrorIs(t, err, storage.ErrNotFound)
		require.Equal(t, sizes+1, counter.sizes.Load())
		require.Equal(t, 2, h.NotFound.Tracked())

		_, err = h.OpenDocumentAs(ctx, nil, "192.0.2.3", "gone", ResponseMemory, RawReadCost)
		require.ErrorIs(t, err, ErrRemoved)

		doc, err := h.OpenDocumentAs(ctx, nil, "192.0.2.3", "pub", ResponseMemory, RawReadCost)
		require.NoError(t, err)
		doc.Close()
		require.EqualValues(t, 1, gate.delivered.Load())

		// OpenDocumentFrom is anonymous
		_, err = h.OpenDocumentFrom("192.0.2.4", "priv", ResponseMemory, RawReadCost)
		require.ErrorIs(t, err, storage.ErrNotFound)
	}
}

func TestGateBanParity(t *testing.T) {
	// A stranger looking up a private key is banned exactly like one
	// looking up missing keys
	h, _, _, router := gatedHandler(t, true)
	h.NotFound = NewNotFoundLimiter(30, time.Hour, time.Hour)
	route := contentRoute{method: http.MethodGet, path: "/raw/%s"}
	var missing, denied []int
	for range 31 {
		missing = append(missing, route.get(router, "nokey", "192.0.2.10").Code)
		denied = append(denied, route.get(router, "priv", "192.0.2.11").Code)
	}
	require.Equal(t, missing, denied)
	require.Equal(t, http.StatusTooManyRequests, denied[30])
	require.Equal(t, http.StatusNotFound, denied[29])
	// Banned now, even for a public paste
	require.Equal(t, http.StatusTooManyRequests, route.get(router, "pub", "192.0.2.11").Code)
}

func TestNoGateUnchanged(t *testing.T) {
	// Without accounts there is no gate: every paste is public
	h, _, _, _ := gatedHandler(t, true)
	h.Access, h.Readers = nil, nil
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	rec := contentRoute{method: http.MethodGet, path: "/raw/%s"}.get(router, "priv", "192.0.2.1")
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.HasSuffix(rec.Body.String(), "priv"))
}
