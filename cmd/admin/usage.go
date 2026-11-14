package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"gorm.io/gorm"
)

const costExpr = "COALESCE(SUM(usages.completion_tokens_cost + usages.prompt_cache_hit_tokens_cost + usages.prompt_cache_miss_tokens_cost + usages.prompt_cache_write_level1_tokens_cost + usages.prompt_cache_write_level2_tokens_cost), 0)"
const tokensExpr = "COALESCE(SUM(usages.completion_tokens + usages.prompt_cache_hit_tokens + usages.prompt_cache_miss_tokens + usages.prompt_cache_write_level1_tokens + usages.prompt_cache_write_level2_tokens), 0)"

// usageFilter is what the usage endpoints accept to narrow their numbers: the
// last N days (?timespan=) and/or a single provider (?provider=).
type usageFilter struct {
	since    time.Time
	provider string
}

// parseUsageFilter reads the filter from the request's query string. When it's
// invalid, the second result is the message to send back to the client.
func parseUsageFilter(r *http.Request) (usageFilter, string) {
	var filter usageFilter

	if timespan := r.URL.Query().Get("timespan"); timespan != "" {
		days, err := strconv.Atoi(timespan)
		if err != nil || days < 0 {
			return usageFilter{}, "Invalid timespan"
		}
		filter.since = time.Now().AddDate(0, 0, -days)
	}

	filter.provider = r.URL.Query().Get("provider")
	switch filter.provider {
	case "", "anthropic", "openai", "deepseek", "gemini":
	default:
		return usageFilter{}, "Invalid provider"
	}

	return filter, ""
}

// usageQuery starts a query over the usages the filter lets through, joined to
// their provider keys.
func usageQuery(ctx context.Context, db *gorm.DB, filter usageFilter) *gorm.DB {
	q := db.WithContext(ctx).
		Model(&models.Usage{}).
		Joins("JOIN provider_keys ON provider_keys.id = usages.provider_key_id")
	if !filter.since.IsZero() {
		q = q.Where("usages.created_at >= ?", filter.since)
	}
	if filter.provider != "" {
		q = q.Where("provider_keys.provider = ?", filter.provider)
	}
	return q
}

func getGeneralUsageHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		filter, invalid := parseUsageFilter(r)
		if invalid != "" {
			slog.Warn("get-usage: invalid filter", "reason", invalid, "query", r.URL.RawQuery)
			api.WriteError(w, invalid, http.StatusBadRequest)
			return
		}

		var summary struct {
			TotalCost   float64
			TotalTokens int64
			UserCount   int64
			KeyCount    int64
		}
		err := usageQuery(r.Context(), db, filter).
			Select(costExpr + " AS total_cost, " + tokensExpr + " AS total_tokens, COUNT(DISTINCT usages.consumer_id) AS user_count, COUNT(DISTINCT usages.provider_key_id) AS key_count").
			Scan(&summary).Error
		if err != nil {
			slog.Error("get-usage: could not fetch usage summary", "error", err)
			api.WriteError(w, "Could not fetch usage summary", http.StatusInternalServerError)
			return
		}

		var costOverTime []api.DailyCostResponse
		err = usageQuery(r.Context(), db, filter).
			Select("TO_CHAR(usages.created_at, 'YYYY-MM-DD') AS date, " + costExpr + " AS cost").
			Group("TO_CHAR(usages.created_at, 'YYYY-MM-DD')").
			Order("date").
			Scan(&costOverTime).Error
		if err != nil {
			slog.Error("get-usage: could not fetch cost over time", "error", err)
			api.WriteError(w, "Could not fetch cost over time", http.StatusInternalServerError)
			return
		}
		if costOverTime == nil {
			costOverTime = []api.DailyCostResponse{}
		}

		response := api.UsageSummaryResponse{
			TotalCost:    summary.TotalCost,
			TotalTokens:  summary.TotalTokens,
			UserCount:    summary.UserCount,
			KeyCount:     summary.KeyCount,
			CostOverTime: costOverTime,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}

// getConsumerUsageHandler returns one consumer's usage stats, optionally
// scoped to the last N days (?timespan=) and/or a single provider
// (?provider=).
func getConsumerUsageHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		consumerID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			slog.Warn("get-consumer-usage: invalid consumer id", "id", r.PathValue("id"), "error", err)
			api.WriteError(w, "Invalid consumer id", http.StatusBadRequest)
			return
		}

		var consumer models.Consumer
		if err := db.WithContext(r.Context()).First(&consumer, consumerID).Error; err != nil {
			slog.Warn("get-consumer-usage: consumer not found", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Consumer not found", http.StatusNotFound)
			return
		}

		filter, invalid := parseUsageFilter(r)
		if invalid != "" {
			slog.Warn("get-consumer-usage: invalid filter", "reason", invalid, "query", r.URL.RawQuery)
			api.WriteError(w, invalid, http.StatusBadRequest)
			return
		}

		baseQuery := func() *gorm.DB {
			return usageQuery(r.Context(), db, filter).Where("usages.consumer_id = ?", consumerID)
		}

		var summary struct {
			TotalCost   float64
			TotalTokens int64
		}
		if err := baseQuery().
			Select(costExpr + " AS total_cost, " + tokensExpr + " AS total_tokens").
			Scan(&summary).Error; err != nil {
			slog.Error("get-consumer-usage: could not fetch usage summary", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Could not fetch usage summary", http.StatusInternalServerError)
			return
		}

		var tokensOverTime []api.DailyTokensResponse
		if err := baseQuery().
			Select("TO_CHAR(usages.created_at, 'YYYY-MM-DD') AS date, " + tokensExpr + " AS tokens").
			Group("TO_CHAR(usages.created_at, 'YYYY-MM-DD')").
			Order("date").
			Scan(&tokensOverTime).Error; err != nil {
			slog.Error("get-consumer-usage: could not fetch tokens over time", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Could not fetch tokens over time", http.StatusInternalServerError)
			return
		}
		if tokensOverTime == nil {
			tokensOverTime = []api.DailyTokensResponse{}
		}

		var costByKey []api.KeyCostResponse
		if err := baseQuery().
			Select("usages.provider_key_id AS provider_key_id, provider_keys.provider AS provider, " + costExpr + " AS cost").
			Group("usages.provider_key_id, provider_keys.provider").
			Order("cost DESC").
			Scan(&costByKey).Error; err != nil {
			slog.Error("get-consumer-usage: could not fetch cost by key", "consumer_id", consumerID, "error", err)
			api.WriteError(w, "Could not fetch cost by key", http.StatusInternalServerError)
			return
		}
		if costByKey == nil {
			costByKey = []api.KeyCostResponse{}
		}

		response := api.UserUsageSummaryResponse{
			TotalCost:      summary.TotalCost,
			TotalTokens:    summary.TotalTokens,
			TokensOverTime: tokensOverTime,
			CostByKey:      costByKey,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}
