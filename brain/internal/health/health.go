// Package health decides whether the collector is degraded and records the verdict in meta.
package health

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
)

const (
	// MetaHealth / MetaHealthAt are the meta keys the collector's menu bar reads.
	MetaHealth   = "health"
	MetaHealthAt = "health_at"
	// metaStaleTS / metaStaleRuns count consecutive in-active-hours runs with no new sample.
	metaStaleTS   = "health_stale_ts"
	metaStaleRuns = "health_stale_runs"

	staleAfter     = 30 * 60 // no rows for this long while awake = collector dead
	staleRunsMax   = 12      // after sleep/resign: this many runs (1 h) with no new row = dead anyway
	titleWindow    = 60 * 60 // look-back for the title check
	titleMinRows   = 12      // fewer rows than this and the title ratio is noise
	urlWindow      = 2 * 60 * 60
	urlNullMax     = 60 * 60 // browser-foreground seconds without a URL that trigger the alert
	recentWindow   = urlWindow
	schemaMismatch = "schema version mismatch"
)

// NoTitleApps are frontmost apps that legitimately have no window title.
var NoTitleApps = map[string]bool{
	"com.apple.loginwindow": true,
	"com.apple.finder":      true, // desktop focus
}

// BrowserApps are the apps the collector samples URLs from.
var BrowserApps = map[string]bool{
	"com.bookry.wavebox":      true,
	"com.google.Chrome":       true,
	"com.officecommun.search": true,
}

// Facts is what Evaluate needs from the database.
type Facts struct {
	HasSamples   bool
	LatestTS     int64
	LastKind     string             // kind of the newest row (by id); "" when none
	LastBoundary string             // kind of the newest non-work row; "" when none
	StaleRuns    int                // consecutive in-active-hours runs that saw no new row
	Recent       []attribute.Sample // rows with ts >= now - 2 h, ordered by id
	StretchErrs  int                // stretches rows whose answer failed to apply
	StretchErr   string             // the oldest such row's rules_error
}

// Evaluate returns the reasons the collector looks degraded; nil means healthy.
// Outside settings.active_hours nothing is checked. Staleness rules: no row for 30 min
// alerts, except while the newest row is a clean `term` (the collector was stopped on
// purpose) and, for up to staleRunsMax runs, after a `sleep`/`resign` boundary (the Mac
// may still be asleep; after an hour of brain runs it clearly is not).
func Evaluate(f Facts, cfg *config.Config, p attribute.Params, now time.Time) []string {
	if !cfg.InActiveHours(now) {
		return nil
	}
	var reasons []string
	stale := now.Unix() - f.LatestTS
	switch {
	case !f.HasSamples:
		reasons = append(reasons, "no samples in the database")
	case stale <= staleAfter, f.LastKind == "term":
	case f.LastBoundary == "sleep" || f.LastBoundary == "resign":
		if f.StaleRuns >= staleRunsMax {
			reasons = append(reasons, fmt.Sprintf("no samples for %d min after %s (collector not back?)", stale/60, f.LastBoundary))
		}
	default:
		reasons = append(reasons, fmt.Sprintf("no samples for %d min while awake", stale/60))
	}
	var titled, untitled int
	for _, s := range f.Recent {
		if !attribute.IsWorkRow(s.Kind) || s.TS < now.Unix()-titleWindow || s.App == "" || NoTitleApps[s.App] {
			continue
		}
		if s.Title == "" {
			untitled++
		} else {
			titled++
		}
	}
	if n := titled + untitled; n >= titleMinRows && untitled*2 > n {
		reasons = append(reasons, fmt.Sprintf("title NULL on %d%% of samples in the last hour (Accessibility grant?)", untitled*100/n))
	}
	// The 2 h window is Check's: Recent holds only rows with ts >= now - urlWindow.
	var noURL int64
	for i, sp := range attribute.Coverage(f.Recent, p) {
		s := f.Recent[i]
		if sp.Working && BrowserApps[s.App] && s.URL == "" {
			noURL += sp.To - sp.From
		}
	}
	if noURL > urlNullMax {
		reasons = append(reasons, fmt.Sprintf("url NULL for %d min of browser foreground in the last 2 h (Automation grant?)", noURL/60))
	}
	if f.StretchErrs > 0 {
		reasons = append(reasons, fmt.Sprintf("stretch answer not applied (%d): %s", f.StretchErrs, f.StretchErr))
	}
	return reasons
}

// Check gathers facts, advances the stale-run counter, evaluates and writes meta.health /
// meta.health_at (plus the counter) in one transaction. When a read fails it still stamps
// health_at with a "check_failed: <err>" reason, so the menu bar never shows a stale "ok",
// and returns the read error with no reasons.
func Check(sq *sql.DB, cfg *config.Config, p attribute.Params, now time.Time) ([]string, error) {
	f, prevTS, prevRuns, err := readFacts(sq, now)
	if err != nil {
		return nil, errors.Join(err, WriteMeta(sq, []string{"check_failed: " + err.Error()}, now))
	}
	inHours := cfg.InActiveHours(now)
	f.StaleRuns = nextStaleRuns(prevTS, prevRuns, f.LatestTS, inHours)
	reasons := Evaluate(f, cfg, p, now)
	tx, err := sq.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if inHours {
		if err := db.SetMetaTx(tx, metaStaleTS, strconv.FormatInt(f.LatestTS, 10)); err != nil {
			return nil, err
		}
		if err := db.SetMetaTx(tx, metaStaleRuns, strconv.Itoa(f.StaleRuns)); err != nil {
			return nil, err
		}
	}
	if err := writeMetaTx(tx, reasons, now); err != nil {
		return nil, err
	}
	return reasons, tx.Commit()
}

// readFacts reads everything Check needs before any transaction opens (see package db:
// a *sql.DB call while a Tx is open deadlocks on the single connection).
func readFacts(sq *sql.DB, now time.Time) (f Facts, prevTS, prevRuns string, err error) {
	if f.LatestTS, f.HasSamples, err = db.LatestSampleTS(sq); err != nil {
		return
	}
	if f.LastKind, _, err = db.LastKind(sq); err != nil {
		return
	}
	if f.LastBoundary, _, err = db.LastBoundaryKind(sq); err != nil {
		return
	}
	if f.Recent, err = db.ReadSamplesBetween(sq, now.Unix()-recentWindow, 1<<62-1); err != nil {
		return
	}
	if f.StretchErrs, f.StretchErr, err = db.StretchErrors(sq); err != nil {
		return
	}
	if prevTS, _, err = db.GetMeta(sq, metaStaleTS); err != nil {
		return
	}
	prevRuns, _, err = db.GetMeta(sq, metaStaleRuns)
	return
}

// nextStaleRuns is the counter after this run: +1 when the newest row's ts is unchanged
// since the previous in-hours run, 0 when a new row arrived. Outside active hours the
// counter is frozen (Check writes nothing), so a counter built up before 23:00 continues
// at the first in-hours run the next morning instead of starting over.
func nextStaleRuns(prevTS, prevRuns string, latestTS int64, inHours bool) int {
	n, _ := strconv.Atoi(prevRuns) // absent or corrupt: start the count at 0
	if !inHours {
		return n
	}
	if prevTS != strconv.FormatInt(latestTS, 10) {
		return 0
	}
	return n + 1
}

// WriteMeta stores "ok" or the joined reasons, plus the epoch time of the write, atomically.
func WriteMeta(sq *sql.DB, reasons []string, now time.Time) error {
	tx, err := sq.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeMetaTx(tx, reasons, now); err != nil {
		return err
	}
	return tx.Commit()
}

func writeMetaTx(tx *sql.Tx, reasons []string, now time.Time) error {
	v := "ok"
	if len(reasons) > 0 {
		v = strings.Join(reasons, "; ")
	}
	if err := db.SetMetaTx(tx, MetaHealth, v); err != nil {
		return err
	}
	return db.SetMetaTx(tx, MetaHealthAt, fmt.Sprint(now.Unix()))
}

// SchemaReason is the health string for a database the brain refuses to process; a nil
// error gives the bare mismatch text.
func SchemaReason(err error) string {
	if err == nil {
		return schemaMismatch
	}
	return schemaMismatch + ": " + err.Error()
}
