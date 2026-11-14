package providers

import (
	"fmt"
	"net/url"
	"strings"
)

var Models = map[string]Model{}

func Register(id string, model Model) {
	if _, exists := Models[id]; exists {
		panic(fmt.Sprintf("providers: duplicate model id %q", id))
	}

	Models[id] = model
}

// Reset empties the registry.
func Reset() {
	Models = map[string]Model{}
}

// RegisterCatalog registers a provider's models, each reached at baseURL.
// Nothing is registered when baseURL can't have an endpoint path joined on.
func RegisterCatalog(models map[string]Model, baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || strings.HasSuffix(baseURL, "/") {
		return fmt.Errorf("invalid base URL %q: want http(s)://host[/path], with no trailing slash", baseURL)
	}

	for id, model := range models {
		model.BaseUrl = baseURL
		Register(id, model)
	}
	return nil
}
