package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
)

// noon is twelve o'clock UTC, the given number of days ago. Usage rows are
// created at noon so the day they fall on doesn't depend on when, or where,
// the test runs.
func noon(daysAgo int) time.Time {
	d := time.Now().UTC().AddDate(0, 0, -daysAgo)
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.UTC)
}

func day(daysAgo int) string {
	return noon(daysAgo).Format("2006-01-02")
}

// usageWorld is two consumers using two provider keys, with usage spread over
// two days, small enough that every total below can be added up by hand.
//
//	row  consumer  key        day       tokens (completion/hit/miss/write1/write2)   cost
//	1    acme      openai     today     100 / 200 / 300 / 10 / 20 = 630              1 + 0.5 + 2 + 0.25 + 0.25 = 4
//	2    acme      anthropic  today     50 (completion only)                         3
//	3    globex    openai     10 days   40 (completion only)                         0.5
//	4    globex    openai     today     1000 (completion only)                       10
type usageWorld struct {
	p                 *panel
	acme, globex      models.Consumer
	openai, anthropic models.ProviderKey
}

func newUsageWorld(t *testing.T) usageWorld {
	t.Helper()
	p := newPanel(t)
	w := usageWorld{
		p:         p,
		acme:      p.createConsumer(t, "acme"),
		globex:    p.createConsumer(t, "globex"),
		openai:    p.createProviderKey(t, "openai", "1111"),
		anthropic: p.createProviderKey(t, "anthropic", "2222"),
	}
	rows := []models.Usage{
		{
			ConsumerID: w.acme.ID, ProviderKeyID: w.openai.ID,
			CompletionTokens: 100, CompletionTokensCost: 1,
			PromptCacheHitTokens: 200, PromptCacheHitTokensCost: 0.5,
			PromptCacheMissTokens: 300, PromptCacheMissTokensCost: 2,
			PromptCacheWriteLevel1Tokens: 10, PromptCacheWriteLevel1TokensCost: 0.25,
			PromptCacheWriteLevel2Tokens: 20, PromptCacheWriteLevel2TokensCost: 0.25,
		},
		{ConsumerID: w.acme.ID, ProviderKeyID: w.anthropic.ID, CompletionTokens: 50, CompletionTokensCost: 3},
		{ConsumerID: w.globex.ID, ProviderKeyID: w.openai.ID, CompletionTokens: 40, CompletionTokensCost: 0.5},
		{ConsumerID: w.globex.ID, ProviderKeyID: w.openai.ID, CompletionTokens: 1000, CompletionTokensCost: 10},
	}
	ages := []int{0, 0, 10, 0}
	for i := range rows {
		rows[i].CreatedAt = noon(ages[i])
		if err := p.db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seeding usage: %v", err)
		}
	}
	return w
}

func TestGetGeneralUsageHandler(t *testing.T) {
	get := func(t *testing.T, p *panel, query string) *httptest.ResponseRecorder {
		return p.call(t, getGeneralUsageHandler(p.cfg, p.db), "GET /usage", "/usage"+query, "")
	}

	t.Run("adds up what every consumer used, day by day", func(t *testing.T) {
		w := newUsageWorld(t)
		tests := []struct {
			name         string
			query        string
			cost         float64
			tokens       int64
			consumers    int64
			keys         int64
			costOverTime []api.DailyCostResponse
		}{
			{"over all time", "", 17.5, 1720, 2, 2, []api.DailyCostResponse{{Date: day(10), Cost: 0.5}, {Date: day(0), Cost: 17}}},
			{"over the last five days", "?timespan=5", 17, 1680, 2, 2, []api.DailyCostResponse{{Date: day(0), Cost: 17}}},
			{"over the last thirty days", "?timespan=30", 17.5, 1720, 2, 2, []api.DailyCostResponse{{Date: day(10), Cost: 0.5}, {Date: day(0), Cost: 17}}},
			{"for one provider", "?provider=openai", 14.5, 1670, 2, 1, []api.DailyCostResponse{{Date: day(10), Cost: 0.5}, {Date: day(0), Cost: 14}}},
			{"for another provider", "?provider=anthropic", 3, 50, 1, 1, []api.DailyCostResponse{{Date: day(0), Cost: 3}}},
			{"for one provider over the last five days", "?provider=openai&timespan=5", 14, 1630, 2, 1, []api.DailyCostResponse{{Date: day(0), Cost: 14}}},
			{"for a provider nobody used", "?provider=gemini", 0, 0, 0, 0, []api.DailyCostResponse{}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := get(t, w.p, tt.query)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
				}
				got := decode[api.UsageSummaryResponse](t, rec)
				if got.TotalCost != tt.cost || got.TotalTokens != tt.tokens || got.UserCount != tt.consumers || got.KeyCount != tt.keys {
					t.Errorf("summary = cost %v, tokens %d, %d consumers, %d keys; want %v, %d, %d, %d",
						got.TotalCost, got.TotalTokens, got.UserCount, got.KeyCount, tt.cost, tt.tokens, tt.consumers, tt.keys)
				}
				if len(got.CostOverTime) != len(tt.costOverTime) {
					t.Fatalf("cost over time = %+v, want %+v", got.CostOverTime, tt.costOverTime)
				}
				for i, want := range tt.costOverTime {
					if got.CostOverTime[i] != want {
						t.Errorf("cost over time = %+v, want %+v (oldest day first)", got.CostOverTime, tt.costOverTime)
						break
					}
				}
			})
		}
	})

	t.Run("answers with exactly the fields the client expects, with the summed values", func(t *testing.T) {
		w := newUsageWorld(t)

		object := jsonObject(t, get(t, w.p, ""))

		wantFields(t, object, "costOverTime", "keyCount", "totalCost", "totalTokens", "userCount")
		want := map[string]any{"totalCost": 17.5, "totalTokens": float64(1720), "userCount": float64(2), "keyCount": float64(2)}
		for field, value := range want {
			if object[field] != value {
				t.Errorf("%s = %v, want %v", field, object[field], value)
			}
		}
		days := entriesOf(t, object, "costOverTime")
		if len(days) != 2 {
			t.Fatalf("costOverTime = %v, want two days", days)
		}
		for i, want := range []map[string]any{{"date": day(10), "cost": 0.5}, {"date": day(0), "cost": float64(17)}} {
			wantFields(t, days[i], "cost", "date")
			if days[i]["date"] != want["date"] || days[i]["cost"] != want["cost"] {
				t.Errorf("costOverTime[%d] = %v, want %v", i, days[i], want)
			}
		}
	})

	t.Run("answers with zeros and an empty list, not null, when nothing was used", func(t *testing.T) {
		p := newPanel(t)

		rec := get(t, p, "")

		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"costOverTime":[]`) {
			t.Errorf("response = %d %q, want 200 with an empty costOverTime list", rec.Code, rec.Body.String())
		}
		got := decode[api.UsageSummaryResponse](t, rec)
		if got.TotalCost != 0 || got.TotalTokens != 0 || got.UserCount != 0 || got.KeyCount != 0 {
			t.Errorf("summary = %+v, want zeros", got)
		}
	})

	t.Run("accepts every provider the gateway serves", func(t *testing.T) {
		p := newPanel(t)
		for _, provider := range []string{"anthropic", "openai", "deepseek", "gemini"} {
			if rec := get(t, p, "?provider="+provider); rec.Code != http.StatusOK {
				t.Errorf("provider %s = %d, want 200: %s", provider, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("turns the request down when the filter can't be used", func(t *testing.T) {
		p := newPanel(t)
		tests := []struct {
			query   string
			message string
		}{
			{"?timespan=abc", "Invalid timespan"},
			{"?timespan=-1", "Invalid timespan"},
			{"?timespan=1.5", "Invalid timespan"},
			{"?provider=bogus", "Invalid provider"},
			{"?provider=OPENAI", "Invalid provider"},
			{"?timespan=5&provider=bogus", "Invalid provider"},
			{"?timespan=abc&provider=openai", "Invalid timespan"},
		}
		for _, tt := range tests {
			t.Run(tt.query, func(t *testing.T) {
				wantError(t, get(t, p, tt.query), http.StatusBadRequest, tt.message)
			})
		}
	})

	t.Run("reports a failure to total the usage", func(t *testing.T) {
		p := newPanel(t)
		p.breakTable(t, "usages")

		wantError(t, get(t, p, ""), http.StatusInternalServerError, "Could not fetch usage summary")
	})

	t.Run("reports a failure to break the cost down by day", func(t *testing.T) {
		w := newUsageWorld(t)
		w.p.failQueriesSelecting(t, "TO_CHAR")

		wantError(t, get(t, w.p, ""), http.StatusInternalServerError, "Could not fetch cost over time")
	})
}

func TestGetConsumerUsageHandler(t *testing.T) {
	get := func(t *testing.T, p *panel, id, query string) *httptest.ResponseRecorder {
		return p.call(t, getConsumerUsageHandler(p.cfg, p.db), "GET /usage/consumers/{id}", "/usage/consumers/"+id+query, "")
	}

	t.Run("adds up what one consumer used, by day and by key, and nothing of anyone else's", func(t *testing.T) {
		w := newUsageWorld(t)
		tests := []struct {
			name           string
			consumer       models.Consumer
			query          string
			cost           float64
			tokens         int64
			tokensOverTime []api.DailyTokensResponse
			costByKey      []api.KeyCostResponse
		}{
			{
				"acme over all time", w.acme, "", 7, 680,
				[]api.DailyTokensResponse{{Date: day(0), Tokens: 680}},
				[]api.KeyCostResponse{{ProviderKeyId: w.openai.ID, Provider: "openai", Cost: 4}, {ProviderKeyId: w.anthropic.ID, Provider: "anthropic", Cost: 3}},
			},
			{
				"globex over all time", w.globex, "", 10.5, 1040,
				[]api.DailyTokensResponse{{Date: day(10), Tokens: 40}, {Date: day(0), Tokens: 1000}},
				[]api.KeyCostResponse{{ProviderKeyId: w.openai.ID, Provider: "openai", Cost: 10.5}},
			},
			{
				"globex over the last five days", w.globex, "?timespan=5", 10, 1000,
				[]api.DailyTokensResponse{{Date: day(0), Tokens: 1000}},
				[]api.KeyCostResponse{{ProviderKeyId: w.openai.ID, Provider: "openai", Cost: 10}},
			},
			{
				"acme for one provider", w.acme, "?provider=anthropic", 3, 50,
				[]api.DailyTokensResponse{{Date: day(0), Tokens: 50}},
				[]api.KeyCostResponse{{ProviderKeyId: w.anthropic.ID, Provider: "anthropic", Cost: 3}},
			},
			{
				"globex for one provider over the last thirty days", w.globex, "?provider=openai&timespan=30", 10.5, 1040,
				[]api.DailyTokensResponse{{Date: day(10), Tokens: 40}, {Date: day(0), Tokens: 1000}},
				[]api.KeyCostResponse{{ProviderKeyId: w.openai.ID, Provider: "openai", Cost: 10.5}},
			},
			{
				"globex for a provider it didn't use", w.globex, "?provider=anthropic", 0, 0,
				[]api.DailyTokensResponse{},
				[]api.KeyCostResponse{},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := get(t, w.p, idOf(tt.consumer.ID), tt.query)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
				}
				got := decode[api.UserUsageSummaryResponse](t, rec)
				if got.TotalCost != tt.cost || got.TotalTokens != tt.tokens {
					t.Errorf("totals = cost %v, tokens %d; want %v, %d", got.TotalCost, got.TotalTokens, tt.cost, tt.tokens)
				}
				if len(got.TokensOverTime) != len(tt.tokensOverTime) {
					t.Fatalf("tokens over time = %+v, want %+v", got.TokensOverTime, tt.tokensOverTime)
				}
				for i, want := range tt.tokensOverTime {
					if got.TokensOverTime[i] != want {
						t.Errorf("tokens over time = %+v, want %+v (oldest day first)", got.TokensOverTime, tt.tokensOverTime)
						break
					}
				}
				if len(got.CostByKey) != len(tt.costByKey) {
					t.Fatalf("cost by key = %+v, want %+v", got.CostByKey, tt.costByKey)
				}
				for i, want := range tt.costByKey {
					if got.CostByKey[i] != want {
						t.Errorf("cost by key = %+v, want %+v (most expensive first)", got.CostByKey, tt.costByKey)
						break
					}
				}
			})
		}
	})

	t.Run("answers with exactly the fields the client expects, with the summed values", func(t *testing.T) {
		w := newUsageWorld(t)

		object := jsonObject(t, get(t, w.p, idOf(w.acme.ID), ""))

		wantFields(t, object, "costByKey", "tokensOverTime", "totalCost", "totalTokens")
		if object["totalCost"] != float64(7) || object["totalTokens"] != float64(680) {
			t.Errorf("totals = cost %v, tokens %v, want 7 and 680", object["totalCost"], object["totalTokens"])
		}
		days := entriesOf(t, object, "tokensOverTime")
		if len(days) != 1 {
			t.Fatalf("tokensOverTime = %v, want one day", days)
		}
		wantFields(t, days[0], "date", "tokens")
		if days[0]["date"] != day(0) || days[0]["tokens"] != float64(680) {
			t.Errorf("tokensOverTime[0] = %v, want %s with 680 tokens", days[0], day(0))
		}
		keys := entriesOf(t, object, "costByKey")
		if len(keys) != 2 {
			t.Fatalf("costByKey = %v, want two keys", keys)
		}
		for i, want := range []map[string]any{
			{"providerKeyId": float64(w.openai.ID), "provider": "openai", "cost": float64(4)},
			{"providerKeyId": float64(w.anthropic.ID), "provider": "anthropic", "cost": float64(3)},
		} {
			wantFields(t, keys[i], "cost", "provider", "providerKeyId")
			if keys[i]["providerKeyId"] != want["providerKeyId"] || keys[i]["provider"] != want["provider"] || keys[i]["cost"] != want["cost"] {
				t.Errorf("costByKey[%d] = %v, want %v", i, keys[i], want)
			}
		}
	})

	t.Run("answers with zeros and empty lists, not null, for a consumer that used nothing", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")

		rec := get(t, p, idOf(consumer.ID), "")

		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `"tokensOverTime":[]`) || !strings.Contains(body, `"costByKey":[]`) {
			t.Errorf("response = %d %q, want 200 with empty lists", rec.Code, body)
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		p := newPanel(t)
		consumer := p.createConsumer(t, "acme")
		gone := p.createConsumer(t, "gone")
		p.db.Delete(&gone)
		id := idOf(consumer.ID)
		tests := []struct {
			name    string
			id      string
			query   string
			status  int
			message string
		}{
			{"an id that is not a number", "abc", "", http.StatusBadRequest, "Invalid consumer id"},
			{"a consumer that doesn't exist", "999999", "", http.StatusNotFound, "Consumer not found"},
			{"a consumer that was deleted", idOf(gone.ID), "", http.StatusNotFound, "Consumer not found"},
			{"a timespan that is not a number", id, "?timespan=abc", http.StatusBadRequest, "Invalid timespan"},
			{"a negative timespan", id, "?timespan=-1", http.StatusBadRequest, "Invalid timespan"},
			{"a provider that doesn't exist", id, "?provider=bogus", http.StatusBadRequest, "Invalid provider"},
			{"a bad filter on a consumer that doesn't exist", "999999", "?provider=bogus", http.StatusNotFound, "Consumer not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantError(t, get(t, p, tt.id, tt.query), tt.status, tt.message)
			})
		}
	})

	t.Run("reports a failure at each of its three queries", func(t *testing.T) {
		tests := []struct {
			name     string
			fragment string
			message  string
		}{
			{"the totals", "AS total_cost", "Could not fetch usage summary"},
			{"the tokens by day", "AS tokens", "Could not fetch tokens over time"},
			{"the cost by key", "AS provider_key_id", "Could not fetch cost by key"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				w := newUsageWorld(t)
				w.p.failQueriesSelecting(t, tt.fragment)

				wantError(t, get(t, w.p, idOf(w.acme.ID), ""), http.StatusInternalServerError, tt.message)
			})
		}
	})
}
