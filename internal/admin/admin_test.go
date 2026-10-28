package admin

import (
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestFilePastesScannedAtMostEveryTenMinutes(t *testing.T) {
	store := storage.NewFileStorage(t.TempDir(), 0).(*storage.FileStorage)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Set("one", "12345", false))

	h := &Handler{Files: store}
	start := time.Unix(1_800_000_000, 0)
	fp := h.filePastes(start)
	require.EqualValues(t, 1, fp.Count)
	require.EqualValues(t, 5, fp.Bytes)
	require.Equal(t, start.UTC(), fp.AsOf)

	// A new paste shows only once the cached count is ten minutes old
	require.NoError(t, store.Set("two", "123", false))
	fp = h.filePastes(start.Add(9 * time.Minute))
	require.EqualValues(t, 1, fp.Count)
	require.Equal(t, start.UTC(), fp.AsOf)
	fp = h.filePastes(start.Add(10 * time.Minute))
	require.EqualValues(t, 2, fp.Count)
	require.EqualValues(t, 8, fp.Bytes)
	require.Equal(t, start.Add(10*time.Minute).UTC(), fp.AsOf)
}
