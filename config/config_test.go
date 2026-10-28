package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewConfig_DefaultValues(t *testing.T) {
	cfg := NewConfig("nonexistent.yaml") // Should use defaults since file doesn't exist

	require.Equal(t, "0.0.0.0", cfg.Host)
	require.Equal(t, 7777, cfg.Port)
	require.Equal(t, 10, cfg.KeyLength)
	require.Equal(t, "phonetic", cfg.KeyGenerator)
	require.Equal(t, "file", cfg.Storage.Type)
	require.Equal(t, "data", cfg.Storage.FilePath)
	require.Equal(t, "info", cfg.Logging.Level)
}

func TestNewConfig_OverrideWithEnvVars(t *testing.T) {
	t.Setenv("HOST", "127.0.0.1")
	t.Setenv("PORT", "8080")
	t.Setenv("KEY_LENGTH", "15")
	t.Setenv("MAX_LENGTH", "5000000")
	t.Setenv("STORAGE_TYPE", "redis")
	t.Setenv("STORAGE_HOST", "localhost")
	t.Setenv("STORAGE_PORT", "6379")
	t.Setenv("LOGGING_LEVEL", "debug")
	t.Setenv("RATE_LIMITING_ENABLE", "true")
	t.Setenv("RATE_LIMITING_LIMIT", "100")

	defer os.Clearenv()

	cfg := NewConfig("nonexistent.yaml") // Load with environment variables

	require.Equal(t, "127.0.0.1", cfg.Host)
	require.Equal(t, 8080, cfg.Port)
	require.Equal(t, 15, cfg.KeyLength)
	require.Equal(t, 5000000, cfg.MaxLength)
	require.Equal(t, "redis", cfg.Storage.Type)
	require.Equal(t, "localhost", cfg.Storage.Host)
	require.Equal(t, 6379, cfg.Storage.Port)
	require.Equal(t, "debug", cfg.Logging.Level)
	require.Equal(t, true, cfg.RateLimiting.Enable)
	require.Equal(t, 100, cfg.RateLimiting.Limit)
}

func TestNewConfig_LoadFromYAML(t *testing.T) {
	yamlContent := `
host: "192.168.1.1"
port: 9090
key_length: 20
storage:
  type: "mongodb"
  host: "mongo.example.com"
  port: 27017
logging:
  level: "warn"
  type: "json"
`

	// Write to a temporary file
	tmpFile, err := os.CreateTemp("", "config_test_*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write([]byte(yamlContent))
	require.NoError(t, err)
	tmpFile.Close()

	cfg := NewConfig(tmpFile.Name()) // Load config from YAML file

	require.Equal(t, "192.168.1.1", cfg.Host)
	require.Equal(t, 9090, cfg.Port)
	require.Equal(t, 20, cfg.KeyLength)
	require.Equal(t, "mongodb", cfg.Storage.Type)
	require.Equal(t, "mongo.example.com", cfg.Storage.Host)
	require.Equal(t, 27017, cfg.Storage.Port)
	require.Equal(t, "warn", cfg.Logging.Level)
}

func TestNewConfig_SecurityOptions(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "172.18.0.0/16, 10.0.0.1,")
	t.Setenv("METRICS_ADDR", "127.0.0.1:9100")
	t.Setenv("BASE_URL", "https://paste.example.com/")
	t.Setenv("STORAGE_TLS", "true")
	t.Setenv("REAL_IP_HEADER", "CF-Connecting-IP")

	cfg := NewConfig("nonexistent.yaml")

	require.Equal(t, "CF-Connecting-IP", cfg.RealIPHeader)

	require.Equal(t, []string{"172.18.0.0/16", "10.0.0.1"}, cfg.TrustedProxies)
	require.Equal(t, "127.0.0.1:9100", cfg.MetricsAddr)
	require.Equal(t, "https://paste.example.com", cfg.BaseURL)
	require.True(t, cfg.Storage.TLS)
}

func TestNewConfig_SecurityDefaults(t *testing.T) {
	cfg := NewConfig("nonexistent.yaml")

	require.Empty(t, cfg.TrustedProxies)
	require.Empty(t, cfg.RealIPHeader)
	require.Empty(t, cfg.MetricsAddr)
	require.Empty(t, cfg.BaseURL)
	require.False(t, cfg.Storage.TLS)
}

func TestConfig_ExpirationDuration(t *testing.T) {
	yamlContent := `
expiration: 3600
trusted_proxies:
  - "172.18.0.0/16"
storage:
  tls: true
`
	tmpFile, err := os.CreateTemp("", "config_test_*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write([]byte(yamlContent))
	require.NoError(t, err)
	tmpFile.Close()

	cfg := NewConfig(tmpFile.Name())

	require.Equal(t, time.Hour, cfg.ExpirationDuration())
	require.Equal(t, []string{"172.18.0.0/16"}, cfg.TrustedProxies)
	require.True(t, cfg.Storage.TLS)

	require.Equal(t, time.Duration(0), (&Config{}).ExpirationDuration())
}
