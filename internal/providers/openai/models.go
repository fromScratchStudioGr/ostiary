package openai

import (
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

// longContextThreshold is the input size above which a request is priced at
// a model's long-context rates ("Long context: >272K input tokens").
const longContextThreshold = 272_000

var OpenAIModels = map[string]providers.Model{
	"gpt-6.1-sol": {
		Id:                   "gpt-6.1-sol",
		Name:                 "GPT-6.1 Sol",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:            2.00,
			Output:           10.00,
			CacheRead:        providers.Price(0.10),
			CacheWriteLevel1: providers.Price(2.50),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:            4.00,
					Output:           15.00,
					CacheRead:        providers.Price(0.20),
					CacheWriteLevel1: providers.Price(5.00),
				},
			},
		},
	},
	"gpt-6-astra": {
		Id:                   "gpt-6-astra",
		Name:                 "GPT-6 Astra",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:            10.00,
			Output:           50.00,
			CacheRead:        providers.Price(1.00),
			CacheWriteLevel1: providers.Price(12.50),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:            20.00,
					Output:           75.00,
					CacheRead:        providers.Price(2.00),
					CacheWriteLevel1: providers.Price(25.00),
				},
			},
		},
	},
	"gpt-6-sol": {
		Id:                   "gpt-6-sol",
		Name:                 "GPT-6 Sol",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:            2.00,
			Output:           10.00,
			CacheRead:        providers.Price(0.20),
			CacheWriteLevel1: providers.Price(2.50),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:            4.00,
					Output:           15.00,
					CacheRead:        providers.Price(0.40),
					CacheWriteLevel1: providers.Price(5.00),
				},
			},
		},
	},
	"gpt-6-luna": {
		Id:                   "gpt-6-luna",
		Name:                 "GPT-6 Luna",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:            0.10,
			Output:           0.50,
			CacheRead:        providers.Price(0.01),
			CacheWriteLevel1: providers.Price(0.125),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:            0.20,
					Output:           0.75,
					CacheRead:        providers.Price(0.02),
					CacheWriteLevel1: providers.Price(0.25),
				},
			},
		},
	},
	"gpt-5.6-sol": {
		Id:                   "gpt-5.6-sol",
		Name:                 "GPT-5.6 Sol",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:            4.00,
			Output:           20.00,
			CacheRead:        providers.Price(0.40),
			CacheWriteLevel1: providers.Price(5.00),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:            8.00,
					Output:           30.00,
					CacheRead:        providers.Price(0.80),
					CacheWriteLevel1: providers.Price(10.00),
				},
			},
		},
	},
	"gpt-5.6-terra": {
		Id:                   "gpt-5.6-terra",
		Name:                 "GPT-5.6 Terra",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			// Short-context pricing (per official pricing page).
			Input:            2.00,
			Output:           12.00,
			CacheRead:        providers.Price(0.20),
			CacheWriteLevel1: providers.Price(2.50),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:            4.00,
					Output:           18.00,
					CacheRead:        providers.Price(0.40),
					CacheWriteLevel1: providers.Price(5.00),
				},
			},
		},
	},
	"gpt-5.6-luna": {
		Id:                   "gpt-5.6-luna",
		Name:                 "GPT-5.6 Luna",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			// Short-context pricing (per official pricing page).
			Input:            0.20,
			Output:           1.20,
			CacheRead:        providers.Price(0.02),
			CacheWriteLevel1: providers.Price(0.25),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:            0.40,
					Output:           1.80,
					CacheRead:        providers.Price(0.04),
					CacheWriteLevel1: providers.Price(0.50),
				},
			},
		},
	},
	"gpt-5.5": {
		Id:                   "gpt-5.5",
		Name:                 "GPT-5.5",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:     5.00,
			Output:    30.00,
			CacheRead: providers.Price(0.50),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:     10.00,
					Output:    45.00,
					CacheRead: providers.Price(1.00),
				},
			},
		},
	},
	"gpt-5.4": {
		Id:                   "gpt-5.4",
		Name:                 "GPT-5.4",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:     2.50,
			Output:    15.00,
			CacheRead: providers.Price(0.25),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:     5.00,
					Output:    22.50,
					CacheRead: providers.Price(0.50),
				},
			},
		},
	},
	"gpt-5.4-mini": {
		Id:                   "gpt-5.4-mini",
		Name:                 "GPT-5.4 Mini",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			Input:     0.75,
			Output:    4.50,
			CacheRead: providers.Price(0.075),
		},
	},
	"gpt-5.4-nano": {
		Id:                   "gpt-5.4-nano",
		Name:                 "GPT-5.4 Nano",
		Provider:             "openai",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		Cost: providers.Cost{
			// Short-context pricing (per official pricing page); the page
			// lists no long-context tier for this model.
			Input:     0.20,
			Output:    1.25,
			CacheRead: providers.Price(0.02),
			// No cache-write tiers for pre-5.6 models.
		},
	},
}
