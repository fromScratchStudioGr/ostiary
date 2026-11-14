package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/metrics"
	"github.com/fromScratchStudioGr/ostiary/internal/payload"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"

	"github.com/redis/go-redis/v9"
)

type UserCache struct {
	ModelID          string
	ProviderKeyID    uint
	ProviderURL      *string
	ProviderEndpoint *providers.EndpointType
	Nonce            []byte
	Ciphertext       []byte
	EncryptedDEK     []byte
	KMSKeyID         string
	ConsumerID       uint
	// KeyExpiresAt caps how long the in-process layer keeps this entry.
	// Zero for entries cached in Redis before this field existed.
	KeyExpiresAt time.Time
}

type contextKey string

const userCacheKey contextKey = "userCache"

const cacheKeyPrefix = "ostiary:cache:"

const sessionTTL = 60 * time.Minute

const lastUsedThrottlePrefix = "ostiary:lastused:"
const lastUsedThrottleWindow = 30 * time.Second

// touchLastUsedAt runs update in the background, but only if no other
// request has already done so for this same throttleKey within the last
// lastUsedThrottleWindow. last_used_at is purely informational, so this
// caps how often a row actually gets written to instead of every request
// that touches it re-writing it — without this, many concurrent requests
// sharing one row (e.g. a provider key assigned to many consumers) would
// serialize on that row's lock and each hold a DB connection while waiting,
// starving the pool for unrelated queries.
func touchLastUsedAt(c *Cache, throttleKey string, update func(ctx context.Context) error) {
	go func() {
		ctx := context.Background()
		ok, err := c.client.SetNX(ctx, lastUsedThrottlePrefix+throttleKey, "1", lastUsedThrottleWindow).Result()
		if err != nil {
			slog.Error("touchLastUsedAt: could not check throttle key", "key", throttleKey, "error", err)
			return
		}
		if !ok {
			return
		}
		if err := update(ctx); err != nil {
			slog.Error("touchLastUsedAt: could not update last_used_at", "key", throttleKey, "error", err)
		}
	}()
}

func (c *Cache) HandleCache(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stopHandleCache := metrics.Observe("handle_cache")

		// The body was read, and its model found, by payload.Read.
		p, ok := payload.FromContext(r)
		if !ok {
			slog.Error("handleCache: the request's payload was not read")
			api.WriteError(w, "Could not read request payload", http.StatusInternalServerError)
			return
		}

		token, _ := auth.ExtractAPIKeyToken(r)

		hashedToken := apiKeyCachePrefix(auth.HashAPIKey(token)) + p.Model

		if c.local != nil {
			if userCache, ok := c.local.get(hashedToken); ok {
				metrics.CacheLocalHit()
				stopHandleCache()
				next(w, r.WithContext(context.WithValue(r.Context(), userCacheKey, userCache)))
				return
			}
		}

		stopRedisGet := metrics.Observe("handle_cache_redis_get")
		value, err := c.client.Get(r.Context(), hashedToken).Bytes()
		stopRedisGet()
		if err != nil && !errors.Is(err, redis.Nil) {
			slog.Error("handleCache: could not read token from Redis", "error", err)
			api.WriteError(w, "Could not read token from Redis", http.StatusInternalServerError)
			return
		}

		var userCache UserCache
		if errors.Is(err, redis.Nil) {
			metrics.CacheMiss()

			model, ok := providers.Models[p.Model]
			if !ok {
				slog.Warn("handleCache: unsupported model", "model_id", p.Model)
				api.WriteError(w, "Unsupported model", http.StatusBadRequest)
				return
			}

			stopDBLookup := metrics.Observe("handle_cache_db_lookup")
			access, keyExpiresAt, ok := getKeyAccess(r, w, c, token, model)
			stopDBLookup()
			if !ok {
				return
			}

			// Expiry is only checked here, on a cache miss, so the entry must
			// not outlive the key.
			// A key that expires between the check in getKeyAccess and here still
			// gets this request through. Redis would treat a non-positive TTL as
			// "no expiry", so the floor keeps that entry from living forever.
			ttl := max(min(sessionTTL, time.Until(keyExpiresAt)), time.Millisecond)

			userCache = UserCache{
				ModelID:          model.Id,
				ProviderKeyID:    access.ProviderKeyID,
				ProviderURL:      &model.BaseUrl,
				ProviderEndpoint: &model.Endpoint,
				Nonce:            access.ProviderKey.Nonce,
				Ciphertext:       access.ProviderKey.Ciphertext,
				EncryptedDEK:     access.ProviderKey.EncryptedDEK,
				KMSKeyID:         access.ProviderKey.KMSKeyID,
				ConsumerID:       access.ConsumerID,
				KeyExpiresAt:     keyExpiresAt,
			}

			encoded, _ := json.Marshal(userCache)
			stopRedisSet := metrics.Observe("handle_cache_redis_set")
			err = c.client.Set(r.Context(), hashedToken, encoded, ttl).Err()
			stopRedisSet()
			if err != nil {
				slog.Error("handleCache: could not store token in Redis", "consumer_id", userCache.ConsumerID, "error", err)
				api.WriteError(w, "Could not store token in Redis", http.StatusInternalServerError)
				return
			}
		} else {
			metrics.CacheHit()

			stopDecodeEntry := metrics.Observe("handle_cache_decode_entry")
			err = json.Unmarshal(value, &userCache)
			stopDecodeEntry()
			if err != nil {
				slog.Error("handleCache: could not parse cached user data", "error", err)
				api.WriteError(w, "Could not parse cached user data", http.StatusInternalServerError)
				return
			}
		}

		if c.local != nil {
			c.local.set(hashedToken, userCache)
		}

		stopHandleCache()

		next(w, r.WithContext(context.WithValue(r.Context(), userCacheKey, userCache)))
	}
}

func getKeyAccess(r *http.Request, w http.ResponseWriter, c *Cache, token string, model providers.Model) (models.UserProviderAccess, time.Time, bool) {
	var apiKey models.APIKey
	if err := c.db.WithContext(r.Context()).Where("key_hash = ?", auth.HashAPIKey(token)).First(&apiKey).Error; err != nil {
		slog.Warn("getKeyAccess: api key not found", "error", err)
		api.WriteError(w, "Invalid API key", http.StatusUnauthorized)
		return models.UserProviderAccess{}, time.Time{}, false
	}

	if apiKey.Status != "active" {
		slog.Warn("getKeyAccess: api key is revoked", "api_key_id", apiKey.ID)
		api.WriteError(w, "API key revoked", http.StatusUnauthorized)
		return models.UserProviderAccess{}, time.Time{}, false
	}

	if time.Now().After(apiKey.ExpiresAt) {
		slog.Warn("getKeyAccess: api key expired", "api_key_id", apiKey.ID)
		api.WriteError(w, "API key expired", http.StatusUnauthorized)
		return models.UserProviderAccess{}, time.Time{}, false
	}

	var access models.UserProviderAccess
	err := c.db.WithContext(r.Context()).
		Joins("JOIN provider_keys ON provider_keys.id = user_provider_accesses.provider_key_id").
		Where("user_provider_accesses.consumer_id = ? AND provider_keys.provider = ?", apiKey.ConsumerID, model.Provider).
		Preload("ProviderKey").
		Preload("Consumer").
		First(&access).Error
	if err != nil {
		slog.Error("getKeyAccess: no provider key found for consumer", "consumer_id", apiKey.ConsumerID, "provider", model.Provider, "error", err)
		api.WriteError(w, "No provider key found for consumer", http.StatusForbidden)
		return access, time.Time{}, false
	}

	if access.Consumer.Status != "active" {
		slog.Warn("getKeyAccess: consumer is revoked", "consumer_id", access.ConsumerID)
		api.WriteError(w, "Consumer access revoked", http.StatusForbidden)
		return access, time.Time{}, false
	}

	if access.UsageLimit > 0 && access.TotalCost > access.UsageLimit {
		slog.Warn("getKeyAccess: usage limit exceeded", "consumer_id", access.ConsumerID, "provider_key_id", access.ProviderKeyID, "total_cost", access.TotalCost, "usage_limit", access.UsageLimit)
		api.WriteError(w, "Usage limit exceeded", http.StatusForbidden)
		return access, time.Time{}, false
	}

	if access.ProviderKey.Status != "active" {
		slog.Warn("getKeyAccess: provider key is inactive", "consumer_id", access.ConsumerID, "provider_key_id", access.ProviderKeyID)
		api.WriteError(w, "Provider key is inactive", http.StatusForbidden)
		return access, time.Time{}, false
	}

	if access.Status != "active" {
		slog.Warn("getKeyAccess: access is inactive", "consumer_id", access.ConsumerID, "provider_key_id", access.ProviderKeyID)
		api.WriteError(w, "Access key is inactive", http.StatusForbidden)
		return access, time.Time{}, false
	}

	touchLastUsedAt(c, fmt.Sprintf("providerkey:%d", access.ProviderKeyID), func(ctx context.Context) error {
		return c.db.WithContext(ctx).
			Model(&models.ProviderKey{}).
			Where("id = ?", access.ProviderKeyID).
			Update("last_used_at", time.Now()).Error
	})

	touchLastUsedAt(c, fmt.Sprintf("apikey:%d", apiKey.ID), func(ctx context.Context) error {
		return c.db.WithContext(ctx).
			Model(&models.APIKey{}).
			Where("id = ?", apiKey.ID).
			Update("last_used_at", time.Now()).Error
	})

	return access, apiKey.ExpiresAt, true
}

func UserCacheFromContext(r *http.Request) UserCache {
	userCache, _ := r.Context().Value(userCacheKey).(UserCache)
	return userCache
}
