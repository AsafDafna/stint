package attribute

import (
	"reflect"
	"testing"
)

func TestAttributeTable(t *testing.T) {
	cases := []struct {
		name string
		in   []Sample
		now  int64
		want []Interval
	}{
		{
			"consecutive same-project samples merge",
			[]Sample{work(0, "A", 0, ""), work(60, "A", 0, ""), work(120, "A", 0, "")},
			180,
			[]Interval{{From: 0, To: 180, Project: "alpha", RuleID: "0"}},
		},
		{
			"project switch splits; subproject kept",
			[]Sample{work(0, "A", 0, ""), {TS: 60, Kind: "sample", App: "B", CWD: "repo"}},
			120,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "beta", Subproject: "repo", RuleID: "1"}},
		},
		{
			"two rules for the same project stay separate rows (rule label differs)",
			[]Sample{work(0, "A", 0, ""), work(60, "C", 0, "")},
			120,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "alpha", RuleID: "2"}},
		},
		{
			"subproject change under the same rule splits",
			[]Sample{{TS: 0, Kind: "sample", App: "B", CWD: "x"}, {TS: 60, Kind: "sample", App: "B", CWD: "y"}},
			120,
			[]Interval{{From: 0, To: 60, Project: "beta", Subproject: "x", RuleID: "1"}, {From: 60, To: 120, Project: "beta", Subproject: "y", RuleID: "1"}},
		},
		{
			"heartbeat rows count like samples",
			[]Sample{work(0, "A", 0, ""), {TS: 60, Kind: "heartbeat", App: "A"}},
			120,
			[]Interval{{From: 0, To: 120, Project: "alpha", RuleID: "0"}},
		},
		{
			"no match within 10 min carries the previous project, then unattributed",
			[]Sample{work(0, "A", 0, ""), work(60, "", 0, ""), work(120, "", 0, ""), work(180, "", 0, ""), work(240, "", 0, ""), work(300, "", 0, ""), work(360, "", 0, ""), work(420, "", 0, ""), work(480, "", 0, ""), work(540, "", 0, ""), work(600, "", 0, ""), work(660, "", 0, "")},
			720,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 660, Project: "alpha", RuleID: RuleCarryover}, {From: 660, To: 720, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"nothing before -> unattributed from the start",
			[]Sample{work(0, "", 0, ""), work(60, "", 0, "")},
			120,
			[]Interval{{From: 0, To: 120, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"idle resets carry-over",
			[]Sample{work(0, "A", 0, ""), work(60, "", 0, ""), work(420, "", 300, ""), work(480, "", 0, "")},
			540,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "alpha", RuleID: RuleCarryover}, {From: 480, To: 540, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"a gap over 2 min leaves a hole but carry-over bridges the project",
			[]Sample{work(0, "A", 0, ""), work(300, "", 0, "")},
			420,
			[]Interval{{From: 0, To: 120, Project: "alpha", RuleID: "0"}, {From: 300, To: 420, Project: "alpha", RuleID: RuleCarryover}},
		},
		{
			"sleep ends the interval; work after wake starts a new one",
			[]Sample{work(0, "A", 0, ""), row(30, "sleep"), row(500, "wake"), work(505, "A", 0, "")},
			560,
			[]Interval{{From: 0, To: 30, Project: "alpha", RuleID: "0"}, {From: 505, To: 560, Project: "alpha", RuleID: "0"}},
		},
		{
			"call project fixed by the first rule match inside the call",
			[]Sample{work(0, "", 0, "us.zoom.xos"), work(60, "A", 0, "us.zoom.xos"), work(120, "", 0, "us.zoom.xos")},
			180,
			[]Interval{{From: 0, To: 180, Project: "alpha", RuleID: "0"}},
		},
		{
			"call project stays fixed when a later sample matches another rule",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), {TS: 60, Kind: "sample", App: "B", CWD: "x", CallApp: "us.zoom.xos"}},
			120,
			[]Interval{{From: 0, To: 120, Project: "alpha", RuleID: "0"}},
		},
		{
			"call without a match carries the project that ended < 10 min ago, labelled call",
			[]Sample{work(0, "A", 0, ""), work(60, "", 0, "us.zoom.xos"), work(120, "", 0, "us.zoom.xos")},
			180,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 180, Project: "alpha", RuleID: RuleCall}},
		},
		{
			"call without a match more than 10 min after the last project is unattributed",
			[]Sample{work(0, "A", 0, ""), work(60, "", 0, ""), work(120, "", 0, ""), work(180, "", 0, ""), work(240, "", 0, ""), work(300, "", 0, ""), work(360, "", 0, ""), work(420, "", 0, ""), work(480, "", 0, ""), work(540, "", 0, ""), work(600, "", 0, ""), work(660, "", 0, ""), work(720, "", 0, ""), work(780, "", 0, "us.zoom.xos")},
			840,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 660, Project: "alpha", RuleID: RuleCarryover}, {From: 660, To: 840, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"a rule match later in the call still claims the whole call",
			[]Sample{work(0, "A", 0, ""), work(60, "", 0, "us.zoom.xos"), work(120, "", 0, "us.zoom.xos"), work(180, "", 0, "us.zoom.xos"), work(240, "", 0, "us.zoom.xos"), work(300, "", 0, "us.zoom.xos"), work(360, "", 0, "us.zoom.xos"), work(420, "", 0, "us.zoom.xos"), work(480, "", 0, "us.zoom.xos"), work(540, "", 0, "us.zoom.xos"), work(600, "", 0, "us.zoom.xos"), work(660, "", 0, "us.zoom.xos"), work(720, "", 0, "us.zoom.xos"), {TS: 780, Kind: "sample", App: "B", CWD: "m", CallApp: "us.zoom.xos"}},
			840,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 840, Project: "beta", Subproject: "m", RuleID: "1"}},
		},
		{
			"sleep resets carry-over",
			[]Sample{work(0, "A", 0, ""), row(30, "sleep"), row(100, "wake"), work(105, "", 0, "")},
			165,
			[]Interval{{From: 0, To: 30, Project: "alpha", RuleID: "0"}, {From: 105, To: 165, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"term and resign reset carry-over; wake and active do not",
			[]Sample{work(0, "A", 0, ""), row(30, "active"), work(35, "", 0, ""), row(60, "term"), work(65, "", 0, ""), work(100, "A", 0, ""), row(130, "resign"), work(135, "", 0, "")},
			195,
			[]Interval{{From: 0, To: 30, Project: "alpha", RuleID: "0"}, {From: 35, To: 60, Project: "alpha", RuleID: RuleCarryover}, {From: 65, To: 100, Project: Unattributed, RuleID: RuleUnattributed}, {From: 100, To: 130, Project: "alpha", RuleID: "0"}, {From: 135, To: 195, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"call identity compares the set of mic holders, not the string",
			[]Sample{work(0, "A", 0, "us.zoom.xos,com.apple.dictation"), {TS: 60, Kind: "sample", App: "B", CWD: "x", CallApp: "com.apple.dictation,us.zoom.xos"}},
			120,
			[]Interval{{From: 0, To: 120, Project: "alpha", RuleID: "0"}},
		},
		{
			"a changed set of mic holders starts a new call",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), {TS: 60, Kind: "sample", App: "B", CWD: "x", CallApp: "us.zoom.xos,com.apple.dictation"}},
			120,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "beta", Subproject: "x", RuleID: "1"}},
		},
		{
			"call with nothing before is unattributed",
			[]Sample{work(0, "", 0, "us.zoom.xos"), work(60, "", 0, "us.zoom.xos")},
			120,
			[]Interval{{From: 0, To: 120, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"idle before the call resets the fallback",
			// alpha keeps [0,100) after back-dating (idle at 400 with idle_s 300 -> last input 100),
			// so without the reset the call at 460 (< 600 s after 100) would carry alpha as RuleCall.
			[]Sample{work(0, "A", 0, ""), work(400, "", 300, ""), work(460, "", 0, "us.zoom.xos")},
			520,
			[]Interval{{From: 0, To: 100, Project: "alpha", RuleID: "0"}, {From: 460, To: 520, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"idle inside a call is ignored",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), work(60, "A", 900, "us.zoom.xos")},
			120,
			[]Interval{{From: 0, To: 120, Project: "alpha", RuleID: "0"}},
		},
		{
			"a different call_app starts a new call with its own project",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), {TS: 60, Kind: "sample", App: "B", CWD: "s", CallApp: "com.tinyspeck.slackmacgap"}},
			120,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "beta", Subproject: "s", RuleID: "1"}},
		},
		{
			"mic holder outside the allowlist is an ordinary sample",
			[]Sample{work(0, "A", 0, "com.apple.dictation"), work(60, "", 600, "com.apple.dictation")},
			120,
			nil,
		},
		{
			"after a call carry-over continues from the call project",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), work(60, "", 0, "")},
			120,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "alpha", RuleID: RuleCarryover}},
		},
		{
			"resign inside a call ends it; the next call starts fresh",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), row(30, "resign"), work(35, "", 0, "us.zoom.xos")},
			90,
			[]Interval{{From: 0, To: 30, Project: "alpha", RuleID: "0"}, {From: 35, To: 90, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"empty input",
			nil,
			0,
			nil,
		},
		{
			"lock screen resets carry-over",
			[]Sample{work(0, "A", 0, ""), work(60, "com.apple.loginwindow", 0, ""), work(120, "", 0, "")},
			240,
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 120, To: 240, Project: Unattributed, RuleID: RuleUnattributed}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testParams
			p.Now = c.now
			got := Attribute(c.in, p)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestCallKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"us.zoom.xos", "us.zoom.xos"},
		{"us.zoom.xos,com.apple.dictation", "com.apple.dictation,us.zoom.xos"},
		{"com.apple.dictation,us.zoom.xos", "com.apple.dictation,us.zoom.xos"},
		{" us.zoom.xos , com.apple.dictation ", "com.apple.dictation,us.zoom.xos"},
		{"us.zoom.xos,us.zoom.xos", "us.zoom.xos"},
		{",us.zoom.xos,,", "us.zoom.xos"},
	}
	for _, c := range cases {
		if got := CallKey(c.in); got != c.want {
			t.Errorf("CallKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAttributeCallDecisions(t *testing.T) {
	call := []Sample{work(0, "", 0, "us.zoom.xos"), work(60, "A", 0, "us.zoom.xos"), work(120, "", 0, "us.zoom.xos")}
	cases := []struct {
		name string
		in   []Sample
		now  int64
		ds   []Decision
		want []Interval
	}{
		{
			"project decision books every span to the chosen project",
			call, 180,
			[]Decision{{FromTS: 0, ToTS: 180, Decision: DecisionProject, Project: "beta", Subproject: "x"}},
			[]Interval{{From: 0, To: 180, Project: "beta", Subproject: "x", RuleID: RuleCallProject}},
		},
		{
			"split: rule match first, everything else to the guess",
			call, 180,
			[]Decision{{FromTS: 0, ToTS: 180, Decision: DecisionSplit, Project: "beta", Subproject: "g"}},
			[]Interval{
				{From: 0, To: 60, Project: "beta", Subproject: "g", RuleID: RuleCallMeeting},
				{From: 60, To: 120, Project: "alpha", RuleID: "0"},
				{From: 120, To: 180, Project: "beta", Subproject: "g", RuleID: RuleCallMeeting},
			},
		},
		{
			"a decision whose span does not contain the block's start is ignored",
			call, 180,
			[]Decision{{FromTS: 500, ToTS: 900, Decision: DecisionProject, Project: "beta"}},
			[]Interval{{From: 0, To: 180, Project: "alpha", RuleID: "0"}},
		},
		{
			"a block starting exactly at a decision's to_ts is outside it (half-open span)",
			call, 180,
			[]Decision{{FromTS: -600, ToTS: 0, Decision: DecisionProject, Project: "beta"}},
			[]Interval{{From: 0, To: 180, Project: "alpha", RuleID: "0"}},
		},
		{
			"split never consults carry-over: unmatched call minutes go to the guess even when it is unattributed",
			[]Sample{work(0, "A", 0, ""), work(60, "", 0, "us.zoom.xos"), work(120, "", 0, "us.zoom.xos")}, 180,
			[]Decision{{FromTS: 60, ToTS: 180, Decision: DecisionSplit, Project: Unattributed}},
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 180, Project: Unattributed, RuleID: RuleCallMeeting}},
		},
		{
			"idle stays suspended inside a split",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), work(60, "", 400, "us.zoom.xos")}, 120,
			[]Decision{{FromTS: 0, ToTS: 120, Decision: DecisionSplit, Project: "beta"}},
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "beta", RuleID: RuleCallMeeting}},
		},
		{
			"one decision covers two CallKey blocks inside its span",
			[]Sample{work(0, "", 0, "us.zoom.xos"), work(60, "", 0, "com.tinyspeck.slackmacgap")}, 120,
			[]Decision{{FromTS: 0, ToTS: 120, Decision: DecisionProject, Project: "beta"}},
			[]Interval{{From: 0, To: 120, Project: "beta", RuleID: RuleCallProject}},
		},
	}
	for _, c := range cases {
		p := testParams
		p.Now = c.now
		p.Decisions = c.ds
		if got := Attribute(c.in, p); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}
