package handler

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	// DefaultUploadTimeout is how long an upload may take to arrive and be answered
	DefaultUploadTimeout = 5 * time.Minute

	// DefaultMinUploadBytes must arrive in every DefaultUploadWindow, or the upload is dropped.
	// A client that trickles just enough would hold its upload slot for the whole
	// timeout, so the floor is high enough that this costs it real bandwidth.
	DefaultMinUploadBytes = 64 << 10

	// DefaultUploadWindow is the period over which the minimum upload rate is checked
	DefaultUploadWindow = 10 * time.Second

	// DefaultSustainAfter is how long an upload may run at the floor rate. From
	// then on it must have averaged DefaultSustainRate since it started, or it
	// is dropped. So clients that trickle at the floor hold the upload slots for
	// a minute, not for the whole timeout. The cost: a client slower than
	// 512 kbit/s can upload only what it sends in the first minute.
	DefaultSustainAfter = time.Minute

	// DefaultSustainRate is the average rate, in bytes per second, an upload must
	// keep after DefaultSustainAfter
	DefaultSustainRate = 64 << 10
)

// ErrTooSlow is returned when an upload body arrives slower than the minimum rate
var ErrTooSlow = errors.New("upload too slow")

// UploadGuard watches an upload body: it extends the server deadlines for the
// upload and drops the upload when too few bytes arrive in a window, or, once
// it has run for sustainAfter, when it has averaged less than sustainRate.
type UploadGuard struct {
	body         io.ReadCloser
	min          int64
	window       time.Duration
	sustainAfter time.Duration
	sustainRate  int64
	interrupt    func()
	start        time.Time

	mu    sync.Mutex
	count int64 // bytes in the current window
	total int64 // bytes since the start
	slow  bool
	done  bool
	timer *time.Timer
}

// GuardUpload prepares a request with a possibly large body. It moves the read
// and write deadlines to UploadTimeout from now, so uploads are not cut by the
// server defaults, and wraps r.Body with the minimum rate check.
// The caller must call Stop when the body is no longer read.
func (h *DocumentHandler) GuardUpload(w http.ResponseWriter, r *http.Request) *UploadGuard {
	rc := http.NewResponseController(w)

	if h.UploadTimeout > 0 {
		deadline := time.Now().Add(h.UploadTimeout)
		// Writers that do not support deadlines (tests) keep the server defaults
		if err := rc.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Warn().Err(err).Msg("Failed to extend the upload read deadline")
		}
		if err := rc.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Warn().Err(err).Msg("Failed to extend the upload write deadline")
		}
	}

	if h.MinUploadBytes <= 0 || h.UploadWindow <= 0 || r.Body == nil {
		return nil
	}

	// A body read blocked on a silent client is unblocked by moving the read deadline to now
	g := newUploadGuard(r.Body, int64(h.MinUploadBytes), h.UploadWindow, h.SustainAfter, int64(h.SustainRate), func() {
		_ = rc.SetReadDeadline(time.Now())
	})
	r.Body = g
	return g
}

// newUploadGuard watches body: at least min bytes in every window and, from
// sustainAfter on, an average of sustainRate bytes per second since the start
// (0 turns that check off). interrupt unblocks a read waiting on a silent client.
func newUploadGuard(body io.ReadCloser, min int64, window, sustainAfter time.Duration, sustainRate int64, interrupt func()) *UploadGuard {
	g := &UploadGuard{body: body, min: min, window: window, sustainAfter: sustainAfter, sustainRate: sustainRate,
		interrupt: interrupt, start: time.Now()}
	g.timer = time.AfterFunc(window, g.tick)
	return g
}

// tooSlowLocked reports whether the upload is under the floor in the window
// that just ended, or under the sustained rate once that applies
func (g *UploadGuard) tooSlowLocked(now time.Time) bool {
	if g.count < g.min {
		return true
	}
	if g.sustainAfter <= 0 || g.sustainRate <= 0 {
		return false
	}
	elapsed := now.Sub(g.start)
	return elapsed >= g.sustainAfter && float64(g.total) < elapsed.Seconds()*float64(g.sustainRate)
}

// tick runs at the end of each window
func (g *UploadGuard) tick() {
	g.mu.Lock()
	if g.done {
		g.mu.Unlock()
		return
	}
	if g.tooSlowLocked(time.Now()) {
		g.slow = true
		g.done = true
		g.mu.Unlock()
		if g.interrupt != nil {
			g.interrupt()
		}
		return
	}
	g.count = 0
	g.timer.Reset(g.window)
	g.mu.Unlock()
}

func (g *UploadGuard) Read(p []byte) (int, error) {
	g.mu.Lock()
	slow := g.slow
	g.mu.Unlock()
	if slow {
		return 0, ErrTooSlow
	}

	n, err := g.body.Read(p)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.count += int64(n)
	g.total += int64(n)
	if g.slow {
		// The read was cut by the rate check
		return n, ErrTooSlow
	}
	if err != nil {
		// End of the body: the rate no longer matters
		g.stopLocked()
	}
	return n, err
}

// Close stops the rate check and closes the body
func (g *UploadGuard) Close() error {
	g.Stop()
	return g.body.Close()
}

// Stop ends the rate check. It is safe on a nil guard.
func (g *UploadGuard) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.stopLocked()
	g.mu.Unlock()
}

func (g *UploadGuard) stopLocked() {
	g.done = true
	g.timer.Stop()
}

// TooSlow reports whether the upload was dropped for arriving too slowly. It is safe on a nil guard.
func (g *UploadGuard) TooSlow() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.slow
}

// Upload is an upload in progress: for a large one, its upload slot and
// memory charge; for a body charged without a slot (ChargeUpload), the charge only
type Upload struct {
	h      *DocumentHandler // nil for a small upload, which holds nothing
	key    string
	slot   bool    // holds an upload slot
	charge *Charge // nil for a small upload

	once sync.Once
	busy atomic.Bool
}

// Body wraps an upload body that is held in memory as it arrives, so that it
// is charged to the upload memory budget cost times what has arrived. When
// the charge does not fit, reading stops with ErrMemoryBusy. The body of a
// small upload from StartUpload, which holds nothing, is returned as is.
func (u *Upload) Body(body io.ReadCloser, cost int64) io.ReadCloser {
	if u == nil || u.charge == nil || body == nil {
		return body
	}
	return &chargedBody{ReadCloser: body, u: u, cost: cost}
}

// Hold charges a fixed n bytes, the memory a streamed upload needs whatever
// its size. It reports false, and marks the upload out of memory, when they
// do not fit. A small upload is never charged.
func (u *Upload) Hold(n int64) bool {
	if u == nil || u.charge == nil {
		return true
	}
	if !u.charge.Grow(n) {
		u.busy.Store(true)
		return false
	}
	return true
}

// held charges cost times n, the bytes received so far, in steps of at most
// UploadStep, rounded up to ChargeUnit. It reports false, and marks the upload
// out of memory, when that does not fit.
func (u *Upload) held(n, cost int64) bool {
	if u == nil || u.charge == nil {
		return true
	}
	want := cost * n
	for held := u.charge.Size(); held < want; held = u.charge.Size() {
		step := min(UploadStep, (want-held+ChargeUnit-1)/ChargeUnit*ChargeUnit)
		// Near the client's share a whole step may not fit where the exact rest does
		if !u.charge.Grow(step) && !u.charge.Grow(want-held) {
			u.busy.Store(true)
			return false
		}
	}
	return true
}

// OutOfMemory reports whether the body was cut because the memory budget had
// no room for it. It is safe on a nil upload.
func (u *Upload) OutOfMemory() bool {
	return u != nil && u.busy.Load()
}

// Release frees the slot and the memory charge. It is safe to call more than once, and on a nil upload.
func (u *Upload) Release() {
	if u == nil || u.h == nil {
		return
	}
	u.once.Do(func() {
		u.charge.Release()
		if !u.slot {
			return
		}
		h := u.h
		h.slots.Lock()
		defer h.slots.Unlock()
		h.slotCount--
		if u.key != "" {
			if h.slotsByIP[u.key]--; h.slotsByIP[u.key] <= 0 {
				delete(h.slotsByIP, u.key)
			}
		}
	})
}

// chargedBody charges an upload body held in memory as it is read
type chargedBody struct {
	io.ReadCloser
	u    *Upload
	cost int64
	read int64
}

func (b *chargedBody) Read(p []byte) (int, error) {
	if b.u.busy.Load() {
		return 0, ErrMemoryBusy
	}
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	if !b.u.held(b.read, b.cost) {
		return n, ErrMemoryBusy
	}
	return n, err
}

// chargedWriter charges an upload held in memory as its content is written
type chargedWriter struct {
	io.Writer
	u       *Upload
	written int64
}

func (w *chargedWriter) Write(p []byte) (int, error) {
	if !w.u.held(w.written+int64(len(p)), UploadCost) {
		return 0, ErrMemoryBusy
	}
	n, err := w.Writer.Write(p)
	w.written += int64(n)
	return n, err
}
