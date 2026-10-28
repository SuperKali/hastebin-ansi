package handler

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
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

	// maxMultipartHeaders caps the bytes the part headers of one multipart
	// upload may take in total, as each part's headers are held in memory
	maxMultipartHeaders = 64 << 10

	// LargeUpload is the size from which an upload needs a free upload slot.
	// Bodies of unknown size count as large.
	LargeUpload = 1 << 20

	// PreviewSize is how much of a paste /documents returns. A larger paste is
	// cut there and marked truncated; /raw serves the whole of it.
	PreviewSize = 1 << 20

	// busyRetryAfter is the Retry-After, in seconds, when the server is busy
	busyRetryAfter = "10"
)

var (
	// ErrTooLarge is returned when a document is longer than MaxLength
	ErrTooLarge = errors.New("document exceeds maximum length")

	// ErrBusy is returned when every upload slot is in use
	ErrBusy = errors.New("too many uploads in progress")

	// ErrEmpty is returned for an upload without content
	ErrEmpty = errors.New("document is empty")

	// ErrHeadersTooLarge is returned when the part headers of a multipart upload are too large
	ErrHeadersTooLarge = errors.New("multipart part headers too large")
)

// BannedError is returned when the client is banned for looking up too many unknown keys
type BannedError struct {
	// RetryAfter is how long the ban still lasts
	RetryAfter time.Duration
}

func (e *BannedError) Error() string {
	return fmt.Sprintf("too many lookups of unknown keys, banned for %s", e.RetryAfter.Round(time.Second))
}

// DocumentHandler manages document operations
type DocumentHandler struct {
	KeyLength    int
	MaxLength    int
	Store        storage.Storage
	KeyGenerator keygenerator.KeyGenerator

	// BaseURL is the public URL used in /log links. Empty means https://<Host>.
	BaseURL string

	// UploadTimeout replaces the server read and write timeouts for an upload. 0 keeps them.
	UploadTimeout time.Duration

	// MinUploadBytes must arrive in every UploadWindow, or the upload is dropped with 408.
	// 0 turns the check off.
	MinUploadBytes int
	UploadWindow   time.Duration

	// SustainAfter is how long a large upload may run before it must have
	// averaged SustainRate bytes per second since it started, or be dropped
	// with 408. 0 turns the check off.
	SustainAfter time.Duration
	SustainRate  int

	// NotFound bans client IPs that look up too many unknown keys. nil never bans.
	NotFound *NotFoundLimiter

	// ReadMemory and UploadMemory cap the memory held by reads and by large
	// uploads, apart, so neither can starve the other. nil never refuses.
	// SetMemoryBudget sets both.
	ReadMemory   *MemoryBudget
	UploadMemory *MemoryBudget

	// Quota caps the bytes one client may store per window. nil never refuses.
	Quota *UploadQuota

	// slots counts the large uploads in progress, in total and per client IP
	slots     sync.Mutex
	maxSlots  int
	maxPerIP  int
	slotCount int
	slotsByIP map[string]int
}

func NewDocumentHandler(keyLength, maxLength int, store storage.Storage, keyGenerator keygenerator.KeyGenerator) *DocumentHandler {
	return &DocumentHandler{
		KeyLength:    keyLength,
		MaxLength:    maxLength,
		Store:        store,
		KeyGenerator: keyGenerator,

		UploadTimeout:  DefaultUploadTimeout,
		MinUploadBytes: DefaultMinUploadBytes,
		UploadWindow:   DefaultUploadWindow,
		SustainAfter:   DefaultSustainAfter,
		SustainRate:    DefaultSustainRate,
	}
}

// LimitUploads caps how many large uploads are received at the same time.
// n <= 0 means no limit. Call it before the handler serves requests.
func (h *DocumentHandler) LimitUploads(n int) {
	h.slots.Lock()
	defer h.slots.Unlock()
	h.maxSlots = max(n, 0)
}

// LimitUploadsPerIP caps how many large uploads one client IP may send at the same time.
// n <= 0 means no limit. Call it before the handler serves requests.
func (h *DocumentHandler) LimitUploadsPerIP(n int) {
	h.slots.Lock()
	defer h.slots.Unlock()
	h.maxPerIP = max(n, 0)
}

// AcquireUpload takes an upload slot without waiting for content of the given
// size that is already in memory, and charges the upload memory budget
// UploadCost times that size. size < 0 means unknown and charges nothing.
// When ok is true the caller must call release once the document is stored.
func (h *DocumentHandler) AcquireUpload(ip string, size int64) (release func(), ok bool) {
	u, ok := h.StartUpload(ip, size)
	if !ok {
		return nil, false
	}
	if size > 0 && !u.held(size, UploadCost) {
		u.Release()
		return nil, false
	}
	return u.Release, true
}

// StartUpload takes an upload slot for a body of the given size without
// waiting. ip is the client address, "" when unknown; per client limits count
// an IPv6 /64 as one client. Small bodies never wait for a slot; size < 0
// means unknown and counts as large.
//
// Nothing is charged up front, whatever size the client announces. A body
// streamed to file storage is charged its copy buffer (Upload.Hold); one held
// in memory is charged as it arrives (Upload.Body), so a slow client holds
// only what it has sent.
// When ok is true the caller must call Release once the document is stored.
func (h *DocumentHandler) StartUpload(ip string, size int64) (u *Upload, ok bool) {
	if !IsLargeUpload(size) {
		return &Upload{}, true
	}
	key := ClientKey(ip)

	h.slots.Lock()
	defer h.slots.Unlock()
	if h.maxSlots > 0 && h.slotCount >= h.maxSlots {
		return nil, false
	}
	if h.maxPerIP > 0 && key != "" && h.slotsByIP[key] >= h.maxPerIP {
		return nil, false
	}
	h.slotCount++
	if key != "" {
		if h.slotsByIP == nil {
			h.slotsByIP = make(map[string]int)
		}
		h.slotsByIP[key]++
	}
	return &Upload{h: h, key: key, slot: true, charge: h.UploadMemory.NewCharge(ip)}, true
}

// ChargeUpload starts the charge of a body of any size that is held in
// memory, without an upload slot: Upload.Body charges it as it arrives. The
// MCP endpoint wraps every request body in it, as the MCP library reads and
// decodes the whole body before any tool runs. The caller must call Release.
func (h *DocumentHandler) ChargeUpload(ip string) *Upload {
	return &Upload{h: h, charge: h.UploadMemory.NewCharge(ip)}
}

// UploadsInProgress returns how many upload slots are taken
func (h *DocumentHandler) UploadsInProgress() int {
	h.slots.Lock()
	defer h.slots.Unlock()
	return h.slotCount
}

// UploadSlots returns how many upload slots are taken and how many there
// are, 0 for no limit
func (h *DocumentHandler) UploadSlots() (inUse, total int) {
	h.slots.Lock()
	defer h.slots.Unlock()
	return h.slotCount, h.maxSlots
}

// ClientIP returns the client address of a request, as resolved by the real IP middleware
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ClientKey returns the bucket a client address is counted in by every per
// client limit: an IPv4 address as is, an IPv6 address as its /64 network,
// since one host or home usually gets a whole /64 and can pick any address in it.
func ClientKey(ip string) string {
	return httprate.CanonicalizeIP(ip)
}

// IsLargeUpload reports whether a body of the given size needs an upload slot.
// size < 0 means unknown, which counts as large.
func IsLargeUpload(size int64) bool {
	return size < 0 || size > LargeUpload
}

// CreateDocument stores content under a new free key and returns the key.
// It returns ErrEmpty for empty content and ErrTooLarge when content is longer than MaxLength.
func (h *DocumentHandler) CreateDocument(content string) (string, error) {
	return h.CreateDocumentFrom("", content)
}

// CreateDocumentFrom is CreateDocument for the client at ip: the stored bytes
// count against its upload quota, and a client over it gets a *QuotaError.
// When the disk is too full for content it returns storage.ErrNoSpace.
func (h *DocumentHandler) CreateDocumentFrom(ip, content string) (string, error) {
	return h.storeContent(Uploader{IP: ip}, "", content)
}

// Uploader is who sends an upload, and the limits its paste is held to
type Uploader struct {
	// IP is the client address, "" when unknown. Upload slots and the upload
	// memory budget count per client by it.
	IP string

	// QuotaKey is what the stored bytes count against in the upload quota, "" for IP
	QuotaKey string

	// Owner is the account that owns the paste, 0 for none, and Pin keeps the
	// paste past the expiration. Both are reserved for registered users and
	// not used yet.
	Owner int64
	Pin   bool

	// MaxLength is the largest paste accepted, 0 for the handler's MaxLength
	MaxLength int
}

// AnonymousUploader is the Uploader of a request sent without an account
func AnonymousUploader(r *http.Request) Uploader {
	return Uploader{IP: ClientIP(r)}
}

// quotaKey is what the upload quota counts the uploader by
func (u Uploader) quotaKey() string {
	if u.QuotaKey != "" {
		return u.QuotaKey
	}
	return u.IP
}

// maxLength is the largest paste u may store, 0 for no limit
func (h *DocumentHandler) maxLength(u Uploader) int {
	if u.MaxLength > 0 {
		return u.MaxLength
	}
	return h.MaxLength
}

// storeContent stores content for u under key, or under a new free key when
// key is "", and returns the key. It returns ErrEmpty, ErrTooLarge, a
// *QuotaError or storage.ErrNoSpace when content may not be stored.
func (h *DocumentHandler) storeContent(u Uploader, key, content string) (string, error) {
	if content == "" {
		return "", ErrEmpty
	}
	if maxLength := h.maxLength(u); maxLength > 0 && len(content) > maxLength {
		return "", ErrTooLarge
	}

	size := int64(len(content))
	if err := h.CheckSpace(size); err != nil {
		return "", err
	}
	quota := u.quotaKey()
	if left, ok := h.Quota.Charge(quota, size); !ok {
		return "", &QuotaError{RetryAfter: left}
	}

	if key == "" {
		var err error
		if key, err = h.freeKey(); err != nil {
			h.Quota.Refund(quota, size)
			return "", fmt.Errorf("generating document key: %w", err)
		}
	}

	if err := h.Store.Set(key, content, false); err != nil {
		h.Quota.Refund(quota, size)
		return "", fmt.Errorf("saving document: %w", err)
	}

	pasteCreated.Inc()
	return key, nil
}

// Document is an open paste. Close it once done with it.
type Document struct {
	// ReadSeeker reads the paste from its start
	io.ReadSeeker

	// Size is the paste size in bytes
	Size int64

	file    io.Closer // the paste file, nil for a paste loaded in memory
	release func()
}

// Close closes the paste and gives back its memory charge. It is safe to call more than once.
func (d *Document) Close() error {
	d.release()
	if d.file != nil {
		file := d.file
		d.file = nil
		return file.Close()
	}
	return nil
}

// OpenDocumentFrom opens a paste for the client at ip, see openDocument. A
// lookup of an unknown key counts against ip, and a banned ip gets a
// *BannedError instead.
func (h *DocumentHandler) OpenDocumentFrom(ip, key string, stream, cost int64) (*Document, error) {
	if left, banned := h.NotFound.Banned(ip); banned {
		return nil, &BannedError{RetryAfter: left}
	}
	doc, err := h.openDocument(ip, key, stream, cost)
	if errors.Is(err, storage.ErrNotFound) {
		if left, banned := h.NotFound.Miss(ip); banned {
			return nil, &BannedError{RetryAfter: left}
		}
	}
	return doc, err
}

// openDocument opens a paste for the client at ip, whose reads may hold at
// most its share of the read budget. A backend that can stream opens the
// file and charges the fixed stream bytes. Any other backend loads the whole
// paste and charges cost times its size, plus stream. A missing or empty
// paste is storage.ErrNotFound, and a full budget is ErrMemoryBusy.
func (h *DocumentHandler) openDocument(ip, key string, stream, cost int64) (*Document, error) {
	if opener, ok := h.Store.(storage.Opener); ok {
		file, _, err := opener.Open(key)
		if err != nil {
			return nil, err
		}
		size, err := file.Seek(0, io.SeekEnd)
		if err == nil {
			_, err = file.Seek(0, io.SeekStart)
		}
		if err == nil && size <= 0 {
			err = storage.ErrNotFound
		}
		if err != nil {
			file.Close()
			return nil, err
		}
		release, ok := h.chargeRead(ip, size, stream)
		if !ok {
			file.Close()
			return nil, ErrMemoryBusy
		}
		pasteRead.Inc()
		return &Document{ReadSeeker: file, Size: size, file: file, release: release}, nil
	}

	size, err := h.pasteSize(key)
	if err != nil {
		return nil, err
	}
	release, ok := h.chargeRead(ip, size, cost*size+stream)
	if !ok {
		return nil, ErrMemoryBusy
	}
	data, err := h.Store.Get(key, false)
	if err == nil && data == "" {
		err = storage.ErrNotFound
	}
	if err != nil {
		release()
		return nil, err
	}
	pasteRead.Inc()
	return &Document{ReadSeeker: strings.NewReader(data), Size: int64(len(data)), release: release}, nil
}

// PublicURL returns the base URL for paste links: BaseURL, or https://<host>
func (h *DocumentHandler) PublicURL(host string) string {
	if h.BaseURL != "" {
		return h.BaseURL
	}
	return "https://" + host
}

// RegisterRoutes registers document routes. None of their answers may be
// cached, see NoStore.
func (h *DocumentHandler) RegisterRoutes(r chi.Router) {
	r = r.With(NoStore)

	r.Get("/raw/{id}", h.HandleRawGet)
	r.Head("/raw/{id}", h.HandleRawGet)

	r.Post("/log", h.HandlePutLog)
	r.Put("/log", h.HandlePutLog)

	r.Post("/documents", h.HandlePost)

	r.Get("/documents/{id}", h.HandleGet)
	r.Head("/documents/{id}", h.HandleGet)
}

// NoStore marks every answer as one no cache may keep: a paste, a link to
// one, or an error about one. Without it a CDN that caches by extension, as
// Cloudflare does for .js and .css, would keep /raw/<key>.js for everyone.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		next.ServeHTTP(w, r)
	})
}

// Handle retrieving a document. A paste over PreviewSize comes back cut, with
// "truncated": true and its whole "size"; /raw serves all of it.
func (h *DocumentHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	key := strings.Split(chi.URLParam(r, "id"), ".")[0]
	ip := ClientIP(r)
	if left, banned := h.NotFound.Banned(ip); banned {
		writeBanned(w, left)
		return
	}

	if r.Method == http.MethodHead {
		if _, err := h.pasteSize(key); err != nil {
			h.writeReadError(w, ip, key, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}

	doc, err := h.openDocument(ip, key, previewMemory, RawReadCost)
	if err != nil {
		h.writeReadError(w, ip, key, err)
		return
	}
	defer doc.Close()

	log.Debug().Str("key", key).Int64("size", doc.Size).Msg("Retrieved document")
	w.Header().Set("Content-Type", "application/json")
	if err := writePreviewJSON(w, doc, key, PreviewSize); err != nil {
		log.Debug().Err(err).Msg("Failed to send document")
	}
}

// jsonChunk is the write buffer of a /documents answer
const jsonChunk = 32 << 10

// previewMemory is the charge of a /documents answer: its copy and write
// buffers and the socket buffers of a slow client, see ResponseMemory. The
// text is read and escaped a piece at a time.
const previewMemory = ResponseMemory

// writePreviewJSON writes {"data": text, "key": key} as json.Encoder would,
// where text is the paste, or its first limit bytes cut on a character
// boundary when it is longer; then "size" and "truncated": true are added.
// The text is read and escaped in pieces straight to w, so neither the text
// nor its escaped form is ever held whole.
func writePreviewJSON(w io.Writer, doc *Document, key string, limit int64) error {
	n, err := previewLength(doc, limit)
	if err != nil {
		return err
	}
	if _, err := doc.Seek(0, io.SeekStart); err != nil {
		return err
	}

	bw := bufio.NewWriterSize(w, jsonChunk)
	bw.WriteString(`{"data":"`)
	text := &jsonStringWriter{w: bw}
	buf := make([]byte, max(min(n, CopyBuffer), 1))
	if _, err := io.CopyBuffer(text, io.LimitReader(doc, n), buf); err != nil {
		return err
	}
	text.Flush()
	bw.WriteString(`","key":`)
	escapedKey, err := json.Marshal(key)
	if err != nil {
		return err
	}
	bw.Write(escapedKey)
	if n < doc.Size {
		fmt.Fprintf(bw, `,"size":%d,"truncated":true`, doc.Size)
	}
	bw.WriteString("}\n")
	return bw.Flush()
}

// previewLength returns how many bytes of a paste /documents shows: all of
// it, or limit bytes moved back to the start of the character that limit
// splits. Only a few bytes around limit are read. Invalid UTF-8 there is cut at limit.
func previewLength(doc *Document, limit int64) (int64, error) {
	if doc.Size <= limit {
		return doc.Size, nil
	}
	from := max(limit-(utf8.UTFMax-1), 0)
	if _, err := doc.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	var around [utf8.UTFMax]byte
	window := around[:limit-from+1]
	if _, err := io.ReadFull(doc, window); err != nil {
		return 0, err
	}
	for i := len(window) - 1; i >= 0; i-- {
		if utf8.RuneStart(window[i]) {
			return from + int64(i), nil
		}
	}
	return limit, nil
}

// jsonStringWriter escapes the bytes written to it as the inside of a JSON
// string, the same way encoding/json does with HTML escaping on. A character
// split between two writes is held until the next one; Flush escapes what is
// left of it as invalid UTF-8.
type jsonStringWriter struct {
	w     *bufio.Writer
	carry [utf8.UTFMax - 1]byte
	held  int
}

const hexDigits = "0123456789abcdef"

func (j *jsonStringWriter) Write(p []byte) (int, error) {
	n := len(p)
	if j.held > 0 {
		// Finish the held character with the first bytes of p
		var joined [2 * utf8.UTFMax]byte
		k := copy(joined[:], j.carry[:j.held])
		k += copy(joined[k:], p)
		used := j.escape(joined[:k], false)
		if used < j.held {
			// Still not whole: all of p went into it
			j.held = copy(j.carry[:], joined[used:k])
			return n, nil
		}
		p = p[used-j.held:]
		j.held = 0
	}
	used := j.escape(p, false)
	j.held = copy(j.carry[:], p[used:])
	return n, nil
}

// Flush escapes a character left unfinished at the end
func (j *jsonStringWriter) Flush() {
	j.escape(j.carry[:j.held], true)
	j.held = 0
}

// escape writes src escaped and returns how many bytes it used: all of them,
// unless the last character is cut and final is false
func (j *jsonStringWriter) escape(src []byte, final bool) int {
	start, i := 0, 0
	for i < len(src) {
		b := src[i]
		if b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			j.w.Write(src[start:i])
			switch b {
			case '"', '\\':
				j.w.WriteByte('\\')
				j.w.WriteByte(b)
			case '\b':
				j.w.WriteString(`\b`)
			case '\f':
				j.w.WriteString(`\f`)
			case '\n':
				j.w.WriteString(`\n`)
			case '\r':
				j.w.WriteString(`\r`)
			case '\t':
				j.w.WriteString(`\t`)
			default:
				j.w.WriteString(`\u00`)
				j.w.WriteByte(hexDigits[b>>4])
				j.w.WriteByte(hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		if !final && !utf8.FullRune(src[i:]) {
			break
		}
		c, size := utf8.DecodeRune(src[i:])
		if c == utf8.RuneError && size == 1 {
			j.w.Write(src[start:i])
			j.w.WriteString(`\ufffd`)
			i++
			start = i
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			j.w.Write(src[start:i])
			j.w.WriteString(`\u202`)
			j.w.WriteByte(hexDigits[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	j.w.Write(src[start:i])
	return i
}

// Handle retrieving raw document. A backend that can stream serves the file
// with Range support and without loading it.
func (h *DocumentHandler) HandleRawGet(w http.ResponseWriter, r *http.Request) {
	key := strings.Split(chi.URLParam(r, "id"), ".")[0]
	ip := ClientIP(r)
	if left, banned := h.NotFound.Banned(ip); banned {
		writeBanned(w, left)
		return
	}

	if opener, ok := h.Store.(storage.Opener); ok {
		h.serveRawFile(w, r, opener, ip, key)
		return
	}

	if r.Method == http.MethodHead {
		size, err := h.pasteSize(key)
		if err != nil {
			h.writeReadError(w, ip, key, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.WriteHeader(http.StatusOK)
		return
	}

	doc, err := h.openDocument(ip, key, ResponseMemory, RawReadCost)
	if err != nil {
		h.writeReadError(w, ip, key, err)
		return
	}
	defer doc.Close()

	log.Debug().Str("key", key).Msg("Retrieved raw document")
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	io.Copy(w, doc)
}

// serveRawFile streams a paste from a backend that can open it
func (h *DocumentHandler) serveRawFile(w http.ResponseWriter, r *http.Request, opener storage.Opener, ip, key string) {
	file, modTime, err := opener.Open(key)
	if err != nil {
		h.writeReadError(w, ip, key, err)
		return
	}
	defer file.Close()

	size, err := file.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err == nil && size <= 0 {
		err = storage.ErrNotFound
	}
	if err != nil {
		h.writeReadError(w, ip, key, err)
		return
	}

	// The answer is charged until it is sent: a client that reads slowly keeps
	// the socket buffers full for as long as the write timeout allows
	if r.Method != http.MethodHead {
		release, ok := h.chargeRead(ip, size, ResponseMemory)
		if !ok {
			h.writeReadError(w, ip, key, ErrMemoryBusy)
			return
		}
		defer release()
		pasteRead.Inc()
	}
	log.Debug().Str("key", key).Int64("size", size).Msg("Retrieved raw document")
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	http.ServeContent(w, r, "", modTime, file)
}

// pasteSize returns the size of a paste; an empty paste counts as missing
func (h *DocumentHandler) pasteSize(key string) (int64, error) {
	size, err := h.Store.Size(key)
	if err == nil && size <= 0 {
		err = storage.ErrNotFound
	}
	return size, err
}

// writeReadError answers a failed lookup: 404 for a missing paste, which counts
// against the client and may ban it (429), 503 when the memory budget is full,
// 500 for a storage failure
func (h *DocumentHandler) writeReadError(w http.ResponseWriter, ip, key string, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		log.Debug().Str("key", key).Msg("Document not found")
		if left, banned := h.NotFound.Miss(ip); banned {
			writeBanned(w, left)
			return
		}
		http.Error(w, `{"message": "Document not found."}`, http.StatusNotFound)
	case errors.Is(err, ErrMemoryBusy):
		log.Warn().Msg("Read refused, the memory budget is full")
		w.Header().Set("Retry-After", busyRetryAfter)
		http.Error(w, `{"message": "Server busy, try again shortly."}`, http.StatusServiceUnavailable)
	default:
		log.Error().Err(err).Msg("Error retrieving document")
		http.Error(w, `{"message": "Error retrieving document."}`, http.StatusInternalServerError)
	}
}

// Handle adding a new document (POST)
func (h *DocumentHandler) HandlePost(w http.ResponseWriter, r *http.Request) {
	key, ok := h.ReceiveInto(w, r, AnonymousUploader(r), "")
	if !ok {
		return
	}

	log.Debug().Str("key", key).Msg("Added document")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"key": key})
}

// Handle PUT request that returns a direct link
func (h *DocumentHandler) HandlePutLog(w http.ResponseWriter, r *http.Request) {
	key, ok := h.ReceiveInto(w, r, AnonymousUploader(r), "")
	if !ok {
		return
	}

	log.Debug().Str("key", key).Msg("Added document with log link")
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "\n%s/%s\n\n", h.PublicURL(r.Host), key)
}

// ReceiveInto receives the request body as the paste of u and stores it under
// key, or under a new free key when key is "". A paste already under key is
// replaced only once the new one is stored whole. Every upload limit applies:
// the quota, max length, free space floor, upload slots, slow upload guard and
// memory budget. It returns the key; on failure it writes the error response
// and returns false.
func (h *DocumentHandler) ReceiveInto(w http.ResponseWriter, r *http.Request, u Uploader, key string) (string, bool) {
	ip := u.IP
	if left, over := h.Quota.Exceeded(u.quotaKey()); over {
		writeQuota(w, left)
		return "", false
	}

	// Refuse before reading the body when it is announced over the limit
	if limit := bodyLimit(r, h.maxLength(u)); limit > 0 && r.ContentLength > limit {
		log.Info().Int64("size", r.ContentLength).Msg("Upload refused, announced size over max_length")
		writeTooLarge(w)
		return "", false
	}

	// Refuse before reading the body when the disk is already too full for it
	if err := h.CheckSpace(max(r.ContentLength, 0)); err != nil {
		writeNoSpace(w, err)
		return "", false
	}

	// A large body takes a slot while it arrives
	upload, ok := h.StartUpload(ip, r.ContentLength)
	if !ok {
		log.Warn().Int64("size", r.ContentLength).Msg("Upload rejected, all upload slots are busy")
		writeUploadBusy(w)
		return "", false
	}
	defer upload.Release()

	// Only a large upload, which holds a slot, gets the long deadlines. A small
	// one keeps the server timeouts, so many slow small uploads cannot pile up.
	var guard *UploadGuard
	if IsLargeUpload(r.ContentLength) {
		guard = h.GuardUpload(w, r)
	}
	defer guard.Stop()

	key, err := h.receive(w, r, u, key, upload)
	if err == nil {
		return key, true
	}

	var maxBytesErr *http.MaxBytesError
	var quotaErr *QuotaError
	switch {
	case guard.TooSlow() || errors.Is(err, ErrTooSlow):
		log.Info().Msg("Upload dropped, the body arrived too slowly")
		http.Error(w, `{"message": "Upload too slow, try again on a faster connection."}`, http.StatusRequestTimeout)
	case upload.OutOfMemory() || errors.Is(err, ErrMemoryBusy):
		log.Warn().Msg("Upload dropped, the upload memory budget is full")
		writeUploadBusy(w)
	case errors.Is(err, ErrHeadersTooLarge):
		log.Info().Msg("Upload refused, multipart part headers too large")
		http.Error(w, `{"message": "Multipart part headers too large."}`, http.StatusBadRequest)
	case errors.As(err, &maxBytesErr) || errors.Is(err, multipart.ErrMessageTooLarge) || errors.Is(err, ErrTooLarge):
		log.Info().Msg("Document exceeds max length")
		writeTooLarge(w)
	case errors.As(err, &quotaErr):
		writeQuota(w, quotaErr.RetryAfter)
	case errors.Is(err, ErrEmpty):
		log.Info().Msg("Empty document refused")
		http.Error(w, `{"message": "Document is empty."}`, http.StatusBadRequest)
	case errors.Is(err, storage.ErrNoSpace):
		writeNoSpace(w, err)
	case errors.As(err, new(*saveError)):
		log.Error().Err(err).Msg("Error saving document")
		http.Error(w, `{"message": "Error saving document."}`, http.StatusInternalServerError)
	default:
		log.Warn().Err(err).Msg("Error reading request body")
		http.Error(w, `{"message": "Error reading request body."}`, http.StatusBadRequest)
	}
	return "", false
}

// writeTooLarge answers 413 to an upload over max_length
func writeTooLarge(w http.ResponseWriter) {
	http.Error(w, `{"message": "Document exceeds maximum length."}`, http.StatusRequestEntityTooLarge)
}

// bodyLimit is the largest request body an upload may have: maxLength, plus
// room for the framing of a multipart form; 0 for no limit
func bodyLimit(r *http.Request, maxLength int) int64 {
	if maxLength <= 0 {
		return 0
	}
	limit := int64(maxLength)
	if isMultipart(r) {
		limit += multipartOverhead
	}
	return limit
}

// isMultipart reports whether an upload is a multipart form
func isMultipart(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data")
}

// writeUploadBusy answers 503 to an upload refused for want of a slot or of memory
func writeUploadBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", busyRetryAfter)
	http.Error(w, `{"message": "Server busy with other large uploads, try again shortly."}`, http.StatusServiceUnavailable)
}

// saveError is a failure to store a paste, as opposed to one to read the body
type saveError struct{ err error }

func (e *saveError) Error() string { return "saving document: " + e.err.Error() }
func (e *saveError) Unwrap() error { return e.err }

// receive reads the paste out of the request body, limited to the max length
// of u, and stores it under key, or a new free key when key is "". A backend
// that can stream gets the paste as it arrives, through a fixed buffer; any
// other backend gets it whole, after it is held in memory and charged as it arrives.
func (h *DocumentHandler) receive(w http.ResponseWriter, r *http.Request, u Uploader, key string, upload *Upload) (string, error) {
	isMultipart := isMultipart(r)
	maxLength := h.maxLength(u)
	if limit := bodyLimit(r, maxLength); limit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	if writer, ok := h.Store.(storage.Writer); ok {
		if !upload.Hold(StreamUploadMemory) {
			return "", ErrMemoryBusy
		}
		return h.receiveStream(r, u, key, writer, isMultipart, maxLength)
	}

	var buffer strings.Builder
	if _, err := copyContent(r, isMultipart, maxLength, &chargedWriter{Writer: &buffer, u: upload}); err != nil {
		return "", err
	}
	key, err := h.storeContent(u, key, buffer.String())
	if err != nil {
		return "", &saveError{err}
	}
	return key, nil
}

// receiveStream writes the paste to the store as it arrives and stores it
// under key, or a new free key when key is "", once it is complete. Each piece
// counts against the uploader's quota and the free space floor as it is
// written; nothing is left behind on failure.
func (h *DocumentHandler) receiveStream(r *http.Request, u Uploader, key string, writer storage.Writer, isMultipart bool, maxLength int) (string, error) {
	if key == "" {
		var err error
		if key, err = h.freeKey(); err != nil {
			return "", &saveError{fmt.Errorf("generating document key: %w", err)}
		}
	}
	paste, err := writer.Create(key)
	if err != nil {
		return "", &saveError{err}
	}

	quota := u.quotaKey()
	dst := &quotaWriter{Writer: paste, q: h.Quota, client: quota}
	n, err := copyContent(r, isMultipart, maxLength, dst)
	if err == nil && n == 0 {
		err = ErrEmpty
	}
	if err == nil {
		if err = paste.Close(); err != nil {
			err = &saveError{err}
		}
	}
	if err != nil {
		paste.Abort()
		h.Quota.Refund(quota, dst.charged)
		return "", err
	}

	pasteCreated.Inc()
	return key, nil
}

// copyContent copies the paste out of the request body to dst: the body
// itself, or the "data" field of a multipart form, which may be a file, cut
// at maxLength (0 for no limit). It returns how many bytes it copied. Failed
// writes come back as *saveError.
func copyContent(r *http.Request, isMultipart bool, maxLength int, dst io.Writer) (int64, error) {
	buf := make([]byte, copyBufferSize(r.ContentLength))
	if !isMultipart {
		return copyBody(dst, r.Body, buf)
	}

	// The part headers are read only inside NextPart: count what it reads
	headers := &headerLimiter{ReadCloser: r.Body, left: maxMultipartHeaders}
	r.Body = headers
	form, err := r.MultipartReader()
	if err != nil {
		return 0, err
	}
	var n int64
	found := false
	for {
		headers.counting = true
		part, err := form.NextPart()
		headers.counting = false
		if headers.over {
			return n, ErrHeadersTooLarge
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if part.FormName() != "data" || found {
			if _, err := io.CopyBuffer(io.Discard, part, buf); err != nil {
				return n, err
			}
			continue
		}
		found = true
		src := io.Reader(part)
		if maxLength > 0 {
			src = io.LimitReader(part, int64(maxLength)+1)
		}
		if n, err = copyBody(dst, src, buf); err != nil {
			return n, err
		}
		if maxLength > 0 && n > int64(maxLength) {
			return n, ErrTooLarge
		}
	}
}

// headerLimiter caps what a multipart reader reads while it parses part
// headers. The standard library holds up to 10 MiB of headers per part; this
// stops at left bytes for all parts together. The count also takes in the read
// ahead of the multipart reader's 4 KiB buffer, so it is a little high.
type headerLimiter struct {
	io.ReadCloser
	left     int64
	counting bool
	over     bool
}

func (h *headerLimiter) Read(p []byte) (int, error) {
	if !h.counting {
		return h.ReadCloser.Read(p)
	}
	if h.left <= 0 {
		h.over = true
		return 0, ErrHeadersTooLarge
	}
	if int64(len(p)) > h.left {
		p = p[:h.left]
	}
	n, err := h.ReadCloser.Read(p)
	h.left -= int64(n)
	return n, err
}

// copyBufferSize is the copy buffer for a body of the given length: CopyBuffer,
// or less for a small body of known length
func copyBufferSize(contentLength int64) int {
	if contentLength >= 0 && contentLength < CopyBuffer {
		return int(contentLength) + 1
	}
	return CopyBuffer
}

// copyBody copies src to dst through buf. A failed write comes back as *saveError.
func copyBody(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	var n int64
	for {
		read, readErr := src.Read(buf)
		if read > 0 {
			written, err := dst.Write(buf[:read])
			n += int64(written)
			if err == nil && written < read {
				err = io.ErrShortWrite
			}
			if err != nil {
				return n, &saveError{err}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return n, nil
		}
		if readErr != nil {
			return n, readErr
		}
	}
}

// quotaWriter counts what it writes against the upload quota of a client
type quotaWriter struct {
	io.Writer
	q       *UploadQuota
	client  string // the quota key of the client
	charged int64
}

func (w *quotaWriter) Write(p []byte) (int, error) {
	if left, ok := w.q.Charge(w.client, int64(len(p))); !ok {
		return 0, &QuotaError{RetryAfter: left}
	}
	w.charged += int64(len(p))
	return w.Writer.Write(p)
}

// freeKey generates a key that is not used by an existing document.
// Only a storage miss counts as free; any other storage error is returned.
func (h *DocumentHandler) freeKey() (string, error) {
	for i := 0; i < maxKeyAttempts; i++ {
		key := h.KeyGenerator.Generate(h.KeyLength)
		size, err := h.Store.Size(key)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && size <= 0) {
			return key, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking a new key: %w", err)
		}
	}
	return "", fmt.Errorf("no free key after %d attempts", maxKeyAttempts)
}

// CheckSpace returns storage.ErrNoSpace when the store keeps pastes on a local
// disk and storing size more bytes would leave less free space than its floor
func (h *DocumentHandler) CheckSpace(size int64) error {
	if checker, ok := h.Store.(storage.SpaceChecker); ok {
		return checker.CheckSpace(size)
	}
	return nil
}

// writeNoSpace answers 507 to an upload refused because the disk is nearly full
func writeNoSpace(w http.ResponseWriter, err error) {
	log.Warn().Err(err).Msg("Upload refused, the data dir is low on free disk space")
	http.Error(w, `{"message": "The server is low on disk space, try again later."}`, http.StatusInsufficientStorage)
}

// writeQuota answers 429 to a client that used up its upload quota
func writeQuota(w http.ResponseWriter, left time.Duration) {
	w.Header().Set("Retry-After", fmt.Sprint(RetryAfterSeconds(left)))
	http.Error(w, `{"message": "Upload quota reached, try again later."}`, http.StatusTooManyRequests)
}
