package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"gorm.io/gorm"
)

// createConsumerHandler creates a consumer: the entity assigned provider-key
// access and gateway API keys. Admin-only.
func createConsumerHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateConsumerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("create-consumer: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("create-consumer: validation failed", "name", req.Name, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		consumer := models.Consumer{Name: req.Name}
		if err := db.WithContext(r.Context()).Create(&consumer).Error; err != nil {
			slog.Error("create-consumer: could not create consumer", "name", req.Name, "error", err)
			api.WriteError(w, "Could not create consumer", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.ConsumerResponse{
			ID:        consumer.ID,
			Name:      consumer.Name,
			Status:    consumer.Status,
			CreatedAt: consumer.CreatedAt,
		})
	})
}

// getConsumersHandler lists all consumers. Admin-only.
func getConsumersHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var consumers []models.Consumer
		if err := db.WithContext(r.Context()).Find(&consumers).Error; err != nil {
			slog.Error("get-consumers: could not fetch consumers", "error", err)
			api.WriteError(w, "Could not fetch consumers", http.StatusInternalServerError)
			return
		}

		response := make([]api.ConsumerResponse, len(consumers))
		for i, c := range consumers {
			response[i] = api.ConsumerResponse{
				ID:        c.ID,
				Name:      c.Name,
				Status:    c.Status,
				CreatedAt: c.CreatedAt,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}

// deleteConsumerHandler soft-deletes a consumer (Consumer embeds gorm.Model,
// so Delete just sets deleted_at — the row stays put, keeping Usage.ConsumerID
// a valid FK) and hard-deletes its gateway API keys, since keys for a
// removed consumer serve no purpose. Admin-only.
func deleteConsumerHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		consumerID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			slog.Warn("delete-consumer: invalid consumer id", "id", r.PathValue("id"), "error", err)
			api.WriteError(w, "Invalid consumer id", http.StatusBadRequest)
			return
		}

		var consumer models.Consumer
		if err := db.WithContext(r.Context()).First(&consumer, consumerID).Error; err != nil {
			slog.Warn("delete-consumer: consumer not found", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Consumer not found", http.StatusNotFound)
			return
		}

		keyHashes, err := consumerKeyHashes(r.Context(), db, uint(consumerID))
		if err != nil {
			slog.Error("delete-consumer: could not fetch consumer api keys", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Could not delete consumer", http.StatusInternalServerError)
			return
		}

		err = db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("consumer_id = ?", consumerID).Delete(&models.APIKey{}).Error; err != nil {
				return err
			}
			return tx.Delete(&consumer).Error
		})
		if err != nil {
			slog.Error("delete-consumer: could not delete consumer", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Could not delete consumer", http.StatusInternalServerError)
			return
		}

		if err := redis.InvalidateAPIKeys(r.Context(), keyHashes); err != nil {
			slog.Error("delete-consumer: could not invalidate cached sessions", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Consumer deleted, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}

func getConsumerProviderKeysHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		consumerID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			slog.Warn("get-consumer-provider-keys: invalid consumer id", "id", r.PathValue("id"), "error", err)
			api.WriteError(w, "Invalid consumer id", http.StatusBadRequest)
			return
		}

		var consumer models.Consumer
		if err := db.WithContext(r.Context()).First(&consumer, consumerID).Error; err != nil {
			slog.Warn("get-consumer-provider-keys: consumer not found", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Consumer not found", http.StatusNotFound)
			return
		}

		var response []api.ConsumerProviderKeyResponse
		err = db.WithContext(r.Context()).
			Model(&models.UserProviderAccess{}).
			Select("provider_keys.id, provider_keys.provider, provider_keys.display_hint AS key, user_provider_accesses.status, user_provider_accesses.total_cost, user_provider_accesses.usage_limit").
			Joins("JOIN provider_keys ON provider_keys.id = user_provider_accesses.provider_key_id").
			Where("user_provider_accesses.consumer_id = ?", consumerID).
			Scan(&response).Error
		if err != nil {
			slog.Error("get-consumer-provider-keys: could not fetch provider keys", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Could not fetch provider keys", http.StatusInternalServerError)
			return
		}

		if response == nil {
			response = []api.ConsumerProviderKeyResponse{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}

func updateConsumerStatusHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateConsumerStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("update-consumer-status: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("update-consumer-status: validation failed", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var consumer models.Consumer
		if err := db.WithContext(r.Context()).First(&consumer, req.ConsumerId).Error; err != nil {
			slog.Warn("update-consumer-status: consumer not found", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Consumer not found", http.StatusNotFound)
			return
		}

		if err := db.WithContext(r.Context()).Model(&consumer).Update("status", req.Status).Error; err != nil {
			slog.Error("update-consumer-status: could not update status", "consumer_id", req.ConsumerId, "status", req.Status, "error", err)
			api.WriteError(w, "Could not update status", http.StatusInternalServerError)
			return
		}

		if err := invalidateConsumerSessions(r.Context(), db, redis, consumer.ID); err != nil {
			slog.Error("update-consumer-status: could not invalidate cached sessions", "consumer_id", consumer.ID, "error", err)
			api.WriteError(w, "Status updated, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}
