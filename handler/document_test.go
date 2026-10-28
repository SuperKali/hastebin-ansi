package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

type mockStorage struct {
	data map[string]string
}

func (m *mockStorage) Get(key string, _ bool) (string, error) {
	val, exists := m.data[key]
	if !exists {
		return "", storage.ErrNotFound
	}
	return val, nil
}

func (m *mockStorage) Size(key string) (int64, error) {
	val, exists := m.data[key]
	if !exists {
		return 0, storage.ErrNotFound
	}
	return int64(len(val)), nil
}

func (m *mockStorage) Set(key, value string, _ bool) error {
	m.data[key] = value
	return nil
}

func (m *mockStorage) Close() error {
	return nil
}

type failingStorage struct {
	mockStorage
}

func (m *failingStorage) Set(_, _ string, _ bool) error {
	return errors.New("disk full")
}

type brokenStorage struct {
	mockStorage
}

func (m *brokenStorage) Get(_ string, _ bool) (string, error) {
	return "", errors.New("connection refused")
}

func (m *brokenStorage) Size(_ string) (int64, error) {
	return 0, errors.New("connection refused")
}

type sequenceKeyGenerator struct {
	n int
}

func (m *sequenceKeyGenerator) Generate(_ int) string {
	m.n++
	return "key" + strconv.Itoa(m.n)
}

type mockKeyGenerator struct {
	fixedKey string
}

func (m *mockKeyGenerator) Generate(_ int) string {
	return m.fixedKey
}

func setupHandler() *DocumentHandler {
	store := &mockStorage{data: make(map[string]string)}
	keyGen := &mockKeyGenerator{fixedKey: "test123"}
	return NewDocumentHandler(6, 1024, store, keyGen)
}

func sendRequest(handler http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHandlePost(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	body := bytes.NewBufferString("test content")
	resp := sendRequest(router, http.MethodPost, "/documents", body)

	require.Equal(t, http.StatusOK, resp.Code)

	var responseData map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&responseData))
	require.Equal(t, "test123", responseData["key"])
}

func TestHandleGet_NotFound(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	resp := sendRequest(router, http.MethodGet, "/documents/unknown", nil)

	require.Equal(t, http.StatusNotFound, resp.Code)
}

func TestHandleGet_Found(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// Add test document
	handler.Store.Set("test123", "stored content", false)

	resp := sendRequest(router, http.MethodGet, "/documents/test123", nil)

	require.Equal(t, http.StatusOK, resp.Code)

	var responseData map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&responseData))
	require.Equal(t, "stored content", responseData["data"])

	// Test HEAD request
	resp = sendRequest(router, http.MethodHead, "/documents/test123", nil)
	require.Equal(t, http.StatusOK, resp.Code)
	require.Empty(t, resp.Body.String())
}

func TestHandleRawGet_Found(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// Add test document
	handler.Store.Set("test123", "raw data", false)

	resp := sendRequest(router, http.MethodGet, "/raw/test123", nil)

	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, "raw data", resp.Body.String())

	// Test HEAD request
	resp = sendRequest(router, http.MethodHead, "/raw/test123", nil)
	require.Equal(t, http.StatusOK, resp.Code)
	require.Empty(t, resp.Body.String())
}

func TestHandleRawGet_NotFound(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	resp := sendRequest(router, http.MethodGet, "/raw/unknown", nil)

	require.Equal(t, http.StatusNotFound, resp.Code)
}

func TestHandlePutLog(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	body := bytes.NewBufferString("log entry")
	resp := sendRequest(router, http.MethodPut, "/log", body)

	require.Equal(t, http.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "https://")
	require.Contains(t, resp.Body.String(), "test123")
}

func TestHandlePutLog_ExceedsMaxLength(t *testing.T) {
	handler := NewDocumentHandler(6, 10, &mockStorage{data: make(map[string]string)}, &mockKeyGenerator{fixedKey: "test123"})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	body := bytes.NewBufferString("this content is too long")
	resp := sendRequest(router, http.MethodPut, "/log", body)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.Code)
	require.Equal(t, "{\"message\": \"Document exceeds maximum length.\"}\n", resp.Body.String())
}

func TestHandlePost_ExceedsMaxLength(t *testing.T) {
	handler := NewDocumentHandler(6, 10, &mockStorage{data: make(map[string]string)}, &mockKeyGenerator{fixedKey: "test123"})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	body := bytes.NewBufferString("this content is too long")
	resp := sendRequest(router, http.MethodPost, "/documents", body)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.Code)
	require.Equal(t, "{\"message\": \"Document exceeds maximum length.\"}\n", resp.Body.String())
}

func multipartBody(t *testing.T, data string) (*bytes.Buffer, string) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("data", data))
	require.NoError(t, writer.Close())
	return &body, writer.FormDataContentType()
}

func TestBodyLimit(t *testing.T) {
	const maxLength = 1024

	tests := []struct {
		name      string
		multipart bool
		size      int
		want      int
	}{
		{name: "raw under limit", size: maxLength, want: http.StatusOK},
		{name: "raw over limit", size: maxLength + 1, want: http.StatusRequestEntityTooLarge},
		{name: "raw far over limit", size: 10 * maxLength, want: http.StatusRequestEntityTooLarge},
		{name: "multipart under limit", multipart: true, size: maxLength, want: http.StatusOK},
		{name: "multipart value over limit", multipart: true, size: maxLength + 1, want: http.StatusRequestEntityTooLarge},
		{name: "multipart body over limit", multipart: true, size: maxLength + multipartOverhead, want: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		for _, path := range []string{"/documents", "/log"} {
			t.Run(tt.name+" "+path, func(t *testing.T) {
				store := &mockStorage{data: make(map[string]string)}
				handler := NewDocumentHandler(6, maxLength, store, &mockKeyGenerator{fixedKey: "test123"})
				router := chi.NewRouter()
				handler.RegisterRoutes(router)

				data := strings.Repeat("a", tt.size)
				var req *http.Request
				if tt.multipart {
					body, contentType := multipartBody(t, data)
					req = httptest.NewRequest(http.MethodPost, path, body)
					req.Header.Set("Content-Type", contentType)
				} else {
					req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(data))
					req.Header.Set("Content-Type", "text/plain")
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)

				require.Equal(t, tt.want, rec.Code)
				if tt.want == http.StatusOK {
					require.Equal(t, data, store.data["test123"])
				} else {
					require.Equal(t, "{\"message\": \"Document exceeds maximum length.\"}\n", rec.Body.String())
					require.Empty(t, store.data)
				}
			})
		}
	}
}

func TestBodyLimit_Unlimited(t *testing.T) {
	store := &mockStorage{data: make(map[string]string)}
	handler := NewDocumentHandler(6, 0, store, &mockKeyGenerator{fixedKey: "test123"})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	data := strings.Repeat("a", 2<<20)
	resp := sendRequest(router, http.MethodPost, "/documents", strings.NewReader(data))

	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, data, store.data["test123"])
}

func TestBadMultipart(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodPost, "/documents", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "multipart/form-data")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSetFailure(t *testing.T) {
	for _, path := range []string{"/documents", "/log"} {
		t.Run(path, func(t *testing.T) {
			store := &failingStorage{mockStorage{data: make(map[string]string)}}
			handler := NewDocumentHandler(6, 1024, store, &mockKeyGenerator{fixedKey: "test123"})
			router := chi.NewRouter()
			handler.RegisterRoutes(router)

			resp := sendRequest(router, http.MethodPost, path, bytes.NewBufferString("content"))

			require.Equal(t, http.StatusInternalServerError, resp.Code)
			require.Equal(t, "{\"message\": \"Error saving document.\"}\n", resp.Body.String())
			require.NotContains(t, resp.Body.String(), "test123")
		})
	}
}

func TestKeyCollision(t *testing.T) {
	// Every generated key is taken: give up with 500 and keep the stored document
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	require.NoError(t, handler.Store.Set("test123", "existing", false))

	resp := sendRequest(router, http.MethodPost, "/documents", bytes.NewBufferString("new content"))

	require.Equal(t, http.StatusInternalServerError, resp.Code)
	require.Equal(t, "existing", handler.Store.(*mockStorage).data["test123"])

	// The first key is taken: the next free one is used
	store := &mockStorage{data: map[string]string{"key1": "existing"}}
	handler = NewDocumentHandler(6, 1024, store, &sequenceKeyGenerator{})
	router = chi.NewRouter()
	handler.RegisterRoutes(router)

	resp = sendRequest(router, http.MethodPost, "/documents", bytes.NewBufferString("new content"))

	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, "existing", store.data["key1"])
	require.Equal(t, "new content", store.data["key2"])
}

func TestStorageGetError(t *testing.T) {
	store := &brokenStorage{mockStorage{data: make(map[string]string)}}
	handler := NewDocumentHandler(6, 1024, store, &mockKeyGenerator{fixedKey: "test123"})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// Reads fail with 500, not 404
	for _, path := range []string{"/documents/test123", "/raw/test123"} {
		resp := sendRequest(router, http.MethodGet, path, nil)
		require.Equal(t, http.StatusInternalServerError, resp.Code, path)
	}

	// A key cannot be checked, so nothing is saved
	for _, path := range []string{"/documents", "/log"} {
		resp := sendRequest(router, http.MethodPost, path, bytes.NewBufferString("content"))
		require.Equal(t, http.StatusInternalServerError, resp.Code, path)
		require.Equal(t, "{\"message\": \"Error saving document.\"}\n", resp.Body.String())
		require.Empty(t, store.data)
	}
}

func TestHandlePutLog_BaseURL(t *testing.T) {
	handler := setupHandler()
	handler.BaseURL = "https://paste.example.com"
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodPut, "/log", bytes.NewBufferString("log entry"))
	req.Host = "evil.example.net"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "\nhttps://paste.example.com/test123\n\n", rec.Body.String())
}

func TestUploadLimit(t *testing.T) {
	store := &mockStorage{data: make(map[string]string)}
	handler := NewDocumentHandler(6, 4*LargeUpload, store, &sequenceKeyGenerator{})
	handler.LimitUploads(1)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	large := strings.Repeat("a", LargeUpload+1)

	// Hold the only slot, as a slow large upload would
	release, ok := handler.AcquireUpload("", -1)
	require.True(t, ok)
	inUse, total := handler.UploadSlots()
	require.Equal(t, 1, inUse)
	require.Equal(t, 1, total)

	for _, path := range []string{"/documents", "/log"} {
		resp := sendRequest(router, http.MethodPost, path, strings.NewReader(large))
		require.Equal(t, http.StatusServiceUnavailable, resp.Code, path)
		require.Equal(t, "{\"message\": \"Server busy with other large uploads, try again shortly.\"}\n", resp.Body.String())
		require.NotEmpty(t, resp.Header().Get("Retry-After"))
	}

	// A body of unknown size counts as large
	req := httptest.NewRequest(http.MethodPost, "/log", strings.NewReader("chunked"))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Empty(t, store.data)

	// Small uploads never wait for a slot
	resp := sendRequest(router, http.MethodPost, "/documents", strings.NewReader("small"))
	require.Equal(t, http.StatusOK, resp.Code)

	// Once the slot is free, a large upload goes through and frees it again
	release()
	for i := 0; i < 2; i++ {
		resp = sendRequest(router, http.MethodPost, "/documents", strings.NewReader(large))
		require.Equal(t, http.StatusOK, resp.Code)
	}
	require.Len(t, store.data, 3)
}

func TestAcquireUpload_NoLimit(t *testing.T) {
	handler := setupHandler()
	for _, n := range []int{0, -1} {
		handler.LimitUploads(n)
		handler.LimitUploadsPerIP(n)
		for i := 0; i < 10; i++ {
			_, ok := handler.AcquireUpload("192.0.2.1", -1)
			require.True(t, ok)
		}
	}
}

func TestUploadLimitPerIP(t *testing.T) {
	store := &mockStorage{data: make(map[string]string)}
	handler := NewDocumentHandler(6, 4*LargeUpload, store, &sequenceKeyGenerator{})
	handler.LimitUploads(4)
	handler.LimitUploadsPerIP(1)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	large := strings.Repeat("a", LargeUpload+1)
	post := func(remoteAddr, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/log", strings.NewReader(body))
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	// 198.51.100.7 already has a large upload in progress
	release, ok := handler.AcquireUpload("198.51.100.7", -1)
	require.True(t, ok)

	require.Equal(t, http.StatusServiceUnavailable, post("198.51.100.7:4000", large))
	require.Equal(t, http.StatusOK, post("198.51.100.7:4001", "small"))
	require.Equal(t, http.StatusOK, post("203.0.113.9:4000", large))

	// Releasing twice must not free a slot that is not held
	release()
	release()
	require.Equal(t, 0, handler.UploadsInProgress())
	require.Equal(t, http.StatusOK, post("198.51.100.7:4000", large))

	// The total limit still applies across IPs
	handler.LimitUploads(1)
	hold, ok := handler.AcquireUpload("203.0.113.1", -1)
	require.True(t, ok)
	require.Equal(t, http.StatusServiceUnavailable, post("203.0.113.2:1", large))
	hold()

	require.Equal(t, "198.51.100.7", ClientIP(&http.Request{RemoteAddr: "198.51.100.7:4000"}))
	require.Equal(t, "2001:db8::1", ClientIP(&http.Request{RemoteAddr: "[2001:db8::1]:4000"}))
	require.Equal(t, "pipe", ClientIP(&http.Request{RemoteAddr: "pipe"}))
}

// readAll returns the whole of a stored paste
func readAll(t *testing.T, h *DocumentHandler, key string) string {
	t.Helper()
	doc, err := h.openDocument("", key, 0, RawReadCost)
	require.NoError(t, err)
	defer doc.Close()
	data, err := io.ReadAll(doc)
	require.NoError(t, err)
	return string(data)
}

func TestCreateAndReadDocument(t *testing.T) {
	handler := setupHandler()

	key, err := handler.CreateDocument("hello")
	require.NoError(t, err)
	require.Equal(t, "test123", key)

	require.Equal(t, "hello", readAll(t, handler, key))

	_, err = handler.CreateDocument(strings.Repeat("a", 1025))
	require.ErrorIs(t, err, ErrTooLarge)

	_, err = handler.CreateDocument("")
	require.ErrorIs(t, err, ErrEmpty)

	_, err = handler.openDocument("", "missing", 0, RawReadCost)
	require.ErrorIs(t, err, storage.ErrNotFound)

	require.NoError(t, handler.Store.Set("empty", "", false))
	_, err = handler.openDocument("", "empty", 0, RawReadCost)
	require.ErrorIs(t, err, storage.ErrNotFound)

	require.Equal(t, "https://paste.local", handler.PublicURL("paste.local"))
	handler.BaseURL = "https://paste.example.com"
	require.Equal(t, "https://paste.example.com", handler.PublicURL("paste.local"))
}

func BenchmarkHandlePost(b *testing.B) {
	handler := setupHandler()
	handler.KeyGenerator = &sequenceKeyGenerator{}
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// Disable logging for benchmark
	log.Logger = log.Level(zerolog.Disabled)

	body := bytes.NewBufferString("benchmark content")
	for i := 0; i < b.N; i++ {
		sendRequest(router, http.MethodPost, "/documents", body)
	}
}

func BenchmarkHandleGet(b *testing.B) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// Disable logging for benchmark
	log.Logger = log.Level(zerolog.Disabled)

	// Add document
	handler.Store.Set("test123", "benchmark data", false)

	for i := 0; i < b.N; i++ {
		sendRequest(router, http.MethodGet, "/documents/test123", nil)
	}
}

func BenchmarkHandlePutLog(b *testing.B) {
	handler := setupHandler()
	handler.KeyGenerator = &sequenceKeyGenerator{}
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// Disable logging for benchmark
	log.Logger = log.Level(zerolog.Disabled)

	body := bytes.NewBufferString("benchmark log entry")
	for i := 0; i < b.N; i++ {
		sendRequest(router, http.MethodPut, "/log", body)
	}
}

func BenchmarkHandleRawGet(b *testing.B) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// Disable logging for benchmark
	log.Logger = log.Level(zerolog.Disabled)

	// Add document
	handler.Store.Set("test123", "benchmark data", false)

	for i := 0; i < b.N; i++ {
		sendRequest(router, http.MethodGet, "/raw/test123", nil)
	}
}

func TestEmptyUpload(t *testing.T) {
	handler := setupHandler()
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	for _, path := range []string{"/documents", "/log"} {
		resp := sendRequest(router, http.MethodPost, path, strings.NewReader(""))
		require.Equal(t, http.StatusBadRequest, resp.Code, path)
		require.Equal(t, "{\"message\": \"Document is empty.\"}\n", resp.Body.String())
	}

	body, contentType := multipartBody(t, "")
	req := httptest.NewRequest(http.MethodPost, "/documents", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, handler.Store.(*mockStorage).data)
}

// fileHandler is a handler on file storage, which streams raw reads
func fileHandler(t *testing.T, maxLength int) (*DocumentHandler, chi.Router) {
	t.Helper()
	store := storage.NewFileStorage(t.TempDir(), 0)
	t.Cleanup(func() { store.Close() })
	handler := NewDocumentHandler(6, maxLength, store, &sequenceKeyGenerator{})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	return handler, router
}

type previewResponse struct {
	Data      string `json:"data"`
	Key       string `json:"key"`
	Truncated bool   `json:"truncated"`
	Size      int64  `json:"size"`
}

func TestDocumentPreview(t *testing.T) {
	// A large paste comes back cut to PreviewSize on a character boundary, from
	// a backend that streams and from one that loads the whole paste
	large := strings.Repeat("a", PreviewSize-1) + "é" + strings.Repeat("b", 1000)
	stores := map[string]func() (*DocumentHandler, chi.Router){
		"file": func() (*DocumentHandler, chi.Router) { return fileHandler(t, 4<<20) },
		"loaded": func() (*DocumentHandler, chi.Router) {
			handler := NewDocumentHandler(6, 4<<20, &mockStorage{data: make(map[string]string)}, &sequenceKeyGenerator{})
			router := chi.NewRouter()
			handler.RegisterRoutes(router)
			return handler, router
		},
	}
	for name, setup := range stores {
		t.Run(name, func(t *testing.T) {
			handler, router := setup()
			key, err := handler.CreateDocument(large)
			require.NoError(t, err)
			small, err := handler.CreateDocument("small paste")
			require.NoError(t, err)

			resp := sendRequest(router, http.MethodGet, "/documents/"+key, nil)
			require.Equal(t, http.StatusOK, resp.Code)
			var doc previewResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
			require.True(t, doc.Truncated)
			require.EqualValues(t, len(large), doc.Size)
			require.Equal(t, strings.Repeat("a", PreviewSize-1), doc.Data)

			// A small paste is whole, without the extra fields
			resp = sendRequest(router, http.MethodGet, "/documents/"+small, nil)
			require.Equal(t, http.StatusOK, resp.Code)
			var plain map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&plain))
			require.Equal(t, map[string]any{"data": "small paste", "key": small}, plain)

			// /raw has all of it
			resp = sendRequest(router, http.MethodGet, "/raw/"+key, nil)
			require.Equal(t, http.StatusOK, resp.Code)
			require.Equal(t, large, resp.Body.String())
			require.Equal(t, "text/plain; charset=UTF-8", resp.Header().Get("Content-Type"))

			// HEAD answers without a body
			for _, path := range []string{"/raw/" + key, "/documents/" + key} {
				resp = sendRequest(router, http.MethodHead, path, nil)
				require.Equal(t, http.StatusOK, resp.Code, path)
				require.Empty(t, resp.Body.String(), path)
			}
			resp = sendRequest(router, http.MethodHead, "/raw/missing", nil)
			require.Equal(t, http.StatusNotFound, resp.Code)

			// Nothing stays charged
			require.Zero(t, handler.ReadMemory.InUse())
		})
	}
}

func TestRawRange(t *testing.T) {
	handler, router := fileHandler(t, 1024)
	key, err := handler.CreateDocument("0123456789")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/raw/"+key+".log", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusPartialContent, rec.Code)
	require.Equal(t, "2345", rec.Body.String())

	// An empty paste file counts as missing
	require.NoError(t, handler.Store.Set("empty", "", false))
	resp := sendRequest(router, http.MethodGet, "/raw/empty", nil)
	require.Equal(t, http.StatusNotFound, resp.Code)
	resp = sendRequest(router, http.MethodGet, "/documents/empty", nil)
	require.Equal(t, http.StatusNotFound, resp.Code)
}

func TestReadMemoryBudget(t *testing.T) {
	// A store that loads whole pastes: reads are charged before they load
	big := strings.Repeat("x", 100<<10)
	store := &mockStorage{data: map[string]string{"big": big, "small": "y"}}
	handler := NewDocumentHandler(6, 4<<20, store, &sequenceKeyGenerator{})
	handler.ReadMemory = NewMemoryBudget(4 << 20)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	get := func(path, remoteAddr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Reads of twice 100 KiB from four clients, plus a fifth one, leave less than that free
	var held []*Document
	for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		for range 4 {
			doc, err := handler.openDocument(ip, "big", 0, RawReadCost)
			require.NoError(t, err)
			held = append(held, doc)
		}
	}
	hold, ok := handler.ReadMemory.Acquire("192.0.2.5", 4<<20-16*200<<10-100<<10)
	require.True(t, ok)

	resp := get("/raw/big", "192.0.2.9:1")
	require.Equal(t, http.StatusServiceUnavailable, resp.Code)
	require.Equal(t, "10", resp.Header().Get("Retry-After"))
	require.Equal(t, "{\"message\": \"Server busy, try again shortly.\"}\n", resp.Body.String())

	// /documents needs the paste and room to encode it
	resp = get("/documents/big", "192.0.2.9:1")
	require.Equal(t, http.StatusServiceUnavailable, resp.Code)

	// Reads under 64 KiB are not charged at all
	require.Equal(t, http.StatusOK, get("/raw/small", "192.0.2.9:1").Code)
	require.Equal(t, http.StatusOK, get("/documents/small", "192.0.2.9:1").Code)

	// A busy server is not a miss: no ban, no 404
	require.Zero(t, handler.NotFound.Tracked())

	for _, doc := range held {
		doc.Close()
		doc.Close()
	}
	hold()
	require.Zero(t, handler.ReadMemory.InUse())
	resp = get("/raw/big", "192.0.2.9:1")
	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, big, resp.Body.String())
	require.Zero(t, handler.ReadMemory.InUse())

	// No single read may be larger than one client's share, even alone
	store.data["huge"] = strings.Repeat("z", 600<<10)
	require.Equal(t, http.StatusServiceUnavailable, get("/raw/huge", "192.0.2.9:1").Code)
	require.Equal(t, http.StatusServiceUnavailable, get("/documents/huge", "192.0.2.9:1").Code)
	require.Zero(t, handler.ReadMemory.InUse())

	// Full uploads leave reads alone
	handler.UploadMemory = NewMemoryBudget(1 << 20)
	full, ok := handler.UploadMemory.Acquire("", 256<<10)
	require.True(t, ok)
	require.Equal(t, http.StatusOK, get("/raw/big", "192.0.2.9:1").Code)
	full()
}

func TestReadMemoryClientShare(t *testing.T) {
	// One client may hold a quarter of the read budget: 1 MiB of 4 MiB. A /raw
	// read of 100 KiB is charged twice that plus ResponseMemory, 456 KiB.
	store := &mockStorage{data: map[string]string{"big": strings.Repeat("x", 100<<10)}}
	handler := NewDocumentHandler(6, 4<<20, store, &sequenceKeyGenerator{})
	handler.ReadMemory = NewMemoryBudget(4 << 20)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	get := func(remoteAddr string) int {
		req := httptest.NewRequest(http.MethodGet, "/raw/big", nil)
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	// 2001:db8::1 holds a 900 KiB read, as a slow reader would
	doc, err := handler.OpenDocumentFrom("2001:db8::1", "big", 700<<10, RawReadCost)
	require.NoError(t, err)

	// Its /64 cannot take another, other clients can
	require.Equal(t, http.StatusServiceUnavailable, get("[2001:db8::2]:1"))
	require.Equal(t, http.StatusOK, get("[2001:db8:0:1::1]:1"))
	require.Equal(t, http.StatusOK, get("192.0.2.9:1"))

	doc.Close()
	require.Equal(t, http.StatusOK, get("[2001:db8::2]:1"))
	require.Zero(t, handler.ReadMemory.InUse())
}

func TestStreamedReadCharge(t *testing.T) {
	// With file storage a read is charged a small fixed amount, whatever the paste size
	handler, router := fileHandler(t, 40<<20)
	handler.ReadMemory = NewMemoryBudget(64 << 20)
	key, err := handler.CreateDocument(strings.Repeat("a", 32<<20))
	require.NoError(t, err)

	doc, err := handler.openDocument("192.0.2.1", key, previewMemory, RawReadCost)
	require.NoError(t, err)
	require.EqualValues(t, 32<<20, doc.Size)
	require.EqualValues(t, previewMemory, handler.ReadMemory.InUse())
	doc.Close()
	require.Zero(t, handler.ReadMemory.InUse())

	// Slow readers of one client fit in its share: 16 MiB / 256 KiB
	var held []*Document
	for range 16 << 20 / previewMemory {
		doc, err := handler.openDocument("192.0.2.1", key, previewMemory, RawReadCost)
		require.NoError(t, err)
		held = append(held, doc)
	}
	_, err = handler.openDocument("192.0.2.1", key, previewMemory, RawReadCost)
	require.ErrorIs(t, err, ErrMemoryBusy)
	// Other clients still read
	req := httptest.NewRequest(http.MethodGet, "/documents/"+key, nil)
	req.RemoteAddr = "192.0.2.2:1"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var doc2 previewResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&doc2))
	require.True(t, doc2.Truncated)
	require.Len(t, doc2.Data, PreviewSize)
	for _, doc := range held {
		doc.Close()
	}
	require.Zero(t, handler.ReadMemory.InUse())
}

func TestUploadMemoryBudget(t *testing.T) {
	store := &mockStorage{data: make(map[string]string)}
	handler := NewDocumentHandler(6, 4*LargeUpload, store, &sequenceKeyGenerator{})
	handler.UploadMemory = NewMemoryBudget(40 * LargeUpload)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// A body of known size is not charged up front, whatever it announces
	upload, ok := handler.StartUpload("192.0.2.1", 3*LargeUpload)
	require.True(t, ok)
	require.Zero(t, handler.UploadMemory.InUse())
	require.Equal(t, 1, handler.UploadsInProgress())
	upload.Release()
	upload.Release()
	require.Zero(t, handler.UploadsInProgress())

	// It is charged as it arrives: twice what came, and all is given back
	resp := sendRequest(router, http.MethodPost, "/documents", strings.NewReader(strings.Repeat("a", 3*LargeUpload)))
	require.Equal(t, http.StatusOK, resp.Code)
	require.Zero(t, handler.UploadMemory.InUse())
	require.Zero(t, handler.UploadsInProgress())

	// Content already in memory is charged at once
	release, ok := handler.AcquireUpload("192.0.2.2", 2*LargeUpload)
	require.True(t, ok)
	require.EqualValues(t, 4*LargeUpload, handler.UploadMemory.InUse())
	release()

	// No upload may hold more than one client's share: 10 MiB here
	_, ok = handler.AcquireUpload("192.0.2.2", 6*LargeUpload)
	require.False(t, ok)
	require.Zero(t, handler.UploadMemory.InUse())
	require.Zero(t, handler.UploadsInProgress())

	// Full reads leave uploads alone
	handler.ReadMemory = NewMemoryBudget(1 << 20)
	hold, ok := handler.ReadMemory.Acquire("", 256<<10)
	require.True(t, ok)
	resp = sendRequest(router, http.MethodPost, "/documents", strings.NewReader(strings.Repeat("b", LargeUpload+1)))
	require.Equal(t, http.StatusOK, resp.Code)
	hold()
}

// chunkedPost sends a body of unknown size, written by write, to /documents
// from remoteAddr and returns the response once write is done
func chunkedPost(router http.Handler, remoteAddr string, write func(w io.Writer)) *httptest.ResponseRecorder {
	pr, pw := io.Pipe()
	go func() {
		write(pw)
		pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPost, "/documents", pr)
	req.ContentLength = -1
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	pr.CloseWithError(io.ErrClosedPipe)
	return rec
}

func TestChunkedUploadCharge(t *testing.T) {
	store := &mockStorage{data: make(map[string]string)}
	handler := NewDocumentHandler(6, 8*LargeUpload, store, &sequenceKeyGenerator{})
	handler.UploadMemory = NewMemoryBudget(64 * UploadStep)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// The charge grows with the body, one step at a time
	var during int64
	rec := chunkedPost(router, "192.0.2.1:1", func(w io.Writer) {
		w.Write([]byte(strings.Repeat("a", 3*UploadStep/2)))
		// The last piece may still be on its way through the charge
		for deadline := time.Now().Add(2 * time.Second); handler.UploadMemory.InUse() < 3*UploadStep && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		during = handler.UploadMemory.InUse()
		w.Write([]byte(strings.Repeat("a", UploadStep/2)))
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.EqualValues(t, 3*UploadStep, during, "1.5 MiB arrived: charged twice that")
	require.Zero(t, handler.UploadMemory.InUse())

	// When a step does not fit, the upload stops with 503 and gives everything back
	var holds []func()
	for _, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4"} {
		hold, ok := handler.UploadMemory.Acquire(ip, 15*UploadStep)
		require.True(t, ok)
		holds = append(holds, hold)
	}
	rec = chunkedPost(router, "192.0.2.2:1", func(w io.Writer) {
		w.Write([]byte(strings.Repeat("b", 4*UploadStep)))
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "10", rec.Header().Get("Retry-After"))
	require.EqualValues(t, 60*UploadStep, handler.UploadMemory.InUse())
	require.Zero(t, handler.UploadsInProgress())
	for _, hold := range holds {
		hold()
	}

	// One client cannot hold more than its share (16 MiB) with a second upload
	handler.LimitUploadsPerIP(2)
	first, ok := handler.AcquireUpload("192.0.2.3", 6*LargeUpload)
	require.True(t, ok)
	rec = chunkedPost(router, "192.0.2.3:1", func(w io.Writer) {
		w.Write([]byte(strings.Repeat("c", 3*LargeUpload)))
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	rec = chunkedPost(router, "192.0.2.4:1", func(w io.Writer) {
		w.Write([]byte(strings.Repeat("c", 3*LargeUpload)))
	})
	require.Equal(t, http.StatusOK, rec.Code)
	first()
	require.Zero(t, handler.UploadMemory.InUse())
	require.Len(t, store.data, 2)
}

// spaceStore is a mock store with a free space floor
type spaceStore struct {
	*mockStorage
	free int64
}

func (s *spaceStore) CheckSpace(size int64) error {
	if s.free-size < 100 {
		return fmt.Errorf("%w: test", storage.ErrNoSpace)
	}
	return nil
}

func TestDiskFloor(t *testing.T) {
	store := &spaceStore{mockStorage: &mockStorage{data: make(map[string]string)}, free: 150}
	handler := NewDocumentHandler(6, 1024, store, &sequenceKeyGenerator{})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	// 50 bytes still fit above the floor, 51 do not
	for _, path := range []string{"/documents", "/log"} {
		resp := sendRequest(router, http.MethodPost, path, strings.NewReader(strings.Repeat("a", 50)))
		require.Equal(t, http.StatusOK, resp.Code, path)
		resp = sendRequest(router, http.MethodPost, path, strings.NewReader(strings.Repeat("a", 51)))
		require.Equal(t, http.StatusInsufficientStorage, resp.Code, path)
		require.Equal(t, "{\"message\": \"The server is low on disk space, try again later.\"}\n", resp.Body.String())
	}

	// A body of unknown size is refused once it is read
	rec := chunkedPost(router, "192.0.2.1:1", func(w io.Writer) { w.Write([]byte(strings.Repeat("a", 60))) })
	require.Equal(t, http.StatusInsufficientStorage, rec.Code)

	// A full disk refuses before the body is read
	store.free = 99
	req := httptest.NewRequest(http.MethodPost, "/log", errReader{})
	req.ContentLength = 10
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusInsufficientStorage, rec.Code)

	_, err := handler.CreateDocumentFrom("192.0.2.1", "x")
	require.ErrorIs(t, err, storage.ErrNoSpace)
	require.Len(t, store.data, 2)
}

// errReader fails the test path if a body is read
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("the body must not be read") }

func TestUploadQuotaHTTP(t *testing.T) {
	store := &mockStorage{data: make(map[string]string)}
	handler := NewDocumentHandler(6, 1024, store, &sequenceKeyGenerator{})
	handler.Quota = NewUploadQuota(100, time.Hour)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	post := func(path, remoteAddr string, size int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("a", size)))
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	require.Equal(t, http.StatusOK, post("/documents", "[2001:db8::1]:1", 60).Code)

	// Another address in the same /64 shares the quota
	rec := post("/log", "[2001:db8::2]:1", 60)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "{\"message\": \"Upload quota reached, try again later.\"}\n", rec.Body.String())
	require.Equal(t, "3600", rec.Header().Get("Retry-After"))

	// What is left can still be used, then nothing more
	require.Equal(t, http.StatusOK, post("/log", "[2001:db8::2]:1", 40).Code)
	require.Equal(t, http.StatusTooManyRequests, post("/documents", "[2001:db8::3]:1", 1).Code)

	// Other clients are not affected
	require.Equal(t, http.StatusOK, post("/documents", "[2001:db8:0:1::1]:1", 60).Code)
	require.Equal(t, http.StatusOK, post("/documents", "192.0.2.1:1", 60).Code)
	require.Len(t, store.data, 4)
}

func TestClientKey(t *testing.T) {
	require.Equal(t, "192.0.2.1", ClientKey("192.0.2.1"))
	require.Equal(t, ClientKey("2001:db8::1"), ClientKey("2001:db8::2"))
	require.Equal(t, ClientKey("2001:db8::1"), ClientKey("2001:db8:0:0:ffff:ffff:ffff:ffff"))
	require.NotEqual(t, ClientKey("2001:db8::1"), ClientKey("2001:db8:0:1::1"))
	require.Equal(t, "192.0.2.1", ClientKey("::ffff:192.0.2.1"))
	require.Equal(t, "", ClientKey(""))
}

func TestUploadSlotsIPv6(t *testing.T) {
	// One client holds its only slot from an IPv6 address: the rest of its /64
	// cannot take more, so it cannot starve other clients by rotating addresses
	handler := setupHandler()
	handler.LimitUploads(8)
	handler.LimitUploadsPerIP(1)

	release, ok := handler.AcquireUpload("2001:db8::1", -1)
	require.True(t, ok)
	for i := 2; i < 6; i++ {
		_, ok := handler.AcquireUpload(fmt.Sprintf("2001:db8::%x", i), -1)
		require.False(t, ok)
	}
	other, ok := handler.AcquireUpload("2001:db8:1::1", -1)
	require.True(t, ok)
	other()
	release()
	require.Zero(t, handler.UploadsInProgress())
}

func TestWritePreviewJSON(t *testing.T) {
	// The streamed writer gives what json.Encoder gives, for any text, cut or not
	texts := []string{
		"plain",
		"\x1b[31mred\x1b[0m <b>&amp;</b> \"quoted\" \\ \b\f\t\r\n\x7f é \u2028\u2029 😀",
		strings.Repeat("\x1b", 3*CopyBuffer+7),
		strings.Repeat("é", CopyBuffer),
		strings.Repeat("😀", CopyBuffer/3),
		strings.Repeat("a", CopyBuffer-1) + "\xc3" + strings.Repeat("\xa9", 5) + "\xff",
		"\xe2\x82",
		"ab\xf0\x9f\x98",
	}
	for _, text := range texts {
		for _, limit := range []int64{int64(len(text)), int64(len(text)) - 1, int64(len(text)) / 2, 1} {
			if limit < 1 {
				continue
			}
			doc := &Document{ReadSeeker: strings.NewReader(text), Size: int64(len(text)), release: func() {}}
			var got bytes.Buffer
			require.NoError(t, writePreviewJSON(&got, doc, "k<ey>", limit, ""))

			shown := text
			if int64(len(text)) > limit {
				shown = cutPreview(text, limit)
			}
			want := map[string]any{"data": shown, "key": "k<ey>"}
			if len(shown) < len(text) {
				want["truncated"] = true
				want["size"] = len(text)
			}
			var expected bytes.Buffer
			require.NoError(t, json.NewEncoder(&expected).Encode(want))
			require.Equal(t, expected.String(), got.String(), "%q cut at %d", text, limit)
		}
	}
}

// cutPreview is the cut /documents makes: limit bytes, moved back to the start
// of the character there, at most UTFMax-1 bytes
func cutPreview(s string, limit int64) string {
	for i := limit; i >= max(limit-(utf8.UTFMax-1), 0); i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:limit]
}

func TestJSONStringWriterPieces(t *testing.T) {
	// The text escapes the same however it is split into writes
	text := []byte("a\x1b[0mé😀<\xff\xc3\u2028z\xe2\x82")
	var whole bytes.Buffer
	bw := bufio.NewWriter(&whole)
	j := &jsonStringWriter{w: bw}
	j.Write(text)
	j.Flush()
	bw.Flush()
	for size := 1; size <= len(text); size++ {
		var got bytes.Buffer
		bw := bufio.NewWriter(&got)
		j := &jsonStringWriter{w: bw}
		for rest := text; len(rest) > 0; {
			n := min(size, len(rest))
			j.Write(rest[:n])
			rest = rest[n:]
		}
		j.Flush()
		bw.Flush()
		require.Equal(t, whole.String(), got.String(), "pieces of %d", size)
	}
}

func TestReceiveInto(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(t *testing.T) storage.Storage
	}{
		{"in memory", func(*testing.T) storage.Storage { return &mockStorage{data: make(map[string]string)} }},
		{"streamed to file", func(t *testing.T) storage.Storage {
			store := storage.NewFileStorage(t.TempDir(), 0)
			t.Cleanup(func() { store.Close() })
			return store
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			h := NewDocumentHandler(6, 10, store, &sequenceKeyGenerator{})
			h.Quota = NewUploadQuota(100, time.Hour)

			receive := func(u Uploader, key, body string) (*httptest.ResponseRecorder, string) {
				req := httptest.NewRequest(http.MethodPost, "/documents", strings.NewReader(body))
				req.RemoteAddr = "192.0.2.1:1"
				rec := httptest.NewRecorder()
				got, ok := h.ReceiveInto(rec, req, u, key)
				require.Equal(t, ok, got != "")
				return rec, got
			}

			// An uploader's MaxLength replaces the handler's
			rec, _ := receive(Uploader{IP: "192.0.2.1"}, "", "12345678901")
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
			_, key := receive(Uploader{IP: "192.0.2.1", MaxLength: 20}, "", "12345678901")
			require.Equal(t, "key1", key)
			rec, _ = receive(Uploader{IP: "192.0.2.1", MaxLength: 20}, "", strings.Repeat("a", 21))
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

			// A given key is stored as is, replacing what was there
			_, key = receive(Uploader{IP: "192.0.2.1"}, "key1", "new")
			require.Equal(t, "key1", key)
			got, err := store.Get("key1", true)
			require.NoError(t, err)
			require.Equal(t, "new", got)

			// The quota counts by QuotaKey when it is set, apart from the IP
			_, key = receive(Uploader{IP: "192.0.2.1", QuotaKey: "user:1", MaxLength: 100}, "", strings.Repeat("b", 100))
			require.NotEmpty(t, key)
			rec, _ = receive(Uploader{IP: "192.0.2.1", QuotaKey: "user:1"}, "", "c")
			require.Equal(t, http.StatusTooManyRequests, rec.Code)
			_, key = receive(Uploader{IP: "192.0.2.1"}, "", "c")
			require.NotEmpty(t, key)

			// An empty body is refused and leaves nothing behind
			rec, _ = receive(Uploader{IP: "192.0.2.1"}, "empty", "")
			require.Equal(t, http.StatusBadRequest, rec.Code)
			_, err = store.Size("empty")
			require.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
}

func TestNoStore(t *testing.T) {
	_, router := fileHandler(t, 1024)
	created := sendRequest(router, http.MethodPost, "/documents", strings.NewReader("hello"))
	require.Equal(t, http.StatusOK, created.Code)
	var res map[string]string
	require.NoError(t, json.NewDecoder(created.Body).Decode(&res))
	key := res["key"]

	// Every answer about a paste, found or not, anonymous included
	for _, rec := range []*httptest.ResponseRecorder{
		created,
		sendRequest(router, http.MethodPost, "/log", strings.NewReader("hello")),
		sendRequest(router, http.MethodPut, "/log", strings.NewReader("hello")),
		sendRequest(router, http.MethodGet, "/raw/"+key+".js", nil),
		sendRequest(router, http.MethodHead, "/raw/"+key+".css", nil),
		sendRequest(router, http.MethodGet, "/documents/"+key, nil),
		sendRequest(router, http.MethodHead, "/documents/"+key, nil),
		sendRequest(router, http.MethodGet, "/raw/missing.js", nil),
		sendRequest(router, http.MethodGet, "/documents/missing", nil),
	} {
		require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	}
}
