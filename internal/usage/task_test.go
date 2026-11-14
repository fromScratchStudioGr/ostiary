package usage

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// sessionKey is the Redis key cache.HandleCache stores a session under. The
// layout is private to the cache package, so it's mirrored here to seed and
// inspect sessions: if it ever changes, the invalidation tests below fail.
func sessionKey(keyHash, model string) string {
	return "ostiary:cache:" + keyHash + ":" + model
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// persistFixture is a consumer with a provider key, an optional access grant
// between them, and two API keys, on a fresh Postgres schema and a Redis
// shared with other tests.
type persistFixture struct {
	ctx       context.Context
	db        *gorm.DB
	cache     *cache.Cache
	rdb       *redis.Client
	consumer  models.Consumer
	provider  models.ProviderKey
	keyHashes []string
}

func newPersistFixture(t *testing.T, withAccess bool, usageLimit, totalCost float64) *persistFixture {
	t.Helper()
	db := testutil.Postgres(t)
	addr, rdb := testutil.Redis(t)

	c, err := cache.New(addr, "", "", 5, 0, db, nil)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	f := &persistFixture{ctx: context.Background(), db: db, cache: c, rdb: rdb}
	f.consumer = f.createConsumer(t)
	f.provider = models.ProviderKey{Provider: "openai", Status: "active"}
	if err := db.Create(&f.provider).Error; err != nil {
		t.Fatalf("creating provider key: %v", err)
	}
	if withAccess {
		access := models.UserProviderAccess{
			ConsumerID:    f.consumer.ID,
			ProviderKeyID: f.provider.ID,
			Status:        "active",
			UsageLimit:    usageLimit,
			TotalCost:     totalCost,
		}
		if err := db.Omit(clause.Associations).Create(&access).Error; err != nil {
			t.Fatalf("creating access: %v", err)
		}
	}
	f.keyHashes = []string{f.createAPIKey(t, f.consumer), f.createAPIKey(t, f.consumer)}
	return f
}

func (f *persistFixture) createConsumer(t *testing.T) models.Consumer {
	t.Helper()
	c := models.Consumer{Name: "consumer", Status: "active"}
	if err := f.db.Create(&c).Error; err != nil {
		t.Fatalf("creating consumer: %v", err)
	}
	return c
}

// createAPIKey stores an API key for the consumer and returns its hash.
func (f *persistFixture) createAPIKey(t *testing.T, c models.Consumer) string {
	t.Helper()
	hash := testutil.Unique()
	key := models.APIKey{ConsumerID: c.ID, Name: "key", KeyHash: hash, Status: "active", ExpiresAt: time.Now().Add(time.Hour)}
	if err := f.db.Omit(clause.Associations).Create(&key).Error; err != nil {
		t.Fatalf("creating api key: %v", err)
	}
	return hash
}

// seedSessions caches a session for each of two models under each key hash,
// and returns the Redis keys. They're removed when the test ends.
func (f *persistFixture) seedSessions(t *testing.T, keyHashes ...string) []string {
	t.Helper()
	var keys []string
	for _, hash := range keyHashes {
		for _, model := range []string{"model-a", "model-b"} {
			keys = append(keys, sessionKey(hash, model))
		}
	}
	for _, key := range keys {
		if err := f.rdb.Set(f.ctx, key, "session", time.Hour).Err(); err != nil {
			t.Fatalf("seeding session: %v", err)
		}
	}
	t.Cleanup(func() { f.rdb.Del(context.Background(), keys...) })
	return keys
}

// cached reports how many of the keys are still in Redis.
func (f *persistFixture) cached(t *testing.T, keys []string) int64 {
	t.Helper()
	n, err := f.rdb.Exists(f.ctx, keys...).Result()
	if err != nil {
		t.Fatalf("checking sessions: %v", err)
	}
	return n
}

func (f *persistFixture) access(t *testing.T) models.UserProviderAccess {
	t.Helper()
	var a models.UserProviderAccess
	if err := f.db.Where("consumer_id = ? AND provider_key_id = ?", f.consumer.ID, f.provider.ID).First(&a).Error; err != nil {
		t.Fatalf("loading access: %v", err)
	}
	return a
}

func (f *persistFixture) usageRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.db.Model(&models.Usage{}).Count(&n).Error; err != nil {
		t.Fatalf("counting usage rows: %v", err)
	}
	return n
}

// record is a usage record for the fixture's consumer and provider key whose
// whole cost is cost.
func (f *persistFixture) record(cost float64) models.Usage {
	return models.Usage{ConsumerID: f.consumer.ID, ProviderKeyID: f.provider.ID, CompletionTokens: 10, CompletionTokensCost: cost}
}

// fullRecord is a usage record for the fixture's consumer and provider key
// with a different value in every token and cost field, so a field that's
// dropped or mixed up along the way shows. Its costs sum to 1.5.
func (f *persistFixture) fullRecord() models.Usage {
	return models.Usage{
		ConsumerID:                       f.consumer.ID,
		ProviderKeyID:                    f.provider.ID,
		CompletionTokens:                 1,
		CompletionTokensCost:             0.1,
		PromptCacheHitTokens:             2,
		PromptCacheHitTokensCost:         0.2,
		PromptCacheMissTokens:            3,
		PromptCacheMissTokensCost:        0.3,
		PromptCacheWriteLevel1Tokens:     4,
		PromptCacheWriteLevel1TokensCost: 0.4,
		PromptCacheWriteLevel2Tokens:     5,
		PromptCacheWriteLevel2TokensCost: 0.5,
	}
}

// requireFullRecordStored checks that the one stored usage row is fullRecord.
func (f *persistFixture) requireFullRecordStored(t *testing.T) {
	t.Helper()
	if rows := f.usageRows(t); rows != 1 {
		t.Fatalf("usage rows = %d, want 1", rows)
	}
	var got models.Usage
	if err := f.db.Where("consumer_id = ?", f.consumer.ID).First(&got).Error; err != nil {
		t.Fatalf("loading the usage row: %v", err)
	}
	if got.ProviderKeyID != f.provider.ID ||
		got.CompletionTokens != 1 || got.PromptCacheHitTokens != 2 || got.PromptCacheMissTokens != 3 ||
		got.PromptCacheWriteLevel1Tokens != 4 || got.PromptCacheWriteLevel2Tokens != 5 {
		t.Fatalf("stored tokens differ: %+v", got)
	}
	if !approxEqual(got.CompletionTokensCost, 0.1) || !approxEqual(got.PromptCacheHitTokensCost, 0.2) ||
		!approxEqual(got.PromptCacheMissTokensCost, 0.3) || !approxEqual(got.PromptCacheWriteLevel1TokensCost, 0.4) ||
		!approxEqual(got.PromptCacheWriteLevel2TokensCost, 0.5) {
		t.Fatalf("stored costs differ: %+v", got)
	}
}

func TestPersistUsage(t *testing.T) {
	t.Run("stores the usage record and adds the sum of its costs to the total", func(t *testing.T) {
		f := newPersistFixture(t, true, 100, 10)

		if err := persistUsage(f.ctx, f.db, f.cache, f.fullRecord()); err != nil {
			t.Fatalf("persistUsage: %v", err)
		}

		f.requireFullRecordStored(t)
		if total := f.access(t).TotalCost; !approxEqual(total, 11.5) {
			t.Fatalf("total cost = %v, want 11.5 (10 + 0.1+0.2+0.3+0.4+0.5)", total)
		}
	})

	t.Run("keeps adding to the running total", func(t *testing.T) {
		f := newPersistFixture(t, true, 100, 10)

		for _, cost := range []float64{1.5, 2} {
			if err := persistUsage(f.ctx, f.db, f.cache, f.record(cost)); err != nil {
				t.Fatalf("persistUsage: %v", err)
			}
		}

		if total := f.access(t).TotalCost; !approxEqual(total, 13.5) {
			t.Fatalf("total cost = %v, want 13.5", total)
		}
		if rows := f.usageRows(t); rows != 2 {
			t.Fatalf("usage rows = %d, want 2", rows)
		}
	})

	t.Run("drops the consumer's cached sessions once the limit is exceeded", func(t *testing.T) {
		f := newPersistFixture(t, true, 1, 0.5)
		consumerSessions := f.seedSessions(t, f.keyHashes...)

		other := f.createConsumer(t)
		otherSessions := f.seedSessions(t, f.createAPIKey(t, other))

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1)); err != nil {
			t.Fatalf("persistUsage: %v", err)
		}

		if n := f.cached(t, consumerSessions); n != 0 {
			t.Fatalf("%d of the consumer's %d sessions are still cached, want none", n, len(consumerSessions))
		}
		if n := f.cached(t, otherSessions); n != int64(len(otherSessions)) {
			t.Fatalf("another consumer has %d of %d sessions cached, want all", n, len(otherSessions))
		}
	})

	t.Run("keeps cached sessions while under the limit", func(t *testing.T) {
		f := newPersistFixture(t, true, 100, 0.5)
		sessions := f.seedSessions(t, f.keyHashes...)

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1)); err != nil {
			t.Fatalf("persistUsage: %v", err)
		}

		if n := f.cached(t, sessions); n != int64(len(sessions)) {
			t.Fatalf("%d of %d sessions cached, want all", n, len(sessions))
		}
	})

	t.Run("keeps cached sessions when there is no limit", func(t *testing.T) {
		f := newPersistFixture(t, true, 0, 0)
		sessions := f.seedSessions(t, f.keyHashes...)

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1_000_000)); err != nil {
			t.Fatalf("persistUsage: %v", err)
		}

		if n := f.cached(t, sessions); n != int64(len(sessions)) {
			t.Fatalf("%d of %d sessions cached, want all (a limit of 0 means unlimited)", n, len(sessions))
		}
	})

	t.Run("treats a total exactly at the limit as within it", func(t *testing.T) {
		f := newPersistFixture(t, true, 2, 1)
		sessions := f.seedSessions(t, f.keyHashes...)

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1)); err != nil {
			t.Fatalf("persistUsage: %v", err)
		}

		if total := f.access(t).TotalCost; !approxEqual(total, 2) {
			t.Fatalf("total cost = %v, want 2", total)
		}
		if n := f.cached(t, sessions); n != int64(len(sessions)) {
			t.Fatalf("%d of %d sessions cached, want all (only a total above the limit blocks)", n, len(sessions))
		}
	})

	t.Run("keeps the usage and returns nil when the access row is gone", func(t *testing.T) {
		f := newPersistFixture(t, false, 0, 0)
		sessions := f.seedSessions(t, f.keyHashes...)

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1)); err != nil {
			t.Fatalf("persistUsage: %v, want nil (retrying can't bring the access row back)", err)
		}

		if rows := f.usageRows(t); rows != 1 {
			t.Fatalf("usage rows = %d, want 1", rows)
		}
		if n := f.cached(t, sessions); n != int64(len(sessions)) {
			t.Fatalf("%d of %d sessions cached, want all", n, len(sessions))
		}
	})

	t.Run("returns the error and writes nothing when the usage row can't be stored", func(t *testing.T) {
		f := newPersistFixture(t, true, 1, 0.5)
		sessions := f.seedSessions(t, f.keyHashes...)

		record := f.record(1)
		record.ConsumerID = f.consumer.ID + 1_000_000 // no such consumer: violates the foreign key

		if err := persistUsage(f.ctx, f.db, f.cache, record); err == nil {
			t.Fatal("persistUsage returned nil, want an error")
		}

		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d, want 0", rows)
		}
		if total := f.access(t).TotalCost; !approxEqual(total, 0.5) {
			t.Fatalf("total cost = %v, want it unchanged at 0.5", total)
		}
		if n := f.cached(t, sessions); n != int64(len(sessions)) {
			t.Fatalf("%d of %d sessions cached, want all", n, len(sessions))
		}
	})

	t.Run("rolls the usage row back when updating the total fails", func(t *testing.T) {
		f := newPersistFixture(t, true, 1, 0.5)
		sessions := f.seedSessions(t, f.keyHashes...)

		// The usage row is inserted first; the update that follows then
		// fails because its table is gone.
		if err := f.db.Exec("DROP TABLE user_provider_accesses").Error; err != nil {
			t.Fatalf("dropping table: %v", err)
		}

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1)); err == nil {
			t.Fatal("persistUsage returned nil, want an error")
		}

		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d, want 0 (the insert must be rolled back)", rows)
		}
		if n := f.cached(t, sessions); n != int64(len(sessions)) {
			t.Fatalf("%d of %d sessions cached, want all", n, len(sessions))
		}
	})

	t.Run("rolls everything back when reading the limit back fails", func(t *testing.T) {
		f := newPersistFixture(t, true, 1, 0.5)
		sessions := f.seedSessions(t, f.keyHashes...)

		// The update only touches total_cost, so it still succeeds, and the
		// read of usage_limit that follows it is what fails.
		if err := f.db.Exec("ALTER TABLE user_provider_accesses DROP COLUMN usage_limit").Error; err != nil {
			t.Fatalf("dropping column: %v", err)
		}

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1)); err == nil {
			t.Fatal("persistUsage returned nil, want an error")
		}

		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d, want 0 (the insert must be rolled back)", rows)
		}
		if total := f.access(t).TotalCost; !approxEqual(total, 0.5) {
			t.Fatalf("total cost = %v, want it rolled back to 0.5", total)
		}
		if n := f.cached(t, sessions); n != int64(len(sessions)) {
			t.Fatalf("%d of %d sessions cached, want all", n, len(sessions))
		}
	})

	t.Run("returns nil when dropping the cached sessions fails", func(t *testing.T) {
		f := newPersistFixture(t, true, 1, 0.5)
		f.seedSessions(t, f.keyHashes...)

		// Redis is unreachable for the invalidation that follows the commit.
		// An error here would make asynq retry the task and store the usage
		// a second time, so it must only be logged.
		f.cache.Close()

		if err := persistUsage(f.ctx, f.db, f.cache, f.record(1)); err != nil {
			t.Fatalf("persistUsage: %v, want nil", err)
		}

		if rows := f.usageRows(t); rows != 1 {
			t.Fatalf("usage rows = %d, want 1", rows)
		}
		if total := f.access(t).TotalCost; !approxEqual(total, 1.5) {
			t.Fatalf("total cost = %v, want 1.5", total)
		}
	})
}

func TestNewLogUsageTask(t *testing.T) {
	t.Run("returns an error for a cost JSON can't encode", func(t *testing.T) {
		// A NaN or infinite cost (e.g. from a bad price) can't be written as
		// JSON. The error is what lets LogUsage fall back to a direct write
		// instead of enqueueing a task that can never be decoded.
		for name, cost := range map[string]float64{"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1)} {
			t.Run(name, func(t *testing.T) {
				task, err := newLogUsageTask(models.Usage{ConsumerID: 1, ProviderKeyID: 1, CompletionTokensCost: cost}, 25)
				if err == nil || task != nil {
					t.Fatalf("got (%v, %v), want an error and no task", task, err)
				}
			})
		}
	})
}

func TestHandleLogUsageTask(t *testing.T) {
	t.Run("persists a task built by newLogUsageTask", func(t *testing.T) {
		f := newPersistFixture(t, true, 100, 10)
		task, err := newLogUsageTask(f.fullRecord(), 25)
		if err != nil {
			t.Fatalf("newLogUsageTask: %v", err)
		}
		if task.Type() != TaskTypeLogUsage {
			t.Fatalf("task type = %q, want %q", task.Type(), TaskTypeLogUsage)
		}

		if err := HandleLogUsageTask(f.db, f.cache)(f.ctx, task); err != nil {
			t.Fatalf("handler: %v", err)
		}

		// Every field made it through the payload and into the stored row.
		f.requireFullRecordStored(t)
		if total := f.access(t).TotalCost; !approxEqual(total, 11.5) {
			t.Fatalf("total cost = %v, want 11.5", total)
		}
	})

	t.Run("reads the payload layout already sitting in the queue", func(t *testing.T) {
		f := newPersistFixture(t, true, 100, 10)

		// Tasks wait in Redis, so after a deploy the handler can get a
		// payload written by the previous build. This literal is that wire
		// format: the round trip above can't catch a renamed JSON key,
		// since it renames both sides. The extra field stands for one a
		// newer build added, which must be ignored.
		payload := fmt.Sprintf(`{
			"consumer_id": %d,
			"provider_key_id": %d,
			"completion_tokens": 1,
			"completion_tokens_cost": 0.1,
			"prompt_cache_hit_tokens": 2,
			"prompt_cache_hit_tokens_cost": 0.2,
			"prompt_cache_miss_tokens": 3,
			"prompt_cache_miss_tokens_cost": 0.3,
			"prompt_cache_write_level1_tokens": 4,
			"prompt_cache_write_level1_tokens_cost": 0.4,
			"prompt_cache_write_level2_tokens": 5,
			"prompt_cache_write_level2_tokens_cost": 0.5,
			"field_from_a_newer_build": true
		}`, f.consumer.ID, f.provider.ID)
		task := asynq.NewTask(TaskTypeLogUsage, []byte(payload))

		if err := HandleLogUsageTask(f.db, f.cache)(f.ctx, task); err != nil {
			t.Fatalf("handler: %v", err)
		}

		f.requireFullRecordStored(t)
		if total := f.access(t).TotalCost; !approxEqual(total, 11.5) {
			t.Fatalf("total cost = %v, want 11.5", total)
		}
	})

	t.Run("returns an error and stores nothing for a payload that isn't valid JSON", func(t *testing.T) {
		f := newPersistFixture(t, true, 100, 10)
		task := asynq.NewTask(TaskTypeLogUsage, []byte(`{"consumer_id":`))

		if err := HandleLogUsageTask(f.db, f.cache)(f.ctx, task); err == nil {
			t.Fatal("handler returned nil, want an error")
		}

		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d, want 0", rows)
		}
		if total := f.access(t).TotalCost; !approxEqual(total, 10) {
			t.Fatalf("total cost = %v, want it unchanged at 10", total)
		}
	})

	t.Run("returns the error when the usage can't be persisted, so asynq retries", func(t *testing.T) {
		f := newPersistFixture(t, true, 100, 10)
		task, err := newLogUsageTask(f.fullRecord(), 25)
		if err != nil {
			t.Fatalf("newLogUsageTask: %v", err)
		}
		if err := f.db.Exec("DROP TABLE user_provider_accesses").Error; err != nil {
			t.Fatalf("dropping table: %v", err)
		}

		if err := HandleLogUsageTask(f.db, f.cache)(f.ctx, task); err == nil {
			t.Fatal("handler returned nil, want an error (nil would drop the usage for good)")
		}

		if rows := f.usageRows(t); rows != 0 {
			t.Fatalf("usage rows = %d, want 0 (a retry must not find a half-written record)", rows)
		}
	})
}
