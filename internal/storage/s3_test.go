package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

	requireDelete(t, store)

	require.NoError(t, store.Close())
}

// TestS3Delete checks Delete against a fake S3 endpoint, as MinIO is not always available to the tests
func TestS3Delete(t *testing.T) {
	var deleted []string
	status := http.StatusNoContent
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleted = append(deleted, r.Method+" "+r.URL.Path)
		switch status {
		case http.StatusNotFound:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(status)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
		default:
			w.WriteHeader(status)
		}
	}))
	t.Cleanup(ts.Close)

	store := &S3Storage{
		svc: s3.New(s3.Options{
			Region:           minioRegion,
			BaseEndpoint:     aws.String(ts.URL),
			UsePathStyle:     true,
			Credentials:      credentials.NewStaticCredentialsProvider(minioUser, minioPass, ""),
			RetryMaxAttempts: 1,
		}),
		bucket: minioBucket,
	}

	// S3 answers 204 whether the object was there or not
	require.NoError(t, store.Delete("someKey"))
	require.Equal(t, []string{"DELETE /" + minioBucket + "/someKey"}, deleted)

	// A store that says the key is missing is not an error either
	status = http.StatusNotFound
	require.NoError(t, store.Delete("missingKey"))

	status = http.StatusForbidden
	require.Error(t, store.Delete("someKey"))
}
