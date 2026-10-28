package storage

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/redis/go-redis/v9"
)

type RedisStorage struct {
	client     *redis.Client
	expiration time.Duration
}

func NewRedisStorage(host string, port int, username string, password string, expiration time.Duration) *RedisStorage {
	client := redis.NewClient(&redis.Options{
		Addr:     host + ":" + strconv.Itoa(port),
		Username: username,
		Password: password,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check if connection is established with 5s timeout
	if status := client.Ping(ctx); status.Err() != nil {
		log.Fatal().Err(status.Err()).Msg("Failed to connect to Redis")
	}

	return &RedisStorage{client: client, expiration: expiration}
}

var (
	_ Storage = (*RedisStorage)(nil)
	_ Deleter = (*RedisStorage)(nil)
)

func (s *RedisStorage) Set(key string, value string, skip_expiration bool) error {
	ctx := context.Background() // TODO: Add timeout control

	expiry := s.expiration
	if skip_expiration || expiry < 0 {
		expiry = 0
	}

	return s.client.Set(ctx, key, value, expiry).Err()
}

func (s *RedisStorage) Get(key string, skip_expiration bool) (string, error) {
	ctx := context.Background() // TODO: Add timeout control

	res, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", notFound(err)
	}
	if err != nil {
		return "", err
	}

	// Update expiration, only for keys that already expire
	if !skip_expiration && s.expiration > 0 {
		if ttl, err := s.client.TTL(ctx, key).Result(); err == nil && ttl > 0 {
			s.client.Expire(ctx, key, s.expiration)
		}
	}

	return res, nil
}

func (s *RedisStorage) Size(key string) (int64, error) {
	ctx := context.Background() // TODO: Add timeout control

	n, err := s.client.StrLen(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		// STRLEN answers 0 for a missing key too
		exists, err := s.client.Exists(ctx, key).Result()
		if err != nil {
			return 0, err
		}
		if exists == 0 {
			return 0, ErrNotFound
		}
	}
	return n, nil
}

// Delete removes key. A missing key is not an error.
func (s *RedisStorage) Delete(key string) error {
	ctx := context.Background() // TODO: Add timeout control

	return s.client.Del(ctx, key).Err()
}

func (s *RedisStorage) Close() error {
	return s.client.Close()
}
