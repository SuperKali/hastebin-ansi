package storage

import (
	"errors"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/rs/zerolog/log"
)

// noExpirationFlag marks items stored with skip_expiration, so reads never add a TTL
const noExpirationFlag = 1

type MemcachedStorage struct {
	client     *memcache.Client
	expiration int
}

func NewMemcachedStorage(host string, port int, expiration int) *MemcachedStorage {
	client := memcache.New(host + ":" + strconv.Itoa(port))

	// Check if connection is established
	if err := client.Ping(); err != nil {
		log.Error().Err(err).Msg("Failed to connect to Memcached")
	}

	return &MemcachedStorage{client: client, expiration: expiration}
}

var _ Storage = (*MemcachedStorage)(nil)

// maxRelativeExpiration is the largest TTL memcached treats as relative seconds;
// larger values are read as a unix timestamp.
const maxRelativeExpiration = 30 * 24 * 60 * 60

// itemExpiration returns the memcached expiration value for the configured seconds
func (s *MemcachedStorage) itemExpiration() int32 {
	if s.expiration <= 0 {
		return 0
	}
	if s.expiration > maxRelativeExpiration {
		return int32(time.Now().Unix() + int64(s.expiration))
	}
	return int32(s.expiration)
}

func (s *MemcachedStorage) Set(key string, value string, skip_expiration bool) error {
	item := &memcache.Item{
		Key:        key,
		Value:      []byte(value),
		Expiration: s.itemExpiration(),
	}
	if skip_expiration {
		item.Expiration = 0
		item.Flags = noExpirationFlag
	}
	return s.client.Set(item)
}

func (s *MemcachedStorage) Get(key string, skip_expiration bool) (string, error) {
	item, err := s.client.Get(key)
	if errors.Is(err, memcache.ErrCacheMiss) {
		return "", notFound(err)
	}
	if err != nil {
		return "", err
	}

	if !skip_expiration && s.expiration > 0 && item.Flags&noExpirationFlag == 0 {
		s.client.Replace(&memcache.Item{
			Key:        key,
			Value:      item.Value,
			Flags:      item.Flags,
			Expiration: s.itemExpiration(),
		})
	}

	return string(item.Value), nil
}

// Size loads the item to measure it: memcached has no cheaper way, and its items are small (1 MiB by default)
func (s *MemcachedStorage) Size(key string) (int64, error) {
	item, err := s.client.Get(key)
	if errors.Is(err, memcache.ErrCacheMiss) {
		return 0, notFound(err)
	}
	if err != nil {
		return 0, err
	}
	return int64(len(item.Value)), nil
}

func (s *MemcachedStorage) Close() error {
	return s.client.Close()
}
