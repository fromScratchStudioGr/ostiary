package deepseek

import (
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

var DeepSeekModels = map[string]providers.Model{
	"deepseek-flash": {
		Id:                   "deepseek-flash",
		Name:                 "DeepSeek Flash",
		Provider:             "deepseek",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		PeakPricing:          peakPricing,
		Cost: providers.Cost{
			Input:     0.15,
			Output:    0.6,
			CacheRead: providers.Price(0.003),
		},
	},
	"deepseek-v4-pro": {
		Id:                   "deepseek-v4-pro",
		Name:                 "DeepSeek v4 Pro",
		Provider:             "deepseek",
		Endpoint:             providers.OpenAIEndpoint,
		AuthenticationHeader: providers.Bearer,
		PeakPricing:          peakPricing,
		Cost: providers.Cost{
			Input:     0.66,
			Output:    1.98,
			CacheRead: providers.Price(0.022),
		},
	},
}
