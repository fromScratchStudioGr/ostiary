package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/payload"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/gorm"
)

const testProvider = "testprov"

// eventually waits for cond to become true: the last-used updates run in the
// background.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("timed out waiting for %s", what)
}

func TestTouchLastUsedAt(t *testing.T) {
	f := setup(t, 0, nil)
	// counting returns an update that counts how many times it ran.
	counting := func(counter *atomic.Int32) func(context.Context) error {
		return func(context.Context) error {
			counter.Add(1)
			return nil
		}
	}
	// settle gives updates that would wrongly run a moment to do so.
	settle := func() { time.Sleep(300 * time.Millisecond) }
	clearThrottle := func(t *testing.T, key string) {
		t.Helper()
		f.raw.Del(context.Background(), lastUsedThrottlePrefix+key)
		t.Cleanup(func() { f.raw.Del(context.Background(), lastUsedThrottlePrefix+key) })
	}

	t.Run("runs the update for a key it hasn't seen in the window", func(t *testing.T) {
		key := "test:" + testutil.Unique()
		clearThrottle(t, key)
		var runs atomic.Int32

		touchLastUsedAt(f.c, key, counting(&runs))

		eventually(t, "the update to run", func() bool { return runs.Load() == 1 })
	})

	t.Run("skips the update when the key was already handled within the window", func(t *testing.T) {
		key := "test:" + testutil.Unique()
		clearThrottle(t, key)
		var runs atomic.Int32
		touchLastUsedAt(f.c, key, counting(&runs))
		eventually(t, "the first update to run", func() bool { return runs.Load() == 1 })

		touchLastUsedAt(f.c, key, counting(&runs))
		touchLastUsedAt(f.c, key, counting(&runs))
		settle()

		if got := runs.Load(); got != 1 {
			t.Errorf("the update ran %d times, want once for the window", got)
		}
	})

	t.Run("throttles each key on its own", func(t *testing.T) {
		first, second := "test:"+testutil.Unique(), "test:"+testutil.Unique()
		clearThrottle(t, first)
		clearThrottle(t, second)
		var firstRuns, secondRuns atomic.Int32

		touchLastUsedAt(f.c, first, counting(&firstRuns))
		touchLastUsedAt(f.c, second, counting(&secondRuns))

		eventually(t, "both updates to run", func() bool { return firstRuns.Load() == 1 && secondRuns.Load() == 1 })
	})

	t.Run("skips the update when Redis can't say whether it's due", func(t *testing.T) {
		broken := setup(t, 0, nil)
		broken.c.Close()
		var runs atomic.Int32

		touchLastUsedAt(broken.c, "test:"+testutil.Unique(), counting(&runs))
		settle()

		if got := runs.Load(); got != 0 {
			t.Errorf("the update ran %d times, want none", got)
		}
	})

	t.Run("survives an update that fails", func(t *testing.T) {
		key := "test:" + testutil.Unique()
		clearThrottle(t, key)
		var runs atomic.Int32

		touchLastUsedAt(f.c, key, func(context.Context) error {
			runs.Add(1)
			return fmt.Errorf("database is down")
		})

		eventually(t, "the failing update to run", func() bool { return runs.Load() == 1 })
	})
}

// world is a consumer that can use testProvider through an API key, with
// everything getKeyAccess checks in a passing state.
type world struct {
	token       string
	consumer    models.Consumer
	providerKey models.ProviderKey
	apiKey      models.APIKey
}

func newWorld(t *testing.T, db *gorm.DB, f fixture) world {
	t.Helper()
	create := func(value any) {
		t.Helper()
		if err := db.Create(value).Error; err != nil {
			t.Fatalf("creating %T: %v", value, err)
		}
	}

	var w world
	w.consumer = models.Consumer{Name: testutil.Unique()[:12], Status: "active"}
	create(&w.consumer)
	w.providerKey = models.ProviderKey{
		Provider:     testProvider,
		Ciphertext:   []byte("ciphertext"),
		Nonce:        []byte("nonce"),
		EncryptedDEK: []byte("wrapped-dek"),
		KMSKeyID:     "kms-key-1",
		Status:       "active",
	}
	create(&w.providerKey)
	create(&models.UserProviderAccess{ConsumerID: w.consumer.ID, ProviderKeyID: w.providerKey.ID, Status: "active"})

	raw, hash, hint := auth.GenerateAPIKey()
	w.token = raw
	w.apiKey = models.APIKey{
		ConsumerID:  w.consumer.ID,
		Name:        "key",
		KeyHash:     hash,
		DisplayHint: hint,
		Status:      "active",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	create(&w.apiKey)

	// The throttle keys depend only on row ids, which every test schema
	// starts counting from 1 again.
	throttleKeys := []string{
		fmt.Sprintf("%sproviderkey:%d", lastUsedThrottlePrefix, w.providerKey.ID),
		fmt.Sprintf("%sapikey:%d", lastUsedThrottlePrefix, w.apiKey.ID),
	}
	f.raw.Del(context.Background(), throttleKeys...)
	t.Cleanup(func() { f.raw.Del(context.Background(), throttleKeys...) })
	return w
}

func TestGetKeyAccess(t *testing.T) {
	db := testutil.Postgres(t)
	f := setup(t, 0, db)

	// call runs getKeyAccess for the world's API key and the given model's
	// provider, and returns what it did.
	type result struct {
		access  models.UserProviderAccess
		expires time.Time
		ok      bool
		rec     *httptest.ResponseRecorder
	}
	call := func(token, provider string) result {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		access, expires, ok := getKeyAccess(req, rec, f.c, token, providers.Model{Provider: provider})
		return result{access, expires, ok, rec}
	}
	update := func(t *testing.T, model any, where string, id any, column string, value any) {
		t.Helper()
		if err := db.Model(model).Where(where, id).Update(column, value).Error; err != nil {
			t.Fatalf("updating %s: %v", column, err)
		}
	}
	updateAccess := func(t *testing.T, w world, values map[string]any) {
		t.Helper()
		err := db.Model(&models.UserProviderAccess{}).
			Where("consumer_id = ? AND provider_key_id = ?", w.consumer.ID, w.providerKey.ID).
			Updates(values).Error
		if err != nil {
			t.Fatalf("updating access: %v", err)
		}
	}

	t.Run("returns the access, with its provider key and consumer, and the API key's expiry", func(t *testing.T) {
		w := newWorld(t, db, f)

		got := call(w.token, testProvider)

		if !got.ok {
			t.Fatalf("not ok: %d %s", got.rec.Code, got.rec.Body.String())
		}
		if got.rec.Body.Len() != 0 {
			t.Errorf("wrote a response: %s", got.rec.Body.String())
		}
		access := got.access
		if access.ConsumerID != w.consumer.ID || access.ProviderKeyID != w.providerKey.ID {
			t.Errorf("access = consumer %d, provider key %d; want %d, %d", access.ConsumerID, access.ProviderKeyID, w.consumer.ID, w.providerKey.ID)
		}
		key := access.ProviderKey
		if !bytes.Equal(key.Ciphertext, w.providerKey.Ciphertext) || !bytes.Equal(key.Nonce, w.providerKey.Nonce) ||
			!bytes.Equal(key.EncryptedDEK, w.providerKey.EncryptedDEK) || key.KMSKeyID != w.providerKey.KMSKeyID {
			t.Errorf("provider key = %+v, want the stored key material", key)
		}
		if access.Consumer.Name != w.consumer.Name {
			t.Errorf("consumer = %q, want %q", access.Consumer.Name, w.consumer.Name)
		}
		// The database keeps microseconds.
		if diff := got.expires.Sub(w.apiKey.ExpiresAt).Abs(); diff > time.Millisecond {
			t.Errorf("expiry = %v, want the API key's %v", got.expires, w.apiKey.ExpiresAt)
		}
	})

	t.Run("records that the API key and the provider key were used, in the background", func(t *testing.T) {
		w := newWorld(t, db, f)

		if got := call(w.token, testProvider); !got.ok {
			t.Fatalf("not ok: %d %s", got.rec.Code, got.rec.Body.String())
		}

		eventually(t, "last_used_at to be set on both rows", func() bool {
			var apiKey models.APIKey
			var providerKey models.ProviderKey
			db.First(&apiKey, w.apiKey.ID)
			db.First(&providerKey, w.providerKey.ID)
			return apiKey.LastUsedAt != nil && providerKey.LastUsedAt != nil
		})
	})

	t.Run("lets a consumer through at its usage limit, or with no limit however much it's used", func(t *testing.T) {
		tests := []struct {
			name   string
			values map[string]any
		}{
			{"exactly at the limit", map[string]any{"usage_limit": 10, "total_cost": 10}},
			{"below the limit", map[string]any{"usage_limit": 10, "total_cost": 9.99}},
			{"no limit set", map[string]any{"usage_limit": 0, "total_cost": 1_000_000}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				w := newWorld(t, db, f)
				updateAccess(t, w, tt.values)

				if got := call(w.token, testProvider); !got.ok {
					t.Errorf("not ok: %d %s", got.rec.Code, got.rec.Body.String())
				}
			})
		}
	})

	t.Run("turns a request down, with the reason and nothing else, when", func(t *testing.T) {
		tests := []struct {
			name     string
			change   func(t *testing.T, w world)
			token    string // overrides the world's, when set
			provider string // overrides testProvider, when set
			status   int
			message  string
		}{
			{
				name:  "the API key isn't one that was issued",
				token: "ost_never-issued", status: http.StatusUnauthorized, message: "Invalid API key",
			},
			{
				name: "the API key is revoked",
				change: func(t *testing.T, w world) {
					update(t, &models.APIKey{}, "id = ?", w.apiKey.ID, "status", "revoked")
				},
				status: http.StatusUnauthorized, message: "API key revoked",
			},
			{
				name: "the API key has expired",
				change: func(t *testing.T, w world) {
					update(t, &models.APIKey{}, "id = ?", w.apiKey.ID, "expires_at", time.Now().Add(-time.Minute))
				},
				status: http.StatusUnauthorized, message: "API key expired",
			},
			{
				name:     "the consumer has no key for the model's provider",
				provider: "another-provider", status: http.StatusForbidden, message: "No provider key found for consumer",
			},
			{
				name: "the consumer is revoked",
				change: func(t *testing.T, w world) {
					update(t, &models.Consumer{}, "id = ?", w.consumer.ID, "status", "revoked")
				},
				status: http.StatusForbidden, message: "Consumer access revoked",
			},
			{
				name: "the consumer is over its usage limit",
				change: func(t *testing.T, w world) {
					updateAccess(t, w, map[string]any{"usage_limit": 10, "total_cost": 10.01})
				},
				status: http.StatusForbidden, message: "Usage limit exceeded",
			},
			{
				name: "the provider key is revoked",
				change: func(t *testing.T, w world) {
					update(t, &models.ProviderKey{}, "id = ?", w.providerKey.ID, "status", "revoked")
				},
				status: http.StatusForbidden, message: "Provider key is inactive",
			},
			{
				name: "the provider key was deleted",
				change: func(t *testing.T, w world) {
					if err := db.Delete(&models.ProviderKey{}, w.providerKey.ID).Error; err != nil {
						t.Fatalf("deleting the provider key: %v", err)
					}
				},
				status: http.StatusForbidden, message: "Provider key is inactive",
			},
			{
				name: "the consumer's access to the provider key is revoked",
				change: func(t *testing.T, w world) {
					updateAccess(t, w, map[string]any{"status": "revoked"})
				},
				status: http.StatusForbidden, message: "Access key is inactive",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				w := newWorld(t, db, f)
				if tt.change != nil {
					tt.change(t, w)
				}
				token, provider := w.token, testProvider
				if tt.token != "" {
					token = tt.token
				}
				if tt.provider != "" {
					provider = tt.provider
				}

				got := call(token, provider)

				if got.ok {
					t.Fatal("ok, want the request turned down")
				}
				if got.rec.Code != tt.status {
					t.Errorf("status = %d, want %d", got.rec.Code, tt.status)
				}
				if body := got.rec.Body.String(); !bytes.Contains([]byte(body), []byte(tt.message)) {
					t.Errorf("body = %s, want the message %q", body, tt.message)
				}
				if !got.expires.IsZero() {
					t.Errorf("returned an expiry %v alongside the rejection", got.expires)
				}
			})
		}
	})
}

// lookups returns handle_cache's lookup counter for each result so far.
func lookups(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	counts := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "gateway_cache_lookups_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" {
					counts[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	return counts
}

// wantLookups checks how far each lookup counter has risen since before; a
// result missing from want is expected not to have moved.
func wantLookups(t *testing.T, before, want map[string]float64) {
	t.Helper()
	after := lookups(t)
	for _, result := range []string{"local_hit", "hit", "miss"} {
		if got := after[result] - before[result]; got != want[result] {
			t.Errorf("%s lookups rose by %v, want %v", result, got, want[result])
		}
	}
}

// registerModel adds a model of testProvider to the registry for the test.
func registerModel(t *testing.T) providers.Model {
	t.Helper()
	id := "test-model-" + testutil.Unique()[:10]
	model := providers.Model{
		Id:                   id,
		Name:                 id,
		Provider:             testProvider,
		BaseUrl:              "https://" + id + ".test/v1",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
	}
	providers.Register(id, model)
	t.Cleanup(func() { delete(providers.Models, id) })
	return model
}

// checkSession checks a session against what the world and model say it
// should hold.
func checkSession(t *testing.T, got UserCache, w world, model providers.Model) {
	t.Helper()
	if got.ModelID != model.Id || got.ProviderKeyID != w.providerKey.ID || got.ConsumerID != w.consumer.ID || got.KMSKeyID != w.providerKey.KMSKeyID {
		t.Errorf("session = %+v, want model %q, provider key %d, consumer %d, KMS key %q", got, model.Id, w.providerKey.ID, w.consumer.ID, w.providerKey.KMSKeyID)
	}
	if got.ProviderURL == nil || *got.ProviderURL != model.BaseUrl {
		t.Errorf("provider URL = %v, want %q", got.ProviderURL, model.BaseUrl)
	}
	if got.ProviderEndpoint == nil || *got.ProviderEndpoint != model.Endpoint {
		t.Errorf("provider endpoint = %v, want %q", got.ProviderEndpoint, model.Endpoint)
	}
	if !bytes.Equal(got.Nonce, w.providerKey.Nonce) || !bytes.Equal(got.Ciphertext, w.providerKey.Ciphertext) || !bytes.Equal(got.EncryptedDEK, w.providerKey.EncryptedDEK) {
		t.Errorf("session holds other key material than the provider key's")
	}
	if diff := got.KeyExpiresAt.Sub(w.apiKey.ExpiresAt).Abs(); diff > time.Millisecond {
		t.Errorf("key expiry = %v, want the API key's %v", got.KeyExpiresAt, w.apiKey.ExpiresAt)
	}
}

func TestHandleCache(t *testing.T) {
	db := testutil.Postgres(t)

	// served is what a request through HandleCache did.
	type served struct {
		rec   *httptest.ResponseRecorder
		calls int
		got   UserCache
		body  string
	}
	// serve sends a request through payload.Read and HandleCache, as the
	// gateway's routes do.
	serve := func(f fixture, token string, body io.Reader) served {
		var s served
		handler := payload.Read(f.c.HandleCache(func(w http.ResponseWriter, r *http.Request) {
			s.calls++
			s.got = UserCacheFromContext(r)
			b, _ := io.ReadAll(r.Body)
			s.body = string(b)
			w.WriteHeader(http.StatusAccepted)
		}))
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		s.rec = httptest.NewRecorder()
		handler(s.rec, req)
		return s
	}
	bodyFor := func(model string) io.Reader {
		return strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, model))
	}
	// sessionKey is where the session of a token and model is cached, cleaned
	// up when the test ends.
	sessionKey := func(t *testing.T, f fixture, token, model string) string {
		t.Helper()
		key := apiKeyCachePrefix(auth.HashAPIKey(token)) + model
		t.Cleanup(func() { f.raw.Del(context.Background(), key) })
		return key
	}
	wantRejected := func(t *testing.T, s served, status int, message string) {
		t.Helper()
		if s.calls != 0 {
			t.Errorf("the wrapped handler ran %d times for a rejected request", s.calls)
		}
		if s.rec.Code != status {
			t.Errorf("status = %d, want %d", s.rec.Code, status)
		}
		if body := s.rec.Body.String(); !strings.Contains(body, message) {
			t.Errorf("body = %s, want it to contain %q", body, message)
		}
	}
	revokeAPIKey := func(t *testing.T, w world) {
		t.Helper()
		if err := db.Model(&models.APIKey{}).Where("id = ?", w.apiKey.ID).Update("status", "revoked").Error; err != nil {
			t.Fatalf("revoking the API key: %v", err)
		}
	}

	t.Run("on a first request, checks the key in the database, caches the session and passes it on", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, model := newWorld(t, db, f), registerModel(t)
		key := sessionKey(t, f, w.token, model.Id)
		before := lookups(t)

		s := serve(f, w.token, bodyFor(model.Id))

		if s.calls != 1 || s.rec.Code != http.StatusAccepted {
			t.Fatalf("calls = %d, status = %d; want one call and the wrapped handler's 202: %s", s.calls, s.rec.Code, s.rec.Body.String())
		}
		checkSession(t, s.got, w, model)

		stored, err := f.raw.Get(context.Background(), key).Bytes()
		if err != nil {
			t.Fatalf("the session isn't in Redis: %v", err)
		}
		var inRedis UserCache
		if err := json.Unmarshal(stored, &inRedis); err != nil {
			t.Fatalf("the cached session isn't valid JSON: %v", err)
		}
		checkSession(t, inRedis, w, model)
		if ttl, _ := f.raw.TTL(context.Background(), key).Result(); ttl <= 0 || ttl > sessionTTL {
			t.Errorf("Redis TTL = %v, want up to %v", ttl, sessionTTL)
		}
		if !f.inLocal(key) {
			t.Error("the session isn't in the local layer")
		}
		wantLookups(t, before, map[string]float64{"miss": 1})
	})

	t.Run("never keeps a session longer than its API key lasts", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, model := newWorld(t, db, f), registerModel(t)
		key := sessionKey(t, f, w.token, model.Id)
		expiry := time.Now().Add(5 * time.Minute)
		if err := db.Model(&models.APIKey{}).Where("id = ?", w.apiKey.ID).Update("expires_at", expiry).Error; err != nil {
			t.Fatal(err)
		}

		if s := serve(f, w.token, bodyFor(model.Id)); s.calls != 1 {
			t.Fatalf("not served: %d %s", s.rec.Code, s.rec.Body.String())
		}

		ttl, _ := f.raw.TTL(context.Background(), key).Result()
		if ttl <= 4*time.Minute || ttl > 5*time.Minute {
			t.Errorf("Redis TTL = %v, want just under the key's 5 minutes", ttl)
		}
		if got := f.c.local.entries[key].expiresAt; got.After(expiry) {
			t.Errorf("local entry expires at %v, after the key's expiry %v", got, expiry)
		}
	})

	t.Run("serves later requests from the local layer, without Postgres or Redis", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, model := newWorld(t, db, f), registerModel(t)
		key := sessionKey(t, f, w.token, model.Id)
		serve(f, w.token, bodyFor(model.Id))
		// Take the session's other sources away: only the local layer is left.
		revokeAPIKey(t, w)
		f.raw.Del(context.Background(), key)
		before := lookups(t)

		s := serve(f, w.token, bodyFor(model.Id))

		if s.calls != 1 {
			t.Fatalf("not served: %d %s", s.rec.Code, s.rec.Body.String())
		}
		checkSession(t, s.got, w, model)
		wantLookups(t, before, map[string]float64{"local_hit": 1})
	})

	t.Run("serves from Redis when the local layer doesn't have it, and fills the local layer again", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, model := newWorld(t, db, f), registerModel(t)
		key := sessionKey(t, f, w.token, model.Id)
		serve(f, w.token, bodyFor(model.Id))
		f.c.local.deletePrefix(key)
		// Postgres has nothing to say any more: only Redis can serve it.
		revokeAPIKey(t, w)
		before := lookups(t)

		s := serve(f, w.token, bodyFor(model.Id))

		if s.calls != 1 {
			t.Fatalf("not served: %d %s", s.rec.Code, s.rec.Body.String())
		}
		checkSession(t, s.got, w, model)
		if !f.inLocal(key) {
			t.Error("the local layer wasn't filled from Redis")
		}
		wantLookups(t, before, map[string]float64{"hit": 1})
	})

	t.Run("with the local layer off, serves later requests from Redis", func(t *testing.T) {
		f := setup(t, 0, db)
		w, model := newWorld(t, db, f), registerModel(t)
		sessionKey(t, f, w.token, model.Id)
		before := lookups(t)

		serve(f, w.token, bodyFor(model.Id))
		revokeAPIKey(t, w)
		s := serve(f, w.token, bodyFor(model.Id))

		if s.calls != 1 {
			t.Fatalf("not served: %d %s", s.rec.Code, s.rec.Body.String())
		}
		wantLookups(t, before, map[string]float64{"miss": 1, "hit": 1})
	})

	t.Run("keeps a session for each model and for each API key", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, other := newWorld(t, db, f), newWorld(t, db, f)
		first, second := registerModel(t), registerModel(t)
		for _, token := range []string{w.token, other.token} {
			sessionKey(t, f, token, first.Id)
			sessionKey(t, f, token, second.Id)
		}
		before := lookups(t)

		a := serve(f, w.token, bodyFor(first.Id))
		b := serve(f, w.token, bodyFor(second.Id))
		c := serve(f, other.token, bodyFor(first.Id))

		if a.got.ModelID != first.Id || b.got.ModelID != second.Id {
			t.Errorf("models = %q and %q, want %q and %q", a.got.ModelID, b.got.ModelID, first.Id, second.Id)
		}
		if c.got.ConsumerID != other.consumer.ID || c.got.ConsumerID == a.got.ConsumerID {
			t.Errorf("another API key got consumer %d, want its own (%d), not %d", c.got.ConsumerID, other.consumer.ID, a.got.ConsumerID)
		}
		wantLookups(t, before, map[string]float64{"miss": 3})
	})

	t.Run("hands the request body on to the wrapped handler intact", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, model := newWorld(t, db, f), registerModel(t)
		sessionKey(t, f, w.token, model.Id)
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model.Id)

		s := serve(f, w.token, strings.NewReader(body))

		if s.body != body {
			t.Errorf("the wrapped handler read %q, want %q", s.body, body)
		}
	})

	t.Run("answers 500, and looks nothing up, for a request whose payload wasn't read", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, model := newWorld(t, db, f), registerModel(t)
		var calls int
		handler := f.c.HandleCache(func(http.ResponseWriter, *http.Request) { calls++ })
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bodyFor(model.Id))
		req.Header.Set("Authorization", "Bearer "+w.token)
		rec := httptest.NewRecorder()
		before := lookups(t)

		handler(rec, req)

		wantRejected(t, served{rec: rec, calls: calls}, http.StatusInternalServerError, "Could not read request payload")
		wantLookups(t, before, nil)
	})

	t.Run("turns a miss down, with the reason, and caches nothing, when", func(t *testing.T) {
		tests := []struct {
			name    string
			setup   func(t *testing.T, w world) (token, model string)
			status  int
			message string
		}{
			{
				name: "the model isn't supported",
				setup: func(t *testing.T, w world) (string, string) {
					return w.token, "no-such-model-" + testutil.Unique()[:8]
				},
				status: http.StatusBadRequest, message: "Unsupported model",
			},
			{
				name: "the API key was never issued",
				setup: func(t *testing.T, w world) (string, string) {
					return "ost_never-issued", registerModel(t).Id
				},
				status: http.StatusUnauthorized, message: "Invalid API key",
			},
			{
				name: "there's no API key at all",
				setup: func(t *testing.T, w world) (string, string) {
					return "", registerModel(t).Id
				},
				status: http.StatusUnauthorized, message: "Invalid API key",
			},
			{
				name: "the API key is revoked",
				setup: func(t *testing.T, w world) (string, string) {
					revokeAPIKey(t, w)
					return w.token, registerModel(t).Id
				},
				status: http.StatusUnauthorized, message: "API key revoked",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := setup(t, time.Hour, db)
				w := newWorld(t, db, f)
				token, model := tt.setup(t, w)
				key := sessionKey(t, f, token, model)

				s := serve(f, token, bodyFor(model))

				wantRejected(t, s, tt.status, tt.message)
				if f.inRedis(t, key) || f.inLocal(key) {
					t.Error("a rejected request left a session in the cache")
				}
			})
		}
	})

	t.Run("answers 500 when Redis can't be read", func(t *testing.T) {
		f := setup(t, 0, db)
		w, model := newWorld(t, db, f), registerModel(t)
		f.c.Close()

		wantRejected(t, serve(f, w.token, bodyFor(model.Id)), http.StatusInternalServerError, "Could not read token from Redis")
	})

	t.Run("answers 500 when the cached session can't be parsed", func(t *testing.T) {
		f := setup(t, 0, db)
		w, model := newWorld(t, db, f), registerModel(t)
		key := sessionKey(t, f, w.token, model.Id)
		if err := f.raw.Set(context.Background(), key, "not json", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}

		wantRejected(t, serve(f, w.token, bodyFor(model.Id)), http.StatusInternalServerError, "Could not parse cached user data")
	})

	t.Run("applies an invalidation to the next request, even through the local layer", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		w, model := newWorld(t, db, f), registerModel(t)
		sessionKey(t, f, w.token, model.Id)
		serve(f, w.token, bodyFor(model.Id))

		// A revocation alone changes nothing for a cached session.
		revokeAPIKey(t, w)
		if s := serve(f, w.token, bodyFor(model.Id)); s.calls != 1 {
			t.Fatalf("a cached session was dropped by a revocation alone: %d %s", s.rec.Code, s.rec.Body.String())
		}

		if err := f.c.InvalidateAPIKeys(context.Background(), []string{auth.HashAPIKey(w.token)}); err != nil {
			t.Fatalf("InvalidateAPIKeys: %v", err)
		}

		wantRejected(t, serve(f, w.token, bodyFor(model.Id)), http.StatusUnauthorized, "API key revoked")
	})
}
