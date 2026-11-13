package db

import (
	"database/sql"
	"errors"

	"stint/internal/attribute"
)

const sampleColumns = `id, ts, kind, COALESCE(app,''), COALESCE(title,''), COALESCE(url,''), COALESCE(cwd,''), COALESCE(idle_s,0), COALESCE(call_app,'')`

// ReadSamples returns rows with fromID <= id <= toID, ordered by id. toID <= 0 means no upper bound.
func ReadSamples(sq *sql.DB, fromID, toID int64) ([]attribute.Sample, error) {
	if toID <= 0 {
		toID = 1<<62 - 1
	}
	return scanSamples(sq.Query(`SELECT `+sampleColumns+` FROM samples WHERE id >= ? AND id <= ? ORDER BY id`, fromID, toID))
}

// ReadSamplesBetween returns rows with fromTS <= ts < toTS, ordered by id.
func ReadSamplesBetween(sq *sql.DB, fromTS, toTS int64) ([]attribute.Sample, error) {
	return scanSamples(sq.Query(`SELECT `+sampleColumns+` FROM samples WHERE ts >= ? AND ts < ? ORDER BY id`, fromTS, toTS))
}

// LastSampleBefore returns the latest row (by ts) with ts <= t.
func LastSampleBefore(sq *sql.DB, t int64) (attribute.Sample, bool, error) {
	rows, err := scanSamples(sq.Query(`SELECT `+sampleColumns+` FROM samples WHERE ts <= ? ORDER BY ts DESC, id DESC LIMIT 1`, t))
	if err != nil || len(rows) == 0 {
		return attribute.Sample{}, false, err
	}
	return rows[0], true, nil
}

// InsertSample adds a row and returns its id. Tests and fixtures only; the collector writes production rows.
func InsertSample(sq *sql.DB, s attribute.Sample) (int64, error) {
	res, err := sq.Exec(`INSERT INTO samples(ts, kind, app, title, url, cwd, idle_s, call_app) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.TS, s.Kind, nullIfEmpty(s.App), nullIfEmpty(s.Title), nullIfEmpty(s.URL), nullIfEmpty(s.CWD), s.IdleS, nullIfEmpty(s.CallApp))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OldestSampleTS is the smallest ts in samples; ok is false when the table is empty.
func OldestSampleTS(sq *sql.DB) (ts int64, ok bool, err error) {
	var v sql.NullInt64
	if err := sq.QueryRow(`SELECT MIN(ts) FROM samples`).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

// LatestSampleTS is the largest ts in samples; ok is false when the table is empty.
func LatestSampleTS(sq *sql.DB) (ts int64, ok bool, err error) {
	var v sql.NullInt64
	if err := sq.QueryRow(`SELECT MAX(ts) FROM samples`).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

// AppsWithURLOrCWD lists the apps of the rows with ts >= sinceTS that carried a URL or a cwd:
// the browsers and terminals the collector reads (no_always).
func AppsWithURLOrCWD(sq *sql.DB, sinceTS int64) (map[string]bool, error) {
	rows, err := sq.Query(`SELECT DISTINCT app FROM samples WHERE ts >= ? AND app IS NOT NULL AND (url IS NOT NULL OR cwd IS NOT NULL)`, sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var app string
		if err := rows.Scan(&app); err != nil {
			return nil, err
		}
		out[app] = true
	}
	return out, rows.Err()
}

// LastKind is the kind of the newest row by id.
func LastKind(sq *sql.DB) (kind string, ok bool, err error) {
	err = sq.QueryRow(`SELECT kind FROM samples ORDER BY id DESC LIMIT 1`).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return kind, err == nil, err
}

// LastBoundaryKind is the kind of the most recent non-work row (sleep/wake/resign/active/term).
func LastBoundaryKind(sq *sql.DB) (kind string, ok bool, err error) {
	err = sq.QueryRow(`SELECT kind FROM samples WHERE kind NOT IN ('sample','heartbeat') ORDER BY id DESC LIMIT 1`).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return kind, err == nil, err
}

// TSOfID returns the ts of one row; ok is false when the row is gone (purged).
func TSOfID(sq *sql.DB, id int64) (ts int64, ok bool, err error) {
	err = sq.QueryRow(`SELECT ts FROM samples WHERE id = ?`, id).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return ts, err == nil, err
}

func scanSamples(rows *sql.Rows, err error) ([]attribute.Sample, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attribute.Sample
	for rows.Next() {
		var s attribute.Sample
		if err := rows.Scan(&s.ID, &s.TS, &s.Kind, &s.App, &s.Title, &s.URL, &s.CWD, &s.IdleS, &s.CallApp); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
