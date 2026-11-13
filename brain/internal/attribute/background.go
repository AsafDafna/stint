package attribute

import "strings"

// BackgroundApp is the call app with the most in-call seconds among the samples whose TS is
// inside [d.FromTS, d.ToTS), over the raw Coverage (ties: the smaller id); "" when none.
func BackgroundApp(samples []Sample, d Decision, p Params) string {
	secs := map[string]int64{}
	for i, sp := range Coverage(samples, p) {
		if ts := samples[i].TS; !sp.InCall || ts < d.FromTS || ts >= d.ToTS {
			continue
		}
		for _, id := range strings.Split(samples[i].CallApp, ",") {
			if id = strings.TrimSpace(id); p.CallApps[id] {
				secs[id] += sp.To - sp.From
			}
		}
	}
	best, bestSecs := "", int64(0)
	for id, n := range secs {
		if n > bestSecs || (n == bestSecs && id < best) {
			best, bestSecs = id, n
		}
	}
	return best
}

// BackgroundApps maps each sample to the BackgroundApp of the background decision whose
// [FromTS, ToTS) holds its TS, or "" when none does or the sample is in a joined meeting
// (another call app holds the mic). nil when p.Decisions has no background decision.
func BackgroundApps(samples []Sample, p Params) []string {
	var bgs []Decision
	for _, d := range p.Decisions {
		if d.Decision == DecisionBackground {
			bgs = append(bgs, d)
		}
	}
	if len(bgs) == 0 {
		return nil
	}
	out := make([]string, len(samples))
	for i, s := range samples {
		if d, ok := decisionFor(s.TS, bgs); ok && d.BackgroundApp != "" && !joinedMeeting(s.CallApp, d.BackgroundApp, p.CallApps) {
			out[i] = d.BackgroundApp
		}
	}
	return out
}

// joinedMeeting reports whether a call app other than app holds the mic.
func joinedMeeting(callApp, app string, callApps map[string]bool) bool {
	for _, id := range strings.Split(callApp, ",") {
		if id = strings.TrimSpace(id); id != app && callApps[id] {
			return true
		}
	}
	return false
}
