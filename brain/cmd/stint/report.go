package main

import (
	"flag"
	"fmt"

	"stint/internal/ingest"
	"stint/internal/localday"
	"stint/internal/report"
	"stint/internal/totals"
)

// report writes and prints the weekly markdown for the week containing --week.
func (a *app) report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	week := fs.String("week", "", "any date inside the week (YYYY-MM-DD, local)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := noArgs("report", fs.Args()); err != nil {
		return err
	}
	if *week == "" {
		return fmt.Errorf("report: --week YYYY-MM-DD is required")
	}
	day, err := localday.Parse(*week, a.cfg.Location)
	if err != nil {
		return fmt.Errorf("report: --week must be YYYY-MM-DD, got %q", *week)
	}
	p := ingest.Params(a.cfg, a.matcher, a.now)
	path, md, err := report.Write(a.sq, a.cfg, p, totals.WeekStart(day, a.cfg.Location, a.cfg.FirstWeekday))
	if err != nil {
		return err
	}
	fmt.Fprint(a.stdout, md)
	// stdout carries the markdown (pipeable), so the status line goes to stderr; every
	// other command logs through logf.
	fmt.Fprintf(a.stderr, "written %s\n", path)
	return nil
}
