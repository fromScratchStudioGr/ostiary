package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"gorm.io/gorm"
)

// createGatewayAPIKeyHandler issues a new long-lived gateway API key on
// behalf of a consumer (e.g. for a machine integration like OpenCode) — a
// separate credential from the admin panel's own JWTs, with its own expiry
// and independent revocation. Admin-only, mirroring how provider keys are
// provisioned rather than self-served.
func createGatewayAPIKeyHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateGatewayAPIKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("create-gateway-api-key: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("create-gateway-api-key: validation failed", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var consumer models.Consumer
		if err := db.WithContext(r.Context()).First(&consumer, req.ConsumerId).Error; err != nil {
			slog.Warn("create-gateway-api-key: consumer not found", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Consumer not found", http.StatusNotFound)
			return
		}

		raw, hash, displayHint := auth.GenerateAPIKey()

		apiKey := models.APIKey{
			ConsumerID:  req.ConsumerId,
			Name:        req.Name,
			KeyHash:     hash,
			DisplayHint: displayHint,
			ExpiresAt:   time.Now().AddDate(0, 0, req.ExpiresInDays),
		}

		if err := db.WithContext(r.Context()).Create(&apiKey).Error; err != nil {
			slog.Error("create-gateway-api-key: could not store key", "consumer_id", req.ConsumerId, "error", err)
			api.WriteError(w, "Could not store key", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.CreateGatewayAPIKeyResponse{
			ID:          apiKey.ID,
			Name:        apiKey.Name,
			Key:         raw,
			DisplayHint: apiKey.DisplayHint,
			ExpiresAt:   apiKey.ExpiresAt,
		})
	})
}

// getConsumerGatewayAPIKeysHandler lists a given consumer's gateway API
// keys. Admin-only. Never returns the raw key or its hash.
func getConsumerGatewayAPIKeysHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		consumerID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			slog.Warn("get-consumer-gateway-api-keys: invalid consumer id", "id", r.PathValue("id"), "error", err)
			api.WriteError(w, "Invalid consumer id", http.StatusBadRequest)
			return
		}

		var consumer models.Consumer
		if err := db.WithContext(r.Context()).First(&consumer, consumerID).Error; err != nil {
			slog.Warn("get-consumer-gateway-api-keys: consumer not found", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Consumer not found", http.StatusNotFound)
			return
		}

		var keys []models.APIKey
		if err := db.WithContext(r.Context()).Where("consumer_id = ?", consumerID).Find(&keys).Error; err != nil {
			slog.Error("get-consumer-gateway-api-keys: could not fetch keys", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Could not fetch keys", http.StatusInternalServerError)
			return
		}

		response := make([]api.GatewayAPIKeyResponse, len(keys))
		for i, k := range keys {
			response[i] = api.GatewayAPIKeyResponse{
				ID:          k.ID,
				Name:        k.Name,
				DisplayHint: k.DisplayHint,
				Status:      k.Status,
				ExpiresAt:   k.ExpiresAt,
				LastUsedAt:  k.LastUsedAt,
				CreatedAt:   k.CreatedAt,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}

// updateGatewayAPIKeyStatusHandler sets the status (active/revoked) of any
// consumer's gateway API key by ID. Admin-only.
func updateGatewayAPIKeyStatusHandler(cfg config.Config, db *gorm.DB, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateGatewayAPIKeyStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("update-gateway-api-key-status: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("update-gateway-api-key-status: validation failed", "id", req.ID, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var apiKey models.APIKey
		if err := db.WithContext(r.Context()).First(&apiKey, req.ID).Error; err != nil {
			slog.Warn("update-gateway-api-key-status: key not found", "id", req.ID, "error", err)
			api.WriteError(w, "Key not found", http.StatusNotFound)
			return
		}

		if err := db.WithContext(r.Context()).Model(&apiKey).Update("status", req.Status).Error; err != nil {
			slog.Error("update-gateway-api-key-status: could not update status", "id", req.ID, "error", err)
			api.WriteError(w, "Could not update status", http.StatusInternalServerError)
			return
		}

		if err := redis.InvalidateAPIKeys(r.Context(), []string{apiKey.KeyHash}); err != nil {
			slog.Error("update-gateway-api-key-status: could not invalidate cached sessions", "id", req.ID, "error", err)
			api.WriteError(w, "Status updated, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}
