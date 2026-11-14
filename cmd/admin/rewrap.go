package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"

	"gorm.io/gorm"
)

const (
	// rewrapLockID is the Postgres advisory lock that keeps two rewraps from
	// running at once.
	rewrapLockID = 7348291
	// rewrapBatchSize is how many provider keys are read from Postgres at a time.
	rewrapBatchSize = 100
	// rewrapTimeout bounds one whole run.
	rewrapTimeout = 10 * time.Minute
)

// rewrapProviderKeysHandler re-wraps every provider key's DEK under the
// current version of the KMS key that wrapped it, so a rotated key can be
// retired. Soft-deleted keys are included, so none is left on an old version.
// Running it again is harmless, with or without a rotation in between.
func rewrapProviderKeysHandler(cfg config.Config, db *gorm.DB, kms keystore.KMSClient, redis *cache.Cache) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		rewrapper, ok := kms.(keystore.Rewrapper)
		if !ok {
			slog.Warn("rewrap-provider-keys: the configured KMS does not support rewrapping")
			api.WriteError(w, "The configured KMS does not support rewrapping keys", http.StatusBadRequest)
			return
		}

		// A run that stopped half-way when the client went away would leave
		// the keys split between versions, so it finishes regardless.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rewrapTimeout)
		defer cancel()

		var (
			locked bool
			result api.RewrapProviderKeysResponse
			runErr error
		)
		// The lock belongs to a Postgres session, so it's taken and released
		// on one connection.
		err := db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
			if err := conn.Raw("SELECT pg_try_advisory_lock(?)", rewrapLockID).Scan(&locked).Error; err != nil {
				return err
			}
			if !locked {
				return nil
			}
			defer conn.WithContext(context.Background()).Exec("SELECT pg_advisory_unlock(?)", rewrapLockID)

			result, runErr = rewrapProviderKeys(ctx, conn, rewrapper)
			return nil
		})
		if err == nil && !locked {
			slog.Warn("rewrap-provider-keys: another rewrap is already running")
			api.WriteError(w, "A rewrap is already running", http.StatusConflict)
			return
		}
		if err == nil {
			err = runErr
		}

		// The sessions hold the old wrapped DEKs, so they go as soon as any
		// key was rewrapped, even if the run did not get to the end.
		var flushErr error
		if result.Rewrapped > 0 {
			if flushErr = redis.InvalidateAllSessions(ctx); flushErr != nil {
				slog.Error("rewrap-provider-keys: could not invalidate cached sessions", "error", flushErr)
			}
		}

		if err != nil {
			slog.Error("rewrap-provider-keys: could not rewrap provider keys", "rewrapped", result.Rewrapped, "failed", result.Failed, "error", err)
			api.WriteError(w, "Could not rewrap provider keys", http.StatusInternalServerError)
			return
		}
		if flushErr != nil {
			api.WriteError(w, "Provider keys rewrapped, but cached sessions could not be invalidated", http.StatusInternalServerError)
			return
		}

		slog.Info("rewrap-provider-keys: done", "rewrapped", result.Rewrapped, "failed", result.Failed)

		status := http.StatusOK
		if result.Failed > 0 {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(result)
	})
}

// rewrapProviderKeys goes through every provider key, soft-deleted ones
// included, in id order. A key that can't be rewrapped is reported and the
// run goes on; an error is returned only when the keys can't be read, along
// with what was done up to then.
func rewrapProviderKeys(ctx context.Context, db *gorm.DB, rewrapper keystore.Rewrapper) (api.RewrapProviderKeysResponse, error) {
	result := api.RewrapProviderKeysResponse{FailedIDs: []uint{}}

	var afterID uint
	for {
		var batch []models.ProviderKey
		err := db.WithContext(ctx).Unscoped().
			Select("id", "encrypted_dek", "kms_key_id").
			Where("id > ?", afterID).
			Order("id").
			Limit(rewrapBatchSize).
			Find(&batch).Error
		if err != nil {
			return result, err
		}
		if len(batch) == 0 {
			return result, nil
		}

		for _, providerKey := range batch {
			afterID = providerKey.ID
			if err := rewrapProviderKey(ctx, db, rewrapper, providerKey); err != nil {
				slog.Error("rewrap-provider-keys: could not rewrap provider key", "provider_key_id", providerKey.ID, "error", err)
				result.Failed++
				result.FailedIDs = append(result.FailedIDs, providerKey.ID)
				continue
			}
			result.Rewrapped++
		}
	}
}

// rewrapProviderKey stores the rewrapped DEK only if the row still holds the
// one it was read with, so a concurrent change is never overwritten.
func rewrapProviderKey(ctx context.Context, db *gorm.DB, rewrapper keystore.Rewrapper, providerKey models.ProviderKey) error {
	rewrapped, err := rewrapper.Rewrap(ctx, providerKey.EncryptedDEK, providerKey.KMSKeyID)
	if err != nil {
		return err
	}

	update := db.WithContext(ctx).Unscoped().
		Model(&models.ProviderKey{}).
		Where("id = ? AND encrypted_dek = ?", providerKey.ID, providerKey.EncryptedDEK).
		UpdateColumn("encrypted_dek", rewrapped)
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected == 0 {
		return fmt.Errorf("the provider key changed while it was being rewrapped")
	}
	return nil
}
