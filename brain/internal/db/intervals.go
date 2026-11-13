package db

import (
	"database/sql"
	"errors"

	"stint/internal/attribute"
)

// InsertIntervals writes rows inside tx. "" subproject becomes NULL; rule_id is always set.
func InsertIntervals(tx *sql.Tx, ivs []attribute.Interval) error {
	stmt, err := tx.Prepare(`INSERT INTO intervals(from_ts, to_ts, project, subproject, rule_id) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, iv := range ivs {
		if _, err := stmt.Exec(iv.From, iv.To, iv.Project, nullIfEmpty(iv.Subproject), iv.RuleID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteIntervals removes rows with to_ts > fromTS and from_ts < toTS (toTS <= 0: unbounded).
func DeleteIntervals(tx *sql.Tx, fromTS, toTS int64) error {
	if toTS <= 0 {
		toTS = 1<<62 - 1
	}
	_, err := tx.Exec(`DELETE FROM intervals WHERE to_ts > ? AND from_ts < ?`, fromTS, toTS)
	return err
}

// ReadIntervals returns rows overlapping [fromTS, toTS), ordered by from_ts.
func ReadIntervals(sq *sql.DB, fromTS, toTS int64) ([]attribute.Interval, error) {
	rows, err := sq.Query(`SELECT from_ts, to_ts, project, COALESCE(subproject,''), COALESCE(rule_id,'') FROM intervals WHERE to_ts > ? AND from_ts < ? ORDER BY from_ts, rowid`, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attribute.Interval
	for rows.Next() {
		var iv attribute.Interval
		if err := rows.Scan(&iv.From, &iv.To, &iv.Project, &iv.Subproject, &iv.RuleID); err != nil {
			return nil, err
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// IntervalAt returns the interval covering t (from_ts <= t < to_ts).
func IntervalAt(sq *sql.DB, t int64) (attribute.Interval, bool, error) {
	var iv attribute.Interval
	err := sq.QueryRow(`SELECT from_ts, to_ts, project, COALESCE(subproject,''), COALESCE(rule_id,'') FROM intervals WHERE from_ts <= ? AND to_ts > ? ORDER BY from_ts DESC LIMIT 1`, t, t).
		Scan(&iv.From, &iv.To, &iv.Project, &iv.Subproject, &iv.RuleID)
	if errors.Is(err, sql.ErrNoRows) {
		return attribute.Interval{}, false, nil
	}
	return iv, err == nil, err
}

// OldestIntervalTS is the smallest from_ts; ok is false when there are no intervals.
func OldestIntervalTS(sq *sql.DB) (ts int64, ok bool, err error) {
	var v sql.NullInt64
	if err := sq.QueryRow(`SELECT MIN(from_ts) FROM intervals`).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

// RuleSeconds sums the intervals labelled ruleID, clipped to [fromTS, toTS).
func RuleSeconds(sq *sql.DB, ruleID string, fromTS, toTS int64) (int64, error) {
	var n int64
	err := sq.QueryRow(`SELECT COALESCE(SUM(MIN(to_ts, ?) - MAX(from_ts, ?)), 0) FROM intervals WHERE rule_id = ? AND to_ts > ? AND from_ts < ?`,
		toTS, fromTS, ruleID, fromTS, toTS).Scan(&n)
	return n, err
}
