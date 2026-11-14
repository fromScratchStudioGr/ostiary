package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/anthropic"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/deepseek"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/gemini"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/openai"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"
)

const (
	healthToken  = "health-secret"
	metricsToken = "metrics-secret"
)

// writeServiceAccountKey writes a service account key file made of a freshly
// generated RSA key, so no real credential is involved, and returns its path.
// The GCP client only parses it when it's created: it asks for a token on
// first use.
func writeServiceAccountKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	credentials, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": "test-key-id",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "kms@test-project.iam.gserviceaccount.com",
		"client_id":      "123456789",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshaling credentials: %v", err)
	}
	path := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(path, credentials, 0o600); err != nil {
		t.Fatalf("writing credentials: %v", err)
	}
	return path
}

// useLocalKMS gives the local KMS the master key it reads from the environment.
func useLocalKMS(t *testing.T) {
	t.Helper()
	t.Setenv("LOCAL_KMS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// gatewayConfig returns a config for the test Postgres, in a schema of its
// own, and the test Redis, with the local KMS set up.
func gatewayConfig(t *testing.T) config.Config {
	t.Helper()
	redisAddr, _ := testutil.Redis(t)

	// testutil.Postgres makes a fresh, migrated schema. The gateway is pointed
	// at that same schema, so its own migration and queries stay inside it.
	db := testutil.Postgres(t)
	var schema string
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatalf("finding the test schema: %v", err)
	}
	dsn, err := url.Parse(testutil.PostgresDSN(t))
	if err != nil {
		t.Fatalf("parsing %s: %v", testutil.PostgresEnv, err)
	}
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()

	useLocalKMS(t)
	return config.Config{
		RedisAddr:                   redisAddr,
		RedisPoolSize:               5,
		LocalCacheTTL:               time.Minute,
		PostgreAddr:                 dsn.String(),
		PostgreAutoMigrate:          true,
		PostgreMaxOpenConns:         5,
		PostgreMaxIdleConns:         5,
		HealthCheckToken:            healthToken,
		MetricsToken:                metricsToken,
		ProviderMaxIdleConns:        10,
		ProviderMaxIdleConnsPerHost: 10,
		OpenAIBaseURL:               "https://api.openai.com/v1",
		AnthropicBaseURL:            "https://api.anthropic.com",
		GeminiBaseURL:               "https://generativelanguage.googleapis.com/v1beta",
		DeepSeekBaseURL:             "https://api.deepseek.com",
		KMS:                         config.KMS{Provider: "local"},
	}
}

// start bootstraps the gateway without its usage worker (which would take
// tasks from the other tests sharing this Redis) and serves it.
func start(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()
	handler, closeGateway, err := bootstrap(cfg, false)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(closeGateway)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// send makes a request to the server and returns the response and its body.
func send(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestBootstrapNeedsPostgresAndRedis(t *testing.T) {
	cfg := gatewayConfig(t)

	tests := []struct {
		name   string
		change func(*config.Config)
		want   string
	}{
		{
			"Postgres can't be reached",
			func(c *config.Config) {
				c.PostgreAddr = "postgres://postgres:test@127.0.0.1:1/ostiary?sslmode=disable&connect_timeout=2"
			},
			"could not connect to Postgres",
		},
		{"no Postgres address is configured", func(c *config.Config) { c.PostgreAddr = "" }, "could not connect to Postgres"},
		{"Redis can't be reached", func(c *config.Config) { c.RedisAddr = "127.0.0.1:1" }, "could not connect to Redis"},
		{"the KMS can't be set up", func(c *config.Config) { c.KMS = config.KMS{Provider: "openbao"} }, "could not initialize KMS"},
		{"a provider's base URL is invalid", func(c *config.Config) { c.AnthropicBaseURL = "api.anthropic.com" }, "could not register the anthropic models"},
	}
	for _, tt := range tests {
		t.Run("fails, and says why, when "+tt.name, func(t *testing.T) {
			broken := cfg
			tt.change(&broken)

			handler, closeGateway, err := bootstrap(broken, false)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one containing %q", err, tt.want)
			}
			if handler != nil || closeGateway != nil {
				t.Error("returned a handler or closer alongside the error")
			}
		})
	}
}

func TestBootstrapWithEachKMS(t *testing.T) {
	tests := []struct {
		name    string
		kms     config.KMS
		prepare func(t *testing.T)
	}{
		{"local", config.KMS{Provider: "local"}, func(*testing.T) {}},
		{
			"gcp", config.KMS{Provider: "gcp", GCPKeyName: "projects/p/locations/l/keyRings/r/cryptoKeys/k"},
			func(t *testing.T) { t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeServiceAccountKey(t)) },
		},
		{
			"openbao",
			config.KMS{Provider: "openbao", OpenbaoVaultAddr: "http://127.0.0.1:8200", OpenbaoVaultToken: "token", OpenbaoVaultKeyname: "key"},
			func(*testing.T) {},
		},
	}
	for _, tt := range tests {
		t.Run("starts with the "+tt.name+" KMS", func(t *testing.T) {
			cfg := gatewayConfig(t)
			tt.prepare(t)
			cfg.KMS = tt.kms

			srv := start(t, cfg)

			resp, _ := send(t, srv, http.MethodGet, "/health", map[string]string{"x-api-key": healthToken})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("health status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

func TestBootstrapSetsProviderBaseURLs(t *testing.T) {
	cfg := gatewayConfig(t)
	cfg.OpenAIBaseURL = "http://openai.test/v1"
	cfg.AnthropicBaseURL = "http://anthropic.test"
	cfg.GeminiBaseURL = "http://gemini.test/v1beta"
	cfg.DeepSeekBaseURL = "http://deepseek.test"

	start(t, cfg)

	total := 0
	for _, catalog := range []struct {
		models  map[string]providers.Model
		baseURL string
	}{
		{openai.OpenAIModels, cfg.OpenAIBaseURL},
		{anthropic.AnthropicModels, cfg.AnthropicBaseURL},
		{gemini.GeminiModels, cfg.GeminiBaseURL},
		{deepseek.DeepSeekModels, cfg.DeepSeekBaseURL},
	} {
		for id := range catalog.models {
			total++
			model, ok := providers.Models[id]
			if !ok {
				t.Errorf("%s isn't registered", id)
			} else if model.BaseUrl != catalog.baseURL {
				t.Errorf("%s base URL = %q, want %q", id, model.BaseUrl, catalog.baseURL)
			}
		}
	}
	if len(providers.Models) != total {
		t.Errorf("%d models are registered, but the catalogs hold %d", len(providers.Models), total)
	}
}

func TestGatewayEndpoints(t *testing.T) {
	srv := start(t, gatewayConfig(t))

	t.Run("serves Prometheus metrics to a caller with the metrics token, and turns anyone else away", func(t *testing.T) {
		for name, headers := range map[string]map[string]string{
			"without a token":       nil,
			"with the health token": {"x-api-key": healthToken},
		} {
			if resp, _ := send(t, srv, http.MethodGet, "/metrics", headers); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
			}
		}

		resp, body := send(t, srv, http.MethodGet, "/metrics", map[string]string{"x-api-key": metricsToken})

		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "gateway_cache_lookups_total") {
			t.Errorf("GET /metrics = %d, body without the gateway's metrics", resp.StatusCode)
		}
	})

	t.Run("reports health to a caller with the token, and turns anyone else away", func(t *testing.T) {
		if resp, _ := send(t, srv, http.MethodGet, "/health", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("without a token: status = %d, want 401", resp.StatusCode)
		}
		if resp, _ := send(t, srv, http.MethodGet, "/health", map[string]string{"x-api-key": "wrong"}); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("with a wrong token: status = %d, want 401", resp.StatusCode)
		}

		resp, body := send(t, srv, http.MethodGet, "/health", map[string]string{"x-api-key": healthToken})

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("with the token: status = %d, want 200: %s", resp.StatusCode, body)
		}
		var report struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		if err := json.Unmarshal([]byte(body), &report); err != nil {
			t.Fatalf("decoding the report: %v", err)
		}
		if report.Status != "ok" || report.Checks["postgres"] != "ok" || report.Checks["redis"] != "ok" {
			t.Errorf("report = %+v, want ok with postgres and redis both ok", report)
		}
	})

	t.Run("serves the chat endpoints, behind an API key", func(t *testing.T) {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/interactions", "/v1beta/interactions"} {
			t.Run(path, func(t *testing.T) {
				resp, body := send(t, srv, http.MethodPost, path, nil)

				var got api.ErrorResponse
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatalf("decoding the error: %v (%s)", err, body)
				}
				if resp.StatusCode != http.StatusUnauthorized || got.Error != "Missing access token" {
					t.Errorf("POST %s = %d %q, want 401 \"Missing access token\"", path, resp.StatusCode, got.Error)
				}
			})
		}
	})

	t.Run("only answers the methods each endpoint is for, and nothing at other paths", func(t *testing.T) {
		tests := []struct {
			method, path string
			status       int
		}{
			{http.MethodGet, "/v1/messages", http.StatusMethodNotAllowed},
			{http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed},
			{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
			{http.MethodPost, "/health", http.StatusMethodNotAllowed},
			{http.MethodGet, "/", http.StatusNotFound},
			{http.MethodPost, "/v1/unknown", http.StatusNotFound},
		}
		for _, tt := range tests {
			t.Run(tt.method+" "+tt.path, func(t *testing.T) {
				if resp, _ := send(t, srv, tt.method, tt.path, nil); resp.StatusCode != tt.status {
					t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
				}
			})
		}
	})

	t.Run("lets any origin call it from a browser, without credentials", func(t *testing.T) {
		resp, _ := send(t, srv, http.MethodOptions, "/v1/messages", map[string]string{
			"Origin":                        "https://app.example",
			"Access-Control-Request-Method": "POST",
		})

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("preflight status = %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want none: * and credentials don't mix", got)
		}
	})

	t.Run("sends HSTS only when it's enabled", func(t *testing.T) {
		resp, _ := send(t, srv, http.MethodGet, "/metrics", nil)
		if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS = %q with it disabled, want none", got)
		}

		cfg := gatewayConfig(t)
		cfg.HSTSEnabled = true
		hsts := start(t, cfg)

		resp, _ = send(t, hsts, http.MethodGet, "/metrics", nil)
		if got := resp.Header.Get("Strict-Transport-Security"); got == "" {
			t.Error("no HSTS header with it enabled")
		}
	})
}
