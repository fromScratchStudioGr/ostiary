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

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"gorm.io/gorm"
)

const (
	healthToken   = "health-secret"
	metricsToken  = "metrics-secret"
	allowedOrigin = "https://admin.example"
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

// adminConfig returns a config for the test Postgres, in a schema of its own,
// and the test Redis, with the local KMS set up.
func adminConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, _ := adminSetup(t)
	return cfg
}

// adminSetup is adminConfig, and also returns the test schema's connection.
func adminSetup(t *testing.T) (config.Config, *gorm.DB) {
	t.Helper()
	redisAddr, _ := testutil.Redis(t)

	// testutil.Postgres makes a fresh, migrated schema. The admin panel is
	// pointed at that same schema, so its queries stay inside it.
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

	t.Setenv("LOCAL_KMS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	return config.Config{
		RedisAddr:                redisAddr,
		AdminRedisPoolSize:       5,
		PostgreAddr:              dsn.String(),
		AdminPostgreMaxOpenConns: 5,
		AdminPostgreMaxIdleConns: 5,
		HealthCheckToken:         healthToken,
		MetricsToken:             metricsToken,
		CORSAllowedOrigin:        allowedOrigin,
		CookieSameSite:           http.SameSiteLaxMode,
		JWTAuthentication:        config.JWTAuthentication{Secret: "jwt-secret", ExpirationInHours: 1},
		KMS:                      config.KMS{Provider: "local"},
	}, db
}

// start bootstraps the admin panel and serves it.
func start(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()
	handler, closeAdmin, err := bootstrap(cfg)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(closeAdmin)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// send makes a request to the server and returns the response and its body.
func send(t *testing.T, srv *httptest.Server, method, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
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
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestBootstrapNeedsPostgresAndRedis(t *testing.T) {
	cfg := adminConfig(t)

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
	}
	for _, tt := range tests {
		t.Run("fails, and says why, when "+tt.name, func(t *testing.T) {
			broken := cfg
			tt.change(&broken)

			handler, closeAdmin, err := bootstrap(broken)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one containing %q", err, tt.want)
			}
			if handler != nil || closeAdmin != nil {
				t.Error("returned a handler or closer alongside the error")
			}
		})
	}

	t.Run("fails when the database has not been migrated by the gateway yet", func(t *testing.T) {
		cfg, db := adminSetup(t)
		if err := db.Migrator().DropTable(&models.User{}); err != nil {
			t.Fatal(err)
		}

		handler, closeAdmin, err := bootstrap(cfg)

		if err == nil || !strings.Contains(err.Error(), "could not seed default admin") {
			t.Errorf("error = %v, want one containing %q", err, "could not seed default admin")
		}
		if handler != nil || closeAdmin != nil {
			t.Error("returned a handler or closer alongside the error")
		}
	})
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
			cfg := adminConfig(t)
			tt.prepare(t)
			cfg.KMS = tt.kms

			srv := start(t, cfg)

			resp, _ := send(t, srv, http.MethodGet, "/health", "", map[string]string{"x-api-key": healthToken})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("health status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

func TestAdminEndpoints(t *testing.T) {
	srv := start(t, adminConfig(t))

	t.Run("serves Prometheus metrics to a caller with the metrics token, and turns anyone else away", func(t *testing.T) {
		for name, headers := range map[string]map[string]string{
			"without a token":       nil,
			"with the health token": {"x-api-key": healthToken},
		} {
			if resp, _ := send(t, srv, http.MethodGet, "/metrics", "", headers); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
			}
		}

		resp, body := send(t, srv, http.MethodGet, "/metrics", "", map[string]string{"x-api-key": metricsToken})

		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "go_goroutines") {
			t.Errorf("GET /metrics = %d, body without metrics", resp.StatusCode)
		}
	})

	t.Run("reports health to a caller with the token, and turns anyone else away", func(t *testing.T) {
		if resp, _ := send(t, srv, http.MethodGet, "/health", "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("without a token: status = %d, want 401", resp.StatusCode)
		}

		resp, body := send(t, srv, http.MethodGet, "/health", "", map[string]string{"x-api-key": healthToken})

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

	t.Run("every route except login, logout, health and metrics needs a session", func(t *testing.T) {
		routes := []struct{ method, path string }{
			{http.MethodGet, "/me"},
			{http.MethodPost, "/register"},
			{http.MethodGet, "/provider-keys"},
			{http.MethodGet, "/provider-keys/1/consumers"},
			{http.MethodPost, "/provider-keys"},
			{http.MethodDelete, "/provider-keys/1"},
			{http.MethodPost, "/provider-keys/rewrap"},
			{http.MethodPost, "/provider-keys/assign"},
			{http.MethodPatch, "/provider-keys/status"},
			{http.MethodPatch, "/provider-keys/consumers/status"},
			{http.MethodDelete, "/provider-keys/consumers"},
			{http.MethodPatch, "/provider-keys/consumers/usage"},
			{http.MethodGet, "/usage"},
			{http.MethodGet, "/usage/consumers/1"},
			{http.MethodGet, "/users"},
			{http.MethodPatch, "/users/password"},
			{http.MethodDelete, "/users/1"},
			{http.MethodPost, "/consumers"},
			{http.MethodGet, "/consumers"},
			{http.MethodGet, "/consumers/1/provider-keys"},
			{http.MethodGet, "/consumers/1/keys"},
			{http.MethodPatch, "/consumers/status"},
			{http.MethodDelete, "/consumers/1"},
			{http.MethodPost, "/keys"},
			{http.MethodPatch, "/keys/status"},
		}
		for _, route := range routes {
			t.Run(route.method+" "+route.path, func(t *testing.T) {
				resp, body := send(t, srv, route.method, route.path, "", nil)

				var got api.ErrorResponse
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatalf("decoding the error: %v (%s)", err, body)
				}
				if resp.StatusCode != http.StatusUnauthorized || got.Error != "Missing access token" {
					t.Errorf("%s %s = %d %q, want 401 \"Missing access token\"", route.method, route.path, resp.StatusCode, got.Error)
				}
			})
		}
	})

	t.Run("lets the seeded admin log in, and the session it gets works until logout", func(t *testing.T) {
		resp, body := send(t, srv, http.MethodPost, "/login", `{"username":"admin","password":"admin"}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login = %d: %s", resp.StatusCode, body)
		}
		var session *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == auth.SessionCookieName {
				session = c
			}
		}
		if session == nil {
			t.Fatal("login set no session cookie")
		}
		cookie := map[string]string{"Cookie": session.Name + "=" + session.Value}

		resp, body = send(t, srv, http.MethodGet, "/me", "", cookie)

		var me api.MeResponse
		if err := json.Unmarshal([]byte(body), &me); err != nil {
			t.Fatalf("decoding /me: %v (%s)", err, body)
		}
		if resp.StatusCode != http.StatusOK || me.Username != "admin" {
			t.Errorf("GET /me = %d %+v, want 200 for admin", resp.StatusCode, me)
		}

		resp, _ = send(t, srv, http.MethodPost, "/logout", "", cookie)
		var cleared bool
		for _, c := range resp.Cookies() {
			if c.Name == auth.SessionCookieName && c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Error("logout did not clear the session cookie")
		}
	})

	t.Run("only answers the methods each endpoint is for, and nothing at other paths", func(t *testing.T) {
		tests := []struct {
			method, path string
			status       int
		}{
			{http.MethodGet, "/login", http.StatusMethodNotAllowed},
			{http.MethodPost, "/me", http.StatusMethodNotAllowed},
			{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
			{http.MethodPut, "/consumers", http.StatusMethodNotAllowed},
			{http.MethodGet, "/", http.StatusNotFound},
			{http.MethodGet, "/unknown", http.StatusNotFound},
		}
		for _, tt := range tests {
			t.Run(tt.method+" "+tt.path, func(t *testing.T) {
				if resp, _ := send(t, srv, tt.method, tt.path, "", nil); resp.StatusCode != tt.status {
					t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
				}
			})
		}
	})

	t.Run("lets only the configured origin call it from a browser, with credentials", func(t *testing.T) {
		resp, _ := send(t, srv, http.MethodOptions, "/consumers", "", map[string]string{
			"Origin":                        allowedOrigin,
			"Access-Control-Request-Method": "POST",
		})

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("preflight status = %d, want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != allowedOrigin {
			t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, allowedOrigin)
		}
		if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want true: the session is a cookie", got)
		}
	})

	t.Run("refuses a state-changing request from another origin, but not a read", func(t *testing.T) {
		resp, _ := send(t, srv, http.MethodPost, "/login", `{"username":"admin","password":"admin"}`, map[string]string{"Origin": "https://evil.example"})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST /login from another origin = %d, want 403", resp.StatusCode)
		}

		resp, _ = send(t, srv, http.MethodPost, "/login", `{"username":"admin","password":"admin"}`, map[string]string{"Origin": allowedOrigin})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST /login from the allowed origin = %d, want 200", resp.StatusCode)
		}

		resp, _ = send(t, srv, http.MethodGet, "/metrics", "", map[string]string{"Origin": "https://evil.example", "x-api-key": metricsToken})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /metrics from another origin = %d, want 200: reads are exempt", resp.StatusCode)
		}
	})

	t.Run("sends HSTS only when it's enabled", func(t *testing.T) {
		resp, _ := send(t, srv, http.MethodGet, "/metrics", "", nil)
		if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS = %q with it disabled, want none", got)
		}

		cfg := adminConfig(t)
		cfg.HSTSEnabled = true
		hsts := start(t, cfg)

		resp, _ = send(t, hsts, http.MethodGet, "/metrics", "", nil)
		if got := resp.Header.Get("Strict-Transport-Security"); got == "" {
			t.Error("no HSTS header with it enabled")
		}
	})
}
