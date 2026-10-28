package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/testcontainers/testcontainers-go/modules/minio"
)

const (
	minioUser   string = "minio-user"
	minioPass   string = "minio-password"
	minioRegion string = "us-east-1"
	minioBucket string = "test-bucket"
)

func setupMinio(t *testing.T) (string, int, func()) {
	ctx := context.Background()

	c, err := minio.Run(ctx,
		"docker.io/minio/minio",
		minio.WithUsername(minioUser),
		minio.WithPassword(minioPass),
	)
	if err != nil {
		panic(err)
	}

	host, err := c.Host(ctx)
	require.NoError(t, err)

	fmt.Println(c.ConnectionString(ctx))

	port, err := c.MappedPort(ctx, "9000")
	require.NoError(t, err)

	return host, int(port.Num()), func() {
		c.Terminate(ctx)
	}
}

func TestS3Storage(t *testing.T) {
	host, port, cleanup := setupMinio(t)
	defer cleanup()

	store := NewS3Storage(host, port, minioUser, minioPass, minioRegion, minioBucket, false)

	// Test Set
	err := store.Set("testKey", "testValue", false)
	require.NoError(t, err)

	// Test Get
	val, err := store.Get("testKey", false)
	require.NoError(t, err)
	require.Equal(t, "testValue", val)

	size, err := store.Size("testKey")
	require.NoError(t, err)
	require.EqualValues(t, len("testValue"), size)
	_, err = store.Size("nonExistingKey")
	require.ErrorIs(t, err, ErrNotFound)

	// Test Get not existing key
	val, err = store.Get("nonExistingKey", false)
	require.ErrorIs(t, ErrNotFound, err)
	require.Equal(t, "", val)

	require.NoError(t, store.Close())
}
