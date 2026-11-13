package attribute

import (
	"reflect"
	"testing"
)

const zoom, slack = "us.zoom.xos", "com.tinyspeck.slackmacgap"

func TestStretches(t *testing.T) {
	cases := []struct {
		name string
		in   []Sample
		now  int64
		want []Stretch
	}{
		{
			"two runs with under a gap of non-call time between them merge; closed by a row a gap later",
			[]Sample{work(0, "", 0, zoom), work(60, "", 0, zoom), work(120, "A", 0, ""), work(180, "", 0, zoom), work(240, "", 0, zoom), work(300, "A", 0, ""), work(420, "A", 0, "")},
			500,
			[]Stretch{{Start: 0, End: 5, FromTS: 0, ToTS: 300, Key: zoom, Closed: true, ClosedTS: 420}},
		},
		{
			"a gap or more of non-call time splits stretches",
			[]Sample{work(0, "", 0, zoom), work(60, "", 0, zoom), work(120, "A", 0, ""), work(180, "A", 0, ""), work(240, "A", 0, ""), work(300, "", 0, zoom)},
			360,
			[]Stretch{{Start: 0, End: 2, FromTS: 0, ToTS: 120, Key: zoom, Closed: true, ClosedTS: 240}, {Start: 5, End: 6, FromTS: 300, ToTS: 360, Key: zoom, Closed: false}},
		},
		{
			"a reset row between two runs splits them",
			[]Sample{work(0, "", 0, zoom), row(30, "sleep"), row(60, "wake"), work(70, "", 0, zoom)},
			100,
			[]Stretch{{Start: 0, End: 1, FromTS: 0, ToTS: 30, Key: zoom, Closed: true, ClosedTS: 30}, {Start: 3, End: 4, FromTS: 70, ToTS: 100, Key: zoom, Closed: false}},
		},
		{
			"different mic holders within a gap are one stretch keyed by the first",
			[]Sample{work(0, "", 0, zoom), work(60, "", 0, slack), work(300, "A", 0, "")},
			400,
			[]Stretch{{Start: 0, End: 2, FromTS: 0, ToTS: 180, Key: zoom, Closed: true, ClosedTS: 300}},
		},
		{
			"open stretch: nothing follows",
			[]Sample{work(0, "", 0, zoom), work(60, "", 0, zoom)},
			100,
			[]Stretch{{Start: 0, End: 2, FromTS: 0, ToTS: 100, Key: zoom, Closed: false}},
		},
		{
			"a non-call row less than a gap after the last span does not close it",
			[]Sample{work(0, "", 0, zoom), work(60, "A", 0, "")},
			100,
			[]Stretch{{Start: 0, End: 1, FromTS: 0, ToTS: 60, Key: zoom, Closed: false}},
		},
		{
			"an idle non-call row inside the gap window (a one-tick mic drop) does not split the stretch",
			[]Sample{work(0, "", 0, zoom), work(60, "", 0, zoom), work(120, "M", 600, ""), work(180, "", 0, zoom), work(240, "", 0, zoom), work(600, "M", 0, ""), work(1200, "M", 0, "")},
			1300,
			// zoom@240's span is gap-capped at 360 (the next row is at 600), which closes at 600.
			[]Stretch{{Start: 0, End: 5, FromTS: 0, ToTS: 360, Key: zoom, Closed: true, ClosedTS: 600}},
		},
		{
			"no calls, no stretches",
			[]Sample{work(0, "A", 0, ""), work(60, "A", 0, "")},
			100,
			nil,
		},
	}
	for _, c := range cases {
		p := testParams
		p.Now = c.now
		if got := Stretches(c.in, p); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestGuess(t *testing.T) {
	cases := []struct {
		name      string
		in        []Sample
		st        int // stretch index
		proj, sub string
	}{
		{"first rule match across the stretch, even in the second block", []Sample{work(0, "", 0, zoom), work(60, "B", 0, slack), work(300, "", 0, "")}, 0, "beta", ""},
		{"no match: carry-over from before the call", []Sample{work(0, "A", 0, ""), work(60, "", 0, zoom), work(120, "", 0, zoom), work(400, "", 0, "")}, 0, "alpha", ""},
		{"no match, nothing before: unattributed", []Sample{work(0, "", 0, zoom), work(60, "", 0, zoom)}, 0, Unattributed, ""},
		{"match wins over carry-over", []Sample{work(0, "A", 0, ""), work(60, "", 0, zoom), work(120, "B", 0, zoom)}, 0, "beta", ""},
	}
	for _, c := range cases {
		p := testParams
		p.Now = 1000
		sts := Stretches(c.in, p)
		if len(sts) <= c.st {
			t.Fatalf("%s: no stretch %d in %+v", c.name, c.st, sts)
		}
		proj, sub := Guess(c.in, sts[c.st], p)
		if proj != c.proj || sub != c.sub {
			t.Errorf("%s: got %s/%s want %s/%s", c.name, proj, sub, c.proj, c.sub)
		}
	}
}

func TestBreakdown(t *testing.T) {
	in := []Sample{
		work(0, "", 0, zoom),
		work(60, "A", 0, zoom),
		{TS: 120, Kind: "sample", App: "B", CWD: "x", CallApp: zoom},
		work(180, "", 0, zoom),
		work(240, "A", 0, ""), // non-call row inside the stretch's gap window: not part of the call
		work(300, "", 0, zoom),
		work(600, "A", 0, ""),
	}
	p := testParams
	p.Now = 700
	sts := Stretches(in, p)
	if len(sts) != 1 {
		t.Fatalf("stretches: %+v", sts)
	}
	got := Breakdown(in, sts[0], p)
	want := []Line{
		{Project: "alpha", Seconds: 60},
		{Project: "beta", Subproject: "x", Seconds: 60},
		{Project: MeetingLine, Seconds: 240}, // 0-60, 180-240, 300-420 (gap-capped)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}
