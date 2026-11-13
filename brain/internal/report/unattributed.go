// Package report ranks unattributed activity and renders the weekly markdown.
package report

import (
	"database/sql"
	"sort"

	"stint/internal/attribute"
	"stint/internal/db"
)

// Entry is one ranked unattributed title/URL.
type Entry struct {
	Label   string
	Seconds int64
}

// TopUnattributed ranks the titles/URLs behind unattributed intervals in [fromTS, toTS).
// Each sample's working coverage is clipped to the unattributed intervals it overlaps,
// and those are clipped to the window; the read starts one gap before fromTS so a sample
// whose coverage straddles the window start is counted. Idle back-dating by a sample
// after toTS is not seen (bounded by IdleSeconds), so the tail of the window may credit
// slightly more than the intervals do.
func TopUnattributed(sq *sql.DB, p attribute.Params, fromTS, toTS int64, n int) ([]Entry, error) {
	ivs, err := db.ReadIntervals(sq, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	var un []attribute.Interval
	for _, iv := range ivs {
		if iv.Project == attribute.Unattributed {
			iv.From, iv.To = max(iv.From, fromTS), min(iv.To, toTS)
			un = append(un, iv)
		}
	}
	if len(un) == 0 {
		return nil, nil
	}
	samples, err := db.ReadSamplesBetween(sq, fromTS-p.GapSeconds, toTS)
	if err != nil {
		return nil, err
	}
	return rank(samples, attribute.Coverage(samples, p), un, n), nil
}

// rank is the pure part of TopUnattributed. spans must be attribute.Coverage(samples, p),
// one per sample, and n >= 0.
func rank(samples []attribute.Sample, spans []attribute.Span, unattributed []attribute.Interval, n int) []Entry {
	secs := map[string]int64{}
	for i, sp := range spans {
		if !sp.Working {
			continue
		}
		for _, iv := range unattributed {
			from, to := max(sp.From, iv.From), min(sp.To, iv.To)
			if to > from {
				secs[Label(samples[i])] += to - from
			}
		}
	}
	out := make([]Entry, 0, len(secs))
	for l, s := range secs {
		out = append(out, Entry{Label: l, Seconds: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seconds != out[j].Seconds {
			return out[i].Seconds > out[j].Seconds
		}
		return out[i].Label < out[j].Label
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Label is what a sample is reported as: its URL, else its title, else its app.
func Label(s attribute.Sample) string {
	switch {
	case s.URL != "":
		return s.URL
	case s.Title != "":
		return s.Title
	case s.App != "":
		return s.App
	}
	return "(no title)"
}
