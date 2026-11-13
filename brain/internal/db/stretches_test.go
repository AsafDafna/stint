package db

import (
	"testing"

	"stint/internal/attribute"
)

func unanswered(from, to int64) Stretch {
	return Stretch{FromTS: from, ToTS: to, UnattributedS: to - from, Breakdown: `[{"key":"app:W","seconds":600}]`}
}

const answerW = `[{"key":"app:W","project":"phd"}]`

func TestInsertStretchSkipsOverlap(t *testing.T) {
	sq := testDB(t)
	for _, c := range []struct {
		from, to int64
		want     bool
	}{
		{1000, 2000, true},
		{1500, 2500, false}, // a recompute shifted the same stretch
		{500, 1001, false},
		{1000, 1600, false}, // same from_ts
		{2000, 2600, true},  // touching is not overlapping
		{400, 1000, true},
	} {
		if ok, err := InsertStretch(sq, unanswered(c.from, c.to)); err != nil || ok != c.want {
			t.Fatalf("[%d,%d): inserted=%v err=%v, want %v", c.from, c.to, ok, err, c.want)
		}
	}
	rows, _ := ReadStretches(sq, 0)
	if len(rows) != 3 || rows[0].FromTS != 400 || rows[1].FromTS != 1000 || rows[2].FromTS != 2000 {
		t.Fatalf("rows: %+v", rows)
	}
	if s := rows[1]; s.ToTS != 2000 || s.UnattributedS != 1000 || s.Answer != "" || s.DecidedAt != 0 || s.AppliedAt != 0 || s.RulesError != "" {
		t.Fatalf("new row: %+v", s)
	}
}

func TestRefreshAndDeleteLeaveAnsweredRows(t *testing.T) {
	sq := testDB(t)
	InsertStretch(sq, unanswered(1000, 2000))
	InsertStretch(sq, unanswered(3000, 4000))
	InsertStretch(sq, unanswered(5000, 6000))
	SetStretchAnswer(sq, 3000, answerW, 10)
	SetStretchAnswer(sq, 5000, answerW, 10)
	ReopenStretch(sq, 5000, 20)
	if ok, err := RefreshStretch(sq, 1000, 300, "[]"); err != nil || !ok {
		t.Fatalf("refresh unanswered: ok=%v err=%v", ok, err)
	}
	if ok, _ := RefreshStretch(sq, 3000, 300, "[]"); ok {
		t.Fatal("an answered row must not be refreshed")
	}
	if ok, _ := RefreshStretch(sq, 5000, 300, "[]"); !ok {
		t.Fatal("a re-opened row is refreshed")
	}
	if ok, _ := DeleteUndecidedStretch(sq, 3000); ok {
		t.Fatal("an answered row must not be deleted")
	}
	if ok, _ := DeleteUndecidedStretch(sq, 5000); ok {
		t.Fatal("a re-opened row must not be deleted")
	}
	if ok, _ := DeleteUndecidedStretch(sq, 1000); !ok {
		t.Fatal("a never-decided row is deleted")
	}
	un, _ := UnansweredStretches(sq, 0)
	if len(un) != 1 || un[0].FromTS != 5000 || un[0].UnattributedS != 300 {
		t.Fatalf("unanswered: %+v", un)
	}
	if un, _ := UnansweredStretches(sq, 5001); len(un) != 0 {
		t.Fatalf("sinceTS filter: %+v", un)
	}
}

// A decision written between the brain's read and its applied_at mark stays unapplied.
func TestMarkStretchAppliedSkipsRedecidedRow(t *testing.T) {
	sq := testDB(t)
	InsertStretch(sq, unanswered(1000, 2000))
	SetStretchAnswer(sq, 1000, answerW, 10)
	rows, _ := UnappliedStretches(sq)
	if len(rows) != 1 || rows[0].Answer != answerW || rows[0].DecidedAt != 10 {
		t.Fatalf("unapplied: %+v", rows)
	}
	ReopenStretch(sq, 1000, 15) // meanwhile
	if err := MarkStretchApplied(sq, 1000, 10, 20); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := ReadStretch(sq, 1000); s.AppliedAt != 0 {
		t.Fatalf("stale mark applied: %+v", s)
	}
	MarkStretchApplied(sq, 1000, 15, 20)
	if rows, _ := UnappliedStretches(sq); len(rows) != 0 {
		t.Fatalf("still unapplied: %+v", rows)
	}
}

func TestAnsweredStretchesOverlapping(t *testing.T) {
	sq := testDB(t)
	InsertStretch(sq, unanswered(1000, 2000))
	InsertStretch(sq, unanswered(3000, 4000))
	InsertStretch(sq, unanswered(5000, 6000))
	SetStretchAnswer(sq, 1000, answerW, 10)
	SetStretchAnswer(sq, 3000, answerW, 10)
	MarkStretchApplied(sq, 3000, 10, 11)
	got, err := AnsweredStretchesOverlapping(sq, 1500, 0)
	if err != nil || len(got) != 2 || got[0].FromTS != 1000 || got[0].AppliedAt != 0 || got[1].FromTS != 3000 || got[1].AppliedAt != 11 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if got, _ := AnsweredStretchesOverlapping(sq, 2000, 3000); len(got) != 0 {
		t.Fatalf("half-open windows touching the range must not count: %+v", got)
	}
}

func TestReopenStretch(t *testing.T) {
	sq := testDB(t)
	InsertStretch(sq, unanswered(1000, 2000))
	if ok, err := ReopenStretch(sq, 1000, 50); err != nil || ok {
		t.Fatalf("reopen without an answer: ok=%v err=%v", ok, err)
	}
	SetStretchAnswer(sq, 1000, answerW, 10)
	SetStretchRulesError(sq, 1000, "boom")
	if ok, err := ReopenStretch(sq, 1000, 50); err != nil || !ok {
		t.Fatalf("reopen: ok=%v err=%v", ok, err)
	}
	s, _, _ := ReadStretch(sq, 1000)
	if s.Answer != "" || s.DecidedAt != 50 || s.RulesError != "" {
		t.Fatalf("after reopen: %+v", s)
	}
	if ok, _ := SetStretchAnswer(sq, 1000, answerW, 60); !ok {
		t.Fatal("a re-opened row takes a new answer")
	}
	if ok, _ := SetStretchAnswer(sq, 1000, `[]`, 70); ok {
		t.Fatal("an answered row must not be overwritten")
	}
}

func TestReadPendingCountsOnlyPendingRows(t *testing.T) {
	sq := testDB(t)
	InsertCall(sq, Call{FromTS: 1, ToTS: 400, CallApp: "x", GuessProject: "p", Breakdown: "[]"})
	InsertCall(sq, Call{FromTS: 500, ToTS: 900, CallApp: "x", GuessProject: "p", Breakdown: "[]"})
	SetDecision(sq, 500, attribute.DecisionSplit, "", "", 10)
	for _, s := range []Stretch{
		{FromTS: 1000, ToTS: 2000, UnattributedS: 600, Breakdown: "[]"}, // never decided: pending
		{FromTS: 3000, ToTS: 4000, UnattributedS: 700, Breakdown: "[]"}, // answered: not pending
		{FromTS: 5000, ToTS: 6000, UnattributedS: 800, Breakdown: "[]"}, // re-opened, not yet recomputed: not pending
		{FromTS: 7000, ToTS: 8000, UnattributedS: 900, Breakdown: "[]"}, // re-opened and recomputed: pending
	} {
		InsertStretch(sq, s)
	}
	for _, from := range []int64{3000, 5000, 7000} {
		SetStretchAnswer(sq, from, answerW, 10)
	}
	ReopenStretch(sq, 5000, 20)
	ReopenStretch(sq, 7000, 20)
	MarkStretchApplied(sq, 7000, 20, 21)
	got, err := ReadPending(sq)
	if err != nil || got != (Pending{Calls: 1, Stretches: 2, StretchSeconds: 1500}) {
		t.Fatalf("pending = %+v err=%v", got, err)
	}
}

func TestStretchErrorsAndPurge(t *testing.T) {
	sq := testDB(t)
	if n, first, err := StretchErrors(sq); err != nil || n != 0 || first != "" {
		t.Fatalf("none: %d %q %v", n, first, err)
	}
	InsertStretch(sq, unanswered(1000, 2000))
	InsertStretch(sq, unanswered(3000, 4000))
	SetStretchRulesError(sq, 3000, "later")
	SetStretchRulesError(sq, 1000, "first")
	if n, first, _ := StretchErrors(sq); n != 2 || first != "first" {
		t.Fatalf("errors: %d %q", n, first)
	}
	SetStretchRulesError(sq, 1000, "")
	if n, first, _ := StretchErrors(sq); n != 1 || first != "later" {
		t.Fatalf("after clearing: %d %q", n, first)
	}
	if n, err := PurgeStretches(sq, 3000); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if rows, _ := ReadStretches(sq, 0); len(rows) != 1 || rows[0].FromTS != 3000 {
		t.Fatalf("after purge: %+v", rows)
	}
}

func TestAppsWithURLOrCWDAndRuleSeconds(t *testing.T) {
	sq := testDB(t)
	for _, s := range []attribute.Sample{
		{TS: 10, Kind: "heartbeat", App: "com.google.Chrome", URL: "https://x"},
		{TS: 20, Kind: "heartbeat", App: "com.mitchellh.ghostty", CWD: "/x"},
		{TS: 30, Kind: "heartbeat", App: "net.whatsapp.WhatsApp", Title: "WhatsApp"},
		{TS: 5, Kind: "heartbeat", App: "com.old.Browser", URL: "https://y"},
	} {
		InsertSample(sq, s)
	}
	got, err := AppsWithURLOrCWD(sq, 10)
	if err != nil || len(got) != 2 || !got["com.google.Chrome"] || !got["com.mitchellh.ghostty"] {
		t.Fatalf("apps = %v err=%v", got, err)
	}
	tx, _ := sq.Begin()
	InsertIntervals(tx, []attribute.Interval{
		{From: 0, To: 100, Project: "phd", RuleID: "2"},
		{From: 200, To: 300, Project: "phd", RuleID: "2"},
		{From: 300, To: 400, Project: "phd", RuleID: "3"},
	})
	tx.Commit()
	if n, err := RuleSeconds(sq, "2", 50, 250); err != nil || n != 100 {
		t.Fatalf("RuleSeconds = %d err=%v, want 50+50", n, err)
	}
}
