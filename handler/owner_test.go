package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// pinStore is a loaded store that records skip_expiration and can delete
type pinStore struct {
	mockStorage
	pinned  map[string]bool
	deleted []string
}

func (s *pinStore) Set(key, value string, skip bool) error {
	s.pinned[key] = skip
	return s.mockStorage.Set(key, value, skip)
}

func (s *pinStore) Delete(key string) error {
	delete(s.data, key)
	s.deleted = append(s.deleted, key)
	return nil
}

func TestOwnedUploadOnLoadedStore(t *testing.T) {
	store := &pinStore{mockStorage: mockStorage{data: map[string]string{}}, pinned: map[string]bool{}}
	h := NewDocumentHandler(6, 10, store, &sequenceKeyGenerator{})
	h.Quota = NewUploadQuota(1000, DefaultQuotaWindow)
	h.UserQuota = NewUploadQuota(30, DefaultQuotaWindow)

	var committed []string
	var fail error
	h.Uploaders = func(w http.ResponseWriter, r *http.Request) (Uploader, bool) {
		if r.Header.Get("X-Owner") == "" {
			return AnonymousUploader(r), true
		}
		if r.Header.Get("X-Owner") == "refuse" {
			w.WriteHeader(http.StatusTeapot)
			return Uploader{}, false
		}
		return Uploader{IP: ClientIP(r), QuotaKey: "u:1", Owner: 1, Pin: true, MaxLength: 20,
			Commit: func(key string, size int64) error {
				if fail != nil {
					return fail
				}
				committed = append(committed, key)
				require.EqualValues(t, len(store.data[key]), size)
				return nil
			}}, true
	}
	upload := func(body string, owner string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/documents", strings.NewReader(body))
		req.RemoteAddr = "192.0.2.1:1"
		if owner != "" {
			req.Header.Set("X-Owner", owner)
		}
		rec := httptest.NewRecorder()
		h.HandlePost(rec, req)
		return rec
	}

	// Owned: pinned, the owner's max length, committed with its size
	rec := upload(strings.Repeat("o", 15), "1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"key1"}, committed)
	require.True(t, store.pinned["key1"])
	// Anonymous: the handler's max length, not pinned
	require.Equal(t, http.StatusRequestEntityTooLarge, upload(strings.Repeat("a", 15), "").Code)
	require.Equal(t, http.StatusOK, upload("anon", "").Code)
	require.False(t, store.pinned["key2"])
	// The refusal of Uploaders is the answer
	require.Equal(t, http.StatusTeapot, upload("x", "refuse").Code)

	// A failed commit deletes the content, gives the quota back and answers as told
	fail = &UploadRefused{Status: http.StatusUnauthorized, Message: "revoked", Header: map[string]string{"WWW-Authenticate": "Bearer"}}
	rec = upload("commit fails", "1")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"))
	require.JSONEq(t, `{"message":"revoked"}`, rec.Body.String())
	require.Equal(t, []string{"key3"}, store.deleted)
	_, exists := store.data["key3"]
	require.False(t, exists)
	fail = errors.New("database gone")
	require.Equal(t, http.StatusInternalServerError, upload("commit fails", "1").Code)
	fail = nil

	// The owner's quota of 30: 15 used, the refunds kept it there, so 10 more
	// fit and then 6 do not
	require.Equal(t, http.StatusOK, upload(strings.Repeat("q", 10), "1").Code)
	require.Equal(t, http.StatusTooManyRequests, upload(strings.Repeat("q", 6), "1").Code)
	// The address still has its own quota
	require.Equal(t, http.StatusOK, upload("anon", "").Code)
}
