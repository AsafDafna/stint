package notify

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/report"
	"stint/internal/totals"
)

const testTOML = `
[settings]
vault_reports_dir = "/tmp/x"
ntfy_topic = "t"
tune_until = "2026-10-16"
[projects.acme]
weekly_cap_hours = 30
[projects.phd]
[[rules]]
project = "acme"
app = "A"
`

type fake struct {
	sent []string
	fail bool
}

func (f *fake) Send(title, body string) error {
	if f.fail {
		return errors.New("offline")
	}
	f.sent = append(f.sent, title+"\n"+body)
	return nil
}

var params = attribute.Params{GapSeconds: 120, IdleSeconds: 300, CarryoverSeconds: 600, CallApps: map[string]bool{}}

func setup(t *testing.T) (*sql.DB, *config.Config) {
	t.Helper()
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	cfg, err := config.Parse([]byte(testTOML), "/h")
	if err != nil {
		t.Fatal(err)
	}
	return sq, cfg
}

// seed writes intervals for the week of Sun 2026-09-13 and recomputes totals.
func seed(t *testing.T, sq *sql.DB, cfg *config.Config, acmeHours, phdHours, unHours float64) {
	t.Helper()
	sun := time.Date(2026, 9, 13, 8, 0, 0, 0, cfg.Location).Unix()
	var ivs []attribute.Interval
	cur := sun
	for _, x := range []struct {
		p string
		h float64
	}{{"acme", acmeHours}, {"phd", phdHours}, {attribute.Unattributed, unHours}} {
		if x.h > 0 {
			ivs = append(ivs, attribute.Interval{From: cur, To: cur + int64(x.h*3600), Project: x.p, RuleID: "0"})
			cur += int64(x.h * 3600)
		}
	}
	insertIntervals(t, sq, ivs)
	if err := totals.Recompute(sq, cfg.Location, cfg.FirstWeekday, time.Unix(cur, 0)); err != nil {
		t.Fatal(err)
	}
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

// unattributedDay writes a 30-minute unattributed interval from ts, backed by one sample
// a minute: the material the morning tune list ranks.
func unattributedDay(t *testing.T, sq *sql.DB, ts int64) {
	t.Helper()
	insertIntervals(t, sq, []attribute.Interval{{From: ts, To: ts + 1800, Project: attribute.Unattributed, RuleID: attribute.RuleUnattributed}})
	for s := ts; s < ts+1800; s += 60 {
		if _, err := db.InsertSample(sq, attribute.Sample{TS: s, Kind: "heartbeat", App: "X", URL: "https://mystery.example"}); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, sq *sql.DB, kind string) int {
	t.Helper()
	var n int
	if err := sq.QueryRow(`SELECT COUNT(*) FROM notified WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMorningBodyFormat(t *testing.T) {
	_, cfg := setup(t)
	thu := time.Date(2026, 9, 17, 8, 5, 0, 0, cfg.Location) // Thursday: Thu, Fri, Sat left
	body := MorningBody(map[string]int64{"acme": 19*3600 + 1800, "phd": 3*3600 + 720, attribute.Unattributed: 3960}, cfg, thu, nil, db.Pending{})
	want := "acme 19.5h / 30.0h · 10.5h left · 3 days\nphd 3.2h\nunattributed 1.1h"
	if body != want {
		t.Fatalf("got\n%s\nwant\n%s", body, want)
	}
	body = MorningBody(map[string]int64{"acme": 32 * 3600}, cfg, thu, []report.Entry{{Label: "https://x", Seconds: 1800}}, db.Pending{})
	want = "acme 32.0h / 30.0h · 0.0h left · 3 days\n\nyesterday unattributed:\n0.5h https://x"
	if body != want {
		t.Fatalf("got\n%s\nwant\n%s", body, want)
	}
	if got := MorningBody(nil, &config.Config{Projects: map[string]config.Project{}, Location: cfg.Location}, thu, nil, db.Pending{}); got != "nothing tracked yet this week" {
		t.Fatalf("empty: %q", got)
	}
}

func TestDaysLeft(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Jerusalem")
	cases := []struct {
		first time.Weekday
		day   int // of 2026-09; the 13th is a Sunday
		want  int
	}{
		{time.Sunday, 13, 7},
		{time.Sunday, 16, 4},
		{time.Sunday, 19, 1},
		{time.Monday, 14, 7},
		{time.Monday, 16, 5},
		{time.Monday, 20, 1},
		{time.Saturday, 19, 7},
		{time.Saturday, 18, 1},
	}
	for _, c := range cases {
		if got := DaysLeft(time.Date(2026, 9, c.day, 12, 0, 0, 0, loc), loc, c.first); got != c.want {
			t.Errorf("DaysLeft(2026-09-%02d, %v) = %d want %d", c.day, c.first, got, c.want)
		}
	}
}

func TestMorningPushOncePerDayAfterEight(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 10, 0, 0)
	f := &fake{}
	early := time.Date(2026, 9, 16, 7, 59, 0, 0, cfg.Location)
	if _, err := Run(sq, cfg, f, params, nil, early); err != nil || len(f.sent) != 0 {
		t.Fatalf("before 08:00: sent=%v err=%v", f.sent, err)
	}
	at8 := time.Date(2026, 9, 16, 8, 0, 0, 0, cfg.Location)
	var kinds []string
	for i := 0; i < 3; i++ {
		k, err := Run(sq, cfg, f, params, nil, at8.Add(time.Duration(i)*5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k...)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "acme 10.0h / 30.0h · 20.0h left · 4 days") {
		t.Fatalf("morning push: %q", f.sent)
	}
	if !strings.HasPrefix(f.sent[0], "stint | week through Tue\n") {
		t.Fatalf("title must be ASCII: %q", f.sent[0])
	}
	if len(kinds) != 1 || kinds[0] != KindMorning {
		t.Fatalf("sent kinds = %v", kinds)
	}
	if count(t, sq, KindMorning) != 1 {
		t.Fatal("notified row missing")
	}
	nextDay := time.Date(2026, 9, 17, 9, 0, 0, 0, cfg.Location)
	if _, err := Run(sq, cfg, f, params, nil, nextDay); err != nil || len(f.sent) != 2 {
		t.Fatalf("next day: sent=%d err=%v", len(f.sent), err)
	}
}

func TestMorningPushAppendsTuningListDuringTunePeriod(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 1, 0, 0)
	y := time.Date(2026, 9, 15, 10, 0, 0, 0, cfg.Location).Unix()
	unattributedDay(t, sq, y)
	f := &fake{}
	if _, err := Run(sq, cfg, f, params, nil, time.Date(2026, 9, 16, 8, 0, 0, 0, cfg.Location)); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "yesterday unattributed:\n0.5h https://mystery.example") {
		t.Fatalf("tune list missing: %q", f.sent)
	}
	// After tune_until the list is dropped.
	f = &fake{}
	if _, err := Run(sq, cfg, f, params, nil, time.Date(2026, 10, 17, 8, 0, 0, 0, cfg.Location)); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || strings.Contains(f.sent[0], "yesterday unattributed") {
		t.Fatalf("tune list must stop after tune_until: %q", f.sent)
	}
}

func TestCapPushesOncePerWeek(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 24.5, 0, 0) // 81.7%
	f := &fake{}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, cfg.Location)
	record(sq, KindMorning, "", "2026-09-16", now) // keep the morning push out of the way
	for i := 0; i < 2; i++ {
		if _, err := Run(sq, cfg, f, params, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "acme 24.5h / 30.0h · 5.5h left · 4 days") {
		t.Fatalf("cap80: %q", f.sent)
	}
	seed(t, sq, cfg, 6, 0, 0) // adds 6 h -> 30.5 h
	if _, err := Run(sq, cfg, f, params, nil, now); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 2 || !strings.Contains(f.sent[1], "acme 30.5h / 30.0h · cap reached") {
		t.Fatalf("cap100: %q", f.sent)
	}
	if count(t, sq, KindCap80) != 1 || count(t, sq, KindCap100) != 1 {
		t.Fatal("cap rows")
	}
	// Next week: fresh counters, nothing used, nothing sent.
	nextWeek := time.Date(2026, 9, 23, 12, 0, 0, 0, cfg.Location)
	record(sq, KindMorning, "", "2026-09-23", now)
	if _, err := Run(sq, cfg, f, params, nil, nextWeek); err != nil || len(f.sent) != 2 {
		t.Fatalf("next week: %q %v", f.sent, err)
	}
}

func TestCap100SkipsStraightPast80(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 31, 0, 0)
	f := &fake{}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, cfg.Location)
	record(sq, KindMorning, "", "2026-09-16", now)
	if _, err := Run(sq, cfg, f, params, nil, now); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "cap reached") {
		t.Fatalf("want only cap100: %q", f.sent)
	}
	if count(t, sq, KindCap80) != 1 {
		t.Fatal("cap80 must be marked sent so it never fires late")
	}
}

func TestHealthPushOncePerDay(t *testing.T) {
	sq, cfg := setup(t)
	f := &fake{}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, cfg.Location)
	record(sq, KindMorning, "", "2026-09-16", now)
	var kinds []string
	for i := 0; i < 2; i++ {
		k, err := Run(sq, cfg, f, params, []string{"url NULL for 61 min"}, now)
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k...)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "url NULL for 61 min") || len(kinds) != 1 || kinds[0] != KindHealth {
		t.Fatalf("health: %q kinds=%v", f.sent, kinds)
	}
	if _, err := Run(sq, cfg, f, params, nil, now); err != nil || len(f.sent) != 1 {
		t.Fatalf("healthy run must not push: %q %v", f.sent, err)
	}
}

func TestFailedSendIsNotRecorded(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 1, 0, 0)
	f := &fake{fail: true}
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, cfg.Location)
	if _, err := Run(sq, cfg, f, params, nil, now); err == nil {
		t.Fatal("want error from failed send")
	}
	if count(t, sq, KindMorning) != 0 {
		t.Fatal("failed send must not be recorded")
	}
	f.fail = false
	if _, err := Run(sq, cfg, f, params, nil, now.Add(5*time.Minute)); err != nil || len(f.sent) != 1 {
		t.Fatalf("retry: %q %v", f.sent, err)
	}
}

func TestClientPostsWithTitleAndRejectsNon2xx(t *testing.T) {
	var gotTitle, gotBody string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		gotBody = string(b)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/topic", time.Second)
	if err := c.Send("t", "hello"); err != nil || gotTitle != "t" || gotBody != "hello" {
		t.Fatalf("send: err=%v title=%q body=%q", err, gotTitle, gotBody)
	}
	status = 500
	if err := c.Send("t", "x"); err == nil {
		t.Fatal("500 must be an error")
	}
}

// dropIntervals makes report.TopUnattributed fail while totals (and so WeekToDate) keep working.
func dropIntervals(t *testing.T, sq *sql.DB) {
	t.Helper()
	if _, err := sq.Exec(`DROP TABLE intervals`); err != nil {
		t.Fatal(err)
	}
}

func TestMorningReadFailureStillSendsCapAndHealth(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 24.5, 0, 0) // 81.7%
	dropIntervals(t, sq)
	f := &fake{}
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, cfg.Location) // tune period, after 08:00
	kinds, err := Run(sq, cfg, f, params, []string{"url NULL for 61 min"}, now)
	if err == nil {
		t.Fatal("want the morning read error surfaced")
	}
	if len(f.sent) != 2 || !strings.Contains(f.sent[0], "80% of cap") || !strings.Contains(f.sent[1], "collector degraded") {
		t.Fatalf("cap and health pushes must still go out: %q", f.sent)
	}
	if len(kinds) != 2 || kinds[0] != KindCap80 || kinds[1] != KindHealth {
		t.Fatalf("kinds = %v", kinds)
	}
	if count(t, sq, KindMorning) != 0 || count(t, sq, KindCap80) != 1 || count(t, sq, KindHealth) != 1 {
		t.Fatal("morning must not be recorded (retried next run); cap80 and health must be")
	}
}

func TestSentMorningSkipsDayScan(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 1, 0, 0)
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, cfg.Location) // tune period, after 08:00
	if err := record(sq, KindMorning, "", "2026-09-16", now); err != nil {
		t.Fatal(err)
	}
	dropIntervals(t, sq) // a day scan would now fail, so an error proves it ran
	f := &fake{}
	if kinds, err := Run(sq, cfg, f, params, nil, now); err != nil || len(f.sent) != 0 || len(kinds) != 0 {
		t.Fatalf("already-sent morning must not scan the day: kinds=%v sent=%q err=%v", kinds, f.sent, err)
	}
}

func TestOneDayLeftIsSingular(t *testing.T) {
	sq, cfg := setup(t)
	sat := time.Date(2026, 9, 19, 12, 0, 0, 0, cfg.Location)
	if body := MorningBody(map[string]int64{"acme": 3600}, cfg, sat, nil, db.Pending{}); !strings.HasSuffix(body, "· 1 day") {
		t.Fatalf("morning body on Saturday: %q", body)
	}
	seed(t, sq, cfg, 24.5, 0, 0)
	if err := record(sq, KindMorning, "", "2026-09-19", sat); err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	if _, err := Run(sq, cfg, f, params, nil, sat); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || !strings.HasSuffix(f.sent[0], "· 1 day") {
		t.Fatalf("cap80 body on Saturday: %q", f.sent)
	}
}

func TestTuneListStillSentOnTuneUntilDay(t *testing.T) {
	sq, cfg := setup(t) // tune_until = 2026-10-16: that whole day is inside the period
	y := time.Date(2026, 10, 15, 10, 0, 0, 0, cfg.Location).Unix()
	unattributedDay(t, sq, y)
	f := &fake{}
	if _, err := Run(sq, cfg, f, params, nil, time.Date(2026, 10, 16, 8, 0, 0, 0, cfg.Location)); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "yesterday unattributed:\n0.5h https://mystery.example") {
		t.Fatalf("tune list must still be sent on tune_until itself: %q", f.sent)
	}
}

func TestCappedProjectAtZeroHours(t *testing.T) {
	sq, cfg := setup(t)
	wed := time.Date(2026, 9, 16, 12, 0, 0, 0, cfg.Location)
	if body := MorningBody(map[string]int64{}, cfg, wed, nil, db.Pending{}); body != "acme 0.0h / 30.0h · 30.0h left · 4 days" {
		t.Fatalf("capped project must appear at 0 h: %q", body)
	}
	if err := record(sq, KindMorning, "", "2026-09-16", wed); err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	if kinds, err := Run(sq, cfg, f, params, nil, wed); err != nil || len(f.sent) != 0 || len(kinds) != 0 {
		t.Fatalf("0 h must not trigger a cap push: kinds=%v sent=%q err=%v", kinds, f.sent, err)
	}
}

func TestTwoProjectsCrossingCapReturnEachKindOnce(t *testing.T) {
	sq, _ := setup(t)
	cfg, err := config.Parse([]byte(strings.Replace(testTOML, "[projects.phd]\n", "[projects.phd]\nweekly_cap_hours = 10\n", 1)), "/h")
	if err != nil {
		t.Fatal(err)
	}
	seed(t, sq, cfg, 25, 9, 0) // acme 83%, phd 90%
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, cfg.Location)
	if err := record(sq, KindMorning, "", "2026-09-16", now); err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	kinds, err := Run(sq, cfg, f, params, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 2 || !strings.Contains(f.sent[0], "acme 25.0h / 30.0h") || !strings.Contains(f.sent[1], "phd 9.0h / 10.0h") {
		t.Fatalf("each project gets its own push: %q", f.sent)
	}
	if len(kinds) != 1 || kinds[0] != KindCap80 {
		t.Fatalf("kinds must be deduped: %v", kinds)
	}
}

// seedDays writes one 08:00 interval per entry (seconds on the given September 2026 day)
// and recomputes totals.
func seedDays(t *testing.T, sq *sql.DB, cfg *config.Config, entries ...dayWork) {
	t.Helper()
	var ivs []attribute.Interval
	for _, e := range entries {
		from := time.Date(2026, 9, e.day, 8, 0, 0, 0, cfg.Location).Unix()
		ivs = append(ivs, attribute.Interval{From: from, To: from + e.seconds, Project: e.project, RuleID: "0"})
	}
	insertIntervals(t, sq, ivs)
	if err := totals.Recompute(sq, cfg.Location, cfg.FirstWeekday, time.Date(2026, 9, 30, 0, 0, 0, 0, cfg.Location)); err != nil {
		t.Fatal(err)
	}
}

type dayWork struct {
	day     int
	project string
	seconds int64
}

func morningPush(t *testing.T, sq *sql.DB, cfg *config.Config, now time.Time) string {
	t.Helper()
	f := &fake{}
	if _, err := Run(sq, cfg, f, params, nil, now); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.sent {
		if strings.HasPrefix(m, "stint | week") || strings.HasPrefix(m, "stint | last week") {
			return m
		}
	}
	t.Fatalf("no morning push in %q", f.sent)
	return ""
}

func TestSundayPushIsLastWeeksFinalIncludingSaturday(t *testing.T) {
	cases := []struct {
		name    string
		satSecs int64
		want    string
	}{
		{"short", 5*3600 + 360, "acme 29.1h / 30.0h · short by 0.9h"},
		{"over", 8*3600 + 1080, "acme 32.3h / 30.0h · over by 2.3h"},
		{"met", 6 * 3600, "acme 30.0h / 30.0h · met"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sq, cfg := setup(t)
			seedDays(t, sq, cfg,
				dayWork{13, "acme", 10 * 3600}, dayWork{16, "acme", 14 * 3600}, dayWork{19, "acme", c.satSecs},
				dayWork{13, "phd", 3600}, dayWork{20, "acme", 5 * 3600}) // Sunday the 20th is today: excluded
			got := morningPush(t, sq, cfg, time.Date(2026, 9, 20, 8, 0, 0, 0, cfg.Location))
			if want := "stint | last week (final)\n" + c.want + "\nphd 1.0h"; got != want {
				t.Fatalf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestSundayPushEmptyWeek(t *testing.T) {
	sq, cfg := setup(t)
	cfg.Projects = map[string]config.Project{"phd": {}}
	seedDays(t, sq, cfg, dayWork{20, "phd", 3600}) // today: would show as "phd 1.0h" if it leaked in
	got := morningPush(t, sq, cfg, time.Date(2026, 9, 20, 8, 0, 0, 0, cfg.Location))
	if want := "stint | last week (final)\nnothing tracked last week"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMondayPushIsSundayOnlyAndExcludesToday(t *testing.T) {
	sq, cfg := setup(t)
	seedDays(t, sq, cfg,
		dayWork{19, "acme", 9 * 3600}, // previous Saturday: other week
		dayWork{20, "acme", 2 * 3600},
		dayWork{21, "acme", 5 * 3600}) // today
	got := morningPush(t, sq, cfg, time.Date(2026, 9, 21, 8, 0, 0, 0, cfg.Location))
	if want := "stint | week through Sun\nacme 2.0h / 30.0h · 28.0h left · 6 days"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMondayStartPushIsLastWeeksFinalOnMonday(t *testing.T) {
	sq, cfg := setup(t)
	cfg.FirstWeekday = time.Monday
	seedDays(t, sq, cfg,
		dayWork{13, "acme", 9 * 3600}, // Sunday before: the week that began on the 7th
		dayWork{14, "acme", 10 * 3600},
		dayWork{16, "acme", 14 * 3600},
		dayWork{20, "acme", 5*3600 + 360}, // Sunday closes the week
		dayWork{21, "acme", 5 * 3600})     // today
	got := morningPush(t, sq, cfg, time.Date(2026, 9, 21, 8, 0, 0, 0, cfg.Location))
	if want := "stint | last week (final)\nacme 29.1h / 30.0h · short by 0.9h"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMondayStartSundayPushIsNotFinal(t *testing.T) {
	sq, cfg := setup(t)
	cfg.FirstWeekday = time.Monday
	seedDays(t, sq, cfg,
		dayWork{13, "acme", 9 * 3600}, // previous week
		dayWork{14, "acme", 10 * 3600},
		dayWork{19, "acme", 2 * 3600},
		dayWork{20, "acme", 5 * 3600}) // today
	got := morningPush(t, sq, cfg, time.Date(2026, 9, 20, 8, 0, 0, 0, cfg.Location))
	if want := "stint | week through Sat\nacme 12.0h / 30.0h · 18.0h left · 1 day"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// Chile skips 00:00 on Sunday 2026-09-06 and Go's time.Date resolves that midnight to 23:00 on
// Saturday. The week that starts that Sunday must not take in Saturday the 5th.
func TestWeekWindowAfterMidnightGap(t *testing.T) {
	sq, cfg := setup(t)
	cfg.Location = santiago(t)
	cfg.Projects["acme"] = config.Project{WeeklyCapHours: 10}
	seedDays(t, sq, cfg,
		dayWork{5, "acme", 10 * 3600}, // Saturday: the week before
		dayWork{6, "acme", 2 * 3600},
		dayWork{7, "acme", 3 * 3600}) // today, before noon
	f := &fake{}
	if _, err := Run(sq, cfg, f, params, nil, time.Date(2026, 9, 7, 12, 0, 0, 0, cfg.Location)); err != nil {
		t.Fatal(err)
	}
	// 5 h this week: under 80% of the cap, so the morning push is the only one.
	want := []string{"stint | week through Sun\nacme 2.0h / 10.0h · 8.0h left · 6 days"}
	if !reflect.DeepEqual(f.sent, want) {
		t.Fatalf("sent %q\nwant %q", f.sent, want)
	}
}

// On the gap Sunday itself the final push still covers the whole week before, Saturday included.
func TestFinalPushOnMidnightGapSunday(t *testing.T) {
	sq, cfg := setup(t)
	cfg.Location = santiago(t)
	cfg.Projects["acme"] = config.Project{WeeklyCapHours: 10}
	seedDays(t, sq, cfg,
		dayWork{1, "acme", 4 * 3600},
		dayWork{5, "acme", 3 * 3600},
		dayWork{6, "acme", 5 * 3600}) // today
	got := morningPush(t, sq, cfg, time.Date(2026, 9, 6, 8, 0, 0, 0, cfg.Location))
	if want := "stint | last week (final)\nacme 7.0h / 10.0h · short by 3.0h"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func santiago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestMidweekPushExcludesToday(t *testing.T) {
	sq, cfg := setup(t)
	seedDays(t, sq, cfg, dayWork{13, "acme", 3600}, dayWork{15, "acme", 2 * 3600}, dayWork{16, "acme", 7 * 3600})
	got := morningPush(t, sq, cfg, time.Date(2026, 9, 16, 8, 0, 0, 0, cfg.Location))
	if want := "stint | week through Tue\nacme 3.0h / 30.0h · 27.0h left · 4 days"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMorningEmptyMidweek(t *testing.T) {
	sq, cfg := setup(t)
	cfg.Projects = map[string]config.Project{}
	got := morningPush(t, sq, cfg, time.Date(2026, 9, 17, 8, 0, 0, 0, cfg.Location))
	if want := "stint | week through Wed\nnothing tracked yet this week"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCap80StillCountsToday(t *testing.T) {
	sq, cfg := setup(t)
	seedDays(t, sq, cfg, dayWork{13, "acme", 10 * 3600}, dayWork{16, "acme", 14*3600 + 1800}) // 80% only with today's 14.5h
	f := &fake{}
	if _, err := Run(sq, cfg, f, params, nil, time.Date(2026, 9, 16, 12, 0, 0, 0, cfg.Location)); err != nil {
		t.Fatal(err)
	}
	if count(t, sq, KindCap80) != 1 {
		t.Fatalf("cap80 must use live week-to-date including today: %q", f.sent)
	}
}

func TestMorningReminderLine(t *testing.T) {
	_, cfg := setup(t)
	thu := time.Date(2026, 9, 17, 8, 5, 0, 0, cfg.Location)
	through := map[string]int64{"phd": 3600, attribute.Unattributed: 1800}
	for _, c := range []struct {
		p    db.Pending
		want string
	}{
		{db.Pending{Calls: 2, Stretches: 3, StretchSeconds: 5760}, "\nunattributed 0.5h\nto attribute: 2 meetings, 3 stretches (1.6h)"},
		{db.Pending{Calls: 1}, "\nunattributed 0.5h\nto attribute: 1 meeting"},
		{db.Pending{Stretches: 1, StretchSeconds: 1800}, "\nunattributed 0.5h\nto attribute: 1 stretch (0.5h)"},
	} {
		if body := MorningBody(through, cfg, thu, nil, c.p); !strings.HasSuffix(body, c.want) {
			t.Errorf("%+v: body\n%s", c.p, body)
		}
	}
	if body := MorningBody(through, cfg, thu, nil, db.Pending{}); strings.Contains(body, "to attribute") {
		t.Fatalf("nothing pending, no line: %q", body)
	}
	tune := []report.Entry{{Label: "https://x", Seconds: 1800}}
	if body := MorningBody(through, cfg, thu, tune, db.Pending{Calls: 1}); !strings.Contains(body, "to attribute: 1 meeting\n\nyesterday unattributed:") {
		t.Fatalf("the reminder goes before the tune list: %q", body)
	}
}

// Only pending rows count: an answered stretch is not reminded about.
func TestMorningPushRemindsAboutPendingRows(t *testing.T) {
	sq, cfg := setup(t)
	seed(t, sq, cfg, 1, 0, 0)
	db.InsertStretch(sq, db.Stretch{FromTS: 1000, ToTS: 2000, UnattributedS: 720, Breakdown: "[]"})
	db.InsertStretch(sq, db.Stretch{FromTS: 3000, ToTS: 4000, UnattributedS: 900, Breakdown: "[]"})
	db.SetStretchAnswer(sq, 3000, `[]`, 5)
	f := &fake{}
	if _, err := Run(sq, cfg, f, params, nil, time.Date(2026, 9, 16, 8, 0, 0, 0, cfg.Location)); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "\nto attribute: 1 stretch (0.2h)") {
		t.Fatalf("morning push: %q", f.sent)
	}
}
