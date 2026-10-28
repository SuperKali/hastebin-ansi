package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// requireDelete checks the Deleter of a backend: a deleted paste is gone for
// every read, and deleting a missing key is not an error
func requireDelete(t *testing.T, store Storage) {
	t.Helper()
	deleter, ok := store.(Deleter)
	require.True(t, ok, "the backend implements Deleter")

	require.NoError(t, store.Set("deleteKey", "to be deleted", false))
	require.NoError(t, store.Set("keptKey", "kept", false))
	require.NoError(t, deleter.Delete("deleteKey"))

	_, err := store.Get("deleteKey", false)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Size("deleteKey")
	require.ErrorIs(t, err, ErrNotFound)
	val, err := store.Get("keptKey", true)
	require.NoError(t, err)
	require.Equal(t, "kept", val)

	// Deleting again, or a key never stored, ends the same way
	require.NoError(t, deleter.Delete("deleteKey"))
	require.NoError(t, deleter.Delete("neverStoredKey"))
}
