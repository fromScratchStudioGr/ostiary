package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// TaskTypeLogUsage identifies the background task that persists a usage
// record. Enqueued by LogUsage, processed by HandleLogUsageTask.
const TaskTypeLogUsage = "usage:log"

// logUsageTaskPayload is the JSON payload carried by a TaskTypeLogUsage
// task. It carries an already-priced record (cost computed once, at
// request time, by toUsageRecord) rather than a model ID for the handler
// to re-price later — that guarantees a request is billed at the price in
// effect when it was made, regardless of when the task actually gets
// processed (e.g. after a backlog, or after a pricing change ships).
type logUsageTaskPayload struct {
	ConsumerID                       uint    `json:"consumer_id"`
	ProviderKeyID                    uint    `json:"provider_key_id"`
	CompletionTokens                 int     `json:"completion_tokens"`
	CompletionTokensCost             float64 `json:"completion_tokens_cost"`
	PromptCacheHitTokens             int     `json:"prompt_cache_hit_tokens"`
	PromptCacheHitTokensCost         float64 `json:"prompt_cache_hit_tokens_cost"`
	PromptCacheMissTokens            int     `json:"prompt_cache_miss_tokens"`
	PromptCacheMissTokensCost        float64 `json:"prompt_cache_miss_tokens_cost"`
	PromptCacheWriteLevel1Tokens     int     `json:"prompt_cache_write_level1_tokens"`
	PromptCacheWriteLevel1TokensCost float64 `json:"prompt_cache_write_level1_tokens_cost"`
	PromptCacheWriteLevel2Tokens     int     `json:"prompt_cache_write_level2_tokens"`
	PromptCacheWriteLevel2TokensCost float64 `json:"prompt_cache_write_level2_tokens_cost"`
}

// newLogUsageTask builds the asynq task to enqueue for persisting an
// already-priced usage record, retried up to maxRetry times.
func newLogUsageTask(record models.Usage, maxRetry int) (*asynq.Task, error) {
	payload, err := json.Marshal(logUsageTaskPayload{
		ConsumerID:                       record.ConsumerID,
		ProviderKeyID:                    record.ProviderKeyID,
		CompletionTokens:                 record.CompletionTokens,
		CompletionTokensCost:             record.CompletionTokensCost,
		PromptCacheHitTokens:             record.PromptCacheHitTokens,
		PromptCacheHitTokensCost:         record.PromptCacheHitTokensCost,
		PromptCacheMissTokens:            record.PromptCacheMissTokens,
		PromptCacheMissTokensCost:        record.PromptCacheMissTokensCost,
		PromptCacheWriteLevel1Tokens:     record.PromptCacheWriteLevel1Tokens,
		PromptCacheWriteLevel1TokensCost: record.PromptCacheWriteLevel1TokensCost,
		PromptCacheWriteLevel2Tokens:     record.PromptCacheWriteLevel2Tokens,
		PromptCacheWriteLevel2TokensCost: record.PromptCacheWriteLevel2TokensCost,
	})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TaskTypeLogUsage, payload, asynq.MaxRetry(maxRetry)), nil
}

// HandleLogUsageTask processes a TaskTypeLogUsage task: it decodes the
// already-priced payload and persists it as-is. No pricing lookup here —
// cost was already computed once, at request time, before this was
// enqueued. Register this against TaskTypeLogUsage on an asynq.ServeMux.
func HandleLogUsageTask(db *gorm.DB, redis *cache.Cache) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var payload logUsageTaskPayload
		if err := json.Unmarshal(t.Payload(), &payload); err != nil {
			return fmt.Errorf("could not unmarshal usage task payload: %w", err)
		}

		record := models.Usage{
			ConsumerID:                       payload.ConsumerID,
			ProviderKeyID:                    payload.ProviderKeyID,
			CompletionTokens:                 payload.CompletionTokens,
			CompletionTokensCost:             payload.CompletionTokensCost,
			PromptCacheHitTokens:             payload.PromptCacheHitTokens,
			PromptCacheHitTokensCost:         payload.PromptCacheHitTokensCost,
			PromptCacheMissTokens:            payload.PromptCacheMissTokens,
			PromptCacheMissTokensCost:        payload.PromptCacheMissTokensCost,
			PromptCacheWriteLevel1Tokens:     payload.PromptCacheWriteLevel1Tokens,
			PromptCacheWriteLevel1TokensCost: payload.PromptCacheWriteLevel1TokensCost,
			PromptCacheWriteLevel2Tokens:     payload.PromptCacheWriteLevel2Tokens,
			PromptCacheWriteLevel2TokensCost: payload.PromptCacheWriteLevel2TokensCost,
		}

		return persistUsage(ctx, db, redis, record)
	}
}

func persistUsage(ctx context.Context, db *gorm.DB, redis *cache.Cache, record models.Usage) error {
	totalCost := record.CompletionTokensCost + record.PromptCacheHitTokensCost + record.PromptCacheMissTokensCost + record.PromptCacheWriteLevel1TokensCost + record.PromptCacheWriteLevel2TokensCost

	limitExceeded := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}

		result := tx.Model(&models.UserProviderAccess{}).
			Where("consumer_id = ? AND provider_key_id = ?", record.ConsumerID, record.ProviderKeyID).
			Update("total_cost", gorm.Expr("total_cost + ?", totalCost))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			// Same condition getKeyAccess uses to reject a request.
			var access models.UserProviderAccess
			if err := tx.Select("usage_limit", "total_cost").
				Where("consumer_id = ? AND provider_key_id = ?", record.ConsumerID, record.ProviderKeyID).
				First(&access).Error; err != nil {
				return err
			}
			limitExceeded = access.UsageLimit > 0 && access.TotalCost > access.UsageLimit
		}
		if result.RowsAffected == 0 {
			// Access was revoked/removed between the request being served
			// and this running. The usage record above is still
			// correct and kept, but the running total this row exists to
			// support can no longer be updated — retrying wouldn't help
			// since the row is gone, so just log rather than fail the
			// task (which would trigger up to 25 retries for nothing).
			slog.Warn("no user_provider_access row, total_cost not updated", "consumer_id", record.ConsumerID, "provider_key_id", record.ProviderKeyID)
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Cached sessions skip the limit check, so drop them once it's passed.
	// Done after the commit so a request can't re-cache from the old total,
	// and never returned as an error: a retry would persist the usage
	// record a second time.
	if limitExceeded {
		if err := redis.InvalidateConsumer(ctx, record.ConsumerID); err != nil {
			slog.Error("could not invalidate cached sessions after usage limit was exceeded", "consumer_id", record.ConsumerID, "provider_key_id", record.ProviderKeyID, "error", err)
		}
	}

	return nil
}
