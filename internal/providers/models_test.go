package providers

import "testing"

func TestRegisterCatalog(t *testing.T) {
	t.Run("registers every model, reached at the base URL", func(t *testing.T) {
		Reset()
		catalog := map[string]Model{
			"catalog-a": {Id: "catalog-a", Provider: "catalog-test"},
			"catalog-b": {Id: "catalog-b", Provider: "catalog-test"},
		}

		if err := RegisterCatalog(catalog, "http://mock.example:9093/v1"); err != nil {
			t.Fatal(err)
		}

		if len(Models) != 2 {
			t.Fatalf("%d models are registered, want 2", len(Models))
		}
		for id := range catalog {
			if got := Models[id].BaseUrl; got != "http://mock.example:9093/v1" {
				t.Errorf("%s base URL = %q, want the catalog's", id, got)
			}
		}
	})

	t.Run("refuses a base URL requests can't be built on, and registers nothing", func(t *testing.T) {
		for _, baseURL := range []string{
			"api.example.com/v1",           // no scheme
			"ftp://api.example.com",        // not http(s)
			"http://",                      // no host
			"https://api.example.com/v1/",  // the endpoint path would follow a double slash
			"https://api.example.com/v1?a", // a query would end up before the endpoint path
		} {
			Reset()
			if err := RegisterCatalog(map[string]Model{"catalog-c": {Id: "catalog-c"}}, baseURL); err == nil {
				t.Errorf("%q was accepted", baseURL)
			}
			if len(Models) != 0 {
				t.Errorf("%q: %d models were registered, want none", baseURL, len(Models))
			}
		}
	})
}
