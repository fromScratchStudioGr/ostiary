package cache

import (
	"strings"
	"sync"
	"time"
)

// localCache is a short-lived, in-process layer in front of Redis for
// handle_cache's session entries, keyed by the same key as Redis. Redis stays
// the source of truth: invalidation only deletes from Redis, so a revocation
// done elsewhere (e.g. by the admin process) takes effect here once the local
// entry's TTL runs out. Entries hold the same encrypted key material as
// Redis, never plaintext.

type localCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]localEntry
}

type localEntry struct {
	value     UserCache
	expiresAt time.Time
}

func newLocalCache(ttl time.Duration) *localCache {
	return &localCache{
		ttl:     ttl,
		entries: make(map[string]localEntry),
	}
}

func (l *localCache) get(key string) (UserCache, bool) {
	l.mu.RLock()
	e, ok := l.entries[key]
	l.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return UserCache{}, false
	}
	return e.value, true
}

func (l *localCache) set(key string, value UserCache) {
	expiresAt := time.Now().Add(l.ttl)
	if !value.KeyExpiresAt.IsZero() && value.KeyExpiresAt.Before(expiresAt) {
		expiresAt = value.KeyExpiresAt
	}

	l.mu.Lock()
	l.entries[key] = localEntry{value: value, expiresAt: expiresAt}
	l.mu.Unlock()
}

func (l *localCache) delete(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

func (l *localCache) deletePrefix(prefix string) {
	l.mu.Lock()
	for key := range l.entries {
		if strings.HasPrefix(key, prefix) {
			delete(l.entries, key)
		}
	}
	l.mu.Unlock()
}
