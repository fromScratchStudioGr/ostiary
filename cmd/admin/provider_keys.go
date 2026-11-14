package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"

	"gorm.io/gorm"
)

func registerKeyHandler(cfg config.Config, db *gorm.DB, kms keystore.KMSClient) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateProviderKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("register-key: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			// Never log req.ApiKey anywhere in this handler — it's the
			// plaintext provider secret being registered.
			slog.Warn("register-key: validation failed", "provider", req.Provider, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		ciphertext, nonce, encryptedDEK, kmsKeyID, err := keystore.SealProviderKey(r.Context(), kms, []byte(req.ApiKey))
		if err != nil {
			slog.Error("register-key: could not seal provider key", "provider", req.Provider, "error", err)
			api.WriteError(w, "Could not seal provider key", http.StatusInternalServerError)
			return
		}

		displayHint := req.ApiKey
		if len(displayHint) > 4 {
			displayHint = displayHint[len(displayHint)-4:]
		}

		providerKey := models.ProviderKey{
			Provider:     req.Provider,
			Ciphertext:   ciphertext,
			Nonce:        nonce,
			EncryptedDEK: encryptedDEK,
			KMSKeyID:     kmsKeyID,
			DisplayHint:  displayHint,
		}

		if err := db.WithContext(r.Context()).Create(&providerKey).Error; err != nil {
			slog.Error("register-key: could not store provider key", "provider", req.Provider, "error", err)
			api.WriteError(w, "Could not store provider key", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "Provider key stored for %s", providerKey.Provider)
	})
}

func assignKeyHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.AssignProviderKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("assign-key: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("assign-key: validation failed", "consumer_id", req.ConsumerId, "provider_key_id", req.ProviderKeyId, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var providerKey models.ProviderKey
		if err := db.WithContext(r.Context()).First(&providerKey, req.ProviderKeyId).Error; err != nil {
			slog.Warn("assign-key: provider key not found", "provider_key_id", req.ProviderKeyId, "error", err)
			api.WriteError(w, "Provider key not found", http.StatusNotFound)
			return
		}

		var consumer models.Consumer
		if err := db.WithContext(r.Context()).First(&consumer, req.ConsumerId).Error; err != nil {
			slog.Warn("assign-key: consumer not found", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Consumer not found", http.StatusNotFound)
			return
		}

		access := models.UserProviderAccess{
			ConsumerID:    req.ConsumerId,
			ProviderKeyID: req.ProviderKeyId,
			TotalCost:     0,
			UsageLimit:    req.UsageLimit,
		}

		if err := db.WithContext(r.Context()).Create(&access).Error; err != nil {
			if database.IsUniqueViolation(err) {
				slog.Warn("assign-key: access already exists", "consumer_id", req.ConsumerId, "provider_key_id", req.ProviderKeyId, "error", err)
				api.WriteError(w, "Consumer already has access to this provider key", http.StatusConflict)
				return
			}
			slog.Error("assign-key: could not store access", "consumer_id", req.ConsumerId, "provider_key_id", req.ProviderKeyId, "error", err)
			api.WriteError(w, "Could not store access", http.StatusInternalServerError)
			return
		}

		if err := invalidateConsumerSessions(r.Context(), db, redis, req.ConsumerId); err != nil {
			slog.Error("assign-key: could not invalidate cached sessions", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Access granted, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "Access to %s granted for consumer %d", providerKey.Provider, req.ConsumerId)
	})
}

func getProviderKeysHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			ID          uint
			Provider    string
			DisplayHint string
			Status      string
			LastUsedAt  *time.Time
			CreatedAt   time.Time
			UserCount   int64
		}

		var rows []row
		err := db.WithContext(r.Context()).
			Model(&models.ProviderKey{}).
			Select("provider_keys.id, provider_keys.provider, provider_keys.display_hint, provider_keys.status, provider_keys.last_used_at, provider_keys.created_at, COUNT(consumers.id) AS user_count").
			Joins("LEFT JOIN user_provider_accesses ON user_provider_accesses.provider_key_id = provider_keys.id").
			Joins("LEFT JOIN consumers ON consumers.id = user_provider_accesses.consumer_id AND consumers.deleted_at IS NULL").
			Group("provider_keys.id").
			Scan(&rows).Error
		if err != nil {
			slog.Error("get-provider-keys: could not fetch provider keys", "error", err)
			api.WriteError(w, "Could not fetch provider keys", http.StatusInternalServerError)
			return
		}

		response := make([]api.ProviderKeyResponse, len(rows))
		for i, pk := range rows {
			response[i] = api.ProviderKeyResponse{
				ID:           pk.ID,
				Provider:     pk.Provider,
				Key:          pk.DisplayHint,
				Status:       pk.Status,
				UserCount:    pk.UserCount,
				LastUsedAt:   pk.LastUsedAt,
				RegisteredAt: pk.CreatedAt,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}

func getProviderKeyConsumersHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		providerKeyID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			slog.Warn("get-provider-key-consumers: invalid provider key id", "id", r.PathValue("id"), "error", err)
			api.WriteError(w, "Invalid provider key id", http.StatusBadRequest)
			return
		}

		var providerKey models.ProviderKey
		if err := db.WithContext(r.Context()).First(&providerKey, providerKeyID).Error; err != nil {
			slog.Warn("get-provider-key-consumers: provider key not found", "provider_key_id", providerKeyID, "error", err)
			api.WriteError(w, "Provider key not found", http.StatusNotFound)
			return
		}

		var response []api.ProviderKeyConsumerResponse
		err = db.WithContext(r.Context()).
			Model(&models.UserProviderAccess{}).
			Select("consumers.id, consumers.name, user_provider_accesses.status, user_provider_accesses.total_cost, user_provider_accesses.usage_limit").
			Joins("JOIN consumers ON consumers.id = user_provider_accesses.consumer_id AND consumers.deleted_at IS NULL").
			Where("user_provider_accesses.provider_key_id = ?", providerKeyID).
			Scan(&response).Error
		if err != nil {
			slog.Error("get-provider-key-consumers: could not fetch consumers", "provider_key_id", providerKeyID, "error", err)
			api.WriteError(w, "Could not fetch consumers", http.StatusInternalServerError)
			return
		}

		if response == nil {
			response = []api.ProviderKeyConsumerResponse{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}

func updateProviderKeyStatusHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateProviderKeyStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("update-provider-key-status: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("update-provider-key-status: validation failed", "provider_key_id", req.ProviderKeyId, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var providerKey models.ProviderKey
		if err := db.WithContext(r.Context()).First(&providerKey, req.ProviderKeyId).Error; err != nil {
			slog.Warn("update-provider-key-status: provider key not found", "provider_key_id", req.ProviderKeyId, "error", err)
			api.WriteError(w, "Provider key not found", http.StatusNotFound)
			return
		}

		if err := db.WithContext(r.Context()).Model(&providerKey).Update("status", req.Status).Error; err != nil {
			slog.Error("update-provider-key-status: could not update status", "provider_key_id", req.ProviderKeyId, "status", req.Status, "error", err)
			api.WriteError(w, "Could not update status", http.StatusInternalServerError)
			return
		}

		if err := invalidateProviderKeySessions(r.Context(), db, redis, req.ProviderKeyId); err != nil {
			slog.Error("update-provider-key-status: could not invalidate cached sessions", "provider_key_id", req.ProviderKeyId, "error", err)
			api.WriteError(w, "Status updated, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}

func updateProviderKeyConsumerStatusHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateProviderKeyConsumerStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("update-provider-key-consumer-status: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("update-provider-key-consumer-status: validation failed", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var userProviderAccess models.UserProviderAccess
		if err := db.WithContext(r.Context()).Where("provider_key_id = ? AND consumer_id = ?", req.ProviderKeyId, req.ConsumerId).First(&userProviderAccess).Error; err != nil {
			slog.Warn("update-provider-key-consumer-status: access not found", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Consumer provider access not found", http.StatusNotFound)
			return
		}

		if err := db.WithContext(r.Context()).Model(&userProviderAccess).Update("status", req.Status).Error; err != nil {
			slog.Error("update-provider-key-consumer-status: could not update status", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "status", req.Status, "error", err)
			api.WriteError(w, "Could not update consumer provider status", http.StatusInternalServerError)
			return
		}

		if err := invalidateConsumerSessions(r.Context(), db, redis, req.ConsumerId); err != nil {
			slog.Error("update-provider-key-consumer-status: could not invalidate cached sessions", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Status updated, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}

func deleteProviderKeyConsumerHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.DeleteProviderKeyConsumerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("delete-provider-key-consumer: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("delete-provider-key-consumer: validation failed", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var userProviderAccess models.UserProviderAccess
		if err := db.WithContext(r.Context()).Where("provider_key_id = ? AND consumer_id = ?", req.ProviderKeyId, req.ConsumerId).First(&userProviderAccess).Error; err != nil {
			slog.Warn("delete-provider-key-consumer: access not found", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Consumer provider access not found", http.StatusNotFound)
			return
		}

		if err := db.WithContext(r.Context()).Delete(&userProviderAccess).Error; err != nil {
			slog.Error("delete-provider-key-consumer: could not delete access", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Could not delete consumer provider key link", http.StatusInternalServerError)
			return
		}

		if err := invalidateConsumerSessions(r.Context(), db, redis, req.ConsumerId); err != nil {
			slog.Error("delete-provider-key-consumer: could not invalidate cached sessions", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Link deleted, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}

func updateProviderKeyConsumerLimitHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateProviderKeyConsumerLimitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("update-provider-key-consumer-limit: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("update-provider-key-consumer-limit: validation failed", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "usage_limit", req.UsageLimit, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var userProviderAccess models.UserProviderAccess
		if err := db.WithContext(r.Context()).Where("provider_key_id = ? AND consumer_id = ?", req.ProviderKeyId, req.ConsumerId).First(&userProviderAccess).Error; err != nil {
			slog.Warn("update-provider-key-consumer-limit: access not found", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Consumer provider access not found", http.StatusNotFound)
			return
		}

		if err := db.WithContext(r.Context()).Model(&userProviderAccess).Update("usage_limit", req.UsageLimit).Error; err != nil {
			slog.Error("update-provider-key-consumer-limit: could not update usage limit", "provider_key_id", req.ProviderKeyId, "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Could not update consumer provider usage limit", http.StatusInternalServerError)
			return
		}

		if err := invalidateConsumerSessions(r.Context(), db, redis, req.ConsumerId); err != nil {
			slog.Error("update-provider-key-consumer-limit: could not invalidate cached sessions", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Usage limit updated, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}

func deleteProviderKeyHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		providerKeyID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			slog.Warn("delete-provider-key: invalid provider key id", "id", r.PathValue("id"), "error", err)
			api.WriteError(w, "Invalid provider key id", http.StatusBadRequest)
			return
		}

		var providerKey models.ProviderKey
		if err := db.WithContext(r.Context()).First(&providerKey, providerKeyID).Error; err != nil {
			slog.Warn("delete-provider-key: provider key not found", "provider_key_id", providerKeyID, "error", err)
			api.WriteError(w, "Provider key not found", http.StatusNotFound)
			return
		}

		keyHashes, err := providerKeyKeyHashes(r.Context(), db, uint(providerKeyID))
		if err != nil {
			slog.Error("delete-provider-key: could not fetch affected api keys", "provider_key_id", providerKeyID, "error", err)
			api.WriteError(w, "Could not delete provider key", http.StatusInternalServerError)
			return
		}

		err = db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("provider_key_id = ?", providerKeyID).Delete(&models.UserProviderAccess{}).Error; err != nil {
				return err
			}
			return tx.Delete(&providerKey).Error
		})
		if err != nil {
			slog.Error("delete-provider-key: could not delete provider key", "provider_key_id", providerKeyID, "error", err)
			api.WriteError(w, "Could not delete provider key", http.StatusInternalServerError)
			return
		}

		if err := redis.InvalidateAPIKeys(r.Context(), keyHashes); err != nil {
			slog.Error("delete-provider-key: could not invalidate cached sessions", "provider_key_id", providerKeyID, "error", err)
			api.WriteError(w, "Provider key deleted, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}
