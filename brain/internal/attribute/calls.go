package attribute

import "sort"

// Stretch is one meeting as the pop-up sees it: consecutive
// in-call spans, whatever their mic holders, with less than GapSeconds of non-call time
// between one span's end and the next span's start and no reset row in between. Attribute
// still groups blocks by CallKey inside a stretch; a stretch's decision reaches all of them.
type Stretch struct {
	Start, End   int   // sample indices, half-open; non-call rows inside are not part of the call
	FromTS, ToTS int64 // first in-call span start, last in-call span end
	Key          string
	Closed       bool  // a reset row, or any row at least GapSeconds after ToTS, follows
	ClosedTS     int64 // ts of that closing row; 0 while open
}

// Stretches lists the stretches of samples (a slice starting at a cold point) in order.
func Stretches(samples []Sample, p Params) []Stretch {
	spans := Coverage(samples, p)
	var out []Stretch
	for i, sp := range spans {
		if !sp.InCall {
			continue
		}
		if n := len(out); n > 0 && sp.From-out[n-1].ToTS < p.GapSeconds && !resetBetween(samples, out[n-1].End, i) {
			out[n-1].End = i + 1
			out[n-1].ToTS = sp.To
			continue
		}
		out = append(out, Stretch{Start: i, End: i + 1, FromTS: sp.From, ToTS: sp.To, Key: CallKey(samples[i].CallApp)})
	}
	for k := range out {
		out[k].ClosedTS, out[k].Closed = closedAfter(samples, out[k], p)
	}
	return out
}

func resetBetween(samples []Sample, from, to int) bool {
	for j := from; j < to; j++ {
		if IsResetRow(samples[j].Kind) {
			return true
		}
	}
	return false
}

// closedAfter finds the first row that closes the stretch (a reset row, or any row at
// least GapSeconds after ToTS) and returns its ts.
func closedAfter(samples []Sample, st Stretch, p Params) (int64, bool) {
	for j := st.End; j < len(samples); j++ {
		if IsResetRow(samples[j].Kind) || samples[j].TS >= st.ToTS+p.GapSeconds {
			return samples[j].TS, true
		}
	}
	return 0, false
}

// Guess is the stretch's project under today's rule: the first rule match
// across the whole stretch, else what a decision-free attribution gives its first span
// (carry-over from before the call), else Unattributed.
func Guess(samples []Sample, st Stretch, p Params) (project, subproject string) {
	if m, ok := firstMatch(samples[st.Start:st.End], p); ok {
		return m.Project, m.Subproject
	}
	q := p
	q.Decisions = nil
	for _, iv := range Attribute(samples[:st.End], q) {
		if iv.From <= st.FromTS && st.FromTS < iv.To {
			return iv.Project, iv.Subproject
		}
	}
	return Unattributed, ""
}

// MeetingLine is the breakdown bucket for spans a split would book to the guess.
const MeetingLine = "meeting"

// Line is one breakdown entry, stored as JSON in calls.breakdown.
type Line struct {
	Project    string `json:"project"`
	Subproject string `json:"subproject,omitempty"`
	Seconds    int64  `json:"seconds"`
}

// Breakdown is the split in dry-run over the stretch's in-call spans, summed per
// project and subproject: seconds descending, then name; the meeting bucket last.
func Breakdown(samples []Sample, st Stretch, p Params) []Line {
	spans := Coverage(samples, p)
	acc := map[[2]string]int64{}
	for j := st.Start; j < st.End; j++ {
		sp := spans[j]
		if !sp.InCall || sp.To <= sp.From {
			continue
		}
		k := [2]string{MeetingLine, ""}
		if m, ok := p.Match(fields(samples[j])); ok {
			k = [2]string{m.Project, m.Subproject}
		}
		acc[k] += sp.To - sp.From
	}
	out := make([]Line, 0, len(acc))
	for k, s := range acc {
		out = append(out, Line{Project: k[0], Subproject: k[1], Seconds: s})
	}
	sort.Slice(out, func(a, b int) bool {
		am, bm := out[a].Project == MeetingLine, out[b].Project == MeetingLine
		if am != bm {
			return bm
		}
		if out[a].Seconds != out[b].Seconds {
			return out[a].Seconds > out[b].Seconds
		}
		if out[a].Project != out[b].Project {
			return out[a].Project < out[b].Project
		}
		return out[a].Subproject < out[b].Subproject
	})
	return out
}

// AwayLine is the background bucket for the call's seconds that book nothing.
const AwayLine = "away"

// BackgroundLine is one background-hours entry in calls.breakdown. It has no "project" key,
// so a collector that predates it skips it.
type BackgroundLine struct {
	Background string `json:"background"`
	Seconds    int64  `json:"seconds"`
}

// BackgroundBreakdown is what a background answer would book inside the stretch:
// Attribute over all samples with this call decided background and the
// other decisions in p kept, clipped to [FromTS, ToTS). One line per project or
// project/subproject (seconds descending, then name), Unattributed when > 0, AwayLine always.
func BackgroundBreakdown(samples []Sample, st Stretch, p Params) []BackgroundLine {
	d := Decision{FromTS: st.FromTS, ToTS: st.ToTS, Decision: DecisionBackground}
	d.BackgroundApp = BackgroundApp(samples, d, p)
	q := p
	q.Decisions = make([]Decision, 0, len(p.Decisions)+1)
	for _, o := range p.Decisions {
		if o.FromTS != st.FromTS {
			q.Decisions = append(q.Decisions, o)
		}
	}
	q.Decisions = append(q.Decisions, d)
	acc := map[string]int64{}
	var counted int64
	for _, iv := range Attribute(samples, q) {
		from, to := max(iv.From, st.FromTS), min(iv.To, st.ToTS)
		if to <= from {
			continue
		}
		name := iv.Project
		if iv.Subproject != "" {
			name += "/" + iv.Subproject
		}
		acc[name] += to - from
		counted += to - from
	}
	un := acc[Unattributed]
	delete(acc, Unattributed)
	out := make([]BackgroundLine, 0, len(acc)+2)
	for k, s := range acc {
		out = append(out, BackgroundLine{Background: k, Seconds: s})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Seconds != out[b].Seconds {
			return out[a].Seconds > out[b].Seconds
		}
		return out[a].Background < out[b].Background
	})
	if un > 0 {
		out = append(out, BackgroundLine{Background: Unattributed, Seconds: un})
	}
	return append(out, BackgroundLine{Background: AwayLine, Seconds: st.ToTS - st.FromTS - counted})
}
