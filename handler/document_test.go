package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

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
