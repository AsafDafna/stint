package attribute

import (
	"strconv"

	"stint/internal/rules"
)

// state is the carry-over memory between samples.
type state struct {
	last          Interval // most recent rule-matched or call-attributed project (RuleID unused)
	lastOK        bool     // false until something was attributed, and again after idle, a reset row or a background call's app in front
	lastMatchedTo int64    // end of the most recent attributed span
	openIdx       int      // index in out of the interval being extended, -1 when none
}

// Attribute converts samples (ordered by id) into intervals. A sample inside a background
// call (BackgroundApps) is not in a call, so idle applies to it. Per sample, in order:
//  1. in a call: a decided calls row whose span contains this block's start wins
//     (DecisionProject: every span to the chosen project, RuleCallProject; DecisionSplit:
//     each span to its own rule match, else to the row's guess, RuleCallMeeting); otherwise,
//     DecisionBackground included (only its joined-meeting blocks are still in a call),
//     the call's project, fixed once for the run of consecutive rows with the same set of
//     mic holders: the first rule match inside the call, else the project whose attributed
//     span ended less than CarryoverSeconds before this span (labelled RuleCall), else
//     Unattributed;
//  2. idle (idle_s >= IdleSeconds) or a sleep/resign/term row: no project, carry-over reset;
//  3. first matching rule;
//  4. inside a background call, its call app in front: no project, carry-over reset;
//  5. inside an applied stretch answer's window, the entry for the span's RowKey: its
//     project (RuleStretch), or for Unattributed RuleStretchLeave, which leaves the
//     carry-over state as it was;
//  6. otherwise the previous attributed project (with its subproject) if its span ended
//     less than CarryoverSeconds ago (labelled RuleCarryover), else Unattributed.
//
// Consecutive spans with the same (project, subproject, rule label) and no hole merge into
// one interval, so a carried-over stretch is its own row labelled RuleCarryover. The slice
// must start at a cold point (see ingest) so no state is needed.
func Attribute(samples []Sample, p Params) []Interval {
	bg := BackgroundApps(samples, p)
	spans := coverage(samples, p, bg)
	var out []Interval
	st := state{openIdx: -1}
	emit := func(sp Span, proj, sub, rule string) {
		if sp.To <= sp.From {
			return
		}
		if st.openIdx >= 0 {
			o := &out[st.openIdx]
			if o.To == sp.From && o.Project == proj && o.Subproject == sub && o.RuleID == rule {
				o.To = sp.To
				return
			}
		}
		out = append(out, Interval{From: sp.From, To: sp.To, Project: proj, Subproject: sub, RuleID: rule})
		st.openIdx = len(out) - 1
	}
	attributed := func(sp Span, proj, sub string) {
		st.last = Interval{Project: proj, Subproject: sub}
		st.lastOK = true
		st.lastMatchedTo = sp.To
	}
	carry := func(sp Span) bool {
		return st.lastOK && sp.From-st.lastMatchedTo < p.CarryoverSeconds
	}

	for i := 0; i < len(samples); {
		s, sp := samples[i], spans[i]
		if !sp.Working {
			// Every non-working row closes the open interval, wake/active included. Assumes the
			// collector never writes a wake/active row at the same ts as the following work
			// row; if it did, one mergeable interval would land as two adjacent rows.
			st.openIdx = -1
			if IsIdle(s, sp, p) || IsResetRow(s.Kind) {
				st.lastOK = false
			}
			i++
			continue
		}
		if sp.InCall {
			key := CallKey(s.CallApp)
			end := i
			for end < len(samples) && spans[end].InCall && CallKey(samples[end].CallApp) == key {
				end++
			}
			if d, ok := decisionFor(sp.From, p.Decisions); ok && d.Decision != DecisionBackground {
				for j := i; j < end; j++ {
					proj, sub, rule := d.Project, d.Subproject, RuleCallProject
					if d.Decision == DecisionSplit {
						if m, ok := p.Match(fields(samples[j])); ok {
							proj, sub, rule = m.Project, m.Subproject, strconv.Itoa(m.RuleID)
						} else {
							rule = RuleCallMeeting
						}
					}
					emit(spans[j], proj, sub, rule)
					if proj != Unattributed {
						attributed(spans[j], proj, sub)
					}
				}
				i = end
				continue
			}
			proj, sub, rule := Unattributed, "", RuleUnattributed
			if m, ok := firstMatch(samples[i:end], p); ok {
				proj, sub, rule = m.Project, m.Subproject, strconv.Itoa(m.RuleID)
			} else if carry(sp) {
				proj, sub, rule = st.last.Project, st.last.Subproject, RuleCall
			}
			for j := i; j < end; j++ {
				emit(spans[j], proj, sub, rule)
				// Invariant: config rejects a project named "unattributed", so this string test
				// is exactly "a rule or carry-over applied".
				if proj != Unattributed {
					attributed(spans[j], proj, sub)
				}
			}
			i = end
			continue
		}
		if m, ok := p.Match(fields(s)); ok {
			emit(sp, m.Project, m.Subproject, strconv.Itoa(m.RuleID))
			attributed(sp, m.Project, m.Subproject)
		} else if len(bg) > 0 && bg[i] != "" && s.App == bg[i] {
			st.openIdx = -1
			st.lastOK = false
		} else if e, ok := answerFor(sp.From, s, p.Answers); ok {
			if e.Project == Unattributed {
				emit(sp, Unattributed, "", RuleStretchLeave)
			} else {
				emit(sp, e.Project, e.Subproject, RuleStretch)
				attributed(sp, e.Project, e.Subproject)
			}
		} else if carry(sp) {
			emit(sp, st.last.Project, st.last.Subproject, RuleCarryover)
		} else {
			emit(sp, Unattributed, "", RuleUnattributed)
		}
		i++
	}
	return out
}

// firstMatch is the first rule match inside a call's samples.
func firstMatch(call []Sample, p Params) (rules.Match, bool) {
	for _, s := range call {
		if m, ok := p.Match(fields(s)); ok {
			return m, true
		}
	}
	return rules.Match{}, false
}
