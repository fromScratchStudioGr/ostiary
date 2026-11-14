package usage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"

	"github.com/andybalholm/brotli"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

type UsageInfo struct {
	CompletionTokens             int
	PromptCacheHitTokens         int
	PromptCacheMissTokens        int
	PromptCacheWriteLevel1Tokens int
	PromptCacheWriteLevel2Tokens int
	// This is used by Anthropic as they have
	// a price multipler for certain regions.
	PriceMultiplier float64
}

type OpenAIUsageInfo struct {
	PromptTokens          int                     `json:"prompt_tokens"`
	CompletionTokens      int                     `json:"completion_tokens"`
	PromptCacheHitTokens  *int                    `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens *int                    `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   OpenAIUsageTokenDetails `json:"prompt_tokens_details"`
}

type OpenAIUsageTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
	// This is missing in OpenAI models prior to 5.6,
	// but a 'null' value is handled as '0' to keep
	// the mapping generic.
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type AnthropicUsageInfo struct {
	InputTokens              int                         `json:"input_tokens"`
	OutputTokens             int                         `json:"output_tokens"`
	CacheCreationInputTokens int                         `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                         `json:"cache_read_input_tokens"`
	CacheCreation            AnthropicUsageCacheCreation `json:"cache_creation"`
	InferenceGeo             string                      `json:"inference_geo"`
}

type AnthropicUsageCacheCreation struct {
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
}

type GeminiUsageInfo struct {
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalCachedTokens  int `json:"total_cached_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalThoughtTokens int `json:"total_thought_tokens"`
	TotalToolUseTokens int `json:"total_tool_use_tokens"`
}

// UnmarshalJSON requires every usage field, present and non-null (0 is fine).
// Gemini reports all of them even when they're zero, so a missing one means
// the response schema changed, and silently defaulting it to 0 would misprice
// the request: input defaulting to 0, for instance, makes the derived cache
// miss count (input minus cached) negative. Failing instead gets logged.
func (u *GeminiUsageInfo) UnmarshalJSON(data []byte) error {
	var raw struct {
		TotalInputTokens   *int `json:"total_input_tokens"`
		TotalCachedTokens  *int `json:"total_cached_tokens"`
		TotalOutputTokens  *int `json:"total_output_tokens"`
		TotalThoughtTokens *int `json:"total_thought_tokens"`
		TotalToolUseTokens *int `json:"total_tool_use_tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var missing []string
	for name, value := range map[string]*int{
		"total_input_tokens":    raw.TotalInputTokens,
		"total_cached_tokens":   raw.TotalCachedTokens,
		"total_output_tokens":   raw.TotalOutputTokens,
		"total_thought_tokens":  raw.TotalThoughtTokens,
		"total_tool_use_tokens": raw.TotalToolUseTokens,
	} {
		if value == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("gemini usage is missing %s", strings.Join(missing, ", "))
	}

	*u = GeminiUsageInfo{
		TotalInputTokens:   *raw.TotalInputTokens,
		TotalCachedTokens:  *raw.TotalCachedTokens,
		TotalOutputTokens:  *raw.TotalOutputTokens,
		TotalThoughtTokens: *raw.TotalThoughtTokens,
		TotalToolUseTokens: *raw.TotalToolUseTokens,
	}
	return nil
}

// tierCost prices tokens of an optional cost tier. A nil price means the
// model has no such tier, so tokens reported for it can't be priced: they're
// still recorded, billed at 0, and flagged so a missing price gets noticed.
func tierCost(model providers.Model, tier string, tokens int, price *float32, multiplier float64) float64 {
	if price == nil {
		if tokens > 0 {
			slog.Warn("usage reports tokens for a cost tier the model has no price for, billed at 0", "model", model.Id, "tier", tier, "tokens", tokens)
		}
		return 0
	}
	return float64(tokens) / 1_000_000 * float64(*price) * multiplier
}

// toUsageRecord computes per-request cost from usage, using the model's
// per-token pricing, and builds the row to persist. requestedAt is when the
// request was made, which decides whether peak-hour pricing applies.
func toUsageRecord(model providers.Model, consumerID, providerKeyID uint, usage *UsageInfo, requestedAt time.Time) models.Usage {
	multiplier := usage.PriceMultiplier * model.PeakPricing.MultiplierAt(requestedAt)

	// Long requests are priced entirely at the model's long-context rates.
	inputTokens := usage.PromptCacheHitTokens + usage.PromptCacheMissTokens + usage.PromptCacheWriteLevel1Tokens + usage.PromptCacheWriteLevel2Tokens
	cost := model.Cost.ForInputTokens(inputTokens)

	cacheHitCost := tierCost(model, "cache_read", usage.PromptCacheHitTokens, cost.CacheRead, multiplier)
	cacheMissCost := float64(usage.PromptCacheMissTokens) / 1_000_000 * float64(cost.Input) * multiplier
	cacheWriteLevel1Cost := tierCost(model, "cache_write_level1", usage.PromptCacheWriteLevel1Tokens, cost.CacheWriteLevel1, multiplier)
	cacheWriteLevel2Cost := tierCost(model, "cache_write_level2", usage.PromptCacheWriteLevel2Tokens, cost.CacheWriteLevel2, multiplier)
	outputCost := float64(usage.CompletionTokens) / 1_000_000 * float64(cost.Output) * multiplier

	return models.Usage{
		ConsumerID:                       consumerID,
		ProviderKeyID:                    providerKeyID,
		CompletionTokens:                 usage.CompletionTokens,
		CompletionTokensCost:             outputCost,
		PromptCacheHitTokens:             usage.PromptCacheHitTokens,
		PromptCacheHitTokensCost:         cacheHitCost,
		PromptCacheMissTokens:            usage.PromptCacheMissTokens,
		PromptCacheMissTokensCost:        cacheMissCost,
		PromptCacheWriteLevel1Tokens:     usage.PromptCacheWriteLevel1Tokens,
		PromptCacheWriteLevel1TokensCost: cacheWriteLevel1Cost,
		PromptCacheWriteLevel2Tokens:     usage.PromptCacheWriteLevel2Tokens,
		PromptCacheWriteLevel2TokensCost: cacheWriteLevel2Cost,
	}
}

// LogUsage enqueues a task to persist a usage record asynchronously, so a
// slow DB write can't add latency to the proxied request in the normal
// case — the actual write happens later, in HandleLogUsageTask (task.go).
//
// If enqueueing itself fails (e.g. Redis is unreachable), this falls back
// to a synchronous DB write rather than silently losing the record — usage
// feeds billing, so an occasional slower write beats a missing one during
// an outage. usage may be nil (e.g. no usage found), in which case nothing
// happens either way. The queued task is retried up to maxRetry times.
func LogUsage(ctx context.Context, client *asynq.Client, db *gorm.DB, redis *cache.Cache, model providers.Model, consumerID, providerKeyID uint, usage *UsageInfo, requestedAt time.Time, maxRetry int) {
	if usage == nil {
		return
	}

	// Computed once, up front — both the enqueue path and the fallback
	// direct write persist this exact same record, so they can never bill
	// a request differently depending on which path happened to handle it.
	record := toUsageRecord(model, consumerID, providerKeyID, usage, requestedAt)

	task, err := newLogUsageTask(record, maxRetry)
	if err == nil {
		if _, err := client.Enqueue(task); err == nil {
			return
		} else {
			slog.Error("could not enqueue usage task, falling back to direct write", "model", model.Id, "error", err)
		}
	} else {
		slog.Error("could not build usage task, falling back to direct write", "model", model.Id, "error", err)
	}

	// The request's context is cancelled once the response is done (or the
	// client disconnects), and that mustn't cost us the billing record.
	if err := persistUsage(context.WithoutCancel(ctx), db, redis, record); err != nil {
		slog.Error("could not store usage (both enqueue and direct write failed)", "model", model.Id, "error", err)
	}
}

// DecodeContentEncoding decompresses data per the response's Content-Encoding.
// The proxy drops the client's Accept-Encoding, so Go's http.Transport asks
// for gzip itself and decompresses the response before it gets here: this is
// only a fallback for a provider that compresses without being asked.
func DecodeContentEncoding(encoding string, data []byte) ([]byte, error) {
	// Content codings are case-insensitive (RFC 9110, section 8.4.1): a
	// response saying "GZIP" is gzip, and passing it on undecoded would leave
	// the usage parser looking at compressed bytes.
	switch strings.ToLower(encoding) {
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		return io.ReadAll(gz)
	case "br":
		return io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
	default:
		return data, nil
	}
}

// UsageTeeBody wraps a response body so bytes flow straight through to the
// client as they're read (preserving real-time SSE delivery via
// ReverseProxy's copy loop), while also accumulating them into a buffer.
// Once the upstream stream ends, ReverseProxy closes the body, and onClose
// runs against everything that was accumulated.
type UsageTeeBody struct {
	io.ReadCloser
	buf     bytes.Buffer
	OnClose func(data []byte)
	done    bool // reading has finished, at the end or on an error
}

func (t *UsageTeeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.buf.Write(p[:n])
	}
	if err != nil {
		t.done = true
	}
	return n, err
}

// Close runs OnClose on everything the body carried. A body closed while it
// could still be read (the proxy stops copying when the client goes away) is
// first read to the end in the background, since the usage usually comes
// last: Close returns at once, and the reading stops when the body's request
// is cancelled.
func (t *UsageTeeBody) Close() error {
	if t.done {
		err := t.ReadCloser.Close()
		t.OnClose(t.buf.Bytes())
		return err
	}

	go func() {
		io.Copy(&t.buf, t.ReadCloser)
		t.ReadCloser.Close()
		t.OnClose(t.buf.Bytes())
	}()
	return nil
}
