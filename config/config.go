package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

type LoggingConfig struct {
	// Level is the logging level to use
	Level string `yaml:"level"`

	// Colorize is a flag to enable colorized output
	Colorize bool `yaml:"colorize"`
}

type RateLimitingConfig struct {
	// Enable is a flag to enable rate limiting. It defaults to true: NewConfig
	// sets it before the file is read, so only an explicit false turns it off.
	Enable bool `yaml:"enable"`

	// Limit is the maximum number of requests
	Limit int `yaml:"limit"`

	// Window is the time window to limit requests
	Window int `yaml:"window"`
}

type StorageConfig struct {
	// Type is the storage backend to use
	// Available storage backends are: "redis", "file", "memcached", "mongodb", "s3", "postgres"
	Type string `yaml:"type"`

	// Host is the hostname or IP address of the storage backend
	Host string `yaml:"host"`

	// Port is the port of the storage backend
	Port int `yaml:"port"`

	// Username is the username to use for the storage backend
	Username string `yaml:"username"`

	// Password is the password to use for the storage backend
	Password string `yaml:"password"`

	// Database is the database to use for the storage backend
	Database string `yaml:"database"`

	// Bucket is the bucket to use for the storage backend
	Bucket string `yaml:"bucket"`

	// AWSRegion is the AWS region to use for the storage backend
	// This property is only used for the "s3" storage backend
	AWSRegion string `yaml:"aws_region"`

	// FilePath is the file path to use for the "file" storage backend
	// This property is only used for the "file" storage backend
	FilePath string `yaml:"file_path"`

	// TLS is a flag to use https:// instead of http:// for the storage endpoint
	// This property is only used for the "s3" storage backend
	TLS bool `yaml:"tls"`
}

type DocumentConfig struct {
	// Key is the key of the document
	Key string `yaml:"key"`

	// Path is the path of the document which is going to be read
	Path string `yaml:"path"`
}

type Config struct {
	// Host is the hostname or IP address to bind to
	Host string `yaml:"host"`

	// Port is the port to bind to
	Port int `yaml:"port"`

	// KeyLength is the length of the key to generate which is used for storage key
	KeyLength int `yaml:"key_length"`

	// KeySpace is the key space to use for the key generator
	// This property is only used for the "random" key generator
	KeySpace string `yaml:"key_space"`

	// MaxLength is the maximum length of the paste. Backends other than "file"
	// hold whole pastes in memory, so for them it is capped at 8 MiB, or less
	// with a small MemoryBudget.
	MaxLength int `yaml:"max_length"`

	// MemoryBudget is how many bytes reads and large uploads may hold in memory
	// at the same time, by estimate: half for reads, half for uploads, and one
	// client may hold at most a quarter of each half, which is also the most
	// one request may hold. A request that does not fit gets 503. In a
	// container it is capped at half the memory limit.
	MemoryBudget int `yaml:"memory_budget"`

	// MinFreeDisk is how many bytes must stay free on the filesystem of the
	// "file" storage data dir. An upload that would leave less gets 507.
	// A negative value turns the check off.
	MinFreeDisk int64 `yaml:"min_free_disk"`

	// UploadQuota is how many bytes one client (IPv4 address or IPv6 /64) may
	// store per hour. A negative value means no quota.
	UploadQuota int `yaml:"upload_quota"`

	// Expiration is the maximum lifetime of paste entry, in seconds
	// 0 means there will be no expiration.
	// The "s3" storage doesn't support expiration control. With "file" storage a
	// janitor deletes paste files older than this.
	Expiration int `yaml:"expiration"`

	// KeyGenerator is the key generator to use
	// Available key generators are: "random", "phonetic"
	KeyGenerator string `yaml:"key_generator"`

	// TrustedProxies is the list of proxy CIDRs or IPs allowed to set the client IP
	// through CF-Connecting-IP, X-Real-IP or X-Forwarded-For. Empty means no proxy is trusted.
	TrustedProxies []string `yaml:"trusted_proxies"`

	// RealIPHeader is the only header read for the client IP from trusted proxies,
	// e.g. "CF-Connecting-IP" or "X-Real-IP". Empty means X-Forwarded-For, read right to left.
	RealIPHeader string `yaml:"real_ip_header"`

	// MetricsAddr is the address of the separate Prometheus metrics listener (e.g. "127.0.0.1:9100")
	// Empty means metrics are disabled.
	MetricsAddr string `yaml:"metrics_addr"`

	// BaseURL is the public URL used in links returned by /log (e.g. "https://paste.example.com")
	// Empty means the request Host header is used.
	BaseURL string `yaml:"base_url"`

	// MCPEnable serves the MCP endpoint at /mcp for AI assistants
	MCPEnable bool `yaml:"mcp_enable"`

	// MCPMaxLength is the largest content upload_log accepts over MCP, in bytes.
	// It is capped at MaxLength, and so that 6 times it, plus room for JSON
	// escaping, fits in one client's share of the upload memory budget.
	// Larger files go to /log with curl instead.
	MCPMaxLength int `yaml:"mcp_max_length"`

	// MaxConcurrentUploads is how many large uploads may be received at the same time.
	// A negative value means no limit.
	MaxConcurrentUploads int `yaml:"max_concurrent_uploads"`

	// MaxUploadsPerIP is how many large uploads one client IP may send at the same time.
	// A negative value means no limit.
	MaxUploadsPerIP int `yaml:"max_uploads_per_ip"`

	// MaxInflightPerClient is how many requests one client (an IPv4 address or
	// an IPv6 /64) may have in progress at the same time, on all routes
	// together. More get 429. A negative value means no limit.
	MaxInflightPerClient int `yaml:"max_inflight_per_client"`

	// MCPMaxInflightPerClient is how many /mcp requests one client may have in
	// progress at the same time. More get 429. A negative value means no limit.
	MCPMaxInflightPerClient int `yaml:"mcp_max_inflight_per_client"`

	// UploadTimeout is how long an upload may take, in seconds. It replaces the
	// 60 second read and write timeouts on the upload routes. A negative value keeps them.
	UploadTimeout int `yaml:"upload_timeout"`

	// NotFoundLimit is how many lookups of unknown keys one client IP may make
	// within NotFoundWindow before it is banned from lookups. -1 turns the ban off.
	NotFoundLimit int `yaml:"not_found_limit"`

	// NotFoundWindow is the period over which unknown key lookups are counted, in seconds
	NotFoundWindow int `yaml:"not_found_window"`

	// NotFoundBan is how long a banned client IP gets 429 on lookups, in seconds
	NotFoundBan int `yaml:"not_found_ban"`

	// Storage is the storage backend to use
	// Available storage backends are: "redis", "file", "memcached", "mongodb", "s3", "postgres"
	Storage StorageConfig `yaml:"storage"`

	// Logging is the logging configuration
	Logging LoggingConfig `yaml:"logging"`

	// RateLimiting is the rate limiting configuration
	RateLimiting RateLimitingConfig `yaml:"rate_limiting"`

	// Documents is the list of documents to load statically
	Documents []DocumentConfig `yaml:"documents"`
}

var DefaultConfig = &Config{
	Host:                    "0.0.0.0",
	Port:                    7777,
	KeyLength:               16,
	MaxLength:               4000000,
	MemoryBudget:            256 << 20,
	MinFreeDisk:             1 << 30,
	UploadQuota:             256 << 20,
	KeyGenerator:            "phonetic",
	MCPEnable:               true,
	MCPMaxLength:            4 << 20,
	MaxConcurrentUploads:    8,
	MaxUploadsPerIP:         1,
	MaxInflightPerClient:    16,
	MCPMaxInflightPerClient: 4,
	UploadTimeout:           300,
	NotFoundLimit:           30,
	NotFoundWindow:          900,
	NotFoundBan:             3600,
	Storage: StorageConfig{
		Type:     "file",
		FilePath: "data",
	},
	Logging: LoggingConfig{
		Level: "info",
	},
	RateLimiting: RateLimitingConfig{
		Enable: true,
		Limit:  500,
		Window: 15,
	},
	// No documents: the server then serves its built-in about page as "about"
}

// ExpirationDuration returns the paste expiration, which is configured in seconds
func (c *Config) ExpirationDuration() time.Duration {
	return time.Duration(c.Expiration) * time.Second
}

// NewConfig creates a new Config instance
func NewConfig(configFile string) *Config {
	// Booleans that default to true are set before the file is read,
	// so a missing key keeps the default and "false" turns them off
	cfg := &Config{
		MCPEnable:    DefaultConfig.MCPEnable,
		RateLimiting: RateLimitingConfig{Enable: DefaultConfig.RateLimiting.Enable},
	}

	// Read the configuration file
	data, err := os.ReadFile(configFile)
	if err != nil && !os.IsNotExist(err) {
		log.Fatal().Err(err).Msg("Failed to read configuration file")
	}

	// Unmarshal the configuration file
	if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Fatal().Err(err).Msg("Failed to unmarshal configuration file")
	}

	// Override with environment variables
	if host := os.Getenv("HOST"); host != "" {
		cfg.Host = host
	}

	if port := os.Getenv("PORT"); port != "" {
		portInt, err := strconv.Atoi(port)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse PORT environment variable")
		}
		cfg.Port = portInt
	}

	if keyLength := os.Getenv("KEY_LENGTH"); keyLength != "" {
		keyLengthInt, err := strconv.Atoi(keyLength)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse KEY_LENGTH environment variable")
		}
		cfg.KeyLength = keyLengthInt
	}

	if maxLength := os.Getenv("MAX_LENGTH"); maxLength != "" {
		maxLengthInt, err := strconv.Atoi(maxLength)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse MAX_LENGTH environment variable")
		}
		cfg.MaxLength = maxLengthInt
	}

	for _, setting := range []struct {
		env    string
		target *int
	}{
		{"MEMORY_BUDGET", &cfg.MemoryBudget},
		{"UPLOAD_QUOTA", &cfg.UploadQuota},
	} {
		if value := os.Getenv(setting.env); value != "" {
			valueInt, err := strconv.Atoi(value)
			if err != nil {
				log.Fatal().Err(err).Msgf("Failed to parse %s environment variable", setting.env)
			}
			*setting.target = valueInt
		}
	}

	if minFreeDisk := os.Getenv("MIN_FREE_DISK"); minFreeDisk != "" {
		minFreeDiskInt, err := strconv.ParseInt(minFreeDisk, 10, 64)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse MIN_FREE_DISK environment variable")
		}
		cfg.MinFreeDisk = minFreeDiskInt
	}

	if keySpace := os.Getenv("KEY_SPACE"); keySpace != "" {
		cfg.KeySpace = keySpace
	}

	if expiration := os.Getenv("EXPIRATION"); expiration != "" {
		expirationInt, err := strconv.Atoi(expiration)
		if err != nil || expirationInt < 0 {
			log.Fatal().Str("value", expiration).Msg("Failed to parse EXPIRATION environment variable: expected seconds, 0 or more")
		}
		cfg.Expiration = expirationInt
	}

	if keyGenerator := os.Getenv("KEY_GENERATOR"); keyGenerator != "" {
		cfg.KeyGenerator = keyGenerator
	}

	if storageType := os.Getenv("STORAGE_TYPE"); storageType != "" {
		cfg.Storage.Type = storageType
	}

	if storageHost := os.Getenv("STORAGE_HOST"); storageHost != "" {
		cfg.Storage.Host = storageHost
	}

	if storagePort := os.Getenv("STORAGE_PORT"); storagePort != "" {
		storagePortInt, err := strconv.Atoi(storagePort)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse STORAGE_PORT environment variable")
		}
		cfg.Storage.Port = storagePortInt
	}

	if storageUsername := os.Getenv("STORAGE_USERNAME"); storageUsername != "" {
		cfg.Storage.Username = storageUsername
	}

	if storagePassword := os.Getenv("STORAGE_PASSWORD"); storagePassword != "" {
		cfg.Storage.Password = storagePassword
	}

	if storageDatabase := os.Getenv("STORAGE_DATABASE"); storageDatabase != "" {
		cfg.Storage.Database = storageDatabase
	}

	if storageBucket := os.Getenv("STORAGE_BUCKET"); storageBucket != "" {
		cfg.Storage.Bucket = storageBucket
	}

	if storageAWSRegion := os.Getenv("STORAGE_AWS_REGION"); storageAWSRegion != "" {
		cfg.Storage.AWSRegion = storageAWSRegion
	}

	if storageFilePath := os.Getenv("STORAGE_FILE_PATH"); storageFilePath != "" {
		cfg.Storage.FilePath = storageFilePath
	}

	if storageTLS := os.Getenv("STORAGE_TLS"); storageTLS != "" {
		storageTLSBool, err := strconv.ParseBool(storageTLS)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse STORAGE_TLS environment variable")
		}
		cfg.Storage.TLS = storageTLSBool
	}

	if trustedProxies := os.Getenv("TRUSTED_PROXIES"); trustedProxies != "" {
		cfg.TrustedProxies = nil
		for _, proxy := range strings.Split(trustedProxies, ",") {
			if proxy = strings.TrimSpace(proxy); proxy != "" {
				cfg.TrustedProxies = append(cfg.TrustedProxies, proxy)
			}
		}
	}

	if realIPHeader := os.Getenv("REAL_IP_HEADER"); realIPHeader != "" {
		cfg.RealIPHeader = realIPHeader
	}

	if metricsAddr := os.Getenv("METRICS_ADDR"); metricsAddr != "" {
		cfg.MetricsAddr = metricsAddr
	}

	if baseURL := os.Getenv("BASE_URL"); baseURL != "" {
		cfg.BaseURL = baseURL
	}

	if mcpEnable := os.Getenv("MCP_ENABLE"); mcpEnable != "" {
		mcpEnableBool, err := strconv.ParseBool(mcpEnable)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse MCP_ENABLE environment variable")
		}
		cfg.MCPEnable = mcpEnableBool
	}

	if mcpMaxLength := os.Getenv("MCP_MAX_LENGTH"); mcpMaxLength != "" {
		mcpMaxLengthInt, err := strconv.Atoi(mcpMaxLength)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse MCP_MAX_LENGTH environment variable")
		}
		cfg.MCPMaxLength = mcpMaxLengthInt
	}

	if maxConcurrentUploads := os.Getenv("MAX_CONCURRENT_UPLOADS"); maxConcurrentUploads != "" {
		maxConcurrentUploadsInt, err := strconv.Atoi(maxConcurrentUploads)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse MAX_CONCURRENT_UPLOADS environment variable")
		}
		cfg.MaxConcurrentUploads = maxConcurrentUploadsInt
	}

	if maxUploadsPerIP := os.Getenv("MAX_UPLOADS_PER_IP"); maxUploadsPerIP != "" {
		maxUploadsPerIPInt, err := strconv.Atoi(maxUploadsPerIP)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse MAX_UPLOADS_PER_IP environment variable")
		}
		cfg.MaxUploadsPerIP = maxUploadsPerIPInt
	}

	if uploadTimeout := os.Getenv("UPLOAD_TIMEOUT"); uploadTimeout != "" {
		uploadTimeoutInt, err := strconv.Atoi(uploadTimeout)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse UPLOAD_TIMEOUT environment variable")
		}
		cfg.UploadTimeout = uploadTimeoutInt
	}

	for _, setting := range []struct {
		env    string
		target *int
	}{
		{"MAX_INFLIGHT_PER_CLIENT", &cfg.MaxInflightPerClient},
		{"MCP_MAX_INFLIGHT_PER_CLIENT", &cfg.MCPMaxInflightPerClient},
		{"NOT_FOUND_LIMIT", &cfg.NotFoundLimit},
		{"NOT_FOUND_WINDOW", &cfg.NotFoundWindow},
		{"NOT_FOUND_BAN", &cfg.NotFoundBan},
	} {
		if value := os.Getenv(setting.env); value != "" {
			valueInt, err := strconv.Atoi(value)
			if err != nil {
				log.Fatal().Err(err).Msgf("Failed to parse %s environment variable", setting.env)
			}
			*setting.target = valueInt
		}
	}

	if loggingLevel := os.Getenv("LOGGING_LEVEL"); loggingLevel != "" {
		cfg.Logging.Level = loggingLevel
	}

	if loggingColorize := os.Getenv("LOGGING_COLORIZE"); loggingColorize != "" {
		loggingColorizeBool, err := strconv.ParseBool(loggingColorize)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse LOGGING_COLORIZE environment variable")
		}
		cfg.Logging.Colorize = loggingColorizeBool
	}

	if rateLimitingEnable := os.Getenv("RATE_LIMITING_ENABLE"); rateLimitingEnable != "" {
		rateLimitingEnableBool, err := strconv.ParseBool(rateLimitingEnable)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse RATE_LIMITING_ENABLE environment variable")
		}

		cfg.RateLimiting.Enable = rateLimitingEnableBool
	}

	if rateLimitingLimit := os.Getenv("RATE_LIMITING_LIMIT"); rateLimitingLimit != "" {
		rateLimitingLimitInt, err := strconv.Atoi(rateLimitingLimit)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse RATE_LIMITING_LIMIT environment variable")
		}

		cfg.RateLimiting.Limit = rateLimitingLimitInt
	}

	if rateLimitingWindow := os.Getenv("RATE_LIMITING_WINDOW"); rateLimitingWindow != "" {
		rateLimitingWindowInt, err := strconv.Atoi(rateLimitingWindow)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to parse RATE_LIMITING_WINDOW environment variable")
		}

		cfg.RateLimiting.Window = rateLimitingWindowInt
	}

	// Walk environment variables for documents
	for _, env := range os.Environ() {
		if len(env) > 10 && env[:10] == "DOCUMENTS_" {
			parts := strings.Split(env[10:], "=")
			if len(parts) == 2 {
				cfg.Documents = append(cfg.Documents, DocumentConfig{
					Key:  parts[0],
					Path: parts[1],
				})
			}
		}
	}

	// Apply default values to the configuration
	if cfg.Host == "" {
		cfg.Host = DefaultConfig.Host
	}

	if cfg.Port == 0 {
		cfg.Port = DefaultConfig.Port
	}

	if cfg.KeyLength == 0 {
		cfg.KeyLength = DefaultConfig.KeyLength
	}

	if cfg.MaxLength == 0 {
		cfg.MaxLength = DefaultConfig.MaxLength
	}
	if cfg.MaxLength < 0 {
		log.Fatal().Int("max_length", cfg.MaxLength).Msg("max_length must be 1 or more bytes (0 means the default)")
	}

	if cfg.KeyLength < 1 {
		log.Fatal().Int("key_length", cfg.KeyLength).Msg("key_length must be 1 or more characters (0 means the default)")
	}

	if cfg.MemoryBudget < 0 {
		log.Fatal().Int("memory_budget", cfg.MemoryBudget).Msg("memory_budget must be 1 or more bytes (0 means the default)")
	}
	if cfg.MemoryBudget == 0 {
		cfg.MemoryBudget = DefaultConfig.MemoryBudget
	}

	if cfg.MinFreeDisk == 0 {
		cfg.MinFreeDisk = DefaultConfig.MinFreeDisk
	}
	if cfg.MinFreeDisk < 0 {
		cfg.MinFreeDisk = 0
	}

	if cfg.UploadQuota == 0 {
		cfg.UploadQuota = DefaultConfig.UploadQuota
	}
	if cfg.UploadQuota > 0 && cfg.UploadQuota < cfg.MaxLength {
		log.Warn().Int("upload_quota", cfg.UploadQuota).Int("max_length", cfg.MaxLength).
			Msg("upload_quota is less than max_length: no client can upload a paste of the largest size")
	}

	if cfg.KeyGenerator == "" {
		cfg.KeyGenerator = DefaultConfig.KeyGenerator
	}

	if cfg.MCPMaxLength < 0 {
		log.Fatal().Int("mcp_max_length", cfg.MCPMaxLength).Msg("mcp_max_length must be 0 (default) or more bytes")
	}
	mcpMaxLengthSet := cfg.MCPMaxLength != 0
	if !mcpMaxLengthSet {
		cfg.MCPMaxLength = DefaultConfig.MCPMaxLength
	}
	if cfg.MaxLength > 0 && cfg.MCPMaxLength > cfg.MaxLength {
		if mcpMaxLengthSet {
			log.Warn().Int("mcp_max_length", cfg.MCPMaxLength).Int("max_length", cfg.MaxLength).
				Msg("mcp_max_length is larger than max_length, using max_length")
		}
		cfg.MCPMaxLength = cfg.MaxLength
	}

	if cfg.MaxConcurrentUploads == 0 {
		cfg.MaxConcurrentUploads = DefaultConfig.MaxConcurrentUploads
	}

	if cfg.MaxUploadsPerIP == 0 {
		cfg.MaxUploadsPerIP = DefaultConfig.MaxUploadsPerIP
	}

	if cfg.MaxInflightPerClient == 0 {
		cfg.MaxInflightPerClient = DefaultConfig.MaxInflightPerClient
	}

	if cfg.MCPMaxInflightPerClient == 0 {
		cfg.MCPMaxInflightPerClient = DefaultConfig.MCPMaxInflightPerClient
	}

	if cfg.UploadTimeout == 0 {
		cfg.UploadTimeout = DefaultConfig.UploadTimeout
	}

	if cfg.NotFoundLimit == 0 {
		cfg.NotFoundLimit = DefaultConfig.NotFoundLimit
	}

	if cfg.NotFoundWindow <= 0 {
		cfg.NotFoundWindow = DefaultConfig.NotFoundWindow
	}

	if cfg.NotFoundBan <= 0 {
		cfg.NotFoundBan = DefaultConfig.NotFoundBan
	}

	if cfg.Storage.Type == "" {
		cfg.Storage.Type = DefaultConfig.Storage.Type
	}

	if cfg.Storage.FilePath == "" {
		cfg.Storage.FilePath = DefaultConfig.Storage.FilePath
	}

	if cfg.Logging.Level == "" {
		cfg.Logging.Level = DefaultConfig.Logging.Level
	}

	if cfg.RateLimiting.Limit == 0 {
		cfg.RateLimiting.Limit = DefaultConfig.RateLimiting.Limit
	}

	if cfg.RateLimiting.Window == 0 {
		cfg.RateLimiting.Window = DefaultConfig.RateLimiting.Window
	}

	if cfg.Expiration < 0 {
		log.Fatal().Int("expiration", cfg.Expiration).Msg("expiration must be 0 (never) or more seconds")
	}

	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.RealIPHeader = strings.TrimSpace(cfg.RealIPHeader)

	return cfg
}
