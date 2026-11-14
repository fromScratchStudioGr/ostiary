package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// fixture is a Cache on the test Redis, and a second, independent client for
// seeding and checking Redis, so closing the Cache under test doesn't stop
// the checks.
type fixture struct {
	c   *Cache
	raw *redis.Client
}

func setup(t *testing.T, localTTL time.Duration, db *gorm.DB) fixture {
	t.Helper()
	addr, raw := testutil.Redis(t)
	c, err := New(addr, "", "", 5, localTTL, db, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return fixture{c: c, raw: raw}
}

// seed caches a session for each model under the API key hash, in Redis and,
// when it's on, in the local layer, and returns the keys.
func (f fixture) seed(t *testing.T, keyHash string, models ...string) []string {
	t.Helper()
	var keys []string
	for _, model := range models {
		key := apiKeyCachePrefix(keyHash) + model
		keys = append(keys, key)
		if err := f.raw.Set(context.Background(), key, "cached", time.Minute).Err(); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
		if f.c.local != nil {
			f.c.local.set(key, session(model))
		}
	}
	t.Cleanup(func() { f.raw.Del(context.Background(), keys...) })
	return keys
}

func (f fixture) inRedis(t *testing.T, key string) bool {
	t.Helper()
	n, err := f.raw.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("checking %s: %v", key, err)
	}
	return n == 1
}

func (f fixture) inLocal(key string) bool {
	_, ok := f.c.local.get(key)
	return ok
}

// wantCached checks each key's presence in Redis, and in the local layer when
// it's on.
func (f fixture) wantCached(t *testing.T, want bool, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if got := f.inRedis(t, key); got != want {
			t.Errorf("%s in Redis = %v, want %v", key, got, want)
		}
		if f.c.local != nil {
			if got := f.inLocal(key); got != want {
				t.Errorf("%s in the local layer = %v, want %v", key, got, want)
			}
		}
	}
}

func uniqueHash() string { return testutil.Unique()[:16] }

func TestNew(t *testing.T) {
	addr, _ := testutil.Redis(t)

	t.Run("connects to Redis, with the local layer off when the ttl is 0", func(t *testing.T) {
		c, err := New(addr, "", "", 5, 0, nil, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer c.Close()

		if err := c.Ping(context.Background()); err != nil {
			t.Errorf("Ping: %v", err)
		}
		if c.local != nil {
			t.Error("the local layer is on for a ttl of 0")
		}
	})

	t.Run("turns the local layer on for a positive ttl", func(t *testing.T) {
		c, err := New(addr, "", "", 5, time.Minute, nil, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer c.Close()

		if c.local == nil || c.local.ttl != time.Minute {
			t.Errorf("local layer = %+v, want one with a ttl of a minute", c.local)
		}
	})

	t.Run("returns an error, and no cache, when Redis can't be reached", func(t *testing.T) {
		c, err := New("127.0.0.1:1", "", "", 5, time.Minute, nil, nil)

		if err == nil {
			t.Error("got nil error, want one")
		}
		if c != nil {
			t.Error("returned a cache alongside the error")
		}
	})
}

func TestPing(t *testing.T) {
	f := setup(t, 0, nil)

	t.Run("succeeds while Redis is reachable", func(t *testing.T) {
		if err := f.c.Ping(context.Background()); err != nil {
			t.Errorf("Ping: %v", err)
		}
	})

	t.Run("fails once the cache is closed", func(t *testing.T) {
		if err := f.c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		if err := f.c.Ping(context.Background()); err == nil {
			t.Error("Ping succeeded on a closed cache")
		}
	})
}

func TestInvalidateAPIKeys(t *testing.T) {
	ctx := context.Background()

	t.Run("drops the key's sessions for every model, in Redis and the local layer, and nobody else's", func(t *testing.T) {
		f := setup(t, time.Hour, nil)
		hash, other := uniqueHash(), uniqueHash()
		// A hash that starts with the first one: the ":" after the hash in
		// the cache key keeps it from matching.
		longer := hash + "x"
		target := f.seed(t, hash, "model-a", "model-b")
		kept := append(f.seed(t, other, "model-a"), f.seed(t, longer, "model-a")...)

		if err := f.c.InvalidateAPIKeys(ctx, []string{hash}); err != nil {
			t.Fatalf("InvalidateAPIKeys: %v", err)
		}

		f.wantCached(t, false, target...)
		f.wantCached(t, true, kept...)
	})

	t.Run("drops several keys in one call", func(t *testing.T) {
		f := setup(t, time.Hour, nil)
		first, second, third := uniqueHash(), uniqueHash(), uniqueHash()
		dropped := append(f.seed(t, first, "model-a"), f.seed(t, second, "model-a", "model-b")...)
		kept := f.seed(t, third, "model-a")

		if err := f.c.InvalidateAPIKeys(ctx, []string{first, second}); err != nil {
			t.Fatalf("InvalidateAPIKeys: %v", err)
		}

		f.wantCached(t, false, dropped...)
		f.wantCached(t, true, kept...)
	})

	t.Run("finds every session of a key, however many there are to scan through", func(t *testing.T) {
		f := setup(t, 0, nil)
		hash := uniqueHash()
		models := make([]string, 250)
		for i := range models {
			models[i] = fmt.Sprintf("model-%d", i)
		}
		keys := f.seed(t, hash, models...)

		if err := f.c.InvalidateAPIKeys(ctx, []string{hash}); err != nil {
			t.Fatalf("InvalidateAPIKeys: %v", err)
		}

		remaining := 0
		for _, key := range keys {
			if f.inRedis(t, key) {
				remaining++
			}
		}
		if remaining != 0 {
			t.Errorf("%d of %d sessions are still cached", remaining, len(keys))
		}
	})

	t.Run("works with the local layer off", func(t *testing.T) {
		f := setup(t, 0, nil)
		hash := uniqueHash()
		keys := f.seed(t, hash, "model-a")

		if err := f.c.InvalidateAPIKeys(ctx, []string{hash}); err != nil {
			t.Fatalf("InvalidateAPIKeys: %v", err)
		}

		f.wantCached(t, false, keys...)
	})

	t.Run("succeeds when there's nothing cached for the key, or no key at all", func(t *testing.T) {
		f := setup(t, time.Hour, nil)

		if err := f.c.InvalidateAPIKeys(ctx, []string{uniqueHash()}); err != nil {
			t.Errorf("a key with no sessions: %v", err)
		}
		if err := f.c.InvalidateAPIKeys(ctx, nil); err != nil {
			t.Errorf("no keys: %v", err)
		}
	})

	t.Run("returns an error when Redis fails", func(t *testing.T) {
		f := setup(t, time.Hour, nil)
		f.c.Close()

		if err := f.c.InvalidateAPIKeys(ctx, []string{uniqueHash()}); err == nil {
			t.Error("got nil error, want one")
		}
	})
}

func TestInvalidateSession(t *testing.T) {
	ctx := context.Background()
	newToken := func() string { return "ost_" + testutil.Unique() }

	t.Run("drops that key's session for that model, in Redis and the local layer, and nothing else", func(t *testing.T) {
		f := setup(t, time.Hour, nil)
		token, otherToken := newToken(), newToken()
		target := f.seed(t, auth.HashAPIKey(token), "model-a")
		sameKeyOtherModel := f.seed(t, auth.HashAPIKey(token), "model-b")
		otherKeySameModel := f.seed(t, auth.HashAPIKey(otherToken), "model-a")

		if err := f.c.InvalidateSession(ctx, token, "model-a"); err != nil {
			t.Fatalf("InvalidateSession: %v", err)
		}

		f.wantCached(t, false, target...)
		f.wantCached(t, true, append(sameKeyOtherModel, otherKeySameModel...)...)
	})

	t.Run("works with the local layer off", func(t *testing.T) {
		f := setup(t, 0, nil)
		token := newToken()
		keys := f.seed(t, auth.HashAPIKey(token), "model-a")

		if err := f.c.InvalidateSession(ctx, token, "model-a"); err != nil {
			t.Fatalf("InvalidateSession: %v", err)
		}

		f.wantCached(t, false, keys...)
	})

	t.Run("succeeds when nothing is cached for the key and model", func(t *testing.T) {
		f := setup(t, time.Hour, nil)

		if err := f.c.InvalidateSession(ctx, newToken(), "model-a"); err != nil {
			t.Errorf("nothing cached: %v", err)
		}
	})

	t.Run("still drops the local entry, and returns an error, when Redis fails", func(t *testing.T) {
		f := setup(t, time.Hour, nil)
		token := newToken()
		keys := f.seed(t, auth.HashAPIKey(token), "model-a")
		f.c.Close()

		if err := f.c.InvalidateSession(ctx, token, "model-a"); err == nil {
			t.Error("got nil error, want one")
		}
		if f.inLocal(keys[0]) {
			t.Error("the local entry is still cached")
		}
	})
}

func TestInvalidateAllSessions(t *testing.T) {
	ctx := context.Background()

	// Flushing every session would wipe the ones other packages' tests keep in
	// database 0 at the same time, so this runs on a database of its own.
	setupAlone := func(t *testing.T) fixture {
		t.Helper()
		addr, _ := testutil.Redis(t)
		client := redis.NewClient(&redis.Options{Addr: addr, DB: 8})
		t.Cleanup(func() { client.Close() })
		if err := client.FlushDB(ctx).Err(); err != nil {
			t.Fatalf("flushing the test database: %v", err)
		}
		return fixture{c: &Cache{client: client, local: newLocalCache(time.Hour)}, raw: client}
	}

	t.Run("drops every session of every key and model from Redis, and nothing that isn't a session", func(t *testing.T) {
		f := setupAlone(t)
		sessions := append(f.seed(t, uniqueHash(), "model-a", "model-b"), f.seed(t, uniqueHash(), "model-a")...)
		if err := f.raw.Set(ctx, "ostiary:other:thing", "kept", time.Minute).Err(); err != nil {
			t.Fatalf("seeding: %v", err)
		}

		if err := f.c.InvalidateAllSessions(ctx); err != nil {
			t.Fatalf("InvalidateAllSessions: %v", err)
		}

		for _, key := range sessions {
			if f.inRedis(t, key) {
				t.Errorf("%s is still in Redis", key)
			}
		}
		if !f.inRedis(t, "ostiary:other:thing") {
			t.Error("a key that isn't a session was dropped")
		}
	})

	t.Run("leaves the local layer alone, since another process's can't be reached from here", func(t *testing.T) {
		f := setupAlone(t)
		keys := f.seed(t, uniqueHash(), "model-a")

		if err := f.c.InvalidateAllSessions(ctx); err != nil {
			t.Fatalf("InvalidateAllSessions: %v", err)
		}

		if !f.inLocal(keys[0]) {
			t.Error("the local entry was dropped")
		}
	})

	t.Run("drops them all when there are more than one batch", func(t *testing.T) {
		f := setupAlone(t)
		models := make([]string, 1200)
		for i := range models {
			models[i] = fmt.Sprintf("model-%d", i)
		}
		keys := f.seed(t, uniqueHash(), models...)

		if err := f.c.InvalidateAllSessions(ctx); err != nil {
			t.Fatalf("InvalidateAllSessions: %v", err)
		}

		remaining := 0
		for _, key := range keys {
			if f.inRedis(t, key) {
				remaining++
			}
		}
		if remaining != 0 {
			t.Errorf("%d of %d sessions are still cached", remaining, len(keys))
		}
	})

	t.Run("succeeds when nothing is cached", func(t *testing.T) {
		f := setupAlone(t)

		if err := f.c.InvalidateAllSessions(ctx); err != nil {
			t.Errorf("nothing cached: %v", err)
		}
	})

	t.Run("returns an error when Redis fails", func(t *testing.T) {
		f := setupAlone(t)
		f.seed(t, uniqueHash(), "model-a")
		f.c.Close()

		if err := f.c.InvalidateAllSessions(ctx); err == nil {
			t.Error("got nil error, want one")
		}
	})
}

func TestInvalidateConsumer(t *testing.T) {
	db := testutil.Postgres(t)

	// createKey stores an API key with the given hash for the consumer.
	createKey := func(t *testing.T, consumerID uint, hash string) {
		t.Helper()
		key := models.APIKey{ConsumerID: consumerID, Name: hash, KeyHash: hash, Status: "active"}
		if err := db.Create(&key).Error; err != nil {
			t.Fatalf("creating API key: %v", err)
		}
	}
	createConsumer := func(t *testing.T) uint {
		t.Helper()
		consumer := models.Consumer{Name: testutil.Unique()[:12]}
		if err := db.Create(&consumer).Error; err != nil {
			t.Fatalf("creating consumer: %v", err)
		}
		return consumer.ID
	}

	t.Run("drops the sessions of all of the consumer's API keys, and no other consumer's", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		consumer, other := createConsumer(t), createConsumer(t)
		first, second, others := uniqueHash(), uniqueHash(), uniqueHash()
		createKey(t, consumer, first)
		createKey(t, consumer, second)
		createKey(t, other, others)
		dropped := append(f.seed(t, first, "model-a", "model-b"), f.seed(t, second, "model-a")...)
		kept := f.seed(t, others, "model-a")

		if err := f.c.InvalidateConsumer(context.Background(), consumer); err != nil {
			t.Fatalf("InvalidateConsumer: %v", err)
		}

		f.wantCached(t, false, dropped...)
		f.wantCached(t, true, kept...)
	})

	t.Run("succeeds for a consumer with no API keys", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		keys := f.seed(t, uniqueHash(), "model-a")

		if err := f.c.InvalidateConsumer(context.Background(), createConsumer(t)); err != nil {
			t.Fatalf("InvalidateConsumer: %v", err)
		}

		f.wantCached(t, true, keys...)
	})

	t.Run("returns the database's error, and drops nothing, when the keys can't be read", func(t *testing.T) {
		f := setup(t, time.Hour, db)
		consumer := createConsumer(t)
		hash := uniqueHash()
		createKey(t, consumer, hash)
		keys := f.seed(t, hash, "model-a")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := f.c.InvalidateConsumer(ctx, consumer)

		if err == nil {
			t.Error("got nil error, want one")
		}
		f.wantCached(t, true, keys...)
	})
}
