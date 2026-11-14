package config

import (
	"net/http"
	"os"
	"testing"
	"time"
)

const testKey = "CONFIG_TEST_VALUE"

// setTestEnv sets testKey to value, or leaves it unset when set is false.
func setTestEnv(t *testing.T, value string, set bool) {
	t.Helper()
	// t.Setenv restores the original value afterwards, so unsetting after it
	// doesn't leak into other tests.
	t.Setenv(testKey, value)
	if !set {
		os.Unsetenv(testKey)
	}
}

func TestGetEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  string
	}{
		{"unset falls back", "", false, "fallback"},
		{"empty falls back", "", true, "fallback"},
		{"set value wins", "custom", true, "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestEnv(t, tt.value, tt.set)
			if got := getEnv(testKey, "fallback"); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  int
	}{
		{"unset falls back", "", false, 7},
		{"empty falls back", "", true, 7},
		{"not a number falls back", "many", true, 7},
		{"padded number falls back", " 5", true, 7},
		{"number wins", "42", true, 42},
		{"zero is a value, not a fallback", "0", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestEnv(t, tt.value, tt.set)
			if got := getEnvInt(testKey, 7); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGetEnvBool(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		set      bool
		fallback bool
		want     bool
	}{
		{"unset keeps a true fallback", "", false, true, true},
		{"unset keeps a false fallback", "", false, false, false},
		{"empty keeps a true fallback", "", true, true, true},
		{"unparseable keeps a true fallback", "maybe", true, true, true},
		{"unparseable keeps a false fallback", "maybe", true, false, false},
		{"false overrides a true fallback", "false", true, true, false},
		{"true overrides a false fallback", "true", true, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestEnv(t, tt.value, tt.set)
			if got := getEnvBool(testKey, tt.fallback); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetEnvDuration(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  time.Duration
	}{
		{"unset falls back", "", false, time.Minute},
		{"empty falls back", "", true, time.Minute},
		{"missing unit falls back", "30", true, time.Minute},
		{"duration wins", "30s", true, 30 * time.Second},
		{"zero is a value, not a fallback", "0", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestEnv(t, tt.value, tt.set)
			if got := getEnvDuration(testKey, time.Minute); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetEnvSameSite(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  http.SameSite
	}{
		{"unset falls back", "", false, http.SameSiteStrictMode},
		{"unknown mode falls back", "relaxed", true, http.SameSiteStrictMode},
		{"lax", "lax", true, http.SameSiteLaxMode},
		{"none", "none", true, http.SameSiteNoneMode},
		{"strict", "strict", true, http.SameSiteStrictMode},
		{"is case-insensitive", "NoNe", true, http.SameSiteNoneMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestEnv(t, tt.value, tt.set)
			// The fallback is Strict, so every non-Strict row proves the
			// value was read rather than defaulted.
			if got := getEnvSameSite(testKey, http.SameSiteStrictMode); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// loadEnv gives every variable Load reads a value unlike its default and
// unlike every other variable's.
var loadEnv = map[string]string{
	"LISTEN_ADDR":                      ":9001",
	"ADMIN_LISTEN_ADDR":                ":9002",
	"REDIS_ADDR":                       "redis.test:6380",
	"REDIS_USERNAME":                   "redis-user",
	"REDIS_PASSWORD":                   "redis-pass",
	"REDIS_POOL_SIZE":                  "11",
	"ADMIN_REDIS_POOL_SIZE":            "12",
	"LOCAL_CACHE_TTL":                  "13s",
	"POSTGRE_ADDR":                     "postgres://pg.test/db",
	"POSTGRE_AUTO_MIGRATE":             "false",
	"POSTGRE_MAX_OPEN_CONNS":           "14",
	"POSTGRE_MAX_IDLE_CONNS":           "15",
	"ADMIN_POSTGRE_MAX_OPEN_CONNS":     "16",
	"ADMIN_POSTGRE_MAX_IDLE_CONNS":     "17",
	"CORS_ALLOWED_ORIGIN":              "https://panel.test",
	"COOKIE_SAMESITE":                  "strict",
	"ENV":                              "production",
	"HSTS_ENABLED":                     "true",
	"HEALTH_CHECK_TOKEN":               "health-token",
	"METRICS_TOKEN":                    "metrics-token",
	"SEQ_URL":                          "https://seq.test/ingest/clef",
	"SEQ_API_KEY":                      "seq-key",
	"PROVIDER_MAX_IDLE_CONNS":          "18",
	"PROVIDER_MAX_IDLE_CONNS_PER_HOST": "19",
	"OPENAI_BASE_URL":                  "http://openai.test:9093/v1",
	"ANTHROPIC_BASE_URL":               "http://anthropic.test:9092",
	"GEMINI_BASE_URL":                  "http://gemini.test:9094/v1beta",
	"DEEPSEEK_BASE_URL":                "http://deepseek.test:9095",
	"USAGE_WORKER_CONCURRENCY":         "21",
	"USAGE_TASK_MAX_RETRY":             "22",
	"USAGE_DRAIN_LIMIT":                "23s",
	"JWT_SECRET":                       "jwt-secret",
	"JWT_EXPIRATION_HOURS":             "20",
	"KMS_PROVIDER":                     "openbao",
	"GCP_KMS_KEY_NAME":                 "projects/p/locations/l/keyRings/r/cryptoKeys/k",
	"OPENBAO_VAULT_ADDR":               "https://bao.test",
	"OPENBAO_VAULT_TOKEN":              "bao-token",
	"OPENBAO_VAULT_KEYNAME":            "bao-key",
}

func TestLoad(t *testing.T) {
	t.Run("reads every setting from its own environment variable", func(t *testing.T) {
		for key, value := range loadEnv {
			t.Setenv(key, value)
		}

		want := Config{
			ListenAddr:                  ":9001",
			AdminListenAddr:             ":9002",
			RedisAddr:                   "redis.test:6380",
			RedisUsername:               "redis-user",
			RedisPassword:               "redis-pass",
			RedisPoolSize:               11,
			AdminRedisPoolSize:          12,
			LocalCacheTTL:               13 * time.Second,
			PostgreAddr:                 "postgres://pg.test/db",
			PostgreAutoMigrate:          false,
			PostgreMaxOpenConns:         14,
			PostgreMaxIdleConns:         15,
			AdminPostgreMaxOpenConns:    16,
			AdminPostgreMaxIdleConns:    17,
			CORSAllowedOrigin:           "https://panel.test",
			CookieSameSite:              http.SameSiteStrictMode,
			Environment:                 "production",
			HSTSEnabled:                 true,
			HealthCheckToken:            "health-token",
			MetricsToken:                "metrics-token",
			SeqURL:                      "https://seq.test/ingest/clef",
			SeqAPIKey:                   "seq-key",
			ProviderMaxIdleConns:        18,
			ProviderMaxIdleConnsPerHost: 19,
			OpenAIBaseURL:               "http://openai.test:9093/v1",
			AnthropicBaseURL:            "http://anthropic.test:9092",
			GeminiBaseURL:               "http://gemini.test:9094/v1beta",
			DeepSeekBaseURL:             "http://deepseek.test:9095",
			UsageWorkerConcurrency:      21,
			UsageTaskMaxRetry:           22,
			UsageDrainLimit:             23 * time.Second,
			JWTAuthentication: JWTAuthentication{
				Secret:            "jwt-secret",
				ExpirationInHours: 20,
			},
			KMS: KMS{
				Provider:            "openbao",
				GCPKeyName:          "projects/p/locations/l/keyRings/r/cryptoKeys/k",
				OpenbaoVaultAddr:    "https://bao.test",
				OpenbaoVaultToken:   "bao-token",
				OpenbaoVaultKeyname: "bao-key",
			},
		}

		if got := Load(); got != want {
			t.Errorf("Load() = %+v\nwant     %+v", got, want)
		}
	})

	t.Run("falls back to the documented defaults when nothing is configured", func(t *testing.T) {
		// Empty counts as unset for every setting, which isolates the test
		// from whatever the machine running it has exported.
		for key := range loadEnv {
			t.Setenv(key, "")
		}

		want := Config{
			ListenAddr:                  ":8080",
			AdminListenAddr:             ":8081",
			RedisAddr:                   "localhost:6379",
			RedisUsername:               "ostiary",
			RedisPassword:               "redis-password-change-me",
			RedisPoolSize:               5000,
			AdminRedisPoolSize:          10,
			LocalCacheTTL:               5 * time.Minute,
			PostgreAddr:                 "postgres://postgres:postgres-password-change-me@localhost:5432/ostiary?sslmode=disable",
			PostgreAutoMigrate:          true,
			PostgreMaxOpenConns:         75,
			PostgreMaxIdleConns:         75,
			AdminPostgreMaxOpenConns:    10,
			AdminPostgreMaxIdleConns:    5,
			CORSAllowedOrigin:           "http://localhost:3001",
			CookieSameSite:              http.SameSiteLaxMode,
			Environment:                 "development",
			HSTSEnabled:                 false,
			HealthCheckToken:            "health-check-token-change-me",
			MetricsToken:                "metrics-token-change-me",
			SeqURL:                      "",
			SeqAPIKey:                   "",
			ProviderMaxIdleConns:        2000,
			ProviderMaxIdleConnsPerHost: 2000,
			OpenAIBaseURL:               "https://api.openai.com/v1",
			AnthropicBaseURL:            "https://api.anthropic.com",
			GeminiBaseURL:               "https://generativelanguage.googleapis.com/v1beta",
			DeepSeekBaseURL:             "https://api.deepseek.com",
			UsageWorkerConcurrency:      10,
			UsageTaskMaxRetry:           25,
			UsageDrainLimit:             time.Minute,
			JWTAuthentication: JWTAuthentication{
				Secret:            "jwt-secret-change-me",
				ExpirationInHours: 48,
			},
			KMS: KMS{
				Provider: "local",
			},
		}

		if got := Load(); got != want {
			t.Errorf("Load() = %+v\nwant     %+v", got, want)
		}
	})
}
