package providers

type EndpointType string

const (
	OpenAIEndpoint       EndpointType = "/chat/completions"
	AnthropicEndpoint    EndpointType = "/v1/messages"
	InteractionsEndpoint EndpointType = "/interactions"
)

type AuthenticationHeaderType string

const (
	Bearer     AuthenticationHeaderType = "Bearer"
	XAPIKey    AuthenticationHeaderType = "XAPIKey"
	GoogAPIKey AuthenticationHeaderType = "GoogAPIKey"
)

type Model struct {
	Id       string
	Name     string
	Provider string
	// BaseUrl is set when the model is registered, from its provider's
	// configured base URL. Catalogs leave it empty.
	BaseUrl              string
	Endpoint             EndpointType
	AuthenticationHeader AuthenticationHeaderType
	Cost                 Cost
	// PeakPricing is optional: nil means prices don't vary by time.
	PeakPricing *PeakPricing
}

type Cost struct {
	Input            float32
	Output           float32
	CacheRead        *float32
	CacheWriteLevel1 *float32
	CacheWriteLevel2 *float32
	// LongContext is optional: nil means the prices don't depend on how long
	// the request's input is.
	LongContext *LongContextCost
}

// LongContextCost replaces a model's prices for the whole request once its
// input exceeds ThresholdTokens.
type LongContextCost struct {
	ThresholdTokens int
	Cost            Cost
}

// ForInputTokens returns the prices that apply to a request with the given
// total number of input tokens (cached and cache-write tokens included).
func (c Cost) ForInputTokens(inputTokens int) Cost {
	if c.LongContext != nil && inputTokens > c.LongContext.ThresholdTokens {
		return c.LongContext.Cost
	}
	return c
}

func Price(v float32) *float32 {
	return new(v)
}
