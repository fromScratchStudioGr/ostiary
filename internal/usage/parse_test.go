package usage

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

func TestParseBodyUsage(t *testing.T) {
	const errorBody = `{"error":{"message":"rate limited"}}`

	tests := []struct {
		name    string
		model   providers.Model
		body    string
		want    *UsageInfo
		wantErr bool
	}{
		{
			name:  "openai uses prompt_tokens_details",
			model: providers.Model{Provider: "openai", Endpoint: providers.OpenAIEndpoint},
			body:  `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":10}}}`,
			want:  &UsageInfo{CompletionTokens: 20, PromptCacheHitTokens: 30, PromptCacheMissTokens: 60, PromptCacheWriteLevel1Tokens: 10, PriceMultiplier: 1},
		},
		{
			name:  "deepseek uses hit/miss fields",
			model: providers.Model{Provider: "deepseek", Endpoint: providers.OpenAIEndpoint},
			body:  `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":70,"prompt_cache_miss_tokens":30}}`,
			want:  &UsageInfo{CompletionTokens: 20, PromptCacheHitTokens: 70, PromptCacheMissTokens: 30, PriceMultiplier: 1},
		},
		{
			name:  "deepseek derives miss tokens when missing",
			model: providers.Model{Provider: "deepseek", Endpoint: providers.OpenAIEndpoint},
			body:  `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":70}}`,
			want:  &UsageInfo{CompletionTokens: 20, PromptCacheHitTokens: 70, PromptCacheMissTokens: 30, PriceMultiplier: 1},
		},
		{
			name:  "anthropic applies the regional multiplier",
			model: providers.Model{Provider: "anthropic", Endpoint: providers.AnthropicEndpoint},
			body:  `{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"inference_geo":"us"}}`,
			want:  &UsageInfo{CompletionTokens: 5, PromptCacheHitTokens: 3, PromptCacheMissTokens: 10, PromptCacheWriteLevel1Tokens: 2, PriceMultiplier: 1.1},
		},

		// The gateway attaches ParseBodyUsage to every non-stream response,
		// provider error responses included, and those carry no usage. That
		// has to come back as "no usage" and not as an error, or every
		// provider error would be logged as a parse failure.
		{
			name:  "openai error response has no usage",
			model: providers.Model{Provider: "openai", Endpoint: providers.OpenAIEndpoint},
			body:  errorBody,
		},
		{
			name:  "deepseek error response has no usage",
			model: providers.Model{Provider: "deepseek", Endpoint: providers.OpenAIEndpoint},
			body:  errorBody,
		},
		{
			name:  "anthropic error response has no usage",
			model: providers.Model{Provider: "anthropic", Endpoint: providers.AnthropicEndpoint},
			body:  errorBody,
		},
		{
			name:  "gemini error response has no usage",
			model: geminiModel(),
			body:  errorBody,
		},

		{
			name:    "unknown provider returns an error",
			model:   providers.Model{Id: "x", Provider: "newprovider", Endpoint: providers.OpenAIEndpoint},
			body:    `{"usage":{"completion_tokens":1}}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBodyUsage(tt.model, []byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("got error %v, want error: %v", err, tt.wantErr)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseStreamUsage(t *testing.T) {
	const openAIStreamWithoutUsage = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"

	tests := []struct {
		name    string
		model   providers.Model
		stream  string
		want    *UsageInfo
		wantErr bool
	}{
		{
			name:   "openai-schema stream",
			model:  providers.Model{Provider: "deepseek", Endpoint: providers.OpenAIEndpoint},
			stream: "data: {\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_cache_hit_tokens\":70,\"prompt_cache_miss_tokens\":30}}\n\ndata: [DONE]\n\n",
			want:   &UsageInfo{CompletionTokens: 20, PromptCacheHitTokens: 70, PromptCacheMissTokens: 30, PriceMultiplier: 1},
		},
		{
			name:  "anthropic stream",
			model: providers.Model{Provider: "anthropic", Endpoint: providers.AnthropicEndpoint},
			stream: "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
				"event: message_delta\ndata: {\"usage\":{\"output_tokens\":5}}\n\n",
			want: &UsageInfo{CompletionTokens: 5, PromptCacheMissTokens: 10, PriceMultiplier: 1},
		},
		{
			// A message_delta without a preceding message_start used to nil-deref.
			name:   "anthropic message_delta without message_start has no usage",
			model:  providers.Model{Provider: "anthropic", Endpoint: providers.AnthropicEndpoint},
			stream: "event: message_delta\ndata: {\"usage\":{\"output_tokens\":5}}\n",
		},

		// A stream that ends without usage (cut short, or errored) is "no
		// usage", not an error.
		{
			name:   "openai stream without usage",
			model:  providers.Model{Provider: "openai", Endpoint: providers.OpenAIEndpoint},
			stream: openAIStreamWithoutUsage,
		},
		{
			name:   "deepseek stream without usage",
			model:  providers.Model{Provider: "deepseek", Endpoint: providers.OpenAIEndpoint},
			stream: openAIStreamWithoutUsage,
		},
		{
			name:   "anthropic stream without usage",
			model:  providers.Model{Provider: "anthropic", Endpoint: providers.AnthropicEndpoint},
			stream: "event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n",
		},
		{
			name:   "gemini stream without usage",
			model:  geminiModel(),
			stream: "event: interaction.created\ndata: {\"interaction\":{\"id\":\"x\"}}\n\n",
		},

		{
			name:    "unknown provider returns an error",
			model:   providers.Model{Id: "x", Provider: "newprovider", Endpoint: providers.OpenAIEndpoint},
			stream:  "data: {}\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStreamUsage(tt.model, []byte(tt.stream))
			if (err != nil) != tt.wantErr {
				t.Fatalf("got error %v, want error: %v", err, tt.wantErr)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScanOpenAIStreamUsage(t *testing.T) {
	data := func(payload string) string {
		return "data: " + payload + "\n\n"
	}

	const textChunk = `{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`
	const finalChunk = `{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,` +
		`"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":10}}}`
	finalWant := &OpenAIUsageInfo{
		PromptTokens:        100,
		CompletionTokens:    20,
		PromptTokensDetails: OpenAIUsageTokenDetails{CachedTokens: 30, CacheWriteTokens: 10},
	}

	tests := []struct {
		name   string
		stream string
		want   *OpenAIUsageInfo
	}{
		{
			name:   "extracts usage from the final chunk",
			stream: data(textChunk) + data(textChunk) + data(finalChunk) + data("[DONE]"),
			want:   finalWant,
		},
		{
			name: "extracts the deepseek cache hit and miss fields",
			stream: data(textChunk) +
				data(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":70,"prompt_cache_miss_tokens":30}}`) +
				data("[DONE]"),
			want: &OpenAIUsageInfo{
				PromptTokens:          100,
				CompletionTokens:      20,
				PromptCacheHitTokens:  new(70),
				PromptCacheMissTokens: new(30),
			},
		},
		{
			name: "usage comes from the last chunk that carries it",
			stream: data(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`) +
				data(finalChunk) +
				data("[DONE]"),
			want: finalWant,
		},
		{
			name:   "accepts data lines without a space after the prefix",
			stream: "data:" + finalChunk + "\n\ndata:[DONE]\n\n",
			want:   finalWant,
		},
		{
			name:   "handles CRLF line endings",
			stream: "data: " + textChunk + "\r\n\r\ndata: " + finalChunk + "\r\n\r\ndata: [DONE]\r\n\r\n",
			want:   finalWant,
		},
		{
			name:   "ignores comments, event lines and invalid JSON",
			stream: ": keep-alive\n\nevent: message\n" + data(`{invalid`) + data(finalChunk) + data("[DONE]"),
			want:   finalWant,
		},
		{
			name:   "keeps the earlier usage when a later chunk is cut off",
			stream: data(finalChunk) + `data: {"usage":{"prompt_tok`,
			want:   finalWant,
		},
		{
			name:   "returns nil when usage is null in every chunk",
			stream: data(textChunk) + data(textChunk) + data("[DONE]"),
			want:   nil,
		},
		{
			name:   "returns nil when the stream only has the done marker",
			stream: data("[DONE]"),
			want:   nil,
		},
		{
			name:   "returns nil when bytes is empty",
			stream: "",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanOpenAIStreamUsage([]byte(tt.stream))
			// DeepEqual rather than !=, since the DeepSeek fields are pointers.
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

// usageCallsJSON renders the arguments a fake convert received. %+v would
// print the pointers' addresses instead of what they point to.
func usageCallsJSON(calls []*OpenAIUsageInfo) string {
	b, _ := json.Marshal(calls)
	return string(b)
}

// openAISchemaFormat takes the provider-specific conversion as a parameter,
// so these tests pass a fake convert that records its argument and returns a
// sentinel: that checks the wire-format handling (what gets handed to
// convert, and that its result comes back untouched) without depending on
// any real conversion, which have their own tests.
func TestOpenAISchemaFormatBody(t *testing.T) {
	sentinel := &UsageInfo{CompletionTokens: 7}

	tests := []struct {
		name      string
		body      string
		wantCalls []*OpenAIUsageInfo
		wantErr   bool
	}{
		{
			name: "passes the parsed usage to convert",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":10}}}`,
			wantCalls: []*OpenAIUsageInfo{{
				PromptTokens:        100,
				CompletionTokens:    20,
				PromptTokensDetails: OpenAIUsageTokenDetails{CachedTokens: 30, CacheWriteTokens: 10},
			}},
		},
		{
			name: "passes the deepseek cache fields to convert",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":70,"prompt_cache_miss_tokens":30}}`,
			wantCalls: []*OpenAIUsageInfo{{
				PromptTokens:          100,
				CompletionTokens:      20,
				PromptCacheHitTokens:  new(70),
				PromptCacheMissTokens: new(30),
			}},
		},
		{
			name:      "calls convert with nil when there is no usage",
			body:      `{"id":"c1"}`,
			wantCalls: []*OpenAIUsageInfo{nil},
		},
		{
			name:      "calls convert with nil when usage is null",
			body:      `{"usage":null}`,
			wantCalls: []*OpenAIUsageInfo{nil},
		},
		{
			name:    "returns an error without calling convert when the body is not valid JSON",
			body:    `{"usage":`,
			wantErr: true,
		},
		{
			name:    "returns an error without calling convert when a usage field has the wrong type",
			body:    `{"usage":{"prompt_tokens":"100"}}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []*OpenAIUsageInfo
			format := openAISchemaFormat(func(u *OpenAIUsageInfo) *UsageInfo {
				calls = append(calls, u)
				return sentinel
			})

			got, err := format.body([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("got error %v, want error: %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("convert called with %s, want %s", usageCallsJSON(calls), usageCallsJSON(tt.wantCalls))
			}
			if tt.wantErr {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}
			if got != sentinel {
				t.Fatalf("got: %+v\nwant convert's result: %+v", got, sentinel)
			}
		})
	}
}

func TestOpenAISchemaFormatStream(t *testing.T) {
	sentinel := &UsageInfo{CompletionTokens: 7}

	const textChunk = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}` + "\n\n"
	const finalChunk = `data: {"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,` +
		`"prompt_tokens_details":{"cached_tokens":30}}}` + "\n\n"

	tests := []struct {
		name      string
		stream    string
		wantCalls []*OpenAIUsageInfo
	}{
		{
			name:   "passes the scanned usage to convert",
			stream: textChunk + finalChunk + "data: [DONE]\n\n",
			wantCalls: []*OpenAIUsageInfo{{
				PromptTokens:        100,
				CompletionTokens:    20,
				PromptTokensDetails: OpenAIUsageTokenDetails{CachedTokens: 30},
			}},
		},
		{
			name:      "calls convert with nil when the stream carries no usage",
			stream:    textChunk + "data: [DONE]\n\n",
			wantCalls: []*OpenAIUsageInfo{nil},
		},
		{
			name:      "calls convert with nil when the stream is empty",
			stream:    "",
			wantCalls: []*OpenAIUsageInfo{nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []*OpenAIUsageInfo
			format := openAISchemaFormat(func(u *OpenAIUsageInfo) *UsageInfo {
				calls = append(calls, u)
				return sentinel
			})

			got := format.stream([]byte(tt.stream))
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("convert called with %s, want %s", usageCallsJSON(calls), usageCallsJSON(tt.wantCalls))
			}
			if got != sentinel {
				t.Fatalf("got: %+v\nwant convert's result: %+v", got, sentinel)
			}
		})
	}
}

func TestScanAnthropicStreamUsage(t *testing.T) {
	event := func(name, data string) string {
		return "event: " + name + "\ndata: " + data + "\n\n"
	}
	messageStart := func(usage string) string {
		return event("message_start", `{"type":"message_start","message":{"id":"msg_1","usage":`+usage+`}}`)
	}
	messageDelta := func(usage string) string {
		return event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":`+usage+`}`)
	}

	const startUsage = `{"input_tokens":25,"output_tokens":1}`
	const fullStartUsage = `{"input_tokens":25,"cache_creation_input_tokens":50,"cache_read_input_tokens":10,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":30},"output_tokens":1,"inference_geo":"us"}`

	tests := []struct {
		name   string
		stream string
		want   *AnthropicUsageInfo
	}{
		{
			name: "extracts usage from a full stream",
			stream: messageStart(fullStartUsage) +
				event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
				event("ping", `{"type":"ping"}`) +
				event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`) +
				event("content_block_stop", `{"type":"content_block_stop","index":0}`) +
				messageDelta(`{"output_tokens":150}`) +
				event("message_stop", `{"type":"message_stop"}`),
			want: &AnthropicUsageInfo{
				InputTokens:              25,
				OutputTokens:             150,
				CacheCreationInputTokens: 50,
				CacheReadInputTokens:     10,
				CacheCreation: AnthropicUsageCacheCreation{
					Ephemeral5mInputTokens: 20,
					Ephemeral1hInputTokens: 30,
				},
				InferenceGeo: "us",
			},
		},
		{
			name:   "output tokens come from the last message_delta",
			stream: messageStart(startUsage) + messageDelta(`{"output_tokens":10}`) + messageDelta(`{"output_tokens":42}`),
			want:   &AnthropicUsageInfo{InputTokens: 25, OutputTokens: 42},
		},
		{
			name:   "message_delta only updates the output tokens",
			stream: messageStart(startUsage) + messageDelta(`{"input_tokens":999,"cache_read_input_tokens":999,"output_tokens":42}`),
			want:   &AnthropicUsageInfo{InputTokens: 25, OutputTokens: 42},
		},
		{
			name:   "returns the message_start usage when there is no message_delta",
			stream: messageStart(startUsage),
			want:   &AnthropicUsageInfo{InputTokens: 25, OutputTokens: 1},
		},
		{
			name:   "keeps the message_start usage when message_delta is invalid JSON",
			stream: messageStart(startUsage) + event("message_delta", `{"usage":`),
			want:   &AnthropicUsageInfo{InputTokens: 25, OutputTokens: 1},
		},
		{
			name:   "returns nil when message_delta comes without message_start",
			stream: messageDelta(`{"output_tokens":42}`),
			want:   nil,
		},
		{
			name:   "returns nil when message_start has no usage",
			stream: event("message_start", `{"type":"message_start","message":{"id":"msg_1"}}`) + messageDelta(`{"output_tokens":42}`),
			want:   nil,
		},
		{
			name:   "returns nil when message_start is invalid JSON",
			stream: event("message_start", `{"message":{"usage":`) + messageDelta(`{"output_tokens":42}`),
			want:   nil,
		},
		{
			name: "ignores usage in events other than message_start and message_delta",
			stream: event("ping", `{"usage":{"input_tokens":5,"output_tokens":5}}`) +
				event("message_stop", `{"message":{"usage":{"input_tokens":5}}}`),
			want: nil,
		},
		{
			name:   "returns nil when bytes is empty",
			stream: "",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanAnthropicStreamUsage([]byte(tt.stream))
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

const anthropicFullBody = `{
	"id": "msg_1",
	"type": "message",
	"role": "assistant",
	"usage": {
		"input_tokens": 25,
		"output_tokens": 150,
		"cache_read_input_tokens": 10,
		"cache_creation_input_tokens": 50,
		"cache_creation": {"ephemeral_5m_input_tokens": 20, "ephemeral_1h_input_tokens": 30},
		"inference_geo": "us"
	}
}`

const anthropicFullStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":25,"cache_creation_input_tokens":50,"cache_read_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":30},"output_tokens":1,"inference_geo":"us"}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":150}}

event: message_stop
data: {"type":"message_stop"}
`

// anthropicFullWant is what anthropicFullBody and anthropicFullStream both
// convert to. The 5m and 1h breakdown is used, not the 50 total, and "us"
// applies the regional price multiplier.
var anthropicFullWant = &UsageInfo{
	CompletionTokens:             150,
	PromptCacheHitTokens:         10,
	PromptCacheMissTokens:        25,
	PromptCacheWriteLevel1Tokens: 20,
	PromptCacheWriteLevel2Tokens: 30,
	PriceMultiplier:              1.1,
}

func TestAnthropicSchemaFormatBody(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    *UsageInfo
		wantErr bool
	}{
		{
			name: "extracts and converts usage from a response body",
			body: anthropicFullBody,
			want: anthropicFullWant,
		},
		{
			name: "bills the cache creation total as 5m when there is no breakdown",
			body: `{"usage":{"input_tokens":25,"output_tokens":150,"cache_creation_input_tokens":50}}`,
			want: &UsageInfo{CompletionTokens: 150, PromptCacheMissTokens: 25, PromptCacheWriteLevel1Tokens: 50, PriceMultiplier: 1},
		},
		{
			name: "returns nil without an error when there is no usage",
			body: `{"type":"message"}`,
			want: nil,
		},
		{
			name: "returns nil without an error when usage is null",
			body: `{"usage":null}`,
			want: nil,
		},
		{
			name:    "returns an error when the body is not valid JSON",
			body:    `{"usage":`,
			wantErr: true,
		},
		{
			name:    "returns an error when a usage field has the wrong type",
			body:    `{"usage":{"input_tokens":"25","output_tokens":150}}`,
			wantErr: true,
		},
	}

	format := anthropicSchemaFormat()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := format.body([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("got error %v, want error: %v", err, tt.wantErr)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestAnthropicSchemaFormatStream(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   *UsageInfo
	}{
		{
			name:   "extracts and converts usage from a stream",
			stream: anthropicFullStream,
			want:   anthropicFullWant,
		},
		{
			name: "bills the cache creation total as 5m when there is no breakdown",
			stream: "event: message_start\n" + `data: {"message":{"usage":{"input_tokens":25,"cache_creation_input_tokens":50,"output_tokens":1}}}` + "\n\n" +
				"event: message_delta\n" + `data: {"usage":{"output_tokens":150}}` + "\n\n",
			want: &UsageInfo{CompletionTokens: 150, PromptCacheMissTokens: 25, PromptCacheWriteLevel1Tokens: 50, PriceMultiplier: 1},
		},
		{
			name:   "returns nil when there is no message_start",
			stream: "event: message_delta\n" + `data: {"usage":{"output_tokens":42}}` + "\n\n",
			want:   nil,
		},
		{
			name:   "returns nil when the stream carries no usage",
			stream: "event: ping\n" + `data: {"type":"ping"}` + "\n\n",
			want:   nil,
		},
		{
			name:   "returns nil when message_start is invalid JSON",
			stream: "event: message_start\n" + `data: {"message":{"usage":` + "\n\n",
			want:   nil,
		},
		{
			name:   "returns nil when the stream is empty",
			stream: "",
			want:   nil,
		},
	}

	format := anthropicSchemaFormat()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := format.stream([]byte(tt.stream))
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestScanGeminiStreamUsage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want *GeminiUsageInfo
	}{
		{
			name: "extracts usage from the interaction.completed event",
			data: []byte(`event: interaction.created
data: {"interaction":{"id":"x","object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.created"}

event: step.delta
data: {"index":1,"delta":{"text":"hi","type":"text"},"event_type":"step.delta"}

event: interaction.completed
data: {"interaction":{"id":"x","status":"completed","usage":{"total_tokens":119,"total_input_tokens":100,"total_cached_tokens":20,"total_output_tokens":14,"total_tool_use_tokens":5,"total_thought_tokens":0},"object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.completed"}

event: done
data: [DONE]
`),
			want: &GeminiUsageInfo{
				TotalInputTokens:   100,
				TotalCachedTokens:  20,
				TotalOutputTokens:  14,
				TotalToolUseTokens: 5,
			},
		},
		{
			name: "return nil when 'interaction.completed' is missing",
			data: []byte(`event: interaction.created
data: {"interaction":{"id":"x","object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.created"}

event: step.delta
data: {"index":1,"delta":{"text":"hi","type":"text"},"event_type":"step.delta"}

event: step.delta
data: {"index":1,"delta":{"text":"dave","type":"text"},"event_type":"step.delta"}

event: done
data: [DONE]
`),
			want: nil,
		},
		{
			name: "return nil when bytes is empty",
			data: []byte(""),
			want: nil,
		},
		{
			name: "return nil when usage cannot be unmarshalled due to type misalignment",
			data: []byte(`event: interaction.created
data: {"interaction":{"id":"x","object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.created"}

event: step.delta
data: {"index":1,"delta":{"text":"hi","type":"text"},"event_type":"step.delta"}

event: interaction.completed
data: {"interaction":{"id":"x","status":"completed","usage":{"total_tokens":119,"total_input_tokens":"100","total_cached_tokens":20,"total_output_tokens":14,"total_tool_use_tokens":5,"total_thought_tokens":0},"object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.completed"}

event: done
data: [DONE]
`),
			want: nil,
		},
		{
			name: "return nil when usage cannot be unmarshalled due to invalid JSON",
			data: []byte(`event: interaction.created
data: {"interaction":{"id":"x","object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.created"}

event: step.delta
data: {"index":1,"delta":{"text":"hi","type":"text"},"event_type":"step.delta"}

event: interaction.completed
data: {"interaction:{"id":"x","status":"completed","usage":{"total_tokens":119,"total_input_tokens":"100","total_cached_tokens":20,"total_output_tokens":14,"total_tool_use_tokens":5,"total_thought_tokens":0},"object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.completed"}

event: done
data: [DONE]
`),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanGeminiStreamUsage(tt.data)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

// geminiUsageFields is a complete usage object: 100 input of which 20 cached,
// 14 output, 3 thought and 5 tool use tokens. As UsageInfo that's 22
// completion tokens (14+3+5), 20 cache hits and 80 cache misses.
const geminiUsageFields = `{"total_input_tokens":100,"total_cached_tokens":20,"total_output_tokens":14,"total_thought_tokens":3,"total_tool_use_tokens":5}`

var geminiUsageFieldsWant = &UsageInfo{
	CompletionTokens:      22,
	PromptCacheHitTokens:  20,
	PromptCacheMissTokens: 80,
	PriceMultiplier:       1,
}

func TestGeminiSchemaFormatBody(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    *UsageInfo
		wantErr bool
	}{
		{
			name: "extracts usage from a response body",
			body: geminiNonStreamBody,
			want: &UsageInfo{CompletionTokens: 14, PromptCacheMissTokens: 5, PriceMultiplier: 1},
		},
		{
			name: "converts the usage fields to billable tokens",
			body: `{"usage":` + geminiUsageFields + `}`,
			want: geminiUsageFieldsWant,
		},
		{
			name: "returns nil without an error when there is no usage",
			body: `{"status":"completed"}`,
			want: nil,
		},
		{
			name: "returns nil without an error when usage is null",
			body: `{"usage":null}`,
			want: nil,
		},
		{
			name:    "returns an error when the body is not valid JSON",
			body:    `{"usage":`,
			wantErr: true,
		},
		{
			name:    "returns an error when a usage field is missing",
			body:    `{"usage":{"total_input_tokens":100,"total_cached_tokens":20,"total_output_tokens":14,"total_tool_use_tokens":5}}`,
			wantErr: true,
		},
		{
			name:    "returns an error when a usage field has the wrong type",
			body:    `{"usage":{"total_input_tokens":"100","total_cached_tokens":20,"total_output_tokens":14,"total_thought_tokens":3,"total_tool_use_tokens":5}}`,
			wantErr: true,
		},
	}

	format := geminiSchemaFormat()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := format.body([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("got error %v, want error: %v", err, tt.wantErr)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestGeminiSchemaFormatStream(t *testing.T) {
	completed := func(usage string) string {
		return "event: interaction.completed\n" +
			`data: {"interaction":{"id":"x","status":"completed","usage":` + usage + `},"event_type":"interaction.completed"}` + "\n\n"
	}

	tests := []struct {
		name   string
		stream string
		want   *UsageInfo
	}{
		{
			name:   "extracts usage from a stream",
			stream: geminiStream,
			want:   &UsageInfo{CompletionTokens: 14, PromptCacheMissTokens: 5, PriceMultiplier: 1},
		},
		{
			name:   "converts the usage fields to billable tokens",
			stream: completed(geminiUsageFields),
			want:   geminiUsageFieldsWant,
		},
		{
			name:   "returns nil when there is no interaction.completed event",
			stream: "event: interaction.created\n" + `data: {"interaction":{"id":"x"},"event_type":"interaction.created"}` + "\n\n",
			want:   nil,
		},
		{
			name:   "returns nil when the stream is empty",
			stream: "",
			want:   nil,
		},
		{
			name:   "returns nil when the completed event has no usage",
			stream: completed(`null`),
			want:   nil,
		},
		{
			name:   "returns nil when a usage field is missing",
			stream: completed(`{"total_input_tokens":100,"total_cached_tokens":20,"total_output_tokens":14,"total_tool_use_tokens":5}`),
			want:   nil,
		},
	}

	format := geminiSchemaFormat()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := format.stream([]byte(tt.stream))
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *got != *tt.want {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestOpenAIToUsageInfo(t *testing.T) {
	tests := []struct {
		name      string
		usageInfo *OpenAIUsageInfo
		want      *UsageInfo
	}{
		{
			name:      "nil usage returns nil",
			usageInfo: nil,
			want:      nil,
		},
		{
			name:      "price multiplier is always set to 1",
			usageInfo: &OpenAIUsageInfo{},
			want: &UsageInfo{
				PriceMultiplier: 1,
			},
		},
		{
			name: "completion tokens are set",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens: 100,
			},
			want: &UsageInfo{
				CompletionTokens: 100,
				PriceMultiplier:  1,
			},
		},
		{
			name: "all prompt tokens are billed as cache miss when nothing is cached",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens: 100,
				PromptTokens:     200,
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheMissTokens: 200,
				PriceMultiplier:       1,
			},
		},
		{
			name: "cached tokens are billed as cache hit and subtracted from cache miss",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens: 100,
				PromptTokens:     200,
				PromptTokensDetails: OpenAIUsageTokenDetails{
					CachedTokens: 50,
				},
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheHitTokens:  50,
				PromptCacheMissTokens: 150,
				PriceMultiplier:       1,
			},
		},
		{
			name: "cache write tokens are billed as cache level 1 and subtracted from cache miss",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens: 100,
				PromptTokens:     200,
				PromptTokensDetails: OpenAIUsageTokenDetails{
					CachedTokens:     50,
					CacheWriteTokens: 30,
				},
			},
			want: &UsageInfo{
				CompletionTokens:             100,
				PromptCacheHitTokens:         50,
				PromptCacheMissTokens:        120,
				PromptCacheWriteLevel1Tokens: 30,
				PriceMultiplier:              1,
			},
		},
		{
			name: "deepseek cache fields are ignored",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens:      100,
				PromptTokens:          200,
				PromptCacheHitTokens:  new(50),
				PromptCacheMissTokens: new(150),
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheMissTokens: 200,
				PriceMultiplier:       1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usageInfo.toOpenAIUsageInfo()
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *tt.want != *got {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestDeepSeekToUsageInfo(t *testing.T) {
	tests := []struct {
		name      string
		usageInfo *OpenAIUsageInfo
		want      *UsageInfo
	}{
		{
			name:      "nil usage returns nil",
			usageInfo: nil,
			want:      nil,
		},
		{
			name:      "price multiplier is always set to 1",
			usageInfo: &OpenAIUsageInfo{},
			want: &UsageInfo{
				PriceMultiplier: 1,
			},
		},
		{
			name: "completion tokens are set",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens: 100,
			},
			want: &UsageInfo{
				CompletionTokens: 100,
				PriceMultiplier:  1,
			},
		},
		{
			name: "prompt cache hit tokens are set and cache miss tokens are also set implicitly",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens:     100,
				PromptTokens:         100,
				PromptCacheHitTokens: new(50),
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheHitTokens:  50,
				PromptCacheMissTokens: 50,
				PriceMultiplier:       1,
			},
		},
		{
			name: "prompt cache hit tokens are set and cache miss tokens are also set explicitly",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens:      100,
				PromptTokens:          90,
				PromptCacheHitTokens:  new(50),
				PromptCacheMissTokens: new(40),
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheHitTokens:  50,
				PromptCacheMissTokens: 40,
				PriceMultiplier:       1,
			},
		},
		{
			name: "all prompt tokens are billed as cache miss when cache fields are missing",
			usageInfo: &OpenAIUsageInfo{
				CompletionTokens: 100,
				PromptTokens:     200,
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheMissTokens: 200,
				PriceMultiplier:       1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usageInfo.toDeepSeekUsageInfo()
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *tt.want != *got {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestAnthropicToUsageInfo(t *testing.T) {
	tests := []struct {
		name      string
		usageInfo *AnthropicUsageInfo
		want      *UsageInfo
	}{
		{
			name:      "nil usage returns nil",
			usageInfo: nil,
			want:      nil,
		},
		{
			name: "regional multiplier is set to 1.1 for us",
			usageInfo: &AnthropicUsageInfo{
				InferenceGeo: "us",
			},
			want: &UsageInfo{
				PriceMultiplier: 1.1,
			},
		},
		{
			name: "regional multiplier is set to 1 for non us",
			usageInfo: &AnthropicUsageInfo{
				InferenceGeo: "eu",
			},
			want: &UsageInfo{
				PriceMultiplier: 1,
			},
		},
		{
			name:      "regional multiplier is set to 1 for empty inference geo",
			usageInfo: &AnthropicUsageInfo{},
			want: &UsageInfo{
				PriceMultiplier: 1,
			},
		},
		{
			name: "completion, cache miss and cache hit tokens are set",
			usageInfo: &AnthropicUsageInfo{
				OutputTokens:         100,
				InputTokens:          100,
				CacheReadInputTokens: 10,
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheMissTokens: 100,
				PromptCacheHitTokens:  10,
				PriceMultiplier:       1,
			},
		},
		{
			name: "regional multiplier does not affect the rest of the values",
			usageInfo: &AnthropicUsageInfo{
				OutputTokens:         100,
				InputTokens:          100,
				CacheReadInputTokens: 10,
				InferenceGeo:         "us",
			},
			want: &UsageInfo{
				CompletionTokens:      100,
				PromptCacheMissTokens: 100,
				PromptCacheHitTokens:  10,
				PriceMultiplier:       1.1,
			},
		},
		{
			name: "sets as cache level 1 when no ephemeral input tokens are provided but cache is created",
			usageInfo: &AnthropicUsageInfo{
				OutputTokens:             100,
				InputTokens:              100,
				CacheReadInputTokens:     10,
				CacheCreationInputTokens: 50,
			},
			want: &UsageInfo{
				CompletionTokens:             100,
				PromptCacheMissTokens:        100,
				PromptCacheHitTokens:         10,
				PromptCacheWriteLevel1Tokens: 50,
				PriceMultiplier:              1,
			},
		},
		{
			name: "sets as cache level 1 ephemeral 5m input tokens",
			usageInfo: &AnthropicUsageInfo{
				OutputTokens:         100,
				InputTokens:          100,
				CacheReadInputTokens: 10,
				CacheCreation: AnthropicUsageCacheCreation{
					Ephemeral5mInputTokens: 20,
				},
			},
			want: &UsageInfo{
				CompletionTokens:             100,
				PromptCacheMissTokens:        100,
				PromptCacheHitTokens:         10,
				PromptCacheWriteLevel1Tokens: 20,
				PriceMultiplier:              1,
			},
		},
		{
			name: "sets as cache level 2 ephemeral 1h input tokens",
			usageInfo: &AnthropicUsageInfo{
				OutputTokens:         100,
				InputTokens:          100,
				CacheReadInputTokens: 10,
				CacheCreation: AnthropicUsageCacheCreation{
					Ephemeral1hInputTokens: 30,
				},
			},
			want: &UsageInfo{
				CompletionTokens:             100,
				PromptCacheMissTokens:        100,
				PromptCacheHitTokens:         10,
				PromptCacheWriteLevel2Tokens: 30,
				PriceMultiplier:              1,
			},
		},
		{
			name: "sets as cache both level 1 and 2  based on ephemeral 5m and 1h input tokens",
			usageInfo: &AnthropicUsageInfo{
				OutputTokens:         100,
				InputTokens:          100,
				CacheReadInputTokens: 10,
				CacheCreation: AnthropicUsageCacheCreation{
					Ephemeral5mInputTokens: 40,
					Ephemeral1hInputTokens: 30,
				},
			},
			want: &UsageInfo{
				CompletionTokens:             100,
				PromptCacheMissTokens:        100,
				PromptCacheHitTokens:         10,
				PromptCacheWriteLevel1Tokens: 40,
				PromptCacheWriteLevel2Tokens: 30,
				PriceMultiplier:              1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usageInfo.toUsageInfo()
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}

			if got == nil || *tt.want != *got {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestGeminiToUsageInfo(t *testing.T) {
	tests := []struct {
		name      string
		usageInfo *GeminiUsageInfo
		want      *UsageInfo
	}{
		{
			name: "gemini takes into account thought tokens",
			usageInfo: &GeminiUsageInfo{
				TotalInputTokens:   100,
				TotalCachedTokens:  20,
				TotalOutputTokens:  200,
				TotalThoughtTokens: 50,
				TotalToolUseTokens: 15,
			},
			want: &UsageInfo{
				CompletionTokens:             265,
				PromptCacheHitTokens:         20,
				PromptCacheMissTokens:        80,
				PromptCacheWriteLevel1Tokens: 0,
				PromptCacheWriteLevel2Tokens: 0,
				PriceMultiplier:              1,
			},
		},
		{
			name: "no cached tokens bills all input as cache miss",
			usageInfo: &GeminiUsageInfo{
				TotalInputTokens:  100,
				TotalOutputTokens: 40,
			},
			want: &UsageInfo{
				CompletionTokens:      40,
				PromptCacheMissTokens: 100,
				PriceMultiplier:       1,
			},
		},
		{
			name: "fully cached input leaves no cache miss",
			usageInfo: &GeminiUsageInfo{
				TotalInputTokens:  100,
				TotalCachedTokens: 100,
				TotalOutputTokens: 40,
			},
			want: &UsageInfo{
				CompletionTokens:     40,
				PromptCacheHitTokens: 100,
				PriceMultiplier:      1,
			},
		},
		{
			name: "thought and tool use tokens count as output without output text",
			usageInfo: &GeminiUsageInfo{
				TotalInputTokens:   100,
				TotalThoughtTokens: 30,
				TotalToolUseTokens: 10,
			},
			want: &UsageInfo{
				CompletionTokens:      40,
				PromptCacheMissTokens: 100,
				PriceMultiplier:       1,
			},
		},
		{
			name:      "nil usage returns nil",
			usageInfo: nil,
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usageInfo.toUsageInfo()
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got: %+v\nwant: nil", got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}
