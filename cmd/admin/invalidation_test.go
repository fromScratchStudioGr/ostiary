package main

import (
	"context"
	"slices"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
)

func hashes(keys ...models.APIKey) []string {
	var out []string
	for _, k := range keys {
		out = append(out, k.KeyHash)
	}
	slices.Sort(out)
	return out
}

func wantHashes(t *testing.T, got []string, want []string) {
	t.Helper()
	got = slices.Clone(got)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("hashes = %v, want %v", got, want)
	}
}

func TestConsumerKeyHashes(t *testing.T) {
	ctx := context.Background()

	t.Run("returns the hash of every key of the consumer, revoked ones too, and no one else's", func(t *testing.T) {
		p := newPanel(t)
		acme := p.createConsumer(t, "acme")
		globex := p.createConsumer(t, "globex")
		first := p.createAPIKey(t, acme)
		revoked := p.createAPIKey(t, acme)
		p.db.Model(&revoked).Update("status", "revoked")
		p.createAPIKey(t, globex)

		got, err := consumerKeyHashes(ctx, p.db, acme.ID)

		if err != nil {
			t.Fatalf("consumerKeyHashes: %v", err)
		}
		wantHashes(t, got, hashes(first, revoked))
	})

	t.Run("returns nothing for a consumer without keys", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")

		got, err := consumerKeyHashes(ctx, p.db, consumer.ID)

		if err != nil || len(got) != 0 {
			t.Errorf("= %v, %v, want no hashes and no error", got, err)
		}
	})

	t.Run("reports a failure to read the keys", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.breakTable(t, "api_keys")

		if _, err := consumerKeyHashes(ctx, p.db, consumer.ID); err == nil {
			t.Error("expected an error when the keys can't be read")
		}
	})
}

func TestProviderKeyKeyHashes(t *testing.T) {
	ctx := context.Background()

	t.Run("returns the hash of every key of every consumer that uses the provider key, and no one else's", func(t *testing.T) {
		p := newPanel(t)
		providerKey := p.createProviderKey(t, "openai", "1111")
		otherProviderKey := p.createProviderKey(t, "gemini", "2222")
		acme := p.createConsumer(t, "acme")
		globex := p.createConsumer(t, "globex")
		initech := p.createConsumer(t, "initech")
		p.link(t, acme, providerKey, 0)
		p.link(t, globex, providerKey, 0)
		p.link(t, initech, otherProviderKey, 0)
		acmeFirst := p.createAPIKey(t, acme)
		acmeRevoked := p.createAPIKey(t, acme)
		p.db.Model(&acmeRevoked).Update("status", "revoked")
		globexKey := p.createAPIKey(t, globex)
		p.createAPIKey(t, initech)

		got, err := providerKeyKeyHashes(ctx, p.db, providerKey.ID)

		if err != nil {
			t.Fatalf("providerKeyKeyHashes: %v", err)
		}
		wantHashes(t, got, hashes(acmeFirst, acmeRevoked, globexKey))
	})

	t.Run("returns nothing for a provider key nobody uses", func(t *testing.T) {
		p := newPanel(t)
		providerKey := p.createProviderKey(t, "openai", "1111")
		p.createAPIKey(t, p.createConsumer(t, "acme"))

		got, err := providerKeyKeyHashes(ctx, p.db, providerKey.ID)

		if err != nil || len(got) != 0 {
			t.Errorf("= %v, %v, want no hashes and no error", got, err)
		}
	})

	t.Run("reports a failure to read the keys", func(t *testing.T) {
		p := newPanel(t)
		providerKey := p.createProviderKey(t, "openai", "1111")
		p.breakTable(t, "api_keys")

		if _, err := providerKeyKeyHashes(ctx, p.db, providerKey.ID); err == nil {
			t.Error("expected an error when the keys can't be read")
		}
	})
}

func TestInvalidateConsumerSessions(t *testing.T) {
	ctx := context.Background()

	t.Run("drops the cached sessions of the consumer's keys and leaves other keys' alone", func(t *testing.T) {
		p := newPanel(t)
		acme := p.createConsumer(t, "acme")
		globex := p.createConsumer(t, "globex")
		first := p.cacheSession(t, p.createAPIKey(t, acme).KeyHash)
		second := p.cacheSession(t, p.createAPIKey(t, acme).KeyHash)
		other := p.cacheSession(t, p.createAPIKey(t, globex).KeyHash)

		if err := invalidateConsumerSessions(ctx, p.db, p.cache, acme.ID); err != nil {
			t.Fatalf("invalidateConsumerSessions: %v", err)
		}

		if p.isCached(t, first) || p.isCached(t, second) {
			t.Error("a session of the consumer's keys is still cached")
		}
		if !p.isCached(t, other) {
			t.Error("another consumer's session was dropped")
		}
	})

	t.Run("reports a failure to find the keys", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.breakTable(t, "api_keys")

		if err := invalidateConsumerSessions(ctx, p.db, p.cache, consumer.ID); err == nil {
			t.Error("expected an error when the keys can't be read")
		}
	})

	t.Run("reports a failure to reach Redis", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.createAPIKey(t, consumer)

		if err := invalidateConsumerSessions(ctx, p.db, p.deadCache(t), consumer.ID); err == nil {
			t.Error("expected an error when Redis can't be reached")
		}
	})
}

func TestInvalidateProviderKeySessions(t *testing.T) {
	ctx := context.Background()

	t.Run("drops the cached sessions of every consumer using the provider key and leaves the others alone", func(t *testing.T) {
		p := newPanel(t)
		providerKey := p.createProviderKey(t, "openai", "1111")
		acme := p.createConsumer(t, "acme")
		globex := p.createConsumer(t, "globex")
		initech := p.createConsumer(t, "initech")
		p.link(t, acme, providerKey, 0)
		p.link(t, globex, providerKey, 0)
		acmeSession := p.cacheSession(t, p.createAPIKey(t, acme).KeyHash)
		globexSession := p.cacheSession(t, p.createAPIKey(t, globex).KeyHash)
		initechSession := p.cacheSession(t, p.createAPIKey(t, initech).KeyHash)

		if err := invalidateProviderKeySessions(ctx, p.db, p.cache, providerKey.ID); err != nil {
			t.Fatalf("invalidateProviderKeySessions: %v", err)
		}

		if p.isCached(t, acmeSession) || p.isCached(t, globexSession) {
			t.Error("a session of a consumer using the provider key is still cached")
		}
		if !p.isCached(t, initechSession) {
			t.Error("a session of a consumer that doesn't use the provider key was dropped")
		}
	})

	t.Run("reports a failure to find the keys", func(t *testing.T) {
		p := newPanel(t)
		providerKey := p.createProviderKey(t, "openai", "1111")
		p.breakTable(t, "api_keys")

		if err := invalidateProviderKeySessions(ctx, p.db, p.cache, providerKey.ID); err == nil {
			t.Error("expected an error when the keys can't be read")
		}
	})

	t.Run("reports a failure to reach Redis", func(t *testing.T) {
		p := newPanel(t)
		providerKey := p.createProviderKey(t, "openai", "1111")
		consumer := p.createConsumer(t, "acme")
		p.link(t, consumer, providerKey, 0)
		p.createAPIKey(t, consumer)

		if err := invalidateProviderKeySessions(ctx, p.db, p.deadCache(t), providerKey.ID); err == nil {
			t.Error("expected an error when Redis can't be reached")
		}
	})
}
