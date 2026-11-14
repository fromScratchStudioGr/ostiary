package gemini

import (
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

// longContextThreshold is the prompt size above which the Pro models are
// priced at their higher rates ("prompts > 200k tokens").
const longContextThreshold = 200_000

// GeminiModels are served through the Interactions API, Google's recommended
// interface for all new integrations.
var GeminiModels = map[string]providers.Model{
	"gemini-3.8-flash": {
		Id:                   "gemini-3.8-flash",
		Name:                 "Gemini 3.8 Flash",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     0.75,
			Output:    3.75,
			CacheRead: providers.Price(0.075),
		},
	},
	"gemini-3.7-flash": {
		Id:                   "gemini-3.7-flash",
		Name:                 "Gemini 3.7 Flash",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     0.75,
			Output:    3.75,
			CacheRead: providers.Price(0.075),
		},
	},
	"gemini-3.6-flash": {
		Id:                   "gemini-3.6-flash",
		Name:                 "Gemini 3.6 Flash",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     0.75,
			Output:    3.75,
			CacheRead: providers.Price(0.075),
		},
	},
	"gemini-3.5-flash": {
		Id:                   "gemini-3.5-flash",
		Name:                 "Gemini 3.5 Flash",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     1.50,
			Output:    9.00,
			CacheRead: providers.Price(0.15),
		},
	},
	"gemini-3.5-flash-lite": {
		Id:                   "gemini-3.5-flash-lite",
		Name:                 "Gemini 3.5 Flash-Lite",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     0.30,
			Output:    2.50,
			CacheRead: providers.Price(0.03),
		},
	},
	"gemini-3.1-flash-lite": {
		Id:                   "gemini-3.1-flash-lite",
		Name:                 "Gemini 3.1 Flash-Lite",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     0.25,
			Output:    1.50,
			CacheRead: providers.Price(0.025),
		},
	},
	"gemini-2.5-pro": {
		Id:                   "gemini-2.5-pro",
		Name:                 "Gemini 2.5 Pro",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     1.25,
			Output:    10.00,
			CacheRead: providers.Price(0.125),
			LongContext: &providers.LongContextCost{
				ThresholdTokens: longContextThreshold,
				Cost: providers.Cost{
					Input:     2.50,
					Output:    15.00,
					CacheRead: providers.Price(0.25),
				},
			},
		},
	},
	"gemini-2.5-flash": {
		Id:                   "gemini-2.5-flash",
		Name:                 "Gemini 2.5 Flash",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     0.30,
			Output:    2.50,
			CacheRead: providers.Price(0.03),
		},
	},
	"gemini-2.5-flash-lite": {
		Id:                   "gemini-2.5-flash-lite",
		Name:                 "Gemini 2.5 Flash-Lite",
		Provider:             "gemini",
		Endpoint:             providers.InteractionsEndpoint,
		AuthenticationHeader: providers.GoogAPIKey,
		Cost: providers.Cost{
			Input:     0.10,
			Output:    0.40,
			CacheRead: providers.Price(0.01),
		},
	},
}
