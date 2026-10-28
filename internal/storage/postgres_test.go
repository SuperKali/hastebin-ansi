package storage

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupTestContainer(t *testing.T) (string, int, func()) {
	req := testcontainers.ContainerRequest{
		Image:        "postgres:latest",
		Env:          map[string]string{"POSTGRES_USER": "test", "POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor:   wait.ForListeningPort("5432/tcp"),
	}

	container, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, err := container.Host(context.Background())
	require.NoError(t, err)

	port, err := container.MappedPort(context.Background(), "5432")
	require.NoError(t, err)

	return host, int(port.Num()), func() {
		require.NoError(t, container.Terminate(context.Background()))
	}
}

func TestPostgresStorage(t *testing.T) {
	host, port, cleanup := setupTestContainer(t)
	defer cleanup()

	const expiration = 2 // seconds
	store := NewPostgresStorage(host, port, "test", "test", "testdb", expiration)

	err := store.Set("key1", "value1", false)
	require.NoError(t, err)

	val, err := store.Get("key1", false)
	require.NoError(t, err)
	require.Equal(t, "value1", val)

	size, err := store.Size("key1")
	require.NoError(t, err)
	require.EqualValues(t, len("value1"), size)

	time.Sleep(3 * time.Second)

	_, err = store.Size("key1")
	require.ErrorIs(t, err, ErrNotFound)

	val, err = store.Get("key1", false)
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, val)

	// Get with skip_expiration does not refresh the expiration
	err = store.Set("key1", "value1", false)
	require.NoError(t, err)

	val, err = store.Get("key1", true)
	require.NoError(t, err)
	require.Equal(t, "value1", val)

	time.Sleep(3 * time.Second)

	val, err = store.Get("key1", false)
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, val)

	// Entries set with skip_expiration never expire, even after a normal Get
	err = store.Set("static", "about", true)
	require.NoError(t, err)

	val, err = store.Get("static", false)
	require.NoError(t, err)
	require.Equal(t, "about", val)

	time.Sleep(3 * time.Second)

	val, err = store.Get("static", false)
	require.NoError(t, err)
	require.Equal(t, "about", val)

	requireDelete(t, store)

	require.NoError(t, store.Close())
}

func TestPostgresStorage_NotFound(t *testing.T) {
	host, port, cleanup := setupTestContainer(t)
	defer cleanup()

	store := NewPostgresStorage(host, port, "test", "test", "testdb", 0)

	_, err := store.Get("missing", false)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.Size("missing")
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.Close())
}

func TestPostgresDSN(t *testing.T) {
	dsn := postgresDSN("db.example.com", 5432, "user@corp", "p@ss:w/rd?#%", "pastes")
	require.Equal(t, "postgres://user%40corp:p%40ss%3Aw%2Frd%3F%23%25@db.example.com:5432/pastes", dsn)

	u, err := url.Parse(dsn)
	require.NoError(t, err)
	password, _ := u.User.Password()
	require.Equal(t, "user@corp", u.User.Username())
	require.Equal(t, "p@ss:w/rd?#%", password)
}
