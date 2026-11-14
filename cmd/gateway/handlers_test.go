package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"
	"github.com/fromScratchStudioGr/ostiary/internal/payload"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"
	"github.com/fromScratchStudioGr/ostiary/internal/usage"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// syncBuffer is a bytes.Buffer the handler's background goroutines can write
// to while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs sends slog's default logger to the returned buffer until the
// test ends.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// proxyRequestFor builds the proxy request for body, and the Payload
// payload.Read finds in it.
func proxyRequestFor(t testing.TB, body string) (*httputil.ProxyRequest, payload.Payload) {
	t.Helper()
	var p payload.Payload
	read := payload.Read(func(_ http.ResponseWriter, r *http.Request) {
		p, _ = payload.FromContext(r)
	})
	read(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if p.Body == nil {
		t.Fatalf("payload.Read turned down %s", body)
	}

	in := httptest.NewRequest(http.MethodPost, "http://gateway.local/v1/chat/completions", strings.NewReader(body))
	return &httputil.ProxyRequest{In: in, Out: in.Clone(in.Context())}, p
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("could not read body: %v", err)
	}
	return string(data)
}

func readJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	data := readBody(t, r)
	var members map[string]any
	if err := json.Unmarshal([]byte(data), &members); err != nil {
		t.Fatalf("could not parse body: %v\nbody: %s", err, data)
	}
	return members
}

func TestForceOpenAIStreamUsage(t *testing.T) {
	t.Run("adds stream_options when streaming and none set, with a Content-Length to match", func(t *testing.T) {
		pr, p := proxyRequestFor(t, `{"model":"gpt-6.1-sol","stream":true,"messages":[]}`)
		forceOpenAIStreamUsage(pr, p)

		body := readBody(t, pr.Out)
		if want := `{"model":"gpt-6.1-sol","stream":true,"messages":[],"stream_options":{"include_usage":true}}`; body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
		if length := len(body); pr.Out.ContentLength != int64(length) || pr.Out.Header.Get("Content-Length") != fmt.Sprint(length) {
			t.Errorf("Content-Length = %d / %q, want %d", pr.Out.ContentLength, pr.Out.Header.Get("Content-Length"), length)
		}
	})

	t.Run("replaces a stream_options that isn't an object", func(t *testing.T) {
		pr, p := proxyRequestFor(t, `{"model":"gpt-6.1-sol","stream":true,"stream_options":null,"messages":[]}`)
		forceOpenAIStreamUsage(pr, p)

		if body, want := readBody(t, pr.Out), `{"model":"gpt-6.1-sol","stream":true,"stream_options":{"include_usage":true},"messages":[]}`; body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
	})

	t.Run("overrides an explicit false without dropping other stream_options fields", func(t *testing.T) {
		pr, p := proxyRequestFor(t, `{"model":"gpt-6.1-sol","stream":true,"stream_options":{"include_usage":false,"other_field":"keep-me"},"messages":[]}`)
		forceOpenAIStreamUsage(pr, p)

		got := readJSONBody(t, pr.Out)
		opts := got["stream_options"].(map[string]any)
		if opts["include_usage"] != true {
			t.Fatalf("include_usage = %v, want true", opts["include_usage"])
		}
		if opts["other_field"] != "keep-me" {
			t.Fatalf("other_field = %v, want preserved", opts["other_field"])
		}
	})

	t.Run("keeps the client's other values exactly as sent", func(t *testing.T) {
		pr, p := proxyRequestFor(t, `{"model":"gpt-6.1-sol","stream":true,"seed":9007199254740993,"temperature":1.0,"messages":[{"role":"user","content":"hi"}]}`)
		forceOpenAIStreamUsage(pr, p)

		body := readBody(t, pr.Out)
		// Decoded as a float64, a seed above 2^53 would reach the provider
		// changed.
		for _, want := range []string{`"seed":9007199254740993`, `"temperature":1.0`, `"messages":[{"role":"user","content":"hi"}]`} {
			if !strings.Contains(body, want) {
				t.Errorf("body %s is missing %s", body, want)
			}
		}
	})

	t.Run("leaves a request that doesn't stream untouched", func(t *testing.T) {
		for name, body := range map[string]string{
			"stream false":         `{"model":"gpt-6.1-sol","stream":false,"messages":[]}`,
			"no stream":            `{"model":"gpt-6.1-sol","messages":[]}`,
			"stream not a boolean": `{"model":"gpt-6.1-sol","stream":"true","messages":[]}`,
		} {
			t.Run(name, func(t *testing.T) {
				pr, p := proxyRequestFor(t, body)
				forceOpenAIStreamUsage(pr, p)

				if got := readBody(t, pr.Out); got != body {
					t.Errorf("body = %s, want it unchanged", got)
				}
			})
		}
	})

	t.Run("sends a repeated stream or stream_options key only once, the last copy, which the decision was made on", func(t *testing.T) {
		tests := []struct {
			name, body string
			stream     bool
			options    map[string]any
		}{
			{
				name:    "stream repeated, last true",
				body:    `{"model":"gpt-6.1-sol","stream":false,"stream":true,"messages":[]}`,
				stream:  true,
				options: map[string]any{"include_usage": true},
			},
			{
				// A provider reading the first copy would otherwise stream
				// with no usage.
				name:   "stream repeated, last false",
				body:   `{"model":"gpt-6.1-sol","stream":true,"stream":false,"messages":[]}`,
				stream: false,
			},
			{
				name:    "stream_options repeated",
				body:    `{"model":"gpt-6.1-sol","stream":true,"stream_options":{"include_usage":false},"stream_options":{"include_usage":false,"x":1},"messages":[]}`,
				stream:  true,
				options: map[string]any{"include_usage": true, "x": float64(1)},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				pr, p := proxyRequestFor(t, tt.body)
				forceOpenAIStreamUsage(pr, p)

				body := readBody(t, pr.Out)
				for _, key := range []string{`"stream":`, `"stream_options":`} {
					if n := strings.Count(body, key); n > 1 {
						t.Errorf("body %s has %s %d times, want once at most", body, key, n)
					}
				}
				var got struct {
					Stream        bool           `json:"stream"`
					StreamOptions map[string]any `json:"stream_options"`
				}
				json.Unmarshal([]byte(body), &got)
				if got.Stream != tt.stream || fmt.Sprint(got.StreamOptions) != fmt.Sprint(tt.options) {
					t.Errorf("stream = %v, stream_options = %v; want %v, %v", got.Stream, got.StreamOptions, tt.stream, tt.options)
				}
			})
		}
	})
}

// request is what a fake provider was sent.
type request struct {
	path   string
	header http.Header
	body   []byte
}

// fakeProvider is an upstream LLM API that answers with whatever a test set,
// and remembers what it was asked.
type fakeProvider struct {
	*httptest.Server

	mu           sync.Mutex
	status       int
	contentType  string
	encoding     string
	compressible bool // gzip the body when the request accepts it, as a real provider does
	body         []byte
	calls        int
	last         request
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ := io.ReadAll(r.Body)

		p.mu.Lock()
		p.calls++
		p.last = request{path: r.URL.Path, header: r.Header.Clone(), body: sent}
		status, contentType, encoding, body := p.status, p.contentType, p.encoding, p.body
		compress := p.compressible && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
		p.mu.Unlock()

		if compress {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			zw.Write(body)
			zw.Close()
			body, encoding = buf.Bytes(), "gzip"
		}
		w.Header().Set("Content-Type", contentType)
		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
		}
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *fakeProvider) respond(status int, contentType, body string) {
	p.respondEncoded(status, contentType, "", []byte(body))
}

func (p *fakeProvider) respondEncoded(status int, contentType, encoding string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.contentType, p.encoding, p.body = status, contentType, encoding, body
}

// respondCompressible answers with body, gzipped when the request accepts gzip.
func (p *fakeProvider) respondCompressible(status int, contentType, body string) {
	p.respond(status, contentType, body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compressible = true
}

func (p *fakeProvider) received() (calls int, last request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.last
}

const providerSecret = "sk-provider-secret"

// chatOptions says what kind of provider and model a chatFixture sets up;
// the zero value is an OpenAI-style model on the fake provider.
type chatOptions struct {
	provider   string                             // default "openai"
	endpoint   providers.EndpointType             // default the OpenAI chat-completions endpoint
	auth       providers.AuthenticationHeaderType // default Bearer
	baseURL    string                             // default the fake provider's URL
	localTTL   time.Duration                      // default 0: the in-process cache layer is off
	drainLimit time.Duration                      // default a minute
}

// flakyKMS is the KMS the handler opens provider keys with, and can be made to
// fail from the test.
type flakyKMS struct {
	keystore.KMSClient
	broken atomic.Bool
}

func (k *flakyKMS) Decrypt(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	if k.broken.Load() {
		return nil, errors.New("kms is down")
	}
	return k.KMSClient.Decrypt(ctx, wrapped, keyID)
}

// chatFixture is chatCompletionHandler wired to the real Postgres and Redis,
// with a consumer, an API key and a sealed provider key set up, and a fake
// provider behind the model.
type chatFixture struct {
	db            *gorm.DB
	provider      *fakeProvider
	inspector     *asynq.Inspector
	handler       http.HandlerFunc
	providerKeyID uint
	token         string
	modelID       string
	kms           *flakyKMS
	sessions      *cache.Cache
	redis         *redis.Client
}

// sessionKey is where the fixture's API key keeps its cached session for its
// model in Redis.
func (f *chatFixture) sessionKey() string {
	return "ostiary:cache:" + auth.HashAPIKey(f.token) + ":" + f.modelID
}

func (f *chatFixture) sessionCached(t *testing.T) bool {
	t.Helper()
	n, err := f.redis.Exists(context.Background(), f.sessionKey()).Result()
	if err != nil {
		t.Fatalf("checking the cached session: %v", err)
	}
	return n == 1
}

// highID is for rows whose id ends up in a Redis key shared with other tests:
// their schemas count from 1.
func highID() uint { return uint(rand.Uint32()%1_000_000_000) + 1_000_000 }

func newChatFixture(t *testing.T, opts chatOptions) *chatFixture {
	t.Helper()
	ctx := context.Background()
	if opts.provider == "" {
		opts.provider = "openai"
	}
	if opts.endpoint == "" {
		opts.endpoint = providers.OpenAIEndpoint
	}
	if opts.auth == "" {
		opts.auth = providers.Bearer
	}
	if opts.drainLimit == 0 {
		opts.drainLimit = time.Minute
	}

	redisAddr, raw := testutil.Redis(t)
	db := testutil.Postgres(t)

	t.Setenv("LOCAL_KMS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	kms, err := keystore.NewLocalKMSFromEnv()
	if err != nil {
		t.Fatalf("local KMS: %v", err)
	}
	sessions, err := cache.New(redisAddr, "", "", 5, opts.localTTL, db, kms)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	t.Cleanup(func() { sessions.Close() })

	// Usage tasks go to a Redis database of their own: the usage tests look at
	// the queue in database 0 and mustn't see these.
	asynqOpt := asynq.RedisClientOpt{Addr: redisAddr, DB: 9}
	client := asynq.NewClient(asynqOpt)
	inspector := asynq.NewInspector(asynqOpt)
	clearQueue := func() { inspector.DeleteAllPendingTasks("default") }
	clearQueue()
	t.Cleanup(func() {
		clearQueue()
		inspector.Close()
		client.Close()
	})

	provider := newFakeProvider(t)
	baseURL := provider.URL
	if opts.baseURL != "" {
		baseURL = opts.baseURL
	}
	model := providers.Model{
		Id:                   "test-" + opts.provider + "-" + testutil.Unique()[:10],
		Name:                 "Test model",
		Provider:             opts.provider,
		BaseUrl:              baseURL,
		Endpoint:             opts.endpoint,
		AuthenticationHeader: opts.auth,
	}
	providers.Register(model.Id, model)
	t.Cleanup(func() { delete(providers.Models, model.Id) })

	create := func(value any) {
		t.Helper()
		if err := db.Create(value).Error; err != nil {
			t.Fatalf("creating %T: %v", value, err)
		}
	}
	consumer := models.Consumer{Name: "consumer", Status: "active"}
	create(&consumer)
	ciphertext, nonce, encryptedDEK, kmsKeyID, err := keystore.SealProviderKey(ctx, kms, []byte(providerSecret))
	if err != nil {
		t.Fatalf("sealing the provider key: %v", err)
	}
	providerKey := models.ProviderKey{
		ID: highID(), Provider: opts.provider, Status: "active",
		Ciphertext: ciphertext, Nonce: nonce, EncryptedDEK: encryptedDEK, KMSKeyID: kmsKeyID,
	}
	create(&providerKey)
	create(&models.UserProviderAccess{ConsumerID: consumer.ID, ProviderKeyID: providerKey.ID, Status: "active"})
	token, hash, hint := auth.GenerateAPIKey()
	apiKey := models.APIKey{
		ID: highID(), ConsumerID: consumer.ID, Name: "key", KeyHash: hash, DisplayHint: hint,
		Status: "active", ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	create(&apiKey)

	t.Cleanup(func() {
		raw.Del(ctx,
			"ostiary:cache:"+hash+":"+model.Id,
			fmt.Sprintf("ostiary:lastused:providerkey:%d", providerKey.ID),
			fmt.Sprintf("ostiary:lastused:apikey:%d", apiKey.ID),
		)
	})

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	flaky := &flakyKMS{KMSClient: kms}
	return &chatFixture{
		db:            db,
		provider:      provider,
		inspector:     inspector,
		handler:       chatCompletionHandler(db, sessions, flaky, client, transport, opts.drainLimit, 25),
		providerKeyID: providerKey.ID,
		token:         token,
		modelID:       model.Id,
		kms:           flaky,
		sessions:      sessions,
		redis:         raw,
	}
}

// body is a request body for the fixture's model, with extra JSON fields
// (starting with a comma) after the model.
func (f *chatFixture) body(extra string) string {
	return fmt.Sprintf(`{"model":%q%s,"messages":[]}`, f.modelID, extra)
}

// send makes a request with the fixture's API key, plus any extra headers.
func (f *chatFixture) send(body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	f.handler(rec, req)
	return rec
}

// post sends the plainest request for the fixture's model.
func (f *chatFixture) post() *httptest.ResponseRecorder { return f.send(f.body(""), nil) }

// usageTasks returns the usage records handed on for billing so far.
func (f *chatFixture) usageTasks(t *testing.T) []*asynq.TaskInfo {
	t.Helper()
	tasks, err := f.inspector.ListPendingTasks("default")
	if errors.Is(err, asynq.ErrQueueNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("listing the usage queue: %v", err)
	}
	return tasks
}

// wantOneUsageTask and wantNoUsageTask check what was handed on for billing.
func (f *chatFixture) wantOneUsageTask(t *testing.T) {
	t.Helper()
	if tasks := f.usageTasks(t); len(tasks) != 1 || tasks[0].Type != usage.TaskTypeLogUsage {
		t.Errorf("usage tasks = %v, want one %s", tasks, usage.TaskTypeLogUsage)
	}
}

// eventuallyOneUsageTask waits for a usage record handed on in the background.
func (f *chatFixture) eventuallyOneUsageTask(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.usageTasks(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	f.wantOneUsageTask(t)
}

func (f *chatFixture) wantNoUsageTask(t *testing.T) {
	t.Helper()
	if tasks := f.usageTasks(t); len(tasks) != 0 {
		t.Errorf("%d usage tasks, want none", len(tasks))
	}
}

func wantLogged(t *testing.T, logs *syncBuffer, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q is missing %q", logs.String(), want)
		}
	}
}

func TestChatCompletionHandler(t *testing.T) {
	const withUsage = `{"id":"chatcmpl-1","usage":{"prompt_tokens":12,"completion_tokens":5}}`

	t.Run("forwards the request with the provider's key, returns the provider's answer, and hands the usage on for billing", func(t *testing.T) {
		f := newChatFixture(t, chatOptions{})
		f.provider.respond(http.StatusOK, "application/json", withUsage)

		rec := f.post()

		if rec.Code != http.StatusOK || rec.Body.String() != withUsage {
			t.Errorf("response = %d %q, want the provider's 200 %q", rec.Code, rec.Body.String(), withUsage)
		}
		_, got := f.provider.received()
		if got.path != "/chat/completions" {
			t.Errorf("the provider was asked for %q, want /chat/completions", got.path)
		}
		// Exactly the provider's key: the client's own gateway key must not
		// travel on to the provider.
		if authorization := got.header.Get("Authorization"); authorization != "Bearer "+providerSecret {
			t.Errorf("the provider got Authorization %q, want %q", authorization, "Bearer "+providerSecret)
		}
		f.wantOneUsageTask(t)
	})

	t.Run("still returns the provider's answer when its usage can't be read, and bills nothing", func(t *testing.T) {
		logs := captureLogs(t)
		f := newChatFixture(t, chatOptions{})
		const badGateway = "<html><body>502 Bad Gateway</body></html>"
		f.provider.respond(http.StatusBadGateway, "text/html", badGateway)

		rec := f.post()

		if rec.Code != http.StatusBadGateway || rec.Body.String() != badGateway {
			t.Errorf("response = %d %q, want the provider's 502 %q", rec.Code, rec.Body.String(), badGateway)
		}
		if got := rec.Result().Header.Get("Content-Type"); got != "text/html" {
			t.Errorf("Content-Type = %q, want text/html", got)
		}
		f.wantNoUsageTask(t)
		wantLogged(t, logs, "could not parse usage from response, usage not logged", "model_id="+f.modelID, "provider=openai")
	})

	t.Run("passes on a successful answer that carries no usage, and reports it as unbilled", func(t *testing.T) {
		tests := []struct {
			name, contentType, body, extra string
		}{
			{"a response", "application/json", `{"id":"chatcmpl-2"}`, ""},
			// An error event cut the stream off before its usage chunk.
			{"a stream", "text/event-stream", "data: {\"choices\":[]}\n\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n", `,"stream":true`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				logs := captureLogs(t)
				f := newChatFixture(t, chatOptions{})
				f.provider.respond(http.StatusOK, tt.contentType, tt.body)

				rec := f.send(f.body(tt.extra), nil)

				if rec.Code != http.StatusOK || rec.Body.String() != tt.body {
					t.Errorf("response = %d %q, want the provider's 200 %q", rec.Code, rec.Body.String(), tt.body)
				}
				f.wantNoUsageTask(t)
				wantLogged(t, logs, "UNBILLED USAGE: provider answer carries no usage, nothing billed", "model_id="+f.modelID, "provider=openai", "status=200")
			})
		}
	})

	t.Run("doesn't report an error answer, which carries no usage, as unbilled", func(t *testing.T) {
		logs := captureLogs(t)
		f := newChatFixture(t, chatOptions{})
		f.provider.respond(http.StatusTooManyRequests, "application/json", `{"error":{"message":"rate limited"}}`)

		f.post()

		f.wantNoUsageTask(t)
		if strings.Contains(logs.String(), "UNBILLED USAGE") {
			t.Errorf("an error answer was reported as unbilled: %s", logs.String())
		}
	})

	t.Run("answers 500 without calling the provider when the model's URL can't be parsed", func(t *testing.T) {
		f := newChatFixture(t, chatOptions{baseURL: "http://[::1"})

		rec := f.post()

		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Invalid provider URL") {
			t.Errorf("response = %d %q, want 500 \"Invalid provider URL\"", rec.Code, rec.Body.String())
		}
		if calls, _ := f.provider.received(); calls != 0 {
			t.Errorf("the provider was called %d times though the URL was invalid", calls)
		}
	})

	t.Run("answers 500 without calling the provider when its key can't be decrypted", func(t *testing.T) {
		logs := captureLogs(t)
		f := newChatFixture(t, chatOptions{})
		err := f.db.Model(&models.ProviderKey{}).Where("id = ?", f.providerKeyID).Update("ciphertext", []byte("not the sealed key")).Error
		if err != nil {
			t.Fatalf("corrupting the provider key: %v", err)
		}

		rec := f.post()

		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Could not decrypt provider key") {
			t.Errorf("response = %d %q, want 500 \"Could not decrypt provider key\"", rec.Code, rec.Body.String())
		}
		if calls, _ := f.provider.received(); calls != 0 {
			t.Errorf("the provider was called %d times without a key to send", calls)
		}
		f.wantNoUsageTask(t)
		wantLogged(t, logs, "could not decrypt provider key")
		if f.sessionCached(t) {
			t.Error("the session that failed to decrypt is still cached in Redis")
		}
	})

	t.Run("loads the repaired key on the next request, instead of failing again from the cached session", func(t *testing.T) {
		f := newChatFixture(t, chatOptions{localTTL: time.Hour})
		f.provider.respond(http.StatusOK, "application/json", withUsage)
		var good models.ProviderKey
		if err := f.db.First(&good, f.providerKeyID).Error; err != nil {
			t.Fatal(err)
		}
		// Update on a model also writes the new value into it, so keep a copy.
		sealed := append([]byte(nil), good.Ciphertext...)
		if err := f.db.Model(&good).Update("ciphertext", []byte("not the sealed key")).Error; err != nil {
			t.Fatalf("corrupting the provider key: %v", err)
		}
		if rec := f.post(); rec.Code != http.StatusInternalServerError {
			t.Fatalf("with the corrupted key: status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		if err := f.db.Model(&good).Update("ciphertext", sealed).Error; err != nil {
			t.Fatalf("repairing the provider key: %v", err)
		}

		rec := f.post()

		if rec.Code != http.StatusOK {
			t.Errorf("after the repair: status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if calls, _ := f.provider.received(); calls != 1 {
			t.Errorf("the provider was called %d times, want once, by the request after the repair", calls)
		}
	})

	t.Run("still answers 500, and says why, when the failed session can't be dropped from Redis", func(t *testing.T) {
		logs := captureLogs(t)
		f := newChatFixture(t, chatOptions{localTTL: time.Hour})
		f.provider.respond(http.StatusOK, "application/json", withUsage)
		if rec := f.post(); rec.Code != http.StatusOK {
			t.Fatalf("the first request failed: %d %s", rec.Code, rec.Body.String())
		}
		f.kms.broken.Store(true)
		f.sessions.Close()

		rec := f.post()

		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Could not decrypt provider key") {
			t.Errorf("response = %d %q, want 500 \"Could not decrypt provider key\"", rec.Code, rec.Body.String())
		}
		wantLogged(t, logs, "could not invalidate cached session after decrypt failure")
	})

	t.Run("answers 400 when a session was cached for a model that has since been removed", func(t *testing.T) {
		f := newChatFixture(t, chatOptions{})
		f.provider.respond(http.StatusOK, "application/json", withUsage)
		if rec := f.post(); rec.Code != http.StatusOK {
			t.Fatalf("the first request failed: %d %s", rec.Code, rec.Body.String())
		}
		delete(providers.Models, f.modelID)

		rec := f.post()

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Unsupported model") {
			t.Errorf("response = %d %q, want 400 \"Unsupported model\"", rec.Code, rec.Body.String())
		}
		if calls, _ := f.provider.received(); calls != 1 {
			t.Errorf("the provider was called %d times, want only for the first request", calls)
		}
	})

	t.Run("attaches the provider's key the way that provider expects", func(t *testing.T) {
		tests := []struct {
			name       string
			opts       chatOptions
			wantPath   string
			wantHeader string // the header the key goes in
		}{
			{
				"x-api-key for Anthropic",
				chatOptions{provider: "anthropic", endpoint: providers.AnthropicEndpoint, auth: providers.XAPIKey},
				"/v1/messages", "X-Api-Key",
			},
			{
				"x-goog-api-key for Gemini",
				chatOptions{provider: "gemini", endpoint: providers.InteractionsEndpoint, auth: providers.GoogAPIKey},
				"/interactions", "X-Goog-Api-Key",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := newChatFixture(t, tt.opts)
				f.provider.respond(http.StatusOK, "application/json", `{"id":"x"}`)

				if rec := f.post(); rec.Code != http.StatusOK {
					t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
				}

				_, got := f.provider.received()
				if got.path != tt.wantPath {
					t.Errorf("the provider was asked for %q, want %q", got.path, tt.wantPath)
				}
				if key := got.header.Get(tt.wantHeader); key != providerSecret {
					t.Errorf("%s = %q, want the provider's key", tt.wantHeader, key)
				}
				// The client's own Authorization header is dropped, and not
				// replaced: these providers don't take a bearer token.
				if authorization := got.header.Get("Authorization"); authorization != "" {
					t.Errorf("Authorization = %q, want none", authorization)
				}
			})
		}
	})

	t.Run("never passes the client's Ostiary key on to the provider", func(t *testing.T) {
		for _, opts := range []chatOptions{
			{provider: "openai", endpoint: providers.OpenAIEndpoint, auth: providers.Bearer},
			{provider: "anthropic", endpoint: providers.AnthropicEndpoint, auth: providers.XAPIKey},
			{provider: "gemini", endpoint: providers.InteractionsEndpoint, auth: providers.GoogAPIKey},
		} {
			t.Run(opts.provider, func(t *testing.T) {
				f := newChatFixture(t, opts)
				f.provider.respond(http.StatusOK, "application/json", `{"id":"x"}`)

				rec := f.send(f.body(""), map[string]string{"x-api-key": f.token, "x-goog-api-key": f.token})
				if rec.Code != http.StatusOK {
					t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
				}

				_, got := f.provider.received()
				for name, values := range got.header {
					for _, value := range values {
						if strings.Contains(value, f.token) {
							t.Errorf("%s carries the client's Ostiary key to the provider", name)
						}
					}
				}
			})
		}
	})

	t.Run("forwards a request with no key, and says so, when the model's authentication type is unknown", func(t *testing.T) {
		logs := captureLogs(t)
		f := newChatFixture(t, chatOptions{auth: "Mystery"})
		f.provider.respond(http.StatusOK, "application/json", `{"id":"x"}`)

		f.post()

		_, got := f.provider.received()
		for _, header := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
			if value := got.header.Get(header); value != "" {
				t.Errorf("%s = %q, want none: no key should travel under an authentication type we don't know", header, value)
			}
		}
		wantLogged(t, logs, "unknown authentication header type, provider key not attached", "authentication_header=Mystery")
	})

	t.Run("makes a streaming OpenAI-style request report its usage, and leaves other schemas' requests as sent", func(t *testing.T) {
		t.Run("OpenAI chat completions", func(t *testing.T) {
			f := newChatFixture(t, chatOptions{})
			f.provider.respond(http.StatusOK, "application/json", `{}`)

			f.send(f.body(`,"stream":true`), nil)

			_, got := f.provider.received()
			var sent struct {
				StreamOptions struct {
					IncludeUsage bool `json:"include_usage"`
				} `json:"stream_options"`
			}
			if err := json.Unmarshal(got.body, &sent); err != nil || !sent.StreamOptions.IncludeUsage {
				t.Errorf("the provider got %s, want stream_options.include_usage true", got.body)
			}
		})

		t.Run("Anthropic messages", func(t *testing.T) {
			f := newChatFixture(t, chatOptions{provider: "anthropic", endpoint: providers.AnthropicEndpoint, auth: providers.XAPIKey})
			f.provider.respond(http.StatusOK, "application/json", `{}`)
			body := f.body(`,"stream":true`)

			f.send(body, nil)

			if _, got := f.provider.received(); string(got.body) != body {
				t.Errorf("the provider got %s, want the request as the client sent it: %s", got.body, body)
			}
		})
	})

	const stream = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"

	t.Run("passes a streaming answer on as it came, and hands its usage on for billing", func(t *testing.T) {
		f := newChatFixture(t, chatOptions{})
		f.provider.respond(http.StatusOK, "text/event-stream", stream)

		rec := f.send(f.body(`,"stream":true`), nil)

		if rec.Code != http.StatusOK || rec.Body.String() != stream {
			t.Errorf("response = %d %q, want the provider's stream %q", rec.Code, rec.Body.String(), stream)
		}
		f.wantOneUsageTask(t)
	})

	t.Run("still passes a streaming answer on when its usage can't be read, and bills nothing", func(t *testing.T) {
		logs := captureLogs(t)
		// A provider with no usage format: its usage can't be priced, so it
		// isn't guessed.
		f := newChatFixture(t, chatOptions{provider: "mystery"})
		f.provider.respond(http.StatusOK, "text/event-stream", stream)

		rec := f.send(f.body(`,"stream":true`), nil)

		if rec.Code != http.StatusOK || rec.Body.String() != stream {
			t.Errorf("response = %d %q, want the provider's stream %q", rec.Code, rec.Body.String(), stream)
		}
		f.wantNoUsageTask(t)
		wantLogged(t, logs, "could not parse usage from stream, usage not logged", "model_id="+f.modelID, "provider=mystery")
	})

	t.Run("compressed answers", func(t *testing.T) {
		t.Run("are asked for as gzip whatever the client accepts, billed, and passed on decompressed", func(t *testing.T) {
			// zstd and deflate are encodings the usage parser can't read: passed
			// on, a provider honouring them would leave the request unbilled.
			for _, accept := range []string{"", "gzip", "br", "zstd", "deflate", "identity"} {
				t.Run("Accept-Encoding "+accept, func(t *testing.T) {
					f := newChatFixture(t, chatOptions{})
					f.provider.respondCompressible(http.StatusOK, "application/json", withUsage)

					rec := f.send(f.body(""), map[string]string{"Accept-Encoding": accept})

					if _, last := f.provider.received(); last.header.Get("Accept-Encoding") != "gzip" {
						t.Errorf("the provider was asked for %q, want gzip", last.header.Get("Accept-Encoding"))
					}
					if rec.Body.String() != withUsage || rec.Result().Header.Get("Content-Encoding") != "" {
						t.Errorf("the client got %q (Content-Encoding %q), want the plain answer", rec.Body.String(), rec.Result().Header.Get("Content-Encoding"))
					}
					f.wantOneUsageTask(t)
				})
			}
		})

		t.Run("still bill the usage that arrived when the stream is cut off part-way", func(t *testing.T) {
			// Anthropic reports the input tokens in message_start: a stream
			// that breaks after it has still cost them.
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(http.StatusOK)
				zw := gzip.NewWriter(w)
				zw.Write([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n"))
				zw.Flush()
				w.(http.Flusher).Flush()
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			}))
			t.Cleanup(provider.Close)
			f := newChatFixture(t, chatOptions{provider: "anthropic", endpoint: providers.AnthropicEndpoint, auth: providers.XAPIKey, baseURL: provider.URL})

			f.send(f.body(`,"stream":true`), map[string]string{"Accept-Encoding": "gzip"})

			f.wantOneUsageTask(t)
		})

		t.Run("that claim gzip but aren't bill nothing", func(t *testing.T) {
			for _, contentType := range []string{"application/json", "text/event-stream"} {
				t.Run(contentType, func(t *testing.T) {
					f := newChatFixture(t, chatOptions{})
					f.provider.respondEncoded(http.StatusOK, contentType, "gzip", []byte("this is not gzip data"))

					f.post()

					f.wantNoUsageTask(t)
				})
			}
		})
	})
}

func TestChatCompletionHandlerClientGoesAway(t *testing.T) {
	// streamingProvider answers with a first event, then holds the rest of its
	// stream, the usage included, until release is called. cancelled is closed
	// if its request is cancelled first. A test defers release, so the stream
	// ends before the servers' cleanups wait for their handlers.
	streamingProvider := func(t *testing.T) (url string, release func(), cancelled chan struct{}) {
		t.Helper()
		held, cancelled := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-held:
			case <-r.Context().Done():
				close(cancelled)
				return
			}
			io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		}))
		t.Cleanup(server.Close)
		return server.URL, sync.OnceFunc(func() { close(held) }), cancelled
	}

	// goAwayAfterFirstEvent streams a request through a real server, reads the
	// first event, and drops the connection, as a user pressing "Stop" does.
	goAwayAfterFirstEvent := func(t *testing.T, f *chatFixture) {
		t.Helper()
		gateway := httptest.NewServer(f.handler)
		t.Cleanup(gateway.Close)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(f.body(`,"stream":true`)))
		req.Header.Set("Authorization", "Bearer "+f.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("sending the request: %v", err)
		}
		if line, _ := bufio.NewReader(resp.Body).ReadString('\n'); !strings.Contains(line, "hi") {
			t.Fatalf("first line = %q, want the provider's first event", line)
		}
		cancel()
		resp.Body.Close()
	}

	t.Run("still reads the provider's answer to its end, and bills its usage", func(t *testing.T) {
		url, release, cancelled := streamingProvider(t)
		defer release()
		f := newChatFixture(t, chatOptions{baseURL: url})

		goAwayAfterFirstEvent(t, f)

		// Time for the gateway to notice the client is gone, which used to
		// cancel the provider's request there and then.
		select {
		case <-cancelled:
			t.Fatal("the provider's request was cancelled when the client went away")
		case <-time.After(500 * time.Millisecond):
		}
		release()
		f.eventuallyOneUsageTask(t)
	})

	t.Run("gives up on the provider once drainLimit has passed since the client went away", func(t *testing.T) {
		const drainLimit = 300 * time.Millisecond
		url, release, cancelled := streamingProvider(t)
		defer release()
		f := newChatFixture(t, chatOptions{baseURL: url, drainLimit: drainLimit})
		start := time.Now()

		goAwayAfterFirstEvent(t, f)

		select {
		case <-cancelled:
			if elapsed := time.Since(start); elapsed < drainLimit {
				t.Errorf("the provider's request was cancelled after %v, before the %v limit", elapsed, drainLimit)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the provider's request was never cancelled")
		}
		f.wantNoUsageTask(t)
	})
}
