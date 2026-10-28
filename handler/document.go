package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

var (
	pasteCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hastebin_paste_created",
		Help: "The total number of pastes created",
	})

	pasteRead = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hastebin_paste_read",
		Help: "The total number of pastes read",
	})
)

const (
	// maxKeyAttempts is how many keys are tried before giving up on a collision
	maxKeyAttempts = 5

	// multipartOverhead is the extra body size allowed for multipart framing
	multipartOverhead = 1 << 20

	// maxMultipartMemory caps the memory used to parse a multipart body
	maxMultipartMemory = 32 << 20
)

// DocumentHandler manages document operations
type DocumentHandler struct {
	KeyLength    int
	MaxLength    int
	Store        storage.Storage
	KeyGenerator keygenerator.KeyGenerator

	// BaseURL is the public URL used in /log links. Empty means https://<Host>.
	BaseURL string
}

func NewDocumentHandler(keyLength, maxLength int, store storage.Storage, keyGenerator keygenerator.KeyGenerator) *DocumentHandler {
	return &DocumentHandler{
		KeyLength:    keyLength,
		MaxLength:    maxLength,
		Store:        store,
		KeyGenerator: keyGenerator,
	}
}

// RegisterRoutes registers document routes
func (h *DocumentHandler) RegisterRoutes(r chi.Router) {
	r.Get("/raw/{id}", h.HandleRawGet)
	r.Head("/raw/{id}", h.HandleRawGet)

	r.Post("/log", h.HandlePutLog)
	r.Put("/log", h.HandlePutLog)

	r.Post("/documents", h.HandlePost)

	r.Get("/documents/{id}", h.HandleGet)
	r.Head("/documents/{id}", h.HandleGet)
}

// Handle retrieving a document
func (h *DocumentHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	key := strings.Split(chi.URLParam(r, "id"), ".")[0]
	data, err := h.Store.Get(key, false)

	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		log.Error().Err(err).Str("key", key).Msg("Error retrieving document")
		http.Error(w, `{"message": "Error retrieving document."}`, http.StatusInternalServerError)
		return
	}

	if data != "" && err == nil {
		log.Info().Str("key", key).Msg("Retrieved document")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		pasteRead.Inc()
		json.NewEncoder(w).Encode(map[string]string{"data": data, "key": key})
	} else {
		log.Info().Str("key", key).Msg("Document not found")
		http.Error(w, `{"message": "Document not found."}`, http.StatusNotFound)
	}
}

// Handle retrieving raw document
func (h *DocumentHandler) HandleRawGet(w http.ResponseWriter, r *http.Request) {
	key := strings.Split(chi.URLParam(r, "id"), ".")[0]
	data, err := h.Store.Get(key, false)

	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		log.Error().Err(err).Str("key", key).Msg("Error retrieving raw document")
		http.Error(w, `{"message": "Error retrieving document."}`, http.StatusInternalServerError)
		return
	}

	if data != "" && err == nil {
		log.Info().Str("key", key).Msg("Retrieved raw document")
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		pasteRead.Inc()
		w.Write([]byte(data))
	} else {
		log.Info().Str("key", key).Msg("Raw document not found")
		http.Error(w, `{"message": "Document not found."}`, http.StatusNotFound)
	}
}

// Handle adding a new document (POST)
func (h *DocumentHandler) HandlePost(w http.ResponseWriter, r *http.Request) {
	key, ok := h.saveDocument(w, r)
	if !ok {
		return
	}

	log.Info().Str("key", key).Msg("Added document")

	pasteCreated.Inc()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"key": key})
}

// Handle PUT request that returns a direct link
func (h *DocumentHandler) HandlePutLog(w http.ResponseWriter, r *http.Request) {
	key, ok := h.saveDocument(w, r)
	if !ok {
		return
	}

	baseURL := h.BaseURL
	if baseURL == "" {
		baseURL = "https://" + r.Host
	}

	log.Info().Str("key", key).Msg("Added document with log link")
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "\n%s/%s\n\n", baseURL, key)
}

// saveDocument reads the request body and stores it under a new free key.
// On failure it writes the error response and returns false.
func (h *DocumentHandler) saveDocument(w http.ResponseWriter, r *http.Request) (string, bool) {
	var buffer strings.Builder
	if err := h.readBody(w, r, &buffer); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) || errors.Is(err, multipart.ErrMessageTooLarge) {
			log.Info().Msg("Document exceeds max length")
			http.Error(w, `{"message": "Document exceeds maximum length."}`, http.StatusRequestEntityTooLarge)
			return "", false
		}

		log.Warn().Err(err).Msg("Error reading request body")
		http.Error(w, `{"message": "Error reading request body."}`, http.StatusBadRequest)
		return "", false
	}

	if h.MaxLength > 0 && buffer.Len() > h.MaxLength {
		log.Info().Msg("Document exceeds max length")
		http.Error(w, `{"message": "Document exceeds maximum length."}`, http.StatusRequestEntityTooLarge)
		return "", false
	}

	key, err := h.freeKey()
	if err != nil {
		log.Error().Err(err).Msg("Error generating document key")
		http.Error(w, `{"message": "Error saving document."}`, http.StatusInternalServerError)
		return "", false
	}

	if err := h.Store.Set(key, buffer.String(), false); err != nil {
		log.Error().Err(err).Str("key", key).Msg("Error saving document")
		http.Error(w, `{"message": "Error saving document."}`, http.StatusInternalServerError)
		return "", false
	}

	return key, true
}

// freeKey generates a key that is not used by an existing document.
// Only a storage miss counts as free; any other storage error is returned.
func (h *DocumentHandler) freeKey() (string, error) {
	for i := 0; i < maxKeyAttempts; i++ {
		key := h.KeyGenerator.Generate(h.KeyLength)
		data, err := h.Store.Get(key, true)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && data == "") {
			return key, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking key %q: %w", key, err)
		}
	}
	return "", fmt.Errorf("no free key after %d attempts", maxKeyAttempts)
}

// Reads body from the request, limited to MaxLength (plus room for multipart framing)
func (h *DocumentHandler) readBody(w http.ResponseWriter, r *http.Request, buffer *strings.Builder) error {
	isMultipart := strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data")

	if h.MaxLength > 0 {
		limit := int64(h.MaxLength)
		if isMultipart {
			limit += multipartOverhead
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	if isMultipart {
		maxMemory := int64(maxMultipartMemory)
		if h.MaxLength > 0 && int64(h.MaxLength) < maxMemory {
			maxMemory = int64(h.MaxLength)
		}

		if err := r.ParseMultipartForm(maxMemory); err != nil {
			return err
		}
		buffer.WriteString(r.FormValue("data"))
	} else {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		buffer.Write(data)
	}
	return nil
}
