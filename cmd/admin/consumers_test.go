package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
)

func TestCreateConsumerHandler(t *testing.T) {
	p := newPanel(t)
	handler := createConsumerHandler(p.cfg, p.db)
	create := func(body string) *httptest.ResponseRecorder {
		return p.call(t, handler, "POST /consumers", "/consumers", body)
	}
	consumerCount := func() int64 {
		var n int64
		p.db.Model(&models.Consumer{}).Count(&n)
		return n
	}

	t.Run("creates an active consumer and returns it", func(t *testing.T) {
		rec := create(`{"name":"acme"}`)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		got := decode[api.ConsumerResponse](t, rec)
		if got.ID == 0 || got.Name != "acme" || got.Status != "active" || got.CreatedAt.IsZero() {
			t.Errorf("response = %+v, want an active consumer named acme with an id and creation time", got)
		}
		var stored models.Consumer
		if err := p.db.First(&stored, got.ID).Error; err != nil || stored.Name != "acme" || stored.Status != "active" {
			t.Errorf("stored = %+v (%v), want the consumer in the response", stored, err)
		}
	})

	t.Run("answers with exactly the fields the client expects, with the stored values", func(t *testing.T) {
		rec := create(`{"name":"wire"}`)

		object := jsonObject(t, rec)
		wantFields(t, object, "createdAt", "id", "name", "status")
		var stored models.Consumer
		if err := p.db.Where("name = ?", "wire").First(&stored).Error; err != nil {
			t.Fatalf("the consumer was not stored: %v", err)
		}
		if object["id"] != float64(stored.ID) || object["name"] != "wire" || object["status"] != "active" {
			t.Errorf("response = %v, want id %d, name wire, status active", object, stored.ID)
		}
		if !sameInstant(timeField(t, object, "createdAt"), stored.CreatedAt) {
			t.Errorf("createdAt = %v, want %v", object["createdAt"], stored.CreatedAt)
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		before := consumerCount()

		wantError(t, create(`nope`), http.StatusBadRequest, "Invalid payload")
		wantValidationError(t, create(`{}`), "Name")
		wantValidationError(t, create(`{"name":""}`), "Name")

		if got := consumerCount(); got != before {
			t.Errorf("%d consumers were created by rejected requests", got-before)
		}
	})

	t.Run("reports a failure to store the consumer", func(t *testing.T) {
		rec := create(`{"name":"` + strings.Repeat("n", 65) + `"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not create consumer")
	})
}

func TestGetConsumersHandler(t *testing.T) {
	handler := func(p *panel) http.HandlerFunc { return getConsumersHandler(p.cfg, p.db) }
	list := func(t *testing.T, p *panel) *httptest.ResponseRecorder {
		return p.call(t, handler(p), "GET /consumers", "/consumers", "")
	}

	t.Run("lists the consumers that exist, revoked ones included", func(t *testing.T) {
		p := newPanel(t)
		p.createConsumer(t, "acme")
		revoked := p.createConsumer(t, "globex")
		gone := p.createConsumer(t, "initech")
		p.db.Model(&revoked).Update("status", "revoked")
		p.db.Delete(&gone)

		rec := list(t, p)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		got := map[string]string{}
		for _, c := range decode[[]api.ConsumerResponse](t, rec) {
			got[c.Name] = c.Status
			if c.ID == 0 || c.CreatedAt.IsZero() {
				t.Errorf("%s comes without an id or creation time: %+v", c.Name, c)
			}
		}
		if len(got) != 2 || got["acme"] != "active" || got["globex"] != "revoked" {
			t.Errorf("consumers = %v, want acme active and globex revoked (initech was deleted)", got)
		}
	})

	t.Run("answers with exactly the fields the client expects, with the stored values", func(t *testing.T) {
		p := newPanel(t)
		p.createConsumer(t, "acme")
		revoked := p.createConsumer(t, "globex")
		p.db.Model(&revoked).Update("status", "revoked")
		var stored []models.Consumer
		p.db.Find(&stored)

		rec := list(t, p)

		objects := jsonList(t, rec)
		if len(objects) != len(stored) {
			t.Fatalf("got %d consumers, want %d: %s", len(objects), len(stored), rec.Body.String())
		}
		byID := map[float64]map[string]any{}
		for _, object := range objects {
			wantFields(t, object, "createdAt", "id", "name", "status")
			byID[object["id"].(float64)] = object
		}
		for _, consumer := range stored {
			object := byID[float64(consumer.ID)]
			if object == nil {
				t.Fatalf("consumer %d is not in the response", consumer.ID)
			}
			if object["name"] != consumer.Name || object["status"] != consumer.Status || !sameInstant(timeField(t, object, "createdAt"), consumer.CreatedAt) {
				t.Errorf("consumer %d = %v, want name %q, status %q, created at %v", consumer.ID, object, consumer.Name, consumer.Status, consumer.CreatedAt)
			}
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
		p.breakTable(t, "consumers")

		wantError(t, list(t, p), http.StatusInternalServerError, "Could not fetch consumers")
	})
}

func TestDeleteConsumerHandler(t *testing.T) {
	remove := func(t *testing.T, p *panel, id string) *httptest.ResponseRecorder {
		return p.call(t, deleteConsumerHandler(p.cfg, p.db, p.cache), "DELETE /consumers/{id}", "/consumers/"+id, "")
	}
	apiKeyCount := func(p *panel, consumer models.Consumer) int64 {
		var n int64
		p.db.Model(&models.APIKey{}).Where("consumer_id = ?", consumer.ID).Count(&n)
		return n
	}

	t.Run("soft-deletes the consumer, drops its API keys and their cached sessions, and leaves everyone else alone", func(t *testing.T) {
		p := newPanel(t)
		doomed := p.createConsumer(t, "doomed")
		bystander := p.createConsumer(t, "bystander")
		doomedKey := p.createAPIKey(t, doomed)
		bystanderKey := p.createAPIKey(t, bystander)
		doomedSession := p.cacheSession(t, doomedKey.KeyHash)
		bystanderSession := p.cacheSession(t, bystanderKey.KeyHash)
		providerKey := p.createProviderKey(t, "openai", "wxyz")
		if err := p.db.Create(&models.Usage{ConsumerID: doomed.ID, ProviderKeyID: providerKey.ID}).Error; err != nil {
			t.Fatal(err)
		}

		rec := remove(t, p, idOf(doomed.ID))

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
		}
		var row models.Consumer
		if err := p.db.Unscoped().First(&row, doomed.ID).Error; err != nil || !row.DeletedAt.Valid {
			t.Errorf("the consumer row = %+v (%v), want it kept with deleted_at set", row, err)
		}
		if apiKeyCount(p, doomed) != 0 {
			t.Error("the consumer's API keys are still stored")
		}
		if p.isCached(t, doomedSession) {
			t.Error("the deleted consumer's cached session is still in Redis")
		}
		if apiKeyCount(p, bystander) != 1 || !p.isCached(t, bystanderSession) {
			t.Error("another consumer's key or cached session was touched")
		}
		var usages int64
		p.db.Model(&models.Usage{}).Where("consumer_id = ?", doomed.ID).Count(&usages)
		if usages != 1 {
			t.Errorf("%d usage rows remain for the deleted consumer, want its history kept", usages)
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
			{"a negative id", "-1", http.StatusBadRequest, "Invalid consumer id"},
			{"a consumer that doesn't exist", "999999", http.StatusNotFound, "Consumer not found"},
			{"a consumer that was already deleted", idOf(gone.ID), http.StatusNotFound, "Consumer not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantError(t, remove(t, p, tt.id), tt.status, tt.message)
			})
		}
	})

	t.Run("deletes nothing when it can't delete everything", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		key := p.createAPIKey(t, consumer)
		session := p.cacheSession(t, key.KeyHash)
		p.rejectWrites(t, "consumers", "UPDATE", "ROW.id = "+idOf(consumer.ID))

		rec := remove(t, p, idOf(consumer.ID))

		wantError(t, rec, http.StatusInternalServerError, "Could not delete consumer")
		if apiKeyCount(p, consumer) != 1 {
			t.Error("the API keys were deleted although the consumer wasn't")
		}
		if !p.isCached(t, session) {
			t.Error("the cached session was dropped although nothing was deleted")
		}
	})

	t.Run("deletes nothing when the API keys can't be deleted", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.createAPIKey(t, consumer)
		p.rejectWrites(t, "api_keys", "DELETE", "true")

		rec := remove(t, p, idOf(consumer.ID))

		wantError(t, rec, http.StatusInternalServerError, "Could not delete consumer")
		var n int64
		p.db.Model(&models.Consumer{}).Where("id = ?", consumer.ID).Count(&n)
		if n != 1 || apiKeyCount(p, consumer) != 1 {
			t.Error("the consumer or its keys were deleted although a delete failed")
		}
	})

	t.Run("reports a failure to find the API keys, deleting nothing", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.breakTable(t, "api_keys")

		rec := remove(t, p, idOf(consumer.ID))

		wantError(t, rec, http.StatusInternalServerError, "Could not delete consumer")
		var n int64
		p.db.Model(&models.Consumer{}).Where("id = ?", consumer.ID).Count(&n)
		if n != 1 {
			t.Error("the consumer was deleted although its keys could not be listed")
		}
	})

	t.Run("says so when the cached sessions could not be dropped, after the delete went through", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.createAPIKey(t, consumer)

		rec := p.call(t, deleteConsumerHandler(p.cfg, p.db, p.deadCache(t)), "DELETE /consumers/{id}", "/consumers/"+idOf(consumer.ID), "")

		wantError(t, rec, http.StatusInternalServerError, "Consumer deleted, but cached sessions could not be invalidated")
		var n int64
		p.db.Model(&models.Consumer{}).Where("id = ?", consumer.ID).Count(&n)
		if n != 0 {
			t.Error("the consumer is still there")
		}
	})
}

func TestGetConsumerProviderKeysHandler(t *testing.T) {
	list := func(t *testing.T, p *panel, id string) *httptest.ResponseRecorder {
		return p.call(t, getConsumerProviderKeysHandler(p.cfg, p.db), "GET /consumers/{id}/provider-keys", "/consumers/"+id+"/provider-keys", "")
	}

	t.Run("lists the provider keys the consumer can use, and only those", func(t *testing.T) {
		p := newPanel(t)
		acme := p.createConsumer(t, "acme")
		globex := p.createConsumer(t, "globex")
		openai := p.createProviderKey(t, "openai", "1111")
		anthropic := p.createProviderKey(t, "anthropic", "2222")
		other := p.createProviderKey(t, "gemini", "3333")
		p.link(t, acme, openai, 50)
		p.link(t, acme, anthropic, 0)
		p.link(t, globex, other, 10)
		p.db.Model(&models.UserProviderAccess{}).Where("consumer_id = ? AND provider_key_id = ?", acme.ID, anthropic.ID).Updates(map[string]any{"status": "revoked", "total_cost": 1.5})

		rec := list(t, p, idOf(acme.ID))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		got := map[uint]api.ConsumerProviderKeyResponse{}
		for _, k := range decode[[]api.ConsumerProviderKeyResponse](t, rec) {
			got[k.ID] = k
		}
		want := map[uint]api.ConsumerProviderKeyResponse{
			openai.ID:    {ID: openai.ID, Provider: "openai", Key: "1111", Status: "active", TotalCost: 0, UsageLimit: 50},
			anthropic.ID: {ID: anthropic.ID, Provider: "anthropic", Key: "2222", Status: "revoked", TotalCost: 1.5, UsageLimit: 0},
		}
		if len(got) != len(want) || got[openai.ID] != want[openai.ID] || got[anthropic.ID] != want[anthropic.ID] {
			t.Errorf("provider keys = %+v, want %+v", got, want)
		}
	})

	t.Run("answers with exactly the fields the client expects, with the stored values", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		key := p.createProviderKey(t, "openai", "1111")
		p.link(t, consumer, key, 50)
		p.db.Model(&models.UserProviderAccess{}).Where("consumer_id = ?", consumer.ID).Updates(map[string]any{"status": "revoked", "total_cost": 1.5})

		objects := jsonList(t, list(t, p, idOf(consumer.ID)))

		if len(objects) != 1 {
			t.Fatalf("got %d provider keys, want 1: %v", len(objects), objects)
		}
		wantFields(t, objects[0], "id", "key", "provider", "status", "totalCost", "usageLimit")
		want := map[string]any{"id": float64(key.ID), "key": "1111", "provider": "openai", "status": "revoked", "totalCost": 1.5, "usageLimit": float64(50)}
		for field, value := range want {
			if objects[0][field] != value {
				t.Errorf("%s = %v, want %v", field, objects[0][field], value)
			}
		}
	})

	t.Run("answers with an empty list, not null, for a consumer without access", func(t *testing.T) {
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
		p.breakTable(t, "user_provider_accesses")

		wantError(t, list(t, p, idOf(consumer.ID)), http.StatusInternalServerError, "Could not fetch provider keys")
	})
}

func TestUpdateConsumerStatusHandler(t *testing.T) {
	update := func(t *testing.T, p *panel, body string) *httptest.ResponseRecorder {
		return p.call(t, updateConsumerStatusHandler(p.cfg, p.db, p.cache), "PATCH /consumers/status", "/consumers/status", body)
	}
	statusOf := func(p *panel, consumer models.Consumer) string {
		var row models.Consumer
		p.db.First(&row, consumer.ID)
		return row.Status
	}

	t.Run("changes the status and drops the cached sessions of that consumer's keys only", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		bystander := p.createConsumer(t, "globex")
		session := p.cacheSession(t, p.createAPIKey(t, consumer).KeyHash)
		bystanderSession := p.cacheSession(t, p.createAPIKey(t, bystander).KeyHash)

		rec := update(t, p, `{"id":`+idOf(consumer.ID)+`,"status":"revoked"}`)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got := statusOf(p, consumer); got != "revoked" {
			t.Errorf("status = %q, want revoked", got)
		}
		if p.isCached(t, session) {
			t.Error("the consumer's cached session is still in Redis, so a revoked consumer keeps working")
		}
		if !p.isCached(t, bystanderSession) || statusOf(p, bystander) != "active" {
			t.Error("another consumer was affected")
		}

		rec = update(t, p, `{"id":`+idOf(consumer.ID)+`,"status":"active"}`)

		if rec.Code != http.StatusOK || statusOf(p, consumer) != "active" {
			t.Errorf("reactivating = %d, status %q, want 200 and active", rec.Code, statusOf(p, consumer))
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
			{"no consumer", `{"status":"revoked"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "ConsumerId")
			}},
			{"no status", `{"id":` + id + `}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
			{"a status that doesn't exist", `{"id":` + id + `,"status":"banned"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Status")
			}},
			{"a consumer that doesn't exist", `{"id":999999,"status":"revoked"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Consumer not found")
			}},
			{"a consumer that was deleted", `{"id":` + idOf(gone.ID) + `,"status":"revoked"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "Consumer not found")
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, update(t, p, tt.body))
			})
		}

		if got := statusOf(p, consumer); got != "active" {
			t.Errorf("a rejected request left the status at %q", got)
		}
	})

	t.Run("reports a failure to store the status", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.rejectWrites(t, "consumers", "UPDATE", "ROW.id = "+idOf(consumer.ID))

		rec := update(t, p, `{"id":`+idOf(consumer.ID)+`,"status":"revoked"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not update status")
	})

	t.Run("says so when the cached sessions could not be dropped, after the status changed", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		p.createAPIKey(t, consumer)

		rec := p.call(t, updateConsumerStatusHandler(p.cfg, p.db, p.deadCache(t)), "PATCH /consumers/status", "/consumers/status",
			`{"id":`+idOf(consumer.ID)+`,"status":"revoked"}`)

		wantError(t, rec, http.StatusInternalServerError, "Status updated, but cached sessions could not be invalidated")
		if got := statusOf(p, consumer); got != "revoked" {
			t.Errorf("status = %q, want revoked", got)
		}
	})
}
