package storage

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned (possibly wrapped) by Get when the key does not exist or has expired
var ErrNotFound = errors.New("not found")

// notFound wraps a backend specific miss error so errors.Is(err, ErrNotFound) matches
func notFound(err error) error {
	return fmt.Errorf("%w: %w", ErrNotFound, err)
}

type Storage interface {
	Set(key string, value string, skip_expiration bool) error
	Get(key string, skip_expiration bool) (string, error)
	Close() error
}
