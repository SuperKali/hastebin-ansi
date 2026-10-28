package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewConfig_DefaultValues(t *testing.T) {
	cfg := NewConfig("nonexistent.yaml") // Should use defaults since file doesn't exist

	require.Equal(t, "0.0.0.0", cfg.Host)
	require.Equal(t, 7777, cfg.Port)
	require.Equal(t, 16, cfg.KeyLength)
	require.Equal(t, "phonetic", cfg.KeyGenerator)
	require.Equal(t, "file", cfg.Storage.Type)
	require.Equal(t, "data", cfg.Storage.FilePath)
	require.Equal(t, "info", cfg.Logging.Level)
	require.True(t, cfg.MCPEnable)
	require.Equal(t, 8, cfg.MaxConcurrentUploads)
	require.Equal(t, 300, cfg.UploadTimeout)
	require.Equal(t, 1, cfg.MaxUploadsPerIP)
	require.Equal(t, 16, cfg.MaxInflightPerClient)
	require.Equal(t, 4, cfg.MCPMaxInflightPerClient)
	require.Equal(t, 256<<20, cfg.MemoryBudget)
	require.Equal(t, 256<<20, cfg.UploadQuota)
	require.EqualValues(t, 1<<30, cfg.MinFreeDisk)
	require.True(t, cfg.RateLimiting.Enable)
	require.Equal(t, 500, cfg.RateLimiting.Limit)
	require.Equal(t, 15, cfg.RateLimiting.Window)
	require.Empty(t, cfg.Documents)

	// The 4 MiB MCP default is capped at the 4000000 byte max_length
	require.Equal(t, 4000000, cfg.MCPMaxLength)
}

func TestNewConfig_NotFoundBan(t *testing.T) {
	cfg := NewConfig("nonexistent.yaml")
	require.Equal(t, 30, cfg.NotFoundLimit)
	require.Equal(t, 900, cfg.NotFoundWindow)
	require.Equal(t, 3600, cfg.NotFoundBan)

	path := t.TempDir() + "/config.yaml"
	require.NoError(t, os.WriteFile(path, []byte("not_found_limit: 5\nnot_found_window: 60\nnot_found_ban: 120\n"), 0o600))
	cfg = NewConfig(path)
	require.Equal(t, 5, cfg.NotFoundLimit)
	require.Equal(t, 60, cfg.NotFoundWindow)
	require.Equal(t, 120, cfg.NotFoundBan)

	t.Setenv("NOT_FOUND_LIMIT", "-1")
	t.Setenv("NOT_FOUND_WINDOW", "30")
	t.Setenv("NOT_FOUND_BAN", "0")
	cfg = NewConfig(path)
	require.Equal(t, -1, cfg.NotFoundLimit)
	require.Equal(t, 30, cfg.NotFoundWindow)
	require.Equal(t, 3600, cfg.NotFoundBan)
}

func TestNewConfig_MCPMaxLength(t *testing.T) {
	// With a larger max_length the default applies
	t.Setenv("MAX_LENGTH", "67108864")
	cfg := NewConfig("nonexistent.yaml")
	require.Equal(t, 4194304, cfg.MCPMaxLength)

	t.Setenv("MCP_MAX_LENGTH", "0")
	cfg = NewConfig("nonexistent.yaml")
	require.Equal(t, 4194304, cfg.MCPMaxLength)

	t.Setenv("MCP_MAX_LENGTH", "1048576")
	cfg = NewConfig("nonexistent.yaml")
	require.Equal(t, 1048576, cfg.MCPMaxLength)

	// It never goes above max_length
	t.Setenv("MAX_LENGTH", "500000")
	cfg = NewConfig("nonexistent.yaml")
	require.Equal(t, 500000, cfg.MCPMaxLength)

	path := t.TempDir() + "/config.yaml"
	require.NoError(t, os.WriteFile(path, []byte("mcp_max_length: 2048\n"), 0o600))
	t.Setenv("MCP_MAX_LENGTH", "")
	cfg = NewConfig(path)
	require.Equal(t, 2048, cfg.MCPMaxLength)
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

func TestNewConfig_MCPAndUploads(t *testing.T) {
	writeConfig := func(t *testing.T, content string) string {
		path := t.TempDir() + "/config.yaml"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	// A config file without the keys keeps the defaults
	cfg := NewConfig(writeConfig(t, "port: 9090\n"))
	require.True(t, cfg.MCPEnable)
	require.Equal(t, 8, cfg.MaxConcurrentUploads)

	// The file can turn MCP off and change the limit
	cfg = NewConfig(writeConfig(t, "mcp_enable: false\nmax_concurrent_uploads: 2\n"))
	require.False(t, cfg.MCPEnable)
	require.Equal(t, 2, cfg.MaxConcurrentUploads)

	// Environment variables win over the file
	t.Setenv("MCP_ENABLE", "true")
	t.Setenv("MAX_CONCURRENT_UPLOADS", "-1")
	cfg = NewConfig(writeConfig(t, "mcp_enable: false\nmax_concurrent_uploads: 2\n"))
	require.True(t, cfg.MCPEnable)
	require.Equal(t, -1, cfg.MaxConcurrentUploads)

	t.Setenv("MCP_ENABLE", "false")
	cfg = NewConfig("nonexistent.yaml")
	require.False(t, cfg.MCPEnable)

	cfg = NewConfig(writeConfig(t, "upload_timeout: 120\n"))
	require.Equal(t, 120, cfg.UploadTimeout)
	t.Setenv("UPLOAD_TIMEOUT", "-1")
	cfg = NewConfig(writeConfig(t, "upload_timeout: 120\n"))
	require.Equal(t, -1, cfg.UploadTimeout)
}

func TestNewConfig_Inflight(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	require.NoError(t, os.WriteFile(path, []byte("max_inflight_per_client: 32\nmcp_max_inflight_per_client: 2\n"), 0o600))
	cfg := NewConfig(path)
	require.Equal(t, 32, cfg.MaxInflightPerClient)
	require.Equal(t, 2, cfg.MCPMaxInflightPerClient)

	// Environment variables win, -1 turns a limit off
	t.Setenv("MAX_INFLIGHT_PER_CLIENT", "-1")
	t.Setenv("MCP_MAX_INFLIGHT_PER_CLIENT", "8")
	cfg = NewConfig(path)
	require.Equal(t, -1, cfg.MaxInflightPerClient)
	require.Equal(t, 8, cfg.MCPMaxInflightPerClient)
}

func TestNewConfig_ExpirationKeySpaceAndPerIP(t *testing.T) {
	t.Setenv("EXPIRATION", "3600")
	t.Setenv("KEY_SPACE", "abc123")
	t.Setenv("MAX_UPLOADS_PER_IP", "-1")

	cfg := NewConfig("nonexistent.yaml")
	require.Equal(t, 3600, cfg.Expiration)
	require.Equal(t, time.Hour, cfg.ExpirationDuration())
	require.Equal(t, "abc123", cfg.KeySpace)
	require.Equal(t, -1, cfg.MaxUploadsPerIP)
}

func TestNewConfig_UnknownKeysStillLoad(t *testing.T) {
	// Keys that older versions read must not break an old config file
	path := t.TempDir() + "/config.yaml"
	require.NoError(t, os.WriteFile(path, []byte("port: 9091\nstatic_max_age: 3\nrecompress_static_assets: false\nlogging:\n  type: text\n"), 0o600))

	cfg := NewConfig(path)
	require.Equal(t, 9091, cfg.Port)
}

func TestExampleConfig(t *testing.T) {
	cfg := NewConfig("../config.yaml")
	require.Equal(t, "file", cfg.Storage.Type)
	require.Equal(t, "data", cfg.Storage.FilePath)
	require.Empty(t, cfg.Documents)
	require.True(t, cfg.MCPEnable)
	require.Equal(t, 500, cfg.RateLimiting.Limit)
	require.Equal(t, 15, cfg.RateLimiting.Window)
}

func TestNewConfig_RateLimitingDefault(t *testing.T) {
	writeConfig := func(t *testing.T, content string) string {
		path := t.TempDir() + "/config.yaml"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	// On when the file does not mention it, or sets only the numbers
	for _, content := range []string{"port: 9090\n", "rate_limiting:\n  limit: 100\n"} {
		cfg := NewConfig(writeConfig(t, content))
		require.True(t, cfg.RateLimiting.Enable, content)
	}
	cfg := NewConfig(writeConfig(t, "rate_limiting:\n  limit: 100\n"))
	require.Equal(t, 100, cfg.RateLimiting.Limit)
	require.Equal(t, 15, cfg.RateLimiting.Window)

	// An explicit false turns it off, in the file or the environment
	cfg = NewConfig(writeConfig(t, "rate_limiting:\n  enable: false\n"))
	require.False(t, cfg.RateLimiting.Enable)
	t.Setenv("RATE_LIMITING_ENABLE", "false")
	cfg = NewConfig("nonexistent.yaml")
	require.False(t, cfg.RateLimiting.Enable)
}

func TestNewConfig_MemoryBudgetAndQuota(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	require.NoError(t, os.WriteFile(path, []byte("memory_budget: 134217728\nupload_quota: -1\n"), 0o600))
	cfg := NewConfig(path)
	require.Equal(t, 128<<20, cfg.MemoryBudget)
	require.Equal(t, -1, cfg.UploadQuota)

	t.Setenv("MEMORY_BUDGET", "536870912")
	t.Setenv("UPLOAD_QUOTA", "1048576")
	cfg = NewConfig(path)
	require.Equal(t, 512<<20, cfg.MemoryBudget)
	require.Equal(t, 1<<20, cfg.UploadQuota)
}

func TestNewConfig_MinFreeDisk(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	require.NoError(t, os.WriteFile(path, []byte("min_free_disk: 5368709120\n"), 0o600))
	require.EqualValues(t, 5<<30, NewConfig(path).MinFreeDisk)

	t.Setenv("MIN_FREE_DISK", "1048576")
	require.EqualValues(t, 1<<20, NewConfig(path).MinFreeDisk)

	// -1 turns the check off, 0 is the default
	t.Setenv("MIN_FREE_DISK", "-1")
	require.Zero(t, NewConfig(path).MinFreeDisk)
	t.Setenv("MIN_FREE_DISK", "0")
	require.EqualValues(t, 1<<30, NewConfig(path).MinFreeDisk)
}

func TestNewConfig_InvalidValuesAreFatal(t *testing.T) {
	// The child process loads the config and exits on the fatal error
	if os.Getenv("CONFIG_FATAL_TEST") != "" {
		NewConfig("nonexistent.yaml")
		return
	}

	tests := []struct {
		env, value, want string
	}{
		{"MAX_LENGTH", "-1", "max_length must be 1 or more bytes"},
		{"KEY_LENGTH", "-5", "key_length must be 1 or more characters"},
		{"MCP_MAX_LENGTH", "-1", "mcp_max_length must be 0 (default) or more bytes"},
		{"MEMORY_BUDGET", "-1", "memory_budget must be 1 or more bytes"},
		{"MIN_FREE_DISK", "1GiB", "Failed to parse MIN_FREE_DISK"},
		{"SESSION_TTL", "-1", "session_ttl must be 1 or more seconds"},
		{"SESSION_IDLE", "7d", "Failed to parse SESSION_IDLE"},
		{"ADMIN_GITHUB_IDS", "octocat", "Failed to parse ADMIN_GITHUB_IDS"},
		{"REGISTER_MIN_ACCOUNT_AGE", "-2", "register_min_account_age must be seconds"},
		{"REGISTERED_MAX_LENGTH", "-1", "registered_max_length must be 1 or more bytes"},
		{"USER_STORAGE_LIMIT", "5GiB", "Failed to parse USER_STORAGE_LIMIT"},
		{"USER_MAX_PASTES", "-2", "user_max_pastes must be 1 or more"},
		{"MAX_TOKENS_PER_USER", "1001", "max_tokens_per_user must be 1 to 1000"},
		{"EDIT_WINDOW", "-1", "edit_window must be 0"},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNewConfig_InvalidValuesAreFatal$")
			cmd.Env = append(os.Environ(), "CONFIG_FATAL_TEST=1", tt.env+"="+tt.value)
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.True(t, errors.As(err, &exitErr), "the process must exit with an error, got %v: %s", err, out)
			require.Contains(t, string(out), tt.want)
		})
	}
}

func TestNewConfig_Accounts(t *testing.T) {
	cfg := NewConfig("nonexistent.yaml")
	require.False(t, cfg.LoginEnabled())
	require.Equal(t, 2592000, cfg.SessionTTL)
	require.Equal(t, 604800, cfg.SessionIdle)
	require.Equal(t, 43200, cfg.AdminReauth)
	require.Equal(t, 604800, cfg.RegisterMinAccountAge)
	require.Empty(t, cfg.AdminGitHubIDs)
	require.Equal(t, filepath.Join("data", "hastebin.db"), cfg.AccountsDBPath())

	t.Setenv("STORAGE_TYPE", "redis")
	require.Equal(t, "hastebin.db", NewConfig("nonexistent.yaml").AccountsDBPath())

	t.Setenv("GITHUB_CLIENT_ID", " Iv1.abc ")
	t.Setenv("GITHUB_CLIENT_SECRET", "s3cret")
	t.Setenv("ACCOUNTS_DB", "/var/lib/hastebin/accounts.db")
	t.Setenv("SESSION_TTL", "3600")
	t.Setenv("SESSION_IDLE", "7200")
	t.Setenv("ADMIN_GITHUB_IDS", " 583231, 42 ,")
	t.Setenv("ADMIN_REAUTH", "600")
	t.Setenv("REGISTER_MIN_ACCOUNT_AGE", "-1")
	cfg = NewConfig("nonexistent.yaml")
	require.True(t, cfg.LoginEnabled())
	require.Equal(t, "Iv1.abc", cfg.GitHubClientID)
	require.Equal(t, "s3cret", cfg.GitHubClientSecret)
	require.Equal(t, "/var/lib/hastebin/accounts.db", cfg.AccountsDBPath())
	require.Equal(t, 3600, cfg.SessionTTL)
	// Idle never outlasts the session itself
	require.Equal(t, 3600, cfg.SessionIdle)
	require.Equal(t, []int64{583231, 42}, cfg.AdminGitHubIDs)
	require.Equal(t, 600, cfg.AdminReauth)
	require.Equal(t, -1, cfg.RegisterMinAccountAge)

	// The yaml file takes a list of ids
	path := t.TempDir() + "/config.yaml"
	require.NoError(t, os.WriteFile(path, []byte("admin_github_ids: [7, 9]\n"), 0o600))
	t.Setenv("ADMIN_GITHUB_IDS", "")
	require.Equal(t, []int64{7, 9}, NewConfig(path).AdminGitHubIDs)
}

func TestNewConfig_RegisteredLimits(t *testing.T) {
	cfg := NewConfig("nonexistent.yaml")
	require.Equal(t, 2*cfg.MaxLength, cfg.RegisteredMaxLength)
	require.Equal(t, 1<<30, cfg.RegisteredUploadQuota)
	require.EqualValues(t, 5<<30, cfg.UserStorageLimit)
	require.Equal(t, 10000, cfg.UserMaxPastes)
	require.Equal(t, 20, cfg.MaxTokensPerUser)

	t.Setenv("MAX_LENGTH", "1000")
	require.Equal(t, 2000, NewConfig("nonexistent.yaml").RegisteredMaxLength)

	t.Setenv("REGISTERED_MAX_LENGTH", "123456")
	t.Setenv("REGISTERED_UPLOAD_QUOTA", "-1")
	t.Setenv("USER_STORAGE_LIMIT", "10737418240")
	t.Setenv("USER_MAX_PASTES", "-1")
	t.Setenv("MAX_TOKENS_PER_USER", "5")
	cfg = NewConfig("nonexistent.yaml")
	require.Equal(t, 123456, cfg.RegisteredMaxLength)
	require.Equal(t, -1, cfg.RegisteredUploadQuota)
	require.EqualValues(t, 10<<30, cfg.UserStorageLimit)
	require.Equal(t, -1, cfg.UserMaxPastes)
	require.Equal(t, 5, cfg.MaxTokensPerUser)

	values := map[string]any{}
	for _, s := range cfg.Effective() {
		values[s.Key] = s.Value
	}
	require.Equal(t, 123456, values["registered_max_length"])
	require.Equal(t, 5, values["max_tokens_per_user"])
}

func TestParseGitHubIDs(t *testing.T) {
	ids, err := ParseGitHubIDs("1,2, 3")
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, ids)
	for _, bad := range []string{"0", "-4", "1;2", "x", "99999999999999999999"} {
		_, err := ParseGitHubIDs(bad)
		require.Error(t, err, bad)
	}
}

func TestEffectiveRedactsSecrets(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "Iv1.public")
	t.Setenv("GITHUB_CLIENT_SECRET", "very-secret-value")
	t.Setenv("STORAGE_PASSWORD", "db-password")
	cfg := NewConfig("nonexistent.yaml")
	values := map[string]any{}
	for _, s := range cfg.Effective() {
		values[s.Key] = s.Value
	}
	require.Equal(t, "set", values["github_client_secret"])
	require.Equal(t, "set", values["storage.password"])
	require.Equal(t, "unset", values["storage.username"])
	require.Equal(t, "Iv1.public", values["github_client_id"])
	require.Equal(t, 2592000, values["session_ttl"])
	require.NotContains(t, fmt.Sprint(cfg.Effective()), "very-secret-value")
	require.NotContains(t, fmt.Sprint(cfg.Effective()), "db-password")
}

func TestNewConfig_EditWindow(t *testing.T) {
	require.Equal(t, 600, NewConfig("nonexistent.yaml").EditWindow)

	// A file without the key keeps the default; 0 turns editing off
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("port: 7777\n"), 0o600))
	require.Equal(t, 600, NewConfig(path).EditWindow)
	require.NoError(t, os.WriteFile(path, []byte("edit_window: 0\n"), 0o600))
	require.Equal(t, 0, NewConfig(path).EditWindow)

	t.Setenv("EDIT_WINDOW", "120")
	cfg := NewConfig(path)
	require.Equal(t, 120, cfg.EditWindow)
	values := map[string]any{}
	for _, s := range cfg.Effective() {
		values[s.Key] = s.Value
	}
	require.Equal(t, 120, values["edit_window"])
	t.Setenv("EDIT_WINDOW", "0")
	require.Equal(t, 0, NewConfig("nonexistent.yaml").EditWindow)
}
