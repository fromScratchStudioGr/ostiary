package cache

import (
	"context"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type Cache struct {
	client *redis.Client
	db     *gorm.DB
	kms    keystore.KMSClient
	local  *localCache // nil when the in-process layer is disabled
}

// New connects to Redis. localTTL > 0 enables the in-process layer in front
// of Redis for handle_cache, with entries kept for at most localTTL; 0
// disables it.
func New(addr, username, password string, poolSize int, localTTL time.Duration, db *gorm.DB, kms keystore.KMSClient) (*Cache, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Username: username,
		Password: password,
		PoolSize: poolSize,
	})

	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}

	c := &Cache{client: client, db: db, kms: kms}
	if localTTL > 0 {
		c.local = newLocalCache(localTTL)
	}
	return c, nil
}

func (c *Cache) Close() error {
	return c.client.Close()
}

// apiKeyCachePrefix is the common prefix of every cached session for one API
// key (one entry per model: prefix + model). It's built from the key's hash,
// which the admin also has in the DB, so entries can be found without the raw key.
func apiKeyCachePrefix(keyHash string) string {
	return cacheKeyPrefix + keyHash + ":"
}

// InvalidateAPIKeys drops the cached sessions of the given API keys (by key
// hash) across all models.
func (c *Cache) InvalidateAPIKeys(ctx context.Context, keyHashes []string) error {
	for _, keyHash := range keyHashes {
		if c.local != nil {
			c.local.deletePrefix(apiKeyCachePrefix(keyHash))
		}

		if err := c.unlinkMatching(ctx, apiKeyCachePrefix(keyHash)+"*"); err != nil {
			return err
		}
	}
	return nil
}

// InvalidateAllSessions drops every cached session from Redis, for all API
// keys and models. The local layer isn't touched: it lives in each gateway
// process, out of reach of this one, and its entries expire on their own.
func (c *Cache) InvalidateAllSessions(ctx context.Context) error {
	return c.unlinkMatching(ctx, cacheKeyPrefix+"*")
}

// unlinkMatching removes the Redis keys matching pattern, in batches so a
// large match never becomes one huge command.
func (c *Cache) unlinkMatching(ctx context.Context, pattern string) error {
	const batchSize = 500

	var keys []string
	iter := c.client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
		if len(keys) == batchSize {
			if err := c.client.Unlink(ctx, keys...).Err(); err != nil {
				return err
			}
			keys = keys[:0]
		}
	}
	if err := iter.Err(); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	return c.client.Unlink(ctx, keys...).Err()
}

// InvalidateSession drops the cached session of one API key, given its raw
// token, for one model, from Redis and the local layer.
func (c *Cache) InvalidateSession(ctx context.Context, token, model string) error {
	key := apiKeyCachePrefix(auth.HashAPIKey(token)) + model
	if c.local != nil {
		c.local.delete(key)
	}
	return c.client.Unlink(ctx, key).Err()
}

func (c *Cache) InvalidateConsumer(ctx context.Context, consumerID uint) error {
	var keyHashes []string
	if err := c.db.WithContext(ctx).Model(&models.APIKey{}).Where("consumer_id = ?", consumerID).Pluck("key_hash", &keyHashes).Error; err != nil {
		return err
	}
	return c.InvalidateAPIKeys(ctx, keyHashes)
}

func (c *Cache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}
