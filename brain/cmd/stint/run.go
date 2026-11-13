package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"stint/internal/attribute"
	"stint/internal/db"
	"stint/internal/health"
	"stint/internal/ingest"
	"stint/internal/localday"
	"stint/internal/notify"
	"stint/internal/report"
	"stint/internal/totals"
)

// run is the launchd pipeline. Pop-up answers are applied first (log-and-continue),
// then ingest → totals → health → notify → report → purge. An interrupted recompute
// (meta.recompute_since present) is finished first: a marker that is unparseable or now
// before the retention horizon is logged and cleared so the pipeline still runs, a
// rebuild failure there still aborts, and a report-step failure is logged and the
// pipeline continues (see recomputeFrom). Ingest and totals failures abort; later stages
// log and continue so an offline ntfy never blocks the purge, and the exit code reports
// any failure. It logs only when something happened, so a quiet day leaves the log empty.
func (a *app) run(args []string) error {
	if err := noArgs("run", args); err != nil {
		return err
	}
	var errs []error
	// Call and stretch detection below read from where this run's rebuild would start and ask
	// only about calls and stretches closed by rows newer than the watermark. Both are captured
	// first: a resumed recompute and applied pop-up answers rebuild to the newest sample and
	// move the watermark, and a start taken after that can lie past a call or stretch that
	// closed before it.
	p := ingest.Params(a.cfg, a.matcher, a.now)
	from, err := a.captureDetectStart(p)
	if err != nil {
		return err
	}
	if v, ok, err := db.GetMeta(a.sq, metaRecomputeSince); err != nil {
		return err
	} else if ok {
		day, perr := localday.Parse(v, a.cfg.Location)
		if perr != nil {
			a.logf("resume: bad recompute_since %q, clearing: %v", v, perr)
			if cerr := db.DeleteMeta(a.sq, metaRecomputeSince); cerr != nil {
				return cerr
			}
		} else {
			a.logf("resuming interrupted recompute since %s", v)
			switch rerr := a.recomputeFrom(day); {
			case errors.Is(rerr, db.ErrBeforeHorizon), errors.Is(rerr, ingest.ErrEmpty): // recomputeFrom cleared the marker
				a.logf("resume: recompute_since %s cleared: %v", v, rerr)
			case errors.Is(rerr, errRecomputeReport):
				errs = append(errs, rerr)
			case rerr != nil:
				return rerr
			}
		}
	}
	// Pop-up answers next: they rebuild from the earliest answered row's day; a failure is
	// logged and the rest of the pipeline still runs.
	if err := a.applyAnswers(); err != nil {
		errs = append(errs, err)
	}
	// Learned rules reloaded the config: everything below uses the new matcher. The start and
	// watermark above depend on settings only, so they stay.
	p = ingest.Params(a.cfg, a.matcher, a.now)
	res, err := ingest.Run(a.sq, a.cfg, a.matcher, a.now)
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	if res.FirstRun && res.Rows > 0 {
		a.logf("first run: built intervals from the oldest sample (%d rows)", res.Rows)
	}
	if err := totals.Recompute(a.sq, a.cfg.Location, a.cfg.FirstWeekday, a.now); err != nil {
		return fmt.Errorf("totals: %w", err)
	}
	// Calls and stretches that closed in the rows ingested above, read from the start captured
	// at the top.
	if res.Rows > 0 {
		if err := a.detectClosed(from, p); err != nil {
			errs = append(errs, err)
		}
	}
	if err := db.SetMeta(a.sq, metaProjects, strings.Join(a.cfg.ProjectNames(), ",")); err != nil {
		errs = append(errs, fmt.Errorf("meta.projects: %w", err))
	}
	if err := a.writePicker(); err != nil {
		errs = append(errs, fmt.Errorf("meta.picker: %w", err))
	}
	reasons, err := health.Check(a.sq, a.cfg, p, a.now)
	if err != nil {
		errs = append(errs, fmt.Errorf("health: %w", err))
	}
	if len(reasons) > 0 {
		a.logf("health: %s", strings.Join(reasons, "; "))
	}
	if s := a.sender(); s != nil {
		sent, err := notify.Run(a.sq, a.cfg, s, p, reasons, a.now)
		if err != nil {
			errs = append(errs, fmt.Errorf("notify: %w", err))
		}
		for _, kind := range sent {
			a.logf("sent %s push", kind)
		}
	}
	written, err := report.WriteMissing(a.sq, a.cfg, p, a.now)
	if err != nil {
		errs = append(errs, fmt.Errorf("report: %w", err))
	}
	for _, path := range written {
		a.logf("wrote report %s", path)
	}
	purged, err := ingest.Purge(a.sq, a.cfg, a.now)
	if err != nil {
		errs = append(errs, fmt.Errorf("purge: %w", err))
	} else if purged > 0 {
		if err := a.notePurge(purged); err != nil {
			errs = append(errs, fmt.Errorf("purge: %w", err))
		}
	}
	return errors.Join(errs...)
}

// detectStart is where call and stretch detection reads from: the rebuild start a run would
// use (ingest.RunStart) and the watermark ts, captured before anything moves the watermark.
type detectStart struct {
	fromID, sinceTS  int64
	haveFrom, haveWM bool
}

func (a *app) captureDetectStart(p attribute.Params) (detectStart, error) {
	var d detectStart
	var err error
	if d.sinceTS, d.haveWM, err = ingest.WatermarkTS(a.sq); err != nil {
		return d, err
	}
	d.fromID, d.haveFrom, err = ingest.RunStart(a.sq, p)
	return d, err
}

// detectClosed inserts pending rows for the calls and stretches closed by rows newer than
// d.sinceTS. Without a watermark (first run, or its row purged) nothing is inserted: history
// before the deploy keeps the guess.
func (a *app) detectClosed(d detectStart, p attribute.Params) error {
	if !d.haveFrom {
		return nil
	}
	var errs []error
	if d.haveWM {
		if err := a.detectCalls(d.fromID, d.sinceTS, p); err != nil {
			errs = append(errs, fmt.Errorf("calls: %w", err))
		}
	}
	if err := a.stretchesAfterIngest(d.fromID, d.sinceTS, d.haveWM, p); err != nil {
		errs = append(errs, fmt.Errorf("stretches: %w", err))
	}
	return errors.Join(errs...)
}

// Purge counters: once the DB is older than raw_retention_days almost every 5-minute run
// deletes a few rows, so per-run lines would fill the log. notePurge adds to the day's
// tally and logs the previous day's total once, on the first purge of the next day.
const (
	metaPurgeDay  = "purge_day"
	metaPurgeRows = "purge_rows"
)

func (a *app) notePurge(purged int64) error {
	today := a.now.In(a.cfg.Location).Format("2006-01-02")
	day, _, err := db.GetMeta(a.sq, metaPurgeDay)
	if err != nil {
		return err
	}
	rowsStr, _, err := db.GetMeta(a.sq, metaPurgeRows)
	if err != nil {
		return err
	}
	rows, _ := strconv.ParseInt(rowsStr, 10, 64) // absent or corrupt: start the day at 0
	if day != today {
		if day != "" && rows > 0 {
			a.logf("purged %d samples older than %d days on %s", rows, a.cfg.Settings.RawRetentionDays, day)
		}
		rows = 0
	}
	// Both keys land together, so a crash cannot carry one day's count into the next.
	tx, err := a.sq.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.SetMetaTx(tx, metaPurgeDay, today); err != nil {
		return err
	}
	if err := db.SetMetaTx(tx, metaPurgeRows, strconv.FormatInt(rows+purged, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

// logf writes one timestamped line to stdout (launchd's StandardOutPath).
func (a *app) logf(format string, args ...any) {
	fmt.Fprintf(a.stdout, "%s %s\n", a.now.In(a.cfg.Location).Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// sender is the ntfy client, or nil when config.NtfyEndpoint is empty (neither
// settings.ntfy_url nor settings.ntfy_topic set: pushes disabled).
func (a *app) sender() notify.Sender {
	url := a.cfg.NtfyEndpoint()
	if url == "" {
		return nil
	}
	return notify.NewClient(url, time.Duration(a.cfg.Settings.NtfyTimeoutSeconds)*time.Second)
}

func (a *app) fmtTS(ts int64) string {
	return time.Unix(ts, 0).In(a.cfg.Location).Format(time.RFC3339)
}
