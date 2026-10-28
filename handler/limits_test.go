package handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func TestInflightLimiter(t *testing.T) {
	l := NewInflightLimiter(2)
	r1, ok := l.Acquire("192.0.2.1")
	require.True(t, ok)
	r2, ok := l.Acquire("192.0.2.1")
	require.True(t, ok)
	_, ok = l.Acquire("192.0.2.1")
	require.False(t, ok)
	require.Equal(t, 2, l.InUse("192.0.2.1"))
	requests, clients := l.Totals()
	require.Equal(t, 2, requests)
	require.Equal(t, 1, clients)

	// Other clients have their own count; an IPv6 /64 is one client
	_, ok = l.Acquire("192.0.2.2")
	require.True(t, ok)
	a, ok := l.Acquire("2001:db8::1")
	require.True(t, ok)
	b, ok := l.Acquire("2001:db8::2")
	require.True(t, ok)
	_, ok = l.Acquire("2001:db8::3")
	require.False(t, ok)
	_, ok = l.Acquire("2001:db8:0:1::1")
	require.True(t, ok)
	a()
	b()

	// Release frees one place, once
	r1()
	r1()
	require.Equal(t, 1, l.InUse("192.0.2.1"))
	_, ok = l.Acquire("192.0.2.1")
	require.True(t, ok)
	r2()

	// An unknown client and a nil limiter never refuse
	for range 5 {
		_, ok = l.Acquire("")
		require.True(t, ok)
	}
	require.Nil(t, NewInflightLimiter(0))
	require.Nil(t, NewInflightLimiter(-1))
	var none *InflightLimiter
	release, ok := none.Acquire("192.0.2.1")
	require.True(t, ok)
	release()
	require.Zero(t, none.InUse("192.0.2.1"))
	requests, clients = none.Totals()
	require.Zero(t, requests)
	require.Zero(t, clients)
}

func TestUploadGuard_Sustained(t *testing.T) {
	// 100 bytes every 10 ms, 10000 bytes per second, passes the floor of 100
	// bytes per 50 ms but not the sustained 20000 bytes per second after 200 ms
	start := time.Now()
	g := newUploadGuard(&dripReader{chunk: 100, interval: 10 * time.Millisecond, left: 1 << 20}, 100, 50*time.Millisecond,
		200*time.Millisecond, 20000, nil)
	_, err := io.ReadAll(g)
	require.ErrorIs(t, err, ErrTooSlow)
	require.True(t, g.TooSlow())
	require.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
	require.Less(t, time.Since(start), 2*time.Second)

	// The same rate passes when it is above the sustained rate
	g = newUploadGuard(&dripReader{chunk: 1000, interval: 10 * time.Millisecond, left: 50000}, 100, 50*time.Millisecond,
		200*time.Millisecond, 20000, nil)
	data, err := io.ReadAll(g)
	require.NoError(t, err)
	require.Len(t, data, 50000)
	require.False(t, g.TooSlow())

	// The defaults: a minute, then 64 KiB per second on average
	h := NewDocumentHandler(6, 1024, &mockStorage{data: map[string]string{}}, &sequenceKeyGenerator{})
	require.Equal(t, time.Minute, h.SustainAfter)
	require.Equal(t, 64<<10, h.SustainRate)
}

func TestSustainedRateHTTP(t *testing.T) {
	// A large upload that keeps the floor but not the sustained rate gets 408
	// once the sustain time is over, and frees its slot
	store := &mockStorage{data: make(map[string]string)}
	h := NewDocumentHandler(6, 1<<20, store, &sequenceKeyGenerator{})
	h.UploadTimeout = 10 * time.Second
	h.MinUploadBytes = 10
	h.UploadWindow = 100 * time.Millisecond
	h.SustainAfter = 300 * time.Millisecond
	h.SustainRate = 50000
	h.LimitUploads(1)
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	// 100 bytes every 20 ms is 5000 bytes per second
	start := time.Now()
	resp, err := dripPost(t, ts.URL+"/documents", 1000, 100, 20*time.Millisecond)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
	require.Less(t, time.Since(start), 3*time.Second)
	require.Empty(t, store.data)
	require.Eventually(t, func() bool { return h.UploadsInProgress() == 0 }, 2*time.Second, 10*time.Millisecond)

	// A fast upload of unknown size passes
	resp, err = dripPost(t, ts.URL+"/documents", 50, 10000, 5*time.Millisecond)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAnnouncedTooLarge(t *testing.T) {
	// A body announced over max_length is refused before a byte of it is read
	for _, path := range []string{"/documents", "/log"} {
		for _, multipart := range []bool{false, true} {
			store := &mockStorage{data: make(map[string]string)}
			h := NewDocumentHandler(6, 1024, store, &sequenceKeyGenerator{})
			router := chi.NewRouter()
			h.RegisterRoutes(router)

			req := httptest.NewRequest(http.MethodPost, path, io.NopCloser(errReader{}))
			req.ContentLength = 1 << 30
			if multipart {
				req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, path)
			require.Equal(t, "{\"message\": \"Document exceeds maximum length.\"}\n", rec.Body.String())
			require.Empty(t, store.data)
			require.Zero(t, h.UploadsInProgress())
		}
	}

	// A multipart form may be max_length plus its framing allowance
	store := &mockStorage{data: make(map[string]string)}
	h := NewDocumentHandler(6, 1024, store, &sequenceKeyGenerator{})
	router := chi.NewRouter()
	h.RegisterRoutes(router)
	body, contentType := multipartBody(t, "hello")
	req := httptest.NewRequest(http.MethodPost, "/documents", body)
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = 1024 + multipartOverhead
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// rawMultipart builds a multipart body with the given parts, each with its
// header lines as written
func rawMultipart(parts ...string) (*bytes.Buffer, string) {
	var b bytes.Buffer
	for _, part := range parts {
		b.WriteString("--BOUNDARY\r\n" + part + "\r\n")
	}
	b.WriteString("--BOUNDARY--\r\n")
	return &b, "multipart/form-data; boundary=BOUNDARY"
}

func TestMultipartHeadersTooLarge(t *testing.T) {
	post := func(body *bytes.Buffer, contentType string) (*httptest.ResponseRecorder, *mockStorage) {
		store := &mockStorage{data: make(map[string]string)}
		h := NewDocumentHandler(6, 4<<20, store, &mockKeyGenerator{fixedKey: "key123"})
		router := chi.NewRouter()
		h.RegisterRoutes(router)
		req := httptest.NewRequest(http.MethodPost, "/documents", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec, store
	}
	const field = `Content-Disposition: form-data; name="data"`

	// One part with a 100 KiB header
	rec, store := post(rawMultipart(field + "\r\nX-Pad: " + strings.Repeat("a", 100<<10) + "\r\n\r\nhello"))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "headers too large")
	require.Empty(t, store.data)

	// Many parts whose headers are small one by one but large together
	var parts []string
	for i := range 20 {
		parts = append(parts, fmt.Sprintf(`Content-Disposition: form-data; name="f%d"`, i)+"\r\nX-Pad: "+strings.Repeat("b", 8<<10)+"\r\n\r\nx")
	}
	parts = append(parts, field+"\r\n\r\nhello")
	rec, store = post(rawMultipart(parts...))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, store.data)

	// A normal form with a few fields and a large value is fine
	rec, store = post(rawMultipart(
		`Content-Disposition: form-data; name="title"`+"\r\n\r\nbuild",
		`Content-Disposition: form-data; name="data"; filename="build.log"`+"\r\nContent-Type: text/plain\r\n\r\n"+strings.Repeat("log\n", 64<<10),
	))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, strings.Repeat("log\n", 64<<10), store.data["key123"])
}

// blockingWriter is a response writer whose writes wait until release is
// closed, like a client that reads slowly
type blockingWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    bool
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	if !w.once {
		w.once = true
		close(w.started)
	}
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestStreamedResponseCharge(t *testing.T) {
	// A /raw or /documents answer from file storage holds ResponseMemory until it is sent
	handler, router := fileHandler(t, 4<<20)
	handler.ReadMemory = NewMemoryBudget(16 << 20)
	key, err := handler.CreateDocument(strings.Repeat("a", 2<<20))
	require.NoError(t, err)

	for _, path := range []string{"/raw/", "/documents/"} {
		w := &blockingWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
		req := httptest.NewRequest(http.MethodGet, path+key, nil)
		req.RemoteAddr = "192.0.2.1:1"
		done := make(chan struct{})
		go func() {
			router.ServeHTTP(w, req)
			close(done)
		}()
		<-w.started
		require.EqualValues(t, ResponseMemory, handler.ReadMemory.InUse(), path)
		close(w.release)
		<-done
		require.Equal(t, http.StatusOK, w.Code, path)
		require.Zero(t, handler.ReadMemory.InUse(), path)
	}

	// One client fills its share of 4 MiB with 16 slow answers; the next is refused with 503
	var releases []func()
	for range 4 << 20 / ResponseMemory {
		release, ok := handler.chargeRead("192.0.2.1", 2<<20, ResponseMemory)
		require.True(t, ok)
		releases = append(releases, release)
	}
	for _, path := range []string{"/raw/", "/documents/"} {
		req := httptest.NewRequest(http.MethodGet, path+key, nil)
		req.RemoteAddr = "192.0.2.1:1"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
		require.Equal(t, "10", rec.Header().Get("Retry-After"))
	}

	// HEAD and small pastes are not charged; other clients still read
	req := httptest.NewRequest(http.MethodHead, "/raw/"+key, nil)
	req.RemoteAddr = "192.0.2.1:1"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	small, err := handler.CreateDocument("small")
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, "/raw/"+small, nil)
	req.RemoteAddr = "192.0.2.1:1"
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	req = httptest.NewRequest(http.MethodGet, "/raw/"+key, nil)
	req.RemoteAddr = "192.0.2.2:1"
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	for _, release := range releases {
		release()
	}
	require.Zero(t, handler.ReadMemory.InUse())
}

func TestChargeUpload(t *testing.T) {
	h := NewDocumentHandler(6, 1024, &mockStorage{data: map[string]string{}}, &sequenceKeyGenerator{})
	h.UploadMemory = NewMemoryBudget(64 << 20)

	// A small body is charged as it arrives, in steps of ChargeUnit, and takes no slot
	u := h.ChargeUpload("192.0.2.1")
	body := u.Body(io.NopCloser(strings.NewReader(strings.Repeat("a", 1000))), MCPBodyCost)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Len(t, data, 1000)
	require.EqualValues(t, ChargeUnit, h.UploadMemory.InUse())
	require.Zero(t, h.UploadsInProgress())
	u.Release()
	u.Release()
	require.Zero(t, h.UploadMemory.InUse())
	require.Zero(t, h.UploadsInProgress())

	// A near 1 MiB body is charged 6 times over
	u = h.ChargeUpload("192.0.2.1")
	body = u.Body(io.NopCloser(strings.NewReader(strings.Repeat("a", LargeUpload))), MCPBodyCost)
	_, err = io.ReadAll(body)
	require.NoError(t, err)
	require.EqualValues(t, MCPBodyCost*LargeUpload, h.UploadMemory.InUse())
	u.Release()

	// When the client's share is used up, the read stops with ErrMemoryBusy
	hold, ok := h.UploadMemory.Acquire("192.0.2.1", 16<<20-2*ChargeUnit)
	require.True(t, ok)
	u = h.ChargeUpload("192.0.2.1")
	body = u.Body(io.NopCloser(strings.NewReader(strings.Repeat("a", 100<<10))), MCPBodyCost)
	_, err = io.ReadAll(body)
	require.ErrorIs(t, err, ErrMemoryBusy)
	require.True(t, u.OutOfMemory())
	u.Release()
	hold()
	require.Zero(t, h.UploadMemory.InUse())
}
