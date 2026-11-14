package cache

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func session(model string) UserCache {
	return UserCache{
		ModelID:       model,
		ProviderKeyID: 7,
		Ciphertext:    []byte("encrypted-" + model),
		ConsumerID:    3,
	}
}

func TestLocalCacheGetAndSet(t *testing.T) {
	t.Run("returns what was stored for a key", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		want := session("model-a")

		lc.set("key", want)

		got, ok := lc.get("key")
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("get = %+v, %v; want %+v, true", got, ok, want)
		}
	})

	t.Run("misses on a key that was never set", func(t *testing.T) {
		got, ok := newLocalCache(time.Hour).get("missing")

		if ok || !reflect.DeepEqual(got, UserCache{}) {
			t.Errorf("get = %+v, %v; want an empty value and false", got, ok)
		}
	})

	t.Run("keeps different keys apart", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		lc.set("a", session("model-a"))
		lc.set("b", session("model-b"))

		a, _ := lc.get("a")
		b, _ := lc.get("b")

		if a.ModelID != "model-a" || b.ModelID != "model-b" {
			t.Errorf("a = %q, b = %q; want model-a and model-b", a.ModelID, b.ModelID)
		}
	})

	t.Run("replaces the value, and the expiry, when a key is set again", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		lc.entries["key"] = localEntry{value: session("old"), expiresAt: time.Now().Add(-time.Minute)}
		if _, ok := lc.get("key"); ok {
			t.Fatal("the expired entry was returned")
		}

		lc.set("key", session("new"))

		got, ok := lc.get("key")
		if !ok || got.ModelID != "new" {
			t.Errorf("get = %q, %v; want the new value, true", got.ModelID, ok)
		}
	})
}

func TestLocalCacheExpiry(t *testing.T) {
	t.Run("drops an entry once its ttl has run out, and returns no stale value", func(t *testing.T) {
		// A negative ttl is already over when the entry is stored.
		lc := newLocalCache(-time.Second)
		lc.set("key", session("model-a"))

		got, ok := lc.get("key")

		if ok || !reflect.DeepEqual(got, UserCache{}) {
			t.Errorf("get = %+v, %v; want an empty value and false", got, ok)
		}
	})

	t.Run("never keeps an entry past the expiry of its API key", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		value := session("model-a")
		value.KeyExpiresAt = time.Now().Add(-time.Second)
		lc.set("key", value)

		if _, ok := lc.get("key"); ok {
			t.Error("an entry for an expired API key was returned")
		}
	})

	t.Run("expires an entry at its API key's expiry when that comes before the ttl", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		value := session("model-a")
		value.KeyExpiresAt = time.Now().Add(time.Minute)

		lc.set("key", value)

		if got := lc.entries["key"].expiresAt; !got.Equal(value.KeyExpiresAt) {
			t.Errorf("entry expires at %v, want the key's expiry %v", got, value.KeyExpiresAt)
		}
	})

	t.Run("expires an entry at the ttl when its API key lasts longer", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		value := session("model-a")
		value.KeyExpiresAt = time.Now().Add(24 * time.Hour)

		before := time.Now()
		lc.set("key", value)
		after := time.Now()

		got := lc.entries["key"].expiresAt
		if got.Before(before.Add(time.Hour)) || got.After(after.Add(time.Hour)) {
			t.Errorf("entry expires at %v, want one hour from now (between %v and %v)", got, before.Add(time.Hour), after.Add(time.Hour))
		}
	})

	t.Run("applies only the ttl to an entry with no API key expiry", func(t *testing.T) {
		// Entries cached in Redis before the field existed have none, and the
		// zero time is before every real one.
		lc := newLocalCache(time.Hour)
		lc.set("key", session("model-a"))

		if _, ok := lc.get("key"); !ok {
			t.Error("an entry with no key expiry was treated as already expired")
		}
	})
}

func TestLocalCacheDeletePrefix(t *testing.T) {
	const prefix = "ostiary:cache:"

	t.Run("removes the keys that start with the prefix and keeps all others", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		for _, key := range []string{
			prefix + "abc",
			prefix + "abd",
			"ostiary:lastused:abc",
			"legacy:" + prefix + "abc", // contains the prefix, doesn't start with it
			"other",
		} {
			lc.set(key, session(key))
		}

		lc.deletePrefix(prefix)

		for key, wantKept := range map[string]bool{
			prefix + "abc":             false,
			prefix + "abd":             false,
			"ostiary:lastused:abc":     true,
			"legacy:" + prefix + "abc": true,
			"other":                    true,
		} {
			if _, kept := lc.get(key); kept != wantKept {
				t.Errorf("%q kept = %v, want %v", key, kept, wantKept)
			}
		}
	})

	t.Run("removes the key equal to the prefix, and the longer keys that start with it", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		lc.set(prefix+"abc", session("a"))
		lc.set(prefix+"abcd", session("ab"))

		lc.deletePrefix(prefix + "abc")

		if _, ok := lc.get(prefix + "abc"); ok {
			t.Error("the key equal to the prefix was kept")
		}
		if _, ok := lc.get(prefix + "abcd"); ok {
			t.Error("a key that starts with the prefix was kept")
		}
	})

	t.Run("does nothing when no key matches", func(t *testing.T) {
		lc := newLocalCache(time.Hour)
		lc.set("a", session("a"))

		lc.deletePrefix("zzz")

		if _, ok := lc.get("a"); !ok {
			t.Error("a key that doesn't match the prefix was removed")
		}
	})
}

// The cache is shared by every request the gateway serves at once. Without
// its lock this fails: Go stops a program that writes to a map from two
// goroutines, and the race detector flags the unsynchronised reads.
func TestLocalCacheIsSafeForConcurrentUse(t *testing.T) {
	lc := newLocalCache(time.Hour)

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				key := fmt.Sprintf("%s%d", "ostiary:cache:", i%20)
				switch (worker + i) % 3 {
				case 0:
					lc.set(key, session(key))
				case 1:
					lc.get(key)
				default:
					lc.deletePrefix("ostiary:cache:1")
				}
			}
		}()
	}
	wg.Wait()
}
