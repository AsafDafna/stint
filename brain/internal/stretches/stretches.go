// Package stretches turns closed unattributed stretches into rows for the collector's pop-up
// and refreshes unanswered rows after every rebuild. It labels
// spans from the intervals already written and never re-runs attribution, so answered windows,
// call decisions and rules learned earlier in the run are seen exactly as intervals record them.
package stretches

import (
	"database/sql"
	"encoding/json"
	"sort"

	"stint/internal/attribute"
	"stint/internal/db"
)

// MinUnattributed: a closed stretch with less unattributed time gets no row.
const MinUnattributed = 600

// historyDays: an app that carried a URL or a cwd this recently never offers "always".
const historyDays = 30

// noEnd as ReadIntervals' toTS reads every interval from fromTS on.
const noEnd = 1<<62 - 1

// KeySeconds is one entry of stretches.breakdown.
type KeySeconds struct {
	Key      string `json:"key"`
	Seconds  int64  `json:"seconds"`
	NoAlways bool   `json:"no_always,omitempty"`
}

// Window is one stretch as Find sees it.
type Window struct {
	FromTS, ToTS int64            // start of the first unmatched span, end of the last
	Unattributed int64            // seconds labelled unattributed; carry-over is not counted
	Keys         map[string]int64 // unmatched seconds (unattributed and carry-over) per attribute.RowKey
	Closed       bool
	ClosedTS     int64 // ts of the row that closed it; 0 while open
}

// piece is the unmatched part of one working, non-call span.
type piece struct {
	from, to     int64
	key          string
	unattributed bool
}

// unmatched clips every working, non-call span against the intervals labelled unattributed
// or carryover (ivs ordered by from_ts and non-overlapping, as db.ReadIntervals returns them).
// out[i] holds samples[i]'s pieces.
func unmatched(samples []attribute.Sample, spans []attribute.Span, ivs []attribute.Interval) [][]piece {
	out := make([][]piece, len(samples))
	for i, sp := range spans {
		if !sp.Working || sp.InCall || sp.To <= sp.From {
			continue
		}
		for k := sort.Search(len(ivs), func(k int) bool { return ivs[k].To > sp.From }); k < len(ivs) && ivs[k].From < sp.To; k++ {
			iv := ivs[k]
			if iv.RuleID != attribute.RuleUnattributed && iv.RuleID != attribute.RuleCarryover {
				continue
			}
			if from, to := max(sp.From, iv.From), min(sp.To, iv.To); to > from {
				out[i] = append(out[i], piece{from, to, attribute.RowKey(samples[i]), iv.RuleID == attribute.RuleUnattributed})
			}
		}
	}
	return out
}

// Find lists the stretches among samples (a slice starting at a cold point) in order; spans
// are attribute.Coverage(samples, p) and ivs the intervals from samples[0].TS on. An unmatched
// piece joins the current stretch when it starts less than GapSeconds after the stretch's
// ToTS with only working, non-call spans in between (a rule-matched blip is bridged); it is
// closed by the first later row whose span is non-working (idle, lock screen, boundary rows)
// or in a call, or by any row at least GapSeconds after its ToTS. ClosedTS is that row's ts,
// except for a row emptied by idle back-dating: the stretch then closes at the idle row that
// emptied it, the row whose arrival closed it, so the watermark test sees the close in the run
// that ingested the idle row rather than never.
func Find(samples []attribute.Sample, spans []attribute.Span, ivs []attribute.Interval, p attribute.Params) []Window {
	pieces := unmatched(samples, spans, ivs)
	var out []Window
	cur := -1 // index in out of the open stretch
	closeAt := func(ts int64) {
		if cur >= 0 {
			out[cur].Closed, out[cur].ClosedTS = true, ts
			cur = -1
		}
	}
	for i, s := range samples {
		if !spans[i].Working || spans[i].InCall {
			closeAt(closingTS(samples, spans, i, p))
			continue
		}
		if cur >= 0 && s.TS >= out[cur].ToTS+p.GapSeconds {
			closeAt(s.TS)
		}
		for _, pc := range pieces[i] {
			if cur >= 0 && pc.from-out[cur].ToTS >= p.GapSeconds {
				closeAt(s.TS)
			}
			if cur < 0 {
				out = append(out, Window{FromTS: pc.from, Keys: map[string]int64{}})
				cur = len(out) - 1
			}
			w := &out[cur]
			w.ToTS = pc.to
			w.Keys[pc.key] += pc.to - pc.from
			if pc.unattributed {
				w.Unattributed += pc.to - pc.from
			}
		}
	}
	return out
}

// closingTS is the ts at which the non-working or in-call row i closes a stretch: its own, or,
// for a work row that idle back-dating emptied, that of the later idle row that did it.
func closingTS(samples []attribute.Sample, spans []attribute.Span, i int, p attribute.Params) int64 {
	if !attribute.IsWorkRow(samples[i].Kind) || spans[i].InCall || attribute.IsIdle(samples[i], spans[i], p) {
		return samples[i].TS
	}
	for j := i + 1; j < len(samples); j++ {
		if attribute.IsIdle(samples[j], spans[j], p) {
			return samples[j].TS
		}
	}
	return samples[i].TS
}

// Detect inserts a row for every closed stretch among samples (a slice starting at a cold
// point) with at least MinUnattributed seconds, whose closing row is newer than sinceTS (the
// watermark ts captured before the rebuild, so old stretches are never asked about) and whose
// window overlaps no existing row, and returns the rows it inserted.
func Detect(sq *sql.DB, samples []attribute.Sample, p attribute.Params, sinceTS int64) ([]db.Stretch, error) {
	if len(samples) == 0 {
		return nil, nil
	}
	ivs, err := db.ReadIntervals(sq, samples[0].TS, noEnd)
	if err != nil {
		return nil, err
	}
	var hist map[string]bool
	var out []db.Stretch
	for _, w := range Find(samples, attribute.Coverage(samples, p), ivs, p) {
		if !w.Closed || w.Unattributed < MinUnattributed || w.ClosedTS <= sinceTS {
			continue
		}
		if hist == nil {
			if hist, err = history(sq, samples, p); err != nil {
				return out, err
			}
		}
		bd, err := breakdown(w.Keys, hist)
		if err != nil {
			return out, err
		}
		s := db.Stretch{FromTS: w.FromTS, ToTS: w.ToTS, UnattributedS: w.Unattributed, Breakdown: bd}
		inserted, err := db.InsertStretch(sq, s)
		if err != nil {
			return out, err
		}
		if inserted {
			out = append(out, s)
		}
	}
	return out, nil
}

// Refresh re-sums unattributed_s and the breakdown of every unanswered row (never decided or
// re-opened) whose window starts at or after samples[0], the rebuild's start; earlier windows
// were not rebuilt. The window itself never moves. A never-decided row that falls below
// MinUnattributed is deleted (a rule written since already answers it); a decided one is kept
// with whatever keys remain. It reports the rows it changed and deleted.
func Refresh(sq *sql.DB, samples []attribute.Sample, p attribute.Params) (refreshed, deleted int, err error) {
	if len(samples) == 0 {
		return 0, 0, nil
	}
	rows, err := db.UnansweredStretches(sq, samples[0].TS)
	if err != nil || len(rows) == 0 {
		return 0, 0, err
	}
	ivs, err := db.ReadIntervals(sq, samples[0].TS, noEnd)
	if err != nil {
		return 0, 0, err
	}
	hist, err := history(sq, samples, p)
	if err != nil {
		return 0, 0, err
	}
	pieces := unmatched(samples, attribute.Coverage(samples, p), ivs)
	for _, r := range rows {
		keys, un := sumWindow(pieces, r.FromTS, r.ToTS)
		if r.DecidedAt == 0 && un < MinUnattributed {
			ok, err := db.DeleteUndecidedStretch(sq, r.FromTS)
			if err != nil {
				return refreshed, deleted, err
			}
			if ok {
				deleted++
			}
			continue
		}
		bd, err := breakdown(keys, hist)
		if err != nil {
			return refreshed, deleted, err
		}
		if un == r.UnattributedS && bd == r.Breakdown {
			continue
		}
		ok, err := db.RefreshStretch(sq, r.FromTS, un, bd)
		if err != nil {
			return refreshed, deleted, err
		}
		if ok {
			refreshed++
		}
	}
	return refreshed, deleted, nil
}

// sumWindow sums the pieces inside [from, to): seconds per key, and the unattributed seconds.
func sumWindow(pieces [][]piece, from, to int64) (map[string]int64, int64) {
	keys := map[string]int64{}
	var un int64
	for _, ps := range pieces {
		for _, pc := range ps {
			a, b := max(pc.from, from), min(pc.to, to)
			if b <= a {
				continue
			}
			keys[pc.key] += b - a
			if pc.unattributed {
				un += b - a
			}
		}
	}
	return keys, un
}

// history is the apps that carried a URL or a cwd in the last historyDays days or anywhere in
// samples (for no_always).
func history(sq *sql.DB, samples []attribute.Sample, p attribute.Params) (map[string]bool, error) {
	return db.AppsWithURLOrCWD(sq, min(p.Now-historyDays*86400, samples[0].TS))
}

// breakdown renders keys as stretches.breakdown JSON: seconds descending, then key.
func breakdown(keys map[string]int64, hist map[string]bool) (string, error) {
	out := make([]KeySeconds, 0, len(keys))
	for k, s := range keys {
		out = append(out, KeySeconds{Key: k, Seconds: s, NoAlways: attribute.NoAlways(k, hist)})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Seconds != out[b].Seconds {
			return out[a].Seconds > out[b].Seconds
		}
		return out[a].Key < out[b].Key
	})
	js, err := json.Marshal(out)
	return string(js), err
}
