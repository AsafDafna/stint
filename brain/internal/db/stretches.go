package db

import "database/sql"

// Stretch is one stretches row. "" stands for a NULL text column, 0 for a
// NULL timestamp; Answer "" means unanswered (never answered, or re-opened).
type Stretch struct {
	FromTS, ToTS  int64
	UnattributedS int64
	Breakdown     string // JSON, stretches.KeySeconds array
	Answer        string // JSON, attribute.AnswerItem array
	DecidedAt     int64
	AppliedAt     int64
	RulesError    string
}

const stretchColumns = `from_ts, to_ts, unattributed_s, breakdown, COALESCE(answer,''), COALESCE(decided_at,0), COALESCE(applied_at,0), COALESCE(rules_error,'')`

// pendingStretch is the pending predicate (menu, auto-open, morning count): unanswered,
// and either never decided or re-opened and already recomputed by the brain.
const pendingStretch = `answer IS NULL AND (decided_at IS NULL OR applied_at >= decided_at)`

// InsertStretch adds an unanswered row unless its window overlaps an existing row's: a
// stretch's bounds move with every recompute that changes attribution, so the primary key
// alone would not catch a repeat.
func InsertStretch(sq *sql.DB, s Stretch) (bool, error) {
	res, err := sq.Exec(`INSERT INTO stretches(from_ts, to_ts, unattributed_s, breakdown)
		SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM stretches WHERE from_ts < ? AND to_ts > ?)`,
		s.FromTS, s.ToTS, s.UnattributedS, s.Breakdown, s.ToTS, s.FromTS)
	return changed(res, err)
}

// ReadStretch returns the row with that from_ts.
func ReadStretch(sq *sql.DB, fromTS int64) (Stretch, bool, error) {
	rows, err := scanStretches(sq.Query(`SELECT `+stretchColumns+` FROM stretches WHERE from_ts = ?`, fromTS))
	if err != nil || len(rows) == 0 {
		return Stretch{}, false, err
	}
	return rows[0], true, nil
}

// ReadStretches returns rows with from_ts >= sinceTS, oldest first.
func ReadStretches(sq *sql.DB, sinceTS int64) ([]Stretch, error) {
	return scanStretches(sq.Query(`SELECT `+stretchColumns+` FROM stretches WHERE from_ts >= ? ORDER BY from_ts`, sinceTS))
}

// UnansweredStretches returns the rows with no answer (never decided and re-opened alike) and
// from_ts >= sinceTS, oldest first: the rows a rebuild from sinceTS refreshes.
func UnansweredStretches(sq *sql.DB, sinceTS int64) ([]Stretch, error) {
	return scanStretches(sq.Query(`SELECT `+stretchColumns+` FROM stretches WHERE answer IS NULL AND from_ts >= ? ORDER BY from_ts`, sinceTS))
}

// RefreshStretch rewrites an unanswered row's unattributed_s and breakdown; answered rows are
// never touched.
func RefreshStretch(sq *sql.DB, fromTS, unattributedS int64, breakdown string) (bool, error) {
	return changed(sq.Exec(`UPDATE stretches SET unattributed_s = ?, breakdown = ? WHERE from_ts = ? AND answer IS NULL`, unattributedS, breakdown, fromTS))
}

// DeleteUndecidedStretch removes a row nobody ever decided; answered or re-opened rows stay.
func DeleteUndecidedStretch(sq *sql.DB, fromTS int64) (bool, error) {
	return changed(sq.Exec(`DELETE FROM stretches WHERE from_ts = ? AND answer IS NULL AND decided_at IS NULL`, fromTS))
}

// UnappliedStretches returns the rows answered or re-opened since the brain last applied them,
// oldest first.
func UnappliedStretches(sq *sql.DB) ([]Stretch, error) {
	return scanStretches(sq.Query(`SELECT ` + stretchColumns + ` FROM stretches WHERE decided_at > COALESCE(applied_at, 0) ORDER BY from_ts`))
}

// AnsweredStretchesOverlapping returns the rows with an answer, applied or not, whose
// [from_ts, to_ts) meets [fromTS, toTS) (toTS <= 0: unbounded), oldest first. The rebuild
// keeps only the applied ones and the ones being applied (ingest).
func AnsweredStretchesOverlapping(sq *sql.DB, fromTS, toTS int64) ([]Stretch, error) {
	if toTS <= 0 {
		toTS = 1<<62 - 1
	}
	return scanStretches(sq.Query(`SELECT `+stretchColumns+` FROM stretches WHERE answer IS NOT NULL AND to_ts > ? AND from_ts < ? ORDER BY from_ts`, fromTS, toTS))
}

// SetStretchAnswer records an answer exactly as the collector's panel does (only while the
// row has none). Tests and fixtures only; the collector writes production answers.
func SetStretchAnswer(sq *sql.DB, fromTS int64, answer string, now int64) (bool, error) {
	return changed(sq.Exec(`UPDATE stretches SET answer = ?, decided_at = ? WHERE from_ts = ? AND answer IS NULL`, answer, now, fromTS))
}

// ReopenStretch clears an answer and its rules_error and stamps decided_at, so the next run
// recomputes the window without the answer and the panel asks again. It changes
// nothing on a row without an answer.
func ReopenStretch(sq *sql.DB, fromTS, now int64) (bool, error) {
	return changed(sq.Exec(`UPDATE stretches SET answer = NULL, decided_at = ?, rules_error = NULL WHERE from_ts = ? AND answer IS NOT NULL`, now, fromTS))
}

// SetStretchRulesError records why a row's apply failed; "" clears it.
func SetStretchRulesError(sq *sql.DB, fromTS int64, msg string) error {
	_, err := sq.Exec(`UPDATE stretches SET rules_error = ? WHERE from_ts = ?`, nullIfEmpty(msg), fromTS)
	return err
}

// MarkStretchApplied stamps applied_at only while decided_at still equals the value the caller
// read, so an answer or reopen written during the run stays unapplied.
func MarkStretchApplied(sq *sql.DB, fromTS, decidedAt, now int64) error {
	_, err := sq.Exec(`UPDATE stretches SET applied_at = ? WHERE from_ts = ? AND decided_at = ?`, now, fromTS, decidedAt)
	return err
}

// Pending is what the morning push reminds about.
type Pending struct {
	Calls          int   // calls rows with no decision
	Stretches      int   // stretches rows pending as the menu sees them
	StretchSeconds int64 // their summed unattributed_s
}

// ReadPending counts the pending calls and stretches.
func ReadPending(sq *sql.DB) (Pending, error) {
	var p Pending
	err := sq.QueryRow(`SELECT (SELECT COUNT(*) FROM calls WHERE decision IS NULL),
		(SELECT COUNT(*) FROM stretches WHERE `+pendingStretch+`),
		(SELECT COALESCE(SUM(unattributed_s), 0) FROM stretches WHERE `+pendingStretch+`)`).Scan(&p.Calls, &p.Stretches, &p.StretchSeconds)
	return p, err
}

// StretchErrors counts the rows with a rules_error and returns the oldest row's text.
func StretchErrors(sq *sql.DB) (n int, first string, err error) {
	err = sq.QueryRow(`WITH e AS (SELECT from_ts, rules_error FROM stretches WHERE rules_error IS NOT NULL)
		SELECT COUNT(*), COALESCE((SELECT rules_error FROM e ORDER BY from_ts LIMIT 1), '') FROM e`).Scan(&n, &first)
	return n, first, err
}

// PurgeStretches deletes the rows whose window ended before cutoff (their samples are gone;
// the answered intervals stay).
func PurgeStretches(sq *sql.DB, cutoff int64) (int64, error) {
	res, err := sq.Exec(`DELETE FROM stretches WHERE to_ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func changed(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func scanStretches(rows *sql.Rows, err error) ([]Stretch, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stretch
	for rows.Next() {
		var s Stretch
		if err := rows.Scan(&s.FromTS, &s.ToTS, &s.UnattributedS, &s.Breakdown, &s.Answer, &s.DecidedAt, &s.AppliedAt, &s.RulesError); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
