package storage

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrNotFound is returned (possibly wrapped) by Get when the key does not exist or has expired
var ErrNotFound = errors.New("not found")

// notFound wraps a backend specific miss error so errors.Is(err, ErrNotFound) matches
func notFound(err error) error {
	return fmt.Errorf("%w: %w", ErrNotFound, err)
}

type Storage interface {
	Set(key string, value string, skip_expiration bool) error
	Get(key string, skip_expiration bool) (string, error)

	// Size returns the size of a paste in bytes without loading it, so a read
	// can be weighed before it is made. A missing or expired key is ErrNotFound.
	// It does not refresh the expiration.
	Size(key string) (int64, error)

	Close() error
}

// Opener is implemented by backends that can stream a paste instead of loading it whole
type Opener interface {
	// Open returns the paste for reading and the time it was last written.
	// A missing or expired key is ErrNotFound. It refreshes the expiration like Get(key, false).
	// The caller must close the reader.
	Open(key string) (io.ReadSeekCloser, time.Time, error)
}

// Writer is implemented by backends that can store a paste as it arrives,
// without holding it in memory
type Writer interface {
	// Create starts a new paste under key. Nothing is stored under key until
	// the PasteWriter is closed without error.
	Create(key string) (PasteWriter, error)
}

// PasteWriter receives a paste. Close stores it under its key in one step.
// Abort, or a failed Write or Close, leaves nothing behind. Abort after Close does nothing.
type PasteWriter interface {
	io.WriteCloser
	Abort()
}

// ErrNoSpace is returned (possibly wrapped) when the disk has too little free space left for a paste
var ErrNoSpace = errors.New("not enough free disk space")

// SpaceChecker is implemented by backends that keep pastes on a local disk
type SpaceChecker interface {
	// CheckSpace returns ErrNoSpace when storing size more bytes would leave
	// less free space than the configured floor
	CheckSpace(size int64) error
}

// Deleter is implemented by backends that can delete a paste
type Deleter interface {
	// Delete removes the paste under key. A key that does not exist, or has
	// expired already, is not an error: Delete returns nil, so a delete that
	// is tried again after a failure or a race always ends the same way.
	Delete(key string) error
}

// Keeper is implemented by backends that expire pastes on their own clock
// and can keep chosen ones past it. Backends with a TTL per key do not need
// it: a paste stored with skip_expiration never expires there.
type Keeper interface {
	// Keep pins key so it never expires (keep true), or lets it expire again
	// (keep false). The key need not be stored yet; the pin lasts until the
	// process ends, so pins must be set again at startup.
	Keep(key string, keep bool)
}
