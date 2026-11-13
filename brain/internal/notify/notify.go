package notify

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/localday"
	"stint/internal/report"
	"stint/internal/totals"
)

// notified.kind values.
const (
	KindMorning = "morning"
	KindCap80   = "cap80"
	KindCap100  = "cap100"
	KindHealth  = "health"

	morningHour = 8 // first run at or after 08:00 local sends the morning push
	tuneTopN    = 3
)

// Run sends every push that is due and not yet recorded and returns the distinct
// kinds sent (two projects crossing a cap in one run yield two pushes, one kind).
// A push is recorded in notified only after a 2xx, so a failed send is retried on the
// next run. healthReasons come from health.Check; an empty slice sends nothing.
// ntfy titles are ASCII only (the Title header cannot carry UTF-8); bodies may.
func Run(sq *sql.DB, cfg *config.Config, s Sender, p attribute.Params, healthReasons []string, now time.Time) ([]string, error) {
	loc := cfg.Location
	lt := now.In(loc)
	day := totals.DayKey(now, loc)
	week := totals.WeekStart(now, loc, cfg.FirstWeekday)
	weekKey := totals.DayKey(week, loc)
	wtd, err := totals.WeekToDate(sq, week, loc)
	if err != nil {
		return nil, err
	}
	var errs []error
	var sentKinds []string
	// due reports whether kind/project/key has no notified row yet; a DB error is
	// collected and counts as not due.
	due := func(kind, project, key string) bool {
		done, err := sent(sq, kind, project, key)
		if err != nil {
			errs = append(errs, err)
			return false
		}
		return !done
	}
	deliver := func(kind, project, key, title, body string, also ...string) {
		if err := s.Send(title, body); err != nil {
			errs = append(errs, fmt.Errorf("%s push: %w", kind, err))
			return
		}
		if !slices.Contains(sentKinds, kind) {
			sentKinds = append(sentKinds, kind)
		}
		for _, k := range append([]string{kind}, also...) {
			if err := record(sq, k, project, key, now); err != nil {
				errs = append(errs, err)
			}
		}
	}
	push := func(kind, project, key, title, body string, also ...string) {
		if due(kind, project, key) {
			deliver(kind, project, key, title, body, also...)
		}
	}

	// The sent check comes first: the tune list scans a whole day and would otherwise
	// run on every 5-minute run after the push. A failed scan skips the morning push
	// for this run only (retried next run); the cap loop and health push still run.
	if lt.Hour() >= morningHour && due(KindMorning, "", day) {
		today := localday.Of(now, loc)
		yesterday := localday.Add(today, -1)
		var tune []report.Entry
		var tuneErr error
		if now.Before(cfg.TuneUntil) {
			tune, tuneErr = report.TopUnattributed(sq, p, yesterday.Unix(), today.Unix(), tuneTopN)
		}
		var through map[string]int64
		var throughErr error
		if tuneErr == nil {
			through, throughErr = totals.Between(sq, totals.WeekStart(yesterday, loc, cfg.FirstWeekday), today, loc)
		}
		switch {
		case tuneErr != nil:
			errs = append(errs, fmt.Errorf("morning tune list: %w", tuneErr))
		case throughErr != nil:
			errs = append(errs, fmt.Errorf("morning totals: %w", throughErr))
		default:
			pending, err := db.ReadPending(sq)
			if err != nil {
				errs = append(errs, fmt.Errorf("morning reminder: %w", err)) // the push still goes, without the line
			}
			deliver(KindMorning, "", day, MorningTitle(now, loc, cfg.FirstWeekday), MorningBody(through, cfg, now, tune, pending))
		}
	}

	for _, name := range sortedProjects(cfg) {
		capHours := cfg.Projects[name].WeeklyCapHours
		if capHours <= 0 {
			continue
		}
		capSec := int64(capHours * 3600)
		used := wtd[name]
		switch {
		case used >= capSec:
			push(KindCap100, name, weekKey, "stint | cap reached",
				fmt.Sprintf("%s %sh / %sh · cap reached", name, report.Hours(used), report.Hours(capSec)), KindCap80)
		case used*10 >= capSec*8:
			push(KindCap80, name, weekKey, "stint | 80% of cap",
				fmt.Sprintf("%s %sh / %sh · %sh left · %s", name, report.Hours(used), report.Hours(capSec), report.Hours(capSec-used), daysLeftText(now, loc, cfg.FirstWeekday)))
		}
	}

	healthSent, err := PushHealth(sq, cfg, s, healthReasons, now)
	if err != nil {
		errs = append(errs, err)
	}
	if healthSent {
		sentKinds = append(sentKinds, KindHealth)
	}
	return sentKinds, errors.Join(errs...)
}

// PushHealth sends the once-per-day health push when reasons is non-empty and reports
// whether it sent one. It is separate from Run so a schema mismatch (which stops the
// pipeline) can still alert.
func PushHealth(sq *sql.DB, cfg *config.Config, s Sender, reasons []string, now time.Time) (bool, error) {
	if len(reasons) == 0 {
		return false, nil
	}
	day := totals.DayKey(now, cfg.Location)
	done, err := sent(sq, KindHealth, "", day)
	if err != nil || done {
		return false, err
	}
	if err := s.Send("stint | collector degraded", strings.Join(reasons, "\n")); err != nil {
		return false, fmt.Errorf("health push: %w", err)
	}
	return true, record(sq, KindHealth, "", day, now)
}

// MorningTitle is "stint | last week (final)" on the first day of the week and
// "stint | week through Mon" (yesterday's weekday) otherwise.
func MorningTitle(now time.Time, loc *time.Location, first time.Weekday) string {
	lt := now.In(loc)
	if lt.Weekday() == first {
		return "stint | last week (final)"
	}
	return "stint | week through " + lt.AddDate(0, 0, -1).Weekday().String()[:3]
}

// MorningBody formats the morning push from through, the current week's totals up to
// the end of yesterday (today is excluded):
//
//	acme 19.5h / 30.0h · 10.5h left · 3 days
//	phd 3.2h
//	unattributed 1.1h
//
// Days left counts today through the last day of the week (3 on a Thursday when weeks
// start on Sunday): today's work is not in through yet. On the first day of the week,
// through is the full week that just ended and a capped project reads "acme 29.1h /
// 30.0h · short by 0.9h" (or "over by", or "met" when the difference rounds to 0.0).
// Capped projects always appear, even at 0 h; others only when non-zero. While calls or
// stretches wait for the pop-up, a "to attribute: …" line follows (reminderLine). During
// the tuning period yesterday's top unattributed titles/URLs are appended.
func MorningBody(through map[string]int64, cfg *config.Config, now time.Time, tune []report.Entry, pending db.Pending) string {
	final := now.In(cfg.Location).Weekday() == cfg.FirstWeekday
	var lines []string
	for _, name := range sortedProjects(cfg) {
		used := through[name]
		if capHours := cfg.Projects[name].WeeklyCapHours; capHours > 0 {
			capSec := int64(capHours * 3600)
			line := fmt.Sprintf("%s %sh / %sh", name, report.Hours(used), report.Hours(capSec))
			switch {
			case !final:
				line += fmt.Sprintf(" · %sh left · %s", report.Hours(max(capSec-used, 0)), daysLeftText(now, cfg.Location, cfg.FirstWeekday))
			case report.Hours(max(capSec-used, used-capSec)) == "0.0":
				line += " · met"
			case used < capSec:
				line += fmt.Sprintf(" · short by %sh", report.Hours(capSec-used))
			default:
				line += fmt.Sprintf(" · over by %sh", report.Hours(used-capSec))
			}
			lines = append(lines, line)
		} else if used > 0 {
			lines = append(lines, fmt.Sprintf("%s %sh", name, report.Hours(used)))
		}
	}
	if u := through[attribute.Unattributed]; u > 0 {
		lines = append(lines, fmt.Sprintf("%s %sh", attribute.Unattributed, report.Hours(u)))
	}
	if len(lines) == 0 {
		if final {
			lines = append(lines, "nothing tracked last week")
		} else {
			lines = append(lines, "nothing tracked yet this week")
		}
	}
	if line := reminderLine(pending); line != "" {
		lines = append(lines, line)
	}
	if len(tune) > 0 {
		lines = append(lines, "", "yesterday unattributed:")
		for _, e := range tune {
			lines = append(lines, fmt.Sprintf("%sh %s", report.Hours(e.Seconds), e.Label))
		}
	}
	return strings.Join(lines, "\n")
}

// reminderLine is "to attribute: 2 meetings, 3 stretches (1.6h)": zero
// counts omitted, a count of one singular, hours = the pending stretches' unattributed time;
// "" when nothing is pending.
func reminderLine(p db.Pending) string {
	var parts []string
	if p.Calls > 0 {
		parts = append(parts, plural(p.Calls, "meeting", "meetings"))
	}
	if p.Stretches > 0 {
		parts = append(parts, fmt.Sprintf("%s (%sh)", plural(p.Stretches, "stretch", "stretches"), report.Hours(p.StretchSeconds)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "to attribute: " + strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// DaysLeft counts today through the last day of the week inclusive (first = 7, the day
// before first = 1).
func DaysLeft(now time.Time, loc *time.Location, first time.Weekday) int {
	return 7 - totals.DaysFrom(first, now.In(loc).Weekday())
}

// daysLeftText renders DaysLeft as "1 day" or "N days".
func daysLeftText(now time.Time, loc *time.Location, first time.Weekday) string {
	d := DaysLeft(now, loc, first)
	if d == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", d)
}

// sortedProjects lists project names with capped ones first, each group alphabetical.
func sortedProjects(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Projects))
	for n := range cfg.Projects {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := cfg.Projects[names[i]].WeeklyCapHours > 0, cfg.Projects[names[j]].WeeklyCapHours > 0
		if a != b {
			return a
		}
		return names[i] < names[j]
	})
	return names
}

func sent(sq *sql.DB, kind, project, key string) (bool, error) {
	var n int
	err := sq.QueryRow(`SELECT COUNT(*) FROM notified WHERE kind = ? AND project = ? AND period_key = ?`, kind, project, key).Scan(&n)
	return n > 0, err
}

// record marks a push as sent. It runs only after a 2xx, so a failure here (or a
// crash between the two) yields one duplicate push on the next run; the cap100+cap80
// pair is likewise not transactional. A rare duplicate beats a lost push, so this is
// deliberate.
func record(sq *sql.DB, kind, project, key string, now time.Time) error {
	_, err := sq.Exec(`INSERT OR IGNORE INTO notified(kind, project, period_key, sent_at) VALUES (?, ?, ?, ?)`, kind, project, key, now.Unix())
	return err
}
