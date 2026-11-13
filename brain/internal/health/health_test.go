package health

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
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

const testTOML = `
[settings]
vault_reports_dir = "/tmp/x"
call_apps = ["us.zoom.xos"]
[projects.alpha]
[[rules]]
project = "alpha"
app = "A"
`

func cfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(testTOML), "/h")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var params = attribute.Params{GapSeconds: 120, IdleSeconds: 300, CarryoverSeconds: 600, CallApps: map[string]bool{"us.zoom.xos": true}}

// noon is a Wednesday 12:00 local, inside active hours.
func noon(t *testing.T) time.Time {
	return time.Date(2026, 9, 16, 12, 0, 0, 0, cfg(t).Location)
}

func heartbeats(from, to, step int64, app, title, url string) []attribute.Sample {
	var out []attribute.Sample
	for ts := from; ts < to; ts += step {
		out = append(out, attribute.Sample{TS: ts, Kind: "heartbeat", App: app, Title: title, URL: url})
	}
	return out
}

// idle marks every sample as past the idle threshold.
func idle(samples []attribute.Sample, idleS int64) []attribute.Sample {
	for i := range samples {
		samples[i].IdleS = idleS
	}
	return samples
}

func TestEvaluateTable(t *testing.T) {
	c := cfg(t)
	now := noon(t)
	n := now.Unix()
	fresh := heartbeats(n-3600, n, 60, "A", "t", "")
	cases := []struct {
		name  string
		facts Facts
		now   time.Time
		want  string // substring of a reason, or "" for healthy
	}{
		{"healthy", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", LastBoundary: "wake", Recent: fresh}, now, ""},
		{"no samples at all", Facts{}, now, "no samples in the database"},
		{"stale while awake", Facts{HasSamples: true, LatestTS: n - 1801, LastKind: "heartbeat", LastBoundary: "wake"}, now, "no samples for 30 min while awake"},
		{"stale but asleep, first runs", Facts{HasSamples: true, LatestTS: n - 7200, LastKind: "sleep", LastBoundary: "sleep", StaleRuns: 11}, now, ""},
		{"stale and asleep for 12 runs", Facts{HasSamples: true, LatestTS: n - 7200, LastKind: "sleep", LastBoundary: "sleep", StaleRuns: 12}, now, "no samples for 120 min after sleep"},
		{"stale but resigned, bounded too", Facts{HasSamples: true, LatestTS: n - 7200, LastKind: "resign", LastBoundary: "resign", StaleRuns: 12}, now, "after resign"},
		{"trailing term is a clean stop", Facts{HasSamples: true, LatestTS: n - 7200, LastKind: "term", LastBoundary: "term", StaleRuns: 20}, now, ""},
		{"term followed by work rows counts normally", Facts{HasSamples: true, LatestTS: n - 7200, LastKind: "heartbeat", LastBoundary: "term"}, now, "no samples for 120 min while awake"},
		{"outside active hours nothing fires", Facts{}, time.Date(2026, 9, 16, 2, 0, 0, 0, c.Location), ""},
		{"title NULL on most samples", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: heartbeats(n-3600, n, 60, "A", "", "")}, now, "title NULL on 100%"},
		{"title NULL below half is fine", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: append(heartbeats(n-3600, n-1800, 60, "A", "", ""), heartbeats(n-1800, n, 60, "A", "t", "")...)}, now, ""},
		{"title NULL on no-title apps ignored", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: heartbeats(n-3600, n, 60, "com.apple.finder", "", "")}, now, ""},
		{"title NULL on too few rows ignored", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: heartbeats(n-600, n, 60, "A", "", "")}, now, ""},
		{"url NULL over an hour of browser foreground", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: heartbeats(n-7200, n, 60, "com.google.Chrome", "t", "")}, now, "url NULL for 120 min"},
		{"url NULL over an hour of Search foreground", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: heartbeats(n-7200, n, 60, "com.officecommun.search", "t", "")}, now, "url NULL for 120 min"},
		{"url NULL while idle is fine", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: idle(heartbeats(n-7200, n, 60, "com.google.Chrome", "t", ""), params.IdleSeconds)}, now, ""},
		{"url NULL under an hour is fine", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: append(heartbeats(n-7200, n-3600, 60, "com.google.Chrome", "t", "https://x"), heartbeats(n-3600, n, 60, "com.google.Chrome", "t", "")...)}, now, ""},
		{"url NULL in a non-browser app is fine", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: heartbeats(n-7200, n, 60, "A", "t", "")}, now, ""},
		{"a stretch answer that failed to apply", Facts{HasSamples: true, LatestTS: n - 30, LastKind: "heartbeat", Recent: fresh, StretchErrs: 2, StretchErr: `project "ghost" is not in rules.toml`}, now, `stretch answer not applied (2): project "ghost" is not in rules.toml`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Production passes ingest.Params with Now set; the last row's span is capped at it.
			p := params
			p.Now = tc.now.Unix()
			got := Evaluate(tc.facts, c, p, tc.now)
			joined := strings.Join(got, "; ")
			if tc.want == "" && len(got) != 0 {
				t.Fatalf("want healthy, got %q", joined)
			}
			if tc.want != "" && !strings.Contains(joined, tc.want) {
				t.Fatalf("want reason containing %q, got %q", tc.want, joined)
			}
		})
	}
}

func TestCheckWritesMeta(t *testing.T) {
	c := cfg(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	now := noon(t)
	reasons, err := Check(sq, c, params, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 {
		t.Fatalf("empty db should be unhealthy: %v", reasons)
	}
	if v, _, _ := db.GetMeta(sq, MetaHealth); v != "no samples in the database" {
		t.Errorf("meta.health = %q", v)
	}
	if v, _, _ := db.GetMeta(sq, MetaHealthAt); v != "1789549200" {
		t.Errorf("meta.health_at = %q, want 1789549200", v)
	}
	for _, s := range heartbeats(now.Unix()-600, now.Unix(), 60, "A", "t", "") {
		if _, err := db.InsertSample(sq, s); err != nil {
			t.Fatal(err)
		}
	}
	if reasons, err = Check(sq, c, params, now); err != nil || len(reasons) != 0 {
		t.Fatalf("want healthy, got %v %v", reasons, err)
	}
	if v, _, _ := db.GetMeta(sq, MetaHealth); v != "ok" {
		t.Errorf("meta.health = %q, want ok", v)
	}
}

// After a sleep row the staleness alert waits for 12 consecutive in-active-hours runs
// with no new row, then fires once the counter reaches 12.
func TestCheckCountsStaleRunsAfterSleep(t *testing.T) {
	c := cfg(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	start := noon(t)
	db.InsertSample(sq, attribute.Sample{TS: start.Unix() - 7200, Kind: "heartbeat", App: "A", Title: "t"})
	db.InsertSample(sq, attribute.Sample{TS: start.Unix() - 7100, Kind: "sleep"})
	for i := 0; i < 12; i++ {
		reasons, err := Check(sq, c, params, start.Add(time.Duration(i)*5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if len(reasons) != 0 {
			t.Fatalf("run %d must still be exempt, got %v", i+1, reasons)
		}
	}
	reasons, err := Check(sq, c, params, start.Add(60*time.Minute))
	if err != nil || len(reasons) != 1 || !strings.Contains(reasons[0], "after sleep") {
		t.Fatalf("13th run: %v %v", reasons, err)
	}
	// A new row resets the counter.
	db.InsertSample(sq, attribute.Sample{TS: start.Unix() + 3700, Kind: "wake"})
	db.InsertSample(sq, attribute.Sample{TS: start.Unix() + 3705, Kind: "heartbeat", App: "A", Title: "t"})
	if reasons, _ := Check(sq, c, params, start.Add(62*time.Minute)); len(reasons) != 0 {
		t.Fatalf("after a new row: %v", reasons)
	}
	if v, _, _ := db.GetMeta(sq, metaStaleRuns); v != "0" {
		t.Fatalf("counter = %q, want 0", v)
	}
}

// Runs outside active hours neither increment nor reset the counter, so a counter built
// up before 23:00 is continued by the first in-hours run the next morning instead of
// starting over: the Mac slept at 22:00 and the collector never came back; the first evening
// run records the sleep row, the next six count 1..6, the night holds 6, and the alert fires
// on the 12th counted run at 07:25.
func TestCheckFreezesStaleRunsOutsideActiveHours(t *testing.T) {
	c := cfg(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	evening := time.Date(2026, 9, 16, 22, 0, 0, 0, c.Location)
	db.InsertSample(sq, attribute.Sample{TS: evening.Unix() - 100, Kind: "heartbeat", App: "A", Title: "t"})
	db.InsertSample(sq, attribute.Sample{TS: evening.Unix(), Kind: "sleep"})
	for i := 1; i <= 7; i++ {
		if reasons, err := Check(sq, c, params, evening.Add(time.Duration(i)*5*time.Minute)); err != nil || len(reasons) != 0 {
			t.Fatalf("evening run %d: %v %v", i, reasons, err)
		}
	}
	if v, _, _ := db.GetMeta(sq, metaStaleRuns); v != "6" {
		t.Fatalf("counter after seven evening runs = %q, want 6", v)
	}
	night := time.Date(2026, 9, 16, 23, 0, 0, 0, c.Location)
	for i := 0; i < 96; i++ { // 23:00 .. 06:55
		if reasons, err := Check(sq, c, params, night.Add(time.Duration(i)*5*time.Minute)); err != nil || len(reasons) != 0 {
			t.Fatalf("night run %d: %v %v", i, reasons, err)
		}
	}
	if v, _, _ := db.GetMeta(sq, metaStaleRuns); v != "6" {
		t.Fatalf("night runs must leave the counter untouched, got %q", v)
	}
	morning := time.Date(2026, 9, 17, 7, 0, 0, 0, c.Location)
	for i := 0; i < 5; i++ { // 07:00 .. 07:20 → 7..11
		if reasons, err := Check(sq, c, params, morning.Add(time.Duration(i)*5*time.Minute)); err != nil || len(reasons) != 0 {
			t.Fatalf("morning run %d must still be exempt: %v %v", i, reasons, err)
		}
	}
	if v, _, _ := db.GetMeta(sq, metaStaleRuns); v != "11" {
		t.Fatalf("counter at 07:20 = %q, want 11", v)
	}
	reasons, err := Check(sq, c, params, morning.Add(25*time.Minute))
	if err != nil || len(reasons) != 1 || !strings.Contains(reasons[0], "after sleep") {
		t.Fatalf("07:25 run: %v %v", reasons, err)
	}
	// A row written during the night is noticed by the first morning run: the counter resets.
	db.InsertSample(sq, attribute.Sample{TS: night.Unix() + 3*3600, Kind: "wake"})
	db.InsertSample(sq, attribute.Sample{TS: night.Unix() + 3*3600 + 5, Kind: "heartbeat", App: "A", Title: "t"})
	db.InsertSample(sq, attribute.Sample{TS: night.Unix() + 3*3600 + 60, Kind: "sleep"})
	if reasons, err := Check(sq, c, params, morning.Add(30*time.Minute)); err != nil || len(reasons) != 0 {
		t.Fatalf("after a night row: %v %v", reasons, err)
	}
	if v, _, _ := db.GetMeta(sq, metaStaleRuns); v != "0" {
		t.Fatalf("counter after a new row = %q, want 0", v)
	}
}

// A read failure inside Check must still stamp health_at with a check_failed reason, so
// the menu bar never shows a stale "ok".
func TestCheckReadErrorStillWritesHealth(t *testing.T) {
	c := cfg(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	now := noon(t)
	if _, err := Check(sq, c, params, now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.Exec(`DROP TABLE samples`); err != nil {
		t.Fatal(err)
	}
	reasons, err := Check(sq, c, params, now)
	if err == nil || !strings.Contains(err.Error(), "samples") {
		t.Fatalf("want the read error, got %v %v", reasons, err)
	}
	if len(reasons) != 0 {
		t.Fatalf("reasons on a read error = %v, want none", reasons)
	}
	v, _, _ := db.GetMeta(sq, MetaHealth)
	if !strings.HasPrefix(v, "check_failed: ") || !strings.Contains(v, "samples") {
		t.Errorf("meta.health = %q, want check_failed: <read error>", v)
	}
	if v, _, _ := db.GetMeta(sq, MetaHealthAt); v != "1789549200" {
		t.Errorf("meta.health_at = %q, want 1789549200", v)
	}
}

func TestWriteMetaJoinsReasons(t *testing.T) {
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	now := noon(t)
	if err := WriteMeta(sq, []string{"first", "second"}, now); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := db.GetMeta(sq, MetaHealth); v != "first; second" {
		t.Errorf("meta.health = %q, want %q", v, "first; second")
	}
	if v, _, _ := db.GetMeta(sq, MetaHealthAt); v != "1789549200" {
		t.Errorf("meta.health_at = %q, want 1789549200", v)
	}
}

func TestSchemaReason(t *testing.T) {
	if got := SchemaReason(nil); got != "schema version mismatch" {
		t.Errorf("SchemaReason(nil) = %q", got)
	}
	if got := SchemaReason(errors.New("got \"2\", want \"1\"")); got != "schema version mismatch: got \"2\", want \"1\"" {
		t.Errorf("SchemaReason(err) = %q", got)
	}
}

// The reason follows the column: present while any rules_error is set, gone once it clears.
func TestCheckReportsStretchRulesError(t *testing.T) {
	c := cfg(t)
	sq, err := db.Create(filepath.Join(t.TempDir(), "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	now := noon(t)
	for _, s := range heartbeats(now.Unix()-600, now.Unix(), 60, "A", "t", "") {
		db.InsertSample(sq, s)
	}
	db.InsertStretch(sq, db.Stretch{FromTS: 100, ToTS: 800, UnattributedS: 700, Breakdown: "[]"})
	db.SetStretchRulesError(sq, 100, "boom")
	if reasons, _ := Check(sq, c, params, now); len(reasons) != 1 || reasons[0] != "stretch answer not applied (1): boom" {
		t.Fatalf("reasons = %q", reasons)
	}
	db.SetStretchRulesError(sq, 100, "")
	if reasons, _ := Check(sq, c, params, now); len(reasons) != 0 {
		t.Fatalf("after clearing: %q", reasons)
	}
	if v, _, _ := db.GetMeta(sq, MetaHealth); v != "ok" {
		t.Fatalf("meta.health = %q", v)
	}
}
