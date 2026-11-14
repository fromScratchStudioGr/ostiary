package deepseek

import (
	"strings"
	"testing"
	"time"
)

func TestPeakPricingMultiplierAt(t *testing.T) {
	utc := func(year int, month time.Month, day, hour, min int) time.Time {
		return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
	}
	beijing := time.FixedZone("CST", 8*60*60)

	tests := []struct {
		name string
		at   time.Time
		want float64
	}{
		// Mon 2026-01-05 is a normal working day.
		{"before first window", utc(2026, 1, 5, 0, 59), 1},
		{"first window start is inclusive", utc(2026, 1, 5, 1, 0), 2},
		{"inside first window", utc(2026, 1, 5, 3, 59), 2},
		{"first window end is exclusive", utc(2026, 1, 5, 4, 0), 1},
		{"between windows", utc(2026, 1, 5, 5, 59), 1},
		{"second window start is inclusive", utc(2026, 1, 5, 6, 0), 2},
		{"inside second window", utc(2026, 1, 5, 9, 59), 2},
		{"second window end is exclusive", utc(2026, 1, 5, 10, 0), 1},
		{"evening", utc(2026, 1, 5, 20, 0), 1},
		{"friday is a working day", utc(2026, 1, 9, 2, 0), 2},
		{"saturday", utc(2026, 1, 10, 2, 0), 1},
		{"sunday", utc(2026, 1, 11, 7, 0), 1},
		{"other timezones are converted to utc", time.Date(2026, 1, 5, 10, 0, 0, 0, beijing), 2},

		// Weekday holidays are off-peak.
		{"new year's day (thursday)", utc(2026, 1, 1, 2, 0), 1},
		{"spring festival (monday)", utc(2026, 2, 16, 7, 0), 1},
		{"first day after the spring festival", utc(2026, 2, 24, 7, 0), 2},
		{"national day (thursday)", utc(2026, 10, 1, 2, 0), 1},
		{"last day of national day holiday (wednesday)", utc(2026, 10, 7, 2, 0), 1},
		{"first workday after national day", utc(2026, 10, 8, 2, 0), 2},

		// 2027 isn't covered by the holiday list: priced as peak, never lower.
		{"year without a holiday list", utc(2027, 1, 5, 2, 0), 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peakPricing.MultiplierAt(tt.at); got != tt.want {
				t.Fatalf("MultiplierAt(%s) = %v, want %v", tt.at.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

func TestHolidays(t *testing.T) {
	for year, ranges := range holidays {
		for _, r := range ranges {
			from, err := time.Parse(time.DateOnly, r[0])
			if err != nil {
				t.Errorf("%v: start doesn't parse: %v", r, err)
				continue
			}
			to, err := time.Parse(time.DateOnly, r[1])
			if err != nil {
				t.Errorf("%v: end doesn't parse: %v", r, err)
				continue
			}
			if to.Before(from) {
				t.Errorf("%v ends before it starts", r)
			}
			// A range filed under another year would never be looked up
			// where its author expects.
			if from.Year() != year || to.Year() != year {
				t.Errorf("%v is listed under %d", r, year)
			}
		}
	}
}

func TestMustParseDate(t *testing.T) {
	t.Run("parses a date", func(t *testing.T) {
		want := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
		if got := mustParseDate("2026-02-15"); !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("panics on a date it can't parse, naming it", func(t *testing.T) {
		defer func() {
			message, _ := recover().(string)
			if !strings.Contains(message, "2026-13-45") {
				t.Errorf("panic = %q, want a message naming the date", message)
			}
		}()

		mustParseDate("2026-13-45")

		t.Error("didn't panic")
	})
}

func TestEveryDeepSeekModelHasPeakPricing(t *testing.T) {
	for id, model := range DeepSeekModels {
		if model.PeakPricing == nil {
			t.Errorf("%s has no peak pricing", id)
		}
	}
}
