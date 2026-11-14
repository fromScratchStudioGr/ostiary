package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

// Usage extraction is split in two on purpose:
//
//   - The wire format (how to find the usage object in an SSE stream or a
//     response body) depends on the endpoint schema: chat-completions vs
//     messages.
//   - What the numbers mean (which fields count as cache hits, price
//     multipliers, ...) depends on the provider, even when two providers
//     share a schema (DeepSeek speaks the OpenAI schema but reports cache
//     usage differently).
//
// formatFor ties the two together per provider.

// usageFormat extracts a priced-ready UsageInfo from a provider's responses.
// Both funcs return a nil *UsageInfo when the response carries no usage.
type usageFormat struct {
	stream func(data []byte) *UsageInfo
	body   func(data []byte) (*UsageInfo, error)
}

var (
	openAIFormat    = openAISchemaFormat((*OpenAIUsageInfo).toOpenAIUsageInfo)
	deepSeekFormat  = openAISchemaFormat((*OpenAIUsageInfo).toDeepSeekUsageInfo)
	anthropicFormat = anthropicSchemaFormat()
	geminiFormat    = geminiSchemaFormat()
)

// formatFor returns the usage format of the model's provider. An unknown
// provider is an error rather than a guess: silently parsing it with another
// provider's rules would record wrong costs.
func formatFor(model providers.Model) (usageFormat, error) {
	switch model.Provider {
	case "openai":
		return openAIFormat, nil
	case "deepseek":
		return deepSeekFormat, nil
	case "anthropic":
		return anthropicFormat, nil
	case "gemini":
		return geminiFormat, nil
	}
	return usageFormat{}, fmt.Errorf("no usage format for provider %q (model %q, endpoint %q)", model.Provider, model.Id, model.Endpoint)
}

// ParseStreamUsage extracts usage from a buffered SSE response of the model.
func ParseStreamUsage(model providers.Model, data []byte) (*UsageInfo, error) {
	format, err := formatFor(model)
	if err != nil {
		return nil, err
	}
	return format.stream(data), nil
}

// ParseBodyUsage extracts usage from a buffered, non-streamed response body
// of the model.
func ParseBodyUsage(model providers.Model, data []byte) (*UsageInfo, error) {
	format, err := formatFor(model)
	if err != nil {
		return nil, err
	}
	return format.body(data)
}

// openAISchemaFormat reads the chat-completions schema and lets the provider
// decide how its usage object is converted.
func openAISchemaFormat(convert func(*OpenAIUsageInfo) *UsageInfo) usageFormat {
	return usageFormat{
		stream: func(data []byte) *UsageInfo {
			return convert(scanOpenAIStreamUsage(data))
		},
		body: func(data []byte) (*UsageInfo, error) {
			var resp struct {
				Usage *OpenAIUsageInfo `json:"usage"`
			}
			if err := json.Unmarshal(data, &resp); err != nil {
				return nil, err
			}
			return convert(resp.Usage), nil
		},
	}
}

// scanOpenAIStreamUsage scans a buffered SSE stream for the "usage"
// object, which (when requested via stream_options.include_usage) only
// arrives in the final "data: {...}" chunk before "data: [DONE]".
func scanOpenAIStreamUsage(data []byte) *OpenAIUsageInfo {
	var usage *OpenAIUsageInfo
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
		if len(line) == 0 || string(line) == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *OpenAIUsageInfo `json:"usage"`
		}
		if err := json.Unmarshal(line, &chunk); err == nil && chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	return usage
}

func anthropicSchemaFormat() usageFormat {
	return usageFormat{
		stream: func(data []byte) *UsageInfo {
			return scanAnthropicStreamUsage(data).toUsageInfo()
		},
		body: func(data []byte) (*UsageInfo, error) {
			var resp struct {
				Usage *AnthropicUsageInfo `json:"usage"`
			}
			if err := json.Unmarshal(data, &resp); err != nil {
				return nil, err
			}
			return resp.Usage.toUsageInfo(), nil
		},
	}
}

// scanAnthropicStreamUsage scans a buffered SSE stream for the "usage"
// object, which (when requested via stream_options.include_usage) only
// arrives in the penultimate "data: {...}" chunk.
func scanAnthropicStreamUsage(data []byte) *AnthropicUsageInfo {
	var usage *AnthropicUsageInfo
	var currentEvent string
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if strings.HasPrefix(string(bytes.TrimSpace(line)), "data:") {
			line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
		} else if strings.HasPrefix(string(bytes.TrimSpace(line)), "event:") {
			currentEvent = string(bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("event:"))))
			continue
		} else {
			continue
		}

		switch currentEvent {
		case "message_start":
			// On message_start we read all caching details as well as input tokens
			var chunk struct {
				Message struct {
					Usage *AnthropicUsageInfo `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &chunk); err == nil && chunk.Message.Usage != nil {
				usage = chunk.Message.Usage
			}
		case "message_delta":
			// On the message_delta details we read the output tokens
			var chunk struct {
				Usage *AnthropicUsageInfo `json:"usage"`
			}
			if err := json.Unmarshal(line, &chunk); err == nil && chunk.Usage != nil && usage != nil {
				usage.OutputTokens = chunk.Usage.OutputTokens
			}
		}
	}

	return usage
}

// geminiSchemaFormat reads the Interactions API schema. Usage sits at the top
// level of a non-streaming response, but only arrives in the stream nested
// under "interaction" on the interaction.completed event — no other event
// carries it.
func geminiSchemaFormat() usageFormat {
	return usageFormat{
		stream: func(data []byte) *UsageInfo {
			return scanGeminiStreamUsage(data).toUsageInfo()
		},
		body: func(data []byte) (*UsageInfo, error) {
			var resp struct {
				Usage *GeminiUsageInfo `json:"usage"`
			}
			if err := json.Unmarshal(data, &resp); err != nil {
				return nil, err
			}
			return resp.Usage.toUsageInfo(), nil
		},
	}
}

// scanGeminiStreamUsage scans a buffered SSE stream for the interaction.completed
// event, the only one carrying usage, nested under "interaction".
func scanGeminiStreamUsage(data []byte) *GeminiUsageInfo {
	var usage *GeminiUsageInfo
	var currentEvent string
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			currentEvent = string(bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("event:"))))
			continue
		case bytes.HasPrefix(trimmed, []byte("data:")):
			trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		default:
			continue
		}

		if currentEvent != "interaction.completed" {
			continue
		}
		var chunk struct {
			Interaction struct {
				Usage *GeminiUsageInfo `json:"usage"`
			} `json:"interaction"`
		}
		if err := json.Unmarshal(trimmed, &chunk); err != nil {
			slog.Warn("could not parse gemini interaction.completed event, ignoring it", "error", err)
			continue
		}
		if chunk.Interaction.Usage != nil {
			usage = chunk.Interaction.Usage
		}
	}

	return usage
}

// toOpenAIUsageInfo converts usage as reported by OpenAI.
func (u *OpenAIUsageInfo) toOpenAIUsageInfo() *UsageInfo {
	if u == nil {
		return nil
	}

	return &UsageInfo{
		CompletionTokens:             u.CompletionTokens,
		PromptCacheHitTokens:         u.PromptTokensDetails.CachedTokens,
		PromptCacheMissTokens:        u.PromptTokens - u.PromptTokensDetails.CachedTokens - u.PromptTokensDetails.CacheWriteTokens,
		PromptCacheWriteLevel1Tokens: u.PromptTokensDetails.CacheWriteTokens,
		PromptCacheWriteLevel2Tokens: 0,
		PriceMultiplier:              1,
	}
}

// toDeepSeekUsageInfo converts usage as reported by DeepSeek, which reports cache
// hits and misses directly instead of through prompt_tokens_details. DeepSeek
// has no cache write pricing.
func (u *OpenAIUsageInfo) toDeepSeekUsageInfo() *UsageInfo {
	if u == nil {
		return nil
	}

	info := &UsageInfo{
		CompletionTokens: u.CompletionTokens,
		PriceMultiplier:  1,
	}
	if u.PromptCacheHitTokens != nil {
		info.PromptCacheHitTokens = *u.PromptCacheHitTokens
	}
	if u.PromptCacheMissTokens != nil {
		info.PromptCacheMissTokens = *u.PromptCacheMissTokens
	} else {
		// Never bill the input as free if the field is missing.
		info.PromptCacheMissTokens = u.PromptTokens - info.PromptCacheHitTokens
	}

	return info
}

// toUsageInfo converts usage as reported by Anthropic.
func (u *AnthropicUsageInfo) toUsageInfo() *UsageInfo {
	if u == nil {
		return nil
	}

	var regionalMultiplier = 1.0
	if u.InferenceGeo == "us" {
		regionalMultiplier = 1.1
	}

	info := &UsageInfo{
		CompletionTokens:             u.OutputTokens,
		PromptCacheMissTokens:        u.InputTokens,
		PromptCacheHitTokens:         u.CacheReadInputTokens,
		PromptCacheWriteLevel1Tokens: 0,
		PromptCacheWriteLevel2Tokens: 0,
		PriceMultiplier:              regionalMultiplier,
	}

	if u.CacheCreationInputTokens != 0 && u.CacheCreation.Ephemeral1hInputTokens == 0 && u.CacheCreation.Ephemeral5mInputTokens == 0 {
		// If no ephemeral tokens are provided, we handle cache_creation_input_tokens
		// as 5m cache window tokens
		info.PromptCacheWriteLevel1Tokens = u.CacheCreationInputTokens
	} else {
		info.PromptCacheWriteLevel1Tokens = u.CacheCreation.Ephemeral5mInputTokens
		info.PromptCacheWriteLevel2Tokens = u.CacheCreation.Ephemeral1hInputTokens
	}

	return info
}

// toUsageInfo converts usage as reported by Gemini.
func (u *GeminiUsageInfo) toUsageInfo() *UsageInfo {
	if u == nil {
		return nil
	}

	return &UsageInfo{
		CompletionTokens:      u.TotalOutputTokens + u.TotalThoughtTokens + u.TotalToolUseTokens,
		PromptCacheHitTokens:  u.TotalCachedTokens,
		PromptCacheMissTokens: u.TotalInputTokens - u.TotalCachedTokens,
		PriceMultiplier:       1,
	}
}
