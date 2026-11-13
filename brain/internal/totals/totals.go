// Package totals rolls intervals up per project and local day, with week-to-date.
package totals

import (
	"database/sql"
	"strings"
	"time"

	"stint/internal/db"
	"stint/internal/localday"
)

// DayPart is the slice of an interval that falls on one local day.
type DayPart struct {
	Day     string // YYYY-MM-DD
	Seconds int64
}

// DayKey formats t as a local day.
func DayKey(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02") }

// DaysFrom counts the days forward from weekday from to weekday to, 0 through 6.
func DaysFrom(from, to time.Weekday) int { return (int(to) - int(from) + 7) % 7 }

// WeekStart is the first instant (local) of the latest day at or before t that falls on first.
func WeekStart(t time.Time, loc *time.Location, first time.Weekday) time.Time {
	lt := t.In(loc)
	return localday.Start(lt.Year(), lt.Month(), lt.Day()-DaysFrom(first, lt.Weekday()), loc)
}

// NextWeek is the first instant of the day 7 calendar days after weekStart's (167 or 169
// hours across DST). It does not re-normalize: weekStart must be a WeekStart result, because
// the calendar arithmetic runs in weekStart's Location and the result keeps it.
func NextWeek(weekStart time.Time) time.Time { return localday.Add(weekStart, 7) }

// SplitByDay cuts [from, to) where local days begin.
func SplitByDay(from, to int64, loc *time.Location) []DayPart {
	var parts []DayPart
	for cur := from; cur < to; {
		lt := time.Unix(cur, 0).In(loc)
		day := DayKey(lt, loc)
		end := min(localday.Add(lt, 1).Unix(), to)
		parts = append(parts, DayPart{Day: day, Seconds: end - cur})
		cur = end
	}
	return parts
}

// metaWeekStart is the meta key holding the lowercase weekday that starts the weeks in totals;
// the collector's menu reads it (contract: collector/schema.sql).
const metaWeekStart = "week_start"

// Recompute rebuilds the whole totals table from intervals in one transaction. Rows are
// keyed by "project" or "project/subproject" (the collector's menu rolls up on "/").
// week_seconds is that key's total over the 7-day week starting on first that contains the
// day. The same transaction writes first to meta, so the menu never pairs these weeks with
// another week start.
func Recompute(sq *sql.DB, loc *time.Location, first time.Weekday, now time.Time) error {
	// The pool has one connection, so the read must finish (rows closed) before Begin.
	days, err := daySeconds(sq, loc)
	if err != nil {
		return err
	}
	type wkey struct{ project, week string }
	weeks := map[wkey]int64{}
	weekOfDay := map[string]string{} // each distinct day is parsed once
	for k, secs := range days {
		if _, ok := weekOfDay[k.day]; !ok {
			week, err := weekOf(k.day, loc, first)
			if err != nil {
				return err
			}
			weekOfDay[k.day] = week
		}
		weeks[wkey{k.project, weekOfDay[k.day]}] += secs
	}
	tx, err := sq.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM totals`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO totals(project, day, day_seconds, week_seconds, updated_at) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, secs := range days {
		if _, err := stmt.Exec(k.project, k.day, secs, weeks[wkey{k.project, weekOfDay[k.day]}], now.Unix()); err != nil {
			return err
		}
	}
	if err := db.SetMetaTx(tx, metaWeekStart, strings.ToLower(first.String())); err != nil {
		return err
	}
	return tx.Commit()
}

// weekOf is the DayKey of the first day of the week that holds day (a DayKey).
func weekOf(day string, loc *time.Location, first time.Weekday) (string, error) {
	d, err := localday.Parse(day, loc)
	if err != nil {
		return "", err
	}
	return DayKey(WeekStart(d, loc, first), loc), nil
}

type key struct{ project, day string }

// daySeconds sums intervals per (totals key, local day) and closes its cursor before returning.
func daySeconds(sq *sql.DB, loc *time.Location) (map[key]int64, error) {
	rows, err := sq.Query(`SELECT from_ts, to_ts, project, COALESCE(subproject,'') FROM intervals ORDER BY from_ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := map[key]int64{}
	for rows.Next() {
		var from, to int64
		var project, sub string
		if err := rows.Scan(&from, &to, &project, &sub); err != nil {
			return nil, err
		}
		if sub != "" {
			project += "/" + sub
		}
		for _, part := range SplitByDay(from, to, loc) {
			days[key{project, part.Day}] += part.Seconds
		}
	}
	return days, rows.Err()
}

// Parent strips a "/subproject" suffix from a totals key.
func Parent(k string) string {
	if i := strings.IndexByte(k, '/'); i >= 0 {
		return k[:i]
	}
	return k
}

// Subprojects maps each project to every sub-project it has a row for, on any day.
func Subprojects(sq *sql.DB) (map[string]map[string]bool, error) {
	return SubprojectsSince(sq, "")
}

// SubprojectsSince maps each project to the sub-projects it has a row for on a day >= since
// (a DayKey; "" means any day).
func SubprojectsSince(sq *sql.DB, since string) (map[string]map[string]bool, error) {
	rows, err := sq.Query(`SELECT DISTINCT project FROM totals WHERE instr(project, '/') > 0 AND day >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		p, sub, _ := strings.Cut(k, "/")
		if out[p] == nil {
			out[p] = map[string]bool{}
		}
		out[p][sub] = true
	}
	return out, rows.Err()
}

// WeekToDate returns each top-level project's seconds (subprojects rolled up) in the
// week starting at weekStart.
func WeekToDate(sq *sql.DB, weekStart time.Time, loc *time.Location) (map[string]int64, error) {
	return Between(sq, weekStart, NextWeek(weekStart), loc)
}

// Between returns each top-level project's seconds (subprojects rolled up) over the
// local days from the day of from up to, excluding, the day of to.
func Between(sq *sql.DB, from, to time.Time, loc *time.Location) (map[string]int64, error) {
	rows, err := sq.Query(`SELECT project, SUM(day_seconds) FROM totals WHERE day >= ? AND day < ? GROUP BY project`,
		DayKey(from, loc), DayKey(to, loc))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var s int64
		if err := rows.Scan(&p, &s); err != nil {
			return nil, err
		}
		out[Parent(p)] += s
	}
	return out, rows.Err()
}
