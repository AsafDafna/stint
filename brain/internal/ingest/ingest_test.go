package ingest

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/rules"
)

const testTOML = `
[settings]
vault_reports_dir = "/tmp/x"
call_apps = ["us.zoom.xos"]
[projects.alpha]
[projects.beta]
[[rules]]
project = "alpha"
app = "A"
[[rules]]
project = "beta"
app = "B"
`

func setup(t *testing.T) (*sql.DB, *config.Config, *rules.Matcher) {
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
	return sq, cfg, rules.New(cfg)
}

func insert(t *testing.T, sq *sql.DB, samples ...attribute.Sample) {
	t.Helper()
	for _, s := range samples {
		if _, err := db.InsertSample(sq, s); err != nil {
			t.Fatal(err)
		}
	}
}

func work(ts int64, app string, idle int64) attribute.Sample {
	return attribute.Sample{TS: ts, Kind: "sample", App: app, IdleS: idle}
}

func all(t *testing.T, sq *sql.DB) []attribute.Interval {
	t.Helper()
	ivs, err := db.ReadIntervals(sq, 0, 1<<62-1)
	if err != nil {
		t.Fatal(err)
	}
	return ivs
}

// base: an early work stretch closed by an idle row (cold point at 420, last input 120),
// then alpha 10000-10300, idle at 10600 (last input 10300), beta 11000-11120 (gap),
// unmatched work at 11200 carried as beta.
func base() []attribute.Sample {
	return []attribute.Sample{
		work(0, "A", 0), work(60, "A", 0), work(420, "A", 300),
		work(10000, "A", 0), work(10060, "A", 0), work(10120, "A", 0), work(10180, "A", 0), work(10240, "A", 0),
		work(10600, "A", 300),
		work(11000, "B", 0), work(11060, "B", 0),
		work(11200, "", 0), work(11260, "", 0),
	}
}

var baseIntervals = []attribute.Interval{
	{From: 0, To: 120, Project: "alpha", RuleID: "0"},
	{From: 10000, To: 10300, Project: "alpha", RuleID: "0"},
	{From: 11000, To: 11180, Project: "beta", RuleID: "1"},
	{From: 11200, To: 11300, Project: "beta", RuleID: attribute.RuleCarryover},
}

func TestRunBuildsIntervalsAndWatermark(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, base()...)
	res, err := Run(sq, cfg, m, time.Unix(11300, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !res.FromStart || !res.FirstRun || res.FromTS != 0 || res.Rows != 13 {
		t.Errorf("first run should rebuild from the oldest sample, got %+v", res)
	}
	if got := all(t, sq); !reflect.DeepEqual(got, baseIntervals) {
		t.Fatalf("got %+v\nwant %+v", got, baseIntervals)
	}
	if v, _, _ := db.GetMeta(sq, MetaWatermark); v != "13" {
		t.Errorf("watermark = %q, want 13", v)
	}
}

func TestRunIsIdempotentAcrossRestarts(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, base()...)
	if _, err := Run(sq, cfg, m, time.Unix(11300, 0)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		res, err := Run(sq, cfg, m, time.Unix(11300, 0))
		if err != nil {
			t.Fatal(err)
		}
		// watermark ts 11260 - 15 min = 10360: the only cold point that early is the idle row at 420.
		if res.FromStart || res.FromTS != 420 {
			t.Fatalf("run %d should start mid-history at ts 420, got %+v", i+2, res)
		}
	}
	if got := all(t, sq); !reflect.DeepEqual(got, baseIntervals) {
		t.Fatalf("repeated runs changed intervals:\n%+v\nwant %+v", got, baseIntervals)
	}
	// A crash after the intervals commit, then a lost watermark: full rebuild, same result.
	if err := db.SetMeta(sq, MetaWatermark, "0"); err != nil {
		t.Fatal(err)
	}
	if res, err := Run(sq, cfg, m, time.Unix(11300, 0)); err != nil || !res.FromStart {
		t.Fatalf("lost watermark: %+v %v", res, err)
	}
	if got := all(t, sq); !reflect.DeepEqual(got, baseIntervals) {
		t.Fatalf("run after lost watermark changed intervals:\n%+v", got)
	}
}

func TestRunExtendsTailFromColdPoint(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, base()...)
	if _, err := Run(sq, cfg, m, time.Unix(11300, 0)); err != nil {
		t.Fatal(err)
	}
	// Later run: the last sample now has a successor and the provisional tail is re-derived.
	insert(t, sq, work(11320, "", 0), work(12500, "A", 0))
	res, err := Run(sq, cfg, m, time.Unix(12560, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.FromStart || res.FromTS != 420 || res.Rows != 13 {
		t.Errorf("rebuild should start at the cold point ts 420 and read 13 rows (ids 3-15), got %+v", res)
	}
	want := []attribute.Interval{
		{From: 0, To: 120, Project: "alpha", RuleID: "0"},
		{From: 10000, To: 10300, Project: "alpha", RuleID: "0"},
		{From: 11000, To: 11180, Project: "beta", RuleID: "1"},
		{From: 11200, To: 11440, Project: "beta", RuleID: attribute.RuleCarryover},
		{From: 12500, To: 12560, Project: "alpha", RuleID: "0"},
	}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestRunRebuildWindowPicksColdPoint15MinBack(t *testing.T) {
	sq, cfg, m := setup(t)
	// cold points at 600 (idle) and 1300 (sleep); watermark ts 2400 - 900 = 1500 -> the sleep row wins.
	insert(t, sq, work(0, "A", 0), work(600, "A", 300), work(1200, "B", 0), attribute.Sample{TS: 1300, Kind: "sleep"},
		attribute.Sample{TS: 2300, Kind: "wake"}, work(2400, "B", 0))
	if _, err := Run(sq, cfg, m, time.Unix(2400, 0)); err != nil {
		t.Fatal(err)
	}
	res, err := Run(sq, cfg, m, time.Unix(2460, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.FromStart || res.FromTS != 1300 || res.Rows != 3 {
		t.Fatalf("rebuild start = %+v, want the sleep row at 1300 with 3 rows", res)
	}
	want := []attribute.Interval{
		{From: 0, To: 120, Project: "alpha", RuleID: "0"},
		{From: 1200, To: 1300, Project: "beta", RuleID: "1"},
		{From: 2400, To: 2460, Project: "beta", RuleID: "1"},
	}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// TestRunRebuildWindowPicksLoginwindowColdPoint mirrors
// TestRunRebuildWindowPicksColdPoint15MinBack but with a loginwindow idle-0 heartbeat (a
// Power Nap dark-wake row) in place of the idle-threshold row: it must still be picked as
// a cold point, via app membership in attribute.NonWorkApps rather than idle_s.
func TestRunRebuildWindowPicksLoginwindowColdPoint(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, work(0, "A", 0), attribute.Sample{TS: 600, Kind: "heartbeat", App: "com.apple.loginwindow"}, work(1200, "B", 0), work(2400, "B", 0))
	if _, err := Run(sq, cfg, m, time.Unix(2400, 0)); err != nil {
		t.Fatal(err)
	}
	res, err := Run(sq, cfg, m, time.Unix(2460, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.FromStart || res.FromTS != 600 || res.Rows != 3 {
		t.Fatalf("rebuild start = %+v, want the loginwindow row at 600 with 3 rows", res)
	}
}

func TestRunWithoutColdPointStartsAtOldestSampleAndKeepsOlderIntervals(t *testing.T) {
	sq, cfg, m := setup(t)
	old := attribute.Interval{From: 100, To: 200, Project: "alpha", RuleID: "0"} // older than any retained sample
	tx, _ := sq.Begin()
	if err := db.InsertIntervals(tx, []attribute.Interval{old}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	insert(t, sq, work(100000, "B", 0), work(100060, "B", 0))
	now := time.Unix(100120, 0)
	res, err := Run(sq, cfg, m, now)
	if err != nil || !res.FromStart || res.FromTS != 100000 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := []attribute.Interval{old, {From: 100000, To: 100120, Project: "beta", RuleID: "1"}}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("after Run: %+v", got)
	}
	since := time.Date(2026, 9, 16, 0, 0, 0, 0, cfg.Location)
	insertDay := time.Unix(100000, 0).In(cfg.Location)
	since = time.Date(insertDay.Year(), insertDay.Month(), insertDay.Day(), 0, 0, 0, 0, cfg.Location)
	if res, err := Recompute(sq, cfg, m, since, now); err != nil || !res.FromStart {
		t.Fatalf("recompute: %+v %v", res, err)
	}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("after Recompute: %+v", got)
	}
	if _, err := Purge(sq, cfg, now.AddDate(0, 0, 91)); err != nil {
		t.Fatal(err)
	}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("after Purge: %+v", got)
	}
}

func TestNegativeDeltaClampsToZero(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, work(1000, "A", 0), work(900, "A", 0), work(960, "A", 0))
	if _, err := Run(sq, cfg, m, time.Unix(1100, 0)); err != nil {
		t.Fatal(err)
	}
	want := []attribute.Interval{{From: 900, To: 1080, Project: "alpha", RuleID: "0"}}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestRecomputeIsIdempotentAndHonorsHorizon(t *testing.T) {
	sq, cfg, m := setup(t)
	day := time.Date(2026, 9, 16, 10, 0, 0, 0, cfg.Location).Unix()
	insert(t, sq,
		work(day, "A", 0), work(day+60, "A", 0), work(day+400, "A", 300),
		work(day+1000, "B", 0), work(day+1060, "B", 0))
	now := time.Unix(day+1100, 0)
	if _, err := Run(sq, cfg, m, now); err != nil {
		t.Fatal(err)
	}
	before := all(t, sq)
	since := time.Date(2026, 9, 16, 0, 0, 0, 0, cfg.Location)
	for i := 0; i < 2; i++ {
		if _, err := Recompute(sq, cfg, m, since, now); err != nil {
			t.Fatal(err)
		}
		if got := all(t, sq); !reflect.DeepEqual(got, before) {
			t.Fatalf("recompute #%d changed intervals:\n%+v\nwant %+v", i+1, got, before)
		}
	}
	_, err := Recompute(sq, cfg, m, since.AddDate(0, 0, -1), now)
	if !errors.Is(err, db.ErrBeforeHorizon) {
		t.Fatalf("want ErrBeforeHorizon, got %v", err)
	}
}

func TestRecomputeAppliesNewRules(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, work(0, "A", 0), work(60, "C", 0), work(120, "C", 0))
	if _, err := Run(sq, cfg, m, time.Unix(180, 0)); err != nil {
		t.Fatal(err)
	}
	cfg2, err := config.Parse([]byte(testTOML+"\n[projects.gamma]\n[[rules]]\nproject = \"gamma\"\napp = \"C\"\n"), "/h")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Recompute(sq, cfg2, rules.New(cfg2), time.Unix(0, 0), time.Unix(180, 0)); err != nil {
		t.Fatal(err)
	}
	want := []attribute.Interval{
		{From: 0, To: 60, Project: "alpha", RuleID: "0"},
		{From: 60, To: 180, Project: "gamma", RuleID: "2"},
	}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// A --since day whose cold point lies after the watermark (a future-date typo, or a brain
// stopped for a day), or a recompute before any run, must still rebuild the rows no run has
// ingested yet.
func TestRecomputeSinceAfterWatermarkStartsNoLaterThanRun(t *testing.T) {
	later := []attribute.Sample{work(12000, "B", 0), work(12060, "B", 0), work(12500, "B", 300), work(13000, "A", 0), work(13060, "A", 0)}
	now := time.Unix(13120, 0)
	fresh, cfg, m := setup(t)
	insert(t, fresh, append(base(), later...)...)
	if _, err := Run(fresh, cfg, m, now); err != nil {
		t.Fatal(err)
	}
	want := all(t, fresh)
	for _, ranBefore := range []bool{true, false} {
		sq, _, _ := setup(t)
		insert(t, sq, base()...)
		if ranBefore {
			if _, err := Run(sq, cfg, m, time.Unix(11300, 0)); err != nil {
				t.Fatal(err)
			}
		}
		insert(t, sq, later...)
		if _, err := Recompute(sq, cfg, m, time.Date(2100, 1, 1, 0, 0, 0, 0, cfg.Location), now); err != nil {
			t.Fatal(err)
		}
		if got := all(t, sq); !reflect.DeepEqual(got, want) {
			t.Fatalf("ran before: %v\ngot %+v\nwant %+v (a full run)", ranBefore, got, want)
		}
	}
}

func TestRebuildChunksAtColdPoints(t *testing.T) {
	sq, cfg, m := setup(t)
	// Two work stretches split by many idle heartbeats (> chunkMinRows rows) and a sleep,
	// so the rebuild runs as several chunks; results must equal a single pass.
	var samples []attribute.Sample
	ts := int64(0)
	samples = append(samples, work(ts, "A", 0))
	for i := 0; i < chunkMinRows+10; i++ {
		ts += 60
		samples = append(samples, work(ts, "A", 300+int64(i)*60))
	}
	ts += 60
	samples = append(samples, work(ts, "B", 0), work(ts+60, "B", 0), attribute.Sample{TS: ts + 90, Kind: "sleep"})
	for i := 0; i < chunkMinRows+10; i++ {
		ts += 60
		samples = append(samples, work(ts+100, "B", 300+int64(i)*60))
	}
	ts += 200
	samples = append(samples, work(ts, "A", 0), work(ts+60, "A", 0))
	insert(t, sq, samples...)
	res, err := Run(sq, cfg, m, time.Unix(ts+120, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != len(samples) { // the two interior chunk boundaries are read twice but counted once
		t.Errorf("rows read = %d, want %d", res.Rows, len(samples))
	}
	single := attribute.Attribute(samples, Params(cfg, m, time.Unix(ts+120, 0)))
	if got := all(t, sq); !reflect.DeepEqual(got, single) {
		t.Fatalf("chunked rebuild differs from single pass:\n%+v\nwant %+v", got, single)
	}
	if v, _, _ := db.GetMeta(sq, MetaWatermark); v != fmt.Sprint(len(samples)) {
		t.Errorf("watermark after a multi-chunk rebuild = %q, want %d", v, len(samples))
	}
}

// TestRebuildChunksAtSleepBound: a work stretch with no cold row inside, then a sleep row
// exactly chunkMinRows ids after the start, so the sleep row is the only chunk boundary.
func TestRebuildChunksAtSleepBound(t *testing.T) {
	sq, cfg, m := setup(t)
	var samples []attribute.Sample
	ts := int64(0)
	for i := 0; i < chunkMinRows; i++ {
		samples = append(samples, work(ts, "A", 0))
		ts += 60
	}
	samples = append(samples, attribute.Sample{TS: ts, Kind: "sleep"}) // id chunkMinRows+1
	ts += 3600
	samples = append(samples, attribute.Sample{TS: ts, Kind: "wake"}, work(ts+60, "B", 0), work(ts+120, "B", 0))
	insert(t, sq, samples...)
	now := time.Unix(ts+180, 0)
	res, err := Run(sq, cfg, m, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != len(samples) {
		t.Errorf("rows read = %d, want %d", res.Rows, len(samples))
	}
	single := attribute.Attribute(samples, Params(cfg, m, now))
	if got := all(t, sq); !reflect.DeepEqual(got, single) {
		t.Fatalf("chunked rebuild differs from single pass:\n%+v\nwant %+v", got, single)
	}
	if v, _, _ := db.GetMeta(sq, MetaWatermark); v != fmt.Sprint(len(samples)) {
		t.Errorf("watermark = %q, want %d", v, len(samples))
	}
}

func TestRunOnEmptyDBDoesNothing(t *testing.T) {
	sq, cfg, m := setup(t)
	res, err := Run(sq, cfg, m, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Result{FromStart: true, FirstRun: true}); res != want {
		t.Errorf("got %+v, want %+v", res, want)
	}
	if _, ok, _ := db.GetMeta(sq, MetaWatermark); ok {
		t.Error("watermark written with no samples")
	}
	if got := all(t, sq); len(got) != 0 {
		t.Errorf("intervals from no samples: %+v", got)
	}
}

// TestRunAfterPurgedWatermarkRowRebuildsFromOldestSample: the brain did not run for longer
// than raw_retention_days, so every sample up to and including the watermark row is gone.
// The next run starts at the oldest retained sample, keeps the intervals that end before
// it, and moves the watermark on.
func TestRunAfterPurgedWatermarkRowRebuildsFromOldestSample(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, base()...)
	if _, err := Run(sq, cfg, m, time.Unix(11300, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.Exec(`DELETE FROM samples`); err != nil {
		t.Fatal(err)
	}
	insert(t, sq, work(500000, "B", 0), work(500060, "B", 0))
	res, err := Run(sq, cfg, m, time.Unix(500120, 0))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Result{FromID: 1, FromTS: 500000, FromStart: true, Rows: 2}); res != want {
		t.Errorf("got %+v, want %+v", res, want)
	}
	want := append(append([]attribute.Interval{}, baseIntervals...), attribute.Interval{From: 500000, To: 500120, Project: "beta", RuleID: "1"})
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// samples.id has no AUTOINCREMENT: once the table is empty SQLite hands out ids from 1
	// again (ids 1 and 2 here), so the stale watermark 13 exceeds every retained id.
	if v, _, _ := db.GetMeta(sq, MetaWatermark); v != "2" {
		t.Errorf("watermark = %q, want 2", v)
	}
}

// TestRunRebuildSkipsInCallColdRows: an idle row during a call is not a cold point (the
// idle rule is suspended for the call), so the rebuild must start at the earlier
// out-of-call idle row even though the in-call row is the latest candidate. This is why
// coldPointBefore filters in Go and carries no LIMIT.
func TestRunRebuildSkipsInCallColdRows(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, work(0, "A", 0), work(600, "A", 300), work(1200, "B", 0),
		attribute.Sample{TS: 1300, Kind: "sample", App: "B", IdleS: 300, CallApp: "us.zoom.xos"},
		work(2400, "B", 0))
	if _, err := Run(sq, cfg, m, time.Unix(2400, 0)); err != nil {
		t.Fatal(err)
	}
	res, err := Run(sq, cfg, m, time.Unix(2460, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.FromStart || res.FromTS != 600 || res.Rows != 4 {
		t.Fatalf("rebuild start = %+v, want the out-of-call idle row at 600 with 4 rows", res)
	}
}

func TestRunRejectsCorruptWatermark(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, base()...)
	for _, v := range []string{"12abc", "abc", ""} {
		if err := db.SetMeta(sq, MetaWatermark, v); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(sq, cfg, m, time.Unix(11300, 0)); err == nil {
			t.Errorf("watermark %q: want an error, got nil", v)
		}
	}
}

func TestRecomputeOnEmptyDBReturnsErrEmpty(t *testing.T) {
	sq, cfg, m := setup(t)
	_, err := Recompute(sq, cfg, m, time.Unix(0, 0), time.Unix(100, 0))
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("want ErrEmpty, got %v", err)
	}
	if errors.Is(err, db.ErrBeforeHorizon) {
		t.Fatal("ErrEmpty must not be ErrBeforeHorizon: cmd/stint clears the recompute marker on that one")
	}
}

func TestPurge(t *testing.T) {
	sq, cfg, _ := setup(t)
	now := time.Unix(100*86400, 0)
	insert(t, sq, work(now.Unix()-91*86400, "A", 0), work(now.Unix()-89*86400, "A", 0))
	n, err := Purge(sq, cfg, now)
	if err != nil || n != 1 {
		t.Fatalf("purged %d err=%v", n, err)
	}
	if ts, _, _ := db.OldestSampleTS(sq); ts != now.Unix()-89*86400 {
		t.Fatalf("wrong row purged, oldest = %d", ts)
	}
}

// TestRunAfterBackwardsClockStepDoesNotDuplicate: the only cold row (id2, ts 5060) is
// above the watermark ts minus 15 min (1060-900), so the second run takes the
// oldest-sample fallback: start = id1 (ts 5000), while id3/id4 (ts 1000/1060) sit far
// below that ts. The delete floor must reach back to the earliest ts among id >= start.id,
// not just start.ts, or the re-derived [1000,1120) interval is never deleted before being
// re-inserted. Result.FromTS reports that floor. (The cold-point path with a lowered floor
// is TestRunColdPointPathLowersFloorAfterBackwardsClockStep.)
func TestRunAfterBackwardsClockStepDoesNotDuplicate(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq,
		work(5000, "A", 0),
		work(5060, "A", 300),
		work(1000, "A", 0),
		work(1060, "A", 0),
	)
	now := time.Unix(1120, 0)
	if _, err := Run(sq, cfg, m, now); err != nil {
		t.Fatal(err)
	}
	want := []attribute.Interval{{From: 1000, To: 1120, Project: "alpha", RuleID: "0"}}
	if got := all(t, sq); !reflect.DeepEqual(got, want) {
		t.Fatalf("after run 1: got %+v\nwant %+v", got, want)
	}
	for i := 0; i < 2; i++ {
		res, err := Run(sq, cfg, m, now)
		if err != nil {
			t.Fatal(err)
		}
		if !res.FromStart || res.FromTS != 1000 {
			t.Errorf("run %d: want the oldest-sample fallback with the floor lowered to 1000, got %+v", i+2, res)
		}
		if got := all(t, sq); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d duplicated intervals: got %+v\nwant %+v", i+2, got, want)
		}
	}
}

// TestRunColdPointPathLowersFloorAfterBackwardsClockStep: the clock ran ahead, an idle row
// (id3, ts 5400) closed the pre-step interval at 5100, then the clock was corrected back to
// 5050 and work continued uninterrupted past 5400+15 min. The second run picks id3 as the
// cold point (no post-step cold row exists) and must lower the floor to the earliest
// post-step ts (5050): the post-step interval is deleted and re-derived (no duplicate),
// and the pre-step interval [5000,5100) that overlaps the ambiguous window is dropped once
// (the ruled trade-off: a tracker never over-credits). Runs 2 and 3 agree.
func TestRunColdPointPathLowersFloorAfterBackwardsClockStep(t *testing.T) {
	sq, cfg, m := setup(t)
	samples := []attribute.Sample{work(5000, "A", 0), work(5060, "A", 0), work(5400, "A", 300)}
	for ts := int64(5050); ts <= 6370; ts += 60 {
		samples = append(samples, work(ts, "A", 0))
	}
	insert(t, sq, samples...)
	now := time.Unix(6400, 0)
	res, err := Run(sq, cfg, m, now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FromStart || res.FromTS != 5000 {
		t.Fatalf("run 1: %+v", res)
	}
	pre := attribute.Interval{From: 5000, To: 5100, Project: "alpha", RuleID: "0"}
	post := attribute.Interval{From: 5050, To: 6400, Project: "alpha", RuleID: "0"}
	if got := all(t, sq); !reflect.DeepEqual(got, []attribute.Interval{pre, post}) {
		t.Fatalf("after run 1: got %+v", got)
	}
	for i := 2; i <= 3; i++ {
		res, err := Run(sq, cfg, m, now)
		if err != nil {
			t.Fatal(err)
		}
		if res.FromStart || res.FromTS != 5050 || res.Rows != len(samples)-2 {
			t.Errorf("run %d: want the cold point id3 with the floor lowered to 5050, got %+v", i, res)
		}
		if got := all(t, sq); !reflect.DeepEqual(got, []attribute.Interval{post}) {
			t.Fatalf("run %d: got %+v\nwant only %+v", i, got, post)
		}
	}
}

// BenchmarkAttribute100k is the cost of one uninterrupted pass over ~7 days of 60 s
// heartbeats (the worst case for a single chunk). Expected well under 2 s per op.
func BenchmarkAttribute100k(b *testing.B) {
	cfg, err := config.Parse([]byte(testTOML), "/h")
	if err != nil {
		b.Fatal(err)
	}
	p := Params(cfg, rules.New(cfg), time.Unix(100000*60, 0))
	samples := make([]attribute.Sample, 100000)
	apps := []string{"A", "B", "", "A"}
	for i := range samples {
		samples[i] = attribute.Sample{ID: int64(i + 1), TS: int64(i) * 60, Kind: "heartbeat", App: apps[i%4], Title: "t", CWD: "/x"}
		if i%700 > 690 {
			samples[i].IdleS = int64(i%700-690) * 60
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ivs := attribute.Attribute(samples, p); len(ivs) == 0 {
			b.Fatal("no intervals")
		}
	}
}

// TestRunClockStepNeverDeletesIntervalsOlderThanRetention: a sample stamped far in the
// past (dead PRAM battery, a boot before NTP sync, a manual clock change) drags the
// delete floor down with it. Intervals are kept forever while samples are purged after
// raw_retention_days, so the floor must stop at the retention cutoff: below it nothing
// can be re-derived, and run would otherwise wipe every interval ever written.
func TestRunClockStepNeverDeletesIntervalsOlderThanRetention(t *testing.T) {
	sq, cfg, m := setup(t)
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, cfg.Location)
	cutoff := now.Unix() - int64(cfg.Settings.RawRetentionDays)*86400
	old := attribute.Interval{From: cutoff - 2*86400, To: cutoff - 86400, Project: "alpha", RuleID: "0"}
	tx, err := sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertIntervals(tx, []attribute.Interval{old}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// A normal stretch closed by a sleep row, then two rows the clock stamped in 1970.
	n := now.Unix() - 600
	insert(t, sq, work(n, "A", 0), work(n+60, "A", 0), attribute.Sample{TS: n + 120, Kind: "sleep"},
		work(1000, "A", 300), work(1060, "A", 300))

	want := []attribute.Interval{old, {From: n, To: n + 120, Project: "alpha", RuleID: "0"}}
	for i := 1; i <= 3; i++ { // idempotent: runs 2 and 3 must leave the same set
		if _, err := Run(sq, cfg, m, now); err != nil {
			t.Fatal(err)
		}
		if got := all(t, sq); !reflect.DeepEqual(got, want) {
			t.Fatalf("after run %d: got %+v\nwant %+v", i, got, want)
		}
	}
}

func TestRebuildHonoursCallDecision(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq,
		attribute.Sample{TS: 0, Kind: "sample", App: "A", CallApp: "us.zoom.xos"},
		attribute.Sample{TS: 60, Kind: "sample", App: "A", CallApp: "us.zoom.xos"},
		work(120, "A", 0), work(180, "A", 0), work(600, "A", 0),
	)
	if _, err := db.InsertCall(sq, db.Call{FromTS: 0, ToTS: 120, CallApp: "us.zoom.xos", GuessProject: "alpha", Breakdown: "[]"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetDecision(sq, 0, attribute.DecisionProject, "beta", "", 1); err != nil {
		t.Fatal(err)
	}
	res, err := Run(sq, cfg, m, time.Unix(700, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.FromID != 1 {
		t.Fatalf("FromID = %d, want 1 (oldest sample)", res.FromID)
	}
	ivs := all(t, sq)
	if len(ivs) < 2 || ivs[0] != (attribute.Interval{From: 0, To: 120, Project: "beta", RuleID: attribute.RuleCallProject}) || ivs[1].Project != "alpha" {
		t.Fatalf("intervals: %+v", ivs)
	}
}

func TestPurgeRemovesOldCalls(t *testing.T) {
	sq, cfg, _ := setup(t)
	now := time.Unix(100*86400, 0)
	old := now.Unix() - int64(cfg.Settings.RawRetentionDays)*86400 - 10
	db.InsertCall(sq, db.Call{FromTS: old - 600, ToTS: old, CallApp: "x", GuessProject: "alpha", Breakdown: "[]"})
	db.InsertCall(sq, db.Call{FromTS: now.Unix() - 600, ToTS: now.Unix(), CallApp: "x", GuessProject: "alpha", Breakdown: "[]"})
	if _, err := Purge(sq, cfg, now); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.ReadCalls(sq, 0)
	if len(rows) != 1 || rows[0].ToTS != now.Unix() {
		t.Fatalf("calls after purge: %+v", rows)
	}
}

func TestWatermarkTS(t *testing.T) {
	sq, cfg, m := setup(t)
	if _, ok, err := WatermarkTS(sq); err != nil || ok {
		t.Fatalf("no watermark yet: ok=%v err=%v", ok, err)
	}
	insert(t, sq, work(0, "A", 0), work(60, "A", 0))
	if _, err := Run(sq, cfg, m, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	ts, ok, err := WatermarkTS(sq)
	if err != nil || !ok || ts != 60 {
		t.Fatalf("watermark ts: %d ok=%v err=%v", ts, ok, err)
	}
}

// TestRunStartMatchesRun: RunStart reports the id a Run would rebuild from, without writing.
func TestRunStartMatchesRun(t *testing.T) {
	sq, cfg, m := setup(t)
	now := time.Unix(11300, 0)
	p := Params(cfg, m, now)
	if _, ok, err := RunStart(sq, p); err != nil || ok {
		t.Fatalf("empty db: ok=%v err=%v", ok, err)
	}
	insert(t, sq, base()...)
	if id, ok, err := RunStart(sq, p); err != nil || !ok || id != 1 {
		t.Fatalf("no watermark: id=%d ok=%v err=%v, want the oldest sample (1)", id, ok, err)
	}
	if _, err := Run(sq, cfg, m, now); err != nil {
		t.Fatal(err)
	}
	id, ok, err := RunStart(sq, p)
	if err != nil || !ok {
		t.Fatalf("RunStart: ok=%v err=%v", ok, err)
	}
	res, err := Run(sq, cfg, m, now)
	if err != nil {
		t.Fatal(err)
	}
	if id != res.FromID || id != 3 {
		t.Fatalf("RunStart = %d, second Run FromID = %d; want both 3 (the idle row at 420)", id, res.FromID)
	}
}

// TestRunSkipsColdRowInsideCallGapWindow: a one-tick mic drop while listening hands-off
// writes a non-call idle row between two in-call rows, inside the stretch's gap window
// (attribute.Stretches keeps rows 0-4 one stretch). It must be neither a rebuild start nor
// a chunk bound: a rebuild starting there hands call detection only the stretch's second
// half.
func TestRunSkipsColdRowInsideCallGapWindow(t *testing.T) {
	sq, cfg, m := setup(t)
	zoom := func(ts int64) attribute.Sample {
		return attribute.Sample{TS: ts, Kind: "sample", App: "us.zoom.xos", CallApp: "us.zoom.xos"}
	}
	insert(t, sq, zoom(0), zoom(60), work(120, "com.apple.Music", 600), zoom(180), zoom(240),
		work(600, "com.apple.Music", 0), work(1200, "com.apple.Music", 0))
	now := time.Unix(1260, 0)
	if _, err := Run(sq, cfg, m, now); err != nil { // watermark: id 7, ts 1200 (window floor 300)
		t.Fatal(err)
	}
	if colds, err := coldPointsAfter(sq, 0, Params(cfg, m, now)); err != nil || len(colds) != 0 {
		t.Fatalf("cold points = %+v err=%v; want none (the idle row at 120 is inside the call's gap window)", colds, err)
	}
	res, err := Run(sq, cfg, m, now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FromStart || res.FromID != 1 {
		t.Fatalf("second run = %+v; want the oldest sample (id 1), not the idle row at 120", res)
	}
}

func TestPurgeRemovesOldStretches(t *testing.T) {
	sq, cfg, _ := setup(t)
	now := time.Unix(100*86400, 0)
	old := now.Unix() - int64(cfg.Settings.RawRetentionDays)*86400 - 10
	db.InsertStretch(sq, db.Stretch{FromTS: old - 900, ToTS: old, UnattributedS: 900, Breakdown: "[]"})
	db.InsertStretch(sq, db.Stretch{FromTS: now.Unix() - 900, ToTS: now.Unix(), UnattributedS: 900, Breakdown: "[]"})
	if _, err := Purge(sq, cfg, now); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.ReadStretches(sq, 0)
	if len(rows) != 1 || rows[0].ToTS != now.Unix() {
		t.Fatalf("stretches after purge: %+v", rows)
	}
}

// An answer reaches intervals only through the apply step: unapplied it is in no rebuild
// (routine run or manual recompute); while being applied, or once applied, it is.
func TestRebuildHonoursOnlyAppliedOrApplyingAnswers(t *testing.T) {
	sq, cfg, m := setup(t)
	insert(t, sq, work(0, "W", 0), work(60, "W", 0), work(120, "W", 0), work(600, "A", 300))
	db.InsertStretch(sq, db.Stretch{FromTS: 0, ToTS: 180, UnattributedS: 180, Breakdown: "[]"})
	db.SetStretchAnswer(sq, 0, `[{"key":"app:W","project":"beta"}]`, 1)
	now := time.Unix(700, 0)
	at := func(when string) string {
		t.Helper()
		iv, ok, err := db.IntervalAt(sq, 60)
		if err != nil || !ok {
			t.Fatalf("%s: no interval at 60 (err %v)", when, err)
		}
		return iv.Project + "/" + iv.RuleID
	}
	if _, err := Run(sq, cfg, m, now); err != nil {
		t.Fatal(err)
	}
	if got := at("routine run, unapplied"); got != "unattributed/unattributed" {
		t.Fatalf("routine run, unapplied: %s", got)
	}
	if _, err := Recompute(sq, cfg, m, time.Unix(0, 0), now); err != nil {
		t.Fatal(err)
	}
	if got := at("manual recompute, unapplied"); got != "unattributed/unattributed" {
		t.Fatalf("manual recompute, unapplied: %s", got)
	}
	if _, err := RecomputeApplying(sq, cfg, m, time.Unix(0, 0), now, map[int64]bool{0: true}); err != nil {
		t.Fatal(err)
	}
	if got := at("being applied"); got != "beta/stretch" {
		t.Fatalf("being applied: %s", got)
	}
	db.MarkStretchApplied(sq, 0, 1, 2)
	if _, err := Recompute(sq, cfg, m, time.Unix(0, 0), now); err != nil {
		t.Fatal(err)
	}
	if got := at("applied"); got != "beta/stretch" {
		t.Fatalf("applied: %s", got)
	}
}
