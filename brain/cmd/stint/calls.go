package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"stint/internal/attribute"
	"stint/internal/calls"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/ingest"
)

// metaProjects is the meta key holding the sorted project names for the collector's picker.
const metaProjects = "projects"

// detectCalls inserts pending rows for the calls closed by rows newer than sinceTS and
// logs each one. fromID and sinceTS come from captureDetectStart.
// The guesses see the applied stretch answers, as the call's intervals do: a call that
// ended an answered stretch carries over from the answered project. The background hours
// also see the other decided calls, as the books do.
func (a *app) detectCalls(fromID, sinceTS int64, p attribute.Params) error {
	samples, err := db.ReadSamples(a.sq, fromID, 0)
	if err != nil || len(samples) == 0 {
		return err
	}
	// Unreadable answers or decisions still let Detect run (guesses without the carry-over,
	// background hours without the other calls' answers): the watermark already moved, so
	// skipping it would lose these pop-ups.
	answers, answersErr := ingest.Answers(a.sq, samples[0].TS, 0, nil)
	if answersErr == nil {
		p.Answers = answers
	}
	decisions, decisionsErr := ingest.Decisions(a.sq, samples[0].TS, 0, p)
	if decisionsErr == nil {
		p.Decisions = decisions
	}
	inserted, err := calls.Detect(a.sq, samples, p, sinceTS)
	for _, c := range inserted {
		guess := c.GuessProject
		if c.GuessSubproject != "" {
			guess += "/" + c.GuessSubproject
		}
		a.logf("call %s–%s (%s) guessed %s: awaiting the pop-up", a.fmtTS(c.FromTS), a.fmtTS(c.ToTS), c.CallApp, guess)
	}
	return errors.Join(answersErr, decisionsErr, err)
}

// callsDays: `stint calls` lists rows from this many days back.
const callsDays = 30

// callsCmd: `calls` lists recent rows; `calls set <from_ts> --project NAME[/SUB] | --split |
// --background` records an answer that the next run applies.
func (a *app) callsCmd(args []string) error {
	if len(args) == 0 {
		return a.listCalls()
	}
	if args[0] != "set" {
		return fmt.Errorf("calls: unknown subcommand %q (usage: calls | calls set <from_ts> --project NAME[/SUB] | --split | --background)", args[0])
	}
	// The from_ts comes before the flags (Go's flag package stops at the first non-flag).
	if len(args) < 2 {
		return fmt.Errorf("calls set: give a from_ts (see `stint calls`)")
	}
	fromTS, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("calls set: from_ts must be unix seconds, got %q", args[1])
	}
	fs := flag.NewFlagSet("calls set", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	project := fs.String("project", "", "book the whole call to this project or project/sub-project")
	split := fs.Bool("split", false, "book each sample by its own rule; unmatched minutes go to the guess")
	background := fs.Bool("background", false, "count only what was done; idle time during the call is away")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := noArgs("calls set", fs.Args()); err != nil {
		return err
	}
	answers := 0
	for _, given := range []bool{*project != "", *split, *background} {
		if given {
			answers++
		}
	}
	if answers != 1 {
		return fmt.Errorf("calls set: give exactly one of --project NAME, --split or --background")
	}
	c, found, err := db.ReadCall(a.sq, fromTS)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("calls set: no call starts at %d (see `stint calls`)", fromTS)
	}
	decision := attribute.DecisionSplit
	if *background {
		decision = attribute.DecisionBackground
	}
	name, sub, hasSub := strings.Cut(*project, "/")
	if *project != "" {
		if _, ok := a.cfg.Projects[name]; !ok {
			return fmt.Errorf("calls set: project %q is not in the rules file", name)
		}
		if hasSub && !config.SubprojectRE.MatchString(sub) {
			return fmt.Errorf("calls set: sub-project %q must match %s", sub, config.SubprojectRE)
		}
		decision = attribute.DecisionProject
		if !hasSub && name == c.GuessProject {
			sub = c.GuessSubproject // the bare guess project accepts the guess, sub-project included
		}
	}
	if _, err := db.SetDecision(a.sq, fromTS, decision, name, sub, a.now.Unix()); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "call %s: %s; applied on the next run\n", a.fmtTS(fromTS), describeDecision(decision, name, sub))
	return nil
}

func (a *app) listCalls() error {
	rows, err := db.ReadCalls(a.sq, a.now.Unix()-callsDays*86400)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%-11s %-20s %5s  %-26s %-24s %s\n", "from_ts", "start", "dur", "call_app", "guess", "decision")
	// dur is pre-formatted ("57m") so header and rows share one %5s width.
	for _, c := range rows {
		guess := c.GuessProject
		if c.GuessSubproject != "" {
			guess += "/" + c.GuessSubproject
		}
		decision := "pending"
		if c.Decision != "" {
			decision = describeDecision(c.Decision, c.Project, c.Subproject)
			if c.AppliedAt < c.DecidedAt {
				decision += " (not yet applied)"
			}
		}
		dur := fmt.Sprintf("%dm", (c.ToTS-c.FromTS+30)/60)
		fmt.Fprintf(a.stdout, "%-11d %-20s %5s  %-26s %-24s %s\n", c.FromTS, a.fmtTS(c.FromTS), dur, c.CallApp, guess, decision)
	}
	if len(rows) == 0 {
		fmt.Fprintf(a.stdout, "no calls in the last %d days\n", callsDays)
	}
	return nil
}

func describeDecision(decision, project, sub string) string {
	switch decision {
	case attribute.DecisionSplit:
		return "split"
	case attribute.DecisionBackground:
		return "background"
	}
	if sub != "" {
		return "whole call to " + project + "/" + sub
	}
	return "whole call to " + project
}
