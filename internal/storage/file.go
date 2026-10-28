package storage

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type FileStorage struct {
	path string
}

var _ Storage = (*FileStorage)(nil)

func md5Hex(input string) string {
	sum := md5.Sum([]byte(input))
	return hex.EncodeToString(sum[:])
}

func NewFileStorage(path string, _ time.Duration) Storage {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		os.Mkdir(path, 0700)
	}

	return &FileStorage{path: path}
}

func (s *FileStorage) Set(key string, value string, skip_expiration bool) error {
	dst := filepath.Join(s.path, md5Hex(key))

	file, err := os.Create(dst)
	if err != nil {
		return err
	}

	defer file.Close()

	_, err = file.WriteString(value)
	return err
}

func (s *FileStorage) Get(key string, skip_expiration bool) (string, error) {
	dst := filepath.Join(s.path, md5Hex(key))
	file, err := os.ReadFile(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return "", notFound(err)
	}
	if err != nil {
		return "", err
	}

	return string(file), nil
}

func (s *FileStorage) Close() error {
	return nil
}
