package main

import (
	"fmt"
	"time"
)

const dateFmt = "2006-01-02"

// daysBetween: the distance between two date strings in calendar days
// (b - a). Both come from the DB or from nextOccurrence and are valid; UTC,
// so no DST change in between.
func daysBetween(a, b string) int {
	ta, _ := time.Parse(dateFmt, a)
	tb, _ := time.Parse(dateFmt, b)
	return int(tb.Sub(ta).Hours() / 24)
}

// nextOccurrence returns the next occurrence strictly after today (and after
// due): it always advances at least one step, and for overdue tasks keeps
// advancing until the result lies in the future (no backlog of occurrences).
func nextOccurrence(due, rule, today string) (string, error) {
	d, err := time.Parse(dateFmt, due)
	if err != nil {
		return "", fmt.Errorf("invalid date %q: %w", due, err)
	}
	t, err := time.Parse(dateFmt, today)
	if err != nil {
		return "", fmt.Errorf("invalid date %q: %w", today, err)
	}
	step, err := stepFunc(rule)
	if err != nil {
		return "", err
	}
	d = step(d)
	for !d.After(t) {
		d = step(d)
	}
	return d.Format(dateFmt), nil
}

func stepFunc(rule string) (func(time.Time) time.Time, error) {
	switch rule {
	case "daily":
		return func(d time.Time) time.Time { return d.AddDate(0, 0, 1) }, nil
	case "weekly":
		return func(d time.Time) time.Time { return d.AddDate(0, 0, 7) }, nil
	case "monthly":
		return nextMonth, nil
	default:
		return nil, fmt.Errorf("unknown recurrence %q", rule)
	}
}

// nextMonth: same day in the following month, clamped to that month's
// length. Deliberately not AddDate(0,1,0): that normalizes Jan 31 to
// Mar 2/3.
func nextMonth(d time.Time) time.Time {
	y, m, day := d.Date()
	m++
	if m > 12 {
		m = 1
		y++
	}
	if dm := daysInMonth(y, m); day > dm {
		day = dm
	}
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

func daysInMonth(y int, m time.Month) int {
	// day 0 of the following month = the last day of m
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
