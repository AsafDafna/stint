package totals

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stint/internal/attribute"
	"stint/internal/db"
)

func jerusalem(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestWeekStartHalfOpenSunday(t *testing.T) {
	loc := jerusalem(t)
	cases := []struct{ in, want string }{
		{"2026-09-16 10:00", "2026-09-13 00:00"}, // Wednesday
		{"2026-09-13 00:00", "2026-09-13 00:00"}, // Sunday midnight belongs to its own week
		{"2026-09-12 23:59", "2026-09-06 00:00"}, // Saturday night is the previous week
		{"2026-09-19 23:59", "2026-09-13 00:00"},
		{"2026-09-20 00:00", "2026-09-20 00:00"},
	}
	for _, c := range cases {
		in, _ := time.ParseInLocation("2006-01-02 15:04", c.in, loc)
		if got := WeekStart(in, loc, time.Sunday).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("WeekStart(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestWeekStartOtherFirstDays(t *testing.T) {
	loc := jerusalem(t)
	cases := []struct {
		first    time.Weekday
		in, want string
	}{
		{time.Monday, "2026-09-16 10:00", "2026-09-14 00:00"}, // Wednesday
		{time.Monday, "2026-09-14 00:00", "2026-09-14 00:00"}, // Monday midnight belongs to its own week
		{time.Monday, "2026-09-13 23:59", "2026-09-07 00:00"}, // Sunday night is the previous week
		{time.Monday, "2026-09-20 23:59", "2026-09-14 00:00"},
		{time.Saturday, "2026-09-16 10:00", "2026-09-12 00:00"},
		{time.Saturday, "2026-09-12 00:00", "2026-09-12 00:00"},
		{time.Saturday, "2026-09-11 23:59", "2026-09-05 00:00"}, // Friday night is the previous week
	}
	for _, c := range cases {
		in, _ := time.ParseInLocation("2006-01-02 15:04", c.in, loc)
		if got := WeekStart(in, loc, c.first).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("WeekStart(%s, %v) = %s, want %s", c.in, c.first, got, c.want)
		}
	}
}

// WeekStart always answers in loc, whatever zone t carries, and NextWeek keeps that
// Location. Callers pass WeekStart output: across a DST change, the same instant in UTC
// would land an hour off local midnight.
func TestWeekStartAndNextWeekKeepZone(t *testing.T) {
	loc := jerusalem(t)
	ws := WeekStart(time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC), loc, time.Sunday) // Wed, IST
	if ws.Location() != loc {
		t.Fatalf("WeekStart Location = %v, want %v", ws.Location(), loc)
	}
	if got := ws.Format("2006-01-02 15:04"); got != "2026-10-25 00:00" {
		t.Fatalf("WeekStart = %s, want 2026-10-25 00:00", got)
	}
	nw := NextWeek(ws)
	if nw.Location() != loc {
		t.Errorf("NextWeek Location = %v, want %v", nw.Location(), loc)
	}
	if got := nw.Format("2006-01-02 15:04"); got != "2026-11-01 00:00" {
		t.Errorf("NextWeek = %s, want 2026-11-01 00:00", got)
	}
	// NextWeek steps days in weekStart's Location: on the UTC copy it lands on a UTC midnight
	// (02:00 IST on Saturday), which is why callers must not convert.
	if wrong := NextWeek(ws.UTC()); wrong.Equal(nw) {
		t.Errorf("NextWeek(weekStart.UTC()) = %v, expected it to differ from %v", wrong, nw)
	}
}

func load(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

const instant = "2006-01-02T15:04:05-07:00"

// Chile skips 00:00 on Sunday 2026-09-06 (clocks jump to 01:00) and Go's time.Date resolves
// that midnight to 23:00 on Saturday; the week must still start, and end, on Sunday.
func TestWeekStartAcrossMidnightGap(t *testing.T) {
	loc := load(t, "America/Santiago")
	cases := []struct {
		first    time.Weekday
		in, want string
	}{
		{time.Sunday, "2026-09-09T12:00:00-03:00", "2026-09-06T01:00:00-03:00"},
		{time.Sunday, "2026-09-06T10:00:00-03:00", "2026-09-06T01:00:00-03:00"},
		{time.Sunday, "2026-09-05T23:30:00-04:00", "2026-08-30T00:00:00-04:00"},
		{time.Monday, "2026-09-06T10:00:00-03:00", "2026-08-31T00:00:00-04:00"},
		{time.Monday, "2026-09-07T00:30:00-03:00", "2026-09-07T00:00:00-03:00"},
	}
	for _, c := range cases {
		in, _ := time.Parse(time.RFC3339, c.in)
		if got := WeekStart(in, loc, c.first).Format(instant); got != c.want {
			t.Errorf("WeekStart(%s, %v) = %s, want %s", c.in, c.first, got, c.want)
		}
	}
}

// NextWeek is the first instant of the day a week later, also when this week's first day
// starts at 01:00 (Lebanon skips 00:00 on Sunday 2026-03-29 and Go resolves it forward).
func TestNextWeekAcrossMidnightGap(t *testing.T) {
	santiago, beirut := load(t, "America/Santiago"), load(t, "Asia/Beirut")
	cases := []struct {
		loc      *time.Location
		in, want string
	}{
		{santiago, "2026-09-02T12:00:00-04:00", "2026-09-06T01:00:00-03:00"},
		{santiago, "2026-09-09T12:00:00-03:00", "2026-09-13T00:00:00-03:00"},
		{beirut, "2026-04-01T12:00:00+03:00", "2026-04-05T00:00:00+03:00"},
	}
	for _, c := range cases {
		in, _ := time.Parse(time.RFC3339, c.in)
		if got := NextWeek(WeekStart(in, c.loc, time.Sunday)).Format(instant); got != c.want {
			t.Errorf("NextWeek(week of %s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRecomputeWeeksAcrossMidnightGap(t *testing.T) {
	loc := load(t, "America/Santiago")
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	at10 := func(day int) int64 { return time.Date(2026, 9, day, 10, 0, 0, 0, loc).Unix() }
	insertIntervals(t, sq, []attribute.Interval{
		{From: at10(5), To: at10(5) + 3600, Project: "phd", RuleID: "0"},
		{From: at10(6), To: at10(6) + 7200, Project: "phd", RuleID: "0"},
		{From: at10(7), To: at10(7) + 1800, Project: "phd", RuleID: "0"},
	})
	now := time.Unix(at10(7)+3600, 0)
	if err := Recompute(sq, loc, time.Sunday, now); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"phd", "2026-09-05", 3600, 3600, now.Unix()},
		{"phd", "2026-09-06", 7200, 9000, now.Unix()},
		{"phd", "2026-09-07", 1800, 9000, now.Unix()},
	}
	if got := readTotals(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// Israel leaves DST on Sunday 2026-10-25 at 02:00 (clocks back to 01:00): the week
// boundary is 00:00 IDT (21:00Z on the 24th) and the week is 169 hours long.
func TestDSTEndSunday2026(t *testing.T) {
	loc := jerusalem(t)
	sunday := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	if got := sunday.Unix(); got != 1792875600 {
		t.Fatalf("2026-10-25 00:00 local = %d, want 1792875600 (2026-10-24T21:00Z)", got)
	}
	if got := WeekStart(time.Unix(1792890000, 0), loc, time.Sunday); !got.Equal(sunday) { // Sun 03:00 IST
		t.Errorf("WeekStart(Sun 03:00 IST) = %v", got)
	}
	if got := WeekStart(time.Unix(1792873800, 0), loc, time.Sunday); got.Format("2006-01-02") != "2026-10-18" { // Sat 23:30 IDT
		t.Errorf("WeekStart(Sat 23:30) = %v", got)
	}
	if got := NextWeek(sunday).Sub(sunday); got != 169*time.Hour {
		t.Errorf("DST-end week length = %v, want 169h", got)
	}
	if got := NextWeek(sunday).Unix(); got != 1793484000 {
		t.Errorf("next Sunday = %d, want 1793484000", got)
	}
	// Sat 23:30 IDT -> Sun 03:00 IST is 4.5 real hours: 30 min on Saturday, 4 h on Sunday.
	got := SplitByDay(1792873800, 1792890000, loc)
	want := []DayPart{{"2026-10-24", 1800}, {"2026-10-25", 14400}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SplitByDay across DST end = %+v, want %+v", got, want)
	}
}

func TestSplitByDay(t *testing.T) {
	loc := jerusalem(t)
	d16 := time.Date(2026, 9, 16, 0, 0, 0, 0, loc).Unix()
	cases := []struct {
		name     string
		from, to int64
		want     []DayPart
	}{
		{"inside one day", d16 + 3600, d16 + 7200, []DayPart{{"2026-09-16", 3600}}},
		{"across midnight", d16 - 600, d16 + 600, []DayPart{{"2026-09-15", 600}, {"2026-09-16", 600}}},
		{"two full days", d16, d16 + 2*86400, []DayPart{{"2026-09-16", 86400}, {"2026-09-17", 86400}}},
		{"empty", d16, d16, nil},
	}
	for _, c := range cases {
		if got := SplitByDay(c.from, c.to, loc); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

// Cuba springs forward at local midnight (Sun 2026-03-08 00:00 CST -> 01:00 CDT), so
// 00:00 on the 8th does not exist and time.Date resolves it to 23:00 on the 7th: the
// 8th starts at the transition instant, and the split must not stall on the 7th.
func TestSplitByDayMidnightDSTGap(t *testing.T) {
	loc, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Fatal(err)
	}
	// Verify the premise against the zone data rather than the comment.
	transition := time.Date(2026, 3, 8, 1, 0, 0, 0, loc) // first instant of the 8th
	before := transition.Add(-time.Second)
	if _, o1 := before.Zone(); o1 != -5*3600 {
		t.Fatalf("2026-03-07 23:59:59 America/Havana offset = %d, want -18000 (CST)", o1)
	}
	if _, o2 := transition.Zone(); o2 != -4*3600 {
		t.Fatalf("2026-03-08 01:00 America/Havana offset = %d, want -14400 (CDT)", o2)
	}
	if got := time.Date(2026, 3, 8, 0, 0, 0, 0, loc); !got.Before(transition) || got.Day() != 7 {
		t.Fatalf("time.Date(2026-03-08 00:00) = %v, expected a resolution on the 7th", got)
	}
	sat22 := time.Date(2026, 3, 7, 22, 0, 0, 0, loc).Unix()
	mon := time.Date(2026, 3, 9, 0, 0, 0, 0, loc).Unix()
	cases := []struct {
		name     string
		from, to int64
		want     []DayPart
	}{
		{"across the gap", sat22, transition.Unix() + 3600, []DayPart{{"2026-03-07", 7200}, {"2026-03-08", 3600}}},
		{"ending at the transition", sat22, transition.Unix(), []DayPart{{"2026-03-07", 7200}}},
		{"starting at the transition", transition.Unix(), transition.Unix() + 600, []DayPart{{"2026-03-08", 600}}},
		{"the 23-hour day", transition.Unix(), mon, []DayPart{{"2026-03-08", 23 * 3600}}},
	}
	for _, c := range cases {
		if got := SplitByDay(c.from, c.to, loc); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

func TestRecomputeWritesParentSubRowsAndWeeks(t *testing.T) {
	loc := jerusalem(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	sun := time.Date(2026, 9, 13, 9, 0, 0, 0, loc).Unix() // Sunday 09:00
	wed := time.Date(2026, 9, 16, 9, 0, 0, 0, loc).Unix()
	prevSat := time.Date(2026, 9, 12, 9, 0, 0, 0, loc).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: prevSat, To: prevSat + 3600, Project: "acme", Subproject: "a", RuleID: "0"},
		{From: sun, To: sun + 3600, Project: "acme", Subproject: "a", RuleID: "0"},
		{From: sun + 3600, To: sun + 5400, Project: "acme", Subproject: "b", RuleID: "0"},
		{From: wed, To: wed + 7200, Project: "acme", RuleID: "1"},
		{From: wed + 7200, To: wed + 7800, Project: "phd", RuleID: "2"},
	})
	now := time.Unix(wed+8000, 0)
	for i := 0; i < 2; i++ { // idempotent
		if err := Recompute(sq, loc, time.Sunday, now); err != nil {
			t.Fatal(err)
		}
	}
	want := []row{
		{"acme", "2026-09-16", 7200, 7200, now.Unix()},
		{"acme/a", "2026-09-12", 3600, 3600, now.Unix()},
		{"acme/a", "2026-09-13", 3600, 3600, now.Unix()},
		{"acme/b", "2026-09-13", 1800, 1800, now.Unix()},
		{"phd", "2026-09-16", 600, 600, now.Unix()},
	}
	if got := readTotals(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	wtd, err := WeekToDate(sq, WeekStart(now, loc, time.Sunday), loc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wtd, map[string]int64{"acme": 12600, "phd": 600}) {
		t.Fatalf("WeekToDate = %+v", wtd)
	}
}

func TestRecomputeWeeksStartOnFirstWeekday(t *testing.T) {
	loc := jerusalem(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	sun := time.Date(2026, 9, 13, 9, 0, 0, 0, loc).Unix()
	mon := time.Date(2026, 9, 14, 9, 0, 0, 0, loc).Unix()
	wed := time.Date(2026, 9, 16, 9, 0, 0, 0, loc).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: sun, To: sun + 3600, Project: "phd", RuleID: "0"},
		{From: mon, To: mon + 7200, Project: "phd", RuleID: "0"},
		{From: wed, To: wed + 1800, Project: "phd", RuleID: "0"},
	})
	now := time.Unix(wed+3600, 0)
	if err := Recompute(sq, loc, time.Monday, now); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"phd", "2026-09-13", 3600, 3600, now.Unix()}, // Sunday closes the week that began Monday the 7th
		{"phd", "2026-09-14", 7200, 9000, now.Unix()},
		{"phd", "2026-09-16", 1800, 9000, now.Unix()},
	}
	if got := readTotals(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if v, ok, err := db.GetMeta(sq, metaWeekStart); err != nil || !ok || v != "monday" {
		t.Fatalf("meta.week_start = %q (present %v, err %v), want \"monday\"", v, ok, err)
	}
}

type row struct {
	project, day           string
	day_s, week_s, updated int64
}

// insertIntervals commits ivs in one transaction.
func insertIntervals(t *testing.T, sq *sql.DB, ivs []attribute.Interval) {
	t.Helper()
	tx, err := sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertIntervals(tx, ivs); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// readTotals returns every totals row ordered by (project, day).
func readTotals(t *testing.T, sq *sql.DB) []row {
	t.Helper()
	rows, err := sq.Query(`SELECT project, day, day_seconds, week_seconds, updated_at FROM totals ORDER BY project, day`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.project, &r.day, &r.day_s, &r.week_s, &r.updated); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// Israel enters DST on Friday 2026-03-27 at 02:00 IST (clocks forward to 03:00 IDT): the
// week of Sunday 2026-03-22 is 167 hours long and Friday has 23 hours.
func TestDSTStartFriday2026(t *testing.T) {
	loc := jerusalem(t)
	gap := time.Date(2026, 3, 27, 3, 0, 0, 0, loc)
	if _, off := gap.Add(-time.Second).Zone(); off != 2*3600 {
		t.Fatalf("2026-03-27 02:59:59 offset = %d, want 7200 (IST)", off)
	}
	if _, off := gap.Zone(); off != 3*3600 {
		t.Fatalf("2026-03-27 03:00 offset = %d, want 10800 (IDT)", off)
	}
	if got := time.Date(2026, 3, 27, 2, 30, 0, 0, loc); !got.Equal(gap.Add(30*time.Minute)) && !got.Equal(gap.Add(-30*time.Minute)) {
		t.Fatalf("02:30 on 2026-03-27 resolved to %v; expected it to fall in the gap", got)
	}
	sunday := time.Date(2026, 3, 22, 0, 0, 0, 0, loc)
	if got := WeekStart(gap, loc, time.Sunday); !got.Equal(sunday) {
		t.Errorf("WeekStart(Fri 03:00 IDT) = %v, want %v", got, sunday)
	}
	if got := NextWeek(sunday).Sub(sunday); got != 167*time.Hour {
		t.Errorf("DST-start week length = %v, want 167h", got)
	}
	if got := NextWeek(sunday).Format("2006-01-02 15:04 MST"); got != "2026-03-29 00:00 IDT" {
		t.Errorf("next Sunday = %s, want 2026-03-29 00:00 IDT", got)
	}
	// Thu 23:00 IST -> Fri 04:00 IDT is 4 real hours: 1 h on Thursday, 3 h on Friday.
	thu23 := time.Date(2026, 3, 26, 23, 0, 0, 0, loc).Unix()
	got := SplitByDay(thu23, gap.Unix()+3600, loc)
	want := []DayPart{{"2026-03-26", 3600}, {"2026-03-27", 10800}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SplitByDay across DST start = %+v, want %+v", got, want)
	}
	fri := time.Date(2026, 3, 27, 0, 0, 0, 0, loc).Unix()
	sat := time.Date(2026, 3, 28, 0, 0, 0, 0, loc).Unix()
	if got := SplitByDay(fri, sat, loc); !reflect.DeepEqual(got, []DayPart{{"2026-03-27", 23 * 3600}}) {
		t.Errorf("the 23-hour Friday = %+v", got)
	}
}

// An interval that crosses local midnight lands in both day rows; crossing the
// Saturday/Sunday midnight also splits the week totals.
func TestRecomputeSplitsMidnightCrossingIntervals(t *testing.T) {
	loc := jerusalem(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	wedMidnight := time.Date(2026, 9, 16, 0, 0, 0, 0, loc).Unix() // Tue 15 -> Wed 16, same week
	sunMidnight := time.Date(2026, 9, 20, 0, 0, 0, 0, loc).Unix() // Sat 19 -> Sun 20, new week
	insertIntervals(t, sq, []attribute.Interval{
		{From: wedMidnight - 1800, To: wedMidnight + 600, Project: "phd", RuleID: "0"},
		{From: sunMidnight - 3600, To: sunMidnight + 900, Project: "phd", RuleID: "0"},
		{From: sunMidnight + 86400, To: sunMidnight + 86400 + 300, Project: "phd", RuleID: "0"}, // Mon 21
	})
	now := time.Unix(sunMidnight+90000, 0)
	if err := Recompute(sq, loc, time.Sunday, now); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"phd", "2026-09-15", 1800, 1800 + 600 + 3600, now.Unix()},
		{"phd", "2026-09-16", 600, 1800 + 600 + 3600, now.Unix()},
		{"phd", "2026-09-19", 3600, 1800 + 600 + 3600, now.Unix()},
		{"phd", "2026-09-20", 900, 900 + 300, now.Unix()},
		{"phd", "2026-09-21", 300, 900 + 300, now.Unix()},
	}
	if got := readTotals(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	wtd, err := WeekToDate(sq, WeekStart(now, loc, time.Sunday), loc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wtd, map[string]int64{"phd": 1200}) {
		t.Fatalf("WeekToDate = %+v", wtd)
	}
}

// Recompute rebuilds from scratch: rows whose intervals were deleted disappear, and
// surviving rows carry the new updated_at.
func TestRecomputeDropsRowsForDeletedIntervals(t *testing.T) {
	loc := jerusalem(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	mon := time.Date(2026, 9, 14, 9, 0, 0, 0, loc).Unix()
	tue := time.Date(2026, 9, 15, 9, 0, 0, 0, loc).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: mon, To: mon + 3600, Project: "acme", RuleID: "0"},
		{From: tue, To: tue + 1800, Project: "phd", RuleID: "1"},
	})
	first := time.Unix(tue+7200, 0)
	if err := Recompute(sq, loc, time.Sunday, first); err != nil {
		t.Fatal(err)
	}
	if got := readTotals(t, sq); len(got) != 2 {
		t.Fatalf("after first recompute: %+v", got)
	}
	tx, err := sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteIntervals(tx, tue, tue+1800); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	second := first.Add(time.Hour)
	if err := Recompute(sq, loc, time.Sunday, second); err != nil {
		t.Fatal(err)
	}
	want := []row{{"acme", "2026-09-14", 3600, 3600, second.Unix()}}
	if got := readTotals(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("after deletion got %+v\nwant %+v", got, want)
	}
}

func TestBetweenIsHalfOpenAndRollsUpSubprojects(t *testing.T) {
	loc := jerusalem(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	at := func(d int) int64 { return time.Date(2026, 9, d, 9, 0, 0, 0, loc).Unix() }
	insertIntervals(t, sq, []attribute.Interval{
		{From: at(13), To: at(13) + 3600, Project: "acme", Subproject: "a", RuleID: "0"},
		{From: at(14), To: at(14) + 1800, Project: "acme", Subproject: "b", RuleID: "0"},
		{From: at(15), To: at(15) + 600, Project: "phd", RuleID: "1"},
		{From: at(16), To: at(16) + 7200, Project: "acme", RuleID: "0"},
	})
	if err := Recompute(sq, loc, time.Sunday, time.Unix(at(16)+8000, 0)); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	to := time.Date(2026, 9, 16, 0, 0, 0, 0, loc)
	got, err := Between(sq, from, to, loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"acme": 5400, "phd": 600}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Between = %+v, want %+v (the end day is excluded)", got, want)
	}
}
