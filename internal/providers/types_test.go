package providers

import (
	"reflect"
	"strings"
	"testing"
)

func TestCostForInputTokens(t *testing.T) {
	longContext := Cost{Input: 4, Output: 15, CacheRead: Price(0.4)}
	tiered := Cost{
		Input:       2,
		Output:      10,
		CacheRead:   Price(0.2),
		LongContext: &LongContextCost{ThresholdTokens: 200_000, Cost: longContext},
	}
	flat := Cost{Input: 1, Output: 2}

	tests := []struct {
		name   string
		cost   Cost
		tokens int
		want   Cost
	}{
		{"below the threshold", tiered, 199_999, tiered},
		{"exactly at the threshold still gets the base prices", tiered, 200_000, tiered},
		{"one token over switches to the long-context prices as a whole", tiered, 200_001, longContext},
		{"a model without a long-context tier never switches", flat, 10_000_000, flat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cost.ForInputTokens(tt.tokens); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ForInputTokens(%d) = %+v, want %+v", tt.tokens, got, tt.want)
			}
		})
	}
}

// recovered runs f and returns what it panicked with, or nil.
func recovered(f func()) (panicked any) {
	defer func() { panicked = recover() }()
	f()
	return nil
}

func TestRegister(t *testing.T) {
	t.Run("makes the model available under its id", func(t *testing.T) {
		const id = "test-register-model"
		want := Model{Id: id, Name: "Test"}
		t.Cleanup(func() { delete(Models, id) })

		Register(id, want)

		if got, ok := Models[id]; !ok || got != want {
			t.Errorf("Models[%q] = %+v, %v; want %+v", id, got, ok, want)
		}
	})

	t.Run("panics on an id that's already taken, naming it, and keeps the first model", func(t *testing.T) {
		const id = "test-register-duplicate"
		first := Model{Id: id, Name: "first"}
		t.Cleanup(func() { delete(Models, id) })
		Register(id, first)

		panicked := recovered(func() { Register(id, Model{Id: id, Name: "second"}) })

		message, _ := panicked.(string)
		if !strings.Contains(message, id) {
			t.Errorf("panic = %v, want a message naming %q", panicked, id)
		}
		if got := Models[id]; got != first {
			t.Errorf("Models[%q] = %+v, want the first registration %+v", id, got, first)
		}
	})
}
