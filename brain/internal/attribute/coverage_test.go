package attribute

import (
	"reflect"
	"testing"
)

func TestCoverageTable(t *testing.T) {
	cases := []struct {
		name string
		in   []Sample
		now  int64
		want []Span
	}{
		{
			"covers until next row, capped at gap",
			[]Sample{work(0, "A", 0, ""), work(60, "A", 0, ""), work(300, "A", 0, "")},
			1000,
			[]Span{{0, 60, true, false}, {60, 180, true, false}, {300, 420, true, false}},
		},
		{
			"last row capped at Now",
			[]Sample{work(0, "A", 0, "")},
			50,
			[]Span{{0, 50, true, false}},
		},
		{
			"Now before the last row clamps to zero",
			[]Sample{work(100, "A", 0, "")},
			90,
			[]Span{{100, 100, true, false}},
		},
		{
			"negative delta clamps to zero",
			[]Sample{work(100, "A", 0, ""), work(40, "A", 0, ""), work(70, "A", 0, "")},
			1000,
			[]Span{{100, 100, true, false}, {40, 70, true, false}, {70, 190, true, false}},
		},
		{
			"sleep ends the span at its ts; wake starts nothing",
			[]Sample{work(0, "A", 0, ""), row(30, "sleep"), row(500, "wake"), work(505, "A", 0, "")},
			560,
			[]Span{{0, 30, true, false}, {30, 30, false, false}, {500, 500, false, false}, {505, 560, true, false}},
		},
		{
			"resign and term end the span too",
			[]Sample{work(0, "A", 0, ""), row(10, "resign"), work(20, "A", 0, ""), row(25, "term")},
			1000,
			[]Span{{0, 10, true, false}, {10, 10, false, false}, {20, 25, true, false}, {25, 25, false, false}},
		},
		{
			"idle sample covers nothing and back-dates to ts - idle_s",
			[]Sample{work(0, "A", 0, ""), work(100, "A", 0, ""), work(160, "A", 60, ""), work(220, "A", 120, ""), work(400, "A", 300, "")},
			1000,
			[]Span{{0, 100, true, false}, {100, 100, false, false}, {160, 160, false, false}, {220, 220, false, false}, {400, 400, false, false}},
		},
		{
			"back-dating cuts a straddling span",
			[]Sample{work(0, "A", 0, ""), work(60, "A", 30, ""), work(330, "A", 300, "")},
			1000,
			[]Span{{0, 30, true, false}, {60, 60, false, false}, {330, 330, false, false}},
		},
		{
			"a second idle sample is a no-op",
			[]Sample{work(0, "A", 0, ""), work(300, "A", 300, ""), work(360, "A", 360, "")},
			1000,
			[]Span{{0, 0, false, false}, {300, 300, false, false}, {360, 360, false, false}},
		},
		{
			"in a call idle is ignored",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), work(60, "A", 900, "us.zoom.xos")},
			120,
			[]Span{{0, 60, true, true}, {60, 120, true, true}},
		},
		{
			"idle after a call never eats the call",
			[]Sample{work(0, "A", 0, "us.zoom.xos"), work(60, "A", 100, "us.zoom.xos"), work(120, "A", 160, ""), work(420, "A", 460, "")},
			1000,
			[]Span{{0, 60, true, true}, {60, 120, true, true}, {120, 120, false, false}, {420, 420, false, false}},
		},
		{
			"mic holder outside the allowlist is not a call",
			[]Sample{work(0, "A", 0, "com.apple.dictation"), work(60, "A", 600, "com.apple.dictation")},
			1000,
			[]Span{{0, 0, false, false}, {60, 60, false, false}},
		},
		{
			"a call_app list counts as a call when any id is allowlisted",
			[]Sample{work(0, "A", 0, "com.apple.dictation,us.zoom.xos"), work(60, "A", 600, "us.zoom.xos, com.apple.dictation")},
			120,
			[]Span{{0, 60, true, true}, {60, 120, true, true}},
		},
		{
			"idle back-dating stops at a sleep row",
			[]Sample{work(0, "A", 0, ""), work(60, "A", 30, ""), row(90, "sleep"), row(500, "wake"), work(505, "A", 475, "")},
			1000,
			[]Span{{0, 60, true, false}, {60, 90, true, false}, {90, 90, false, false}, {500, 500, false, false}, {505, 505, false, false}},
		},
		{
			"loginwindow heartbeat covers nothing even with idle 0",
			[]Sample{work(0, "A", 0, ""), work(60, "com.apple.loginwindow", 18, ""), work(120, "A", 0, "")},
			240,
			[]Span{{0, 42, true, false}, {60, 60, false, false}, {120, 240, true, false}},
		},
		{
			"loginwindow during a call still counts",
			[]Sample{work(0, "com.apple.loginwindow", 0, "us.zoom.xos")},
			60,
			[]Span{{0, 60, true, true}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testParams
			p.Now = c.now
			got := Coverage(c.in, p)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}
