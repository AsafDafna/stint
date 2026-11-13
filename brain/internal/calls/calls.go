// Package calls turns closed call stretches into pending calls rows for the collector's
// pop-up and refreshes pending rows after a rules change.
package calls

import (
	"database/sql"
	"encoding/json"

	"stint/internal/attribute"
	"stint/internal/db"
)

// MinSeconds: shorter stretches take the guess silently and get no row.
const MinSeconds = 300

// Detect inserts a pending row for every closed stretch of at least MinSeconds among
// samples (a slice starting at a cold point) whose closing row is newer than sinceTS,
// and returns the rows it inserted. sinceTS is the previous watermark's ts, so a run only
// asks about calls closed by rows it ingested (the watermark row itself may be the first
// non-call sample after a call, before the gap that closes it: that call closes on a
// later run and is caught then); rows already present are left alone.
func Detect(sq *sql.DB, samples []attribute.Sample, p attribute.Params, sinceTS int64) ([]db.Call, error) {
	var out []db.Call
	for _, st := range attribute.Stretches(samples, p) {
		if !st.Closed || st.ToTS-st.FromTS < MinSeconds || st.ClosedTS <= sinceTS {
			continue
		}
		c, err := row(samples, st, p)
		if err != nil {
			return out, err
		}
		inserted, err := db.InsertCall(sq, c)
		if err != nil {
			return out, err
		}
		if inserted {
			out = append(out, c)
		}
	}
	return out, nil
}

// Refresh rewrites the guess and breakdown of the pending rows among samples' closed
// stretches (after `recompute --since`); decided rows are untouched. Never inserts.
func Refresh(sq *sql.DB, samples []attribute.Sample, p attribute.Params) (int, error) {
	n := 0
	for _, st := range attribute.Stretches(samples, p) {
		if !st.Closed || st.ToTS-st.FromTS < MinSeconds {
			continue
		}
		c, err := row(samples, st, p)
		if err != nil {
			return n, err
		}
		updated, err := db.RefreshPendingCall(sq, c)
		if err != nil {
			return n, err
		}
		if updated {
			n++
		}
	}
	return n, nil
}

func row(samples []attribute.Sample, st attribute.Stretch, p attribute.Params) (db.Call, error) {
	proj, sub := attribute.Guess(samples, st, p)
	split, bg := attribute.Breakdown(samples, st, p), attribute.BackgroundBreakdown(samples, st, p)
	lines := make([]any, 0, len(split)+len(bg))
	for _, l := range split {
		lines = append(lines, l)
	}
	for _, l := range bg {
		lines = append(lines, l)
	}
	bd, err := json.Marshal(lines)
	if err != nil {
		return db.Call{}, err
	}
	return db.Call{FromTS: st.FromTS, ToTS: st.ToTS, CallApp: st.Key, GuessProject: proj, GuessSubproject: sub, Breakdown: string(bd)}, nil
}
