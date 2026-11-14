package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"
)

func TestRegisterKeyHandler(t *testing.T) {
	const secret = "sk-live-SECRET-wxyz"

	t.Run("stores the key sealed, so only the KMS can open it, with its last four characters as the hint", func(t *testing.T) {
		p := newPanel(t)

		rec := p.call(t, registerKeyHandler(p.cfg, p.db, p.kms), "POST /provider-keys", "/provider-keys",
			`{"provider":"openai","apiKey":"`+secret+`"}`)

		if rec.Code != http.StatusCreated || rec.Body.String() != "Provider key stored for openai" {
			t.Fatalf("response = %d %q, want 201 \"Provider key stored for openai\"", rec.Code, rec.Body.String())
		}
		var stored models.ProviderKey
		if err := p.db.First(&stored).Error; err != nil {
			t.Fatalf("nothing was stored: %v", err)
		}
		if stored.Provider != "openai" || stored.Status != "active" || stored.DisplayHint != "wxyz" {
			t.Errorf("stored = provider %q, status %q, hint %q, want openai, active, wxyz", stored.Provider, stored.Status, stored.DisplayHint)
		}
		if strings.Contains(string(stored.Ciphertext), secret) || strings.Contains(string(stored.EncryptedDEK), secret) {
			t.Error("the key is readable in what was stored")
		}
		if stored.KMSKeyID == "" || len(stored.Nonce) == 0 || len(stored.EncryptedDEK) == 0 {
			t.Errorf("the envelope is incomplete: key id %q, nonce %d bytes, wrapped DEK %d bytes", stored.KMSKeyID, len(stored.Nonce), len(stored.EncryptedDEK))
		}
		opened, err := keystore.OpenProviderKey(context.Background(), p.kms, stored.Ciphertext, stored.Nonce, stored.EncryptedDEK, stored.KMSKeyID)
		if err != nil || string(opened) != secret {
			t.Errorf("opening the stored key = %q (%v), want the key that was registered", opened, err)
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		handler := registerKeyHandler(p.cfg, p.db, p.kms)
		tests := []struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{
			{"not JSON", `nope`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Invalid payload")
			}},
			{"no provider", `{"apiKey":"` + secret + `"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Provider")
			}},
			{"no key", `{"provider":"openai"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "ApiKey")
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, p.call(t, handler, "POST /provider-keys", "/provider-keys", tt.body))
			})
		}

		var n int64
		p.db.Model(&models.ProviderKey{}).Count(&n)
		if n != 0 {
			t.Errorf("%d provider keys were stored by rejected requests", n)
		}
	})

	t.Run("reports a KMS that can't wrap the key, storing nothing", func(t *testing.T) {
		p := newPanel(t)

		rec := p.call(t, registerKeyHandler(p.cfg, p.db, failingKMS{}), "POST /provider-keys", "/provider-keys",
			`{"provider":"openai","apiKey":"`+secret+`"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not seal provider key")
		var n int64
		p.db.Model(&models.ProviderKey{}).Count(&n)
		if n != 0 {
			t.Error("a key was stored although it could not be sealed")
		}
	})

	t.Run("reports a failure to store the key", func(t *testing.T) {
		p := newPanel(t)

		rec := p.call(t, registerKeyHandler(p.cfg, p.db, p.kms), "POST /provider-keys", "/provider-keys",
			`{"provider":"`+strings.Repeat("p", 33)+`","apiKey":"`+secret+`"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not store provider key")
	})

	t.Run("never writes the key to the logs, whatever happens to the request", func(t *testing.T) {
		p := newPanel(t)
		logs := captureLogs(t)
		requests := []struct {
			name    string
			handler http.HandlerFunc
			body    string
		}{
			{"stored", registerKeyHandler(p.cfg, p.db, p.kms), `{"provider":"openai","apiKey":"` + secret + `"}`},
			{"invalid", registerKeyHandler(p.cfg, p.db, p.kms), `{"apiKey":"` + secret + `"}`},
			{"not sealed", registerKeyHandler(p.cfg, p.db, failingKMS{}), `{"provider":"openai","apiKey":"` + secret + `"}`},
			{"not stored", registerKeyHandler(p.cfg, p.db, p.kms), `{"provider":"` + strings.Repeat("p", 33) + `","apiKey":"` + secret + `"}`},
		}
		for _, r := range requests {
			p.call(t, r.handler, "POST /provider-keys", "/provider-keys", r.body)
		}

		if logs.Len() == 0 {
			t.Fatal("nothing was logged, so this checks nothing")
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the plaintext key is in the logs:\n%s", logs.String())
		}
	})
}

func TestAssignKeyHandler(t *testing.T) {
	assign := func(t *testing.T, p *panel, body string) *httptest.ResponseRecorder {
		return p.call(t, assignKeyHandler(p.cfg, p.db, p.cache), "POST /provider-keys/assign", "/provider-keys/assign", body)
	}
	assignBody := func(consumer models.Consumer, key models.ProviderKey, limit string) string {
		return fmt.Sprintf(`{"consumerId":%d,"providerKeyId":%d,"usageLimit":%s}`, consumer.ID, key.ID, limit)
	}

	t.Run("gives the consumer access and drops its cached sessions, so the change takes effect", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		bystander := p.createConsumer(t, "globex")
		key := p.createProviderKey(t, "openai", "wxyz")
		session := p.cacheSession(t, p.createAPIKey(t, consumer).KeyHash)
		bystanderSession := p.cacheSession(t, p.createAPIKey(t, bystander).KeyHash)

		rec := assign(t, p, assignBody(consumer, key, "25.5"))

		want := fmt.Sprintf("Access to openai granted for consumer %d", consumer.ID)
		if rec.Code != http.StatusCreated || rec.Body.String() != want {
			t.Fatalf("response = %d %q, want 201 %q", rec.Code, rec.Body.String(), want)
		}
		var access models.UserProviderAccess
		if err := p.db.Where("consumer_id = ? AND provider_key_id = ?", consumer.ID, key.ID).First(&access).Error; err != nil {
			t.Fatalf("the access was not stored: %v", err)
		}
		if access.Status != "active" || access.UsageLimit != 25.5 || access.TotalCost != 0 {
			t.Errorf("access = status %q, limit %v, cost %v, want active, 25.5, 0", access.Status, access.UsageLimit, access.TotalCost)
		}
		if p.isCached(t, session) {
			t.Error("the consumer's cached session is still in Redis")
		}
		if !p.isCached(t, bystanderSession) {
			t.Error("another consumer's cached session was dropped")
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		key := p.createProviderKey(t, "openai", "wxyz")
		goneConsumer := p.createConsumer(t, "gone")
		p.db.Delete(&goneConsumer)
		goneKey := p.createProviderKey(t, "openai", "gone")
		p.db.Delete(&goneKey)
		p.link(t, consumer, key, 0)
		other := p.createProviderKey(t, "gemini", "1234")
		tests := []struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{
			{"not JSON", `nope`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Invalid payload")
			}},
			{"no consumer", fmt.Sprintf(`{"providerKeyId":%d}`, key.ID), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "ConsumerId")
			}},
			{"no provider key", fmt.Sprintf(`{"consumerId":%d}`, consumer.ID), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "ProviderKeyId")
			}},
			{"a negative usage limit", assignBody(consumer, other, "-1"), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "UsageLimit")
			}},
			{"a provider key that doesn't exist", fmt.Sprintf(`{"consumerId":%d,"providerKeyId":999999}`, consumer.ID), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Provider key not found")
			}},
			{"a provider key that was deleted", assignBody(consumer, goneKey, "0"), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Provider key not found")
			}},
			{"a consumer that doesn't exist", fmt.Sprintf(`{"consumerId":999999,"providerKeyId":%d}`, key.ID), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Consumer not found")
			}},
			{"a consumer that was deleted", assignBody(goneConsumer, key, "0"), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Consumer not found")
			}},
			{"access the consumer already has", assignBody(consumer, key, "0"), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusConflict, "Consumer already has access to this provider key")
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, assign(t, p, tt.body))
			})
		}

		var n int64
		p.db.Model(&models.UserProviderAccess{}).Count(&n)
		if n != 1 {
			t.Errorf("%d accesses are stored, want only the one that existed", n)
		}
	})

	t.Run("reports a failure to store the access", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		key := p.createProviderKey(t, "openai", "wxyz")
		p.rejectWrites(t, "user_provider_accesses", "INSERT", "true")

		wantError(t, assign(t, p, assignBody(consumer, key, "0")), http.StatusInternalServerError, "Could not store access")
	})

	t.Run("says so when the cached sessions could not be dropped, after access was granted", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		key := p.createProviderKey(t, "openai", "wxyz")
		p.createAPIKey(t, consumer)

		rec := p.call(t, assignKeyHandler(p.cfg, p.db, p.deadCache(t)), "POST /provider-keys/assign", "/provider-keys/assign", assignBody(consumer, key, "0"))

		wantError(t, rec, http.StatusInternalServerError, "Access granted, but cached sessions could not be invalidated")
		var n int64
		p.db.Model(&models.UserProviderAccess{}).Count(&n)
		if n != 1 {
			t.Errorf("%d accesses stored, want the one that was granted", n)
		}
	})
}

func TestGetProviderKeysHandler(t *testing.T) {
	list := func(t *testing.T, p *panel) *httptest.ResponseRecorder {
		return p.call(t, getProviderKeysHandler(p.cfg, p.db), "GET /provider-keys", "/provider-keys", "")
	}

	t.Run("lists the provider keys with their hint, status, and how many consumers use them", func(t *testing.T) {
		p := newPanel(t)
		busy := p.createProviderKey(t, "openai", "1111")
		idle := p.createProviderKey(t, "anthropic", "2222")
		revoked := p.createProviderKey(t, "gemini", "3333")
		gone := p.createProviderKey(t, "deepseek", "4444")
		p.link(t, p.createConsumer(t, "acme"), busy, 0)
		p.link(t, p.createConsumer(t, "globex"), busy, 0)
		p.link(t, p.createConsumer(t, "initech"), revoked, 0)
		deletedConsumer := p.createConsumer(t, "umbrella")
		p.link(t, deletedConsumer, busy, 0)
		p.db.Delete(&deletedConsumer)
		p.db.Model(&revoked).Update("status", "revoked")
		p.db.Delete(&gone)

		rec := list(t, p)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		got := map[uint]api.ProviderKeyResponse{}
		for _, k := range decode[[]api.ProviderKeyResponse](t, rec) {
			got[k.ID] = k
			if k.RegisteredAt.IsZero() {
				t.Errorf("key %d comes without a registration time", k.ID)
			}
		}
		if len(got) != 3 {
			t.Fatalf("got %d keys, want 3 (the deleted one is not listed): %+v", len(got), got)
		}
		check := func(key models.ProviderKey, provider, hint, status string, users int64) {
			g := got[key.ID]
			if g.Provider != provider || g.Key != hint || g.Status != status || g.UserCount != users {
				t.Errorf("key %d = %+v, want %s %s %s with %d users", key.ID, g, provider, hint, status, users)
			}
		}
		check(busy, "openai", "1111", "active", 2)
		check(idle, "anthropic", "2222", "active", 0)
		check(revoked, "gemini", "3333", "revoked", 1)
	})

	t.Run("answers with exactly the fields the client expects, with the stored values", func(t *testing.T) {
		p := newPanel(t)
		fresh := p.createProviderKey(t, "openai", "1111")
		used := p.createProviderKey(t, "anthropic", "2222")
		usedAt := time.Now().Add(-time.Hour)
		p.db.Model(&used).Updates(map[string]any{"status": "revoked", "last_used_at": usedAt})
		p.db.First(&fresh, fresh.ID)
		p.db.First(&used, used.ID)
		p.link(t, p.createConsumer(t, "acme"), fresh, 0)
		p.link(t, p.createConsumer(t, "globex"), fresh, 0)

		rec := list(t, p)

		objects := jsonList(t, rec)
		if len(objects) != 2 {
			t.Fatalf("got %d provider keys, want 2: %s", len(objects), rec.Body.String())
		}
		byID := map[float64]map[string]any{}
		for _, object := range objects {
			wantFields(t, object, "id", "key", "lastUsedAt", "provider", "registeredAt", "status", "userCount")
			byID[object["id"].(float64)] = object
		}
		for _, key := range []models.ProviderKey{fresh, used} {
			object := byID[float64(key.ID)]
			if object == nil {
				t.Fatalf("provider key %d is not in the response", key.ID)
			}
			if object["provider"] != key.Provider || object["key"] != key.DisplayHint || object["status"] != key.Status || !sameInstant(timeField(t, object, "registeredAt"), key.CreatedAt) {
				t.Errorf("provider key %d = %v, want %s, hint %q, status %q, registered at %v", key.ID, object, key.Provider, key.DisplayHint, key.Status, key.CreatedAt)
			}
		}
		if byID[float64(fresh.ID)]["userCount"] != float64(2) || byID[float64(used.ID)]["userCount"] != float64(0) {
			t.Errorf("userCount = %v and %v, want 2 and 0", byID[float64(fresh.ID)]["userCount"], byID[float64(used.ID)]["userCount"])
		}
		if value, present := byID[float64(fresh.ID)]["lastUsedAt"]; !present || value != nil {
			t.Errorf("a key never used has lastUsedAt = %v (present: %v), want null", value, present)
		}
		if !sameInstant(timeField(t, byID[float64(used.ID)], "lastUsedAt"), usedAt) {
			t.Errorf("lastUsedAt = %v, want %v", byID[float64(used.ID)]["lastUsedAt"], usedAt)
		}
	})

	t.Run("answers with an empty list, not null, when there are none", func(t *testing.T) {
		p := newPanel(t)

		rec := list(t, p)

		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Errorf("response = %d %q, want 200 []", rec.Code, rec.Body.String())
		}
	})

	t.Run("reports a failure to read them", func(t *testing.T) {
		p := newPanel(t)
		p.breakTable(t, "provider_keys")

		wantError(t, list(t, p), http.StatusInternalServerError, "Could not fetch provider keys")
	})
}

func TestGetProviderKeyConsumersHandler(t *testing.T) {
	list := func(t *testing.T, p *panel, id string) *httptest.ResponseRecorder {
		return p.call(t, getProviderKeyConsumersHandler(p.cfg, p.db), "GET /provider-keys/{id}/consumers", "/provider-keys/"+id+"/consumers", "")
	}

	t.Run("lists the consumers that use the key, with their access, and only those", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")
		otherKey := p.createProviderKey(t, "gemini", "2222")
		acme := p.createConsumer(t, "acme")
		globex := p.createConsumer(t, "globex")
		p.link(t, acme, key, 50)
		p.link(t, globex, key, 0)
		p.link(t, p.createConsumer(t, "initech"), otherKey, 5)
		deletedConsumer := p.createConsumer(t, "umbrella")
		p.link(t, deletedConsumer, key, 0)
		p.db.Delete(&deletedConsumer)
		p.db.Model(&models.UserProviderAccess{}).Where("consumer_id = ?", globex.ID).Updates(map[string]any{"status": "revoked", "total_cost": 2.25})

		rec := list(t, p, idOf(key.ID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		got := map[uint]api.ProviderKeyConsumerResponse{}
		for _, c := range decode[[]api.ProviderKeyConsumerResponse](t, rec) {
			got[c.ID] = c
		}
		want := map[uint]api.ProviderKeyConsumerResponse{
			acme.ID:   {ID: acme.ID, Name: "acme", Status: "active", TotalCost: 0, UsageLimit: 50},
			globex.ID: {ID: globex.ID, Name: "globex", Status: "revoked", TotalCost: 2.25, UsageLimit: 0},
		}
		if len(got) != len(want) || got[acme.ID] != want[acme.ID] || got[globex.ID] != want[globex.ID] {
			t.Errorf("consumers = %+v, want %+v", got, want)
		}
	})

	t.Run("answers with exactly the fields the client expects, with the stored values", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")
		consumer := p.createConsumer(t, "acme")
		p.link(t, consumer, key, 50)
		p.db.Model(&models.UserProviderAccess{}).Where("consumer_id = ?", consumer.ID).Updates(map[string]any{"status": "revoked", "total_cost": 2.25})

		objects := jsonList(t, list(t, p, idOf(key.ID)))

		if len(objects) != 1 {
			t.Fatalf("got %d consumers, want 1: %v", len(objects), objects)
		}
		wantFields(t, objects[0], "id", "name", "status", "totalCost", "usageLimit")
		want := map[string]any{"id": float64(consumer.ID), "name": "acme", "status": "revoked", "totalCost": 2.25, "usageLimit": float64(50)}
		for field, value := range want {
			if objects[0][field] != value {
				t.Errorf("%s = %v, want %v", field, objects[0][field], value)
			}
		}
	})

	t.Run("answers with an empty list, not null, for a key nobody uses", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")

		rec := list(t, p, idOf(key.ID))

		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Errorf("response = %d %q, want 200 []", rec.Code, rec.Body.String())
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		gone := p.createProviderKey(t, "openai", "gone")
		p.db.Delete(&gone)
		tests := []struct {
			name    string
			id      string
			status  int
			message string
		}{
			{"an id that is not a number", "abc", http.StatusBadRequest, "Invalid provider key id"},
			{"a key that doesn't exist", "999999", http.StatusNotFound, "Provider key not found"},
			{"a key that was deleted", idOf(gone.ID), http.StatusNotFound, "Provider key not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantError(t, list(t, p, tt.id), tt.status, tt.message)
			})
		}
	})

	t.Run("reports a failure to read them", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")
		p.breakTable(t, "user_provider_accesses")

		wantError(t, list(t, p, idOf(key.ID)), http.StatusInternalServerError, "Could not fetch consumers")
	})
}

func TestUpdateProviderKeyStatusHandler(t *testing.T) {
	update := func(t *testing.T, p *panel, body string) *httptest.ResponseRecorder {
		return p.call(t, updateProviderKeyStatusHandler(p.cfg, p.db, p.cache), "PATCH /provider-keys/status", "/provider-keys/status", body)
	}
	statusOf := func(p *panel, key models.ProviderKey) string {
		var row models.ProviderKey
		p.db.First(&row, key.ID)
		return row.Status
	}

	t.Run("changes the status and drops the cached sessions of every consumer using the key", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")
		user := p.createConsumer(t, "acme")
		bystander := p.createConsumer(t, "globex")
		p.link(t, user, key, 0)
		userSession := p.cacheSession(t, p.createAPIKey(t, user).KeyHash)
		bystanderSession := p.cacheSession(t, p.createAPIKey(t, bystander).KeyHash)

		rec := update(t, p, fmt.Sprintf(`{"providerKeyId":%d,"status":"revoked"}`, key.ID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got := statusOf(p, key); got != "revoked" {
			t.Errorf("status = %q, want revoked", got)
		}
		if p.isCached(t, userSession) {
			t.Error("a consumer of the key still has its session cached, so the revoked key keeps being used")
		}
		if !p.isCached(t, bystanderSession) {
			t.Error("a consumer that doesn't use the key lost its cached session")
		}

		rec = update(t, p, fmt.Sprintf(`{"providerKeyId":%d,"status":"active"}`, key.ID))

		if rec.Code != http.StatusOK || statusOf(p, key) != "active" {
			t.Errorf("reactivating = %d, status %q, want 200 and active", rec.Code, statusOf(p, key))
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")
		gone := p.createProviderKey(t, "openai", "gone")
		p.db.Delete(&gone)
		id := idOf(key.ID)
		tests := []struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{
			{"not JSON", `nope`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Invalid payload")
			}},
			{"no key", `{"status":"revoked"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "ProviderKeyId")
			}},
			{"no status", `{"providerKeyId":` + id + `}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
			{"a status that doesn't exist", `{"providerKeyId":` + id + `,"status":"invalid"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
			{"a key that doesn't exist", `{"providerKeyId":999999,"status":"revoked"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Provider key not found")
			}},
			{"a key that was deleted", `{"providerKeyId":` + idOf(gone.ID) + `,"status":"revoked"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Provider key not found")
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, update(t, p, tt.body))
			})
		}

		if got := statusOf(p, key); got != "active" {
			t.Errorf("a rejected request left the status at %q", got)
		}
	})

	t.Run("reports a failure to store the status", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")
		p.rejectWrites(t, "provider_keys", "UPDATE", "true")

		rec := update(t, p, fmt.Sprintf(`{"providerKeyId":%d,"status":"revoked"}`, key.ID))

		wantError(t, rec, http.StatusInternalServerError, "Could not update status")
	})

	t.Run("says so when the cached sessions could not be dropped, after the status changed", func(t *testing.T) {
		p := newPanel(t)
		key := p.createProviderKey(t, "openai", "1111")
		consumer := p.createConsumer(t, "acme")
		p.link(t, consumer, key, 0)
		p.createAPIKey(t, consumer)

		rec := p.call(t, updateProviderKeyStatusHandler(p.cfg, p.db, p.deadCache(t)), "PATCH /provider-keys/status", "/provider-keys/status",
			fmt.Sprintf(`{"providerKeyId":%d,"status":"revoked"}`, key.ID))

		wantError(t, rec, http.StatusInternalServerError, "Status updated, but cached sessions could not be invalidated")
		if got := statusOf(p, key); got != "revoked" {
			t.Errorf("status = %q, want revoked", got)
		}
	})
}

// linkFixture is two consumers each linked to two provider keys, so a handler
// that touches the wrong link shows.
type linkFixture struct {
	p                 *panel
	acme, globex      models.Consumer
	openai, anthropic models.ProviderKey
	acmeSession       string
	globexSession     string
}

func newLinkFixture(t *testing.T) linkFixture {
	t.Helper()
	p := newPanel(t)
	f := linkFixture{
		p:         p,
		acme:      p.createConsumer(t, "acme"),
		globex:    p.createConsumer(t, "globex"),
		anthropic: p.createProviderKey(t, "anthropic", "2222"),
		openai:    p.createProviderKey(t, "openai", "1111"),
	}
	for _, c := range []models.Consumer{f.acme, f.globex} {
		p.link(t, c, f.openai, 10)
		p.link(t, c, f.anthropic, 10)
	}
	f.acmeSession = p.cacheSession(t, p.createAPIKey(t, f.acme).KeyHash)
	f.globexSession = p.cacheSession(t, p.createAPIKey(t, f.globex).KeyHash)
	return f
}

func (f linkFixture) access(consumer models.Consumer, key models.ProviderKey) (models.UserProviderAccess, bool) {
	var access models.UserProviderAccess
	err := f.p.db.Where("consumer_id = ? AND provider_key_id = ?", consumer.ID, key.ID).First(&access).Error
	return access, err == nil
}

func (f linkFixture) body(consumer models.Consumer, key models.ProviderKey, extra string) string {
	return fmt.Sprintf(`{"consumerId":%d,"providerKeyId":%d%s}`, consumer.ID, key.ID, extra)
}

// linkRequestCases are the ways a request about one consumer's link to one
// provider key can be refused, whatever it asks for.
func linkRequestCases(f linkFixture, extra string) []struct {
	name string
	body string
	want func(t *testing.T, rec *httptest.ResponseRecorder)
} {
	return []struct {
		name string
		body string
		want func(t *testing.T, rec *httptest.ResponseRecorder)
	}{
		{"not JSON", `nope`, func(t *testing.T, rec *httptest.ResponseRecorder) {
			wantError(t, rec, http.StatusBadRequest, "Invalid payload")
		}},
		{"no consumer", fmt.Sprintf(`{"providerKeyId":%d%s}`, f.openai.ID, extra), func(t *testing.T, rec *httptest.ResponseRecorder) {
			wantValidationError(t, rec, "ConsumerId")
		}},
		{"no provider key", fmt.Sprintf(`{"consumerId":%d%s}`, f.acme.ID, extra), func(t *testing.T, rec *httptest.ResponseRecorder) {
			wantValidationError(t, rec, "ProviderKeyId")
		}},
		{"a consumer that has no access to the key", fmt.Sprintf(`{"consumerId":999999,"providerKeyId":%d%s}`, f.openai.ID, extra), func(t *testing.T, rec *httptest.ResponseRecorder) {
			wantError(t, rec, http.StatusNotFound, "Consumer provider access not found")
		}},
		{"a key the consumer has no access to", fmt.Sprintf(`{"consumerId":%d,"providerKeyId":999999%s}`, f.acme.ID, extra), func(t *testing.T, rec *httptest.ResponseRecorder) {
			wantError(t, rec, http.StatusNotFound, "Consumer provider access not found")
		}},
	}
}

func TestUpdateProviderKeyConsumerStatusHandler(t *testing.T) {
	update := func(t *testing.T, f linkFixture, body string) *httptest.ResponseRecorder {
		return f.p.call(t, updateProviderKeyConsumerStatusHandler(f.p.cfg, f.p.db, f.p.cache), "PATCH /provider-keys/consumers/status", "/provider-keys/consumers/status", body)
	}

	t.Run("changes the status of that one link and drops that consumer's cached sessions", func(t *testing.T) {
		f := newLinkFixture(t)

		rec := update(t, f, f.body(f.acme, f.openai, `,"status":"revoked"`))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		statuses := map[string]string{}
		for name, link := range map[string]struct {
			c models.Consumer
			k models.ProviderKey
		}{"acme/openai": {f.acme, f.openai}, "acme/anthropic": {f.acme, f.anthropic}, "globex/openai": {f.globex, f.openai}} {
			access, _ := f.access(link.c, link.k)
			statuses[name] = access.Status
		}
		if statuses["acme/openai"] != "revoked" || statuses["acme/anthropic"] != "active" || statuses["globex/openai"] != "active" {
			t.Errorf("statuses = %v, want only acme/openai revoked", statuses)
		}
		if f.p.isCached(t, f.acmeSession) {
			t.Error("the consumer's cached session is still in Redis")
		}
		if !f.p.isCached(t, f.globexSession) {
			t.Error("another consumer's cached session was dropped")
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		f := newLinkFixture(t)
		cases := append(linkRequestCases(f, `,"status":"revoked"`), []struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{
			{"no status", f.body(f.acme, f.openai, ``), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
			{"a status that doesn't exist", f.body(f.acme, f.openai, `,"status":"paused"`), func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
		}...)
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, update(t, f, tt.body))
			})
		}

		if access, _ := f.access(f.acme, f.openai); access.Status != "active" {
			t.Errorf("a rejected request left the status at %q", access.Status)
		}
	})

	t.Run("reports a failure to store the status", func(t *testing.T) {
		f := newLinkFixture(t)
		f.p.rejectWrites(t, "user_provider_accesses", "UPDATE", "true")

		rec := update(t, f, f.body(f.acme, f.openai, `,"status":"revoked"`))

		wantError(t, rec, http.StatusInternalServerError, "Could not update consumer provider status")
	})

	t.Run("says so when the cached sessions could not be dropped, after the status changed", func(t *testing.T) {
		f := newLinkFixture(t)

		rec := f.p.call(t, updateProviderKeyConsumerStatusHandler(f.p.cfg, f.p.db, f.p.deadCache(t)), "PATCH /provider-keys/consumers/status", "/provider-keys/consumers/status",
			f.body(f.acme, f.openai, `,"status":"revoked"`))

		wantError(t, rec, http.StatusInternalServerError, "Status updated, but cached sessions could not be invalidated")
		if access, _ := f.access(f.acme, f.openai); access.Status != "revoked" {
			t.Errorf("status = %q, want revoked", access.Status)
		}
	})
}

func TestDeleteProviderKeyConsumerHandler(t *testing.T) {
	remove := func(t *testing.T, f linkFixture, body string) *httptest.ResponseRecorder {
		return f.p.call(t, deleteProviderKeyConsumerHandler(f.p.cfg, f.p.db, f.p.cache), "DELETE /provider-keys/consumers", "/provider-keys/consumers", body)
	}

	t.Run("removes that one link and drops that consumer's cached sessions", func(t *testing.T) {
		f := newLinkFixture(t)

		rec := remove(t, f, f.body(f.acme, f.openai, ``))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if _, ok := f.access(f.acme, f.openai); ok {
			t.Error("the link is still there")
		}
		for name, link := range map[string]struct {
			c models.Consumer
			k models.ProviderKey
		}{"acme/anthropic": {f.acme, f.anthropic}, "globex/openai": {f.globex, f.openai}, "globex/anthropic": {f.globex, f.anthropic}} {
			if _, ok := f.access(link.c, link.k); !ok {
				t.Errorf("the %s link was removed too", name)
			}
		}
		if f.p.isCached(t, f.acmeSession) {
			t.Error("the consumer's cached session is still in Redis, so it keeps using the key it lost")
		}
		if !f.p.isCached(t, f.globexSession) {
			t.Error("another consumer's cached session was dropped")
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		f := newLinkFixture(t)
		for _, tt := range linkRequestCases(f, ``) {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, remove(t, f, tt.body))
			})
		}

		var n int64
		f.p.db.Model(&models.UserProviderAccess{}).Count(&n)
		if n != 4 {
			t.Errorf("%d links remain, want all 4", n)
		}
	})

	t.Run("reports a failure to remove it", func(t *testing.T) {
		f := newLinkFixture(t)
		f.p.rejectWrites(t, "user_provider_accesses", "DELETE", "true")

		wantError(t, remove(t, f, f.body(f.acme, f.openai, ``)), http.StatusInternalServerError, "Could not delete consumer provider key link")
	})

	t.Run("says so when the cached sessions could not be dropped, after the link was removed", func(t *testing.T) {
		f := newLinkFixture(t)

		rec := f.p.call(t, deleteProviderKeyConsumerHandler(f.p.cfg, f.p.db, f.p.deadCache(t)), "DELETE /provider-keys/consumers", "/provider-keys/consumers", f.body(f.acme, f.openai, ``))

		wantError(t, rec, http.StatusInternalServerError, "Link deleted, but cached sessions could not be invalidated")
		if _, ok := f.access(f.acme, f.openai); ok {
			t.Error("the link is still there")
		}
	})
}

func TestUpdateProviderKeyConsumerLimitHandler(t *testing.T) {
	update := func(t *testing.T, f linkFixture, body string) *httptest.ResponseRecorder {
		return f.p.call(t, updateProviderKeyConsumerLimitHandler(f.p.cfg, f.p.db, f.p.cache), "PATCH /provider-keys/consumers/usage", "/provider-keys/consumers/usage", body)
	}

	t.Run("changes the limit of that one link, including to zero, and drops that consumer's cached sessions", func(t *testing.T) {
		f := newLinkFixture(t)

		rec := update(t, f, f.body(f.acme, f.openai, `,"usageLimit":42.5`))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if access, _ := f.access(f.acme, f.openai); access.UsageLimit != 42.5 {
			t.Errorf("limit = %v, want 42.5", access.UsageLimit)
		}
		if access, _ := f.access(f.acme, f.anthropic); access.UsageLimit != 10 {
			t.Errorf("acme's other link has limit %v, want it left at 10", access.UsageLimit)
		}
		if access, _ := f.access(f.globex, f.openai); access.UsageLimit != 10 {
			t.Errorf("globex's link has limit %v, want it left at 10", access.UsageLimit)
		}
		if f.p.isCached(t, f.acmeSession) {
			t.Error("the consumer's cached session is still in Redis, so the old limit keeps applying")
		}
		if !f.p.isCached(t, f.globexSession) {
			t.Error("another consumer's cached session was dropped")
		}

		rec = update(t, f, f.body(f.acme, f.openai, `,"usageLimit":0`))

		if access, _ := f.access(f.acme, f.openai); rec.Code != http.StatusOK || access.UsageLimit != 0 {
			t.Errorf("setting zero = %d, limit %v, want 200 and 0", rec.Code, access.UsageLimit)
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		f := newLinkFixture(t)
		cases := append(linkRequestCases(f, `,"usageLimit":5`), struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{"a negative limit", f.body(f.acme, f.openai, `,"usageLimit":-1`), func(t *testing.T, rec *httptest.ResponseRecorder) {
			wantValidationError(t, rec, "UsageLimit")
		}})
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, update(t, f, tt.body))
			})
		}

		if access, _ := f.access(f.acme, f.openai); access.UsageLimit != 10 {
			t.Errorf("a rejected request left the limit at %v", access.UsageLimit)
		}
	})

	t.Run("reports a failure to store the limit", func(t *testing.T) {
		f := newLinkFixture(t)
		f.p.rejectWrites(t, "user_provider_accesses", "UPDATE", "true")

		rec := update(t, f, f.body(f.acme, f.openai, `,"usageLimit":5`))

		wantError(t, rec, http.StatusInternalServerError, "Could not update consumer provider usage limit")
	})

	t.Run("says so when the cached sessions could not be dropped, after the limit changed", func(t *testing.T) {
		f := newLinkFixture(t)

		rec := f.p.call(t, updateProviderKeyConsumerLimitHandler(f.p.cfg, f.p.db, f.p.deadCache(t)), "PATCH /provider-keys/consumers/usage", "/provider-keys/consumers/usage",
			f.body(f.acme, f.openai, `,"usageLimit":5`))

		wantError(t, rec, http.StatusInternalServerError, "Usage limit updated, but cached sessions could not be invalidated")
		if access, _ := f.access(f.acme, f.openai); access.UsageLimit != 5 {
			t.Errorf("limit = %v, want 5", access.UsageLimit)
		}
	})
}

func TestDeleteProviderKeyHandler(t *testing.T) {
	remove := func(t *testing.T, f linkFixture, id string) *httptest.ResponseRecorder {
		return f.p.call(t, deleteProviderKeyHandler(f.p.cfg, f.p.db, f.p.cache), "DELETE /provider-keys/{id}", "/provider-keys/"+id, "")
	}
	linkCount := func(f linkFixture, key models.ProviderKey) int64 {
		var n int64
		f.p.db.Model(&models.UserProviderAccess{}).Where("provider_key_id = ?", key.ID).Count(&n)
		return n
	}

	t.Run("soft-deletes the key, removes its links, and drops the cached sessions of the consumers that used it", func(t *testing.T) {
		f := newLinkFixture(t)
		unrelated := f.p.createConsumer(t, "initech")
		unrelatedSession := f.p.cacheSession(t, f.p.createAPIKey(t, unrelated).KeyHash)

		rec := remove(t, f, idOf(f.openai.ID))

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
		}
		var row models.ProviderKey
		if err := f.p.db.Unscoped().First(&row, f.openai.ID).Error; err != nil || !row.DeletedAt.Valid {
			t.Errorf("the key row = %+v (%v), want it kept with deleted_at set", row, err)
		}
		if linkCount(f, f.openai) != 0 {
			t.Error("the key's links are still stored")
		}
		if linkCount(f, f.anthropic) != 2 {
			t.Error("another key's links were removed")
		}
		if f.p.isCached(t, f.acmeSession) || f.p.isCached(t, f.globexSession) {
			t.Error("a consumer that used the key still has its session cached, so it keeps using the deleted key")
		}
		if !f.p.isCached(t, unrelatedSession) {
			t.Error("a consumer that never used the key lost its cached session")
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		f := newLinkFixture(t)
		gone := f.p.createProviderKey(t, "gemini", "gone")
		f.p.db.Delete(&gone)
		tests := []struct {
			name    string
			id      string
			status  int
			message string
		}{
			{"an id that is not a number", "abc", http.StatusBadRequest, "Invalid provider key id"},
			{"a key that doesn't exist", "999999", http.StatusNotFound, "Provider key not found"},
			{"a key that was already deleted", idOf(gone.ID), http.StatusNotFound, "Provider key not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantError(t, remove(t, f, tt.id), tt.status, tt.message)
			})
		}
	})

	t.Run("deletes nothing when it can't delete everything", func(t *testing.T) {
		f := newLinkFixture(t)
		f.p.rejectWrites(t, "provider_keys", "UPDATE", "ROW.id = "+idOf(f.openai.ID))

		rec := remove(t, f, idOf(f.openai.ID))

		wantError(t, rec, http.StatusInternalServerError, "Could not delete provider key")
		if linkCount(f, f.openai) != 2 {
			t.Error("the links were removed although the key wasn't deleted")
		}
		if !f.p.isCached(t, f.acmeSession) {
			t.Error("cached sessions were dropped although nothing was deleted")
		}
	})

	t.Run("deletes nothing when the links can't be removed", func(t *testing.T) {
		f := newLinkFixture(t)
		f.p.rejectWrites(t, "user_provider_accesses", "DELETE", "true")

		wantError(t, remove(t, f, idOf(f.openai.ID)), http.StatusInternalServerError, "Could not delete provider key")
		var n int64
		f.p.db.Model(&models.ProviderKey{}).Where("id = ?", f.openai.ID).Count(&n)
		if n != 1 {
			t.Error("the key was deleted although its links could not be removed")
		}
	})

	t.Run("reports a failure to find the affected API keys, deleting nothing", func(t *testing.T) {
		f := newLinkFixture(t)
		f.p.breakTable(t, "api_keys")

		wantError(t, remove(t, f, idOf(f.openai.ID)), http.StatusInternalServerError, "Could not delete provider key")
		if linkCount(f, f.openai) != 2 {
			t.Error("links were removed although the affected keys could not be found")
		}
	})

	t.Run("says so when the cached sessions could not be dropped, after the delete went through", func(t *testing.T) {
		f := newLinkFixture(t)

		rec := f.p.call(t, deleteProviderKeyHandler(f.p.cfg, f.p.db, f.p.deadCache(t)), "DELETE /provider-keys/{id}", "/provider-keys/"+idOf(f.openai.ID), "")

		wantError(t, rec, http.StatusInternalServerError, "Provider key deleted, but cached sessions could not be invalidated")
		if linkCount(f, f.openai) != 0 {
			t.Error("the key's links are still stored")
		}
	})
}
