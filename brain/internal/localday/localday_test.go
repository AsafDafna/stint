package localday

import (
	"testing"
	"time"
)

const instant = "2006-01-02T15:04:05-07:00"

func load(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestStart(t *testing.T) {
	cases := []struct {
		name, zone string
		y          int
		m          time.Month
		d          int
		want       string
	}{
		{"plain day", "Asia/Jerusalem", 2026, 9, 16, "2026-09-16T00:00:00+03:00"},
		{"00:00 skipped, time.Date goes back a day", "America/Santiago", 2026, 9, 6, "2026-09-06T01:00:00-03:00"},
		{"00:00 skipped, time.Date goes back a day", "Atlantic/Azores", 2026, 3, 29, "2026-03-29T01:00:00+00:00"},
		{"00:00 skipped, time.Date goes forward", "Asia/Beirut", 2026, 3, 29, "2026-03-29T01:00:00+03:00"},
		{"23:00 repeats, 00:00 does not", "America/Santiago", 2026, 4, 5, "2026-04-05T00:00:00-04:00"},
		{"23:00 repeats, 00:00 does not", "Asia/Beirut", 2026, 10, 25, "2026-10-25T00:00:00+02:00"},
		{"00:00 repeats, the day starts at the first", "Asia/Amman", 2021, 10, 29, "2021-10-29T00:00:00+03:00"},
		{"date normalized like time.Date", "Asia/Jerusalem", 2026, 9, 0, "2026-08-31T00:00:00+03:00"},
	}
	for _, c := range cases {
		loc := load(t, c.zone)
		got := Start(c.y, c.m, c.d, loc)
		if got.Format(instant) != c.want || got.Location() != loc {
			t.Errorf("%s: Start(%d-%02d-%02d, %s) = %s in %v, want %s", c.name, c.y, c.m, c.d, c.zone, got.Format(instant), got.Location(), c.want)
		}
	}
}

func TestOf(t *testing.T) {
	santiago := load(t, "America/Santiago")
	cases := []struct{ in, want string }{
		{"2026-09-06T10:00:00-03:00", "2026-09-06T01:00:00-03:00"},
		{"2026-09-05T23:30:00-04:00", "2026-09-05T00:00:00-04:00"},
	}
	for _, c := range cases {
		in, _ := time.Parse(time.RFC3339, c.in)
		if got := Of(in.UTC(), santiago).Format(instant); got != c.want {
			t.Errorf("Of(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestAdd(t *testing.T) {
	santiago, beirut := load(t, "America/Santiago"), load(t, "Asia/Beirut")
	cases := []struct {
		name string
		from time.Time
		n    int
		want string
	}{
		{"from a 01:00 start to a day that has 00:00", Start(2026, 3, 29, beirut), 7, "2026-04-05T00:00:00+03:00"},
		{"into a day whose 00:00 is skipped", Start(2026, 8, 30, santiago), 7, "2026-09-06T01:00:00-03:00"},
		{"back out of it", Start(2026, 9, 6, santiago), -1, "2026-09-05T00:00:00-04:00"},
		{"from mid-day", time.Date(2026, 9, 5, 15, 0, 0, 0, santiago), 1, "2026-09-06T01:00:00-03:00"},
	}
	for _, c := range cases {
		got := Add(c.from, c.n)
		if got.Format(instant) != c.want || got.Location() != c.from.Location() {
			t.Errorf("%s: Add(%s, %d) = %s in %v, want %s", c.name, c.from.Format(instant), c.n, got.Format(instant), got.Location(), c.want)
		}
	}
}

func TestParse(t *testing.T) {
	santiago := load(t, "America/Santiago")
	got, err := Parse("2026-09-06", santiago)
	if err != nil || got.Format(instant) != "2026-09-06T01:00:00-03:00" {
		t.Errorf("Parse(2026-09-06) = %s, %v", got.Format(instant), err)
	}
	for _, bad := range []string{"2026-02-30", "2026-11-8", "16/10/2026", ""} {
		if _, err := Parse(bad, santiago); err == nil {
			t.Errorf("Parse(%q): want an error", bad)
		}
	}
}
