package attribute

import (
	"reflect"
	"testing"
)

func TestParseAnswer(t *testing.T) {
	a, err := ParseAnswer(10, 20, `[{"key":"host:grok.com","project":"personal-dev","subproject":"budget","always":true,"label":"grok.com"},{"key":"app:W","project":"unattributed"}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := Answer{FromTS: 10, ToTS: 20, Entries: map[string]AnswerItem{
		"host:grok.com": {Key: "host:grok.com", Project: "personal-dev", Subproject: "budget", Always: true, Label: "grok.com"},
		"app:W":         {Key: "app:W", Project: Unattributed},
	}}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("got %+v", a)
	}
	if _, err := ParseAnswer(10, 20, "not json"); err == nil {
		t.Fatal("garbage must fail")
	}
}

func TestAttributeStretchAnswers(t *testing.T) {
	ans := func(from, to int64, items ...AnswerItem) []Answer {
		a := Answer{FromTS: from, ToTS: to, Entries: map[string]AnswerItem{}}
		for _, it := range items {
			a.Entries[it.Key] = it
		}
		return []Answer{a}
	}
	toBeta := AnswerItem{Key: "app:W", Project: "beta", Subproject: "x"}
	leave := AnswerItem{Key: "app:W", Project: Unattributed}
	cases := []struct {
		name string
		in   []Sample
		now  int64
		as   []Answer
		want []Interval
	}{
		{
			"inside the window the key's entry books the span",
			[]Sample{work(0, "W", 0, ""), work(60, "W", 0, ""), work(120, "W", 0, "")}, 180,
			ans(0, 120, toBeta),
			[]Interval{{From: 0, To: 120, Project: "beta", Subproject: "x", RuleID: RuleStretch}, {From: 120, To: 180, Project: "beta", Subproject: "x", RuleID: RuleCarryover}},
		},
		{
			"a rule beats the answer",
			[]Sample{work(0, "A", 0, ""), work(60, "W", 0, "")}, 120,
			ans(0, 120, AnswerItem{Key: "app:A", Project: "beta"}, toBeta),
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "beta", Subproject: "x", RuleID: RuleStretch}},
		},
		{
			"the answer beats carry-over",
			[]Sample{work(0, "A", 0, ""), work(60, "W", 0, ""), work(120, "W", 0, "")}, 180,
			ans(60, 180, toBeta),
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 180, Project: "beta", Subproject: "x", RuleID: RuleStretch}},
		},
		{
			"leave books nothing and is not a carry-over source",
			[]Sample{work(0, "W", 0, ""), work(60, "W", 0, ""), work(120, "X", 0, "")}, 180,
			ans(0, 120, leave),
			[]Interval{{From: 0, To: 120, Project: Unattributed, RuleID: RuleStretchLeave}, {From: 120, To: 180, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"a key without an entry falls through to carry-over from the answered project",
			[]Sample{work(0, "A", 0, ""), work(60, "W", 0, ""), work(120, "X", 0, "")}, 180,
			ans(60, 180, toBeta),
			[]Interval{{From: 0, To: 60, Project: "alpha", RuleID: "0"}, {From: 60, To: 120, Project: "beta", Subproject: "x", RuleID: RuleStretch}, {From: 120, To: 180, Project: "beta", Subproject: "x", RuleID: RuleCarryover}},
		},
		{
			"an entry for a key not in the window changes nothing",
			[]Sample{work(0, "X", 0, "")}, 60,
			ans(0, 60, AnswerItem{Key: "host:gone.example", Project: "beta"}),
			[]Interval{{From: 0, To: 60, Project: Unattributed, RuleID: RuleUnattributed}},
		},
		{
			"a host key comes from the URL",
			[]Sample{{TS: 0, Kind: "sample", App: "com.google.Chrome", URL: "https://www.Grok.com/c/1"}}, 60,
			ans(0, 60, AnswerItem{Key: "host:grok.com", Project: "beta"}),
			[]Interval{{From: 0, To: 60, Project: "beta", RuleID: RuleStretch}},
		},
		{
			"in-call spans never take an answer",
			[]Sample{work(0, "W", 0, "us.zoom.xos")}, 60,
			ans(0, 60, toBeta),
			[]Interval{{From: 0, To: 60, Project: Unattributed, RuleID: RuleUnattributed}},
		},
	}
	for _, c := range cases {
		p := testParams
		p.Now = c.now
		p.Answers = c.as
		if got := Attribute(c.in, p); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}
