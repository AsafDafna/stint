package attribute

import (
	"reflect"
	"testing"
)

const wispr = "com.electron.wispr-flow"

func bgDecision(from, to int64, app string) Decision {
	return Decision{FromTS: from, ToTS: to, Decision: DecisionBackground, BackgroundApp: app}
}

// backgroundFixture: A (alpha) active 0..540, then idle rising 60..600 at 600..1140, all in a Zoom call;
// then three unmatched non-call rows M at 1200..1320.
func backgroundFixture() []Sample {
	var s []Sample
	for ts := int64(0); ts <= 540; ts += 60 {
		s = append(s, work(ts, "A", 0, zoom))
	}
	for ts := int64(600); ts <= 1140; ts += 60 {
		s = append(s, work(ts, "A", ts-540, zoom))
	}
	return append(s, work(1200, "M", 0, ""), work(1260, "M", 0, ""), work(1320, "M", 0, ""))
}

func TestBackgroundCallAppliesIdle(t *testing.T) {
	p := testParams
	p.Decisions = []Decision{bgDecision(0, 1200, zoom)}
	want := []Interval{{0, 540, "alpha", "", "0"}, {1200, 1440, Unattributed, "", RuleUnattributed}}
	if got := Attribute(backgroundFixture(), p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestBackgroundCallUnmatchedCarriesThenUnattributed(t *testing.T) {
	s := []Sample{work(0, "A", 0, zoom)}
	for ts := int64(60); ts <= 960; ts += 60 {
		s = append(s, work(ts, "W", 0, zoom))
	}
	s = append(s, row(1020, "sleep"))
	p := testParams
	p.Decisions = []Decision{bgDecision(0, 1020, zoom)}
	want := []Interval{{0, 60, "alpha", "", "0"}, {60, 660, "alpha", "", RuleCarryover}, {660, 1020, Unattributed, "", RuleUnattributed}}
	if got := Attribute(s, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestBackgroundCallAppInFrontIsAway(t *testing.T) {
	// Zoom in front, in call and in a non-call gap, books nothing and stops carry-over.
	s := []Sample{work(0, "A", 0, zoom), work(60, zoom, 0, zoom), work(120, zoom, 0, ""), work(180, zoom, 0, zoom),
		work(240, "W", 0, zoom), work(300, "A", 0, zoom), row(360, "sleep")}
	p := testParams
	p.Decisions = []Decision{bgDecision(0, 360, zoom)}
	want := []Interval{{0, 60, "alpha", "", "0"}, {240, 300, Unattributed, "", RuleUnattributed}, {300, 360, "alpha", "", "0"}}
	if got := Attribute(s, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	// A call app that matches a rule (a browser call on a ruled URL) still counts.
	q := testParams
	q.CallApps = map[string]bool{"A": true}
	q.Decisions = []Decision{bgDecision(0, 120, "A")}
	if got := Attribute([]Sample{work(0, "A", 0, "A"), work(60, "A", 0, "A"), row(120, "sleep")}, q); !reflect.DeepEqual(got, []Interval{{0, 120, "alpha", "", "0"}}) {
		t.Fatalf("rule match lost: %+v", got)
	}
}

func TestBackgroundJoinedMeetingKeepsCallMode(t *testing.T) {
	// A active in Zoom 0..540; W idle with Slack joining 600..720; W idle with Wispr Flow 780..840.
	var s []Sample
	for ts := int64(0); ts <= 540; ts += 60 {
		s = append(s, work(ts, "A", 0, zoom))
	}
	for ts := int64(600); ts <= 720; ts += 60 {
		s = append(s, work(ts, "W", 400, zoom+","+slack))
	}
	s = append(s, work(780, "W", 400, zoom+","+wispr), work(840, "W", 400, zoom+","+wispr), row(900, "sleep"))
	p := testParams
	p.Decisions = []Decision{bgDecision(0, 900, zoom)}
	want := []Interval{{0, 600, "alpha", "", "0"}, {600, 780, "alpha", "", RuleCall}}
	if got := Attribute(s, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestBackgroundAppIsPickedOnceForTheWholeCall(t *testing.T) {
	// Zoom alone 0..600 (660 s), then Slack alone 660..900 (300 s): Zoom wins the call; a rebuild
	// chunk holding only the Slack tail must still use Zoom (Slack rows are then a joined meeting).
	var s []Sample
	for ts := int64(0); ts <= 600; ts += 60 {
		s = append(s, work(ts, "A", 0, zoom))
	}
	for ts := int64(660); ts <= 900; ts += 60 {
		s = append(s, work(ts, "A", 0, slack))
	}
	s = append(s, row(960, "sleep"))
	app := BackgroundApp(s, bgDecision(0, 960, ""), testParams)
	if app != zoom {
		t.Fatalf("app = %q", app)
	}
	p := testParams
	p.Decisions = []Decision{bgDecision(0, 960, app)}
	full, tail := BackgroundApps(s, p), BackgroundApps(s[11:], p)
	if full[0] != zoom || full[11] != "" || !reflect.DeepEqual(full[11:], tail) {
		t.Fatalf("full %q tail %q", full, tail)
	}
}

func TestBackgroundBreakdown(t *testing.T) {
	s := backgroundFixture()
	st := Stretches(s, testParams)[0] // [0, 1200), closed by M@1320
	want := []BackgroundLine{{"alpha", 540}, {AwayLine, 660}}
	if got := BackgroundBreakdown(s, st, testParams); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestCoverageIgnoresBackgroundDecisions(t *testing.T) {
	p := testParams
	p.Decisions = []Decision{bgDecision(0, 1200, zoom)}
	if sp := Coverage(backgroundFixture(), p)[15]; !sp.InCall || !sp.Working { // ts 900, idle 360, in call
		t.Fatalf("Coverage must keep raw call state: %+v", sp)
	}
}
