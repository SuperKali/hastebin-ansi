package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// dripReader returns chunk bytes every interval, total bytes in all
type dripReader struct {
	chunk    int
	interval time.Duration
	left     int
}

func (d *dripReader) Read(p []byte) (int, error) {
	if d.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(d.interval)
	n := min(d.chunk, d.left, len(p))
	for i := range n {
		p[i] = 'a'
	}
	d.left -= n
	return n, nil
}

func (d *dripReader) Close() error { return nil }

func TestUploadGuard_Fast(t *testing.T) {
	// 100 bytes every 10 ms is above 100 bytes per 50 ms
	g := newUploadGuard(&dripReader{chunk: 100, interval: 10 * time.Millisecond, left: 3000}, 100, 50*time.Millisecond, 0, 0, nil)
	data, err := io.ReadAll(g)
	require.NoError(t, err)
	require.Len(t, data, 3000)
	require.False(t, g.TooSlow())
	g.Stop()
}

func TestUploadGuard_Slow(t *testing.T) {
	// 10 bytes every 20 ms is below 100 bytes per 50 ms
	g := newUploadGuard(&dripReader{chunk: 10, interval: 20 * time.Millisecond, left: 3000}, 100, 50*time.Millisecond, 0, 0, nil)
	_, err := io.ReadAll(g)
	require.ErrorIs(t, err, ErrTooSlow)
	require.True(t, g.TooSlow())
}

func TestUploadGuard_SilentClient(t *testing.T) {
	// A client that sends nothing blocks the read; the guard interrupts it
	pr, pw := io.Pipe()
	g := newUploadGuard(pr, 100, 50*time.Millisecond, 0, 0, func() {
		pw.CloseWithError(errors.New("read deadline"))
	})
	start := time.Now()
	_, err := io.ReadAll(g)
	require.ErrorIs(t, err, ErrTooSlow)
	require.Less(t, time.Since(start), time.Second)
}

func TestUploadGuard_StopsAtEOF(t *testing.T) {
	// A short body ends before the first window: the timer must not fire later
	var called atomic.Bool
	g := newUploadGuard(io.NopCloser(strings.NewReader("short")), 100, 20*time.Millisecond, 0, 0, func() { called.Store(true) })
	data, err := io.ReadAll(g)
	require.NoError(t, err)
	require.Equal(t, "short", string(data))
	time.Sleep(60 * time.Millisecond)
	require.False(t, called.Load())
	require.False(t, g.TooSlow())

	// Stop and TooSlow are safe on a nil guard, which GuardUpload returns when the check is off
	var none *UploadGuard
	none.Stop()
	require.False(t, none.TooSlow())
}

// slowUploadServer runs the handler on a real server whose timeouts are much
// shorter than the upload, like the 60 second defaults against a large upload
func slowUploadServer(t *testing.T, uploadTimeout time.Duration) (*httptest.Server, *mockStorage) {
	t.Helper()
	store := &mockStorage{data: make(map[string]string)}
	h := NewDocumentHandler(6, 1<<20, store, &mockKeyGenerator{fixedKey: "test123"})
	h.UploadTimeout = uploadTimeout
	h.MinUploadBytes = 10
	h.UploadWindow = 200 * time.Millisecond
	router := chi.NewRouter()
	h.RegisterRoutes(router)

	ts := httptest.NewUnstartedServer(router)
	ts.Config.ReadTimeout = 300 * time.Millisecond
	ts.Config.WriteTimeout = 300 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, store
}

// dripPost sends n chunks of size bytes, one every interval, as a body of unknown length
func dripPost(t *testing.T, url string, n, size int, interval time.Duration) (*http.Response, error) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		chunk := []byte(strings.Repeat("a", size))
		for i := 0; i < n; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
			time.Sleep(interval)
		}
		pw.Close()
	}()
	t.Cleanup(func() { pw.CloseWithError(io.ErrClosedPipe) })
	return http.Post(url, "text/plain", pr)
}

func TestSlowUploadWithinRate(t *testing.T) {
	// 1.2 seconds of upload against 300 ms server timeouts, above 10 bytes per 200 ms
	ts, store := slowUploadServer(t, 10*time.Second)
	resp, err := dripPost(t, ts.URL+"/log", 12, 50, 100*time.Millisecond)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Len(t, store.data["test123"], 600)
}

func TestSlowUploadWithoutTimeout(t *testing.T) {
	// The same upload fails when the server timeouts are kept
	ts, store := slowUploadServer(t, 0)
	resp, err := dripPost(t, ts.URL+"/log", 12, 50, 100*time.Millisecond)
	if err == nil {
		defer resp.Body.Close()
		require.NotEqual(t, http.StatusOK, resp.StatusCode)
	}
	require.Empty(t, store.data)
}

func TestSlowUploadBelowRate(t *testing.T) {
	// 1 byte every 100 ms is below 10 bytes per 200 ms
	ts, store := slowUploadServer(t, 10*time.Second)
	resp, err := dripPost(t, ts.URL+"/documents", 50, 1, 100*time.Millisecond)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
	require.Contains(t, string(body), "too slow")
	require.Empty(t, store.data)
}

func TestSlowSmallUploadKeepsServerTimeout(t *testing.T) {
	// A small body of known length takes no slot, so it keeps the 300 ms server
	// timeouts: 6 seconds of upload above the minimum rate must not get the 10 second upload timeout
	ts, store := slowUploadServer(t, 10*time.Second)

	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 60; i++ {
			if _, err := pw.Write([]byte("aaaaaaaaaa")); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		pw.Close()
	}()
	t.Cleanup(func() { pw.CloseWithError(io.ErrClosedPipe) })

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/log", pr)
	require.NoError(t, err)
	req.ContentLength = 600

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		require.NotEqual(t, http.StatusOK, resp.StatusCode)
	}
	require.Less(t, time.Since(start), 2*time.Second)
	require.Empty(t, store.data)
}

// dataDir lists the files of a data dir
func dataDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// streamHandler is a handler on file storage in dir
func streamHandler(t *testing.T, dir string, maxLength int) (*DocumentHandler, chi.Router) {
	t.Helper()
	store := storage.NewFileStorage(dir, 0)
	t.Cleanup(func() { store.Close() })
	h := NewDocumentHandler(6, maxLength, store, &sequenceKeyGenerator{})
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	return h, router
}

func TestStreamedUpload(t *testing.T) {
	dir := t.TempDir()
	h, router := streamHandler(t, dir, 40<<20)
	h.SetMemoryBudget(256 << 20)
	h.Quota = NewUploadQuota(1<<30, time.Hour)

	// A slow body that announces 32 MiB holds only the copy buffer, and nothing is visible yet
	pr, pw := io.Pipe()
	req := httptest.NewRequest(http.MethodPost, "/log", pr)
	req.ContentLength = 32 << 20
	req.RemoteAddr = "192.0.2.1:1"
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	_, err := pw.Write([]byte(strings.Repeat("a", 3*CopyBuffer)))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(dataDir(t, dir)) == 1 }, 2*time.Second, 5*time.Millisecond)
	require.EqualValues(t, StreamUploadMemory, h.UploadMemory.InUse())
	require.True(t, strings.HasPrefix(dataDir(t, dir)[0], ".upload-"), dataDir(t, dir))

	// The client goes away: nothing is stored, the temp file and the quota are given back
	pw.CloseWithError(io.ErrUnexpectedEOF)
	require.Equal(t, http.StatusBadRequest, <-done)
	require.Empty(t, dataDir(t, dir))
	require.Zero(t, h.UploadMemory.InUse())
	require.Zero(t, h.UploadsInProgress())
	_, over := h.Quota.Exceeded("192.0.2.1")
	require.False(t, over)

	// A whole body is stored as sent
	body := strings.Repeat("0123456789abcdef", 2<<20)
	rec := sendRequest(router, http.MethodPost, "/documents", strings.NewReader(body))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Equal(t, body, readAll(t, h, out["key"]))
	require.Len(t, dataDir(t, dir), 1)
	require.Zero(t, h.UploadMemory.InUse())
}

func TestStreamedUploadForms(t *testing.T) {
	dir := t.TempDir()
	h, router := streamHandler(t, dir, 1024)

	post := func(body io.Reader, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/log", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	stored := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return readAll(t, h, strings.TrimPrefix(strings.TrimSpace(rec.Body.String()), "https://example.com/"))
	}

	// The data field, as a value or as a file, after other fields
	for _, asFile := range []bool{false, true} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		require.NoError(t, form.WriteField("other", strings.Repeat("x", 2000)))
		if asFile {
			part, err := form.CreateFormFile("data", "build.log")
			require.NoError(t, err)
			part.Write([]byte("from a file"))
		} else {
			require.NoError(t, form.WriteField("data", "from a field"))
		}
		require.NoError(t, form.WriteField("data", "a second data field is ignored"))
		require.NoError(t, form.Close())
		want := "from a field"
		if asFile {
			want = "from a file"
		}
		require.Equal(t, want, stored(post(&body, form.FormDataContentType())))
	}

	// Limits and errors leave nothing behind
	before := dataDir(t, dir)
	body, contentType := multipartBody(t, strings.Repeat("a", 1025))
	require.Equal(t, http.StatusRequestEntityTooLarge, post(body, contentType).Code)
	require.Equal(t, http.StatusRequestEntityTooLarge, post(strings.NewReader(strings.Repeat("a", 1025)), "text/plain").Code)
	body, contentType = multipartBody(t, "")
	require.Equal(t, http.StatusBadRequest, post(body, contentType).Code)
	require.Equal(t, http.StatusBadRequest, post(strings.NewReader(""), "text/plain").Code)
	require.Equal(t, http.StatusBadRequest, post(strings.NewReader("not a form"), "multipart/form-data").Code)
	require.Equal(t, before, dataDir(t, dir))
}

func TestStreamedUploadRefused(t *testing.T) {
	dir := t.TempDir()
	h, router := streamHandler(t, dir, 4<<20)
	h.Quota = NewUploadQuota(3<<20, time.Hour)

	// Past the quota partway: 429, and what was counted is given back
	post := func(size int) int {
		req := httptest.NewRequest(http.MethodPost, "/log", io.NopCloser(strings.NewReader(strings.Repeat("q", size))))
		req.ContentLength = -1
		req.RemoteAddr = "192.0.2.7:1"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, http.StatusOK, post(2<<20))
	require.Equal(t, http.StatusTooManyRequests, post(2<<20))
	require.Equal(t, http.StatusOK, post(1<<20))
	require.Len(t, dataDir(t, dir), 2)

	// A disk that fills up while the body arrives: 507, nothing left behind
	h.Quota = nil
	files := h.Store.(*storage.FileStorage)
	free, err := files.FreeSpace()
	require.NoError(t, err)
	files.SetMinFree(free - 2*CopyBuffer)
	require.Equal(t, http.StatusInsufficientStorage, post(1<<20))
	require.Len(t, dataDir(t, dir), 2)
}

func TestUploadDefaults(t *testing.T) {
	h := NewDocumentHandler(6, 1024, &mockStorage{data: make(map[string]string)}, &mockKeyGenerator{})
	require.Equal(t, 5*time.Minute, h.UploadTimeout)
	require.Equal(t, 64<<10, h.MinUploadBytes)
	require.Equal(t, 10*time.Second, h.UploadWindow)
}

func TestSlowUploadsDoNotStarve(t *testing.T) {
	// One client opens 4 slow uploads from different addresses of its IPv6 /64.
	// Only one gets a slot, so other clients can still upload.
	store := &mockStorage{data: make(map[string]string)}
	h := NewDocumentHandler(6, 4*LargeUpload, store, &sequenceKeyGenerator{})
	h.LimitUploads(8)
	h.LimitUploadsPerIP(1)
	router := chi.NewRouter()
	h.RegisterRoutes(router)

	codes := make(chan int, 4)
	var writers []*io.PipeWriter
	for i := 1; i <= 4; i++ {
		pr, pw := io.Pipe()
		writers = append(writers, pw)
		req := httptest.NewRequest(http.MethodPost, "/log", pr)
		req.ContentLength = -1
		req.RemoteAddr = fmt.Sprintf("[2001:db8::%d]:4000", i)
		go func() {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
		if i == 1 {
			require.Eventually(t, func() bool { return h.UploadsInProgress() == 1 }, 2*time.Second, 5*time.Millisecond)
		}
	}
	t.Cleanup(func() {
		for _, pw := range writers {
			pw.CloseWithError(io.ErrClosedPipe)
		}
	})

	// The other 3 are refused at once
	for i := 0; i < 3; i++ {
		select {
		case code := <-codes:
			require.Equal(t, http.StatusServiceUnavailable, code)
		case <-time.After(2 * time.Second):
			t.Fatal("a second upload from the same /64 was not refused")
		}
	}
	require.Equal(t, 1, h.UploadsInProgress())

	// Another client uploads while the slow one still holds its slot
	req := httptest.NewRequest(http.MethodPost, "/log", strings.NewReader(strings.Repeat("b", LargeUpload+1)))
	req.RemoteAddr = "[2001:db8:1::1]:4000"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// The slow upload ends and frees its slot
	_, err := writers[0].Write([]byte("slow body"))
	require.NoError(t, err)
	require.NoError(t, writers[0].Close())
	require.Equal(t, http.StatusOK, <-codes)
	require.Zero(t, h.UploadsInProgress())
}
