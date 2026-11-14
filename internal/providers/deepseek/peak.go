package deepseek

import (
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/providers"
)

// holidays lists the Chinese public holidays that are off-peak, as inclusive
// date ranges.
var holidays = map[int][][2]string{
	2026: {
		{"2026-01-01", "2026-01-03"}, // New Year's Day
		{"2026-02-15", "2026-02-23"}, // Spring Festival
		{"2026-04-04", "2026-04-06"}, // Qingming Festival
		{"2026-05-01", "2026-05-05"}, // Labor Day
		{"2026-06-19", "2026-06-21"}, // Dragon Boat Festival
		{"2026-09-25", "2026-09-27"}, // Mid-Autumn Festival
		{"2026-10-01", "2026-10-07"}, // National Day
	},
}

// peakPricing doubles all prices from 01:00 to 04:00 and from 06:00 to 10:00
// UTC, Monday to Friday, except on Chinese public holidays.
var peakPricing = newPeakPricing()

func newPeakPricing() *providers.PeakPricing {
	p := &providers.PeakPricing{
		Multiplier:    2,
		Weekdays:      []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		HourWindows:   []providers.HourWindow{{Start: 1, End: 4}, {Start: 6, End: 10}},
		ExcludedDates: map[string]bool{},
	}

	for _, ranges := range holidays {
		for _, r := range ranges {
			from := mustParseDate(r[0])
			to := mustParseDate(r[1])
			for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
				p.ExcludedDates[d.Format(time.DateOnly)] = true
			}
		}
	}

	return p
}

func mustParseDate(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic("deepseek: invalid holiday date " + s + ": " + err.Error())
	}
	return d
}
