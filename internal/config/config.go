package config

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr                  string
	AdminListenAddr             string
	RedisAddr                   string
	RedisUsername               string
	RedisPassword               string
	RedisPoolSize               int
	AdminRedisPoolSize          int
	LocalCacheTTL               time.Duration
	PostgreAddr                 string
	PostgreAutoMigrate          bool
	PostgreMaxOpenConns         int
	PostgreMaxIdleConns         int
	AdminPostgreMaxOpenConns    int
	AdminPostgreMaxIdleConns    int
	CORSAllowedOrigin           string
	CookieSameSite              http.SameSite
	Environment                 string
	HSTSEnabled                 bool
	HealthCheckToken            string
	MetricsToken                string
	SeqURL                      string
	SeqAPIKey                   string
	ProviderMaxIdleConns        int
	ProviderMaxIdleConnsPerHost int
	OpenAIBaseURL               string
	AnthropicBaseURL            string
	GeminiBaseURL               string
	DeepSeekBaseURL             string
	UsageWorkerConcurrency      int
	UsageTaskMaxRetry           int
	UsageDrainLimit             time.Duration
	JWTAuthentication           JWTAuthentication
	KMS                         KMS
}

type JWTAuthentication struct {
	Secret            string
	ExpirationInHours int
}

type KMS struct {
	Provider            string // "local" | "gcp" | "openbao"
	GCPKeyName          string
	OpenbaoVaultAddr    string
	OpenbaoVaultToken   string
	OpenbaoVaultKeyname string
}

func Load() Config {
	return Config{
		ListenAddr:               getEnv("LISTEN_ADDR", ":8080"),
		AdminListenAddr:          getEnv("ADMIN_LISTEN_ADDR", ":8081"),
		RedisAddr:                getEnv("REDIS_ADDR", "localhost:6379"),
		RedisUsername:            getEnv("REDIS_USERNAME", "ostiary"),
		RedisPassword:            getEnv("REDIS_PASSWORD", "redis-password-change-me"),
		RedisPoolSize:            getEnvInt("REDIS_POOL_SIZE", 5000),
		AdminRedisPoolSize:       getEnvInt("ADMIN_REDIS_POOL_SIZE", 10),
		PostgreAddr:              getEnv("POSTGRE_ADDR", "postgres://postgres:postgres-password-change-me@localhost:5432/ostiary?sslmode=disable"),
		PostgreAutoMigrate:       getEnvBool("POSTGRE_AUTO_MIGRATE", true),
		PostgreMaxOpenConns:      getEnvInt("POSTGRE_MAX_OPEN_CONNS", 75),
		PostgreMaxIdleConns:      getEnvInt("POSTGRE_MAX_IDLE_CONNS", 75),
		AdminPostgreMaxOpenConns: getEnvInt("ADMIN_POSTGRE_MAX_OPEN_CONNS", 10),
		AdminPostgreMaxIdleConns: getEnvInt("ADMIN_POSTGRE_MAX_IDLE_CONNS", 5),
		CORSAllowedOrigin:        getEnv("CORS_ALLOWED_ORIGIN", "http://localhost:3001"),
		// "lax" is enough when the panel and API share a root domain. Use
		// "none" if they're deployed on unrelated domains.
		CookieSameSite: getEnvSameSite("COOKIE_SAMESITE", http.SameSiteLaxMode),
		Environment:    getEnv("ENV", "development"),
		// Enable only once this is actually reachable over HTTPS end to
		// end, directly or behind a reverse proxy terminating TLS.
		HSTSEnabled:      getEnvBool("HSTS_ENABLED", false),
		HealthCheckToken: getEnv("HEALTH_CHECK_TOKEN", "health-check-token-change-me"),
		// What Prometheus sends in x-api-key to read /metrics.
		MetricsToken: getEnv("METRICS_TOKEN", "metrics-token-change-me"),
		// Both empty disables shipping logs to Seq entirely — local stderr
		// logging is unaffected either way. Must be the full CLEF ingestion
		// endpoint (e.g. ".../ingest/clef"), not just the server's base URL.
		SeqURL:    getEnv("SEQ_URL", ""),
		SeqAPIKey: getEnv("SEQ_API_KEY", ""),
		// How long the gateway keeps a session entry in memory in front of
		// Redis. Also the longest a revocation takes to apply to an instance,
		// since invalidation only deletes from Redis. 0 disables the layer.
		LocalCacheTTL: getEnvDuration("LOCAL_CACHE_TTL", 5*time.Minute),
		// Idle connections to the same upstream provider host above this cap
		// get closed instead of pooled — too low under heavy concurrent load
		// to a shared provider key just means more TCP/TLS handshake churn,
		// not failed requests (Go logs it as "too many idle connections").
		ProviderMaxIdleConns:        getEnvInt("PROVIDER_MAX_IDLE_CONNS", 2000),
		ProviderMaxIdleConnsPerHost: getEnvInt("PROVIDER_MAX_IDLE_CONNS_PER_HOST", 2000),
		// The base URL every model of the provider is reached at. Provider
		// keys are sent there.
		OpenAIBaseURL:    getEnv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		AnthropicBaseURL: getEnv("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		GeminiBaseURL:    getEnv("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta"),
		DeepSeekBaseURL:  getEnv("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		// How many usage tasks each gateway writes to Postgres at once. Each
		// one holds a Postgres connection while it runs.
		UsageWorkerConcurrency: getEnvInt("USAGE_WORKER_CONCURRENCY", 10),
		// How many times a usage task that fails to write is retried before
		// asynq archives it. The waits between retries grow, so the default
		// keeps retrying for weeks.
		UsageTaskMaxRetry: getEnvInt("USAGE_TASK_MAX_RETRY", 25),
		// How long a provider's answer is still read, for its usage, after the
		// client that asked for it has gone away.
		UsageDrainLimit: getEnvDuration("USAGE_DRAIN_LIMIT", time.Minute),
		JWTAuthentication: JWTAuthentication{
			Secret:            getEnv("JWT_SECRET", "jwt-secret-change-me"),
			ExpirationInHours: getEnvInt("JWT_EXPIRATION_HOURS", 48),
		},
		KMS: KMS{
			Provider:            getEnv("KMS_PROVIDER", "local"),
			GCPKeyName:          getEnv("GCP_KMS_KEY_NAME", ""),
			OpenbaoVaultAddr:    getEnv("OPENBAO_VAULT_ADDR", ""),
			OpenbaoVaultToken:   getEnv("OPENBAO_VAULT_TOKEN", ""),
			OpenbaoVaultKeyname: getEnv("OPENBAO_VAULT_KEYNAME", ""),
		},
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}

	return b
}

func getEnvSameSite(key string, fallback http.SameSite) http.SameSite {
	switch strings.ToLower(os.Getenv(key)) {
	case "lax":
		return http.SameSiteLaxMode
	case "none":
		return http.SameSiteNoneMode
	case "strict":
		return http.SameSiteStrictMode
	default:
		return fallback
	}
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}

	return d
}

func getEnvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}

	return n
}
