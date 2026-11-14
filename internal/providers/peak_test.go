package providers

import (
	"testing"
	"time"
)

// The windows, weekdays and holidays of a real provider are tested with that
// provider's data (see deepseek). These check the logic itself, with a
// configuration of its own.
func TestPeakPricingMultiplierAt(t *testing.T) {
	// 2026-01-06 and 2026-01-13 are Tuesdays.
	tuesday := func(day, hour, min int) time.Time {
		return time.Date(2026, 1, day, hour, min, 0, 0, time.UTC)
	}
	peak := &PeakPricing{
		Multiplier:    3,
		Weekdays:      []time.Weekday{time.Tuesday},
		HourWindows:   []HourWindow{{Start: 10, End: 12}},
		ExcludedDates: map[string]bool{"2026-01-06": true},
	}
	kiritimati := time.FixedZone("+14", 14*60*60)

	tests := []struct {
		name    string
		pricing *PeakPricing
		at      time.Time
		want    float64
	}{
		{"no peak pricing is never peak", nil, tuesday(13, 10, 30), 1},
		{"applies the configured multiplier", peak, tuesday(13, 10, 30), 3},
		{"a weekday that isn't listed", peak, time.Date(2026, 1, 14, 10, 30, 0, 0, time.UTC), 1},
		{"an hour outside every window", peak, tuesday(13, 9, 59), 1},
		{"an excluded date", peak, tuesday(6, 10, 30), 1},
		{"the same weekday and hour on another date", peak, tuesday(13, 10, 30), 3},
		{
			// 2026-01-13 23:30 at UTC-5 is already Wednesday in UTC.
			"the weekday is the UTC one, not the local one (local Tuesday)",
			peak, time.Date(2026, 1, 13, 23, 30, 0, 0, time.FixedZone("-5", -5*60*60)), 1,
		},
		{
			// 2026-01-14 00:30 at UTC+14 is 2026-01-13 10:30 in UTC, a Tuesday.
			"the weekday is the UTC one, not the local one (local Wednesday)",
			peak, time.Date(2026, 1, 14, 0, 30, 0, 0, kiritimati), 3,
		},
		{
			"no weekdays means never peak",
			&PeakPricing{Multiplier: 3, HourWindows: peak.HourWindows}, tuesday(13, 10, 30), 1,
		},
		{
			"no hour windows means never peak",
			&PeakPricing{Multiplier: 3, Weekdays: peak.Weekdays}, tuesday(13, 10, 30), 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pricing.MultiplierAt(tt.at); got != tt.want {
				t.Errorf("MultiplierAt(%s) = %v, want %v", tt.at.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}
