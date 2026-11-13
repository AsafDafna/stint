package db

import (
	"database/sql"

	"stint/internal/attribute"
)

// Call is one calls row. "" stands for a NULL text column, 0 for a
// NULL timestamp; Decision "" means pending.
type Call struct {
	FromTS, ToTS    int64
	CallApp         string
	GuessProject    string
	GuessSubproject string
	Breakdown       string // JSON array: attribute.Line entries, then attribute.BackgroundLine entries
	Decision        string // "", or attribute.DecisionProject, DecisionSplit or DecisionBackground
	Project         string
	Subproject      string
	DecidedAt       int64
	AppliedAt       int64
}

const callColumns = `from_ts, to_ts, call_app, guess_project, COALESCE(guess_subproject,''), breakdown, COALESCE(decision,''), COALESCE(project,''), COALESCE(subproject,''), COALESCE(decided_at,0), COALESCE(applied_at,0)`

// InsertCall adds a pending row; a row with the same from_ts is left untouched.
func InsertCall(sq *sql.DB, c Call) (bool, error) {
	res, err := sq.Exec(`INSERT OR IGNORE INTO calls(from_ts, to_ts, call_app, guess_project, guess_subproject, breakdown) VALUES (?, ?, ?, ?, ?, ?)`,
		c.FromTS, c.ToTS, c.CallApp, c.GuessProject, nullIfEmpty(c.GuessSubproject), c.Breakdown)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RefreshPendingCall rewrites a pending row's span, guess and breakdown; decided rows
// are never touched.
func RefreshPendingCall(sq *sql.DB, c Call) (bool, error) {
	res, err := sq.Exec(`UPDATE calls SET to_ts = ?, call_app = ?, guess_project = ?, guess_subproject = ?, breakdown = ? WHERE from_ts = ? AND decision IS NULL`,
		c.ToTS, c.CallApp, c.GuessProject, nullIfEmpty(c.GuessSubproject), c.Breakdown, c.FromTS)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReadCall returns the row with that from_ts.
func ReadCall(sq *sql.DB, fromTS int64) (Call, bool, error) {
	rows, err := scanCalls(sq.Query(`SELECT `+callColumns+` FROM calls WHERE from_ts = ?`, fromTS))
	if err != nil || len(rows) == 0 {
		return Call{}, false, err
	}
	return rows[0], true, nil
}

// ReadCalls returns rows with from_ts >= sinceTS, oldest first.
func ReadCalls(sq *sql.DB, sinceTS int64) ([]Call, error) {
	return scanCalls(sq.Query(`SELECT `+callColumns+` FROM calls WHERE from_ts >= ? ORDER BY from_ts`, sinceTS))
}

// UnappliedCalls returns decided rows the brain has not applied yet, oldest first.
func UnappliedCalls(sq *sql.DB) ([]Call, error) {
	return scanCalls(sq.Query(`SELECT ` + callColumns + ` FROM calls WHERE decision IS NOT NULL AND decided_at > COALESCE(applied_at, 0) ORDER BY from_ts`))
}

// DecisionsOverlapping returns the decided rows whose [from_ts, to_ts] meets
// [fromTS, toTS) (toTS <= 0: unbounded), as attribute decisions: a split carries the guess.
func DecisionsOverlapping(sq *sql.DB, fromTS, toTS int64) ([]attribute.Decision, error) {
	if toTS <= 0 {
		toTS = 1<<62 - 1
	}
	rows, err := scanCalls(sq.Query(`SELECT `+callColumns+` FROM calls WHERE decision IS NOT NULL AND to_ts >= ? AND from_ts < ? ORDER BY from_ts`, fromTS, toTS))
	if err != nil {
		return nil, err
	}
	out := make([]attribute.Decision, 0, len(rows))
	for _, c := range rows {
		d := attribute.Decision{FromTS: c.FromTS, ToTS: c.ToTS, Decision: c.Decision, Project: c.Project, Subproject: c.Subproject}
		if c.Decision == attribute.DecisionSplit {
			d.Project, d.Subproject = c.GuessProject, c.GuessSubproject
		}
		out = append(out, d)
	}
	return out, nil
}

// SetDecision records an answer (from the pop-up or `stint calls set`); found is false
// when no row has that from_ts. project/subproject are stored only for DecisionProject.
func SetDecision(sq *sql.DB, fromTS int64, decision, project, subproject string, now int64) (bool, error) {
	if decision != attribute.DecisionProject {
		project, subproject = "", ""
	}
	res, err := sq.Exec(`UPDATE calls SET decision = ?, project = ?, subproject = ?, decided_at = ? WHERE from_ts = ?`,
		decision, nullIfEmpty(project), nullIfEmpty(subproject), now, fromTS)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkApplied stamps applied_at only while decided_at still equals the value the caller
// read, so an answer written during the run stays unapplied.
func MarkApplied(sq *sql.DB, fromTS, decidedAt, now int64) error {
	_, err := sq.Exec(`UPDATE calls SET applied_at = ? WHERE from_ts = ? AND decided_at = ?`, now, fromTS, decidedAt)
	return err
}

// PurgeCalls deletes the calls rows whose call ended before cutoff. It deletes only calls
// rows; the samples are purged by ingest.Purge.
func PurgeCalls(sq *sql.DB, cutoff int64) (int64, error) {
	res, err := sq.Exec(`DELETE FROM calls WHERE to_ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanCalls(rows *sql.Rows, err error) ([]Call, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Call
	for rows.Next() {
		var c Call
		if err := rows.Scan(&c.FromTS, &c.ToTS, &c.CallApp, &c.GuessProject, &c.GuessSubproject, &c.Breakdown, &c.Decision, &c.Project, &c.Subproject, &c.DecidedAt, &c.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
