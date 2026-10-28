package handler

import (
	"errors"
	"io"
	"testing"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestLanguageOfExtension(t *testing.T) {
	for ext, want := range map[string]string{
		"py": "python", ".PY": "python", "log": "ansi", "sh": "bash", "txt": "text", "dtsi": "dts",
		"zig": "zig", "": "", "a.b": "", "toolongextension": "", "pÿ": "", "a\"b": "", "a\r\nb": "",
	} {
		require.Equal(t, want, LanguageOfExtension(ext), ext)
	}
	for lang, want := range map[string]string{
		"python": "py", "ansi": "log", "bash": "sh", "text": "txt", "zig": "zig", "": "", "c++": "", "objective-c": "",
	} {
		require.Equal(t, want, ExtensionOfLanguage(lang), lang)
	}
}

func TestCopyStored(t *testing.T) {
	for _, file := range []bool{true, false} {
		var store storage.Storage
		if file {
			store = storage.NewFileStorage(t.TempDir(), 0)
		} else {
			store = &pinStore{mockStorage: mockStorage{data: map[string]string{}}, pinned: map[string]bool{}}
		}
		h := NewDocumentHandler(6, 1<<20, store, &sequenceKeyGenerator{})
		require.NoError(t, store.Set("src", "hello", false))
		require.NoError(t, store.Set("dst", "old", false))

		n, err := h.CopyStored("", "src", "dst", true)
		require.NoError(t, err)
		require.EqualValues(t, 5, n)
		got, err := store.Get("dst", true)
		require.NoError(t, err)
		require.Equal(t, "hello", got)

		_, err = h.CopyStored("", "missing", "dst", true)
		require.ErrorIs(t, err, storage.ErrNotFound)
		got, _ = store.Get("dst", true)
		require.Equal(t, "hello", got, "a failed copy leaves dst as it was")

		key, size, err := h.CopyToNewKey("", "src", true)
		require.NoError(t, err)
		require.EqualValues(t, 5, size)
		require.NotEqual(t, "src", key)
		got, _ = store.Get(key, true)
		require.Equal(t, "hello", got)

		doc, err := h.OpenStored("", key)
		require.NoError(t, err)
		data, _ := io.ReadAll(doc)
		doc.Close()
		require.Equal(t, "hello", string(data))

		require.NoError(t, h.DeleteStored(key))
		_, err = store.Size(key)
		require.True(t, errors.Is(err, storage.ErrNotFound))
	}
}

func TestCopyStoredRespectsSpaceFloor(t *testing.T) {
	store := storage.NewFileStorage(t.TempDir(), 0)
	fs := store.(*storage.FileStorage)
	h := NewDocumentHandler(6, 1<<20, store, &sequenceKeyGenerator{})
	require.NoError(t, store.Set("src", "hello", false))
	free, err := fs.FreeSpace()
	require.NoError(t, err)
	fs.SetMinFree(free + 1<<30)
	_, err = h.CopyStored("", "src", "dst", true)
	require.ErrorIs(t, err, storage.ErrNoSpace)
	_, err = store.Size("dst")
	require.ErrorIs(t, err, storage.ErrNotFound)
}
