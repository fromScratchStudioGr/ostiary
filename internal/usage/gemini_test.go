package usage

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

const geminiNonStreamBody = `{
	"id": "v1_ChdwdmpBYXFxTUpZbms3TThQM011dXdBbxIXcHZqQWFxcU1KWW5rN004UDNNdXV3QW8",
	"status": "completed",
	"usage": {
		"total_tokens": 19,
		"total_input_tokens": 5,
		"input_tokens_by_modality": [{"modality": "text", "tokens": 5}],
		"total_cached_tokens": 0,
		"total_output_tokens": 14,
		"total_tool_use_tokens": 0,
		"total_thought_tokens": 0,
		"raw_prompt_token": 36
	},
	"object": "interaction",
	"model": "gemini-3.8-flash"
}`

const geminiStream = `event: interaction.created
data: {"interaction":{"id":"x","object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.created"}

event: interaction.status_update
data: {"interaction_id":"x","status":"in_progress","event_type":"interaction.status_update"}

event: step.start
data: {"index":0,"step":{"type":"thought"},"event_type":"step.start"}

event: step.stop
data: {"index":0,"event_type":"step.stop"}

event: step.start
data: {"index":1,"step":{"type":"model_output"},"event_type":"step.start"}

event: step.delta
data: {"index":1,"delta":{"text":"hi","type":"text"},"event_type":"step.delta"}

event: step.stop
data: {"index":1,"event_type":"step.stop"}

event: interaction.completed
data: {"interaction":{"id":"x","status":"completed","usage":{"total_tokens":19,"total_input_tokens":5,"total_cached_tokens":0,"total_output_tokens":14,"total_tool_use_tokens":0,"total_thought_tokens":0,"raw_prompt_token":36},"object":"interaction","model":"gemini-3.8-flash"},"event_type":"interaction.completed"}

event: done
data: [DONE]
`

func geminiModel() providers.Model {
	return providers.Model{Provider: "gemini", Endpoint: providers.InteractionsEndpoint, Id: "gemini-3.8-flash"}
}

func TestParseBodyUsageGemini(t *testing.T) {
	got, err := ParseBodyUsage(geminiModel(), []byte(geminiNonStreamBody))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := UsageInfo{CompletionTokens: 14, PromptCacheMissTokens: 5, PriceMultiplier: 1}
	if got == nil || *got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseStreamUsageGemini(t *testing.T) {
	got, err := ParseStreamUsage(geminiModel(), []byte(geminiStream))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := UsageInfo{CompletionTokens: 14, PromptCacheMissTokens: 5, PriceMultiplier: 1}
	if got == nil || *got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseStreamUsageGeminiIgnoresOtherEvents(t *testing.T) {
	// No interaction.completed event at all: the stream was cut short (or
	// errored) before usage ever arrived.
	stream := `event: interaction.created
data: {"interaction":{"id":"x"},"event_type":"interaction.created"}

event: step.start
data: {"index":0,"step":{"type":"model_output"},"event_type":"step.start"}
`
	got, err := ParseStreamUsage(geminiModel(), []byte(stream))
	if err != nil || got != nil {
		t.Fatalf("got (%+v, %v), want (nil, nil)", got, err)
	}
}

func TestGeminiUsageInfoRequiresAllFields(t *testing.T) {
	const allFields = `"total_input_tokens":100,"total_cached_tokens":20,"total_output_tokens":14,"total_thought_tokens":3,"total_tool_use_tokens":5`

	t.Run("all fields present", func(t *testing.T) {
		var got GeminiUsageInfo
		if err := json.Unmarshal([]byte(`{`+allFields+`}`), &got); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := GeminiUsageInfo{TotalInputTokens: 100, TotalCachedTokens: 20, TotalOutputTokens: 14, TotalThoughtTokens: 3, TotalToolUseTokens: 5}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("zero is a valid value", func(t *testing.T) {
		var got GeminiUsageInfo
		body := `{"total_input_tokens":0,"total_cached_tokens":0,"total_output_tokens":0,"total_thought_tokens":0,"total_tool_use_tokens":0}`
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unknown fields are ignored", func(t *testing.T) {
		var got GeminiUsageInfo
		body := `{"total_tokens":19,"raw_prompt_token":36,"input_tokens_by_modality":[],` + allFields + `}`
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	for _, field := range []string{
		"total_input_tokens",
		"total_cached_tokens",
		"total_output_tokens",
		"total_thought_tokens",
		"total_tool_use_tokens",
	} {
		t.Run("missing "+field, func(t *testing.T) {
			var got GeminiUsageInfo
			body := strings.Replace(`{`+allFields+`}`, `"`+field+`"`, `"misspelled"`, 1)
			err := json.Unmarshal([]byte(body), &got)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("got error %v, want one naming %s", err, field)
			}
		})

		t.Run("null "+field, func(t *testing.T) {
			var got GeminiUsageInfo
			body := strings.Replace(`{`+allFields+`}`, `"`+field+`":`, `"`+field+`":null,"ignored":`, 1)
			err := json.Unmarshal([]byte(body), &got)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("got error %v, want one naming %s", err, field)
			}
		})
	}

	t.Run("body without a required field returns an error", func(t *testing.T) {
		body := strings.Replace(geminiNonStreamBody, `"total_thought_tokens": 0`, `"total_thought_token": 0`, 1)
		got, err := ParseBodyUsage(geminiModel(), []byte(body))
		if err == nil || got != nil {
			t.Fatalf("got (%+v, %v), want an error and no usage", got, err)
		}
	})

	t.Run("stream without a required field returns no usage", func(t *testing.T) {
		stream := strings.Replace(geminiStream, `"total_cached_tokens":0`, `"total_cached_token":0`, 1)
		got, err := ParseStreamUsage(geminiModel(), []byte(stream))
		if err != nil || got != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil)", got, err)
		}
	})
}

func TestToUsageInfoGeminiCachedIsSubsetOfInput(t *testing.T) {
	u := &GeminiUsageInfo{TotalInputTokens: 100, TotalCachedTokens: 40, TotalOutputTokens: 10}
	got := u.toUsageInfo()
	if got.PromptCacheHitTokens != 40 || got.PromptCacheMissTokens != 60 {
		t.Fatalf("got hit=%d miss=%d, want hit=40 miss=60", got.PromptCacheHitTokens, got.PromptCacheMissTokens)
	}
}

func TestToUsageInfoGeminiThoughtAndToolUseAddToOutput(t *testing.T) {
	u := &GeminiUsageInfo{TotalOutputTokens: 10, TotalThoughtTokens: 5, TotalToolUseTokens: 3}
	if got := u.toUsageInfo().CompletionTokens; got != 18 {
		t.Fatalf("got %d, want 18 (10+5+3)", got)
	}
}
