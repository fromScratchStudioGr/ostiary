package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database"
	"github.com/fromScratchStudioGr/ostiary/internal/httpx"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"
	"github.com/fromScratchStudioGr/ostiary/internal/logging"

	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var validate = validator.New()

// version is the release this binary was built from, set at build time with
// -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// Loads .env from the project root into the process environment, if present.
	// Safe to skip in prod, where real env vars are set some other way.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		slog.Error("could not load .env", "error", err)
		os.Exit(1)
	}

	cfg := config.Load()

	closeLogging := logging.Setup(cfg.SeqURL, cfg.SeqAPIKey)
	defer closeLogging()

	handler, closeAdmin, err := bootstrap(cfg)
	if err != nil {
		slog.Error("could not start the admin panel", "error", err)
		os.Exit(1)
	}
	defer closeAdmin()

	slog.Info("listening", "addr", cfg.AdminListenAddr, "version", version)
	if err := http.ListenAndServe(cfg.AdminListenAddr, handler); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// bootstrap connects everything the admin panel needs (Postgres, a KMS and
// Redis), makes sure there is an account to log in with, and returns the HTTP
// handler to serve, plus a func that releases it all. When it fails, nothing
// it opened is left open.
func bootstrap(cfg config.Config) (http.Handler, func(), error) {
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	fail := func(what string, err error) (http.Handler, func(), error) {
		closeAll()
		return nil, nil, fmt.Errorf("%s: %w", what, err)
	}

	// Migrations are being controlled by the gateway itself
	db, err := database.Connect(cfg.PostgreAddr, false, cfg.AdminPostgreMaxOpenConns, cfg.AdminPostgreMaxIdleConns)
	if err != nil {
		return fail("could not connect to Postgres", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fail("could not get underlying Postgres connection", err)
	}
	closers = append(closers, func() { sqlDB.Close() })

	if err := seedDefaultAdmin(db); err != nil {
		return fail("could not seed default admin", err)
	}

	// Needed to seal newly registered provider keys (registerKeyHandler).
	kms, closeKMS, err := keystore.New(cfg.KMS)
	if err != nil {
		return fail("could not initialize KMS", err)
	}
	closers = append(closers, closeKMS)

	redis, err := cache.New(cfg.RedisAddr, cfg.RedisUsername, cfg.RedisPassword, cfg.AdminRedisPoolSize, 0, db, kms)
	if err != nil {
		return fail("could not connect to Redis", err)
	}
	closers = append(closers, func() { redis.Close() })

	mux := http.NewServeMux()

	mux.HandleFunc("GET /metrics", httpx.RequireToken(cfg.MetricsToken, promhttp.Handler().ServeHTTP))
	mux.HandleFunc("GET /health", httpx.RequireToken(cfg.HealthCheckToken, httpx.HealthHandler(version,
		httpx.HealthCheck{Name: "postgres", Check: func(ctx context.Context) error {
			return sqlDB.PingContext(ctx)
		}},
		httpx.HealthCheck{Name: "redis", Check: redis.Ping},
	)))

	mux.HandleFunc("POST /login", auth.LoginHandler(cfg, db))
	mux.HandleFunc("POST /logout", auth.LogoutHandler(cfg))
	mux.HandleFunc("GET /me", meHandler(cfg, db))
	mux.HandleFunc("POST /register", registerHandler(cfg, db))

	// Provider keys
	mux.HandleFunc("GET /provider-keys", getProviderKeysHandler(cfg, db))
	mux.HandleFunc("GET /provider-keys/{id}/consumers", getProviderKeyConsumersHandler(cfg, db))
	mux.HandleFunc("POST /provider-keys", registerKeyHandler(cfg, db, kms))
	mux.HandleFunc("DELETE /provider-keys/{id}", deleteProviderKeyHandler(cfg, db, redis))
	mux.HandleFunc("POST /provider-keys/rewrap", rewrapProviderKeysHandler(cfg, db, kms, redis))
	mux.HandleFunc("POST /provider-keys/assign", assignKeyHandler(cfg, db, redis))
	mux.HandleFunc("PATCH /provider-keys/status", updateProviderKeyStatusHandler(cfg, db, redis))
	mux.HandleFunc("PATCH /provider-keys/consumers/status", updateProviderKeyConsumerStatusHandler(cfg, db, redis))
	mux.HandleFunc("DELETE /provider-keys/consumers", deleteProviderKeyConsumerHandler(cfg, db, redis))
	mux.HandleFunc("PATCH /provider-keys/consumers/usage", updateProviderKeyConsumerLimitHandler(cfg, db, redis))

	// Usage details
	mux.HandleFunc("GET /usage", getGeneralUsageHandler(cfg, db))
	mux.HandleFunc("GET /usage/consumers/{id}", getConsumerUsageHandler(cfg, db))

	// Admins
	mux.HandleFunc("GET /users", getUsersHandler(cfg, db))
	mux.HandleFunc("PATCH /users/password", updateUserPasswordHandler(cfg, db))
	mux.HandleFunc("DELETE /users/{id}", deleteUserHandler(cfg, db))

	// Consumers
	mux.HandleFunc("POST /consumers", createConsumerHandler(cfg, db))
	mux.HandleFunc("GET /consumers", getConsumersHandler(cfg, db))
	mux.HandleFunc("GET /consumers/{id}/provider-keys", getConsumerProviderKeysHandler(cfg, db))
	mux.HandleFunc("GET /consumers/{id}/keys", getConsumerGatewayAPIKeysHandler(cfg, db))
	mux.HandleFunc("PATCH /consumers/status", updateConsumerStatusHandler(cfg, db, redis))
	mux.HandleFunc("DELETE /consumers/{id}", deleteConsumerHandler(cfg, db, redis))

	// Gateway API keys
	mux.HandleFunc("POST /keys", createGatewayAPIKeyHandler(cfg, db))
	mux.HandleFunc("PATCH /keys/status", updateGatewayAPIKeyStatusHandler(cfg, db, redis))

	protected := httpx.WithHSTS(cfg.HSTSEnabled, httpx.WithCORS(cfg.CORSAllowedOrigin, true, httpx.RequireSameOrigin(cfg.CORSAllowedOrigin, mux)))

	return protected, closeAll, nil
}
