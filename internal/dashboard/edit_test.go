package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/auth"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// editTest is the paste routes, the dashboard API and the gate on a file
// store and a fresh database, with one clock for the handler and the gate
type editTest struct {
	t       *testing.T
	db      *accounts.DB
	store   storage.Storage
	docs    *handler.DocumentHandler
	dash    *Handler
	gate    *Gate
	mux     http.Handler
	alice   *http.Cookie
	bob     *http.Cookie
	aliceID int64

	mu  sync.Mutex
	now time.Time
}

// editCSRF is the CSRF token of the sessions login makes
var editCSRF = base64.RawURLEncoding.EncodeToString(make([]byte, 32))

func newEditTest(t *testing.T, edit func(*editTest)) *editTest {
	t.Helper()
	dir := t.TempDir()
	db, err := accounts.Open(filepath.Join(dir, "hastebin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc, err := auth.New(db, auth.Config{BaseURL: "http://localhost:7777", SessionTTL: time.Hour, SessionIdle: time.Hour})
	require.NoError(t, err)

	store := storage.NewFileStorage(dir, 0)
	t.Cleanup(func() { store.Close() })
	e := &editTest{t: t, db: db, store: store, now: time.Now()}
	clock := func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.now
	}
	e.docs = handler.NewDocumentHandler(10, 1<<20, store, keygenerator.NewRandomKeyGenerator(""))
	locks := &KeyLocks{}
	e.dash = New(svc, db, Limits{MaxLength: 1 << 20}, nil)
	e.dash.Docs, e.dash.Locks, e.dash.EditWindow, e.dash.MaxRevisions, e.dash.now = e.docs, locks, 10*time.Minute, 20, clock
	e.gate = NewGate(db, accounts.NewViews(db), locks, 10*time.Minute)
	e.gate.now = clock
	e.docs.Access, e.docs.Readers, e.docs.Uploaders = e.gate, e.dash.Readers, e.dash.Uploaders
	if edit != nil {
		edit(e)
	}
	mux := chi.NewRouter()
	e.docs.RegisterRoutes(mux)
	e.dash.RegisterRoutes(mux)
	e.mux = mux

	e.alice, e.aliceID = e.login(1, "alice")
	e.bob, _ = e.login(2, "bob")
	return e
}

// login stores a session for a GitHub user and returns its cookie
func (e *editTest) login(githubID int64, login string) (*http.Cookie, int64) {
	ctx := context.Background()
	u, _, err := e.db.LoginUser(ctx, githubID, login, "", true)
	require.NoError(e.t, err)
	id := make([]byte, 32)
	copy(id, login)
	sum := sha256.Sum256(id)
	require.NoError(e.t, e.db.CreateSession(ctx, sum[:], u.ID, make([]byte, 32), time.Now().Add(time.Hour)))
	return &http.Cookie{Name: "hb_session", Value: base64.RawURLEncoding.EncodeToString(id)}, u.ID
}

func (e *editTest) setNow(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = t
}

// do sends a request; a cookie request that changes state gets the CSRF
// token and Origin unless raw is false and the caller set them
func (e *editTest) do(method, path, body string, cookie *http.Cookie, headers ...string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = "198.51.100.7:4444"
	if cookie != nil {
		req.AddCookie(cookie)
		if method != http.MethodGet && method != http.MethodHead {
			req.Header.Set("X-CSRF-Token", editCSRF)
			req.Header.Set("Origin", "http://localhost:7777")
		}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// upload saves content as alice and returns its key and creation time
func (e *editTest) upload(content string) (string, time.Time) {
	rec := e.do(http.MethodPost, "/documents", content, e.alice, "Content-Type", "text/plain")
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct{ Key string }
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &out))
	p, err := e.db.PasteByKey(context.Background(), e.aliceID, out.Key)
	require.NoError(e.t, err)
	return out.Key, p.CreatedAt
}

func (e *editTest) edit(key, content string) *httptest.ResponseRecorder {
	return e.do(http.MethodPut, "/api/v1/pastes/"+key+"/content", content, e.alice, "Content-Type", "text/plain")
}

// raw reads the content as cookie, nil for anonymous
func (e *editTest) raw(key string, cookie *http.Cookie) *httptest.ResponseRecorder {
	return e.do(http.MethodGet, "/raw/"+key, "", cookie)
}

func reasonOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var out errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "edit_window_closed", out.Error)
	return out.Reason
}

func TestEditWindowByClock(t *testing.T) {
	e := newEditTest(t, nil)
	key, created := e.upload("first")

	e.setNow(created.Add(time.Minute))
	rec := e.edit(key, "second")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var item Item
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &item))
	require.Equal(t, 1, item.EditCount)
	require.NotNil(t, item.EditedAt)
	require.Equal(t, int64(len("second")), item.Size)
	require.NotNil(t, item.EditableUntil)
	require.Equal(t, created.Add(10*time.Minute), *item.EditableUntil)
	require.Equal(t, "second", e.raw(key, e.alice).Body.String())

	// The old content is kept, for the owner only
	rec = e.do(http.MethodGet, "/api/v1/pastes/"+key+"/revisions/1", "", e.alice)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "first", rec.Body.String())
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	require.Equal(t, http.StatusNotFound, e.do(http.MethodGet, "/api/v1/pastes/"+key+"/revisions/2", "", e.alice).Code)
	require.Equal(t, http.StatusNotFound, e.do(http.MethodGet, "/api/v1/pastes/"+key+"/revisions/0", "", e.alice).Code)
	require.Equal(t, http.StatusNotFound, e.do(http.MethodGet, "/api/v1/pastes/"+key+"/revisions/x", "", e.alice).Code)

	// One second before the end it still works; at the end it is over
	e.setNow(created.Add(10*time.Minute - time.Second))
	require.Equal(t, http.StatusOK, e.edit(key, "third").Code)
	e.setNow(created.Add(10 * time.Minute))
	require.Equal(t, "expired", reasonOf(t, e.edit(key, "fourth")))
	require.Equal(t, "third", e.raw(key, e.alice).Body.String())

	rec = e.do(http.MethodGet, "/api/v1/pastes/"+key, "", e.alice)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	require.Nil(t, detail["editable_until"])
	require.Len(t, detail["revisions"], 2)
	require.EqualValues(t, 20, detail["max_revisions"])
	require.NotEmpty(t, detail["server_now"])

	// EDIT_WINDOW 0: never
	e.dash.EditWindow = 0
	key2, created2 := e.upload("x")
	e.setNow(created2)
	require.Equal(t, "expired", reasonOf(t, e.edit(key2, "y")))
}

func TestEditClosedBySeen(t *testing.T) {
	e := newEditTest(t, nil)
	key, created := e.upload("one")
	e.setNow(created.Add(time.Minute))

	// The owner's reads and anyone's HEAD keep it open
	require.Equal(t, http.StatusOK, e.raw(key, e.alice).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/documents/"+key, "", e.alice).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodHead, "/raw/"+key, "", nil).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodHead, "/documents/"+key, "", e.bob).Code)
	require.Equal(t, http.StatusOK, e.edit(key, "two").Code)

	// Someone else's content read ends it
	require.Equal(t, "two", e.raw(key, e.bob).Body.String())
	require.Equal(t, "seen", reasonOf(t, e.edit(key, "three")))
	require.Equal(t, "two", e.raw(key, nil).Body.String())

	// An anonymous /documents read ends it too
	key2, _ := e.upload("doc")
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/documents/"+key2, "", nil).Code)
	require.Equal(t, "seen", reasonOf(t, e.edit(key2, "changed")))
}

func TestEditRestoredWhenNotRecorded(t *testing.T) {
	e := newEditTest(t, func(e *editTest) {
		e.docs.UserQuota = handler.NewUploadQuota(100, time.Hour)
	})
	key, created := e.upload("original content")
	e.setNow(created.Add(time.Minute))

	e.dash.recordEdit = func(context.Context, accounts.Edit, accounts.Limits) (*accounts.Paste, error) {
		return nil, errors.New("disk I/O error")
	}
	rec := e.edit(key, strings.Repeat("n", 50))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "original content", e.raw(key, e.alice).Body.String())
	_, err := e.store.Size(accounts.RevisionKey(key, 1))
	require.ErrorIs(t, err, storage.ErrNotFound, "the revision copy is gone")
	revs, err := e.db.Revisions(context.Background(), key)
	require.NoError(t, err)
	require.Empty(t, revs)
	p, err := e.db.PasteByKey(context.Background(), e.aliceID, key)
	require.NoError(t, err)
	require.Zero(t, p.EditCount)
	require.True(t, p.EditedAt.IsZero())

	// The failed edit gave its quota back: 50 more fit in what is left of 100
	e.dash.recordEdit = e.db.RecordEdit
	require.Equal(t, http.StatusOK, e.edit(key, strings.Repeat("m", 50)).Code)
	rec = e.edit(key, strings.Repeat("o", 50))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "quota charged by the edit that was kept")
	require.Equal(t, strings.Repeat("m", 50), e.raw(key, e.alice).Body.String())
	n, _, err := e.db.NextRevision(context.Background(), key)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Deleted while the edit was being put in place: nothing stays in the store
	e.docs.UserQuota = nil
	gone, created := e.upload("doomed")
	e.setNow(created.Add(time.Minute))
	e.dash.recordEdit = func(ctx context.Context, edit accounts.Edit, limits accounts.Limits) (*accounts.Paste, error) {
		if _, _, err := e.db.DeletePastes(ctx, e.aliceID, []string{gone}); err != nil {
			return nil, err
		}
		return e.db.RecordEdit(ctx, edit, limits)
	}
	require.Equal(t, http.StatusNotFound, e.edit(gone, "late edit").Code)
	for _, storeKey := range []string{gone, accounts.RevisionKey(gone, 1)} {
		_, err := e.store.Size(storeKey)
		require.ErrorIs(t, err, storage.ErrNotFound, storeKey)
	}
}

func TestEditMaxRevisions(t *testing.T) {
	e := newEditTest(t, func(e *editTest) { e.dash.MaxRevisions = 2 })
	key, created := e.upload("v0")
	e.setNow(created.Add(time.Minute))
	require.Equal(t, http.StatusOK, e.edit(key, "v1").Code)
	require.Equal(t, http.StatusOK, e.edit(key, "v2").Code)
	rec := e.edit(key, "v3")
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "too_many_revisions")
	require.Equal(t, "v2", e.raw(key, e.alice).Body.String())

	// A new version is still possible, with the history left behind
	rec = e.do(http.MethodPost, "/api/v1/pastes/"+key+"/versions", "v3", e.alice, "Content-Type", "text/plain")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestEditRaceNeverShowsTwoContents(t *testing.T) {
	// 100 goroutines inside the window: owner edits and reads by others. A
	// reader other than the owner always gets the same content, and no edit
	// lands once one of them got it.
	e := newEditTest(t, func(e *editTest) {
		e.dash.MaxRevisions = 0
		e.dash.writes = func(next http.Handler) http.Handler { return next }
	})
	key, created := e.upload("start")
	e.setNow(created.Add(time.Minute))

	type got struct {
		reader  bool
		status  int
		content string
	}
	results := make(chan got, 100)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch i % 4 {
			case 0:
				rec := e.raw(key, nil)
				results <- got{reader: true, status: rec.Code, content: rec.Body.String()}
			case 1:
				rec := e.do(http.MethodGet, "/documents/"+key, "", e.bob)
				var doc struct{ Data string }
				json.Unmarshal(rec.Body.Bytes(), &doc)
				results <- got{reader: true, status: rec.Code, content: doc.Data}
			default:
				rec := e.edit(key, fmt.Sprintf("edit %d", i))
				results <- got{status: rec.Code}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	seen := map[string]bool{}
	edits := 0
	for r := range results {
		if r.reader {
			require.Equal(t, http.StatusOK, r.status)
			seen[r.content] = true
			continue
		}
		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, r.status)
		if r.status == http.StatusOK {
			edits++
		}
	}
	require.Len(t, seen, 1, "readers other than the owner saw %v", seen)
	for content := range seen {
		require.Equal(t, content, e.raw(key, e.alice).Body.String(), "no edit after the first read by someone else")
	}
	revs, err := e.db.Revisions(context.Background(), key)
	require.NoError(t, err)
	require.Len(t, revs, edits)
	require.Zero(t, e.dash.Locks.held(), "every lock is released")
}

func TestEditBoundaryWaitsForSwap(t *testing.T) {
	// A read just past the window end waits for an edit holding the lock
	e := newEditTest(t, nil)
	key, created := e.upload("old")
	e.setNow(created.Add(10*time.Minute + time.Second))
	unlock := e.dash.Locks.Lock(key)
	done := make(chan string)
	go func() { done <- e.raw(key, nil).Body.String() }()
	select {
	case <-done:
		t.Fatal("the read did not wait for the lock")
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, e.store.Set(key, "new", true))
	unlock()
	require.Equal(t, "new", <-done)

	// Past the grace nothing waits
	e.setNow(created.Add(10*time.Minute + editGrace))
	unlock = e.dash.Locks.Lock(key)
	defer unlock()
	require.Equal(t, "new", e.raw(key, nil).Body.String())
}
