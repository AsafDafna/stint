package main

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"stint/internal/attribute"
	"stint/internal/calls"
	"stint/internal/db"
	"stint/internal/ingest"
	"stint/internal/localday"
	"stint/internal/report"
	"stint/internal/stretches"
	"stint/internal/totals"
)

// metaRecomputeSince marks a recompute in progress (YYYY-MM-DD); run resumes it.
const metaRecomputeSince = "recompute_since"

// errRecomputeReport marks a WriteWeeks failure that happens after intervals and totals
// are already consistent and the recompute marker has already been cleared. run's resume
// path treats this as non-fatal so a broken report directory (unmounted volume,
// permissions) cannot wedge every future run into replaying the whole recompute. The
// weekly catch-up (report.WriteMissing) later writes the weeks whose file is missing or
// not yet final; a week whose stale file is already final stays stale until
// `recompute --since` is rerun by hand.
var errRecomputeReport = errors.New("recompute report")

// recompute rebuilds intervals from --since (a local date), refreshes totals,
// regenerates every completed weekly report from that week on, and inserts pending rows
// for the calls and stretches closed by rows no run has ingested yet.
func (a *app) recompute(args []string) error {
	fs := flag.NewFlagSet("recompute", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	since := fs.String("since", "", "rebuild from this local date (YYYY-MM-DD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := noArgs("recompute", fs.Args()); err != nil {
		return err
	}
	if *since == "" {
		return fmt.Errorf("recompute: --since YYYY-MM-DD is required")
	}
	day, err := localday.Parse(*since, a.cfg.Location)
	if err != nil {
		return fmt.Errorf("recompute: --since must be YYYY-MM-DD, got %q", *since)
	}
	// The rebuild moves the watermark past rows no run has ingested yet, so the calls and
	// stretches those rows close are asked about here, as run would have. That holds after a
	// later step fails too: the run that resumes this recompute captures the moved watermark.
	p := ingest.Params(a.cfg, a.matcher, a.now)
	from, err := a.captureDetectStart(p)
	if err != nil {
		return err
	}
	err = a.recomputeFrom(day)
	if errors.Is(err, db.ErrBeforeHorizon) || errors.Is(err, ingest.ErrEmpty) {
		return err // nothing was rebuilt
	}
	return errors.Join(err, a.detectClosed(from, p))
}

// refreshPending refreshes the pending calls rows and the unanswered stretch rows a rebuild
// from fromID covered. The call guesses see the applied stretch answers and those in applying
// (an answered stretch changes the carry-over into the call that ended it), and the background
// hours see the decided calls. It never inserts (no pop-up storm after a rules edit) and only logs
// its failures: the rebuild succeeded.
func (a *app) refreshPending(fromID int64, p attribute.Params, applying map[int64]bool) {
	samples, err := db.ReadSamples(a.sq, fromID, 0)
	if err != nil {
		a.logf("reading samples for pending calls and stretches: %v", err)
		return
	}
	if len(samples) == 0 {
		return
	}
	if p.Answers, err = ingest.Answers(a.sq, samples[0].TS, 0, applying); err != nil {
		a.logf("reading stretch answers for pending calls: %v", err)
		return
	}
	if p.Decisions, err = ingest.Decisions(a.sq, samples[0].TS, 0, p); err != nil {
		a.logf("reading call decisions for pending calls: %v", err)
	} else if n, err := calls.Refresh(a.sq, samples, p); err != nil {
		a.logf("refreshing pending calls: %v", err)
	} else if n > 0 {
		a.logf("refreshed %d pending call(s)", n)
	}
	if n, deleted, err := stretches.Refresh(a.sq, samples, p); err != nil {
		a.logf("refreshing pending stretches: %v", err)
	} else if n+deleted > 0 {
		a.logf("refreshed %d pending stretch(es), dropped %d that rules now cover", n, deleted)
	}
}

// recomputeFrom persists the start day in meta before touching anything and clears it
// as soon as intervals and totals are consistent, so an interrupted recompute is
// finished by the next run and a failure past that point (the report step) never
// replays the rebuild.
func (a *app) recomputeFrom(day time.Time) error {
	if err := db.SetMeta(a.sq, metaRecomputeSince, day.Format("2006-01-02")); err != nil {
		return err
	}
	res, err := ingest.Recompute(a.sq, a.cfg, a.matcher, day, a.now)
	if err != nil {
		if errors.Is(err, db.ErrBeforeHorizon) || errors.Is(err, ingest.ErrEmpty) {
			// Nothing was changed; drop the marker so run does not retry forever. A failed
			// clear is reported on its own (without the sentinel) so run does not log the
			// marker as cleared.
			if cerr := db.DeleteMeta(a.sq, metaRecomputeSince); cerr != nil {
				return fmt.Errorf("recompute: %v; clearing the marker: %w", err, cerr)
			}
		}
		return fmt.Errorf("recompute: %w", err)
	}
	if err := totals.Recompute(a.sq, a.cfg.Location, a.cfg.FirstWeekday, a.now); err != nil {
		return fmt.Errorf("totals: %w", err)
	}
	if err := db.DeleteMeta(a.sq, metaRecomputeSince); err != nil {
		return err
	}
	a.logf("recomputed intervals and totals from %s (%d samples)", a.fmtTS(res.FromTS), res.Rows)
	p := ingest.Params(a.cfg, a.matcher, a.now)
	a.refreshPending(res.FromID, p, nil)
	// On a partial failure the weeks written before it are still reported.
	written, err := report.WriteWeeks(a.sq, a.cfg, p, day, a.now)
	for _, path := range written {
		a.logf("regenerated %s", path)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errRecomputeReport, err)
	}
	return nil
}
