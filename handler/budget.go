package handler

import (
	"errors"
	"sync"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DefaultMemoryBudget is the memory that reads and large uploads may hold at
// the same time, in total. Half of it is for reads, half for uploads.
const DefaultMemoryBudget = 256 << 20

// Peak memory of a request on a backend that loads whole pastes, as a
// multiple of the bytes it loads. File storage streams instead, see the
// fixed charges below.
const (
	// RawReadCost covers the loaded paste and its copy on the way out
	RawReadCost = 2

	// ToolReadCost covers a paste read by the MCP tools and its copy
	ToolReadCost = 2

	// UploadCost covers an upload body and the document built from it
	UploadCost = 2

	// MCPBodyCost covers an /mcp request body held by the MCP library: the
	// body, its JSON decoding and the text taken out of it, about 6 times the
	// body as measured
	MCPBodyCost = 6
)

const (
	// SmallRead is the paste size under which a read is not charged at all:
	// such reads are frequent and cost little
	SmallRead = 64 << 10

	// UploadStep is the most an upload held in memory is charged at a time, as its body arrives
	UploadStep = 1 << 20

	// ChargeUnit is the smallest step of such a charge, so a small body is
	// charged little more than it holds
	ChargeUnit = 64 << 10

	// CopyBuffer is the buffer a streamed upload or read goes through
	CopyBuffer = 32 << 10

	// StreamUploadMemory is the charge of an upload streamed to file storage: its copy buffer
	StreamUploadMemory = CopyBuffer

	// ResponseMemory is the charge of a /raw or /documents answer streamed from
	// file storage: its copy and write buffers and, for a client that reads
	// slowly, the socket buffers the kernel fills for it, about 250 KiB as measured
	ResponseMemory = 256 << 10

	// ToolStreamMemory is the charge of an MCP read or search: the read buffer,
	// the start of a long line, the output and its JSON encoding
	ToolStreamMemory = 2 << 20

	// MaxLoadedLength is the largest max_length for a backend that loads whole
	// pastes, so a paste and its copies fit in one client's share of the budget
	MaxLoadedLength = 8 << 20

	// clientShare is the part of a pool one client may hold: 1/clientShare of it
	clientShare = 4
)

// ErrMemoryBusy is returned when the memory budget has no room for a request
var ErrMemoryBusy = errors.New("server busy, not enough memory free for this request")

var memoryBusy = promauto.NewCounter(prometheus.CounterOpts{
	Name: "hastebin_memory_busy",
	Help: "The total number of requests refused because the memory budget was full",
})

// MemoryBudget caps the memory that requests of one kind (reads, or uploads)
// hold at the same time. Each request charges an estimate of its peak use and
// gives it back when done. A request that does not fit is refused at once,
// never queued.
//
// One client (an IPv4 address or an IPv6 /64) may hold at most a quarter of the
// budget, and no single charge may be larger than that, whoever makes it.
// File storage streams pastes, so its charges are small and fixed; backends
// that load whole pastes have max_length capped to fit (see LoadedLengthCap).
// Methods are safe on a nil budget, which never refuses.
type MemoryBudget struct {
	size      int64
	clientMax int64

	mu      sync.Mutex
	used    int64
	clients map[string]int64
}

// NewMemoryBudget returns a budget of size bytes. size <= 0 returns nil, which never refuses.
func NewMemoryBudget(size int64) *MemoryBudget {
	if size <= 0 {
		return nil
	}
	return &MemoryBudget{size: size, clientMax: max(size/clientShare, 1), clients: make(map[string]int64)}
}

// Charge is the memory held by one request. It can grow while the request
// runs, as an upload of unknown size does. Methods are safe for concurrent use.
type Charge struct {
	b      *MemoryBudget
	client string

	mu   sync.Mutex
	n    int64
	done bool
}

// NewCharge starts an empty charge for the client at ip ("" when unknown,
// which no per client limit applies to)
func (b *MemoryBudget) NewCharge(ip string) *Charge {
	return &Charge{b: b, client: ClientKey(ip)}
}

// Acquire charges n bytes for the client at ip without waiting. When ok is
// true the caller must call release; calling it more than once is safe.
func (b *MemoryBudget) Acquire(ip string, n int64) (release func(), ok bool) {
	c := b.NewCharge(ip)
	if !c.Grow(n) {
		return nil, false
	}
	return c.Release, true
}

// Grow adds n bytes to the charge. It returns false, and charges nothing more,
// when the budget or the client's share has no room, or when the charge would
// be larger than one client's share.
func (c *Charge) Grow(n int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || c.done {
		return !c.done
	}
	b := c.b
	if b == nil {
		c.n += n
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if c.n+n > b.clientMax || b.used+n > b.size {
		memoryBusy.Inc()
		return false
	}
	if c.client != "" {
		held := b.clients[c.client]
		if held+n > b.clientMax {
			memoryBusy.Inc()
			return false
		}
		b.clients[c.client] = held + n
	}
	b.used += n
	c.n += n
	return true
}

// Size returns how many bytes the charge holds
func (c *Charge) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// Release gives the whole charge back. Calling it more than once is safe.
func (c *Charge) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	c.done = true
	b := c.b
	if b == nil || c.n == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= c.n
	if c.client != "" {
		if b.clients[c.client] -= c.n; b.clients[c.client] <= 0 {
			delete(b.clients, c.client)
		}
	}
}

// InUse returns how many bytes are charged
func (b *MemoryBudget) InUse() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Size returns the whole budget in bytes, 0 for no budget
func (b *MemoryBudget) Size() int64 {
	if b == nil {
		return 0
	}
	return b.size
}

// ClientMax returns the most one client may hold, which is also the largest
// single charge; 0 for no budget
func (b *MemoryBudget) ClientMax() int64 {
	if b == nil {
		return 0
	}
	return b.clientMax
}

// Streams reports whether a store streams pastes, both ways, instead of
// loading them whole: then uploads and reads hold only small fixed buffers
func Streams(store storage.Storage) bool {
	_, opens := store.(storage.Opener)
	_, writes := store.(storage.Writer)
	return opens && writes
}

// LoadedLengthCap returns the largest max_length for a backend that loads
// whole pastes under a memory budget of total bytes: MaxLoadedLength, or less
// when one client's share of a half is too small for the costliest load of a
// paste that long. total <= 0 means no budget.
func LoadedLengthCap(total int64) int64 {
	if total <= 0 {
		return MaxLoadedLength
	}
	readShare := NewMemoryBudget(total / 2).ClientMax()
	uploadShare := NewMemoryBudget(total - total/2).ClientMax()
	byRead := (readShare - max(previewMemory, ToolStreamMemory)) / max(RawReadCost, ToolReadCost)
	byUpload := uploadShare / UploadCost
	return max(min(MaxLoadedLength, byRead, byUpload), 1)
}

// SetMemoryBudget splits total bytes into two budgets of half each, one for
// reads and one for uploads, so neither can starve the other. total <= 0
// turns both off.
func (h *DocumentHandler) SetMemoryBudget(total int64) {
	h.ReadMemory = NewMemoryBudget(total / 2)
	h.UploadMemory = NewMemoryBudget(total - total/2)
}

// chargeRead charges the read budget cost bytes for a read of a paste of size
// bytes by the client at ip. A paste under SmallRead is not charged.
func (h *DocumentHandler) chargeRead(ip string, size, cost int64) (release func(), ok bool) {
	if size < SmallRead {
		return func() {}, true
	}
	return h.ReadMemory.Acquire(ip, cost)
}
