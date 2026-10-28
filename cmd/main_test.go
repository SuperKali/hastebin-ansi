package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	hastebin "github.com/armbian/ansi-hastebin"
	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestLoadDocuments_BuiltInAbout(t *testing.T) {
	store := storage.NewFileStorage(t.TempDir(), 0)
	require.NoError(t, loadDocuments(store, nil))

	about, err := store.Get("about", true)
	require.NoError(t, err)
	require.Equal(t, hastebin.AboutMD, about)
	require.NotEmpty(t, about)
}

func TestLoadDocuments_Configured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "help.md")
	require.NoError(t, os.WriteFile(path, []byte("help text"), 0o600))

	store := storage.NewFileStorage(t.TempDir(), 0)
	require.NoError(t, loadDocuments(store, []config.DocumentConfig{{Key: "help", Path: path}}))

	help, err := store.Get("help", true)
	require.NoError(t, err)
	require.Equal(t, "help text", help)

	// Configured documents replace the built-in about page
	_, err = store.Get("about", true)
	require.ErrorIs(t, err, storage.ErrNotFound)

	// A missing file is an error
	err = loadDocuments(store, []config.DocumentConfig{{Key: "x", Path: filepath.Join(dir, "missing.md")}})
	require.ErrorContains(t, err, `reading document "x"`)
}

func TestSetMemoryLimit(t *testing.T) {
	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })

	// Outside a container the budget stays as configured
	require.Equal(t, 256<<20, setMemoryLimit(256<<20, 0))
	require.EqualValues(t, 320<<20, debug.SetMemoryLimit(-1))

	// A 300 MiB container caps the budget at 120 MiB (40 percent), plus 64 MiB is
	// still under 70 percent of it; the rest is for socket buffers and the kernel
	require.Equal(t, 120<<20, setMemoryLimit(256<<20, 300<<20))
	require.EqualValues(t, 184<<20, debug.SetMemoryLimit(-1))

	// A large container keeps the budget plus 64 MiB
	require.Equal(t, 256<<20, setMemoryLimit(256<<20, 1<<30))
	require.EqualValues(t, 320<<20, debug.SetMemoryLimit(-1))

	// GOMEMLIMIT wins over the soft limit, the budget is still capped
	debug.SetMemoryLimit(before)
	t.Setenv("GOMEMLIMIT", "1GiB")
	require.Equal(t, 120<<20, setMemoryLimit(256<<20, 300<<20))
	require.Equal(t, before, debug.SetMemoryLimit(-1))
}

func TestCgroupMemoryLimit(t *testing.T) {
	tests := []struct {
		root string
		want int64
	}{
		{"v2", 300 << 20},
		{"v2-max", 0},
		{"v1", 1 << 30},
		{"v1-unlimited", 0},
		{"none", 0},
		{"missing", 0},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, cgroupMemoryLimit(filepath.Join("testdata", "cgroup", tt.root)), tt.root)
	}

	require.Zero(t, parseMemoryLimit(""))
	require.Zero(t, parseMemoryLimit("garbage"))
	require.Zero(t, parseMemoryLimit("-1"))
	require.EqualValues(t, 1024, parseMemoryLimit(" 1024\n"))
}

func TestMemorySettings(t *testing.T) {
	budget, soft := memorySettings(256<<20, 0)
	require.EqualValues(t, 256<<20, budget)
	require.EqualValues(t, 320<<20, soft)

	// A tiny container: the soft limit stays at 70 percent of the container limit
	budget, soft = memorySettings(256<<20, 100<<20)
	require.EqualValues(t, 40<<20, budget)
	require.EqualValues(t, 70<<20, soft)

	// A 1 GiB container keeps the configured budget and the budget plus 64 MiB
	budget, soft = memorySettings(256<<20, 1<<30)
	require.EqualValues(t, 256<<20, budget)
	require.EqualValues(t, 320<<20, soft)

	// A budget configured above 40 percent is lowered to it
	budget, soft = memorySettings(1<<30, 1<<30)
	require.EqualValues(t, (1<<30)*40/100, budget)
	require.EqualValues(t, (1<<30)*40/100+64<<20, soft)

	// A small container: budget plus 64 MiB would pass 70 percent
	budget, soft = memorySettings(256<<20, 200<<20)
	require.EqualValues(t, 80<<20, budget)
	require.EqualValues(t, (200<<20)*70/100, soft)
}

func TestMemorySettingsFromCgroup(t *testing.T) {
	// The limits in the cgroup fixtures, as the server reads them at startup
	tests := []struct {
		root         string
		budget, soft int64
	}{
		{"v2", 120 << 20, 184 << 20},     // 300 MiB: 40 and 61 percent, the rest for sockets and the kernel
		{"v1", 256 << 20, 320 << 20},     // 1 GiB: the configured budget fits
		{"v2-max", 256 << 20, 320 << 20}, // no limit
		{"none", 256 << 20, 320 << 20},
	}
	for _, tt := range tests {
		limit := cgroupMemoryLimit(filepath.Join("testdata", "cgroup", tt.root))
		budget, soft := memorySettings(256<<20, limit)
		require.Equal(t, tt.budget, budget, tt.root)
		require.Equal(t, tt.soft, soft, tt.root)
		if limit > 0 {
			require.LessOrEqual(t, budget, limit*40/100, tt.root)
			require.LessOrEqual(t, soft, limit*70/100, tt.root)
		}
	}
}

func TestSetMinFreeDisk(t *testing.T) {
	store := storage.NewFileStorage(t.TempDir(), 0)
	setMinFreeDisk(store, 1<<62)
	require.ErrorIs(t, store.(*storage.FileStorage).CheckSpace(0), storage.ErrNoSpace)
	setMinFreeDisk(store, 0)
	require.NoError(t, store.(*storage.FileStorage).CheckSpace(1<<40))
}

// loadedStore stands for a backend that loads whole pastes
type loadedStore struct{ storage.Storage }

func TestLimitLoadedLength(t *testing.T) {
	files := storage.NewFileStorage(t.TempDir(), 0)

	// File storage streams: max_length stays
	require.Equal(t, 32<<20, limitLoadedLength(files, 32<<20, 256<<20))

	// Other backends: at most 8 MiB, less with a small budget, never raised
	loaded := loadedStore{files}
	require.Equal(t, 8<<20, limitLoadedLength(loaded, 32<<20, 256<<20))
	require.Equal(t, 8<<20, limitLoadedLength(loaded, 32<<20, 150<<20))
	require.Equal(t, 4000000, limitLoadedLength(loaded, 4000000, 256<<20))
	require.Less(t, limitLoadedLength(loaded, 32<<20, 32<<20), 8<<20)
}

func TestOpenAccounts(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig("nonexistent.yaml")
	cfg.Storage.FilePath = dir

	// Without login and without a database nothing is opened or created
	db, err := openAccounts(cfg)
	require.NoError(t, err)
	require.Nil(t, db)
	require.NoFileExists(t, filepath.Join(dir, "hastebin.db"))

	// With login the database is created in the data dir, mode 0600
	cfg.GitHubClientID = "Iv1.test"
	db, err = openAccounts(cfg)
	require.NoError(t, err)
	require.NotNil(t, db)
	require.NoError(t, db.Close())
	info, err := os.Stat(filepath.Join(dir, "hastebin.db"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// Login turned off later: the existing database is still opened
	cfg.GitHubClientID = ""
	db, err = openAccounts(cfg)
	require.NoError(t, err)
	require.NotNil(t, db)
	require.NoError(t, db.Close())

	// Memcached may evict registered pastes, so accounts refuse it
	cfg.Storage.Type = "memcached"
	cfg.AccountsDB = filepath.Join(dir, "hastebin.db")
	_, err = openAccounts(cfg)
	require.ErrorContains(t, err, "memcached")
}

func TestBackupAccounts(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig("nonexistent.yaml")
	cfg.Storage.FilePath = dir

	out := filepath.Join(t.TempDir(), "backup.db")
	require.Error(t, backupAccounts(cfg, out), "no database yet")

	cfg.GitHubClientID = "Iv1.test"
	db, err := openAccounts(cfg)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, backupAccounts(cfg, out))
	info, err := os.Stat(out)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
