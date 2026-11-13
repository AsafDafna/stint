package report

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/localday"
	"stint/internal/totals"
)

// topN is how many unattributed titles/URLs the weekly report lists.
const topN = 5

// noProject labels a project's time outside all of its sub-projects.
const noProject = "(no project)"

// WeekFile is the report path for the week starting at weekStart. The name uses the ISO
// week number of the Monday inside that week, e.g. 2026-W38.md.
func WeekFile(dir string, weekStart time.Time) string {
	y, w := isoWeek(weekStart)
	return filepath.Join(dir, fmt.Sprintf("%d-W%02d.md", y, w))
}

// isoWeek is the ISO week of the Monday inside the 7-day week starting at weekStart: every
// such week holds exactly one Monday, so whatever the week start, names never collide.
func isoWeek(weekStart time.Time) (year, week int) {
	return localday.Add(weekStart, totals.DaysFrom(weekStart.Weekday(), time.Monday)).ISOWeek()
}

var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// oneLine keeps a data-derived label on its own line of an aligned block.
func oneLine(s string) string { return lineBreaks.Replace(s) }

// days holds seconds per day of the week, its first day first.
type days [7]int64

func (d *days) add(o days) {
	for i, s := range o {
		d[i] += s
	}
}

func (d days) total() int64 {
	var t int64
	for _, s := range d {
		t += s
	}
	return t
}

// week holds a week's seconds per project and folded sub-project ("" = none).
type week map[string]map[string]*days

// rollup is a project's seconds with its sub-projects included.
func (w week) rollup(project string) days {
	var d days
	for _, s := range w[project] {
		d.add(*s)
	}
	return d
}

// all is the week's seconds over every project.
func (w week) all() days {
	var d days
	for project := range w {
		d.add(w.rollup(project))
	}
	return d
}

// readWeek sums the intervals inside the week starting at weekStart per project, folded
// sub-project and day.
func readWeek(sq *sql.DB, weekStart time.Time, loc *time.Location, known map[string]map[string]bool) (week, error) {
	start, end := weekStart.Unix(), totals.NextWeek(weekStart).Unix()
	ivs, err := db.ReadIntervals(sq, start, end)
	if err != nil {
		return nil, err
	}
	dayIndex := map[string]int{}
	for i := range 7 {
		dayIndex[totals.DayKey(localday.Add(weekStart, i), loc)] = i
	}
	w := week{}
	for _, iv := range ivs {
		sub := fold(iv.Subproject, known[iv.Project])
		if w[iv.Project] == nil {
			w[iv.Project] = map[string]*days{}
		}
		d := w[iv.Project][sub]
		if d == nil {
			d = &days{}
			w[iv.Project][sub] = d
		}
		for _, part := range totals.SplitByDay(max(iv.From, start), min(iv.To, end), loc) {
			d[dayIndex[part.Day]] += part.Seconds
		}
	}
	return w, nil
}

// fold maps a worktree sub-project onto the known sub-project it is named after
// ("acme-app-pr12" -> "acme-app"): the longest known name that the sub-project extends
// with "-". Worktree names are known sub-projects too, so the result is folded again
// until it stops changing.
func fold(sub string, known map[string]bool) string {
	for {
		into := ""
		for k := range known {
			if len(k) > len(into) && strings.HasPrefix(sub, k+"-") {
				into = k
			}
		}
		if into == "" {
			return sub
		}
		sub = into
	}
}

// capStatus compares a week's seconds with a weekly target of capHours.
func capStatus(sec int64, capHours float64) string {
	diff := sec - int64(capHours*3600)
	status := "met"
	if h := Hours(max(diff, -diff)); h != "0.0" {
		if diff > 0 {
			status = "over by " + h
		} else {
			status = "short by " + h
		}
	}
	return fmt.Sprintf("target %g · %s", capHours, status)
}

// isZero reports whether sec prints as 0.0 hours.
func isZero(sec int64) bool { return Hours(sec) == "0.0" }

// cell is a grid cell: blank when it rounds to 0.0.
func cell(sec int64) string {
	if isZero(sec) {
		return ""
	}
	return Hours(sec)
}

// shown is sec as the page prints it, so the heading's difference matches its totals.
func shown(sec int64) float64 {
	f, _ := strconv.ParseFloat(Hours(sec), 64)
	return f
}

// align lays rows out in columns: the first left-aligned and followed by one extra
// space, the others right-aligned two spaces apart.
func align(rows [][]string) []string {
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	lines := make([]string, len(rows))
	for j, r := range rows {
		line := fmt.Sprintf("%-*s ", widths[0], r[0])
		for i, c := range r[1:] {
			line += fmt.Sprintf("  %*s", widths[i+1], c)
		}
		lines[j] = strings.TrimRight(line, " ")
	}
	return lines
}

type row struct {
	label string
	days  days
}

// byTotal orders rows by week total, largest first.
func byTotal(rows []row) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].days.total(), rows[j].days.total()
		if a != b {
			return a > b
		}
		return rows[i].label < rows[j].label
	})
}

// Weekly renders the markdown for the week starting at weekStart, re-normalized to local
// 00:00 of the first day of the week it falls in.
func Weekly(sq *sql.DB, cfg *config.Config, p attribute.Params, weekStart time.Time) (string, error) {
	loc := cfg.Location
	weekStart = totals.WeekStart(weekStart, loc, cfg.FirstWeekday)
	known, err := totals.Subprojects(sq)
	if err != nil {
		return "", err
	}
	w, err := readWeek(sq, weekStart, loc, known)
	if err != nil {
		return "", err
	}
	prev, err := totals.WeekToDate(sq, totals.WeekStart(localday.Add(weekStart, -1), loc, cfg.FirstWeekday), loc)
	if err != nil {
		return "", err
	}
	var prevSec int64
	for _, s := range prev {
		prevSec += s
	}
	top, err := TopUnattributed(sq, p, weekStart.Unix(), totals.NextWeek(weekStart).Unix(), topN)
	if err != nil {
		return "", err
	}
	return render(cfg, weekStart, w, prevSec, top), nil
}

// render lays out the page: heading, a fenced block with the per-project summary and the
// per-day grid, then the top unattributed titles/URLs.
func render(cfg *config.Config, weekStart time.Time, w week, prevSec int64, top []Entry) string {
	capOf := func(project string) float64 { return cfg.Projects[project].WeeklyCapHours }
	all := w.all()
	var parents []row
	for project := range w {
		d := w.rollup(project)
		if project != attribute.Unattributed && (!isZero(d.total()) || capOf(project) > 0) {
			parents = append(parents, row{project, d})
		}
	}
	for project := range cfg.Projects {
		if _, ok := w[project]; !ok && capOf(project) > 0 {
			parents = append(parents, row{label: project})
		}
	}
	byTotal(parents)
	un := w.rollup(attribute.Unattributed)
	showUn := !isZero(un.total())

	var b strings.Builder
	_, wk := isoWeek(weekStart)
	fmt.Fprintf(&b, "# Week %d · %s – %s · %sh", wk, weekStart.Format("Jan 2"), localday.Add(weekStart, 6).Format("Jan 2"), Hours(all.total()))
	if !isZero(prevSec) {
		_, pw := isoWeek(localday.Add(weekStart, -7))
		fmt.Fprintf(&b, " (W%d %sh, %+.1f)", pw, Hours(prevSec), shown(all.total())-shown(prevSec))
	}
	b.WriteString("\n\n```\n")

	var summary [][]string
	var status []string
	for _, p := range parents {
		summary = append(summary, []string{p.label, Hours(p.days.total()) + "h"})
		s := ""
		if c := capOf(p.label); c > 0 {
			s = "  " + capStatus(p.days.total(), c)
		}
		status = append(status, s)
	}
	if showUn {
		summary = append(summary, []string{attribute.Unattributed, Hours(un.total()) + "h"})
		status = append(status, "")
	}
	if len(summary) > 0 {
		for i, line := range align(summary) {
			b.WriteString(line + status[i] + "\n")
		}
		b.WriteString("\n")
	}

	header := []string{""}
	for d := range 7 {
		header = append(header, localday.Add(weekStart, d).Weekday().String()[:3])
	}
	grid := [][]string{append(header, "Total")}
	addRow := func(label string, d days) {
		r := []string{oneLine(label)}
		for _, s := range d {
			r = append(r, cell(s))
		}
		grid = append(grid, append(r, Hours(d.total())))
	}
	for _, p := range parents {
		addRow(p.label, p.days)
		var subs []row
		for sub, d := range w[p.label] {
			if sub != "" && !isZero(d.total()) {
				subs = append(subs, row{sub, *d})
			}
		}
		if len(subs) == 0 {
			continue
		}
		byTotal(subs)
		for _, s := range subs {
			addRow("  "+s.label, s.days)
		}
		if own := w[p.label][""]; own != nil && !isZero(own.total()) {
			addRow("  "+noProject, *own)
		}
	}
	if showUn {
		addRow(attribute.Unattributed, un)
	}
	addRow("Total", all)
	for _, line := range align(grid) {
		b.WriteString(line + "\n")
	}
	b.WriteString("```\n")

	top = slices.DeleteFunc(top, func(e Entry) bool { return isZero(e.Seconds) })
	if len(top) > 0 {
		b.WriteString("\n## Top unattributed\n\n```\n")
		width := 0
		for _, e := range top {
			width = max(width, len(Hours(e.Seconds)))
		}
		for _, e := range top {
			fmt.Fprintf(&b, "%*s  %s\n", width, Hours(e.Seconds), oneLine(e.Label))
		}
		b.WriteString("```\n")
	}
	return b.String()
}

// Write renders the week and writes it to settings.vault_reports_dir, overwriting. The dir
// is never empty: config rejects a missing vault_reports_dir.
func Write(sq *sql.DB, cfg *config.Config, p attribute.Params, weekStart time.Time) (string, string, error) {
	weekStart = totals.WeekStart(weekStart, cfg.Location, cfg.FirstWeekday)
	md, err := Weekly(sq, cfg, p, weekStart)
	if err != nil {
		return "", "", err
	}
	path := WeekFile(cfg.Settings.VaultReportsDir, weekStart)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(md), 0o644); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	return path, md, nil
}

// WriteMissing writes every completed week whose file is missing or was last written
// before the week ended, from the first week with retained samples, and returns the paths
// written.
func WriteMissing(sq *sql.DB, cfg *config.Config, p attribute.Params, now time.Time) ([]string, error) {
	oldest, ok, err := db.OldestSampleTS(sq)
	if err != nil || !ok {
		return nil, err
	}
	return writeWeeks(sq, cfg, p, totals.WeekStart(time.Unix(oldest, 0), cfg.Location, cfg.FirstWeekday), now, false)
}

// WriteWeeks rewrites every completed week from the week containing `from`, whether or
// not a file exists (used after recompute), and returns the paths written.
func WriteWeeks(sq *sql.DB, cfg *config.Config, p attribute.Params, from, now time.Time) ([]string, error) {
	return writeWeeks(sq, cfg, p, totals.WeekStart(from, cfg.Location, cfg.FirstWeekday), now, true)
}

// writeWeeks writes every completed week from `first`. Without force it skips only the
// weeks whose file is already final (see final); with force every week is rewritten.
func writeWeeks(sq *sql.DB, cfg *config.Config, p attribute.Params, first, now time.Time, force bool) ([]string, error) {
	var written []string
	for w := first; !totals.NextWeek(w).After(now); w = totals.NextWeek(w) {
		path := WeekFile(cfg.Settings.VaultReportsDir, w)
		if !force && final(path, w) {
			continue
		}
		if _, _, err := Write(sq, cfg, p, w); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

// final reports whether the week's file exists and was written at or after the week
// ended. An earlier mtime means it holds a partial week (`report --week` inside the
// running week), so the catch-up must replace it once the week is complete. The rule
// trusts the mtime: a sync client that rewrites the file only costs one extra rewrite,
// and a catch-up that fires right after the week ends, before the collector's tail (<=5
// min) has been ingested, marks the week final without it; `recompute` refreshes it.
func final(path string, weekStart time.Time) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.ModTime().Before(totals.NextWeek(weekStart))
}

// Hours formats seconds as "19.5".
func Hours(sec int64) string { return fmt.Sprintf("%.1f", float64(sec)/3600) }
