package attribute

import "stint/internal/rules"

// testMatch: app A -> alpha (rule 0); app B -> beta with subproject = cwd (rule 1);
// app C -> alpha again (rule 2).
func testMatch(f rules.Fields) (rules.Match, bool) {
	switch f.App {
	case "A":
		return rules.Match{Project: "alpha", RuleID: 0}, true
	case "B":
		return rules.Match{Project: "beta", Subproject: f.CWD, RuleID: 1}, true
	case "C":
		return rules.Match{Project: "alpha", RuleID: 2}, true
	}
	return rules.Match{}, false
}

var testParams = Params{
	GapSeconds:       120,
	IdleSeconds:      300,
	CarryoverSeconds: 600,
	Now:              1_000_000,
	CallApps:         map[string]bool{"us.zoom.xos": true, "com.tinyspeck.slackmacgap": true},
	Match:            testMatch,
}

// work builds a sample row; call and idle are optional.
func work(ts int64, app string, idle int64, call string) Sample {
	return Sample{TS: ts, Kind: "sample", App: app, IdleS: idle, CallApp: call}
}

func row(ts int64, kind string) Sample { return Sample{TS: ts, Kind: kind} }
