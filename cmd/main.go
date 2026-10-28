package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	hastebin "github.com/armbian/ansi-hastebin"
	"github.com/armbian/ansi-hastebin/config"
	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/server"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// version is the build version, set with -ldflags "-X main.version=v1.2.3"
var version = "dev"

func handleConfig(location string) (*config.Config, storage.Storage, keygenerator.KeyGenerator) {
	cfg := config.NewConfig(location)
	exp := cfg.ExpirationDuration()

	var pasteStorage storage.Storage
	switch cfg.Storage.Type {
	case "file":
		pasteStorage = storage.NewFileStorage(cfg.Storage.FilePath, exp)
		setMinFreeDisk(pasteStorage, cfg.MinFreeDisk)
	case "redis":
		pasteStorage = storage.NewRedisStorage(cfg.Storage.Host, cfg.Storage.Port, cfg.Storage.Username, cfg.Storage.Password, exp)
	case "memcached":
		pasteStorage = storage.NewMemcachedStorage(cfg.Storage.Host, cfg.Storage.Port, int(cfg.Expiration))
	case "mongodb":
		pasteStorage = storage.NewMongoDBStorage(cfg.Storage.Host, cfg.Storage.Port, cfg.Storage.Username, cfg.Storage.Password, cfg.Storage.Database, exp)
	case "postgres":
		pasteStorage = storage.NewPostgresStorage(cfg.Storage.Host, cfg.Storage.Port, cfg.Storage.Username, cfg.Storage.Password, cfg.Storage.Database, int(cfg.Expiration))
	case "s3":
		pasteStorage = storage.NewS3Storage(cfg.Storage.Host, cfg.Storage.Port, cfg.Storage.Username, cfg.Storage.Password, cfg.Storage.AWSRegion, cfg.Storage.Bucket, cfg.Storage.TLS)
	default:
		log.Fatal().Str("storage_type", cfg.Storage.Type).Msg("Unknown storage type")
		return nil, nil, nil
	}

	// Static documents never expire
	if err := loadDocuments(pasteStorage, cfg.Documents); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			log.Fatal().Err(err).Str("path", cfg.Storage.FilePath).Msg("Failed to set document: " + storage.PermissionHint(cfg.Storage.FilePath))
		}
		log.Fatal().Err(err).Msg("Failed to set document")
	}

	var keyGenerator keygenerator.KeyGenerator

	switch cfg.KeyGenerator {
	case "random":
		keyGenerator = keygenerator.NewRandomKeyGenerator(cfg.KeySpace)
	case "phonetic":
		keyGenerator = keygenerator.NewPhoneticKeyGenerator()
	default:
		log.Fatal().Str("key_generator", cfg.KeyGenerator).Msg("Unknown key generator")
		return nil, nil, nil
	}

	// Adjust logger
	logLevel, err := zerolog.ParseLevel(cfg.Logging.Level)
	if err != nil {
		log.Fatal().Err(err).Str("level", cfg.Logging.Level).Msg("Failed to parse log level")
	}
	log.Logger = log.Level(logLevel)

	if cfg.Logging.Colorize {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
	}

	cfg.MemoryBudget = setMemoryLimit(cfg.MemoryBudget, cgroupMemoryLimit("/"))
	cfg.MaxLength = limitLoadedLength(pasteStorage, cfg.MaxLength, cfg.MemoryBudget)

	return cfg, pasteStorage, keyGenerator
}

// setMinFreeDisk sets the free space floor of a file storage and logs it
func setMinFreeDisk(store storage.Storage, minFree int64) {
	files, ok := store.(*storage.FileStorage)
	if !ok {
		return
	}
	files.SetMinFree(minFree)
	if minFree <= 0 {
		log.Info().Msg("min_free_disk is off, uploads are stored until the disk is full")
		return
	}
	event := log.Info().Int64("min_free_disk", minFree)
	if free, err := files.FreeSpace(); err == nil {
		event = event.Int64("free", free)
	}
	event.Msg("Uploads are refused with 507 when the data dir would have less than min_free_disk free")
}

// limitLoadedLength returns the max_length to use with a store. File storage
// streams pastes, so it keeps maxLength. Other backends load whole pastes
// into memory, so maxLength is capped at handler.LoadedLengthCap: 8 MiB, or
// less when one client's share of the memory budget is smaller.
func limitLoadedLength(store storage.Storage, maxLength, budget int) int {
	if handler.Streams(store) {
		return maxLength
	}
	limit := int(handler.LoadedLengthCap(int64(budget)))
	if maxLength <= limit {
		return maxLength
	}
	log.Warn().Int("max_length", maxLength).Int("used", limit).
		Msg("max_length lowered: this storage backend holds whole pastes in memory, use file storage for larger pastes")
	return limit
}

const (
	// runtimeHeadroom is the memory the server needs besides the memory budget
	runtimeHeadroom = 64 << 20

	// budgetPercent and softPercent are the most of a container memory limit
	// the memory budget and the Go soft memory limit may be. The rest is left
	// to what the Go heap does not hold but the container is charged for: the
	// kernel socket buffers of open connections, which slow clients keep full,
	// and the page cache of paste files.
	budgetPercent = 40
	softPercent   = 70
)

// setMemoryLimit returns the memory budget to use and sets the Go soft memory
// limit. limit is the container memory limit, 0 for none. In a container the
// budget is capped at 40 percent of the limit, and the soft limit is the
// budget plus runtimeHeadroom, at most 70 percent of the limit. GOMEMLIMIT,
// when set, replaces the soft limit. The garbage collector then frees what
// large reads and uploads leave behind before it piles up, instead of letting
// the heap grow to twice what is in use.
func setMemoryLimit(budget int, limit int64) int {
	effective, soft := memorySettings(int64(budget), limit)
	if limit > 0 {
		event := log.Info().Int64("container_limit", limit).Int64("memory_budget", effective)
		if effective < int64(budget) {
			event = event.Int("configured", budget)
		}
		event.Msg("Container memory limit found, memory_budget is at most 40 percent of it")
	}

	if os.Getenv("GOMEMLIMIT") != "" {
		log.Info().Str("gomemlimit", os.Getenv("GOMEMLIMIT")).Msg("Go memory limit taken from GOMEMLIMIT")
	} else {
		debug.SetMemoryLimit(soft)
		log.Info().Int64("limit", soft).Msg("Go memory limit set to memory_budget plus 64 MiB, at most 70 percent of the container limit")
	}
	return int(effective)
}

// memorySettings returns the memory budget and the Go soft memory limit for
// a configured budget and a container limit (0 for none)
func memorySettings(budget, limit int64) (effective, soft int64) {
	if limit <= 0 {
		return budget, budget + runtimeHeadroom
	}
	effective = min(budget, limit*budgetPercent/100)
	return effective, min(effective+runtimeHeadroom, limit*softPercent/100)
}

// cgroupMemoryLimit returns the memory limit of the container the server runs
// in, read from the cgroup files under root (normally /): cgroup v2
// memory.max, else cgroup v1 memory.limit_in_bytes. It returns 0 when there
// is no limit or it cannot be read.
func cgroupMemoryLimit(root string) int64 {
	for _, path := range []string{"sys/fs/cgroup/memory.max", "sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue
		}
		return parseMemoryLimit(string(data))
	}
	return 0
}

// parseMemoryLimit parses a cgroup memory limit file. "max" (v2) and the
// huge value cgroup v1 reports without a limit both mean none, which is 0.
func parseMemoryLimit(text string) int64 {
	text = strings.TrimSpace(text)
	if text == "" || text == "max" {
		return 0
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n <= 0 || n >= 1<<62 {
		return 0
	}
	return n
}

// loadDocuments stores the static documents, which never expire. Without any
// configured, the about page built into the binary is stored as "about".
func loadDocuments(store storage.Storage, docs []config.DocumentConfig) error {
	if len(docs) == 0 {
		if err := store.Set("about", hastebin.AboutMD, true); err != nil {
			return fmt.Errorf("setting the built-in about document: %w", err)
		}
		log.Info().Msg("No documents configured, serving the built-in about page")
		return nil
	}

	for _, doc := range docs {
		content, err := os.ReadFile(doc.Path)
		if err != nil {
			return fmt.Errorf("reading document %q: %w", doc.Key, err)
		}
		if err := store.Set(doc.Key, string(content), true); err != nil {
			return fmt.Errorf("setting document %q: %w", doc.Key, err)
		}
	}
	return nil
}

func main() {
	// Parse command line arguments
	var configFile string
	flag.StringVar(&configFile, "config", "config.yaml", "Configuration file")
	flag.Parse()

	srv := server.NewServer(handleConfig(configFile))
	srv.Version = version
	srv.RegisterRoutes()

	// Start the server in a separate goroutine
	go func() {
		srv.Start()
	}()

	// Wait for signal to stop the server
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGTERM, syscall.SIGINT)
	<-stopCh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv.Shutdown(ctx)
}
