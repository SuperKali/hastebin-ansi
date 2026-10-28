package storage

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func setupTempDir(t *testing.T) (string, func()) {
	dir, err := os.MkdirTemp("", "filestorage_test")
	require.NoError(t, err)
	cleanup := func() { os.RemoveAll(dir) }
	return dir, cleanup
}

func TestFileStorage(t *testing.T) {
	dir, cleanup := setupTempDir(t)
	t.Cleanup(cleanup)

	store := NewFileStorage(dir, 0)

	// Test Set
	err := store.Set("testKey", "testValue", false)
	require.NoError(t, err)

	// Test Get
	val, err := store.Get("testKey", false)
	require.NoError(t, err)
	require.Equal(t, "testValue", val)

	_, err = store.Get("testKey", false)
	require.NoError(t, err)

	_, err = store.Get("testKey2", false)
	require.ErrorIs(t, err, ErrNotFound)
	require.True(t, errors.Is(err, fs.ErrNotExist))

	// Size reads no content
	size, err := store.Size("testKey")
	require.NoError(t, err)
	require.EqualValues(t, len("testValue"), size)
	_, err = store.Size("testKey2")
	require.ErrorIs(t, err, ErrNotFound)

	// Open streams the paste
	file, modTime, err := store.(Opener).Open("testKey")
	require.NoError(t, err)
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, "testValue", string(data))
	require.WithinDuration(t, time.Now(), modTime, time.Minute)
	_, _, err = store.(Opener).Open("testKey2")
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.Close())
	require.NoError(t, store.Close())
}

func TestFileStorageSkipExpiration(t *testing.T) {
	dir, cleanup := setupTempDir(t)
	t.Cleanup(cleanup)

	store := NewFileStorage(dir, 2*time.Second)

	// Test Set
	err := store.Set("persistentKey", "persistentValue", true)
	require.NoError(t, err)

	// Test Get with skip_expiration
	val, err := store.Get("persistentKey", true)
	require.NoError(t, err)
	require.Equal(t, "persistentValue", val)

	require.NoError(t, store.Close())
}

// age sets the modification time of the file of key to d ago
func age(t *testing.T, dir, key string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	require.NoError(t, os.Chtimes(filepath.Join(dir, md5Hex(key)), old, old))
}

func modTime(t *testing.T, dir, key string) time.Time {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, md5Hex(key)))
	require.NoError(t, err)
	return info.ModTime()
}

func TestFileStorageExpiration(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStorage(dir, time.Hour).(*FileStorage)
	t.Cleanup(func() { store.Close() })

	require.NoError(t, store.Set("old", "old paste", false))
	require.NoError(t, store.Set("fresh", "fresh paste", false))
	require.NoError(t, store.Set("read", "read paste", false))
	require.NoError(t, store.Set("about", "static page", true))
	for _, key := range []string{"old", "read", "about"} {
		age(t, dir, key, 2*time.Hour)
	}

	// An expired paste is gone for every kind of read, and the read deletes it
	_, err := store.Size("old")
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = store.Open("old")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Get("old", false)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = os.Stat(filepath.Join(dir, md5Hex("old")))
	require.ErrorIs(t, err, fs.ErrNotExist)

	// Static documents never expire
	val, err := store.Get("about", false)
	require.NoError(t, err)
	require.Equal(t, "static page", val)

	// A read starts the lifetime again, a read with skip_expiration does not
	age(t, dir, "fresh", 30*time.Minute)
	before := modTime(t, dir, "fresh")
	_, err = store.Get("fresh", true)
	require.NoError(t, err)
	require.Equal(t, before, modTime(t, dir, "fresh"))
	_, err = store.Get("fresh", false)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), modTime(t, dir, "fresh"), time.Minute)

	age(t, dir, "fresh", 30*time.Minute)
	file, _, err := store.Open("fresh")
	require.NoError(t, err)
	file.Close()
	require.WithinDuration(t, time.Now(), modTime(t, dir, "fresh"), time.Minute)

	// Size does not refresh
	age(t, dir, "fresh", 30*time.Minute)
	_, err = store.Size("fresh")
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(-30*time.Minute), modTime(t, dir, "fresh"), time.Minute)
}

func TestFileStorageJanitor(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStorage(dir, time.Hour).(*FileStorage)
	t.Cleanup(func() { store.Close() })

	require.NoError(t, store.Set("old", "old paste", false))
	require.NoError(t, store.Set("fresh", "fresh paste", false))
	require.NoError(t, store.Set("about", "static page", true))
	age(t, dir, "old", 2*time.Hour)
	age(t, dir, "about", 30*24*time.Hour)

	// Files that are not pastes are left alone
	other := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o600))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(other, old, old))

	require.Equal(t, 1, store.sweep(time.Now()))
	_, err := os.Stat(filepath.Join(dir, md5Hex("old")))
	require.ErrorIs(t, err, fs.ErrNotExist)
	for _, key := range []string{"fresh", "about"} {
		_, err := store.Get(key, true)
		require.NoError(t, err, key)
	}
	require.FileExists(t, other)

	// Without expiration nothing is ever deleted
	keep := NewFileStorage(dir, 0).(*FileStorage)
	age(t, dir, "fresh", 1000*time.Hour)
	require.Equal(t, 0, keep.sweep(time.Now()))
	_, err = keep.Get("fresh", false)
	require.NoError(t, err)
}

func TestFileStorageDelete(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStorage(dir, time.Hour).(*FileStorage)
	t.Cleanup(func() { store.Close() })

	requireDelete(t, store)
	_, err := os.Stat(filepath.Join(dir, md5Hex("deleteKey")))
	require.ErrorIs(t, err, fs.ErrNotExist)

	// Deleting a pinned paste drops its pin too
	require.NoError(t, store.Set("about", "static page", true))
	require.NoError(t, store.Delete("about"))
	require.False(t, store.isPinned(md5Hex("about")))
}

func TestFileStorageKeep(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStorage(dir, time.Hour).(*FileStorage)
	t.Cleanup(func() { store.Close() })

	// A key can be pinned before it is stored, like pins set at startup
	store.Keep("owned", true)
	require.NoError(t, store.Set("owned", "owned paste", false))
	require.NoError(t, store.Set("released", "released paste", false))
	store.Keep("released", true)
	require.NoError(t, store.Set("about", "static page", true))
	for _, key := range []string{"owned", "released", "about"} {
		age(t, dir, key, 2*time.Hour)
	}
	store.Keep("released", false)

	// Pinned pastes are found by every read and left alone by the janitor
	require.Equal(t, 1, store.sweep(time.Now()))
	for _, key := range []string{"owned", "about"} {
		_, err := store.Size(key)
		require.NoError(t, err, key)
		file, _, err := store.Open(key)
		require.NoError(t, err, key)
		file.Close()
	}
	_, err := store.Size("released")
	require.ErrorIs(t, err, ErrNotFound)

	// A read does not refresh a pinned paste, it has no lifetime to refresh
	require.WithinDuration(t, time.Now().Add(-2*time.Hour), modTime(t, dir, "owned"), time.Minute)

	// Unpinned, it expires like any other paste
	store.Keep("owned", false)
	require.Equal(t, 1, store.sweep(time.Now()))
	_, err = store.Get("owned", false)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFileStorageJanitorKeepsPinned(t *testing.T) {
	// Built by hand to run the janitor loop itself on a short interval
	dir := t.TempDir()
	store := &FileStorage{path: dir, expiration: time.Minute, pinned: make(map[string]bool),
		stop: make(chan struct{}), diskFree: diskFree}
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Set("about", "static page", true))
	require.NoError(t, store.Set("old", "old paste", false))
	age(t, dir, "about", 2*time.Hour)
	age(t, dir, "old", 2*time.Hour)

	go store.janitor(10 * time.Millisecond)
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, md5Hex("old")))
		return errors.Is(err, fs.ErrNotExist)
	}, 5*time.Second, 10*time.Millisecond)
	require.FileExists(t, filepath.Join(dir, md5Hex("about")))
}

func TestIsPasteFileName(t *testing.T) {
	require.True(t, isPasteFileName(md5Hex("abc")))
	require.False(t, isPasteFileName(".write-test-123"))
	require.False(t, isPasteFileName("notes.txt"))
	require.False(t, isPasteFileName(md5Hex("abc")[:31]+"z"))
}

func TestPrepareDir(t *testing.T) {
	// Nested dirs are created
	dir := filepath.Join(t.TempDir(), "a", "b")
	require.NoError(t, prepareDir(dir))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "the write test file is removed")

	// A path that is a file is refused
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	require.Error(t, prepareDir(file))

	// A read only dir is refused (root can write anyway)
	if os.Getuid() != 0 {
		ro := t.TempDir()
		require.NoError(t, os.Chmod(ro, 0o500))
		t.Cleanup(func() { os.Chmod(ro, 0o700) })
		err := prepareDir(ro)
		require.ErrorIs(t, err, fs.ErrPermission)
		require.Contains(t, PermissionHint(ro), "chown -R")
	}
}

func TestFileStorageMinFree(t *testing.T) {
	s := NewFileStorage(t.TempDir(), 0).(*FileStorage)
	t.Cleanup(func() { s.Close() })

	reads := 0
	free := int64(1000)
	var readErr error
	s.diskFree = func(string) (int64, error) {
		reads++
		return free, readErr
	}

	// Off until a floor is set
	require.NoError(t, s.CheckSpace(1<<40))
	require.Zero(t, reads)

	s.SetMinFree(800)
	require.NoError(t, s.CheckSpace(200))
	require.ErrorIs(t, s.CheckSpace(201), ErrNoSpace)

	// The reading is cached, and writes are taken off it
	require.NoError(t, s.Set("k", string(make([]byte, 150)), false))
	require.ErrorIs(t, s.CheckSpace(51), ErrNoSpace)
	require.NoError(t, s.CheckSpace(50))
	require.Equal(t, 1, reads)

	// A new reading after the cache time
	s.freeAt = s.freeAt.Add(-diskFreeCache)
	free = 700
	require.ErrorIs(t, s.CheckSpace(0), ErrNoSpace)
	require.Equal(t, 2, reads)

	// A failed reading never blocks uploads
	s.freeAt = s.freeAt.Add(-diskFreeCache)
	readErr = errors.New("statfs failed")
	require.NoError(t, s.CheckSpace(1<<40))

	s.SetMinFree(-1)
	readErr = nil
	s.freeAt = time.Time{}
	require.NoError(t, s.CheckSpace(1<<40))

	// The real filesystem has some free space
	got, err := diskFree(t.TempDir())
	require.NoError(t, err)
	require.Positive(t, got)
}

func TestFileStorageWriter(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStorage(dir, time.Hour).(*FileStorage)
	t.Cleanup(func() { s.Close() })
	files := func() []string {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}

	// Nothing is visible under the key until Close, which stores it in one step
	w, err := s.Create("streamed")
	require.NoError(t, err)
	_, err = w.Write([]byte("first, "))
	require.NoError(t, err)
	_, err = s.Size("streamed")
	require.ErrorIs(t, err, ErrNotFound)
	require.Len(t, files(), 1)
	_, err = w.Write([]byte("second"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	w.Abort()
	require.ErrorIs(t, w.Close(), os.ErrClosed)
	val, err := s.Get("streamed", false)
	require.NoError(t, err)
	require.Equal(t, "first, second", val)
	require.Equal(t, []string{md5Hex("streamed")}, files())
	require.WithinDuration(t, time.Now(), modTime(t, dir, "streamed"), time.Minute)

	// Abort leaves nothing, and keeps a paste already stored under the key
	w, err = s.Create("streamed")
	require.NoError(t, err)
	_, err = w.Write([]byte("replacement"))
	require.NoError(t, err)
	w.Abort()
	w.Abort()
	_, err = w.Write([]byte("more"))
	require.Error(t, err)
	val, err = s.Get("streamed", false)
	require.NoError(t, err)
	require.Equal(t, "first, second", val)
	require.Equal(t, []string{md5Hex("streamed")}, files())

	// A write past the free space floor fails, and Close then drops the paste
	s.diskFree = func(string) (int64, error) { return 1000, nil }
	s.SetMinFree(900)
	w, err = s.Create("full")
	require.NoError(t, err)
	_, err = w.Write(make([]byte, 60))
	require.NoError(t, err)
	_, err = w.Write(make([]byte, 60))
	require.ErrorIs(t, err, ErrNoSpace)
	require.ErrorIs(t, w.Close(), ErrNoSpace)
	_, err = s.Size("full")
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, []string{md5Hex("streamed")}, files())
}

func TestFileStorageTempFiles(t *testing.T) {
	dir := t.TempDir()

	// Temp files left by a crash are deleted at startup, pastes are kept
	for _, name := range []string{tempPrefix + "1", tempPrefix + "2", writeTestPrefix + "3"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, md5Hex("kept")), []byte("paste"), 0o600))
	s := NewFileStorage(dir, 0).(*FileStorage)
	t.Cleanup(func() { s.Close() })
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, md5Hex("kept"), entries[0].Name())

	// Later the janitor deletes only those not written for an hour: an upload
	// in progress keeps its file
	stale := filepath.Join(dir, tempPrefix+"stale")
	require.NoError(t, os.WriteFile(stale, []byte("partial"), 0o600))
	old := time.Now().Add(-2 * staleTemp)
	require.NoError(t, os.Chtimes(stale, old, old))
	live, err := s.Create("live")
	require.NoError(t, err)
	_, err = live.Write([]byte("arriving"))
	require.NoError(t, err)

	require.Equal(t, 1, s.removeTemps(time.Now(), staleTemp))
	require.NoFileExists(t, stale)
	require.NoError(t, live.Close())
	val, err := s.Get("live", false)
	require.NoError(t, err)
	require.Equal(t, "arriving", val)
}

func TestFileStorageSetAtomic(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStorage(dir, 0).(*FileStorage)

	// Set writes through a temp file and replaces an older paste whole
	require.NoError(t, store.Set("key", "first version", false))
	require.NoError(t, store.Set("key", "second", false))
	data, err := store.Get("key", false)
	require.NoError(t, err)
	require.Equal(t, "second", data)

	// When the paste cannot be put in place, nothing is left behind: no
	// partial paste, no temp file. A non-empty directory at the paste name
	// makes the rename fail.
	target := filepath.Join(dir, md5Hex("blocked"))
	require.NoError(t, os.MkdirAll(filepath.Join(target, "inside"), 0o700))
	require.Error(t, store.Set("blocked", "content", false))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), tempPrefix)
	}
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestFileStorageJanitorIgnoresDatabase(t *testing.T) {
	dir := t.TempDir()

	// The accounts database lives in the data dir, with its WAL and shared memory files
	names := []string{"hastebin.db", "hastebin.db-wal", "hastebin.db-shm", "hastebin.db-journal"}
	old := time.Now().Add(-1000 * time.Hour)
	for _, name := range names {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("sqlite"), 0o600))
		require.NoError(t, os.Chtimes(path, old, old))
	}

	// Neither startup, the expiry sweep nor the temp file sweep touch them
	s := NewFileStorage(dir, time.Second).(*FileStorage)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, s.Set("paste", "content", false))
	age(t, dir, "paste", time.Hour)
	require.Equal(t, 1, s.sweep(time.Now()))
	require.Equal(t, 0, s.removeTemps(time.Now(), 0))
	for _, name := range names {
		require.FileExists(t, filepath.Join(dir, name))
	}

	// Nor do they count as pastes
	count, bytes, err := s.Usage()
	require.NoError(t, err)
	require.Zero(t, count)
	require.Zero(t, bytes)
}

func TestFileStorageUsage(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStorage(dir, 0).(*FileStorage)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, s.Set("a", "12345", false))
	require.NoError(t, s.Set("b", "123", true))
	require.NoError(t, os.WriteFile(filepath.Join(dir, tempPrefix+"x"), []byte("partial"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, md5Hex("dir")), 0o700))

	count, bytes, err := s.Usage()
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.EqualValues(t, 8, bytes)

	s.SetMinFree(42)
	require.EqualValues(t, 42, s.MinFree())
}
