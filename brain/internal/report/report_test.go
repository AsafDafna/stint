package report

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/totals"
)

// config sets cfg.Location to time.Local; these fixtures are written in Asia/Jerusalem.
func TestMain(m *testing.M) {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		panic(err)
	}
	time.Local = loc
	os.Exit(m.Run())
}

func setup(t *testing.T) (*sql.DB, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	sq, err := db.Create(filepath.Join(dir, "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	cfg, err := config.Parse([]byte("[settings]\nvault_reports_dir = \""+filepath.Join(dir, "reports")+"\"\n[projects.acme]\n[[rules]]\nproject = \"acme\"\napp = \"A\"\n"), "/h")
	if err != nil {
		t.Fatal(err)
	}
	return sq, cfg
}

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

func insertSample(t *testing.T, sq *sql.DB, s attribute.Sample) {
	t.Helper()
	if _, err := db.InsertSample(sq, s); err != nil {
		t.Fatal(err)
	}
}

// pinMtime stamps the week's file as written at `at`, so the final-file rule is tested
// against a fixed clock rather than the real one.
func pinMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

var params = attribute.Params{GapSeconds: 120, IdleSeconds: 300, CarryoverSeconds: 600, CallApps: map[string]bool{}}

func TestRank(t *testing.T) {
	samples := []attribute.Sample{
		{TS: 0, Kind: "sample", App: "X", URL: "https://a"},
		{TS: 60, Kind: "sample", App: "X", Title: "Doc"},
		{TS: 120, Kind: "sample", App: "X", Title: "Doc"},
		{TS: 180, Kind: "sample", App: "A"},
		{TS: 240, Kind: "sample", App: "X", IdleS: 30},
	}
	p := params
	p.Now = 300
	un := []attribute.Interval{{From: 30, To: 180, Project: attribute.Unattributed, RuleID: attribute.RuleUnattributed}}
	got := rank(samples, attribute.Coverage(samples, p), un, 10)
	want := []Entry{{"Doc", 120}, {"https://a", 30}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got := rank(samples, attribute.Coverage(samples, p), un, 1); len(got) != 1 || got[0].Label != "Doc" {
		t.Fatalf("top-1 = %+v", got)
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		in   attribute.Sample
		want string
	}{
		{attribute.Sample{URL: "u", Title: "t", App: "a"}, "u"},
		{attribute.Sample{Title: "t", App: "a"}, "t"},
		{attribute.Sample{App: "a"}, "a"},
		{attribute.Sample{}, "(no title)"},
	}
	for _, c := range cases {
		if got := Label(c.in); got != c.want {
			t.Errorf("Label(%+v) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestFold(t *testing.T) {
	known := map[string]bool{
		"acme-app": true, "acme-app-pr12": true, "automations": true,
		"data-exports": true, "a": true, "a-b": true, "a-b-c": true,
	}
	cases := []struct{ in, want string }{
		{"acme-app-pr12", "acme-app"},
		{"acme-app-email-export", "acme-app"},
		{"acme-app-pr12-fix", "acme-app"}, // longest match is the worktree acme-app-pr12, which folds again
		{"a-b-c-d", "a"},
		{"acme-app", "acme-app"},
		{"data-exports", "data-exports"}, // no "data" sub-project
		{"automations", "automations"},
		{"acme-application", "acme-application"}, // a prefix only counts up to a "-"
		{"", ""},
	}
	for _, c := range cases {
		if got := fold(c.in, known); got != c.want {
			t.Errorf("fold(%q) = %q want %q", c.in, got, c.want)
		}
	}
	if got := fold("acme-app-pr12", nil); got != "acme-app-pr12" {
		t.Errorf("no known sub-projects: got %q", got)
	}
}

func TestCapStatus(t *testing.T) {
	h := int64(3600)
	cases := []struct {
		sec  int64
		cap  float64
		want string
	}{
		{337 * 360, 30, "target 30 · over by 3.7"},
		{20 * h, 30, "target 30 · short by 10.0"},
		{0, 10, "target 10 · short by 10.0"},
		{30*h - 170, 30, "target 30 · met"},
		{30*h + 170, 30, "target 30 · met"},
		{30*h + 200, 30, "target 30 · over by 0.1"},
		{27 * h, 27.5, "target 27.5 · short by 0.5"},
	}
	for _, c := range cases {
		if got := capStatus(c.sec, c.cap); got != c.want {
			t.Errorf("capStatus(%d, %g) = %q want %q", c.sec, c.cap, got, c.want)
		}
	}
}

func TestWeekFileUsesISOWeekOfMonday(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Jerusalem")
	sunday := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	if got := WeekFile("/r", sunday); got != "/r/2026-W38.md" {
		t.Fatalf("got %s", got)
	}
	if got := WeekFile("/r", time.Date(2026, 12, 27, 0, 0, 0, 0, loc)); got != "/r/2026-W53.md" {
		t.Fatalf("year-end week: got %s", got)
	}
	// ISO-year rollover: the week of Sun 2025-12-28 holds Mon 2025-12-29, which is 2026-W01.
	if got := WeekFile("/r", time.Date(2025, 12, 28, 0, 0, 0, 0, loc)); got != "/r/2026-W01.md" {
		t.Fatalf("ISO-year rollover: got %s", got)
	}
	for _, c := range []struct {
		start time.Time
		want  string
	}{
		{time.Date(2026, 9, 14, 0, 0, 0, 0, loc), "/r/2026-W38.md"}, // Monday start
		{time.Date(2026, 9, 12, 0, 0, 0, 0, loc), "/r/2026-W38.md"}, // Saturday start: Sat 12 - Fri 18 holds Mon 14
		{time.Date(2026, 9, 15, 0, 0, 0, 0, loc), "/r/2026-W39.md"}, // Tuesday start: Tue 15 - Mon 21 holds Mon 21
	} {
		if got := WeekFile("/r", c.start); got != c.want {
			t.Errorf("week from %s %s: got %s want %s", c.start.Weekday(), c.start.Format("2006-01-02"), got, c.want)
		}
	}
}

// fence swaps each triple-quote placeholder for a code fence, which a raw string cannot hold.
func fence(s string) string { return strings.ReplaceAll(s, "'''", "```") }

// TestWeeklyPage: a W40-shaped week. Worktree sub-projects fold into the one they were cut
// from (garden is known only from the week before), a sub-project and a project whose
// time rounds to 0.0 drop out, (no project) is the parent's time outside sub-projects,
// and only the top 5 unattributed titles are listed.
func TestWeeklyPage(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	cfg.Projects["acme"] = config.Project{WeeklyCapHours: 30}
	sun := time.Date(2026, 9, 27, 0, 0, 0, 0, loc)
	var ivs []attribute.Interval
	cursor := map[int]int64{} // day offset from sun -> next free second
	block := func(day int, secs int64, project, sub string) int64 {
		if cursor[day] == 0 {
			cursor[day] = sun.AddDate(0, 0, day).Add(8 * time.Hour).Unix()
		}
		from := cursor[day]
		cursor[day] += secs + 600
		rule := "1"
		if project == attribute.Unattributed {
			rule = attribute.RuleUnattributed
		}
		ivs = append(ivs, attribute.Interval{From: from, To: from + secs, Project: project, Subproject: sub, RuleID: rule})
		return from
	}
	for _, r := range []struct {
		project, sub string
		tenths       [7]int64 // per day, Sunday first
	}{
		{"acme", "acme-app", [7]int64{17, 43, 33, 7, 53, 30, 11}},
		{"acme", "acme-app-pr12", [7]int64{2: 2}},
		{"acme", "acme-app-email-export", [7]int64{5: 8}},
		{"acme", "acme-app-security-scan", [7]int64{5: 1}},
		{"acme", "automations", [7]int64{5: 8, 6: 3}},
		{"acme", "", [7]int64{2, 21, 24, 24, 19, 24, 7}},
		{"personal-dev", "notebooks", [7]int64{4, 6, 0, 15, 2}},
		{"personal-dev", "photo-sorter", [7]int64{1: 1, 3: 14}},
		{"personal-dev", "garden-fix", [7]int64{1: 2, 3: 1, 5: 1}},
		{"personal-dev", "", [7]int64{34, 4, 2, 18, 2, 2}},
	} {
		for day, n := range r.tenths {
			if n > 0 {
				block(day, n*360, r.project, r.sub)
			}
		}
	}
	block(3, 60, "personal-dev", "data-exports")
	block(4, 100, "phd", "")
	for _, u := range []struct {
		day   int
		secs  int64
		title string
	}{
		{1, 400, "Café notes: meeting prep"},
		{2, 300, "dev-projects"},
		{4, 240, "Login Items"},
		{4, 220, "WhatsApp"},
		{4, 210, "com.apple.systempreferences"},
		{4, 200, "Meeting Recorder"},
		{4, 40, "Phone"},
	} {
		from := block(u.day, u.secs, attribute.Unattributed, "")
		for ts := from; ts < from+u.secs; ts += 60 {
			insertSample(t, sq, attribute.Sample{TS: ts, Kind: "heartbeat", App: "X", Title: u.title})
		}
	}
	// W39: 34.2 h, holding the only garden time.
	for day := -7; day <= -4; day++ {
		block(day, 8*3600, "acme", "")
	}
	block(-3, 17*360, "acme", "")
	block(-6, 5*360, "personal-dev", "garden")
	insertIntervals(t, sq, ivs)
	now := sun.AddDate(0, 0, 7)
	if err := totals.Recompute(sq, loc, cfg.FirstWeekday, now); err != nil {
		t.Fatal(err)
	}
	p := params
	p.Now = now.Unix()
	md, err := Weekly(sq, cfg, p, sun)
	if err != nil {
		t.Fatal(err)
	}
	want := fence(`# Week 40 · Sep 27 – Oct 3 · 45.0h (W39 34.2h, +10.8)

'''
acme           33.7h  target 30 · over by 3.7
personal-dev   10.8h
unattributed    0.4h

                 Sun  Mon  Tue  Wed  Thu  Fri  Sat  Total
acme             1.9  6.4  5.9  3.1  7.2  7.1  2.1   33.7
  acme-app       1.7  4.3  3.5  0.7  5.3  3.9  1.1   20.5
  automations                             0.8  0.3    1.1
  (no project)   0.2  2.1  2.4  2.4  1.9  2.4  0.7   12.1
personal-dev     3.8  1.3  0.2  4.8  0.4  0.3        10.8
  notebooks      0.4  0.6       1.5  0.2              2.7
  photo-sorter        0.1       1.4                   1.5
  garden              0.2       0.1       0.1         0.4
  (no project)   3.4  0.4  0.2  1.8  0.2  0.2         6.2
unattributed          0.1  0.1       0.3              0.4
Total            5.7  7.8  6.2  7.9  7.9  7.4  2.1   45.0
'''

## Top unattributed

'''
0.1  Café notes: meeting prep
0.1  dev-projects
0.1  Login Items
0.1  WhatsApp
0.1  com.apple.systempreferences
'''
`)
	if md != want {
		t.Errorf("got:\n%s\nwant:\n%s", md, want)
	}
}

// TestWeeklyQuietWeek: a capped project with no time still shows; an interval straddling
// the week start counts on both sides; unattributed time and titles that round to 0.0
// drop out, and with no title left the section is omitted.
func TestWeeklyQuietWeek(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	cfg.Projects["acme"] = config.Project{WeeklyCapHours: 10}
	sun := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	mon9 := sun.Add(24*time.Hour + 9*time.Hour).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: sun.Unix() - 6*86400 + 9*3600, To: sun.Unix() - 6*86400 + 9*3600 + 9000, Project: "personal-dev", RuleID: "1"},
		{From: sun.Unix() - 1800, To: sun.Unix() + 1800, Project: "personal-dev", RuleID: "1"},
		{From: mon9, To: mon9 + 3600, Project: "personal-dev", RuleID: "1"},
		{From: mon9 + 3600, To: mon9 + 3640, Project: attribute.Unattributed, RuleID: attribute.RuleUnattributed},
	})
	insertSample(t, sq, attribute.Sample{TS: mon9 + 3600, Kind: "heartbeat", App: "X", Title: "Phone"})
	p := params
	p.Now = mon9 + 20000
	if err := totals.Recompute(sq, loc, cfg.FirstWeekday, time.Unix(p.Now, 0)); err != nil {
		t.Fatal(err)
	}
	md, err := Weekly(sq, cfg, p, sun)
	if err != nil {
		t.Fatal(err)
	}
	want := fence(`# Week 38 · Sep 13 – Sep 19 · 1.5h (W37 3.0h, -1.5)

'''
personal-dev   1.5h
acme           0.0h  target 10 · short by 10.0

               Sun  Mon  Tue  Wed  Thu  Fri  Sat  Total
personal-dev   0.5  1.0                             1.5
acme                                                0.0
Total          0.5  1.0                             1.5
'''
`)
	if md != want {
		t.Errorf("got:\n%s\nwant:\n%s", md, want)
	}
}

// TestWeeklyMondayStart: with week_start = monday the page covers Mon-Sun with the columns in
// that order, Sunday the 13th counts in the previous week, and the file is named after the week.
func TestWeeklyMondayStart(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	cfg.FirstWeekday = time.Monday
	at9 := func(day int) int64 { return time.Date(2026, 9, day, 9, 0, 0, 0, loc).Unix() }
	insertIntervals(t, sq, []attribute.Interval{
		{From: at9(8), To: at9(8) + 7200, Project: "personal-dev", RuleID: "1"},
		{From: at9(13), To: at9(13) + 3600, Project: "personal-dev", RuleID: "1"},
		{From: at9(14), To: at9(14) + 3600, Project: "personal-dev", RuleID: "1"},
		{From: at9(20), To: at9(20) + 1800, Project: "acme", RuleID: "1"},
	})
	p := params
	p.Now = at9(21)
	if err := totals.Recompute(sq, loc, cfg.FirstWeekday, time.Unix(p.Now, 0)); err != nil {
		t.Fatal(err)
	}
	path, md, err := Write(sq, cfg, p, time.Date(2026, 9, 16, 12, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	want := fence(`# Week 38 · Sep 14 – Sep 20 · 1.5h (W37 3.0h, -1.5)

'''
personal-dev   1.0h
acme           0.5h

               Mon  Tue  Wed  Thu  Fri  Sat  Sun  Total
personal-dev   1.0                                  1.0
acme                                         0.5    0.5
Total          1.0                           0.5    1.5
'''
`)
	if md != want {
		t.Errorf("got:\n%s\nwant:\n%s", md, want)
	}
	if wantPath := filepath.Join(cfg.Settings.VaultReportsDir, "2026-W38.md"); path != wantPath {
		t.Errorf("path = %s want %s", path, wantPath)
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

// tenthsPerDay books day d of the month as d*6 minutes at 10:00, for every day in [from, to),
// so each column of a page holds a different value.
func tenthsPerDay(from, to time.Time) []attribute.Interval {
	var ivs []attribute.Interval
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		at := time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, d.Location()).Unix()
		ivs = append(ivs, attribute.Interval{From: at, To: at + int64(d.Day())*360, Project: "acme", RuleID: "1"})
	}
	return ivs
}

// Chile skips 00:00 on Sunday 2026-09-06 and Go's time.Date resolves that midnight to 23:00 on
// Saturday. The page must still put Sunday the 6th in its own column of the right week.
func TestWeeklyAcrossMidnightGap(t *testing.T) {
	loc := load(t, "America/Santiago")
	cases := []struct {
		name  string
		first time.Weekday
		at    time.Time
		want  string
	}{
		{"sunday start: the gap day opens the week", time.Sunday, time.Date(2026, 9, 9, 12, 0, 0, 0, loc), fence(`# Week 37 · Sep 6 – Sep 12 · 6.3h (W36 7.6h, -1.3)

'''
acme   6.3h

        Sun  Mon  Tue  Wed  Thu  Fri  Sat  Total
acme    0.6  0.7  0.8  0.9  1.0  1.1  1.2    6.3
Total   0.6  0.7  0.8  0.9  1.0  1.1  1.2    6.3
'''
`)},
		{"monday start: the gap day closes the week", time.Monday, time.Date(2026, 9, 2, 12, 0, 0, 0, loc), fence(`# Week 36 · Aug 31 – Sep 6 · 5.2h (W35 18.9h, -13.7)

'''
acme   5.2h

        Mon  Tue  Wed  Thu  Fri  Sat  Sun  Total
acme    3.1  0.1  0.2  0.3  0.4  0.5  0.6    5.2
Total   3.1  0.1  0.2  0.3  0.4  0.5  0.6    5.2
'''
`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sq, cfg := setup(t)
			cfg.Location, cfg.FirstWeekday = loc, c.first
			insertIntervals(t, sq, tenthsPerDay(time.Date(2026, 8, 24, 12, 0, 0, 0, loc), time.Date(2026, 9, 13, 12, 0, 0, 0, loc)))
			now := time.Date(2026, 9, 20, 12, 0, 0, 0, loc)
			if err := totals.Recompute(sq, loc, c.first, now); err != nil {
				t.Fatal(err)
			}
			p := params
			p.Now = now.Unix()
			md, err := Weekly(sq, cfg, p, c.at)
			if err != nil {
				t.Fatal(err)
			}
			if md != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", md, c.want)
			}
		})
	}
}

// The catch-up steps from week to week across Chile's skipped midnight and writes each week once.
func TestWriteMissingAcrossMidnightGap(t *testing.T) {
	loc := load(t, "America/Santiago")
	sq, cfg := setup(t)
	cfg.Location = loc
	var ivs []attribute.Interval
	for d := time.Date(2026, 8, 23, 10, 0, 0, 0, loc); d.Before(time.Date(2026, 9, 20, 12, 0, 0, 0, loc)); d = d.AddDate(0, 0, 1) {
		ivs = append(ivs, attribute.Interval{From: d.Unix(), To: d.Unix() + 3600, Project: "acme", RuleID: "1"})
	}
	insertIntervals(t, sq, ivs)
	insertSample(t, sq, attribute.Sample{TS: ivs[0].From, Kind: "heartbeat", App: "A"})
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, loc)
	if err := totals.Recompute(sq, loc, cfg.FirstWeekday, now); err != nil {
		t.Fatal(err)
	}
	written, err := WriteMissing(sq, cfg, params, now)
	if err != nil {
		t.Fatal(err)
	}
	dir := cfg.Settings.VaultReportsDir
	want := []struct{ file, heading string }{
		{"2026-W35.md", "# Week 35 · Aug 23 – Aug 29 · 7.0h"},
		{"2026-W36.md", "# Week 36 · Aug 30 – Sep 5 · 7.0h (W35 7.0h, +0.0)"},
		{"2026-W37.md", "# Week 37 · Sep 6 – Sep 12 · 7.0h (W36 7.0h, +0.0)"},
		{"2026-W38.md", "# Week 38 · Sep 13 – Sep 19 · 7.0h (W37 7.0h, +0.0)"},
	}
	var wantPaths []string
	for _, w := range want {
		wantPaths = append(wantPaths, filepath.Join(dir, w.file))
	}
	if !reflect.DeepEqual(written, wantPaths) {
		t.Fatalf("written = %v\nwant %v", written, wantPaths)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.md")); !reflect.DeepEqual(files, wantPaths) {
		t.Fatalf("files on disk = %v\nwant %v", files, wantPaths)
	}
	for _, w := range want {
		b, err := os.ReadFile(filepath.Join(dir, w.file))
		if err != nil {
			t.Fatal(err)
		}
		if got, _, _ := strings.Cut(string(b), "\n"); got != w.heading {
			t.Errorf("%s: heading %q, want %q", w.file, got, w.heading)
		}
	}
}

// Lebanon skips 00:00 on Sunday 2026-03-29 and Go resolves it forward to 01:00, so that week
// starts at 01:00. It still ends at 00:00 on the next Sunday: 00:10-00:50 on Apr 5 is next week's.
func TestWeeklyGapWeekEndsAtNextMidnight(t *testing.T) {
	loc := load(t, "Asia/Beirut")
	sq, cfg := setup(t)
	cfg.Location = loc
	sun := time.Date(2026, 3, 29, 10, 0, 0, 0, loc).Unix()
	next := time.Date(2026, 4, 5, 0, 10, 0, 0, loc).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: sun, To: sun + 3600, Project: "acme", RuleID: "1"},
		{From: next, To: next + 2400, Project: "acme", RuleID: "1"},
	})
	now := time.Date(2026, 4, 6, 12, 0, 0, 0, loc)
	if err := totals.Recompute(sq, loc, cfg.FirstWeekday, now); err != nil {
		t.Fatal(err)
	}
	p := params
	p.Now = now.Unix()
	md, err := Weekly(sq, cfg, p, time.Date(2026, 4, 1, 12, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	want := fence(`# Week 14 · Mar 29 – Apr 4 · 1.0h

'''
acme   1.0h

        Sun  Mon  Tue  Wed  Thu  Fri  Sat  Total
acme    1.0                                  1.0
Total   1.0                                  1.0
'''
`)
	if md != want {
		t.Errorf("got:\n%s\nwant:\n%s", md, want)
	}
}

// TestWeeklyHidesPreviousWeekBelowRounding: a previous week that prints as 0.0h is not
// compared against.
func TestWeeklyHidesPreviousWeekBelowRounding(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	sun := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	mon9 := sun.Add(24*time.Hour + 9*time.Hour).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: mon9 - 7*86400, To: mon9 - 7*86400 + 170, Project: "acme", RuleID: "1"},
		{From: mon9, To: mon9 + 3600, Project: "acme", RuleID: "1"},
	})
	p := params
	p.Now = mon9 + 20000
	if err := totals.Recompute(sq, loc, cfg.FirstWeekday, time.Unix(p.Now, 0)); err != nil {
		t.Fatal(err)
	}
	md, err := Weekly(sq, cfg, p, sun)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Week 38 · Sep 13 – Sep 19 · 1.0h\n"; !strings.HasPrefix(md, want) {
		t.Errorf("want heading %q, got:\n%s", want, md)
	}
}

// TestWeeklyLabelsStayOnOneLine: a title or sub-project holding a line break must not
// break the aligned block.
func TestWeeklyLabelsStayOnOneLine(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	sun := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	mon9 := sun.Add(24*time.Hour + 9*time.Hour).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: mon9, To: mon9 + 3600, Project: "acme", Subproject: "a|b\nc", RuleID: "0"},
		{From: mon9 + 3600, To: mon9 + 5400, Project: attribute.Unattributed, RuleID: attribute.RuleUnattributed},
	})
	for ts := mon9 + 3600; ts < mon9+5400; ts += 60 {
		insertSample(t, sq, attribute.Sample{TS: ts, Kind: "heartbeat", App: "X", Title: "Bad | title\r\nline two"})
	}
	p := params
	p.Now = mon9 + 20000
	md, err := Weekly(sq, cfg, p, sun)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n  a|b c ", "\n0.5  Bad | title line two\n"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

// TestWeeklyNormalizesWeekStart: callers may pass the week start in another Location or
// off midnight; the report must be the one for the local Sunday 00:00 week, and the file
// must be named after it (a UTC Sunday 00:00 Jerusalem is Saturday 21:00 UTC, whose ISO
// week is the previous one).
func TestWeeklyNormalizesWeekStart(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	sun := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	mon9 := sun.Add(24*time.Hour + 9*time.Hour).Unix()
	insertIntervals(t, sq, []attribute.Interval{{From: mon9, To: mon9 + 3600, Project: "acme", RuleID: "1"}})
	p := params
	p.Now = mon9 + 20000
	want, err := Weekly(sq, cfg, p, sun)
	if err != nil {
		t.Fatal(err)
	}
	for name, ws := range map[string]time.Time{
		"utc":          sun.In(time.UTC),
		"off-midnight": sun.Add(5 * time.Hour),
		"mid-week":     sun.AddDate(0, 0, 3).Add(11 * time.Hour),
	} {
		got, err := Weekly(sq, cfg, p, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: Weekly differs:\n%s", name, got)
		}
		path, md, err := Write(sq, cfg, p, ws)
		if err != nil {
			t.Fatal(err)
		}
		if wantPath := WeekFile(cfg.Settings.VaultReportsDir, sun); path != wantPath || md != want {
			t.Errorf("%s: Write = %q (want %q), md equal = %v", name, path, wantPath, md == want)
		}
	}
}

// TestWeeklyDSTWeeks: Israel's clocks move on Fri 2026-03-27 (spring, 167 h week) and
// Sun 2026-10-25 (fall, 169 h week). Days must still be cut at local midnight and the
// grid must keep its seven day columns.
func TestWeeklyDSTWeeks(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	p := params
	p.Now = time.Date(2026, 12, 1, 0, 0, 0, 0, loc).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		// spring: 01:30 → 03:30 spans the missing hour, so it is 1 h of wall time
		{From: time.Date(2026, 3, 27, 1, 30, 0, 0, loc).Unix(), To: time.Date(2026, 3, 27, 3, 30, 0, 0, loc).Unix(), Project: "acme", RuleID: "1"},
		// fall: 00:30 → 03:30 spans the repeated hour, so it is 4 h of wall time
		{From: time.Date(2026, 10, 25, 0, 30, 0, 0, loc).Unix(), To: time.Date(2026, 10, 25, 3, 30, 0, 0, loc).Unix(), Project: "acme", RuleID: "1"},
		// straddles the end of the fall week: only the hour before Sun 00:00 counts
		{From: time.Date(2026, 10, 31, 23, 0, 0, 0, loc).Unix(), To: time.Date(2026, 11, 1, 1, 0, 0, 0, loc).Unix(), Project: "acme", RuleID: "1"},
	})
	cases := []struct {
		weekStart time.Time
		want      []string
	}{
		{time.Date(2026, 3, 22, 0, 0, 0, 0, loc), []string{
			"# Week 13 · Mar 22 – Mar 28 · 1.0h\n",
			"\nacme" + strings.Repeat(" ", 29) + "1.0" + strings.Repeat(" ", 9) + "1.0\n",
		}},
		{time.Date(2026, 10, 25, 0, 0, 0, 0, loc), []string{
			"# Week 44 · Oct 25 – Oct 31 · 5.0h\n",
			"\nacme    4.0" + strings.Repeat(" ", 27) + "1.0    5.0\n",
		}},
	}
	for _, c := range cases {
		md, err := Weekly(sq, cfg, p, c.weekStart)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range c.want {
			if !strings.Contains(md, want) {
				t.Errorf("week %s: missing %q in:\n%s", c.weekStart.Format("2006-01-02"), want, md)
			}
		}
	}
}

func TestWriteMissingCatchesUpOnce(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, loc) // Tuesday in week of Sun 2026-08-30
	insertSample(t, sq, attribute.Sample{TS: first.Unix(), Kind: "sample", App: "A"})
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, loc) // week of 09-13 is in progress
	written, err := WriteMissing(sq, cfg, params, now)
	if err != nil {
		t.Fatal(err)
	}
	w36 := time.Date(2026, 8, 30, 0, 0, 0, 0, loc)
	w37 := totals.NextWeek(w36)
	w38 := totals.NextWeek(w37)
	want := []string{
		filepath.Join(cfg.Settings.VaultReportsDir, "2026-W36.md"),
		filepath.Join(cfg.Settings.VaultReportsDir, "2026-W37.md"),
	}
	if !reflect.DeepEqual(written, want) {
		t.Fatalf("written %v want %v", written, want)
	}
	if _, err := os.Stat(want[1]); err != nil {
		t.Fatal(err)
	}
	// A file stamped exactly at its week's end is final.
	pinMtime(t, want[0], totals.NextWeek(w36))
	pinMtime(t, want[1], totals.NextWeek(w37))
	if again, err := WriteMissing(sq, cfg, params, now); err != nil || len(again) != 0 {
		t.Fatalf("second run wrote %v err=%v", again, err)
	}
	// One second before the week's end it is a partial week and is rewritten.
	pinMtime(t, want[1], totals.NextWeek(w37).Add(-time.Second))
	if again, err := WriteMissing(sq, cfg, params, now); err != nil || !reflect.DeepEqual(again, want[1:]) {
		t.Fatalf("stale W37 run wrote %v err=%v", again, err)
	}
	pinMtime(t, want[1], totals.NextWeek(w37))
	// The first run after the week ends writes exactly that week.
	later := time.Date(2026, 9, 20, 0, 5, 0, 0, loc)
	w38Path := filepath.Join(cfg.Settings.VaultReportsDir, "2026-W38.md")
	if again, err := WriteMissing(sq, cfg, params, later); err != nil || !reflect.DeepEqual(again, []string{w38Path}) {
		t.Fatalf("after week end wrote %v err=%v", again, err)
	}
	pinMtime(t, w38Path, totals.NextWeek(w38))
	// WriteWeeks overwrites regardless of existing files (recompute path).
	forced, err := WriteWeeks(sq, cfg, params, time.Date(2026, 9, 8, 0, 0, 0, 0, loc), later)
	if err != nil || !reflect.DeepEqual(forced, []string{want[1], w38Path}) {
		t.Fatalf("WriteWeeks = %v err=%v", forced, err)
	}
}

func TestTopUnattributedClipsToWindow(t *testing.T) {
	sq, _ := setup(t)
	insertIntervals(t, sq, []attribute.Interval{{From: 0, To: 10000, Project: attribute.Unattributed, RuleID: attribute.RuleUnattributed}})
	for ts := int64(0); ts < 10000; ts += 60 {
		insertSample(t, sq, attribute.Sample{TS: ts, Kind: "heartbeat", App: "X", Title: "Doc"})
	}
	p := params
	p.Now = 10000
	got, err := TopUnattributed(sq, p, 1000, 2000, 5)
	if err != nil || len(got) != 1 || got[0].Seconds != 1000 {
		t.Fatalf("window [1000,2000) must credit exactly 1000 s, got %+v err=%v", got, err)
	}
	// A window starting mid-span: the straddling sample (ts 960) contributes its part after 1000.
	got, err = TopUnattributed(sq, p, 1010, 2000, 5)
	if err != nil || len(got) != 1 || got[0].Seconds != 990 {
		t.Fatalf("window [1010,2000) must credit 990 s, got %+v err=%v", got, err)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	sun := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	mon9 := sun.Add(24*time.Hour + 9*time.Hour).Unix()
	insertIntervals(t, sq, []attribute.Interval{
		{From: mon9, To: mon9 + 3600, Project: "acme", RuleID: "1"},
	})

	path := WeekFile(cfg.Settings.VaultReportsDir, sun)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil { // pre-existing 0-length file
		t.Fatal(err)
	}

	p := params
	p.Now = mon9 + 20000
	gotPath, md, err := Write(sq, cfg, p, sun)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Fatalf("path = %q want %q", gotPath, path)
	}
	// Write returns the markdown it rendered, i.e. Weekly's output for the same week.
	if want, err := Weekly(sq, cfg, p, sun); err != nil || md != want {
		t.Fatalf("returned md differs from Weekly (err=%v):\n%s", err, md)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != md {
		t.Fatalf("file on disk = %q want the returned markdown %q", data, md)
	}
	if len(data) == 0 {
		t.Fatal("file is still empty after Write")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: err=%v", err)
	}
}

// TestWriteMissingRewritesFileWrittenBeforeWeekEnd: `report --week` on a Wednesday freezes
// a partial table in the week's file. The Sunday catch-up must replace it, because the
// file's mtime predates the end of the week it covers.
func TestWriteMissingRewritesFileWrittenBeforeWeekEnd(t *testing.T) {
	sq, cfg := setup(t)
	loc := cfg.Location
	sun := time.Date(2026, 9, 13, 0, 0, 0, 0, loc)
	wed := time.Date(2026, 9, 16, 10, 0, 0, 0, loc)
	insertSample(t, sq, attribute.Sample{TS: wed.Unix(), Kind: "sample", App: "A"})
	path := WeekFile(cfg.Settings.VaultReportsDir, sun)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("partial week\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pinMtime(t, path, wed) // written mid-week

	now := time.Date(2026, 9, 20, 0, 5, 0, 0, loc) // the week is over
	written, err := WriteMissing(sq, cfg, params, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(written, []string{path}) {
		t.Fatalf("written %v, want the stale report %s rewritten", written, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "partial week") || !strings.Contains(string(data), "# Week 38 · ") {
		t.Fatalf("file still holds the mid-week snapshot: %q", data)
	}
	// Stamped at the week's end (the earliest a post-week rewrite can be), the next
	// catch-up leaves it alone.
	pinMtime(t, path, totals.NextWeek(sun))
	if again, err := WriteMissing(sq, cfg, params, now); err != nil || len(again) != 0 {
		t.Fatalf("second catch-up wrote %v err=%v", again, err)
	}
}

func TestPickerFoldsRecentSubprojects(t *testing.T) {
	sq, _ := setup(t)
	cfg, err := config.Parse([]byte("[settings]\nvault_reports_dir = \"/tmp/x\"\n[projects.phd]\n[projects.acme]\n[projects.personal-dev]\n[[rules]]\nproject = \"phd\"\napp = \"A\"\n"), "/h")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, cfg.Location)
	day := func(daysAgo int) int64 { return now.AddDate(0, 0, -daysAgo).Unix() }
	insertIntervals(t, sq, []attribute.Interval{
		{From: day(40), To: day(40) + 600, Project: "acme", Subproject: "acme-app", RuleID: "0"},    // old: known, not recent
		{From: day(3), To: day(3) + 600, Project: "acme", Subproject: "acme-app-pr12", RuleID: "0"}, // recent worktree
		{From: day(2), To: day(2) + 600, Project: "acme", Subproject: "automations", RuleID: "0"},
		{From: day(31), To: day(31) + 600, Project: "acme", Subproject: "data-exports", RuleID: "0"}, // outside 30 days
		{From: day(1), To: day(1) + 600, Project: "personal-dev", Subproject: "budget-tracker", RuleID: "stretch"},
		{From: day(1) + 600, To: day(1) + 1200, Project: "ghost", Subproject: "x", RuleID: "0"}, // not declared
	})
	if err := totals.Recompute(sq, cfg.Location, cfg.FirstWeekday, now); err != nil {
		t.Fatal(err)
	}
	got, err := Picker(sq, cfg, now)
	want := []string{"acme", "acme/acme-app", "acme/automations", "personal-dev", "personal-dev/budget-tracker", "phd"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v err=%v", got, err)
	}
}

func TestPickerWithNoProjectsIsAnEmptyArray(t *testing.T) {
	sq, _ := setup(t)
	got, err := Picker(sq, &config.Config{Location: time.UTC}, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if js, _ := json.Marshal(got); string(js) != "[]" {
		t.Fatalf("meta.picker would be %s", js)
	}
}
