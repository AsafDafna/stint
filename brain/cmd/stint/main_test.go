package main

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/rules"
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

type env struct {
	dbPath, cfgPath, reports string
	loc                      *time.Location
	pushes                   *atomic.Int32
}

// newEnv creates a temp database with the test schema, a rules file whose ntfy_url points
// at a local server, and a temp reports dir.
func newEnv(t *testing.T) env {
	t.Helper()
	dir := t.TempDir()
	var pushes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	// The real path is "~/Library/Application Support/stint/stint.db": keep the space.
	e := env{dbPath: filepath.Join(dir, "Application Support", "stint", "stint.db"), cfgPath: filepath.Join(dir, "rules.toml"), reports: filepath.Join(dir, "reports"), pushes: &pushes}
	sq, err := db.Create(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sq.Close()
	rules := `
[settings]
ntfy_topic = "test"
ntfy_url = "` + srv.URL + `"
vault_reports_dir = "` + e.reports + `"
call_apps = ["us.zoom.xos"]
tune_until = "2026-10-16"

[projects.acme]
weekly_cap_hours = 30
paid = true
[projects.phd]

[[rules]]
project = "acme"
cwd_prefix = "/Users/me/dev-projects/acme"

[[rules]]
project = "phd"
app = "com.bookry.wavebox"
url_regex = 'overleaf\.com'
`
	if err := os.WriteFile(e.cfgPath, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	e.loc, _ = time.LoadLocation("Asia/Jerusalem")
	return e
}

func (e env) insert(t *testing.T, samples ...attribute.Sample) {
	t.Helper()
	sq, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	for _, s := range samples {
		if _, err := db.InsertSample(sq, s); err != nil {
			t.Fatal(err)
		}
	}
}

func (e env) exec(t *testing.T, now time.Time, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"--config", e.cfgPath, "--db", e.dbPath}, args...), &out, &errb, now)
	return code, out.String(), errb.String()
}

// totalsRows maps each totals row's day to its {day_seconds, week_seconds}.
func totalsRows(t *testing.T, sq *sql.DB) map[string][2]int64 {
	t.Helper()
	rows, err := sq.Query(`SELECT day, day_seconds, week_seconds FROM totals`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var day string
		var d, w int64
		if err := rows.Scan(&day, &d, &w); err != nil {
			t.Fatal(err)
		}
		out[day] = [2]int64{d, w}
	}
	return out
}

// Changing week_start re-buckets every stored week_seconds on the next run, and the run tells
// the collector through meta.week_start.
func TestRunRebuildsWeeksWhenWeekStartChanges(t *testing.T) {
	e := newEnv(t)
	sun := time.Date(2026, 9, 13, 10, 0, 0, 0, e.loc).Unix()
	mon := time.Date(2026, 9, 14, 10, 0, 0, 0, e.loc).Unix()
	var samples []attribute.Sample
	for _, from := range []int64{sun, mon} {
		for ts := from; ts < from+1800; ts += 60 {
			samples = append(samples, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", Title: "zsh", CWD: "/Users/me/dev-projects/acme", IdleS: 2})
		}
	}
	e.insert(t, samples...)
	now := time.Unix(mon+3600, 0)
	if code, out, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: code=%d out=%q err=%q", code, out, errs)
	}
	sq, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	before := totalsRows(t, sq)
	d13, d14 := before["2026-09-13"][0], before["2026-09-14"][0]
	if d13 == 0 || d14 == 0 || before["2026-09-13"][1] != d13+d14 || before["2026-09-14"][1] != d13+d14 {
		t.Fatalf("sunday weeks: %v", before)
	}
	if v, _, _ := db.GetMeta(sq, "week_start"); v != "sunday" {
		t.Fatalf("meta.week_start = %q, want sunday", v)
	}

	rules, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.cfgPath, []byte(strings.Replace(string(rules), "[settings]", "[settings]\nweek_start = \"Monday\"", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := e.exec(t, now.Add(5*time.Minute), "run"); code != 0 {
		t.Fatalf("run after the change: code=%d out=%q err=%q", code, out, errs)
	}
	after := totalsRows(t, sq)
	if want := map[string][2]int64{"2026-09-13": {d13, d13}, "2026-09-14": {d14, d14}}; !reflect.DeepEqual(after, want) {
		t.Fatalf("monday weeks: got %v want %v", after, want)
	}
	if v, _, _ := db.GetMeta(sq, "week_start"); v != "monday" {
		t.Fatalf("meta.week_start = %q, want monday", v)
	}
}

// Chile skips 00:00 on Sunday 2026-09-06: report --week on that day gets the week it opens.
func TestReportWeekOnMidnightGapDay(t *testing.T) {
	e := newEnv(t)
	cfg, err := config.Load(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Location, err = time.LoadLocation("America/Santiago"); err != nil {
		t.Fatal(err)
	}
	sq, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	var out, errb bytes.Buffer
	a := &app{cfg: cfg, cfgPath: e.cfgPath, sq: sq, matcher: rules.New(cfg), stdout: &out, stderr: &errb, now: time.Date(2026, 9, 20, 12, 0, 0, 0, cfg.Location)}
	if err := a.report([]string{"--week", "2026-09-06"}); err != nil {
		t.Fatal(err)
	}
	if head, _, _ := strings.Cut(out.String(), "\n"); head != "# Week 37 · Sep 6 – Sep 12 · 0.0h" {
		t.Errorf("heading = %q", head)
	}
}

func TestUsageAndUnknownCommand(t *testing.T) {
	e := newEnv(t)
	if code, _, errs := e.exec(t, time.Now()); code != 1 || !strings.Contains(errs, "usage: stint") {
		t.Fatalf("no args: code=%d err=%q", code, errs)
	}
	if code, _, errs := e.exec(t, time.Now(), "bogus"); code != 1 || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown: code=%d err=%q", code, errs)
	}
}

func TestRunPipelineEndToEnd(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	var samples []attribute.Sample
	for ts := base; ts < base+3600; ts += 60 { // 1 h in an acme repo
		samples = append(samples, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", Title: "zsh", CWD: "/Users/me/dev-projects/acme/automations", IdleS: 2})
	}
	for ts := base + 3600; ts < base+5400; ts += 60 { // 30 min unmatched (carry-over then unattributed)
		samples = append(samples, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	e.insert(t, samples...)

	now := wed10.Add(95 * time.Minute)
	code, out, errs := e.exec(t, now, "run")
	if code != 0 {
		t.Fatalf("run: code=%d out=%q err=%q", code, out, errs)
	}
	if !strings.Contains(out, "first run: built intervals from the oldest sample (90 rows)") || !strings.Contains(out, "sent morning push") || errs != "" {
		t.Fatalf("first run log: out=%q err=%q", out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	ivs, _ := db.ReadIntervals(sq, 0, 1<<62-1)
	if len(ivs) != 3 || ivs[0].Project != "acme" || ivs[0].Subproject != "automations" || ivs[1].RuleID != attribute.RuleCarryover || ivs[2].Project != attribute.Unattributed {
		t.Fatalf("intervals: %+v", ivs)
	}
	var rows int
	// 60 min matched + 10 min carried over (the carry-over keeps the subproject).
	sq.QueryRow(`SELECT COUNT(*) FROM totals WHERE project = 'acme/automations' AND day = '2026-09-16' AND day_seconds = 4200 AND week_seconds = 4200`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("totals row for acme/automations missing; intervals %+v", ivs)
	}
	if v, _, _ := db.GetMeta(sq, "health"); v != "ok" {
		t.Fatalf("meta.health = %q", v)
	}
	if _, ok, _ := db.GetMeta(sq, "health_at"); !ok {
		t.Fatal("meta.health_at missing")
	}
	if v, _, _ := db.GetMeta(sq, "watermark_rowid"); v == "" {
		t.Fatal("watermark missing")
	}
	if e.pushes.Load() != 1 { // morning push (after 08:00, first today); no cap, healthy
		t.Fatalf("pushes = %d, want 1", e.pushes.Load())
	}
	// Second run: idempotent, no second morning push, and nothing worth logging.
	if code, out, errs := e.exec(t, now.Add(5*time.Minute), "run"); code != 0 || out != "" || errs != "" {
		t.Fatalf("second run: code=%d out=%q err=%q", code, out, errs)
	}
	if again, _ := db.ReadIntervals(sq, 0, 1<<62-1); len(again) != 3 || again[0] != ivs[0] {
		t.Fatalf("second run changed intervals: %+v", again)
	}
	if e.pushes.Load() != 1 {
		t.Fatalf("pushes after second run = %d", e.pushes.Load())
	}

	// explain at a moment inside the acme interval
	code, out, _ = e.exec(t, now, "explain", time.Unix(base+600, 0).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "project=acme subproject=automations rule_id=0") || !strings.Contains(out, "claimed by rule 0: project=acme cwd_prefix=/Users/me/dev-projects/acme") {
		t.Fatalf("explain: code=%d out=%q", code, out)
	}
	code, out, _ = e.exec(t, now, "explain", time.Unix(base+5000, 0).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "matches  no rule") || !strings.Contains(out, "no rule, no carry-over") {
		t.Fatalf("explain unattributed: code=%d out=%q", code, out)
	}
	// explain --yesterday from the next day
	code, out, _ = e.exec(t, now.AddDate(0, 0, 1), "explain", "--yesterday")
	if code != 0 || !strings.Contains(out, "unattributed on 2026-09-16:") || !strings.Contains(out, "h  Music") {
		t.Fatalf("explain --yesterday: code=%d out=%q", code, out)
	}
	// before the horizon: exit 2
	if code, _, errs := e.exec(t, now, "explain", "1000"); code != 2 || !strings.Contains(errs, "oldest") {
		t.Fatalf("horizon: code=%d err=%q", code, errs)
	}
	if code, _, _ := e.exec(t, now, "recompute", "--since", "2026-09-15"); code != 2 {
		t.Fatalf("recompute before horizon must exit 2, got %d", code)
	}
	code, out, errs = e.exec(t, now, "recompute", "--since", "2026-09-16")
	if code != 0 || !strings.Contains(out, "recomputed intervals and totals from 2026-09-16T10:00:00+03:00 (90 samples)") {
		t.Fatalf("recompute: %d out=%q err=%q", code, out, errs)
	}
	if again, _ := db.ReadIntervals(sq, 0, 1<<62-1); len(again) != 3 || again[0] != ivs[0] {
		t.Fatalf("recompute changed intervals: %+v", again)
	}
	if _, ok, _ := db.GetMeta(sq, metaRecomputeSince); ok {
		t.Fatal("meta.recompute_since must be cleared after a successful recompute")
	}
	// A recompute that ends the following week regenerates that week's report.
	code, out, _ = e.exec(t, now.AddDate(0, 0, 7), "recompute", "--since", "2026-09-16")
	if code != 0 || !strings.Contains(out, "regenerated "+filepath.Join(e.reports, "2026-W38.md")) {
		t.Fatalf("recompute a week later: %d out=%q", code, out)
	}
	// report --week writes the file for the (incomplete) current week
	code, out, errs = e.exec(t, now, "report", "--week", "2026-09-16")
	if code != 0 || !strings.Contains(out, "# Week 38 · Sep 13 – Sep 19 · ") || !strings.Contains(out, "\n  automations ") {
		t.Fatalf("report: code=%d out=%q err=%q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(e.reports, "2026-W38.md")); err != nil {
		t.Fatal(err)
	}
}

func TestRunRefusesUnknownSchemaButRecordsHealth(t *testing.T) {
	e := newEnv(t)
	sq, _ := db.Open(e.dbPath)
	db.SetMeta(sq, "schema_version", "5")
	sq.Close()
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	code, _, errs := e.exec(t, now, "run")
	if code != 1 || !strings.Contains(errs, "schema version mismatch") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	sq, _ = db.Open(e.dbPath)
	defer sq.Close()
	if v, _, _ := db.GetMeta(sq, "health"); !strings.Contains(v, "schema version mismatch") {
		t.Fatalf("meta.health = %q", v)
	}
	if e.pushes.Load() != 1 {
		t.Fatalf("health push count = %d", e.pushes.Load())
	}
	var n int
	sq.QueryRow(`SELECT COUNT(*) FROM intervals`).Scan(&n)
	if n != 0 {
		t.Fatal("pipeline must not run on a schema mismatch")
	}
}

func TestRunResumesInterruptedRecompute(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	var samples []attribute.Sample
	for ts := base; ts < base+1800; ts += 60 {
		samples = append(samples, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", CWD: "/Users/me/dev-projects/acme/x", IdleS: 1})
	}
	e.insert(t, samples...)
	now := wed10.Add(40 * time.Minute)
	if code, _, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: %d %q", code, errs)
	}
	// Simulate a recompute that died after writing its marker and wiping intervals.
	sq, _ := db.Open(e.dbPath)
	db.SetMeta(sq, metaRecomputeSince, "2026-09-16")
	sq.Exec(`DELETE FROM intervals`)
	sq.Close()
	code, out, errs := e.exec(t, now.Add(5*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "resuming interrupted recompute since 2026-09-16") || !strings.Contains(out, "recomputed intervals and totals from") {
		t.Fatalf("resume: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ = db.Open(e.dbPath)
	defer sq.Close()
	if ivs, _ := db.ReadIntervals(sq, 0, 1<<62-1); len(ivs) != 1 || ivs[0].Project != "acme" {
		t.Fatalf("intervals after resume: %+v", ivs)
	}
	if _, ok, _ := db.GetMeta(sq, metaRecomputeSince); ok {
		t.Fatal("marker must be cleared")
	}
}

// TestRunReportsNonSchemaCheckErrorPlainly: CheckSchema wraps db.ErrSchema only on a
// version mismatch, so a meta read failure (locked db, corrupt file) must be reported
// plainly and must not be recorded as a schema mismatch or trigger the daily degraded push.
func TestRunReportsNonSchemaCheckErrorPlainly(t *testing.T) {
	e := newEnv(t)
	sq, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sq.Exec(`DROP TABLE meta`); err != nil {
		t.Fatal(err)
	}
	sq.Close()
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	code, _, errs := e.exec(t, now, "run")
	if code != 1 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if errs == "" {
		t.Fatal("expected a non-empty stderr")
	}
	if strings.Contains(errs, "schema version mismatch") {
		t.Fatalf("a non-schema check error must not be reported as a schema mismatch: %q", errs)
	}
	if e.pushes.Load() != 0 {
		t.Fatalf("must not push for a non-schema check error, got %d pushes", e.pushes.Load())
	}
}

// TestRunClearsUnparseableRecomputeMarker: an unparseable meta.recompute_since must not
// wedge every future run; run logs it, clears it, and falls through to the normal pipeline.
func TestRunClearsUnparseableRecomputeMarker(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	var samples []attribute.Sample
	for ts := base; ts < base+1800; ts += 60 {
		samples = append(samples, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", CWD: "/Users/me/dev-projects/acme/x", IdleS: 1})
	}
	e.insert(t, samples...)
	sq, _ := db.Open(e.dbPath)
	if err := db.SetMeta(sq, metaRecomputeSince, "not-a-date"); err != nil {
		t.Fatal(err)
	}
	sq.Close()
	now := wed10.Add(40 * time.Minute)
	code, out, errs := e.exec(t, now, "run")
	if code != 0 || errs != "" {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if !strings.Contains(out, `bad recompute_since "not-a-date"`) || !strings.Contains(out, "parsing time") {
		t.Fatalf("expected the log line to carry the marker and the parse error, got %q", out)
	}
	sq, _ = db.Open(e.dbPath)
	defer sq.Close()
	if _, ok, _ := db.GetMeta(sq, metaRecomputeSince); ok {
		t.Fatal("the unparseable marker must be cleared")
	}
	if ivs, _ := db.ReadIntervals(sq, 0, 1<<62-1); len(ivs) == 0 {
		t.Fatal("the pipeline must still have run and built intervals")
	}
}

// TestRunRecomputeReportFailureClearsMarkerAndContinues: a report-directory failure during
// a resumed recompute must not prevent the marker from clearing, must not abort the rest of the pipeline (intervals
// and totals are already consistent by then), and must not wedge every future run into
// replaying the recompute. Once the directory is fixed and the missing report is
// backfilled, a later run has nothing left to do.
//
// now2 is chosen at the Sep13-19 week's boundary (2026-09-20 00:00 local, outside the
// default 07:00-23:00 active_hours) so health.Check reports nothing regardless of the
// week-old sample data, isolating the report-step failure this test targets.
func TestRunRecomputeReportFailureClearsMarkerAndContinues(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stint.db")
	cfgPath := filepath.Join(dir, "rules.toml")
	blocked := filepath.Join(dir, "reports-blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCfg := func(reportsDir string) {
		t.Helper()
		rules := `
[settings]
vault_reports_dir = "` + reportsDir + `"

[projects.acme]

[[rules]]
project = "acme"
cwd_prefix = "/Users/me/dev-projects/acme"
`
		if err := os.WriteFile(cfgPath, []byte(rules), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCfg(blocked)

	sq, err := db.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sq.Close()

	// Saturday night, ending 15 min before the week (Sun-Sat) rolls over.
	base := time.Date(2026, 9, 19, 23, 26, 0, 0, loc).Unix()
	sq, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for ts := base; ts < base+1200; ts += 60 {
		if _, err := db.InsertSample(sq, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", CWD: "/Users/me/dev-projects/acme/x", IdleS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	sq.Close()

	exec := func(now time.Time, args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(append([]string{"--config", cfgPath, "--db", dbPath}, args...), &out, &errb, now)
		return code, out.String(), errb.String()
	}

	now1 := time.Date(2026, 9, 19, 23, 50, 0, 0, loc) // same, still-incomplete week
	if code, _, errs := exec(now1, "run"); code != 0 {
		t.Fatalf("initial run: code=%d err=%q", code, errs)
	}

	sq, _ = db.Open(dbPath)
	if err := db.SetMeta(sq, metaRecomputeSince, "2026-09-19"); err != nil {
		t.Fatal(err)
	}
	sq.Close()

	now2 := time.Date(2026, 9, 20, 0, 0, 0, 0, loc) // the Sep13-19 week just completed
	code, out, errs := exec(now2, "run")
	if code != 1 {
		t.Fatalf("broken-report run: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ = db.Open(dbPath)
	if _, ok, _ := db.GetMeta(sq, metaRecomputeSince); ok {
		t.Fatal("marker must be cleared even though the report step failed")
	}
	var totalRows int
	sq.QueryRow(`SELECT COUNT(*) FROM totals WHERE project = 'acme/x' AND day = '2026-09-19'`).Scan(&totalRows)
	sq.Close()
	if totalRows != 1 {
		t.Fatal("intervals/totals must have been rebuilt despite the report failure")
	}

	// The mount comes back; an admin backfills the report that failed above.
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	goodReports := filepath.Join(dir, "reports")
	writeCfg(goodReports)
	if code, _, errs := exec(now2, "report", "--week", "2026-09-19"); code != 0 {
		t.Fatalf("backfill report: code=%d err=%q", code, errs)
	}

	code, out, errs = exec(now2.Add(5*time.Minute), "run")
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("second run: code=%d out=%q err=%q", code, out, errs)
	}
}

// acmeSamples returns 30 min of one-per-minute heartbeats in an acme repo from start.
func acmeSamples(start time.Time) []attribute.Sample {
	var samples []attribute.Sample
	for ts := start.Unix(); ts < start.Unix()+1800; ts += 60 {
		samples = append(samples, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", CWD: "/Users/me/dev-projects/acme/x", IdleS: 1})
	}
	return samples
}

// TestHelpAndUnknownCommandNeedNoDB: usage is answered from argv alone. --help/-h/help
// print usage to stdout and exit 0; an unknown command or no command prints usage to
// stderr and exits 1. None of them opens the config or the database.
func TestHelpAndUnknownCommandNeedNoDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "missing", "stint.db")
	cfgPath := filepath.Join(dir, "missing", "rules.toml")
	exec := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(append([]string{"--config", cfgPath, "--db", dbPath}, args...), &out, &errb, time.Now())
		return code, out.String(), errb.String()
	}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		code, out, errs := exec(args...)
		if code != 0 || !strings.Contains(out, "usage: stint") || errs != "" {
			t.Fatalf("%v: code=%d out=%q err=%q", args, code, out, errs)
		}
	}
	code, out, errs := exec("bogus")
	if code != 1 || out != "" || !strings.Contains(errs, `unknown command "bogus"`) || !strings.Contains(errs, "usage: stint") {
		t.Fatalf("unknown: code=%d out=%q err=%q", code, out, errs)
	}
	if code, out, errs := exec(); code != 1 || out != "" || !strings.Contains(errs, "usage: stint") {
		t.Fatalf("no args: code=%d out=%q err=%q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Dir(dbPath)); !os.IsNotExist(err) {
		t.Fatalf("usage paths must not touch the database: %v", err)
	}
}

// TestExtraPositionalArgsRejected: every command fails with a usage error on stray args.
func TestExtraPositionalArgsRejected(t *testing.T) {
	e := newEnv(t)
	e.insert(t, acmeSamples(time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc))...)
	now := time.Date(2026, 9, 16, 10, 40, 0, 0, e.loc)
	for _, args := range [][]string{
		{"run", "extra"},
		{"recompute", "--since", "2026-09-16", "extra"},
		{"report", "--week", "2026-09-16", "extra"},
		{"explain", "--yesterday", "extra"},
		{"explain", "1758006000", "extra"},
	} {
		code, _, errs := e.exec(t, now, args...)
		if code != 1 || !strings.Contains(errs, args[0]+":") {
			t.Fatalf("%v: code=%d err=%q", args, code, errs)
		}
	}
}

// TestReportKeepsStdoutForMarkdown: stdout is only the markdown; the "written" line
// goes to stderr so `stint report --week X > file` stays clean.
func TestReportKeepsStdoutForMarkdown(t *testing.T) {
	e := newEnv(t)
	e.insert(t, acmeSamples(time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc))...)
	now := time.Date(2026, 9, 16, 10, 40, 0, 0, e.loc)
	if code, _, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: %d %q", code, errs)
	}
	code, out, errs := e.exec(t, now, "report", "--week", "2026-09-16")
	if code != 0 || errs != "written "+filepath.Join(e.reports, "2026-W38.md")+"\n" || !strings.HasPrefix(out, "# ") || strings.Contains(out, "written ") {
		t.Fatalf("report: code=%d out=%q err=%q", code, out, errs)
	}
}

// TestExplainLocalTimeFormat: a bare YYYY-MM-DDTHH:MM:SS is read in the configured zone,
// and the parse error names all three accepted forms.
func TestExplainLocalTimeFormat(t *testing.T) {
	e := newEnv(t)
	start := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	e.insert(t, acmeSamples(start)...)
	now := start.Add(40 * time.Minute)
	if code, _, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: %d %q", code, errs)
	}
	_, rfc, _ := e.exec(t, now, "explain", "2026-09-16T10:10:00+03:00")
	code, local, errs := e.exec(t, now, "explain", "2026-09-16T10:10:00")
	if code != 0 || local != rfc || !strings.Contains(local, "sample #11 at 2026-09-16T10:10:00+03:00") {
		t.Fatalf("local format: code=%d out=%q err=%q rfc=%q", code, local, errs, rfc)
	}
	if code, _, errs := e.exec(t, now, "explain", "yesterday-ish"); code != 1 || !strings.Contains(errs, "YYYY-MM-DDTHH:MM:SS") {
		t.Fatalf("bad moment: code=%d err=%q", code, errs)
	}
}

// TestRunWithoutNtfy: no ntfy_url and no ntfy_topic means no sender; run still completes.
func TestRunWithoutNtfy(t *testing.T) {
	e := newEnv(t)
	rules := `
[settings]
vault_reports_dir = "` + e.reports + `"

[projects.acme]

[[rules]]
project = "acme"
cwd_prefix = "/Users/me/dev-projects/acme"
`
	if err := os.WriteFile(e.cfgPath, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	e.insert(t, acmeSamples(time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc))...)
	now := time.Date(2026, 9, 16, 10, 40, 0, 0, e.loc)
	code, out, errs := e.exec(t, now, "run")
	if code != 0 || errs != "" || strings.Contains(out, "sent ") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if e.pushes.Load() != 0 {
		t.Fatalf("pushes = %d", e.pushes.Load())
	}
}

// TestSinceAndWeekValidation: missing or malformed --since/--week is a usage error.
func TestSinceAndWeekValidation(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 9, 16, 10, 40, 0, 0, e.loc)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"recompute"}, "--since YYYY-MM-DD is required"},
		{[]string{"recompute", "--since", "16/09/2026"}, `--since must be YYYY-MM-DD, got "16/09/2026"`},
		{[]string{"report"}, "--week YYYY-MM-DD is required"},
		{[]string{"report", "--week", "2026-9-16"}, `--week must be YYYY-MM-DD, got "2026-9-16"`},
	} {
		code, _, errs := e.exec(t, now, tc.args...)
		if code != 1 || !strings.Contains(errs, tc.want) {
			t.Fatalf("%v: code=%d err=%q", tc.args, code, errs)
		}
	}
}

// TestRunClearsHorizonRecomputeMarker: a resume marker that now lies before the retention
// horizon is logged, cleared and skipped; the pipeline runs and the exit code stays 0.
// (A user-invoked `recompute --since` before the horizon still exits 2.)
func TestRunClearsHorizonRecomputeMarker(t *testing.T) {
	e := newEnv(t)
	e.insert(t, acmeSamples(time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc))...)
	now := time.Date(2026, 9, 16, 10, 40, 0, 0, e.loc)
	if code, _, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: %d %q", code, errs)
	}
	sq, _ := db.Open(e.dbPath)
	if err := db.SetMeta(sq, metaRecomputeSince, "2026-09-10"); err != nil {
		t.Fatal(err)
	}
	sq.Close()
	code, out, errs := e.exec(t, now.Add(5*time.Minute), "run")
	if code != 0 || errs != "" || !strings.Contains(out, "recompute_since") || !strings.Contains(out, "horizon") {
		t.Fatalf("resume: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ = db.Open(e.dbPath)
	defer sq.Close()
	if _, ok, _ := db.GetMeta(sq, metaRecomputeSince); ok {
		t.Fatal("the horizon marker must be cleared")
	}
	if code, out, errs := e.exec(t, now.Add(10*time.Minute), "run"); code != 0 || strings.Contains(out, "recompute") || errs != "" {
		t.Fatalf("next run must not resume again: code=%d out=%q err=%q", code, out, errs)
	}
	if code, _, _ := e.exec(t, now, "recompute", "--since", "2026-09-10"); code != 2 {
		t.Fatalf("recompute before horizon must exit 2, got %d", code)
	}
}

// TestRunPurgeLogsOneLinePerDay: purges are tallied per local day and the previous
// day's total is logged once, on the first purge of the next day; runs that purge
// nothing never log.
func TestRunPurgeLogsOneLinePerDay(t *testing.T) {
	e := newEnv(t)
	old := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	e.insert(t, acmeSamples(old)...) // 30 rows, 10:00-10:29
	music := func(at time.Time) attribute.Sample {
		return attribute.Sample{TS: at.Unix(), Kind: "heartbeat", App: "com.apple.Music", IdleS: 1}
	}
	e.insert(t, music(old.Add(45*time.Minute)), music(old.Add(65*time.Minute)), music(old.Add(2*time.Hour)))
	day1 := old.Add(90*24*time.Hour + time.Hour) // raw_retention_days defaults to 90: cutoff = old+60min (fixed durations: AddDate would add the DST hour)
	e.insert(t, music(day1.Add(-time.Minute)))
	code, out, errs := e.exec(t, day1, "run") // purges 31 rows; silent (tally starts)
	if code != 0 || strings.Contains(out, "purged ") {
		t.Fatalf("first purge: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs = e.exec(t, day1.Add(10*time.Minute), "run") // cutoff old+70min: purges 1 more; silent
	if code != 0 || strings.Contains(out, "purged ") {
		t.Fatalf("second purge: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs = e.exec(t, day1.Add(12*time.Minute), "run") // nothing to purge
	if code != 0 || strings.Contains(out, "purged ") {
		t.Fatalf("no-op run: code=%d out=%q err=%q", code, out, errs)
	}
	day2 := day1.Add(24 * time.Hour)
	code, out, errs = e.exec(t, day2, "run") // purges the old+2h row; logs day 1's tally
	want := "purged 32 samples older than 90 days on " + day1.Format("2006-01-02")
	if code != 0 || strings.Count(out, "purged ") != 1 || !strings.Contains(out, want) {
		t.Fatalf("next-day run: code=%d out=%q err=%q want %q", code, out, errs, want)
	}
	code, out, errs = e.exec(t, day2.Add(5*time.Minute), "run")
	if code != 0 || strings.Contains(out, "purged ") {
		t.Fatalf("same-day run: code=%d out=%q err=%q", code, out, errs)
	}
}

// TestRecomputeReportsRegeneratedWeeksOnPartialFailure: when WriteWeeks fails part way,
// the weeks it did rewrite are still logged before the error is reported.
func TestRecomputeReportsRegeneratedWeeksOnPartialFailure(t *testing.T) {
	e := newEnv(t)
	e.insert(t, acmeSamples(time.Date(2026, 9, 9, 10, 0, 0, 0, e.loc))...) // W37 (Sep 6-12)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, e.loc)                      // W37, W38, W39 complete
	if code, _, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: %d %q", code, errs)
	}
	w38 := filepath.Join(e.reports, "2026-W38.md")
	if err := os.Remove(w38); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(w38, 0o755); err != nil { // a directory in the way: W38 cannot be written
		t.Fatal(err)
	}
	code, out, errs := e.exec(t, now, "recompute", "--since", "2026-09-09")
	if code != 1 || !strings.Contains(out, "regenerated "+filepath.Join(e.reports, "2026-W37.md")) || strings.Contains(out, "2026-W39.md") {
		t.Fatalf("partial: code=%d out=%q err=%q", code, out, errs)
	}
	if !strings.Contains(errs, "2026-W38.md") {
		t.Fatalf("error must name the failed week: %q", errs)
	}
}

// TestRecomputeOnEmptyDBClearsMarker: a recompute before any sample exists fails (exit
// 1) but leaves no recompute_since marker behind, so the next run is a normal one.
func TestRecomputeOnEmptyDBClearsMarker(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 9, 16, 10, 40, 0, 0, e.loc)
	if code, _, errs := e.exec(t, now, "recompute", "--since", "2026-09-14"); code != 1 || !strings.Contains(errs, "no samples yet") {
		t.Fatalf("recompute: code=%d err=%q", code, errs)
	}
	code, out, errs := e.exec(t, now.Add(5*time.Minute), "run")
	if code != 0 || strings.Contains(out, "resum") || errs != "" {
		t.Fatalf("run after empty recompute: code=%d out=%q err=%q", code, out, errs)
	}
}

// zoomCall: a 10-minute Zoom call in an acme repo, then Music (closing rows).
func zoomCall(base int64) []attribute.Sample {
	var s []attribute.Sample
	for ts := base; ts < base+600; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", Title: "zsh", CWD: "/Users/me/dev-projects/acme/automations", IdleS: 2, CallApp: "us.zoom.xos"})
	}
	for ts := base + 600; ts < base+1200; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	return s
}

func TestRunDetectsCallsAndAppliesDecisions(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	// Run 1: only pre-call rows, so a watermark exists before the call.
	var pre []attribute.Sample
	for ts := base - 1200; ts < base-600; ts += 60 {
		pre = append(pre, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	e.insert(t, pre...)
	if code, out, errs := e.exec(t, wed10.Add(-9*time.Minute), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	// Run 2: the call and its closing rows.
	e.insert(t, zoomCall(base)...)
	code, out, errs := e.exec(t, wed10.Add(22*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "guessed acme/automations: awaiting the pop-up") {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	rows, _ := db.ReadCalls(sq, 0)
	if len(rows) != 1 || rows[0].FromTS != base || rows[0].ToTS != base+600 || rows[0].Decision != "" ||
		rows[0].Breakdown != `[{"project":"acme","subproject":"automations","seconds":600},{"background":"acme/automations","seconds":600},{"background":"away","seconds":0}]` {
		t.Fatalf("calls: %+v", rows)
	}
	if v, _, _ := db.GetMeta(sq, "projects"); v != "acme,phd" {
		t.Fatalf("meta.projects = %q", v)
	}
	// Run 3: the same rows again must not add a second call (watermark moved past it).
	if _, out, _ := e.exec(t, wed10.Add(27*time.Minute), "run"); strings.Contains(out, "awaiting the pop-up") {
		t.Fatalf("run 3 re-detected: %q", out)
	}
	// Decide: whole call to phd, then run 4 applies it.
	if code, _, errs := e.exec(t, wed10.Add(30*time.Minute), "calls", "set", strconv.FormatInt(base, 10), "--project", "phd"); code != 0 {
		t.Fatalf("calls set: code=%d err=%q", code, errs)
	}
	code, out, errs = e.exec(t, wed10.Add(32*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "applied 1 call decision(s)") {
		t.Fatalf("run 4: code=%d out=%q err=%q", code, out, errs)
	}
	iv, ok, _ := db.IntervalAt(sq, base+300)
	if !ok || iv.Project != "phd" || iv.RuleID != attribute.RuleCallProject || iv.From != base || iv.To != base+600 {
		t.Fatalf("interval after decision: %+v ok=%v", iv, ok)
	}
	c, _, _ := db.ReadCall(sq, base)
	if c.AppliedAt == 0 {
		t.Fatalf("applied_at not set: %+v", c)
	}
	// 600 s of call plus the ten unmatched Music heartbeats after it (base+600 … base+1260),
	// which carry over from the decided block like after any attributed span.
	if iv, ok, _ := db.IntervalAt(sq, base+900); !ok || iv.Project != "phd" || iv.RuleID != attribute.RuleCarryover {
		t.Fatalf("carry-over after the decided call: %+v ok=%v", iv, ok)
	}
	var phd int64
	sq.QueryRow(`SELECT day_seconds FROM totals WHERE project = 'phd' AND day = '2026-09-16'`).Scan(&phd)
	if phd != 1260 {
		t.Fatalf("phd totals = %d", phd)
	}
	// Run 5: nothing left to apply.
	if _, out, _ := e.exec(t, wed10.Add(37*time.Minute), "run"); strings.Contains(out, "applied") {
		t.Fatalf("run 5 re-applied: %q", out)
	}
}

// Applying a decision rebuilds to the newest sample and moves the watermark before ingest
// runs; call detection must still read from where the run's rebuild would have started
// before that, or a call closed before the last cold point is never asked about.
func TestDetectionSurvivesAppliedDecision(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	music := func(from, to int64) []attribute.Sample {
		var s []attribute.Sample
		for ts := from; ts < to; ts += 60 {
			s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
		}
		return s
	}
	// Run 0: pre-call rows only, so a watermark exists before call A (detection needs one).
	e.insert(t, music(base-2400, base-1800)...)
	if code, out, errs := e.exec(t, time.Unix(base-1800, 0), "run"); code != 0 {
		t.Fatalf("run 0: code=%d out=%q err=%q", code, out, errs)
	}
	// Run 1: call A (base-1800 … base-1200), then Music rows to base-600 that close it.
	e.insert(t, zoomCall(base-1800)...)
	if code, out, errs := e.exec(t, time.Unix(base-600, 0), "run"); code != 0 || !strings.Contains(out, "awaiting the pop-up") {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if _, found, _ := db.ReadCall(sq, base-1800); !found {
		t.Fatal("run 1 inserted no calls row for call A")
	}
	if _, err := db.SetDecision(sq, base-1800, attribute.DecisionProject, "phd", "", base-500); err != nil {
		t.Fatal(err)
	}
	// Call B (base … base+600), Music to base+1200, an idle row at base+1500 (a cold point),
	// then Music base+2700 … base+3300. Run 2's decision rebuild moves the watermark to the
	// last row, whose rebuild window starts at the idle row, after call B.
	e.insert(t, zoomCall(base)...)
	e.insert(t, attribute.Sample{TS: base + 1500, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 300})
	e.insert(t, music(base+2700, base+3300)...)
	code, out, errs := e.exec(t, time.Unix(base+3400, 0), "run")
	if code != 0 || !strings.Contains(out, "applied 1 call decision(s)") {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	if _, found, _ := db.ReadCall(sq, base); !found {
		t.Fatalf("run 2 inserted no calls row for call B; out=%q", out)
	}
}

// A decided call before the retention horizon cannot be rebuilt: logged, marked applied,
// the run still succeeds.
func TestApplyDecisionBeforeHorizonIsMarkedApplied(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, attribute.Sample{TS: base, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	old := base - 40*86400
	sq, _ := db.Open(e.dbPath)
	db.InsertCall(sq, db.Call{FromTS: old, ToTS: old + 600, CallApp: "x", GuessProject: "acme", Breakdown: "[]"})
	db.SetDecision(sq, old, attribute.DecisionProject, "phd", "", base)
	sq.Close()
	code, out, errs := e.exec(t, wed10.Add(5*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "intervals left as they are") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ = db.Open(e.dbPath)
	defer sq.Close()
	if c, _, _ := db.ReadCall(sq, old); c.AppliedAt == 0 {
		t.Fatalf("not marked applied: %+v", c)
	}
}

// A report failure after a successful rebuild still marks the row applied (intervals and
// totals are consistent) and surfaces as a non-zero exit; ingest ran regardless.
func TestApplyDecisionReportFailureStillMarksApplied(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, zoomCall(base)...)
	sq, _ := db.Open(e.dbPath)
	db.InsertCall(sq, db.Call{FromTS: base, ToTS: base + 600, CallApp: "us.zoom.xos", GuessProject: "acme", Breakdown: "[]"})
	db.SetDecision(sq, base, attribute.DecisionProject, "phd", "", base+1300)
	sq.Close()
	if err := os.WriteFile(e.reports, []byte("not a directory"), 0o644); err != nil { // vault_reports_dir is now a file
		t.Fatal(err)
	}
	code, out, errs := e.exec(t, wed10.AddDate(0, 0, 7).Add(25*time.Minute), "run")
	if code == 0 || !strings.Contains(out, "applied 1 call decision(s)") || !strings.Contains(errs, "pop-up answers: report") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ = db.Open(e.dbPath)
	defer sq.Close()
	c, _, _ := db.ReadCall(sq, base)
	if c.AppliedAt == 0 {
		t.Fatalf("not marked applied: %+v", c)
	}
	if _, ok, _ := db.GetMeta(sq, "watermark_rowid"); !ok {
		t.Fatal("ingest did not run after the failed apply")
	}
}

func TestCallsListAndSet(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	sq, _ := db.Open(e.dbPath)
	db.InsertCall(sq, db.Call{FromTS: base, ToTS: base + 3420, CallApp: "us.zoom.xos", GuessProject: "acme", GuessSubproject: "automations", Breakdown: "[]"})
	sq.Close()
	now := wed10.Add(2 * time.Hour)

	code, out, _ := e.exec(t, now, "calls")
	if code != 0 || !strings.Contains(out, strconv.FormatInt(base, 10)) || !strings.Contains(out, "57m") || !strings.Contains(out, "acme/automations") || !strings.Contains(out, "pending") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	// Accepting the guess keeps its subproject.
	if code, _, errs := e.exec(t, now, "calls", "set", strconv.FormatInt(base, 10), "--project", "acme"); code != 0 {
		t.Fatalf("set guess: code=%d err=%q", code, errs)
	}
	sq, _ = db.Open(e.dbPath)
	c, _, _ := db.ReadCall(sq, base)
	if c.Decision != "project" || c.Project != "acme" || c.Subproject != "automations" || c.DecidedAt != now.Unix() {
		t.Fatalf("after set guess: %+v", c)
	}
	sq.Close()
	// Another project drops the subproject.
	e.exec(t, now, "calls", "set", strconv.FormatInt(base, 10), "--project", "phd")
	sq, _ = db.Open(e.dbPath)
	c, _, _ = db.ReadCall(sq, base)
	if c.Project != "phd" || c.Subproject != "" {
		t.Fatalf("after set phd: %+v", c)
	}
	sq.Close()
	// project/sub-project books both; a sub-project needs no rule.
	code, out, errs := e.exec(t, now, "calls", "set", strconv.FormatInt(base, 10), "--project", "phd/thesis")
	if code != 0 || !strings.Contains(out, "whole call to phd/thesis") {
		t.Fatalf("set phd/thesis: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ = db.Open(e.dbPath)
	c, _, _ = db.ReadCall(sq, base)
	if c.Decision != "project" || c.Project != "phd" || c.Subproject != "thesis" {
		t.Fatalf("after set phd/thesis: %+v", c)
	}
	sq.Close()
	// An explicit sub-project of the guess's project replaces the guess's.
	e.exec(t, now, "calls", "set", strconv.FormatInt(base, 10), "--project", "acme/acme-app")
	sq, _ = db.Open(e.dbPath)
	c, _, _ = db.ReadCall(sq, base)
	if c.Project != "acme" || c.Subproject != "acme-app" {
		t.Fatalf("after set acme/acme-app: %+v", c)
	}
	sq.Close()
	// Split.
	e.exec(t, now, "calls", "set", strconv.FormatInt(base, 10), "--split")
	sq, _ = db.Open(e.dbPath)
	c, _, _ = db.ReadCall(sq, base)
	sq.Close()
	if c.Decision != "split" || c.Project != "" {
		t.Fatalf("after split: %+v", c)
	}
	code, out, _ = e.exec(t, now, "calls")
	if !strings.Contains(out, "split") {
		t.Fatalf("list after split: %q", out)
	}
}

// An unknown project, a missing row, or both/neither flags fail without writing.
func TestCallsSetRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc).Unix()
	sq, _ := db.Open(e.dbPath)
	db.InsertCall(sq, db.Call{FromTS: base, ToTS: base + 600, CallApp: "x", GuessProject: "acme", Breakdown: "[]"})
	sq.Close()
	now := time.Unix(base+7200, 0)
	from := strconv.FormatInt(base, 10)
	for _, args := range [][]string{
		{"calls", "set", from, "--project", "nope"},
		{"calls", "set", from, "--project", "nope/globex"},
		{"calls", "set", from, "--project", "phd/"},
		{"calls", "set", from, "--project", "phd/a/b"},
		{"calls", "set", from, "--project", "phd/Thesis"},
		{"calls", "set", "12345", "--split"},
		{"calls", "set", from},
		{"calls", "set", from, "--project", "phd", "--split"},
		{"calls", "bogus"},
	} {
		if code, _, errs := e.exec(t, now, args...); code == 0 || errs == "" {
			t.Fatalf("%v: code=%d err=%q", args, code, errs)
		}
	}
	sq, _ = db.Open(e.dbPath)
	defer sq.Close()
	if c, _, _ := db.ReadCall(sq, base); c.Decision != "" {
		t.Fatalf("a rejected set wrote: %+v", c)
	}
}

func answerBackground(t *testing.T, e env) (wed10 time.Time, base int64) {
	t.Helper()
	wed10 = time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base = wed10.Unix()
	var pre []attribute.Sample
	for ts := base - 1200; ts < base-600; ts += 60 {
		pre = append(pre, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	e.insert(t, pre...)
	if code, out, errs := e.exec(t, wed10.Add(-9*time.Minute), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	e.insert(t, zoomCall(base)...)
	if code, out, errs := e.exec(t, wed10.Add(22*time.Minute), "run"); code != 0 {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs := e.exec(t, wed10.Add(30*time.Minute), "calls", "set", strconv.FormatInt(base, 10), "--background")
	if code != 0 || !strings.Contains(out, "background; applied on the next run") {
		t.Fatalf("calls set: code=%d out=%q err=%q", code, out, errs)
	}
	return wed10, base
}

func TestCallsSetBackground(t *testing.T) {
	e := newEnv(t)
	wed10, base := answerBackground(t, e)
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if c, found, _ := db.ReadCall(sq, base); !found || c.Decision != "background" || c.Project != "" {
		t.Fatalf("call: %+v found=%v", c, found)
	}
	if _, out, _ := e.exec(t, wed10.Add(31*time.Minute), "calls"); !strings.Contains(out, "background") {
		t.Fatalf("calls list: %q", out)
	}
	from := strconv.FormatInt(base, 10)
	for _, args := range [][]string{{"calls", "set", from, "--background", "--split"}, {"calls", "set", from}} {
		code, _, errs := e.exec(t, wed10.Add(32*time.Minute), args...)
		if code != 1 || !strings.Contains(errs, "give exactly one of --project NAME, --split or --background") {
			t.Fatalf("%v: code=%d err=%q", args, code, errs)
		}
	}
}

func TestExplainInBackgroundCall(t *testing.T) {
	e := newEnv(t)
	wed10, _ := answerBackground(t, e)
	_, out, errs := e.exec(t, wed10.Add(31*time.Minute), "explain", "2026-09-16T10:05:00+03:00")
	if !strings.Contains(out, "in call  yes, answered background (idle counts as away)") || strings.Contains(out, "idle ignored") {
		t.Fatalf("explain: out=%q err=%q", out, errs)
	}
}

// backgroundCall: an hour of Zoom in the background: acme work to base+600, WhatsApp to base+2400,
// then Ghostty untouched to base+3600 (idle 60 s at base+2400); then Music, out of the call,
// and a sleep row.
func backgroundCall(base int64) []attribute.Sample {
	ghostty := func(ts, idle int64) attribute.Sample {
		return attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", Title: "zsh", CWD: "/Users/me/dev-projects/acme/automations", IdleS: idle, CallApp: "us.zoom.xos"}
	}
	var s []attribute.Sample
	for ts := base; ts <= base+540; ts += 60 {
		s = append(s, ghostty(ts, 2))
	}
	for ts := base + 600; ts <= base+2340; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2, CallApp: "us.zoom.xos"})
	}
	for ts := base + 2400; ts <= base+3540; ts += 60 {
		s = append(s, ghostty(ts, ts-(base+2340)))
	}
	for ts := base + 3600; ts <= base+3840; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	return append(s, attribute.Sample{TS: base + 3900, Kind: "sleep"})
}

// The background hours the pop-up shows are what the answer books; re-answering
// split books the idle part again.
func TestBackgroundBreakdownMatchesTheBooks(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc).Unix()
	e.insert(t, acmeBlock(base-3600)...)
	if code, out, errs := e.exec(t, time.Unix(base-2900, 0), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	e.insert(t, backgroundCall(base)...)
	if code, out, errs := e.exec(t, time.Unix(base+3960, 0), "run"); code != 0 {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	c, found, _ := db.ReadCall(sq, base)
	if !found || c.ToTS != base+3600 || !strings.Contains(c.Breakdown, `{"background":"acme/automations","seconds":1200},{"background":"unattributed","seconds":1140},{"background":"away","seconds":1260}`) {
		t.Fatalf("call: %+v found=%v", c, found)
	}
	from := strconv.FormatInt(base, 10)
	if code, _, errs := e.exec(t, time.Unix(base+4020, 0), "calls", "set", from, "--background"); code != 0 {
		t.Fatalf("calls set --background: code=%d err=%q", code, errs)
	}
	if code, out, errs := e.exec(t, time.Unix(base+4080, 0), "run"); code != 0 || !strings.Contains(out, "applied 1 call decision(s)") {
		t.Fatalf("run 3: code=%d out=%q err=%q", code, out, errs)
	}
	ivs, err := db.ReadIntervals(sq, base, base+3600)
	if err != nil {
		t.Fatal(err)
	}
	booked := map[string]int64{}
	for _, iv := range ivs {
		name := iv.Project
		if iv.Subproject != "" {
			name += "/" + iv.Subproject
		}
		booked[name] += min(iv.To, base+3600) - max(iv.From, base)
	}
	if want := map[string]int64{"acme/automations": 1200, attribute.Unattributed: 1140}; !reflect.DeepEqual(booked, want) {
		t.Fatalf("booked %v, want %v", booked, want)
	}
	if code, _, errs := e.exec(t, time.Unix(base+4140, 0), "calls", "set", from, "--split"); code != 0 {
		t.Fatalf("calls set --split: code=%d err=%q", code, errs)
	}
	if code, out, errs := e.exec(t, time.Unix(base+4200, 0), "run"); code != 0 {
		t.Fatalf("run 4: code=%d out=%q err=%q", code, out, errs)
	}
	if _, ok, _ := db.IntervalAt(sq, base+3000); !ok {
		t.Fatal("split did not book the idle part of the call again")
	}
}

// The background hours see the other decided calls, in detection and in the refresh: call B
// carries over from call A, which is answered phd (undecided, A books to acme).
func TestBackgroundHoursSeeOtherDecidedCalls(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc).Unix()
	e.insert(t, acmeBlock(base-3600)...)
	if code, out, errs := e.exec(t, time.Unix(base-2900, 0), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	callA := zoomCall(base)[:10]
	for ts := base + 600; ts <= base+720; ts += 60 {
		callA = append(callA, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	e.insert(t, callA...)
	if code, out, errs := e.exec(t, time.Unix(base+780, 0), "run"); code != 0 {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	if code, _, errs := e.exec(t, time.Unix(base+790, 0), "calls", "set", strconv.FormatInt(base, 10), "--project", "phd"); code != 0 {
		t.Fatalf("calls set: code=%d err=%q", code, errs)
	}
	var callB []attribute.Sample
	for ts := base + 780; ts <= base+1080; ts += 60 {
		callB = append(callB, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2, CallApp: "us.zoom.xos"})
	}
	e.insert(t, append(callB, attribute.Sample{TS: base + 1140, Kind: "sleep"})...)
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	const want = `{"background":"phd","seconds":360},{"background":"away","seconds":0}`
	if code, out, errs := e.exec(t, time.Unix(base+1200, 0), "run"); code != 0 {
		t.Fatalf("run 3: code=%d out=%q err=%q", code, out, errs)
	}
	if c, found, _ := db.ReadCall(sq, base+780); !found || !strings.Contains(c.Breakdown, want) {
		t.Fatalf("detected call B: %+v found=%v", c, found)
	}
	if code, out, errs := e.exec(t, time.Unix(base+1260, 0), "recompute", "--since", "2026-09-16"); code != 0 {
		t.Fatalf("recompute: code=%d out=%q err=%q", code, out, errs)
	}
	if c, _, _ := db.ReadCall(sq, base+780); !strings.Contains(c.Breakdown, want) {
		t.Fatalf("refreshed call B: %+v", c)
	}
}

// A background answer applied by a run raises no stretch pop-up for the call's
// unattributed time in that run's detection.
func TestBackgroundAnswerRaisesNoStretch(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc).Unix()
	e.insert(t, acmeBlock(base-3600)...)
	if code, out, errs := e.exec(t, time.Unix(base-2900, 0), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	e.insert(t, backgroundCall(base)...)
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if _, err := db.InsertCall(sq, db.Call{FromTS: base, ToTS: base + 3600, CallApp: "us.zoom.xos", GuessProject: "acme", GuessSubproject: "automations", Breakdown: "[]"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetDecision(sq, base, attribute.DecisionBackground, "", "", base+3950); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := e.exec(t, time.Unix(base+3960, 0), "run"); code != 0 || !strings.Contains(out, "applied 1 call decision(s)") {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	if st, err := db.ReadStretches(sq, 0); err != nil || len(st) != 0 {
		t.Fatalf("stretches: %+v err=%v", st, err)
	}
	if rows, err := db.ReadCalls(sq, 0); err != nil || len(rows) != 1 {
		t.Fatalf("calls: %+v err=%v", rows, err)
	}
	if code, out, errs := e.exec(t, time.Unix(base+4260, 0), "run"); code != 0 {
		t.Fatalf("run 3: code=%d out=%q err=%q", code, out, errs)
	}
	if st, err := db.ReadStretches(sq, 0); err != nil || len(st) != 0 {
		t.Fatalf("stretches after run 3: %+v err=%v", st, err)
	}
	if rows, err := db.ReadCalls(sq, 0); err != nil || len(rows) != 1 {
		t.Fatalf("calls after run 3: %+v err=%v", rows, err)
	}
	// Without the background answer this minute books to acme (the call's first match).
	if iv, ok, _ := db.IntervalAt(sq, base+1500); !ok || iv.Project != attribute.Unattributed {
		t.Fatalf("interval at base+1500: %+v ok=%v", iv, ok)
	}
}

func TestExplainDescribesCallRuleIDs(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc).Unix()
	e.insert(t, attribute.Sample{TS: base, Kind: "heartbeat", App: "us.zoom.xos", Title: "Zoom", IdleS: 2, CallApp: "us.zoom.xos"})
	sq, _ := db.Open(e.dbPath)
	tx, _ := sq.Begin()
	db.InsertIntervals(tx, []attribute.Interval{{From: base, To: base + 60, Project: "phd", RuleID: attribute.RuleCallProject}, {From: base + 60, To: base + 120, Project: "acme", RuleID: attribute.RuleCallMeeting}})
	tx.Commit()
	sq.Close()
	now := time.Unix(base+3600, 0)
	if _, out, _ := e.exec(t, now, "explain", strconv.FormatInt(base+30, 10)); !strings.Contains(out, "meeting pop-up") {
		t.Fatalf("call:project: %q", out)
	}
	if _, out, _ := e.exec(t, now, "explain", strconv.FormatInt(base+90, 10)); !strings.Contains(out, "guessed project") {
		t.Fatalf("call:meeting: %q", out)
	}
}

// whatsapp: one WhatsApp heartbeat a minute in [from, to) (no rule matches it), then a sleep
// row at `to` that closes the stretch.
func whatsapp(from, to int64) []attribute.Sample {
	var s []attribute.Sample
	for ts := from; ts < to; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2})
	}
	return append(s, attribute.Sample{TS: to, Kind: "sleep"})
}

// acmeBlock: ten minutes in an acme repo from `from`, then a sleep row (nothing carries over).
func acmeBlock(from int64) []attribute.Sample {
	var s []attribute.Sample
	for ts := from; ts < from+600; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", Title: "zsh", CWD: "/Users/me/dev-projects/acme/automations", IdleS: 2})
	}
	return append(s, attribute.Sample{TS: from + 600, Kind: "sleep"})
}

func TestRunDetectsStretchesAndWritesPicker(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, acmeBlock(base-3600)...)
	if code, out, errs := e.exec(t, wed10.Add(-50*time.Minute), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	e.insert(t, whatsapp(base, base+1200)...)
	code, out, errs := e.exec(t, wed10.Add(21*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "stretch 2026-09-16T10:00:00+03:00–2026-09-16T10:20:00+03:00: 20 min unattributed, awaiting the pop-up") {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	rows, _ := db.ReadStretches(sq, 0)
	if len(rows) != 1 || rows[0].FromTS != base || rows[0].ToTS != base+1200 || rows[0].UnattributedS != 1200 ||
		rows[0].Breakdown != `[{"key":"app:net.whatsapp.WhatsApp","seconds":1200}]` || rows[0].Answer != "" {
		t.Fatalf("stretches: %+v", rows)
	}
	if v, _, _ := db.GetMeta(sq, "picker"); v != `["acme","acme/automations","phd"]` {
		t.Fatalf("meta.picker = %q", v)
	}
	if _, out, _ := e.exec(t, wed10.Add(26*time.Minute), "run"); strings.Contains(out, "awaiting the pop-up") {
		t.Fatalf("run 3 re-detected: %q", out)
	}
}

// A stretch that a call start closes and that call's row never share a minute: the stretch
// ends where the call begins, and no in-call span (here WhatsApp stays frontmost in the call)
// reaches the stretch's breakdown. The two panels can be answered in any order.
func TestStretchEndedByCallAndTheCallAreDisjoint(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, acmeBlock(base-3600)...)
	e.exec(t, wed10.Add(-50*time.Minute), "run")
	var s []attribute.Sample
	for ts := base; ts < base+1800; ts += 60 {
		call := ""
		if ts >= base+1200 {
			call = "us.zoom.xos"
		}
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2, CallApp: call})
	}
	for ts := base + 1800; ts < base+2100; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	e.insert(t, append(s, attribute.Sample{TS: base + 2100, Kind: "sleep"})...)
	if code, out, errs := e.exec(t, wed10.Add(36*time.Minute), "run"); code != 0 || strings.Count(out, "awaiting the pop-up") != 2 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	st, _ := db.ReadStretches(sq, 0)
	cl, _ := db.ReadCalls(sq, 0)
	if len(st) != 1 || len(cl) != 1 || st[0].ToTS > cl[0].FromTS || st[0].ToTS != base+1200 || cl[0].FromTS != base+1200 {
		t.Fatalf("stretches %+v calls %+v", st, cl)
	}
	if st[0].Breakdown != `[{"key":"app:net.whatsapp.WhatsApp","seconds":1200}]` {
		t.Fatalf("in-call minutes leaked into the stretch: %s", st[0].Breakdown)
	}
}

// recompute --since refreshes unanswered rows but inserts none for rows a run already
// ingested; a rule written by hand deletes a never-decided row it now covers.
func TestRecomputeRefreshesButInsertsNoIngestedStretch(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, whatsapp(base, base+1200)...)
	now := wed10.Add(30 * time.Minute)
	if code, out, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: code=%d out=%q err=%q", code, out, errs)
	}
	if code, out, errs := e.exec(t, now, "recompute", "--since", "2026-09-16"); code != 0 || strings.Contains(out, "awaiting the pop-up") {
		t.Fatalf("recompute: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if rows, _ := db.ReadStretches(sq, 0); len(rows) != 0 {
		t.Fatalf("recompute inserted: %+v", rows)
	}
	db.InsertStretch(sq, db.Stretch{FromTS: base, ToTS: base + 1200, UnattributedS: 1, Breakdown: "[]"})
	if code, out, _ := e.exec(t, now, "recompute", "--since", "2026-09-16"); code != 0 || !strings.Contains(out, "refreshed 1 pending stretch(es), dropped 0") {
		t.Fatalf("refresh: %q", out)
	}
	if s, _, _ := db.ReadStretch(sq, base); s.UnattributedS != 1200 || s.Breakdown != `[{"key":"app:net.whatsapp.WhatsApp","seconds":1200}]` {
		t.Fatalf("refreshed row: %+v", s)
	}
	f, _ := os.OpenFile(e.cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n[[rules]]\nproject = \"phd\"\napp = \"net.whatsapp.WhatsApp\"\n")
	f.Close()
	if code, out, _ := e.exec(t, now, "recompute", "--since", "2026-09-16"); code != 0 || !strings.Contains(out, "dropped 1 that rules now cover") {
		t.Fatalf("rules edit: %q", out)
	}
	if rows, _ := db.ReadStretches(sq, 0); len(rows) != 0 {
		t.Fatalf("covered row kept: %+v", rows)
	}
}

// A manual recompute moves the watermark past rows no run has ingested yet; a stretch and a
// call closed by those rows must still get their pop-up rows.
func TestRecomputeAsksAboutCallsAndStretchesClosedSinceTheLastRun(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, acmeBlock(base-3600)...)
	if code, out, errs := e.exec(t, wed10.Add(-50*time.Minute), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	e.insert(t, whatsapp(base, base+1200)...)
	e.insert(t, zoomCall(base+1800)...)
	if code, out, errs := e.exec(t, wed10.Add(51*time.Minute), "recompute", "--since", "2026-09-16"); code != 0 {
		t.Fatalf("recompute: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if _, found, _ := db.ReadStretch(sq, base); !found {
		t.Error("no stretches row for the WhatsApp stretch")
	}
	if _, found, _ := db.ReadCall(sq, base+1800); !found {
		t.Error("no calls row for the Zoom call")
	}
}

// A recompute that fails after its rebuild (here totals) has already moved the watermark, and
// the run that resumes it captures the moved one, so the recompute must still ask.
func TestFailedRecomputeStillAsksAboutCallsItRebuilt(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, acmeBlock(base-3600)...)
	if code, out, errs := e.exec(t, wed10.Add(-50*time.Minute), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	e.insert(t, zoomCall(base)...)
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if _, err := sq.Exec(`DROP TABLE totals`); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := e.exec(t, wed10.Add(21*time.Minute), "recompute", "--since", "2026-09-16"); code != 1 {
		t.Fatalf("recompute: want exit 1, code=%d out=%q err=%q", code, out, errs)
	}
	if _, found, _ := db.ReadCall(sq, base); !found {
		t.Error("no calls row for the Zoom call")
	}
}

// answer records a stretch answer exactly as the collector's panel does.
func (e env) answer(t *testing.T, from int64, answer string, at int64) {
	t.Helper()
	sq, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	if ok, err := db.SetStretchAnswer(sq, from, answer, at); err != nil || !ok {
		t.Fatalf("answer %d: ok=%v err=%v", from, ok, err)
	}
}

// stretchRow inserts an unanswered stretches row without detection.
func (e env) stretchRow(t *testing.T, from, to int64) {
	t.Helper()
	sq, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	if ok, err := db.InsertStretch(sq, db.Stretch{FromTS: from, ToTS: to, UnattributedS: to - from, Breakdown: "[]"}); err != nil || !ok {
		t.Fatalf("stretch row %d: ok=%v err=%v", from, ok, err)
	}
}

func (e env) rules(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func intervalAt(t *testing.T, sq *sql.DB, ts int64) string {
	t.Helper()
	iv, ok, err := db.IntervalAt(sq, ts)
	if err != nil || !ok {
		t.Fatalf("no interval at %d (err %v)", ts, err)
	}
	return iv.Project + "/" + iv.RuleID
}

func TestApplyStretchAnswerLearnsRuleAndBooksTime(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	original := e.rules(t)
	e.insert(t, acmeBlock(base-3600)...)
	e.exec(t, wed10.Add(-50*time.Minute), "run")
	e.insert(t, whatsapp(base, base+1200)...)
	if _, out, _ := e.exec(t, wed10.Add(21*time.Minute), "run"); !strings.Contains(out, "awaiting the pop-up") {
		t.Fatalf("run 2 did not ask: %q", out)
	}
	e.answer(t, base, `[{"key":"app:net.whatsapp.WhatsApp","project":"phd","always":true,"label":"WhatsApp"}]`, base+1300)
	code, out, errs := e.exec(t, wed10.Add(25*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "applied 1 stretch answer(s): recomputed from") || !strings.Contains(out, "rules.toml: appended 1 learned rule(s)") {
		t.Fatalf("run 3: code=%d out=%q err=%q", code, out, errs)
	}
	learned := "\n# added by stint pop-up 2026-09-16 | stretch Wed 16 Sep 10:00 | WhatsApp\n[[rules]]\nproject = \"phd\"\napp = \"net.whatsapp.WhatsApp\"\n"
	if got := e.rules(t); got != original+learned {
		t.Fatalf("rules.toml:\n%s", got)
	}
	if bak, _ := os.ReadFile(e.cfgPath + ".bak"); string(bak) != original {
		t.Fatalf("rules.toml.bak:\n%s", bak)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if got := intervalAt(t, sq, base+300); got != "phd/2" { // the learned rule beats the answer
		t.Fatalf("interval: %s", got)
	}
	if s, _, _ := db.ReadStretch(sq, base); s.AppliedAt != wed10.Add(25*time.Minute).Unix() || s.RulesError != "" {
		t.Fatalf("row: %+v", s)
	}
	// The same app in a later stretch is claimed by the rule and never asked about.
	e.insert(t, whatsapp(base+3600, base+4800)...)
	if _, out, _ := e.exec(t, wed10.Add(81*time.Minute), "run"); strings.Contains(out, "awaiting the pop-up") || strings.Contains(out, "applied") {
		t.Fatalf("run 4: %q", out)
	}
	if got := e.rules(t); got != original+learned {
		t.Fatal("a later run wrote rules.toml again")
	}
	if got := intervalAt(t, sq, base+3900); got != "phd/2" {
		t.Fatalf("later stretch: %s", got)
	}
}

// One bad row (a project removed from rules.toml) gets rules_error and stays out of the
// recompute; the other row still applies. Once the project is back, the row applies and the
// health reason goes.
func TestApplyStretchFailureIsolatesRow(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, whatsapp(base, base+1200)...)
	var notes []attribute.Sample
	for ts := base + 3600; ts < base+4800; ts += 60 {
		notes = append(notes, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Notes", Title: "Notes", IdleS: 2})
	}
	e.insert(t, append(notes, attribute.Sample{TS: base + 4800, Kind: "sleep"})...)
	e.stretchRow(t, base, base+1200)
	e.stretchRow(t, base+3600, base+4800)
	e.answer(t, base, `[{"key":"app:net.whatsapp.WhatsApp","project":"ghost"}]`, base+4900)
	e.answer(t, base+3600, `[{"key":"app:com.apple.Notes","project":"phd","always":true,"label":"Notes"}]`, base+4900)
	code, out, errs := e.exec(t, wed10.Add(90*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "answer not applied: project \"ghost\" is not in rules.toml") || !strings.Contains(out, "applied 1 stretch answer(s)") {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	bad, _, _ := db.ReadStretch(sq, base)
	if bad.AppliedAt != 0 || bad.RulesError != `project "ghost" is not in rules.toml` {
		t.Fatalf("bad row: %+v", bad)
	}
	if got := intervalAt(t, sq, base+300); got != "unattributed/unattributed" {
		t.Fatalf("a failed answer reached intervals: %s", got)
	}
	if got := intervalAt(t, sq, base+3900); got != "phd/2" {
		t.Fatalf("good row: %s", got)
	}
	if v, _, _ := db.GetMeta(sq, "health"); !strings.Contains(v, `stretch answer not applied (1): project "ghost" is not in rules.toml`) {
		t.Fatalf("meta.health = %q", v)
	}
	before := e.rules(t)
	if _, out, _ := e.exec(t, wed10.Add(95*time.Minute), "run"); strings.Contains(out, ": answer not applied:") || strings.Contains(out, "appended") {
		t.Fatalf("retry logged again or rewrote rules: %q", out)
	}
	if e.rules(t) != before {
		t.Fatal("retry rewrote rules.toml")
	}
	f, _ := os.OpenFile(e.cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n[projects.ghost]\n")
	f.Close()
	if code, out, errs := e.exec(t, wed10.Add(100*time.Minute), "run"); code != 0 || !strings.Contains(out, "applied 1 stretch answer(s)") {
		t.Fatalf("after the fix: code=%d out=%q err=%q", code, out, errs)
	}
	if s, _, _ := db.ReadStretch(sq, base); s.AppliedAt == 0 || s.RulesError != "" {
		t.Fatalf("fixed row: %+v", s)
	}
	if got := intervalAt(t, sq, base+300); got != "ghost/stretch" {
		t.Fatalf("fixed interval: %s", got)
	}
	if v, _, _ := db.GetMeta(sq, "health"); strings.Contains(v, "stretch answer") {
		t.Fatalf("health reason kept: %q", v)
	}
}

// Two rows learning the same key in one run, and a rule already in the file (a half-finished
// earlier run), each end up as exactly one rule.
func TestApplyWritesEachLearnedRuleOnce(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, whatsapp(base, base+1200)...)
	e.insert(t, whatsapp(base+3600, base+4800)...)
	f, _ := os.OpenFile(e.cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n# added by stint pop-up 2026-09-15 | stretch Tue 15 Sep 09:00 | Notes\n[[rules]]\nproject = \"phd\"\napp = \"com.apple.Notes\"\n")
	f.Close()
	original := e.rules(t)
	e.stretchRow(t, base, base+1200)
	e.stretchRow(t, base+3600, base+4800)
	both := `[{"key":"app:net.whatsapp.WhatsApp","project":"phd","always":true,"label":"WhatsApp"},{"key":"app:com.apple.Notes","project":"phd","always":true,"label":"Notes"}]`
	e.answer(t, base, both, base+4900)
	e.answer(t, base+3600, both, base+4900)
	if code, out, errs := e.exec(t, wed10.Add(90*time.Minute), "run"); code != 0 || !strings.Contains(out, "applied 2 stretch answer(s)") || !strings.Contains(out, "appended 1 learned rule(s)") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	got := e.rules(t)
	if !strings.HasPrefix(got, original) || strings.Count(got, "app = \"net.whatsapp.WhatsApp\"") != 1 || strings.Count(got, "app = \"com.apple.Notes\"") != 1 {
		t.Fatalf("rules.toml:\n%s", got)
	}
}

// A failed rules.toml write fails every row with a rule to learn, also the row whose rule was
// deduped against an earlier row of the same run, so none is applied without its rule on disk.
func TestApplyFailedWriteFailsEveryRowWithRules(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, whatsapp(base, base+1200)...)
	e.insert(t, whatsapp(base+3600, base+4800)...)
	original := e.rules(t)
	e.stretchRow(t, base, base+1200)
	e.stretchRow(t, base+3600, base+4800)
	always := `[{"key":"app:net.whatsapp.WhatsApp","project":"phd","always":true,"label":"WhatsApp"}]`
	e.answer(t, base, always, base+4900)
	e.answer(t, base+3600, always, base+4900)
	if err := os.Mkdir(e.cfgPath+".bak", 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := e.exec(t, wed10.Add(90*time.Minute), "run"); code != 0 || strings.Contains(out, "stretch answer(s): recomputed") {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	if e.rules(t) != original {
		t.Fatal("rules.toml changed by a failed write")
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	for _, from := range []int64{base, base + 3600} {
		if s, _, _ := db.ReadStretch(sq, from); s.AppliedAt != 0 || s.RulesError == "" {
			t.Fatalf("row %d after the failed write: %+v", from, s)
		}
	}
	if err := os.Remove(e.cfgPath + ".bak"); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := e.exec(t, wed10.Add(95*time.Minute), "run"); code != 0 || !strings.Contains(out, "applied 2 stretch answer(s)") || !strings.Contains(out, "appended 1 learned rule(s)") {
		t.Fatalf("retry: code=%d out=%q err=%q", code, out, errs)
	}
	for _, from := range []int64{base, base + 3600} {
		if s, _, _ := db.ReadStretch(sq, from); s.AppliedAt == 0 || s.RulesError != "" {
			t.Fatalf("row %d after the retry: %+v", from, s)
		}
	}
	if got := e.rules(t); strings.Count(got, "app = \"net.whatsapp.WhatsApp\"") != 1 {
		t.Fatalf("rules.toml:\n%s", got)
	}
}

// A re-opened row from an earlier day lies before the routine rebuild's start; the apply
// recompute refreshes it before marking it applied, so it is pending again, up to date.
func TestApplyRefreshesReopenedRowBeforeMarkingIt(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	y := base - 86400
	e.insert(t, whatsapp(y, y+1200)...)
	e.insert(t, acmeBlock(base)...)
	e.insert(t, acmeBlock(base+3600)...)
	if code, out, errs := e.exec(t, time.Unix(base+4300, 0), "run"); code != 0 {
		t.Fatalf("run 1: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	db.InsertStretch(sq, db.Stretch{FromTS: y, ToTS: y + 1200, UnattributedS: 1, Breakdown: "[]"})
	db.SetStretchAnswer(sq, y, `[{"key":"app:net.whatsapp.WhatsApp","project":"phd"}]`, base+4300)
	db.ReopenStretch(sq, y, base+4310)
	if p, _ := db.ReadPending(sq); p.Stretches != 0 {
		t.Fatalf("re-opened row pending before the brain ran: %+v", p)
	}
	now := time.Unix(base+4400, 0)
	if code, out, errs := e.exec(t, now, "run"); code != 0 || !strings.Contains(out, "applied 1 stretch answer(s)") {
		t.Fatalf("run 2: code=%d out=%q err=%q", code, out, errs)
	}
	s, _, _ := db.ReadStretch(sq, y)
	if s.UnattributedS != 1200 || s.Breakdown != `[{"key":"app:net.whatsapp.WhatsApp","seconds":1200}]` || s.Answer != "" || s.AppliedAt != now.Unix() {
		t.Fatalf("row: %+v", s)
	}
	if p, _ := db.ReadPending(sq); p.Stretches != 1 || p.StretchSeconds != 1200 {
		t.Fatalf("pending: %+v", p)
	}
}

// Before the retention horizon nothing can be rebuilt: the rule is still learned and the row
// marked applied.
func TestApplyStretchBeforeHorizonStillLearnsRule(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, attribute.Sample{TS: base, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	old := base - 40*86400
	e.stretchRow(t, old, old+1200)
	e.answer(t, old, `[{"key":"app:com.apple.Notes","project":"phd","always":true,"label":"Notes"}]`, base)
	code, out, errs := e.exec(t, wed10.Add(5*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "intervals left as they are") || !strings.Contains(e.rules(t), "app = \"com.apple.Notes\"") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if s, _, _ := db.ReadStretch(sq, old); s.AppliedAt == 0 {
		t.Fatalf("not marked applied: %+v", s)
	}
}

// A stretch closed by a call start is answered and applied while the call goes on; the call
// closes later and is detected by a routine run. Its guess must carry over from the answered
// project, exactly as its intervals do.
func TestCallDetectedAfterAnsweredStretchGuessesFromIt(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, acmeBlock(base-3600)...)
	e.exec(t, wed10.Add(-50*time.Minute), "run")
	zoom := func(from, to int64) []attribute.Sample {
		var s []attribute.Sample
		for ts := from; ts < to; ts += 60 {
			s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "us.zoom.xos", Title: "Zoom", IdleS: 2, CallApp: "us.zoom.xos"})
		}
		return s
	}
	var s []attribute.Sample
	for ts := base; ts < base+1200; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2})
	}
	e.insert(t, append(s, zoom(base+1200, base+1500)...)...)
	if _, out, _ := e.exec(t, time.Unix(base+1500, 0), "run"); !strings.Contains(out, "unattributed, awaiting the pop-up") {
		t.Fatalf("run A did not ask about the stretch: %q", out)
	}
	e.answer(t, base, `[{"key":"app:net.whatsapp.WhatsApp","project":"phd"}]`, base+1510)
	e.insert(t, zoom(base+1500, base+2400)...)
	if _, out, _ := e.exec(t, time.Unix(base+1800, 0), "run"); !strings.Contains(out, "applied 1 stretch answer(s)") {
		t.Fatalf("run B did not apply the answer: %q", out)
	}
	e.insert(t, attribute.Sample{TS: base + 2400, Kind: "sleep"})
	if _, out, _ := e.exec(t, time.Unix(base+2500, 0), "run"); !strings.Contains(out, "guessed phd: awaiting the pop-up") {
		t.Fatalf("run C: %q", out)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if got := intervalAt(t, sq, base+1300); got != "phd/call" {
		t.Fatalf("call interval: %s", got)
	}
}

// An always tick on a no_always key (a browser, or a key the breakdown flagged) is applied as
// a one-off: the minutes move, no rule is written.
func TestApplyIgnoresAlwaysOnNoAlwaysKey(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	var s []attribute.Sample
	for ts := base; ts < base+600; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.google.Chrome", Title: "New Tab", IdleS: 2})
	}
	for ts := base + 600; ts < base+1200; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", Title: "zsh", IdleS: 2})
	}
	e.insert(t, append(s, attribute.Sample{TS: base + 1200, Kind: "sleep"})...)
	original := e.rules(t)
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	db.InsertStretch(sq, db.Stretch{FromTS: base, ToTS: base + 1200, UnattributedS: 1200,
		Breakdown: `[{"key":"app:com.google.Chrome","seconds":600},{"key":"app:com.mitchellh.ghostty","seconds":600,"no_always":true}]`})
	e.answer(t, base, `[{"key":"app:com.google.Chrome","project":"phd","always":true,"label":"Google Chrome"},{"key":"app:com.mitchellh.ghostty","project":"phd","always":true,"label":"Ghostty"}]`, base+1300)
	code, out, errs := e.exec(t, wed10.Add(25*time.Minute), "run")
	if code != 0 || strings.Count(out, "(no_always key); applied as a one-off") != 2 || !strings.Contains(out, "applied 1 stretch answer(s)") || strings.Contains(out, "appended") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if e.rules(t) != original {
		t.Fatal("a rule was written for a no_always key")
	}
	if _, err := os.Stat(e.cfgPath + ".bak"); err == nil {
		t.Fatal("rules.toml.bak written although nothing changed")
	}
	if got := intervalAt(t, sq, base+300); got != "phd/stretch" {
		t.Fatalf("chrome minutes: %s", got)
	}
	if got := intervalAt(t, sq, base+900); got != "phd/stretch" {
		t.Fatalf("ghostty minutes: %s", got)
	}
}

// A stretch from 23:50 to 00:10 is one row; answered, each day's totals get its own part.
func TestApplyStretchAcrossMidnight(t *testing.T) {
	e := newEnv(t)
	mid := time.Date(2026, 9, 17, 0, 0, 0, 0, e.loc).Unix()
	e.insert(t, acmeBlock(mid-7200)...)
	e.exec(t, time.Unix(mid-6000, 0), "run")
	e.insert(t, whatsapp(mid-600, mid+600)...)
	if _, out, _ := e.exec(t, time.Unix(mid+700, 0), "run"); strings.Count(out, "awaiting the pop-up") != 1 {
		t.Fatalf("detect: %q", out)
	}
	e.answer(t, mid-600, `[{"key":"app:net.whatsapp.WhatsApp","project":"phd"}]`, mid+800)
	if code, out, errs := e.exec(t, time.Unix(mid+900, 0), "run"); code != 0 || !strings.Contains(out, "applied 1 stretch answer(s)") {
		t.Fatalf("apply: code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	for _, day := range []string{"2026-09-16", "2026-09-17"} {
		var phd int64
		sq.QueryRow(`SELECT day_seconds FROM totals WHERE project = 'phd' AND day = ?`, day).Scan(&phd)
		if phd != 600 {
			t.Fatalf("phd on %s = %d, want 600", day, phd)
		}
	}
}

// A decided call and an answered stretch share one recompute, and the pending call that ended
// the stretch is refreshed in it: its guess now carries over from the answered project.
func TestApplySharesOneRecomputeWithCalls(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, zoomCall(base-7200)...)
	e.insert(t, attribute.Sample{TS: base - 6000, Kind: "sleep"})
	var s []attribute.Sample
	for ts := base; ts < base+1200; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2})
	}
	for ts := base + 1200; ts < base+1560; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "us.zoom.xos", Title: "Zoom", IdleS: 2, CallApp: "us.zoom.xos"})
	}
	for ts := base + 1560; ts < base+2160; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.apple.Music", Title: "Music", IdleS: 2})
	}
	e.insert(t, s...)
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	db.InsertCall(sq, db.Call{FromTS: base - 7200, ToTS: base - 6600, CallApp: "us.zoom.xos", GuessProject: "acme", Breakdown: "[]"})
	db.SetDecision(sq, base-7200, attribute.DecisionProject, "phd", "", base)
	db.InsertCall(sq, db.Call{FromTS: base + 1200, ToTS: base + 1560, CallApp: "us.zoom.xos", GuessProject: attribute.Unattributed, Breakdown: `[{"project":"meeting","seconds":360}]`})
	e.stretchRow(t, base, base+1200)
	e.answer(t, base, `[{"key":"app:net.whatsapp.WhatsApp","project":"phd"}]`, base+2200)
	code, out, errs := e.exec(t, wed10.Add(40*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "applied 1 call decision(s) and 1 stretch answer(s): recomputed from") || strings.Count(out, "recomputed from") != 1 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if c, _, _ := db.ReadCall(sq, base+1200); c.GuessProject != "phd" {
		t.Fatalf("pending call not refreshed with the answer: %+v", c)
	}
	if got := intervalAt(t, sq, base+300); got != "phd/stretch" {
		t.Fatalf("answered stretch: %s", got)
	}
}

// A rule learned in this run's apply step reaches the rest of the same run: ingest books the
// same app to it (no second stretch row), and a call detected in that run guesses with it.
func TestLearnedRuleReachesTheRestOfTheRun(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, acmeBlock(base-3600)...)
	e.exec(t, wed10.Add(-50*time.Minute), "run")
	e.insert(t, whatsapp(base, base+1200)...)
	e.exec(t, wed10.Add(21*time.Minute), "run")
	e.answer(t, base, `[{"key":"app:net.whatsapp.WhatsApp","project":"phd","always":true,"label":"WhatsApp"}]`, base+1300)
	later := base + 3600
	var s []attribute.Sample
	for ts := later; ts < later+1200; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2})
	}
	for ts := later + 1200; ts < later+1560; ts += 60 {
		s = append(s, attribute.Sample{TS: ts, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2, CallApp: "us.zoom.xos"})
	}
	e.insert(t, append(s, attribute.Sample{TS: later + 1560, Kind: "sleep"})...)
	code, out, errs := e.exec(t, time.Unix(later+1700, 0), "run")
	if code != 0 || !strings.Contains(out, "appended 1 learned rule(s)") || strings.Contains(out, "unattributed, awaiting the pop-up") || !strings.Contains(out, "guessed phd: awaiting the pop-up") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if c, _, _ := db.ReadCall(sq, later+1200); c.Breakdown != `[{"project":"phd","seconds":360},{"background":"phd","seconds":360},{"background":"away","seconds":0}]` {
		t.Fatalf("call: %+v", c)
	}
}

// A key repeated inside one answer is learned once, from the entry attribution uses (the
// last); an always on a Leave entry writes no rule and is not an error.
func TestApplyLearnsARepeatedKeyOnceAndNeverLeave(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	e.insert(t, whatsapp(base, base+1200)...)
	e.stretchRow(t, base, base+1200)
	e.answer(t, base, `[{"key":"app:net.whatsapp.WhatsApp","project":"acme","always":true,"label":"WhatsApp"},{"key":"app:com.apple.Notes","project":"unattributed","always":true,"label":"Notes"},{"key":"app:net.whatsapp.WhatsApp","project":"phd","always":true,"label":"WhatsApp"}]`, base+1300)
	code, out, errs := e.exec(t, wed10.Add(25*time.Minute), "run")
	if code != 0 || !strings.Contains(out, "applied 1 stretch answer(s)") || !strings.Contains(out, "appended 1 learned rule(s)") || strings.Contains(out, "not applied") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	got := e.rules(t)
	if strings.Count(got, "app = \"net.whatsapp.WhatsApp\"") != 1 || strings.Contains(got, "com.apple.Notes") || strings.Contains(got, "project = \"acme\"\napp") {
		t.Fatalf("rules.toml:\n%s", got)
	}
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	if got := intervalAt(t, sq, base+300); got != "phd/2" {
		t.Fatalf("interval: %s", got)
	}
	if s, _, _ := db.ReadStretch(sq, base); s.AppliedAt == 0 || s.RulesError != "" {
		t.Fatalf("row: %+v", s)
	}
}

const learnedRules = `
# added by stint pop-up 2026-09-10 | stretch Thu 10 Sep 11:57 | grok.com
[[rules]]
project = "phd"
url_regex = '(?i)^https?://([^/?#]+\.)?grok\.com(:\d+)?([/?#]|$)'

# added by stint pop-up 2026-09-12 | stretch Sat 12 Sep 09:00 | Claude
[[rules]]
project = "acme"
app = "com.anthropic.claudefordesktop"
`

func TestStretchesListsRowsAndLearnedRules(t *testing.T) {
	e := newEnv(t)
	wed10 := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc)
	base := wed10.Unix()
	f, _ := os.OpenFile(e.cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(learnedRules)
	f.Close()
	e.insert(t, attribute.Sample{TS: base - 30*86400, Kind: "heartbeat", App: "X", Title: "x"})
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	tx, _ := sq.Begin()
	db.InsertIntervals(tx, []attribute.Interval{
		{From: base - 2*86400, To: base - 2*86400 + 3600, Project: "phd", RuleID: "2"},
		{From: base - 20*86400, To: base - 20*86400 + 7200, Project: "phd", RuleID: "2"},
		{From: base - 40*86400, To: base - 40*86400 + 3600, Project: "phd", RuleID: "2"},
	})
	tx.Commit()
	h := int64(3600)
	for i, from := range []int64{base - 8*h, base - 6*h, base - 4*h, base - 2*h} {
		db.InsertStretch(sq, db.Stretch{FromTS: from, ToTS: from + 1200, UnattributedS: 1200,
			Breakdown: `[{"key":"host:grok.com","seconds":700},{"key":"app:net.whatsapp.WhatsApp","seconds":400},{"key":"app:x","seconds":60},{"key":"app:y","seconds":40}]`})
		if i > 0 {
			db.SetStretchAnswer(sq, from, `[]`, base)
		}
	}
	db.MarkStretchApplied(sq, base-4*h, base, base+1)
	db.SetStretchRulesError(sq, base-2*h, "boom")
	code, out, errs := e.exec(t, wed10, "stretches")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	for _, want := range []string{
		strconv.FormatInt(base-8*h, 10) + "  Wed 16 Sep 02:00–02:20   20m  grok.com, net.whatsapp.WhatsApp (app), x (app) pending\n",
		"answered", "applied", "error: boom",
		"learned rule → project     7d    30d\n",
		"grok.com     → phd       1.0h   3.0h\n",
		"Claude (app) → acme      0.0h   0.0h\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	// A hand-written rule inserted above shifts every learned index; the view re-derives them.
	content, _ := os.ReadFile(e.cfgPath)
	shifted := strings.Replace(string(content), "[[rules]]", "[[rules]]\nproject = \"phd\"\napp = \"com.x\"\n\n[[rules]]", 1)
	os.WriteFile(e.cfgPath, []byte(shifted), 0o644)
	if _, out, _ := e.exec(t, wed10, "stretches"); !strings.Contains(out, "grok.com     → phd       0.0h   0.0h\n") {
		t.Fatalf("after a rule inserted above:\n%s", out)
	}
	// Rules written as inline tables cannot be mapped to header lines.
	inline := "rules = [ { project = \"phd\", app = \"x\" } ]\n[settings]\nvault_reports_dir = \"" + e.reports + "\"\n[projects.phd]\n"
	os.WriteFile(e.cfgPath, []byte(inline), 0o644)
	if _, out, _ := e.exec(t, wed10, "stretches"); !strings.Contains(out, "learned rules: cannot map rule indexes") {
		t.Fatalf("inline tables:\n%s", out)
	}
}

func TestStretchesReopen(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc).Unix()
	e.stretchRow(t, base, base+1200)
	e.stretchRow(t, base+3600, base+4800)
	e.answer(t, base+3600, `[{"key":"app:x","project":"ghost"}]`, base+5000)
	sq, _ := db.Open(e.dbPath)
	defer sq.Close()
	db.SetStretchRulesError(sq, base+3600, `project "ghost" is not in rules.toml`)
	now := time.Unix(base+6000, 0)
	for _, args := range [][]string{
		{"stretches", "reopen", strconv.FormatInt(base, 10)}, // no answer yet
		{"stretches", "reopen", "12345"},
		{"stretches", "reopen"},
		{"stretches", "reopen", "x"},
		{"stretches", "bogus"},
	} {
		if code, _, errs := e.exec(t, now, args...); code == 0 || errs == "" {
			t.Fatalf("%v: code=%d err=%q", args, code, errs)
		}
	}
	if s, _, _ := db.ReadStretch(sq, base); s.DecidedAt != 0 {
		t.Fatalf("a refused reopen wrote: %+v", s)
	}
	code, out, errs := e.exec(t, now, "stretches", "reopen", strconv.FormatInt(base+3600, 10))
	if code != 0 || !strings.Contains(out, "re-opened") {
		t.Fatalf("reopen: code=%d out=%q err=%q", code, out, errs)
	}
	if s, _, _ := db.ReadStretch(sq, base+3600); s.Answer != "" || s.DecidedAt != now.Unix() || s.RulesError != "" {
		t.Fatalf("after reopen: %+v", s)
	}
}

func TestExplainDescribesStretchRuleIDsAndSubprojects(t *testing.T) {
	e := newEnv(t)
	f, _ := os.OpenFile(e.cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n[[rules]]\nproject = \"phd\"\napp = \"com.apple.Notes\"\nsubproject = \"thesis\"\n")
	f.Close()
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, e.loc).Unix()
	e.insert(t, attribute.Sample{TS: base, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp", IdleS: 2},
		attribute.Sample{TS: base + 120, Kind: "heartbeat", App: "com.apple.Notes", Title: "Notes", IdleS: 2})
	sq, _ := db.Open(e.dbPath)
	tx, _ := sq.Begin()
	db.InsertIntervals(tx, []attribute.Interval{{From: base, To: base + 60, Project: "phd", RuleID: attribute.RuleStretch}, {From: base + 60, To: base + 120, Project: attribute.Unattributed, RuleID: attribute.RuleStretchLeave}})
	tx.Commit()
	sq.Close()
	now := time.Unix(base+3600, 0)
	if _, out, _ := e.exec(t, now, "explain", strconv.FormatInt(base+30, 10)); !strings.Contains(out, "booked by the stretch pop-up (stint stretches)") {
		t.Fatalf("stretch: %q", out)
	}
	if _, out, _ := e.exec(t, now, "explain", strconv.FormatInt(base+90, 10)); !strings.Contains(out, "left unattributed in the stretch pop-up") {
		t.Fatalf("stretch:leave: %q", out)
	}
	if _, out, _ := e.exec(t, now, "explain", strconv.FormatInt(base+130, 10)); !strings.Contains(out, "matches  rule 2: project=phd subproject=thesis app=com.apple.Notes") {
		t.Fatalf("subproject: %q", out)
	}
}

// A dated correction above an undated cwd rule books only its own local day: run, recompute
// and explain all apply the same window.
func TestDatedRuleAcrossLocalMidnight(t *testing.T) {
	e := newEnv(t)
	f, _ := os.OpenFile(e.cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`
[[rules]]
project = "acme"
cwd_prefix = "/Users/me/dev-projects/freelance"
subproject = "globex"
from = "2026-09-16"
until = "2026-09-16"

[[rules]]
project = "phd"
cwd_prefix = "/Users/me/dev-projects/freelance"
`)
	f.Close()
	midnight := time.Date(2026, 9, 16, 0, 0, 0, 0, e.loc).Unix()
	var samples []attribute.Sample
	for ts := midnight - 300; ts <= midnight+300; ts += 60 {
		samples = append(samples, attribute.Sample{TS: ts, Kind: "heartbeat", App: "com.mitchellh.ghostty", Title: "zsh", CWD: "/Users/me/dev-projects/freelance", IdleS: 2})
	}
	e.insert(t, samples...)
	now := time.Unix(midnight+1800, 0)
	check := func(step string) {
		t.Helper()
		sq, _ := db.Open(e.dbPath)
		defer sq.Close()
		ivs, _ := db.ReadIntervals(sq, 0, 1<<62-1)
		want := []attribute.Interval{
			{From: midnight - 300, To: midnight, Project: "phd", RuleID: "3"},
			{From: midnight, To: ivs[len(ivs)-1].To, Project: "acme", Subproject: "globex", RuleID: "2"},
		}
		if !reflect.DeepEqual(ivs, want) {
			t.Fatalf("%s: intervals %+v, want %+v", step, ivs, want)
		}
	}
	if code, out, errs := e.exec(t, now, "run"); code != 0 {
		t.Fatalf("run: code=%d out=%q err=%q", code, out, errs)
	}
	check("run")
	if code, out, errs := e.exec(t, now, "recompute", "--since", "2026-09-15"); code != 0 {
		t.Fatalf("recompute: code=%d out=%q err=%q", code, out, errs)
	}
	check("recompute")
	if _, out, _ := e.exec(t, now, "explain", strconv.FormatInt(midnight+150, 10)); !strings.Contains(out, "matches  rule 2: project=acme subproject=globex cwd_prefix=/Users/me/dev-projects/freelance from=2026-09-16 until=2026-09-16") {
		t.Fatalf("explain inside the window: %q", out)
	}
	if _, out, _ := e.exec(t, now, "explain", strconv.FormatInt(midnight-150, 10)); !strings.Contains(out, "matches  rule 3: project=phd cwd_prefix=/Users/me/dev-projects/freelance") {
		t.Fatalf("explain before the window: %q", out)
	}
}
