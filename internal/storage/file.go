package storage

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// janitorInterval is how often expired paste files are deleted
const janitorInterval = 10 * time.Minute

// janitorBatch is how many directory entries the janitor reads at a time
const janitorBatch = 1000

// tempPrefix starts the name of a paste file still being written. It is
// renamed to the paste name once it is complete.
const tempPrefix = ".upload-"

// writeTestPrefix starts the name of the file that checks the data dir is writable
const writeTestPrefix = ".write-test-"

// staleTemp is how long a temp file may go unwritten before the janitor
// deletes it. An upload in progress writes far more often than that.
const staleTemp = time.Hour

// diskFreeCache is how long a free space reading is reused. Writes in between
// are taken off it, so a burst of uploads cannot slip past the floor.
const diskFreeCache = 5 * time.Second

type FileStorage struct {
	path string

	// expiration is how long a paste lives after it was last written or read, 0 means forever
	expiration time.Duration

	// pinned holds the file names of the pastes that never expire: the static
	// documents, and any key pinned with Keep
	mu     sync.Mutex
	pinned map[string]bool

	stop      chan struct{}
	closeOnce sync.Once

	// space guards the free space floor minFree (0 for none) and the last
	// reading: free bytes, less what was written since, taken at freeAt.
	// freeErr is set when that reading failed.
	space    sync.Mutex
	minFree  int64
	free     int64
	freeErr  bool
	freeAt   time.Time
	diskFree func(path string) (int64, error)
}

var (
	_ Storage      = (*FileStorage)(nil)
	_ Opener       = (*FileStorage)(nil)
	_ SpaceChecker = (*FileStorage)(nil)
	_ Writer       = (*FileStorage)(nil)
	_ Deleter      = (*FileStorage)(nil)
	_ Keeper       = (*FileStorage)(nil)
)

func md5Hex(input string) string {
	sum := md5.Sum([]byte(input))
	return hex.EncodeToString(sum[:])
}

// NewFileStorage stores pastes as files in path. With an expiration above 0, a
// paste file not written or read for that long is gone: reads no longer find
// it, and a janitor deletes it every 10 minutes. Static documents and keys
// pinned with Keep never expire.
// Temp files of uploads cut by a crash are deleted at startup, and later by
// the janitor once they have not been written for an hour.
func NewFileStorage(path string, expiration time.Duration) Storage {
	if err := prepareDir(path); err != nil {
		msg := "Data dir is not usable"
		if errors.Is(err, fs.ErrPermission) {
			msg = "Data dir is not writable: " + PermissionHint(path)
		}
		log.Fatal().Err(err).Str("path", path).Int("uid", os.Getuid()).Msg(msg)
	}

	s := &FileStorage{
		path:       path,
		expiration: max(expiration, 0),
		pinned:     make(map[string]bool),
		stop:       make(chan struct{}),
		diskFree:   diskFree,
	}
	if n := s.removeTemps(time.Now(), 0); n > 0 {
		log.Info().Int("deleted", n).Str("path", path).Msg("Deleted the temp files of unfinished uploads")
	}
	if s.expiration > 0 {
		log.Info().Dur("expiration", s.expiration).Str("path", path).
			Msg("Paste files not written or read within the expiration are deleted, every 10 minutes")
	}
	go s.janitor(janitorInterval)
	return s
}

// prepareDir creates the data dir if needed and checks that files can be written in it
func prepareDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("creating the data dir: %w", err)
	}

	file, err := os.CreateTemp(path, writeTestPrefix+"*")
	if err != nil {
		return fmt.Errorf("writing to the data dir: %w", err)
	}
	name := file.Name()
	file.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("removing the write test file: %w", err)
	}
	return nil
}

// PermissionHint tells how to make a data dir writable for the user the server runs as
func PermissionHint(path string) string {
	uid, gid := os.Getuid(), os.Getgid()
	return fmt.Sprintf("the data dir must be writable by uid %d (chown -R %d:%d %s)", uid, uid, gid, path)
}

func (s *FileStorage) Set(key string, value string, skip_expiration bool) error {
	if skip_expiration {
		s.Keep(key, true)
	}

	// The same way as Create, so a failed write never leaves a partial paste.
	// The caller checked the free space floor already, and static documents
	// are stored at startup whatever the floor.
	w, err := s.create(key)
	if err != nil {
		return err
	}
	n, err := io.WriteString(w.file, value)
	s.used(int64(n))
	if err != nil {
		w.err = err
	}
	return w.Close()
}

// Create starts a new paste under key. Its bytes go to a temp file in the
// data dir, which Close syncs and renames onto the paste name, so a paste is
// never seen half written. Each write is checked against the free space floor.
func (s *FileStorage) Create(key string) (PasteWriter, error) {
	return s.create(key)
}

func (s *FileStorage) create(key string) (*fileWriter, error) {
	file, err := os.CreateTemp(s.path, tempPrefix+"*")
	if err != nil {
		return nil, err
	}
	return &fileWriter{s: s, file: file, dst: filepath.Join(s.path, md5Hex(key))}, nil
}

// fileWriter writes a paste to a temp file and renames it into place on Close
type fileWriter struct {
	s    *FileStorage
	file *os.File
	dst  string
	err  error // the first write error
	done bool
}

func (w *fileWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, os.ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if err := w.s.CheckSpace(int64(len(p))); err != nil {
		w.err = err
		return 0, err
	}
	n, err := w.file.Write(p)
	w.s.used(int64(n))
	if err != nil {
		w.err = err
	}
	return n, err
}

// Close stores the paste: it syncs the temp file and renames it onto the
// paste name. On any error the temp file is removed.
func (w *fileWriter) Close() error {
	if w.done {
		return os.ErrClosed
	}
	if w.err != nil {
		w.Abort()
		return w.err
	}
	w.done = true
	err := w.file.Sync()
	if closeErr := w.file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(w.file.Name(), w.dst)
	}
	if err != nil {
		w.s.removeTemp(w.file.Name())
	}
	return err
}

// Abort drops the paste and removes its temp file
func (w *fileWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.file.Close()
	w.s.removeTemp(w.file.Name())
}

func (s *FileStorage) removeTemp(name string) {
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn().Err(err).Msg("Failed to delete the temp file of an upload")
	}
}

// SetMinFree sets the free space floor: uploads are refused with ErrNoSpace
// when they would leave less than n bytes free on the filesystem of the data
// dir. n <= 0 turns the check off. Call it before the storage is used.
func (s *FileStorage) SetMinFree(n int64) {
	s.space.Lock()
	defer s.space.Unlock()
	s.minFree = max(n, 0)
}

// FreeSpace returns the bytes free on the filesystem of the data dir
func (s *FileStorage) FreeSpace() (int64, error) {
	return s.diskFree(s.path)
}

// CheckSpace returns ErrNoSpace when storing size more bytes would leave less
// free space than the floor. The free space is read at most every few seconds.
// When it cannot be read the check passes, so a statfs failure never blocks uploads.
func (s *FileStorage) CheckSpace(size int64) error {
	s.space.Lock()
	defer s.space.Unlock()
	if s.minFree <= 0 {
		return nil
	}
	now := time.Now()
	if s.freeAt.IsZero() || now.Sub(s.freeAt) >= diskFreeCache {
		free, err := s.diskFree(s.path)
		if err != nil {
			log.Warn().Err(err).Str("path", s.path).Msg("Failed to read the free disk space, the min_free_disk check is skipped")
		}
		s.free, s.freeErr, s.freeAt = free, err != nil, now
	}
	if s.freeErr {
		return nil
	}
	if s.free-max(size, 0) < s.minFree {
		return fmt.Errorf("%w: %d bytes free, %d more needed, keeping at least %d free", ErrNoSpace, s.free, size, s.minFree)
	}
	return nil
}

// used takes n written bytes off the cached free space
func (s *FileStorage) used(n int64) {
	s.space.Lock()
	defer s.space.Unlock()
	s.free -= n
}

func (s *FileStorage) Get(key string, skip_expiration bool) (string, error) {
	name := md5Hex(key)
	dst := filepath.Join(s.path, name)
	if s.expiration > 0 {
		info, err := os.Stat(dst)
		if err != nil {
			return "", fileError(err)
		}
		if s.expired(name, info, time.Now()) {
			s.remove(dst)
			return "", ErrNotFound
		}
	}

	data, err := readString(dst)
	if err != nil {
		return "", fileError(err)
	}

	if !skip_expiration {
		s.touch(name, dst)
	}
	return data, nil
}

// readString reads a file into a string without the copy that os.ReadFile
// and a string conversion would make, so a large paste is held only once
func readString(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// One byte more than the size, so the final read finds EOF without growing
	b.Grow(int(info.Size()) + 1)
	if _, err := io.Copy(&b, file); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (s *FileStorage) Size(key string) (int64, error) {
	name := md5Hex(key)
	dst := filepath.Join(s.path, name)
	info, err := os.Stat(dst)
	if err != nil {
		return 0, fileError(err)
	}
	if s.expired(name, info, time.Now()) {
		return 0, ErrNotFound
	}
	return info.Size(), nil
}

// Open returns the paste file for streaming, so it is never loaded whole
func (s *FileStorage) Open(key string) (io.ReadSeekCloser, time.Time, error) {
	name := md5Hex(key)
	dst := filepath.Join(s.path, name)
	file, err := os.Open(dst)
	if err != nil {
		return nil, time.Time{}, fileError(err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, time.Time{}, err
	}
	if s.expired(name, info, time.Now()) {
		file.Close()
		return nil, time.Time{}, ErrNotFound
	}

	s.touch(name, dst)
	return file, info.ModTime(), nil
}

func (s *FileStorage) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	return nil
}

// fileError turns a missing file into ErrNotFound
func fileError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return notFound(err)
	}
	return err
}

// Keep pins key so neither reads nor the janitor ever expire it, or lets it
// expire again. Pins live in memory only and are set again at startup.
func (s *FileStorage) Keep(key string, keep bool) {
	name := md5Hex(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if keep {
		s.pinned[name] = true
	} else {
		delete(s.pinned, name)
	}
}

func (s *FileStorage) isPinned(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinned[name]
}

// Delete removes the paste file of key and its pin. A missing file is not an error.
func (s *FileStorage) Delete(key string) error {
	s.Keep(key, false)
	if err := os.Remove(filepath.Join(s.path, md5Hex(key))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// expired reports whether a paste file is past its lifetime. Pinned pastes never are.
func (s *FileStorage) expired(name string, info fs.FileInfo, now time.Time) bool {
	return s.expiration > 0 && now.Sub(info.ModTime()) > s.expiration && !s.isPinned(name)
}

// touch starts the lifetime of a paste again after a read
func (s *FileStorage) touch(name, dst string) {
	if s.expiration <= 0 || s.isPinned(name) {
		return
	}
	now := time.Now()
	if err := os.Chtimes(dst, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn().Err(err).Msg("Failed to refresh the paste expiration")
	}
}

func (s *FileStorage) remove(dst string) {
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn().Err(err).Msg("Failed to delete an expired paste")
	}
}

// janitor deletes expired paste files and stale temp files every interval until Close
func (s *FileStorage) janitor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			now := time.Now()
			if n := s.sweep(now); n > 0 {
				log.Info().Int("deleted", n).Dur("expiration", s.expiration).Msg("Deleted expired pastes")
			}
			if n := s.removeTemps(now, staleTemp); n > 0 {
				log.Info().Int("deleted", n).Msg("Deleted the temp files of unfinished uploads")
			}
		}
	}
}

// sweep deletes the paste files that are past their lifetime and returns how
// many it deleted. Only files named like a paste are touched, never pinned ones.
func (s *FileStorage) sweep(now time.Time) int {
	if s.expiration <= 0 {
		return 0
	}
	return s.eachFile("delete expired pastes", func(name string, info fs.FileInfo) bool {
		return isPasteFileName(name) && s.expired(name, info, now)
	})
}

// removeTemps deletes the temp files of unfinished uploads not written for
// more than maxAge, every one of them when maxAge is 0, and returns how many
// it deleted
func (s *FileStorage) removeTemps(now time.Time, maxAge time.Duration) int {
	return s.eachFile("delete temp files", func(name string, info fs.FileInfo) bool {
		temp := strings.HasPrefix(name, tempPrefix) || strings.HasPrefix(name, writeTestPrefix)
		return temp && (maxAge <= 0 || now.Sub(info.ModTime()) > maxAge)
	})
}

// eachFile deletes the regular files of the data dir that drop selects and
// returns how many it deleted. what names the job in warnings.
func (s *FileStorage) eachFile(what string, drop func(name string, info fs.FileInfo) bool) int {
	dir, err := os.Open(s.path)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to open the data dir to " + what)
		return 0
	}
	defer dir.Close()

	deleted := 0
	for {
		entries, err := dir.ReadDir(janitorBatch)
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil || !drop(entry.Name(), info) {
				continue
			}
			if err := os.Remove(filepath.Join(s.path, entry.Name())); err == nil {
				deleted++
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Warn().Err(err).Msg("Failed to read the data dir to " + what)
			}
			return deleted
		}
	}
}

// isPasteFileName reports whether a file name is the MD5 hex of a key
func isPasteFileName(name string) bool {
	if len(name) != md5.Size*2 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}
