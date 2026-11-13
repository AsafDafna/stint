package calls

import (
	"database/sql"
	"path/filepath"
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
[[rules]]
project = "beta"
app = "B"
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
	p := ingest.Params(cfg, rules.New(cfg), timeAt(100_000))
	return sq, p
}

func zoom(ts int64, app string) attribute.Sample {
	return attribute.Sample{TS: ts, Kind: "sample", App: app, CallApp: "us.zoom.xos"}
}
func work(ts int64, app string) attribute.Sample {
	return attribute.Sample{TS: ts, Kind: "sample", App: app}
}

// tenMinuteCall: A before, 10 min of zoom (first sample matched), then plain work that closes it.
func tenMinuteCall() []attribute.Sample {
	s := []attribute.Sample{work(0, "A")}
	for ts := int64(1000); ts < 1600; ts += 60 {
		s = append(s, zoom(ts, ""))
	}
	s[1].App = "A"
	return append(s, work(1600, "B"), work(1800, "B"))
}

func TestDetectInsertsClosedStretchOnce(t *testing.T) {
	sq, p := setup(t)
	samples := tenMinuteCall()
	rows, err := Detect(sq, samples, p, 500)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	c := rows[0]
	if c.FromTS != 1000 || c.ToTS != 1600 || c.CallApp != "us.zoom.xos" || c.GuessProject != "alpha" ||
		c.Breakdown != `[{"project":"alpha","seconds":60},{"project":"meeting","seconds":540},{"background":"alpha","seconds":600},{"background":"away","seconds":0}]` {
		t.Fatalf("row: %+v", c)
	}
	again, err := Detect(sq, samples, p, 500)
	if err != nil || len(again) != 0 {
		t.Fatalf("second detect inserted %+v err=%v", again, err)
	}
	if all, _ := db.ReadCalls(sq, 0); len(all) != 1 {
		t.Fatalf("calls: %+v", all)
	}
}

func TestDetectSkipsShortOpenAndOld(t *testing.T) {
	sq, p := setup(t)
	short := []attribute.Sample{zoom(0, "A"), zoom(60, ""), work(400, "B")} // 180 s
	if rows, _ := Detect(sq, short, p, 0); len(rows) != 0 {
		t.Fatalf("short call inserted: %+v", rows)
	}
	open := tenMinuteCall()[:11] // no closing row
	if rows, _ := Detect(sq, open, p, 0); len(rows) != 0 {
		t.Fatalf("open call inserted: %+v", rows)
	}
	// Closed by a row at or before sinceTS (already known to an earlier run, or before the deploy).
	if rows, _ := Detect(sq, tenMinuteCall(), p, 1800); len(rows) != 0 {
		t.Fatalf("old call inserted: %+v", rows)
	}
}

// Open on one run, closed on the next: exactly one row, on the second run.
// Two shapes: run 1 ends on an in-call row, and run 1 ends on the first non-call row
// after the call (the collector writes one the moment the mic is released), which is
// then the watermark: the call is still open because the gap has not elapsed.
func TestDetectOpenThenClosed(t *testing.T) {
	all := tenMinuteCall()
	for _, c := range []struct {
		name  string
		first []attribute.Sample
		wmTS  int64
	}{
		{"run 1 ends in the call", all[:11], 1540},
		{"run 1 ends on the non-call row that follows the call", all[:12], 1600},
	} {
		sq, p := setup(t)
		if rows, _ := Detect(sq, c.first, p, 500); len(rows) != 0 {
			t.Fatalf("%s: first run inserted %+v", c.name, rows)
		}
		rows, err := Detect(sq, all, p, c.wmTS)
		if err != nil || len(rows) != 1 || rows[0].FromTS != 1000 {
			t.Fatalf("%s: second run: %+v err=%v", c.name, rows, err)
		}
		if again, _ := Detect(sq, all, p, 1800); len(again) != 0 {
			t.Fatalf("%s: third run inserted %+v", c.name, again)
		}
	}
}

func TestRefreshUpdatesPendingOnly(t *testing.T) {
	sq, p := setup(t)
	samples := tenMinuteCall()
	Detect(sq, samples, p, 0)
	// A rules change: app "" now matches beta. Simulate with a new matcher.
	q := p
	q.Match = func(f rules.Fields) (rules.Match, bool) {
		if f.App == "A" {
			return rules.Match{Project: "alpha", RuleID: 0}, true
		}
		return rules.Match{Project: "beta", RuleID: 1}, true
	}
	n, err := Refresh(sq, samples, q)
	if err != nil || n != 1 {
		t.Fatalf("refresh n=%d err=%v", n, err)
	}
	c, _, _ := db.ReadCall(sq, 1000)
	if c.Breakdown != `[{"project":"beta","seconds":540},{"project":"alpha","seconds":60},{"background":"beta","seconds":540},{"background":"alpha","seconds":60},{"background":"away","seconds":0}]` {
		t.Fatalf("breakdown not refreshed: %+v", c)
	}
	db.SetDecision(sq, 1000, attribute.DecisionSplit, "", "", 5)
	if n, _ := Refresh(sq, samples, p); n != 0 {
		t.Fatalf("decided row refreshed: n=%d", n)
	}
}

func timeAt(ts int64) time.Time { return time.Unix(ts, 0) }
