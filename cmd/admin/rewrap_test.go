package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"
)

// rewrapKMS is a KMS that can rewrap. It moves a wrapped DEK to its next
// version by appending "-next", and records what it was asked. It can refuse
// chosen DEKs, and run a hook inside every rewrap.
type rewrapKMS struct {
	failingKMS

	refuse map[string]bool      // wrapped DEKs it can't rewrap
	during func(wrapped string) // runs inside every Rewrap

	mu    sync.Mutex
	calls []rewrapCall
}

type rewrapCall struct{ wrapped, keyID string }

func (k *rewrapKMS) Rewrap(_ context.Context, wrapped []byte, keyID string) ([]byte, error) {
	k.mu.Lock()
	k.calls = append(k.calls, rewrapCall{string(wrapped), keyID})
	k.mu.Unlock()

	if k.during != nil {
		k.during(string(wrapped))
	}
	if k.refuse[string(wrapped)] {
		return nil, errors.New("kms refused")
	}
	return append(slices.Clone(wrapped), "-next"...), nil
}

func (k *rewrapKMS) rewrapped() []rewrapCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.calls)
}

func TestRewrapProviderKeysHandler(t *testing.T) {
	rewrap := func(t *testing.T, p *panel, kms keystore.KMSClient) *httptest.ResponseRecorder {
		t.Helper()
		return rewrapWith(t, p, kms, p.cache)
	}
	// store saves a provider key wrapped under the given key id.
	store := func(t *testing.T, p *panel, provider, dek, keyID string) models.ProviderKey {
		t.Helper()
		key := models.ProviderKey{
			Provider:     provider,
			Ciphertext:   []byte("ciphertext of " + dek),
			Nonce:        []byte("nonce of " + dek),
			EncryptedDEK: []byte(dek),
			KMSKeyID:     keyID,
			DisplayHint:  "abcd",
		}
		if err := p.db.Create(&key).Error; err != nil {
			t.Fatalf("creating provider key: %v", err)
		}
		return key
	}
	// stored reads a provider key back, soft-deleted or not.
	stored := func(t *testing.T, p *panel, id uint) models.ProviderKey {
		t.Helper()
		var key models.ProviderKey
		if err := p.db.Unscoped().First(&key, id).Error; err != nil {
			t.Fatalf("reading provider key %d: %v", id, err)
		}
		return key
	}
	wantDEK := func(t *testing.T, p *panel, id uint, want string) {
		t.Helper()
		if got := string(stored(t, p, id).EncryptedDEK); got != want {
			t.Errorf("provider key %d holds DEK %q, want %q", id, got, want)
		}
	}

	t.Run("rewraps every key, soft-deleted ones too, under the key that wrapped it, and touches nothing else", func(t *testing.T) {
		p := newPanel(t)
		kms := &rewrapKMS{}
		openai := store(t, p, "openai", "dek-openai", "key-a")
		anthropic := store(t, p, "anthropic", "dek-anthropic", "key-b")
		gone := store(t, p, "gemini", "dek-gone", "key-c")
		p.db.Delete(&gone)
		before := []models.ProviderKey{stored(t, p, openai.ID), stored(t, p, anthropic.ID), stored(t, p, gone.ID)}

		rec := rewrap(t, p, kms)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		wantDEK(t, p, openai.ID, "dek-openai-next")
		wantDEK(t, p, anthropic.ID, "dek-anthropic-next")
		wantDEK(t, p, gone.ID, "dek-gone-next")
		wantCalls := []rewrapCall{{"dek-openai", "key-a"}, {"dek-anthropic", "key-b"}, {"dek-gone", "key-c"}}
		if got := kms.rewrapped(); !slices.Equal(got, wantCalls) {
			t.Errorf("the KMS was asked to rewrap %v, want %v", got, wantCalls)
		}
		for _, was := range before {
			now := stored(t, p, was.ID)
			if string(now.Ciphertext) != string(was.Ciphertext) || string(now.Nonce) != string(was.Nonce) ||
				now.KMSKeyID != was.KMSKeyID || now.Status != was.Status || now.DeletedAt != was.DeletedAt ||
				!sameInstant(now.UpdatedAt, was.UpdatedAt) {
				t.Errorf("provider key %d = %+v, want only its DEK changed from %+v", was.ID, now, was)
			}
		}
	})

	t.Run("answers with how many keys were rewrapped", func(t *testing.T) {
		p := newPanel(t)
		store(t, p, "openai", "dek-1", "key-a")
		store(t, p, "openai", "dek-2", "key-a")

		rec := rewrap(t, p, &rewrapKMS{})

		body := jsonObject(t, rec)
		wantFields(t, body, "rewrapped", "failed", "failedIds")
		if body["rewrapped"] != float64(2) || body["failed"] != float64(0) {
			t.Errorf("body = %v, want rewrapped 2 and failed 0", body)
		}
		if ids, ok := body["failedIds"].([]any); !ok || len(ids) != 0 {
			t.Errorf("failedIds = %#v, want an empty list", body["failedIds"])
		}
	})

	t.Run("succeeds, rewrapping nothing, when there are no keys", func(t *testing.T) {
		p := newPanel(t)

		rec := rewrap(t, p, &rewrapKMS{})

		if got := decode[api.RewrapProviderKeysResponse](t, rec); rec.Code != http.StatusOK || got.Rewrapped != 0 || got.Failed != 0 {
			t.Errorf("response = %d %+v, want 200 with nothing rewrapped", rec.Code, got)
		}
	})

	t.Run("goes through more keys than it reads at a time", func(t *testing.T) {
		p := newPanel(t)
		kms := &rewrapKMS{}
		total := rewrapBatchSize*2 + 50
		keys := make([]models.ProviderKey, total)
		for i := range keys {
			keys[i] = models.ProviderKey{Provider: "openai", EncryptedDEK: []byte("dek"), KMSKeyID: "key-a"}
		}
		if err := p.db.CreateInBatches(&keys, 100).Error; err != nil {
			t.Fatalf("creating provider keys: %v", err)
		}

		rec := rewrap(t, p, kms)

		if got := decode[api.RewrapProviderKeysResponse](t, rec); got.Rewrapped != total || got.Failed != 0 {
			t.Fatalf("response = %d %+v, want all %d rewrapped", rec.Code, got, total)
		}
		var left int64
		p.db.Model(&models.ProviderKey{}).Where("encrypted_dek <> ?", []byte("dek-next")).Count(&left)
		if left != 0 {
			t.Errorf("%d keys still hold their old DEK", left)
		}
	})

	t.Run("drops every cached session, whichever consumer and key it belongs to", func(t *testing.T) {
		p := newPanel(t)
		store(t, p, "openai", "dek-1", "key-a")
		first := p.cacheSession(t, p.createAPIKey(t, p.createConsumer(t, "acme")).KeyHash)
		second := p.cacheSession(t, p.createAPIKey(t, p.createConsumer(t, "globex")).KeyHash)

		rewrap(t, p, &rewrapKMS{})

		if p.isCached(t, first) || p.isCached(t, second) {
			t.Error("a cached session is still there, holding the old wrapped DEK")
		}
	})

	t.Run("keeps the cached sessions when there was nothing to rewrap", func(t *testing.T) {
		p := newPanel(t)
		session := p.cacheSession(t, p.createAPIKey(t, p.createConsumer(t, "acme")).KeyHash)

		rewrap(t, p, &rewrapKMS{})

		if !p.isCached(t, session) {
			t.Error("the cached sessions were dropped although no key changed")
		}
	})

	t.Run("goes on past a key the KMS refuses, and says which one", func(t *testing.T) {
		p := newPanel(t)
		kms := &rewrapKMS{refuse: map[string]bool{"dek-bad": true}}
		first := store(t, p, "openai", "dek-1", "key-a")
		bad := store(t, p, "openai", "dek-bad", "key-a")
		last := store(t, p, "openai", "dek-3", "key-a")
		session := p.cacheSession(t, p.createAPIKey(t, p.createConsumer(t, "acme")).KeyHash)

		rec := rewrap(t, p, kms)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		got := decode[api.RewrapProviderKeysResponse](t, rec)
		if got.Rewrapped != 2 || got.Failed != 1 || !slices.Equal(got.FailedIDs, []uint{bad.ID}) {
			t.Errorf("response = %+v, want 2 rewrapped and key %d failed", got, bad.ID)
		}
		wantDEK(t, p, first.ID, "dek-1-next")
		wantDEK(t, p, bad.ID, "dek-bad")
		wantDEK(t, p, last.ID, "dek-3-next")
		if p.isCached(t, session) {
			t.Error("the cached sessions were kept although two keys changed")
		}
	})

	t.Run("reports a key it could not store, and leaves it as it was", func(t *testing.T) {
		p := newPanel(t)
		fine := store(t, p, "openai", "dek-1", "key-a")
		stuck := store(t, p, "openai", "dek-2", "key-a")
		p.rejectWrites(t, "provider_keys", "UPDATE", "ROW.id = "+idOf(stuck.ID))

		rec := rewrap(t, p, &rewrapKMS{})

		got := decode[api.RewrapProviderKeysResponse](t, rec)
		if rec.Code != http.StatusInternalServerError || got.Rewrapped != 1 || !slices.Equal(got.FailedIDs, []uint{stuck.ID}) {
			t.Errorf("response = %d %+v, want 500 with key %d failed", rec.Code, got, stuck.ID)
		}
		wantDEK(t, p, fine.ID, "dek-1-next")
		wantDEK(t, p, stuck.ID, "dek-2")
	})

	t.Run("never overwrites a DEK that changed while it was being rewrapped", func(t *testing.T) {
		p := newPanel(t)
		kms := &rewrapKMS{}
		key := store(t, p, "openai", "dek-1", "key-a")
		kms.during = func(string) {
			p.db.Model(&models.ProviderKey{}).Where("id = ?", key.ID).UpdateColumn("encrypted_dek", []byte("dek-changed"))
		}

		rec := rewrap(t, p, kms)

		got := decode[api.RewrapProviderKeysResponse](t, rec)
		if rec.Code != http.StatusInternalServerError || got.Rewrapped != 0 || !slices.Equal(got.FailedIDs, []uint{key.ID}) {
			t.Errorf("response = %d %+v, want 500 with key %d failed", rec.Code, got, key.ID)
		}
		wantDEK(t, p, key.ID, "dek-changed")
	})

	t.Run("turns the request down when the KMS can't rewrap, changing nothing", func(t *testing.T) {
		p := newPanel(t)
		key := store(t, p, "openai", "dek-1", "key-a")
		session := p.cacheSession(t, p.createAPIKey(t, p.createConsumer(t, "acme")).KeyHash)

		// The local KMS has no rotation, so there is nothing to rewrap.
		rec := rewrap(t, p, p.kms)

		wantError(t, rec, http.StatusBadRequest, "The configured KMS does not support rewrapping keys")
		wantDEK(t, p, key.ID, "dek-1")
		if !p.isCached(t, session) {
			t.Error("cached sessions were dropped although nothing was rewrapped")
		}
	})

	t.Run("refuses to start while another rewrap is running, and lets the next one in once it ends", func(t *testing.T) {
		p := newPanel(t)
		key := store(t, p, "openai", "dek-1", "key-a")
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		kms := &rewrapKMS{during: func(string) {
			once.Do(func() { close(entered) })
			<-release
		}}
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() { first <- rewrap(t, p, kms) }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the first rewrap never reached the KMS")
		}

		second := rewrap(t, p, kms)
		close(release)
		firstRec := <-first

		wantError(t, second, http.StatusConflict, "A rewrap is already running")
		if firstRec.Code != http.StatusOK {
			t.Errorf("the running rewrap answered %d, want 200: %s", firstRec.Code, firstRec.Body.String())
		}
		wantDEK(t, p, key.ID, "dek-1-next")
		if !rewrapLockIsFree(t, p) {
			t.Error("the lock is still held after the rewrap ended")
		}
		if third := rewrap(t, p, kms); third.Code != http.StatusOK {
			t.Errorf("a rewrap after the first ended answered %d, want 200: %s", third.Code, third.Body.String())
		}
		wantDEK(t, p, key.ID, "dek-1-next-next")
	})

	t.Run("finishes the whole run when the client goes away half-way", func(t *testing.T) {
		p := newPanel(t)
		first := store(t, p, "openai", "dek-1", "key-a")
		second := store(t, p, "openai", "dek-2", "key-a")
		third := store(t, p, "openai", "dek-3", "key-a")
		ctx, disconnect := context.WithCancel(context.Background())
		defer disconnect()
		kms := &rewrapKMS{during: func(wrapped string) {
			if wrapped == "dek-1" {
				disconnect()
			}
		}}
		req := httptest.NewRequestWithContext(ctx, "POST", "/provider-keys/rewrap", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: p.sessionFor(t, p.admin)})
		mux := http.NewServeMux()
		mux.HandleFunc("POST /provider-keys/rewrap", rewrapProviderKeysHandler(p.cfg, p.db, kms, p.cache))

		mux.ServeHTTP(httptest.NewRecorder(), req)

		wantDEK(t, p, first.ID, "dek-1-next")
		wantDEK(t, p, second.ID, "dek-2-next")
		wantDEK(t, p, third.ID, "dek-3-next")
	})

	t.Run("reports a failure to read the keys", func(t *testing.T) {
		p := newPanel(t)
		store(t, p, "openai", "dek-1", "key-a")
		p.failQueriesSelecting(t, "encrypted_dek")

		wantError(t, rewrap(t, p, &rewrapKMS{}), http.StatusInternalServerError, "Could not rewrap provider keys")
	})

	t.Run("still drops the sessions, and reports the failure, when it can't read the keys half-way", func(t *testing.T) {
		p := newPanel(t)
		total := rewrapBatchSize + 1
		keys := make([]models.ProviderKey, total)
		for i := range keys {
			keys[i] = models.ProviderKey{Provider: "openai", EncryptedDEK: []byte("dek"), KMSKeyID: "key-a"}
		}
		if err := p.db.CreateInBatches(&keys, 100).Error; err != nil {
			t.Fatalf("creating provider keys: %v", err)
		}
		session := p.cacheSession(t, p.createAPIKey(t, p.createConsumer(t, "acme")).KeyHash)
		kms := &rewrapKMS{}
		// The second batch can't be read: the first one has been rewrapped by then.
		kms.during = func(string) {
			if len(kms.rewrapped()) == rewrapBatchSize {
				p.breakTable(t, "provider_keys")
			}
		}

		rec := rewrap(t, p, kms)

		wantError(t, rec, http.StatusInternalServerError, "Could not rewrap provider keys")
		if p.isCached(t, session) {
			t.Error("the sessions were kept although part of the keys had been rewrapped")
		}
	})

	t.Run("says so when the cached sessions could not be dropped, after the keys were rewrapped", func(t *testing.T) {
		p := newPanel(t)
		key := store(t, p, "openai", "dek-1", "key-a")

		rec := rewrapWith(t, p, &rewrapKMS{}, p.deadCache(t))

		wantError(t, rec, http.StatusInternalServerError, "Provider keys rewrapped, but cached sessions could not be invalidated")
		wantDEK(t, p, key.ID, "dek-1-next")
	})
}

// rewrapLockIsFree says whether the rewrap lock can be taken from any session.
// It tries from several connections held open together, since a session can
// take a lock it already holds itself and a single connection might be that one.
func rewrapLockIsFree(t *testing.T, p *panel) bool {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := p.db.DB()
	if err != nil {
		t.Fatalf("getting the connection pool: %v", err)
	}
	var conns []*sql.Conn
	for range 3 {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatalf("opening a connection: %v", err)
		}
		defer conn.Close()
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		var got bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", rewrapLockID).Scan(&got); err != nil {
			t.Fatalf("trying the lock: %v", err)
		}
		if !got {
			return false
		}
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", rewrapLockID)
	}
	return true
}

// rewrapWith calls the rewrap handler as the panel's admin, with the given KMS
// and cache.
func rewrapWith(t *testing.T, p *panel, kms keystore.KMSClient, c *cache.Cache) *httptest.ResponseRecorder {
	t.Helper()
	return p.call(t, rewrapProviderKeysHandler(p.cfg, p.db, kms, c), "POST /provider-keys/rewrap", "/provider-keys/rewrap", "")
}
