package stretches

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/ingest"
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
`

func setup(t *testing.T) (*sql.DB, attribute.Params) {
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
	return sq, ingest.Params(cfg, rules.New(cfg), time.Unix(100_000, 0))
}

// hb: one heartbeat a minute in [from, to).
func hb(from, to int64, app string) []attribute.Sample {
	var out []attribute.Sample
	for ts := from; ts < to; ts += 60 {
		out = append(out, attribute.Sample{TS: ts, Kind: "heartbeat", App: app})
	}
	return out
}

func row(ts int64, kind, app string, idle int64, call string) attribute.Sample {
	return attribute.Sample{TS: ts, Kind: kind, App: app, IdleS: idle, CallApp: call}
}

func cat(parts ...[]attribute.Sample) []attribute.Sample {
	var out []attribute.Sample
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// withRule adds a rule app -> project in front of p's matcher (a rule written later).
func withRule(p attribute.Params, app, project string) attribute.Params {
	prev := p.Match
	p.Match = func(f rules.Fields) (rules.Match, bool) {
		if f.App == app {
			return rules.Match{Project: project, RuleID: 9}, true
		}
		return prev(f)
	}
	return p
}

// load inserts samples and rewrites every interval from them, as ingest would leave the
// database, and returns the stored samples.
func load(t *testing.T, sq *sql.DB, p attribute.Params, samples ...attribute.Sample) []attribute.Sample {
	t.Helper()
	for _, s := range samples {
		if _, err := db.InsertSample(sq, s); err != nil {
			t.Fatal(err)
		}
	}
	return reattribute(t, sq, p)
}

func reattribute(t *testing.T, sq *sql.DB, p attribute.Params) []attribute.Sample {
	t.Helper()
	got, err := db.ReadSamples(sq, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := db.DeleteIntervals(tx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertIntervals(tx, attribute.Attribute(got, p)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFind(t *testing.T) {
	_, p := setup(t)
	w := func(from, to, un int64, keys map[string]int64, closedTS int64) Window {
		return Window{FromTS: from, ToTS: to, Unattributed: un, Keys: keys, Closed: closedTS > 0, ClosedTS: closedTS}
	}
	answered := p
	answered.Answers = []attribute.Answer{{FromTS: 0, ToTS: 660, Entries: map[string]attribute.AnswerItem{"app:W": {Key: "app:W", Project: "beta"}}}}
	left := p
	left.Answers = []attribute.Answer{{FromTS: 0, ToTS: 660, Entries: map[string]attribute.AnswerItem{"app:W": {Key: "app:W", Project: attribute.Unattributed}}}}
	cases := []struct {
		name string
		in   []attribute.Sample
		p    attribute.Params
		want []Window
	}{
		{"carry-over head included; closed by a sleep row",
			cat(hb(0, 60, "A"), hb(60, 1260, "W"), []attribute.Sample{row(1260, "sleep", "", 0, "")}), p,
			[]Window{w(60, 1260, 600, map[string]int64{"app:W": 1200}, 1260)}},
		{"open while nothing follows",
			hb(0, 600, "W"), p,
			[]Window{w(0, 660, 660, map[string]int64{"app:W": 660}, 0)}},
		{"a rule-matched blip under a gap is bridged",
			cat(hb(0, 600, "W"), hb(600, 660, "A"), hb(660, 1260, "W"), []attribute.Sample{row(1260, "sleep", "", 0, "")}), p,
			[]Window{w(0, 1260, 600, map[string]int64{"app:W": 1200}, 1260)}},
		{"a blip of a gap or more splits",
			cat(hb(0, 600, "W"), hb(600, 720, "A"), hb(720, 1320, "W"), []attribute.Sample{row(1320, "sleep", "", 0, "")}), p,
			[]Window{w(0, 600, 600, map[string]int64{"app:W": 600}, 720), w(720, 1320, 0, map[string]int64{"app:W": 600}, 1320)}},
		{"a coverage hole longer than the gap ends a stretch",
			cat(hb(0, 600, "W"), hb(900, 1500, "W"), []attribute.Sample{row(1500, "sleep", "", 0, "")}), p,
			[]Window{w(0, 660, 660, map[string]int64{"app:W": 660}, 900), w(900, 1500, 600, map[string]int64{"app:W": 600}, 1500)}},
		{"an attributed row a gap or more later closes it",
			cat(hb(0, 600, "W"), hb(900, 960, "A")), p,
			[]Window{w(0, 660, 660, map[string]int64{"app:W": 660}, 900)}},
		{"an idle row closes it; the window ends at the last input",
			cat(hb(0, 720, "W"), []attribute.Sample{row(900, "heartbeat", "W", 300, "")}), p,
			[]Window{w(0, 600, 600, map[string]int64{"app:W": 600}, 900)}},
		{"a call row closes it and is not part of it",
			cat(hb(0, 600, "W"), []attribute.Sample{row(600, "heartbeat", "W", 0, "us.zoom.xos"), row(660, "heartbeat", "W", 0, "us.zoom.xos")}), p,
			[]Window{w(0, 600, 600, map[string]int64{"app:W": 600}, 600)}},
		{"a lock-screen row closes it",
			cat(hb(0, 600, "W"), []attribute.Sample{row(600, "heartbeat", "com.apple.loginwindow", 0, "")}), p,
			[]Window{w(0, 600, 600, map[string]int64{"app:W": 600}, 600)}},
		{"an answered window is not unmatched", hb(0, 600, "W"), answered, nil},
		{"a left window is not unmatched", hb(0, 600, "W"), left, nil},
	}
	for _, c := range cases {
		spans := attribute.Coverage(c.in, c.p)
		if got := Find(c.in, spans, attribute.Attribute(c.in, c.p), c.p); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// carryHead: alpha, then 20 min of W (10 carried over, 10 unattributed), closed by a sleep.
func carryHead() []attribute.Sample {
	return cat(hb(0, 60, "A"), hb(60, 1260, "W"), []attribute.Sample{row(1260, "sleep", "", 0, "")})
}

func TestDetectInsertsOnceAndHonoursThresholdAndWatermark(t *testing.T) {
	sq, p := setup(t)
	samples := load(t, sq, p, carryHead()...)
	if got, err := Detect(sq, samples, p, 1260); err != nil || len(got) != 0 {
		t.Fatalf("closed at the watermark: %+v err=%v", got, err)
	}
	got, err := Detect(sq, samples, p, 0)
	want := []db.Stretch{{FromTS: 60, ToTS: 1260, UnattributedS: 600, Breakdown: `[{"key":"app:W","seconds":1200}]`}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if again, err := Detect(sq, samples, p, 0); err != nil || len(again) != 0 {
		t.Fatalf("rerun inserted %+v err=%v", again, err)
	}
	sq2, _ := setup(t)
	short := load(t, sq2, p, cat(hb(0, 60, "A"), hb(60, 1200, "W"), []attribute.Sample{row(1200, "sleep", "", 0, "")})...)
	if got, err := Detect(sq2, short, p, 0); err != nil || len(got) != 0 {
		t.Fatalf("540 s unattributed must not ask: %+v err=%v", got, err)
	}
}

func TestDetectOpenThenClosed(t *testing.T) {
	sq, p := setup(t)
	samples := load(t, sq, p, hb(0, 720, "W")...)
	if got, _ := Detect(sq, samples, p, 0); len(got) != 0 {
		t.Fatalf("open stretch inserted: %+v", got)
	}
	samples = load(t, sq, p, row(1000, "heartbeat", "X", 0, ""))
	got, err := Detect(sq, samples, p, 0)
	if err != nil || len(got) != 1 || got[0].FromTS != 0 || got[0].ToTS != 780 || got[0].UnattributedS != 780 {
		t.Fatalf("after the closing row: %+v err=%v", got, err)
	}
}

// A walk-away: the last input at 1200, heartbeats with a growing idle_s, the idle row at 1500.
// The previous run ingested up to 1320 and saw the stretch open; this run's idle row closes it
// and back-dates its tail. The close must count as this run's, not as the emptied row's at 1200,
// which lies before the watermark.
func TestDetectWalkAwayClosedByIdleAfterWatermark(t *testing.T) {
	sq, p := setup(t)
	in := hb(0, 1260, "W")
	for _, ts := range []int64{1260, 1320, 1380, 1440} {
		in = append(in, row(ts, "heartbeat", "W", ts-1200, ""))
	}
	in = append(in, row(1500, "heartbeat", "W", 300, ""))
	prev := load(t, sq, p, in[:23]...)
	if got, _ := Detect(sq, prev, p, 0); len(got) != 0 {
		t.Fatalf("still open at the previous run, yet inserted: %+v", got)
	}
	samples := load(t, sq, p, in[23:]...)
	got, err := Detect(sq, samples, p, 1320)
	if err != nil || len(got) != 1 || got[0].FromTS != 0 || got[0].ToTS != 1200 || got[0].UnattributedS != 1200 {
		t.Fatalf("walk-away stretch: %+v err=%v", got, err)
	}
}

// After a recompute the same stretch can start elsewhere; the overlap check keeps it single.
func TestDetectSkipsOverlapAfterShiftedRecompute(t *testing.T) {
	sq, p := setup(t)
	db.InsertStretch(sq, db.Stretch{FromTS: 0, ToTS: 1200, UnattributedS: 1200, Breakdown: "[]"})
	samples := load(t, sq, p, carryHead()...)
	if got, err := Detect(sq, samples, p, 0); err != nil || len(got) != 0 {
		t.Fatalf("shifted stretch inserted again: %+v err=%v", got, err)
	}
}

func TestDetectBreakdownNoAlways(t *testing.T) {
	sq, p := setup(t)
	chrome := func(from, to int64, url string) []attribute.Sample {
		out := hb(from, to, "com.google.Chrome")
		for i := range out {
			out[i].URL = url
		}
		return out
	}
	samples := load(t, sq, p, cat(
		[]attribute.Sample{{TS: 0, Kind: "heartbeat", App: "com.mitchellh.ghostty", CWD: "/x"}, row(300, "heartbeat", "X", 300, "")},
		chrome(1000, 1300, ""), chrome(1300, 1600, "http://localhost:3000/"), hb(1600, 1900, "com.mitchellh.ghostty"), hb(1900, 2200, "W"),
		[]attribute.Sample{row(2200, "sleep", "", 0, "")},
	)...)
	got, err := Detect(sq, samples, p, 0)
	want := `[{"key":"app:W","seconds":300},{"key":"app:com.google.Chrome","seconds":300,"no_always":true},{"key":"app:com.mitchellh.ghostty","seconds":300,"no_always":true},{"key":"host:localhost","seconds":300,"no_always":true}]`
	if err != nil || len(got) != 1 || got[0].Breakdown != want {
		t.Fatalf("got %+v err=%v", got, err)
	}
}

// twoKeys: 12 min of W then 12 min of X, nothing attributed before, closed by a sleep.
func twoKeys(w, x int64) []attribute.Sample {
	return cat(hb(0, w, "W"), hb(w, w+x, "X"), []attribute.Sample{row(w+x, "sleep", "", 0, "")})
}

func TestRefreshFollowsRulesAndDeletesOnlyUndecidedRows(t *testing.T) {
	sq, p := setup(t)
	samples := load(t, sq, p, twoKeys(720, 720)...)
	if got, _ := Detect(sq, samples, p, 0); len(got) != 1 || got[0].UnattributedS != 1440 {
		t.Fatalf("detect: %+v", got)
	}
	px := withRule(p, "X", "beta")
	reattribute(t, sq, px)
	if n, d, err := Refresh(sq, samples, px); err != nil || n != 1 || d != 0 {
		t.Fatalf("refresh: n=%d d=%d err=%v", n, d, err)
	}
	s, _, _ := db.ReadStretch(sq, 0)
	if s.ToTS != 1440 || s.UnattributedS != 720 || s.Breakdown != `[{"key":"app:W","seconds":720}]` {
		t.Fatalf("refreshed row: %+v", s)
	}
	if n, d, _ := Refresh(sq, samples, px); n != 0 || d != 0 {
		t.Fatalf("an unchanged row is not rewritten: n=%d d=%d", n, d)
	}
	pwx := withRule(px, "W", "beta")
	reattribute(t, sq, pwx)
	if _, d, err := Refresh(sq, samples, pwx); err != nil || d != 1 {
		t.Fatalf("never-decided row under 600 s must go: d=%d err=%v", d, err)
	}
	if rows, _ := db.ReadStretches(sq, 0); len(rows) != 0 {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestRefreshKeepsReopenedAndSkipsAnsweredRows(t *testing.T) {
	sq, p := setup(t)
	samples := load(t, sq, p, twoKeys(300, 720)...)
	Detect(sq, samples, p, 0)
	db.SetStretchAnswer(sq, 0, `[{"key":"app:W","project":"beta"}]`, 10)
	before, _, _ := db.ReadStretch(sq, 0)
	px := withRule(p, "X", "beta")
	reattribute(t, sq, px)
	if n, d, _ := Refresh(sq, samples, px); n != 0 || d != 0 {
		t.Fatalf("answered row touched: n=%d d=%d", n, d)
	}
	if after, _, _ := db.ReadStretch(sq, 0); after != before {
		t.Fatalf("answered row changed: %+v", after)
	}
	db.ReopenStretch(sq, 0, 20)
	if _, d, err := Refresh(sq, samples, px); err != nil || d != 0 {
		t.Fatalf("re-opened row deleted: d=%d err=%v", d, err)
	}
	s, _, _ := db.ReadStretch(sq, 0)
	if s.UnattributedS != 300 || s.Breakdown != `[{"key":"app:W","seconds":300}]` {
		t.Fatalf("re-opened row: %+v", s)
	}
	if n, _, _ := Refresh(sq, samples[1:], withRule(px, "W", "beta")); n != 0 {
		t.Fatal("a window starting before the rebuild's first sample must be left alone")
	}
}
