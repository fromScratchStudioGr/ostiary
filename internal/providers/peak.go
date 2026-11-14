package providers

import (
	"time"
)

type HourWindow struct {
	Start int
	End   int
}

type PeakPricing struct {
	Multiplier    float64
	Weekdays      []time.Weekday
	HourWindows   []HourWindow
	ExcludedDates map[string]bool
}

func (p *PeakPricing) MultiplierAt(t time.Time) float64 {
	if p == nil {
		return 1
	}

	t = t.UTC()

	if !containsWeekday(p.Weekdays, t.Weekday()) {
		return 1
	}
	if !inWindow(p.HourWindows, t.Hour()) {
		return 1
	}

	if p.ExcludedDates[t.Format(time.DateOnly)] {
		return 1
	}

	return p.Multiplier
}

func containsWeekday(days []time.Weekday, day time.Weekday) bool {
	for _, d := range days {
		if d == day {
			return true
		}
	}
	return false
}

func inWindow(windows []HourWindow, hour int) bool {
	for _, w := range windows {
		if hour >= w.Start && hour < w.End {
			return true
		}
	}
	return false
}
