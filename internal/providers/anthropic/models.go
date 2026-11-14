package anthropic

import (
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

var AnthropicModels = map[string]providers.Model{
	"claude-fable-5-1": {
		Id:                   "claude-fable-5-1",
		Name:                 "Claude Fable 5.1",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            10.00,
			Output:           50.00,
			CacheRead:        providers.Price(0.25),
			CacheWriteLevel1: providers.Price(12.50), // 5m cache writes
			CacheWriteLevel2: providers.Price(20.00), // 1h cache writes
		},
	},
	"claude-opus-5-5": {
		Id:                   "claude-opus-5-5",
		Name:                 "Claude Opus 5.5",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            4.00,
			Output:           20.00,
			CacheRead:        providers.Price(0.20),
			CacheWriteLevel1: providers.Price(5.00), // 5m cache writes
			CacheWriteLevel2: providers.Price(8.00), // 1h cache writes
		},
	},
	"claude-sonnet-5-5": {
		Id:                   "claude-sonnet-5-5",
		Name:                 "Claude Sonnet 5.5",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            2.00,
			Output:           10.00,
			CacheRead:        providers.Price(0.20),
			CacheWriteLevel1: providers.Price(2.50), // 5m cache writes
			CacheWriteLevel2: providers.Price(4.00), // 1h cache writes
		},
	},
	"claude-fable-5": {
		Id:                   "claude-fable-5",
		Name:                 "Claude Fable 5",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            10.00,
			Output:           50.00,
			CacheRead:        providers.Price(1.00),
			CacheWriteLevel1: providers.Price(12.50), // 5m cache writes
			CacheWriteLevel2: providers.Price(20.00), // 1h cache writes
		},
	},
	"claude-opus-5": {
		Id:                   "claude-opus-5",
		Name:                 "Claude Opus 5",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            5.00,
			Output:           25.00,
			CacheRead:        providers.Price(0.50),
			CacheWriteLevel1: providers.Price(6.25),  // 5m cache writes
			CacheWriteLevel2: providers.Price(10.00), // 1h cache writes
		},
	},
	"claude-sonnet-5": {
		Id:                   "claude-sonnet-5",
		Name:                 "Claude Sonnet 5",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            2.00,
			Output:           10.00,
			CacheRead:        providers.Price(0.20),
			CacheWriteLevel1: providers.Price(2.50), // 5m cache writes
			CacheWriteLevel2: providers.Price(4.00), // 1h cache writes
		},
	},
	"claude-opus-4-8": {
		Id:                   "claude-opus-4-8",
		Name:                 "Claude Opus 4.8",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            5.00,
			Output:           25.00,
			CacheRead:        providers.Price(0.50),
			CacheWriteLevel1: providers.Price(6.25),  // 5m cache writes
			CacheWriteLevel2: providers.Price(10.00), // 1h cache writes
		},
	},
	"claude-opus-4-7": {
		Id:                   "claude-opus-4-7",
		Name:                 "Claude Opus 4.7",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            5.00,
			Output:           25.00,
			CacheRead:        providers.Price(0.50),
			CacheWriteLevel1: providers.Price(6.25),  // 5m cache writes
			CacheWriteLevel2: providers.Price(10.00), // 1h cache writes
		},
	},
	"claude-opus-4-6": {
		Id:                   "claude-opus-4-6",
		Name:                 "Claude Opus 4.6",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            5.00,
			Output:           25.00,
			CacheRead:        providers.Price(0.50),
			CacheWriteLevel1: providers.Price(6.25),  // 5m cache writes
			CacheWriteLevel2: providers.Price(10.00), // 1h cache writes
		},
	},
	"claude-sonnet-4-6": {
		Id:                   "claude-sonnet-4-6",
		Name:                 "Claude Sonnet 4.6",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            3.00,
			Output:           15.00,
			CacheRead:        providers.Price(0.30),
			CacheWriteLevel1: providers.Price(3.75), // 5m cache writes
			CacheWriteLevel2: providers.Price(6.00), // 1h cache writes
		},
	},
	"claude-opus-4-5": {
		Id:                   "claude-opus-4-5",
		Name:                 "Claude Opus 4.5",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            5.00,
			Output:           25.00,
			CacheRead:        providers.Price(0.50),
			CacheWriteLevel1: providers.Price(6.25),  // 5m cache writes
			CacheWriteLevel2: providers.Price(10.00), // 1h cache writes
		},
	},
	"claude-haiku-4-5": {
		Id:                   "claude-haiku-4-5",
		Name:                 "Claude Haiku 4.5",
		Provider:             "anthropic",
		Endpoint:             providers.AnthropicEndpoint,
		AuthenticationHeader: providers.XAPIKey,
		Cost: providers.Cost{
			Input:            1.00,
			Output:           5.00,
			CacheRead:        providers.Price(0.10),
			CacheWriteLevel1: providers.Price(1.25), // 5m cache writes
			CacheWriteLevel2: providers.Price(2.00), // 1h cache writes
		},
	},
}
