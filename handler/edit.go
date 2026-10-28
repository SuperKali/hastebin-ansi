package handler

import (
	"errors"
	"fmt"
	"io"

	"github.com/armbian/ansi-hastebin/internal/abuse"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/rs/zerolog/log"
)

// The store work behind editing, new versions, duplicates and history. The
// callers, the dashboard API, check who asks first: nothing here looks at
// the AccessGate.

// OpenStored opens the content under a store key for a reader the caller
// checked already, such as an older content of a paste kept under a dotted
// key no route reaches. It is charged to ip in the read budget like /raw. A
// missing key is storage.ErrNotFound, a full budget ErrMemoryBusy.
func (h *DocumentHandler) OpenStored(ip, storeKey string) (*Document, error) {
	return h.openDocument(ip, storeKey, ResponseMemory, RawReadCost)
}

// CopyStored copies the content under src to dst, replacing what dst held
// in one step, and pins dst when pin is set: in a store that is a
// storage.Keeper, else by storing it with skip_expiration. A backend that
// can stream copies through a fixed buffer; any other holds the content in
// memory, charged to ip in the read budget. The free space floor applies.
// It returns the size copied; a missing or empty src is storage.ErrNotFound.
// On failure dst is as it was, but a pin set for it stays.
func (h *DocumentHandler) CopyStored(ip, src, dst string, pin bool) (int64, error) {
	opener, canOpen := h.Store.(storage.Opener)
	writer, canWrite := h.Store.(storage.Writer)
	if !canOpen || !canWrite {
		return h.copyLoaded(ip, src, dst, pin)
	}

	file, _, err := opener.Open(src)
	if err != nil {
		return 0, err
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
		return 0, err
	}
	if err := h.CheckSpace(size); err != nil {
		return 0, err
	}
	release, ok := h.chargeRead(ip, size, CopyBuffer)
	if !ok {
		return 0, ErrMemoryBusy
	}
	defer release()

	if keeper, ok := h.Store.(storage.Keeper); ok && pin {
		keeper.Keep(dst, true)
	}
	paste, err := writer.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.CopyBuffer(paste, file, make([]byte, CopyBuffer))
	if err == nil && n != size {
		err = fmt.Errorf("copied %d bytes of %d", n, size)
	}
	if err != nil {
		paste.Abort()
		return 0, err
	}
	if err := paste.Close(); err != nil {
		return 0, err
	}
	return n, nil
}

// copyLoaded is CopyStored for a backend that cannot stream
func (h *DocumentHandler) copyLoaded(ip, src, dst string, pin bool) (int64, error) {
	size, err := h.pasteSize(src)
	if err != nil {
		return 0, err
	}
	if err := h.CheckSpace(size); err != nil {
		return 0, err
	}
	release, ok := h.chargeRead(ip, size, RawReadCost*size)
	if !ok {
		return 0, ErrMemoryBusy
	}
	defer release()
	data, err := h.Store.Get(src, true)
	if err == nil && data == "" {
		err = storage.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if err := h.Store.Set(dst, data, pin); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// CopyToNewKey copies the content under src to a new free key, see
// CopyStored, and returns the key and size. On failure nothing is left
// under the new key.
func (h *DocumentHandler) CopyToNewKey(ip, src string, pin bool) (string, int64, error) {
	key, err := h.freeKey()
	if err != nil {
		return "", 0, fmt.Errorf("generating document key: %w", err)
	}
	size, err := h.CopyStored(ip, src, key, pin)
	if err != nil {
		h.DeleteStored(key)
		return "", 0, err
	}
	pasteCreated.Inc()
	return key, size, nil
}

// DeleteStored deletes the content under key and lets its pin go. A
// failure is logged and returned; the delete queue tries again later for
// content whose row is gone.
func (h *DocumentHandler) DeleteStored(key string) error {
	if keeper, ok := h.Store.(storage.Keeper); ok {
		defer keeper.Keep(key, false)
	}
	deleter, ok := h.Store.(storage.Deleter)
	if !ok {
		return errors.New("the store cannot delete")
	}
	if err := deleter.Delete(key); err != nil {
		log.Error().Err(err).Msg("Failed to delete stored content")
		return err
	}
	return nil
}

// ChargeQuota counts n bytes against the upload quota of u, for content
// stored without an upload, such as a duplicate. Over it, it returns a
// *QuotaError and counts nothing.
func (h *DocumentHandler) ChargeQuota(u Uploader, n int64) error {
	if left, ok := h.quota(u).Charge(u.quotaKey(), n); !ok {
		h.Event(abuse.KindQuota, u.IP, u.QuotaKey)
		return &QuotaError{RetryAfter: left}
	}
	return nil
}

// RefundQuota gives back n bytes charged to u for content that was not kept
func (h *DocumentHandler) RefundQuota(u Uploader, n int64) {
	h.quota(u).Refund(u.quotaKey(), n)
}
