package providers_test

import (
	"regexp"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/providers"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/anthropic"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/deepseek"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/gemini"
	"github.com/fromScratchStudioGr/ostiary/internal/providers/openai"
)

var catalogs = []struct {
	provider string
	models   map[string]providers.Model
}{
	{"anthropic", anthropic.AnthropicModels},
	{"deepseek", deepseek.DeepSeekModels},
	{"gemini", gemini.GeminiModels},
	{"openai", openai.OpenAIModels},
}

func TestCatalogs(t *testing.T) {
	seen := map[string]string{}
	for _, c := range catalogs {
		for id, model := range c.models {
			if model.Provider != c.provider {
				t.Errorf("%s says its provider is %q, but it's in the %s catalog", id, model.Provider, c.provider)
			}
			// The base URL comes from the provider's configuration when the
			// catalog is registered, so one written here would be ignored.
			if model.BaseUrl != "" {
				t.Errorf("%s has base URL %q, want none", id, model.BaseUrl)
			}
			// Registering both catalogs would panic on the duplicate.
			if other, ok := seen[id]; ok {
				t.Errorf("%s is in both the %s and the %s catalog", id, other, c.provider)
			}
			seen[id] = c.provider
		}
	}
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

func TestModelDefinitions(t *testing.T) {
	endpoints := []providers.EndpointType{providers.OpenAIEndpoint, providers.AnthropicEndpoint, providers.InteractionsEndpoint}
	headers := []providers.AuthenticationHeaderType{providers.Bearer, providers.XAPIKey, providers.GoogAPIKey}

	models := map[string]providers.Model{}
	for _, c := range catalogs {
		for id, model := range c.models {
			models[id] = model
		}
	}
	for id, model := range models {
		t.Run(id, func(t *testing.T) {
			if model.Id != id {
				t.Errorf("listed as %q but its Id is %q", id, model.Id)
			}
			// The id goes into requests and URLs.
			if !idPattern.MatchString(id) {
				t.Errorf("id %q isn't lowercase letters, digits, dots and dashes", id)
			}
			if model.Name == "" {
				t.Error("no name")
			}
			if model.Provider == "" {
				t.Error("no provider")
			}
			if !slicesContains(endpoints, model.Endpoint) {
				t.Errorf("endpoint %q isn't a known one", model.Endpoint)
			}
			if !slicesContains(headers, model.AuthenticationHeader) {
				t.Errorf("authentication header %q isn't a known one", model.AuthenticationHeader)
			}
			checkCost(t, model.Cost, false)
			// Every provider reports cached input tokens, and Anthropic reports
			// both kinds of cache write: tokens of a tier with no price are
			// billed at 0.
			if model.Cost.CacheRead == nil {
				t.Error("no cache read price, so cached tokens would be billed at 0")
			}
			if model.Provider == "anthropic" && (model.Cost.CacheWriteLevel1 == nil || model.Cost.CacheWriteLevel2 == nil) {
				t.Error("a cache write price is missing, so those cache writes would be billed at 0")
			}
		})
	}
}

func slicesContains[T comparable](values []T, v T) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// checkCost checks a price list for the mistakes a hand-edited table is prone
// to: a misplaced decimal point, or a long-context tier that forgets a price.
func checkCost(t *testing.T, c providers.Cost, isLongContext bool) {
	t.Helper()
	if c.Input <= 0 {
		t.Errorf("input price = %v, want it positive", c.Input)
	}
	if c.Output < c.Input {
		t.Errorf("output price %v is below input price %v", c.Output, c.Input)
	}
	for name, price := range map[string]*float32{
		"cache read":          c.CacheRead,
		"cache write level 1": c.CacheWriteLevel1,
		"cache write level 2": c.CacheWriteLevel2,
	} {
		if price != nil && *price <= 0 {
			t.Errorf("%s price = %v, want it positive", name, *price)
		}
	}
	if c.CacheRead != nil && *c.CacheRead >= c.Input {
		t.Errorf("cache read price %v isn't below input price %v", *c.CacheRead, c.Input)
	}
	if c.CacheWriteLevel1 != nil && c.CacheWriteLevel2 != nil && *c.CacheWriteLevel2 < *c.CacheWriteLevel1 {
		t.Errorf("level 2 cache write price %v is below level 1's %v", *c.CacheWriteLevel2, *c.CacheWriteLevel1)
	}

	if c.LongContext == nil {
		return
	}
	if isLongContext {
		t.Error("a long-context tier has a long-context tier of its own")
		return
	}
	long := c.LongContext
	if long.ThresholdTokens <= 0 {
		t.Errorf("long-context threshold = %d, want it positive", long.ThresholdTokens)
	}
	checkCost(t, long.Cost, true)
	if long.Cost.Input < c.Input || long.Cost.Output < c.Output {
		t.Errorf("long-context prices %v/%v are below the base prices %v/%v", long.Cost.Input, long.Cost.Output, c.Input, c.Output)
	}
	// The long-context prices replace the base ones as a whole, so a price
	// they leave out would stop being charged above the threshold.
	for name, pair := range map[string][2]*float32{
		"cache read":          {c.CacheRead, long.Cost.CacheRead},
		"cache write level 1": {c.CacheWriteLevel1, long.Cost.CacheWriteLevel1},
		"cache write level 2": {c.CacheWriteLevel2, long.Cost.CacheWriteLevel2},
	} {
		if pair[0] != nil && pair[1] == nil {
			t.Errorf("the long-context tier has no %s price, but the base prices do", name)
		}
		if pair[0] != nil && pair[1] != nil && *pair[1] < *pair[0] {
			t.Errorf("long-context %s price %v is below the base price %v", name, *pair[1], *pair[0])
		}
	}
}
