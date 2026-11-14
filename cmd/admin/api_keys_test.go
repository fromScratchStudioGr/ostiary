package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
)

func TestCreateGatewayAPIKeyHandler(t *testing.T) {
	create := func(t *testing.T, p *panel, body string) *httptest.ResponseRecorder {
		return p.call(t, createGatewayAPIKeyHandler(p.cfg, p.db), "POST /keys", "/keys", body)
	}
	keyCount := func(p *panel) int64 {
		var n int64
		p.db.Model(&models.APIKey{}).Count(&n)
		return n
	}

	t.Run("issues a key shown once, storing only its hash, which expires after the requested days", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")

		rec := create(t, p, fmt.Sprintf(`{"consumerId":%d,"name":"opencode","expiresInDays":30}`, consumer.ID))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		issued := decode[api.CreateGatewayAPIKeyResponse](t, rec)
		if !auth.IsAPIKey(issued.Key) || issued.Name != "opencode" || issued.DisplayHint != issued.Key[len(issued.Key)-4:] {
			t.Errorf("response = %+v, want a gateway key named opencode with its last four characters as the hint", issued)
		}
		if want := time.Now().AddDate(0, 0, 30); issued.ExpiresAt.Sub(want).Abs() > time.Minute {
			t.Errorf("expires at %v, want about %v", issued.ExpiresAt, want)
		}

		var stored models.APIKey
		if err := p.db.First(&stored, issued.ID).Error; err != nil {
			t.Fatalf("the key was not stored: %v", err)
		}
		if stored.KeyHash != auth.HashAPIKey(issued.Key) {
			t.Error("the stored hash is not the hash of the key that was handed out, so the key would never authenticate")
		}
		if stored.ConsumerID != consumer.ID || stored.Name != "opencode" || stored.Status != "active" || stored.DisplayHint != issued.DisplayHint {
			t.Errorf("stored = %+v, want an active key of the consumer", stored)
		}
		var rawCopies int64
		p.db.Raw("SELECT count(*) FROM api_keys WHERE key_hash = ? OR name = ? OR display_hint = ?", issued.Key, issued.Key, issued.Key).Scan(&rawCopies)
		if rawCopies != 0 {
			t.Error("the raw key is stored in the database")
		}
	})

	t.Run("answers with exactly the fields the client expects, under their JSON names", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		// A key of another consumer first, so the new key's id isn't also its consumer's id.
		p.createAPIKey(t, p.createConsumer(t, "globex"))

		rec := create(t, p, fmt.Sprintf(`{"consumerId":%d,"name":"opencode","expiresInDays":30}`, consumer.ID))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the response is not a JSON object: %v (%q)", err, rec.Body.String())
		}
		if fields, want := fieldsOf(body), []string{"displayHint", "expiresAt", "id", "key", "name"}; !slices.Equal(fields, want) {
			t.Fatalf("fields = %v, want exactly %v", fields, want)
		}

		var stored models.APIKey
		if err := p.db.Where("consumer_id = ?", consumer.ID).First(&stored).Error; err != nil {
			t.Fatalf("the key was not stored: %v", err)
		}
		key, _ := body["key"].(string)
		if id, _ := body["id"].(float64); uint(id) != stored.ID {
			t.Errorf("id = %v, want the stored key's id %d", body["id"], stored.ID)
		}
		if body["name"] != "opencode" {
			t.Errorf("name = %v, want opencode", body["name"])
		}
		if !strings.HasPrefix(key, auth.APIKeyPrefix) || len(key) <= len(auth.APIKeyPrefix)+4 {
			t.Errorf("key = %q, want a full gateway key", key)
		}
		if body["displayHint"] != key[len(key)-4:] {
			t.Errorf("displayHint = %v, want the key's last four characters %q", body["displayHint"], key[len(key)-4:])
		}
		expiresAt, _ := body["expiresAt"].(string)
		parsed, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			t.Fatalf("expiresAt = %q, want an RFC 3339 timestamp: %v", body["expiresAt"], err)
		}
		if parsed.Sub(stored.ExpiresAt).Abs() > time.Millisecond {
			t.Errorf("expiresAt = %v, want the stored expiry %v", parsed, stored.ExpiresAt)
		}
	})

	t.Run("hands out a different key every time", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		body := fmt.Sprintf(`{"consumerId":%d,"name":"k","expiresInDays":1}`, consumer.ID)

		first := decode[api.CreateGatewayAPIKeyResponse](t, create(t, p, body))
		second := decode[api.CreateGatewayAPIKeyResponse](t, create(t, p, body))

		if first.Key == second.Key || first.ID == second.ID {
			t.Error("two requests got the same key")
		}
	})

	t.Run("accepts the longest lifetime and refuses anything outside it", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		body := func(days string) string {
			return fmt.Sprintf(`{"consumerId":%d,"name":"k","expiresInDays":%s}`, consumer.ID, days)
		}

		if rec := create(t, p, body("365")); rec.Code != http.StatusCreated {
			t.Errorf("365 days = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		before := keyCount(p)
		for _, days := range []string{"0", "-1", "366", "10000"} {
			wantValidationError(t, create(t, p, body(days)), "ExpiresInDays")
		}
		if keyCount(p) != before {
			t.Error("a key was issued for a lifetime that is not allowed")
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		gone := p.createConsumer(t, "gone")
		p.db.Delete(&gone)
		id := idOf(consumer.ID)
		tests := []struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{
			{"not JSON", `nope`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Invalid payload")
			}},
			{"no consumer", `{"name":"k","expiresInDays":1}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "ConsumerId")
			}},
			{"no name", `{"consumerId":` + id + `,"expiresInDays":1}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Name")
			}},
			{"no lifetime", `{"consumerId":` + id + `,"name":"k"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "ExpiresInDays")
			}},
			{"a consumer that doesn't exist", `{"consumerId":999999,"name":"k","expiresInDays":1}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Consumer not found")
			}},
			{"a consumer that was deleted", `{"consumerId":` + idOf(gone.ID) + `,"name":"k","expiresInDays":1}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Consumer not found")
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, create(t, p, tt.body))
			})
		}

		if got := keyCount(p); got != 0 {
			t.Errorf("%d keys were issued by rejected requests", got)
		}
	})

	t.Run("reports a failure to store the key, without handing one out", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.rejectWrites(t, "api_keys", "INSERT", "true")

		rec := create(t, p, fmt.Sprintf(`{"consumerId":%d,"name":"k","expiresInDays":1}`, consumer.ID))

		wantError(t, rec, http.StatusInternalServerError, "Could not store key")
		if strings.Contains(rec.Body.String(), auth.APIKeyPrefix) {
			t.Error("a key was handed out although it was not stored")
		}
	})
}

func TestGetConsumerGatewayAPIKeysHandler(t *testing.T) {
	list := func(t *testing.T, p *panel, id string) *httptest.ResponseRecorder {
		return p.call(t, getConsumerGatewayAPIKeysHandler(p.cfg, p.db), "GET /consumers/{id}/keys", "/consumers/"+id+"/keys", "")
	}

	t.Run("lists the consumer's keys, revoked ones included, and never a raw key or hash", func(t *testing.T) {
		p := newPanel(t)
		acme := p.createConsumer(t, "acme")
		globex := p.createConsumer(t, "globex")
		active := p.createAPIKey(t, acme)
		revoked := p.createAPIKey(t, acme)
		other := p.createAPIKey(t, globex)
		used := time.Now().Add(-time.Hour).Truncate(time.Second)
		p.db.Model(&revoked).Updates(map[string]any{"status": "revoked", "last_used_at": used})

		rec := list(t, p, idOf(acme.ID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		got := map[uint]api.GatewayAPIKeyResponse{}
		for _, k := range decode[[]api.GatewayAPIKeyResponse](t, rec) {
			got[k.ID] = k
		}
		if len(got) != 2 {
			t.Fatalf("got %d keys, want acme's two: %+v", len(got), got)
		}
		if g := got[active.ID]; g.Name != active.Name || g.DisplayHint != "abcd" || g.Status != "active" || g.LastUsedAt != nil || g.CreatedAt.IsZero() || g.ExpiresAt.IsZero() {
			t.Errorf("active key = %+v, want its name, hint, status, times and no last use", g)
		}
		if g := got[revoked.ID]; g.Status != "revoked" || g.LastUsedAt == nil || !g.LastUsedAt.Equal(used) {
			t.Errorf("revoked key = %+v, want status revoked and last used at %v", g, used)
		}
		if _, leaked := got[other.ID]; leaked {
			t.Error("another consumer's key is listed")
		}
		for _, key := range []models.APIKey{active, revoked} {
			if strings.Contains(rec.Body.String(), key.KeyHash) {
				t.Errorf("the response contains the hash of key %d", key.ID)
			}
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "hash") {
			t.Errorf("the response mentions hashes: %s", rec.Body.String())
		}
	})

	t.Run("answers with exactly the fields the client expects, under their JSON names", func(t *testing.T) {
		p := newPanel(t)
		acme := p.createConsumer(t, "acme")
		fresh := p.createAPIKey(t, acme)
		used := p.createAPIKey(t, acme)
		usedAt := time.Now().Add(-time.Hour)
		p.db.Model(&used).Updates(map[string]any{"status": "revoked", "last_used_at": usedAt})
		p.db.First(&used, used.ID)
		p.db.First(&fresh, fresh.ID)

		rec := list(t, p, idOf(acme.ID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var keys []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
			t.Fatalf("the response is not a JSON list of objects: %v (%q)", err, rec.Body.String())
		}
		if len(keys) != 2 {
			t.Fatalf("got %d keys, want 2: %s", len(keys), rec.Body.String())
		}
		byID := map[uint]map[string]any{}
		for _, k := range keys {
			wantFields := []string{"createdAt", "displayHint", "expiresAt", "id", "lastUsedAt", "name", "status"}
			if got := fieldsOf(k); !slices.Equal(got, wantFields) {
				t.Fatalf("fields = %v, want exactly %v", got, wantFields)
			}
			id, _ := k["id"].(float64)
			byID[uint(id)] = k
		}

		timeOf := func(k map[string]any, field string) time.Time {
			parsed, err := time.Parse(time.RFC3339, fmt.Sprint(k[field]))
			if err != nil {
				t.Fatalf("%s = %v, want an RFC 3339 timestamp: %v", field, k[field], err)
			}
			return parsed
		}
		for _, stored := range []models.APIKey{fresh, used} {
			k := byID[stored.ID]
			if k == nil {
				t.Fatalf("key %d is not in the response", stored.ID)
			}
			if k["name"] != stored.Name || k["displayHint"] != stored.DisplayHint || k["status"] != stored.Status {
				t.Errorf("key %d = %v, want name %q, hint %q, status %q", stored.ID, k, stored.Name, stored.DisplayHint, stored.Status)
			}
			if timeOf(k, "expiresAt").Sub(stored.ExpiresAt).Abs() > time.Millisecond || timeOf(k, "createdAt").Sub(stored.CreatedAt).Abs() > time.Millisecond {
				t.Errorf("key %d times = expires %v, created %v, want %v and %v", stored.ID, k["expiresAt"], k["createdAt"], stored.ExpiresAt, stored.CreatedAt)
			}
		}
		if value, present := byID[fresh.ID]["lastUsedAt"]; !present || value != nil {
			t.Errorf("a key never used has lastUsedAt = %v (present: %v), want null", value, present)
		}
		if timeOf(byID[used.ID], "lastUsedAt").Sub(usedAt).Abs() > time.Millisecond {
			t.Errorf("lastUsedAt = %v, want %v", byID[used.ID]["lastUsedAt"], usedAt)
		}
	})

	t.Run("answers with an empty list, not null, for a consumer without keys", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")

		rec := list(t, p, idOf(consumer.ID))

		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Errorf("response = %d %q, want 200 []", rec.Code, rec.Body.String())
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		gone := p.createConsumer(t, "gone")
		p.db.Delete(&gone)
		tests := []struct {
			name    string
			id      string
			status  int
			message string
		}{
			{"an id that is not a number", "abc", http.StatusBadRequest, "Invalid consumer id"},
			{"a consumer that doesn't exist", "999999", http.StatusNotFound, "Consumer not found"},
			{"a consumer that was deleted", idOf(gone.ID), http.StatusNotFound, "Consumer not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantError(t, list(t, p, tt.id), tt.status, tt.message)
			})
		}
	})

	t.Run("reports a failure to read them", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.breakTable(t, "api_keys")

		wantError(t, list(t, p, idOf(consumer.ID)), http.StatusInternalServerError, "Could not fetch keys")
	})
}

func TestUpdateGatewayAPIKeyStatusHandler(t *testing.T) {
	update := func(t *testing.T, p *panel, body string) *httptest.ResponseRecorder {
		return p.call(t, updateGatewayAPIKeyStatusHandler(p.cfg, p.db, p.cache), "PATCH /keys/status", "/keys/status", body)
	}
	statusOf := func(p *panel, key models.APIKey) string {
		var row models.APIKey
		p.db.First(&row, key.ID)
		return row.Status
	}

	t.Run("changes that key's status and drops its cached session, and no one else's", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		key := p.createAPIKey(t, consumer)
		sibling := p.createAPIKey(t, consumer)
		session := p.cacheSession(t, key.KeyHash)
		siblingSession := p.cacheSession(t, sibling.KeyHash)

		rec := update(t, p, fmt.Sprintf(`{"id":%d,"status":"revoked"}`, key.ID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got := statusOf(p, key); got != "revoked" {
			t.Errorf("status = %q, want revoked", got)
		}
		if p.isCached(t, session) {
			t.Error("the key's cached session is still in Redis, so the revoked key keeps working")
		}
		if !p.isCached(t, siblingSession) || statusOf(p, sibling) != "active" {
			t.Error("the consumer's other key was affected")
		}

		rec = update(t, p, fmt.Sprintf(`{"id":%d,"status":"active"}`, key.ID))

		if rec.Code != http.StatusOK || statusOf(p, key) != "active" {
			t.Errorf("reactivating = %d, status %q, want 200 and active", rec.Code, statusOf(p, key))
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		key := p.createAPIKey(t, p.createConsumer(t, "acme"))
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
				wantValidationError(t, rec, "ID")
			}},
			{"no status", `{"id":` + id + `}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
			{"a status that doesn't exist", `{"id":` + id + `,"status":"expired"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
			{"a key that doesn't exist", `{"id":999999,"status":"revoked"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Key not found")
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
		key := p.createAPIKey(t, p.createConsumer(t, "acme"))
		p.rejectWrites(t, "api_keys", "UPDATE", "true")

		rec := update(t, p, fmt.Sprintf(`{"id":%d,"status":"revoked"}`, key.ID))

		wantError(t, rec, http.StatusInternalServerError, "Could not update status")
	})

	t.Run("says so when the cached session could not be dropped, after the status changed", func(t *testing.T) {
		p := newPanel(t)
		key := p.createAPIKey(t, p.createConsumer(t, "acme"))

		rec := p.call(t, updateGatewayAPIKeyStatusHandler(p.cfg, p.db, p.deadCache(t)), "PATCH /keys/status", "/keys/status",
			fmt.Sprintf(`{"id":%d,"status":"revoked"}`, key.ID))

		wantError(t, rec, http.StatusInternalServerError, "Status updated, but cached sessions could not be invalidated")
		if got := statusOf(p, key); got != "revoked" {
			t.Errorf("status = %q, want revoked", got)
		}
	})
}
