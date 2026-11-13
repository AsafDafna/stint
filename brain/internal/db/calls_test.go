package db

import (
	"reflect"
	"testing"

	"stint/internal/attribute"
)

func pending(from, to int64) Call {
	return Call{FromTS: from, ToTS: to, CallApp: "us.zoom.xos", GuessProject: "acme", GuessSubproject: "app", Breakdown: `[{"project":"meeting","seconds":600}]`}
}

func TestInsertCallIgnoresDuplicateFromTS(t *testing.T) {
	sq := testDB(t)
	if ok, err := InsertCall(sq, pending(100, 700)); err != nil || !ok {
		t.Fatalf("first insert: ok=%v err=%v", ok, err)
	}
	dup := pending(100, 999)
	if ok, err := InsertCall(sq, dup); err != nil || ok {
		t.Fatalf("duplicate insert: ok=%v err=%v", ok, err)
	}
	c, found, err := ReadCall(sq, 100)
	if err != nil || !found || c.ToTS != 700 || c.Decision != "" || c.DecidedAt != 0 || c.AppliedAt != 0 {
		t.Fatalf("row after duplicate: %+v found=%v err=%v", c, found, err)
	}
}

func TestRefreshPendingCallLeavesDecidedRows(t *testing.T) {
	sq := testDB(t)
	InsertCall(sq, pending(100, 700))
	InsertCall(sq, pending(2000, 2600))
	if _, err := SetDecision(sq, 2000, attribute.DecisionSplit, "", "", 50); err != nil {
		t.Fatal(err)
	}
	fresh := pending(100, 760)
	fresh.GuessProject = "phd"
	if ok, err := RefreshPendingCall(sq, fresh); err != nil || !ok {
		t.Fatalf("refresh pending: ok=%v err=%v", ok, err)
	}
	fresh2 := pending(2000, 2700)
	if ok, err := RefreshPendingCall(sq, fresh2); err != nil || ok {
		t.Fatalf("refresh decided must not update: ok=%v err=%v", ok, err)
	}
	c, _, _ := ReadCall(sq, 100)
	if c.ToTS != 760 || c.GuessProject != "phd" {
		t.Fatalf("pending row not refreshed: %+v", c)
	}
	c, _, _ = ReadCall(sq, 2000)
	if c.ToTS != 2600 {
		t.Fatalf("decided row was touched: %+v", c)
	}
}

func TestSetDecisionAndUnapplied(t *testing.T) {
	sq := testDB(t)
	InsertCall(sq, pending(100, 700))
	if found, err := SetDecision(sq, 999, attribute.DecisionSplit, "", "", 50); err != nil || found {
		t.Fatalf("unknown from_ts: found=%v err=%v", found, err)
	}
	if found, err := SetDecision(sq, 100, attribute.DecisionProject, "phd", "", 50); err != nil || !found {
		t.Fatalf("set: found=%v err=%v", found, err)
	}
	rows, err := UnappliedCalls(sq)
	if err != nil || len(rows) != 1 || rows[0].Decision != "project" || rows[0].Project != "phd" || rows[0].DecidedAt != 50 {
		t.Fatalf("unapplied: %+v err=%v", rows, err)
	}
	if err := MarkApplied(sq, 100, 50, 60); err != nil {
		t.Fatal(err)
	}
	if rows, _ := UnappliedCalls(sq); len(rows) != 0 {
		t.Fatalf("still unapplied: %+v", rows)
	}
	// Re-deciding makes it unapplied again.
	SetDecision(sq, 100, attribute.DecisionSplit, "", "", 70)
	if rows, _ := UnappliedCalls(sq); len(rows) != 1 || rows[0].Decision != "split" || rows[0].Project != "" {
		t.Fatalf("re-decided: %+v", rows)
	}
}

// A decision written after the brain read the row must not be marked applied.
func TestMarkAppliedSkipsRedecidedRow(t *testing.T) {
	sq := testDB(t)
	InsertCall(sq, pending(100, 700))
	SetDecision(sq, 100, attribute.DecisionProject, "phd", "", 50)
	SetDecision(sq, 100, attribute.DecisionSplit, "", "", 55) // between read (50) and mark
	if err := MarkApplied(sq, 100, 50, 60); err != nil {
		t.Fatal(err)
	}
	c, _, _ := ReadCall(sq, 100)
	if c.AppliedAt != 0 {
		t.Fatalf("applied_at set although decided_at moved: %+v", c)
	}
}

func TestDecisionsOverlapping(t *testing.T) {
	sq := testDB(t)
	InsertCall(sq, pending(100, 700))   // pending: never a decision
	InsertCall(sq, pending(1000, 1600)) // project
	InsertCall(sq, pending(5000, 5600)) // split: carries the guess
	SetDecision(sq, 1000, attribute.DecisionProject, "phd", "", 1)
	SetDecision(sq, 5000, attribute.DecisionSplit, "", "", 1)
	got, err := DecisionsOverlapping(sq, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []attribute.Decision{
		{FromTS: 1000, ToTS: 1600, Decision: "project", Project: "phd"},
		{FromTS: 5000, ToTS: 5600, Decision: "split", Project: "acme", Subproject: "app"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %+v\nwant %+v", got, want)
	}
	got, _ = DecisionsOverlapping(sq, 1600, 5000) // touching both ends: inclusive on to_ts, exclusive on from_ts >= toTS
	if len(got) != 1 || got[0].FromTS != 1000 {
		t.Fatalf("range: %+v", got)
	}
}

func TestReadCallsAndPurge(t *testing.T) {
	sq := testDB(t)
	InsertCall(sq, pending(100, 700))
	InsertCall(sq, pending(1000, 1600))
	rows, err := ReadCalls(sq, 1000)
	if err != nil || len(rows) != 1 || rows[0].FromTS != 1000 {
		t.Fatalf("since 1000: %+v err=%v", rows, err)
	}
	n, err := PurgeCalls(sq, 1000)
	if err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	if rows, _ := ReadCalls(sq, 0); len(rows) != 1 || rows[0].FromTS != 1000 {
		t.Fatalf("after purge: %+v", rows)
	}
}
