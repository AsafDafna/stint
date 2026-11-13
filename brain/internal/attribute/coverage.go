package attribute

// Span is the time a sample accounts for. To == From means it accounts for nothing.
type Span struct {
	From    int64
	To      int64
	Working bool // false for boundary rows, idle samples and time removed by idle back-dating
	InCall  bool // a call_app id is in settings.call_apps; Attribute clears it for background-decided calls
}

// Coverage computes each sample's span:
//   - a work row covers [ts, next.ts) capped at GapSeconds; the last row is capped at Now;
//   - boundary rows (sleep/wake/resign/active/term) cover nothing, so a sleep at ts ends the
//     previous span at ts and a wake starts nothing by itself;
//   - a negative delta (next.ts < ts) clamps the span to zero;
//   - an idle sample (idle_s >= IdleSeconds, or a NonWorkApps frontmost app such as the lock
//     screen, not in a call) covers nothing and back-dates: earlier spans lose any time at
//     or after ts - idle_s. The walk stops at an in-call span (idle is ignored in calls) and
//     at a sleep/resign/term row (those already ended everything at their own ts), so it
//     never reaches past a cold point.
//
// Precondition: samples are in id order, which the collector writes chronologically. The
// back-dating walk breaks at the first earlier span ending at or before ts - idle_s, so it
// relies on earlier spans ending no later than later ones.
func Coverage(samples []Sample, p Params) []Span { return coverage(samples, p, nil) }

// coverage is Coverage with every sample where bg[i] != "" (inside a background call, see
// BackgroundApps) taken out of call mode. Only Attribute passes bg.
func coverage(samples []Sample, p Params, bg []string) []Span {
	spans := make([]Span, len(samples))
	for i, s := range samples {
		sp := Span{From: s.TS, To: s.TS}
		if IsWorkRow(s.Kind) {
			end := s.TS + p.GapSeconds
			if i+1 < len(samples) {
				if samples[i+1].TS < end {
					end = samples[i+1].TS
				}
			} else if p.Now < end {
				end = p.Now
			}
			if end < s.TS {
				end = s.TS
			}
			sp.InCall = InCall(s.CallApp, p.CallApps) && (len(bg) == 0 || bg[i] == "")
			sp.Working = sp.InCall || !idleRow(s, p)
			if sp.Working {
				sp.To = end
			}
		}
		spans[i] = sp
	}
	for i, s := range samples {
		if !IsIdle(s, spans[i], p) {
			continue
		}
		lastInput := s.TS - s.IdleS
		for j := i - 1; j >= 0; j-- {
			if spans[j].InCall || IsResetRow(samples[j].Kind) || spans[j].To <= lastInput {
				break
			}
			if spans[j].From >= lastInput {
				spans[j].To = spans[j].From
				spans[j].Working = false
			} else {
				spans[j].To = lastInput
			}
		}
	}
	return spans
}

// idleRow reports whether a work row's frontmost time should not count as active input:
// past the idle threshold, or a frontmost app in which no work can happen (NonWorkApps).
func idleRow(s Sample, p Params) bool {
	return s.IdleS >= p.IdleSeconds || NonWorkApps[s.App]
}

// IsIdle: a work row that is idleRow and not in a call.
func IsIdle(s Sample, sp Span, p Params) bool {
	return IsWorkRow(s.Kind) && !sp.InCall && idleRow(s, p)
}
