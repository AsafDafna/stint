package db

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stint/internal/attribute"
)

// testDB creates the fixture under a directory with a space, like the real
// "~/Library/Application Support/stint/stint.db".
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	sq, err := Create(filepath.Join(t.TempDir(), "Application Support", "stint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	return sq
}

func TestOpenRefusesMissingFile(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		t.Fatal("Open must not create a database")
	}
}

func TestOpenPathWithSpaceAndPercent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Application Support 100%")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stint.db")
	sq, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	sq.Close()
	sq, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	if err := CheckSchema(sq); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("database was created somewhere else:", err)
	}
}

// Open must honour both DSN parameters: _busy_timeout (5 s) and mode=rw. The mode is
// asserted by opening the same file with mode=ro through dsn() and watching a write fail.
func TestOpenDSNParameters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stint.db")
	sq, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	sq.Close()

	sq, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	var timeout int
	if err := sq.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("busy_timeout = %d %v, want 5000", timeout, err)
	}
	if err := SetMeta(sq, "k", "v"); err != nil {
		t.Fatalf("mode=rw must allow writes: %v", err)
	}

	ro, err := sql.Open("sqlite3", dsn(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := SetMeta(ro, "k", "w"); err == nil {
		t.Fatal("mode=ro must refuse writes (the DSN mode parameter is not reaching SQLite)")
	}
	if v, ok, err := GetMeta(sq, "k"); err != nil || !ok || v != "v" {
		t.Fatalf("read-only handle must not have written: %q %v %v", v, ok, err)
	}
}

func TestSchemaVersionCheck(t *testing.T) {
	sq := testDB(t)
	if err := CheckSchema(sq); err != nil {
		t.Fatalf("fresh fixture should pass: %v", err)
	}
	if err := SetMeta(sq, "schema_version", "5"); err != nil {
		t.Fatal(err)
	}
	err := CheckSchema(sq)
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema, got %v", err)
	}
}

// A meta read failure (not a version mismatch) must surface as a plain error, never
// as ErrSchema: the cmd routes ErrSchema to the "schema version mismatch" health reason.
func TestCheckSchemaReadFailureIsNotErrSchema(t *testing.T) {
	t.Run("missing meta table", func(t *testing.T) {
		sq := testDB(t)
		if _, err := sq.Exec(`DROP TABLE meta`); err != nil {
			t.Fatal(err)
		}
		err := CheckSchema(sq)
		if err == nil || errors.Is(err, ErrSchema) {
			t.Fatalf("want a non-ErrSchema error, got %v", err)
		}
	})
	t.Run("closed database", func(t *testing.T) {
		sq := testDB(t)
		sq.Close()
		err := CheckSchema(sq)
		if err == nil || errors.Is(err, ErrSchema) {
			t.Fatalf("want a non-ErrSchema error, got %v", err)
		}
	})
}

func TestMetaRoundTrip(t *testing.T) {
	sq := testDB(t)
	if _, ok, err := GetMeta(sq, "ingest_rowid"); err != nil || ok {
		t.Fatalf("absent key: ok=%v err=%v", ok, err)
	}
	for _, v := range []string{"10", "11"} {
		if err := SetMeta(sq, "ingest_rowid", v); err != nil {
			t.Fatal(err)
		}
		got, ok, err := GetMeta(sq, "ingest_rowid")
		if err != nil || !ok || got != v {
			t.Fatalf("got %q ok=%v err=%v want %q", got, ok, err, v)
		}
	}
}

func TestSetMetaTx(t *testing.T) {
	sq := testDB(t)
	if err := SetMeta(sq, "k", "old"); err != nil {
		t.Fatal(err)
	}
	tx, err := sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMetaTx(tx, "k", "rolled back"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetMeta(sq, "k"); err != nil || !ok || v != "old" {
		t.Fatalf("after rollback: %q %v %v, want \"old\"", v, ok, err)
	}
	tx, err = sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMetaTx(tx, "k", "new"); err != nil { // upsert over an existing key
		t.Fatal(err)
	}
	if err := SetMetaTx(tx, "k2", "v2"); err != nil { // insert of an absent key
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetMeta(sq, "k"); err != nil || !ok || v != "new" {
		t.Fatalf("after commit: %q %v %v, want \"new\"", v, ok, err)
	}
	if v, ok, err := GetMeta(sq, "k2"); err != nil || !ok || v != "v2" {
		t.Fatalf("after commit: %q %v %v, want \"v2\"", v, ok, err)
	}
}

func TestDeleteMeta(t *testing.T) {
	sq := testDB(t)
	if err := DeleteMeta(sq, "absent"); err != nil {
		t.Fatalf("deleting an absent key must be a no-op: %v", err)
	}
	if err := SetMeta(sq, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteMeta(sq, "k"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetMeta(sq, "k"); err != nil || ok {
		t.Fatalf("after delete: %q %v %v", v, ok, err)
	}
	if err := CheckSchema(sq); err != nil {
		t.Fatalf("DeleteMeta must only touch its key: %v", err)
	}
}

func TestSamplesRoundTripAndNulls(t *testing.T) {
	sq := testDB(t)
	in := []attribute.Sample{
		{TS: 100, Kind: "sample", App: "A", Title: "t", URL: "u", CWD: "/c", IdleS: 3, CallApp: "z"},
		{TS: 160, Kind: "heartbeat", App: "A"},
		{TS: 200, Kind: "sleep"},
	}
	for _, s := range in {
		if _, err := InsertSample(sq, s); err != nil {
			t.Fatal(err)
		}
	}
	var nulls int
	if err := sq.QueryRow(`SELECT COUNT(*) FROM samples WHERE title IS NULL`).Scan(&nulls); err != nil || nulls != 2 {
		t.Fatalf("empty strings must be stored as NULL: nulls=%d err=%v", nulls, err)
	}
	got, err := ReadSamples(sq, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		in[i].ID = int64(i + 1)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v\nwant %+v", got, in)
	}
	if got, err := ReadSamples(sq, 2, 2); err != nil || len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("bounded read: %+v %v", got, err)
	}
	if got, err := ReadSamplesBetween(sq, 100, 200); err != nil || len(got) != 2 {
		t.Fatalf("between read: %+v %v", got, err)
	}
	if s, ok, err := LastSampleBefore(sq, 199); err != nil || !ok || s.ID != 2 {
		t.Fatalf("LastSampleBefore: %+v %v %v", s, ok, err)
	}
	if s, ok, err := LastSampleBefore(sq, 160); err != nil || !ok || s.ID != 2 {
		t.Fatalf("LastSampleBefore is inclusive at ts == t: %+v %v %v", s, ok, err)
	}
	if _, ok, err := LastSampleBefore(sq, 99); err != nil || ok {
		t.Fatalf("LastSampleBefore before the oldest row must report ok=false: %v %v", ok, err)
	}
	if ts, ok, err := OldestSampleTS(sq); err != nil || !ok || ts != 100 {
		t.Fatalf("oldest = %d %v %v", ts, ok, err)
	}
	if ts, ok, err := LatestSampleTS(sq); err != nil || !ok || ts != 200 {
		t.Fatalf("latest = %d %v %v", ts, ok, err)
	}
	if k, ok, err := LastBoundaryKind(sq); err != nil || !ok || k != "sleep" {
		t.Fatalf("last boundary = %q %v %v", k, ok, err)
	}
	if k, ok, err := LastKind(sq); err != nil || !ok || k != "sleep" {
		t.Fatalf("last kind = %q %v %v", k, ok, err)
	}
	if ts, ok, err := TSOfID(sq, 3); err != nil || !ok || ts != 200 {
		t.Fatalf("TSOfID = %d %v %v", ts, ok, err)
	}
	if _, ok, err := TSOfID(sq, 99); err != nil || ok {
		t.Fatalf("missing id must report ok=false: %v %v", ok, err)
	}
}

// Every reader must report "nothing" (ok=false / empty slice) with a nil error on empty tables.
func TestEmptyTables(t *testing.T) {
	sq := testDB(t)
	if ts, ok, err := OldestSampleTS(sq); err != nil || ok || ts != 0 {
		t.Fatalf("OldestSampleTS = %d %v %v", ts, ok, err)
	}
	if ts, ok, err := LatestSampleTS(sq); err != nil || ok || ts != 0 {
		t.Fatalf("LatestSampleTS = %d %v %v", ts, ok, err)
	}
	if ts, ok, err := OldestIntervalTS(sq); err != nil || ok || ts != 0 {
		t.Fatalf("OldestIntervalTS = %d %v %v", ts, ok, err)
	}
	if k, ok, err := LastKind(sq); err != nil || ok || k != "" {
		t.Fatalf("LastKind = %q %v %v", k, ok, err)
	}
	if k, ok, err := LastBoundaryKind(sq); err != nil || ok || k != "" {
		t.Fatalf("LastBoundaryKind = %q %v %v", k, ok, err)
	}
	if ts, ok, err := TSOfID(sq, 1); err != nil || ok || ts != 0 {
		t.Fatalf("TSOfID = %d %v %v", ts, ok, err)
	}
	if s, ok, err := LastSampleBefore(sq, 1<<40); err != nil || ok || s.ID != 0 {
		t.Fatalf("LastSampleBefore = %+v %v %v", s, ok, err)
	}
	if iv, ok, err := IntervalAt(sq, 0); err != nil || ok || iv.Project != "" {
		t.Fatalf("IntervalAt = %+v %v %v", iv, ok, err)
	}
	if got, err := ReadSamples(sq, 0, 0); err != nil || len(got) != 0 {
		t.Fatalf("ReadSamples = %+v %v", got, err)
	}
	if got, err := ReadSamplesBetween(sq, 0, 1<<40); err != nil || len(got) != 0 {
		t.Fatalf("ReadSamplesBetween = %+v %v", got, err)
	}
	if got, err := ReadIntervals(sq, 0, 1<<40); err != nil || len(got) != 0 {
		t.Fatalf("ReadIntervals = %+v %v", got, err)
	}
}

func TestIntervalsRoundTripAndNulls(t *testing.T) {
	sq := testDB(t)
	in := []attribute.Interval{
		{From: 0, To: 100, Project: "acme", Subproject: "automations", RuleID: "0"},
		{From: 100, To: 200, Project: attribute.Unattributed, Subproject: "", RuleID: attribute.RuleUnattributed},
		{From: 300, To: 400, Project: "phd", RuleID: "3"},
		{From: 400, To: 500, Project: "phd", RuleID: ""}, // exercises the rule_id NULL assertion below
	}
	tx, err := sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := InsertIntervals(tx, in); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var nulls int
	if err := sq.QueryRow(`SELECT COUNT(*) FROM intervals WHERE subproject IS NULL`).Scan(&nulls); err != nil || nulls != 3 {
		t.Fatalf("NULL mapping: %d %v", nulls, err)
	}
	if err := sq.QueryRow(`SELECT COUNT(*) FROM intervals WHERE rule_id IS NULL`).Scan(&nulls); err != nil || nulls != 0 {
		t.Fatalf("rule_id must never be NULL (an empty RuleID is stored as ''): %d %v", nulls, err)
	}
	got, err := ReadIntervals(sq, 0, 1000)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if got, err := ReadIntervals(sq, 150, 350); err != nil || len(got) != 2 {
		t.Fatalf("overlap read: %+v %v", got, err)
	}
	if iv, ok, err := IntervalAt(sq, 150); err != nil || !ok || iv.Project != attribute.Unattributed {
		t.Fatalf("IntervalAt(150) = %+v %v %v", iv, ok, err)
	}
	if _, ok, err := IntervalAt(sq, 250); err != nil || ok {
		t.Fatalf("IntervalAt(250) must be empty: %v %v", ok, err)
	}
	if ts, ok, err := OldestIntervalTS(sq); err != nil || !ok || ts != 0 {
		t.Fatalf("oldest interval = %d %v %v", ts, ok, err)
	}
	tx, err = sq.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteIntervals(tx, 100, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadIntervals(sq, 0, 1000); err != nil || len(got) != 1 || got[0].To != 100 {
		t.Fatalf("after delete: %+v %v", got, err)
	}
}

// A database the collector has not restarted on yet still says "3"; the brain must
// refuse it (the run command turns this into the health push) rather than fail later on
// a decision value the old schema rejects.
func TestCheckSchemaRefusesVersion3(t *testing.T) {
	sq := testDB(t)
	if err := SetMeta(sq, "schema_version", "3"); err != nil {
		t.Fatal(err)
	}
	err := CheckSchema(sq)
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema, got %v", err)
	}
	if err := SetMeta(sq, "schema_version", "4"); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(sq); err != nil {
		t.Fatal(err)
	}
}
