// Package localday finds where local calendar days begin. time.Date(y, m, d, 0, 0, 0, 0, loc)
// is not that instant on a day whose 00:00 a DST change skips or repeats: Go can return
// 23:00 of the day before (America/Santiago) or the second 00:00 (Asia/Amman, 2021).
package localday

import "time"

// Start is the first instant of the calendar day year-month-day in loc, with the date
// normalized like time.Date's (day 0 is the last day of the month before).
func Start(year int, month time.Month, day int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, 0, 0, 0, 0, loc)
	if _, _, d := t.Date(); d != time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Day() {
		// 00:00 is skipped and t landed before the change: the day begins at the change.
		_, t = t.ZoneBounds()
	} else if p := t.Add(-time.Nanosecond); p.Day() == d {
		// 00:00 repeats and t is the second one: the day began at the first, in p's offset.
		_, off := p.Zone()
		t = time.Date(year, month, day, 0, 0, 0, 0, time.FixedZone("", off)).In(loc)
	}
	return t
}

// Of is the first instant of the local day in loc that holds t.
func Of(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return Start(lt.Year(), lt.Month(), lt.Day(), loc)
}

// Add is the first instant of the day n days after the day that holds t, in t's Location.
// Use it, not AddDate, to step from one day or week boundary to another: AddDate keeps the
// clock time, which is not 00:00 on every day.
func Add(t time.Time, n int) time.Time {
	return Start(t.Year(), t.Month(), t.Day()+n, t.Location())
}

// Parse reads a YYYY-MM-DD date as the first instant of that day in loc. Out-of-range dates
// such as 2026-02-30 are errors, not normalized.
func Parse(s string, loc *time.Location) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	return Start(d.Year(), d.Month(), d.Day(), loc), nil
}
