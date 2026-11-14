package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database"
	"github.com/fromScratchStudioGr/ostiary/internal/httpx"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"
	"github.com/fromScratchStudioGr/ostiary/internal/logging"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/anthropic"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/deepseek"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/gemini"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/openai"
	"github.com/fromScratchStudioGr/ostiary/internal/usage"

	"github.com/hibiken/asynq"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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

	handler, closeGateway, err := bootstrap(cfg, true)
	if err != nil {
		slog.Error("could not start the gateway", "error", err)
		os.Exit(1)
	}
	defer closeGateway()

	slog.Info("listening", "addr", cfg.ListenAddr, "version", version)
	if err := http.ListenAndServe(cfg.ListenAddr, handler); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// bootstrap connects everything the gateway needs (Postgres, a KMS and Redis)
// and returns the HTTP handler to serve, plus a func that releases it all.
// When it fails, nothing it opened is left open.
//
// withWorker starts the in-process consumer of the usage queue. It's a switch
// so that a test can bootstrap the gateway on a shared Redis without that
// consumer taking tasks that belong to other tests.
func bootstrap(cfg config.Config, withWorker bool) (http.Handler, func(), error) {
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

	providers.Reset()
	for provider, catalog := range map[string]struct {
		models  map[string]providers.Model
		baseURL string
	}{
		"openai":    {openai.OpenAIModels, cfg.OpenAIBaseURL},
		"anthropic": {anthropic.AnthropicModels, cfg.AnthropicBaseURL},
		"gemini":    {gemini.GeminiModels, cfg.GeminiBaseURL},
		"deepseek":  {deepseek.DeepSeekModels, cfg.DeepSeekBaseURL},
	} {
		if err := providers.RegisterCatalog(catalog.models, catalog.baseURL); err != nil {
			return fail("could not register the "+provider+" models", err)
		}
	}

	db, err := database.Connect(cfg.PostgreAddr, cfg.PostgreAutoMigrate, cfg.PostgreMaxOpenConns, cfg.PostgreMaxIdleConns)
	if err != nil {
		return fail("could not connect to Postgres", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fail("could not get underlying Postgres connection", err)
	}
	closers = append(closers, func() { sqlDB.Close() })

	kms, closeKMS, err := keystore.New(cfg.KMS)
	if err != nil {
		return fail("could not initialize KMS", err)
	}
	closers = append(closers, closeKMS)

	redis, err := cache.New(cfg.RedisAddr, cfg.RedisUsername, cfg.RedisPassword, cfg.RedisPoolSize, cfg.LocalCacheTTL, db, kms)
	if err != nil {
		return fail("could not connect to Redis", err)
	}
	closers = append(closers, func() { redis.Close() })

	// Enqueue side: LogUsage hands usage records off here instead of writing
	// them directly, so a slow DB write can't add latency to the proxied
	// request.
	asynqClient := asynq.NewClient(asynq.RedisClientOpt{Addr: cfg.RedisAddr, Username: cfg.RedisUsername, Password: cfg.RedisPassword})
	closers = append(closers, func() { asynqClient.Close() })

	if withWorker {
		// Processing side: consumes what LogUsage enqueues and does the actual
		// DB write. Runs in-process for now; since asynq is Redis-backed,
		// multiple gateway instances could each run one of these against the
		// same queue without any extra coordination.
		asynqServer := asynq.NewServer(
			asynq.RedisClientOpt{Addr: cfg.RedisAddr, Username: cfg.RedisUsername, Password: cfg.RedisPassword},
			asynq.Config{
				Concurrency:    cfg.UsageWorkerConcurrency,
				RetryDelayFunc: asynq.DefaultRetryDelayFunc,
			},
		)

		asynqMux := asynq.NewServeMux()
		asynqMux.HandleFunc(usage.TaskTypeLogUsage, usage.HandleLogUsageTask(db, redis))
		if err := asynqServer.Start(asynqMux); err != nil {
			return fail("could not start asynq server", err)
		}
		closers = append(closers, asynqServer.Shutdown)
	}

	// Shared across every proxied request so upstream connections (e.g. to
	// DeepSeek) actually get reused. http.DefaultTransport's
	// MaxIdleConnsPerHost defaults to 2, which causes constant connection
	// churn under concurrent load to the same provider host; a Transport
	// created per-request would have the same problem, since each would
	// get its own isolated idle-connection pool.
	providerTransport := &http.Transport{
		MaxIdleConns:        cfg.ProviderMaxIdleConns,
		MaxIdleConnsPerHost: cfg.ProviderMaxIdleConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /metrics", httpx.RequireToken(cfg.MetricsToken, promhttp.Handler().ServeHTTP))
	mux.HandleFunc("GET /health", httpx.RequireToken(cfg.HealthCheckToken, httpx.HealthHandler(version,
		httpx.HealthCheck{Name: "postgres", Check: func(ctx context.Context) error {
			return sqlDB.PingContext(ctx)
		}},
		httpx.HealthCheck{Name: "redis", Check: redis.Ping},
	)))

	chatHandler := chatCompletionHandler(db, redis, kms, asynqClient, providerTransport, cfg.UsageDrainLimit, cfg.UsageTaskMaxRetry)
	mux.HandleFunc("POST /v1/chat/completions", chatHandler)
	mux.HandleFunc("POST /v1/messages", chatHandler)
	mux.HandleFunc("POST /v1/interactions", chatHandler)
	// Where Google's own library sends interactions requests.
	mux.HandleFunc("POST /v1beta/interactions", chatHandler)

	protected := httpx.WithHSTS(cfg.HSTSEnabled, httpx.WithCORS("*", false, mux))

	return protected, closeAll, nil
}
