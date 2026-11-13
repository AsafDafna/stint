package main

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/ingest"
	"stint/internal/localday"
	"stint/internal/report"
	"stint/internal/rules"
)

const explainTopN = 10

// explain prints the sample and interval at a moment, or yesterday's unattributed list.
func (a *app) explain(args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	yesterday := fs.Bool("yesterday", false, "list yesterday's top unattributed titles/URLs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *yesterday {
		if err := noArgs("explain", fs.Args()); err != nil {
			return err
		}
		return a.explainYesterday()
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("explain: give one moment (unix seconds, RFC3339 or YYYY-MM-DDTHH:MM:SS local) or --yesterday")
	}
	t, err := parseMoment(fs.Arg(0), a.cfg.Location)
	if err != nil {
		return err
	}
	return a.explainAt(t)
}

// parseMoment accepts unix seconds, RFC3339, or a bare YYYY-MM-DDTHH:MM:SS read in loc.
func parseMoment(s string, loc *time.Location) (time.Time, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("explain: cannot parse %q as unix seconds, RFC3339 or YYYY-MM-DDTHH:MM:SS (local)", s)
}

// checkHorizon fails with db.ErrBeforeHorizon when [from, to) lies entirely before the
// oldest retained sample. For a single moment pass to = from.
func (a *app) checkHorizon(from, to time.Time) error {
	oldest, ok, err := db.OldestSampleTS(a.sq)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no samples yet")
	}
	if from.Unix() < oldest && to.Unix() <= oldest {
		return fmt.Errorf("%w: oldest sample is at %s", db.ErrBeforeHorizon, a.fmtTS(oldest))
	}
	return nil
}

func (a *app) explainAt(t time.Time) error {
	if err := a.checkHorizon(t, t); err != nil {
		return err
	}
	s, ok, err := db.LastSampleBefore(a.sq, t.Unix())
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(a.stdout, "no sample at or before %s\n", a.fmtTS(t.Unix()))
		return nil
	}
	fmt.Fprintf(a.stdout, "sample #%d at %s (kind=%s)\n", s.ID, a.fmtTS(s.TS), s.Kind)
	for _, kv := range [][2]string{{"app", s.App}, {"title", s.Title}, {"url", s.URL}, {"cwd", s.CWD}, {"idle_s", strconv.FormatInt(s.IdleS, 10)}, {"call_app", s.CallApp}} {
		v := kv[1]
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(a.stdout, "  %-8s %s\n", kv[0], v)
	}
	if attribute.IsWorkRow(s.Kind) {
		if m, ok := a.matcher.Match(rules.Fields{App: s.App, Title: s.Title, URL: s.URL, CWD: s.CWD, TS: s.TS}); ok {
			fmt.Fprintf(a.stdout, "  matches  rule %d: %s\n", m.RuleID, describeRule(a.cfg.Rules[m.RuleID]))
		} else {
			fmt.Fprintln(a.stdout, "  matches  no rule")
		}
		if attribute.InCall(s.CallApp, a.cfg.CallAppSet()) {
			bg, err := a.inBackgroundCall(s)
			if err != nil {
				return err
			}
			if bg {
				fmt.Fprintln(a.stdout, "  in call  yes, answered background (idle counts as away)")
			} else {
				fmt.Fprintln(a.stdout, "  in call  yes (idle ignored, project fixed per call)")
			}
		}
	}
	iv, ok, err := db.IntervalAt(a.sq, t.Unix())
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(a.stdout, "interval: none covers %s (not working, or not yet ingested)\n", a.fmtTS(t.Unix()))
		return nil
	}
	sub := iv.Subproject
	if sub == "" {
		sub = "-"
	}
	fmt.Fprintf(a.stdout, "interval %s – %s project=%s subproject=%s rule_id=%s\n", a.fmtTS(iv.From), a.fmtTS(iv.To), iv.Project, sub, iv.RuleID)
	switch iv.RuleID {
	case attribute.RuleCarryover:
		fmt.Fprintf(a.stdout, "  carried over from the previous %s activity (< %d min ago)\n", iv.Project, a.cfg.Settings.CarryoverMinutes)
	case attribute.RuleCall:
		fmt.Fprintf(a.stdout, "  call with no rule match; carried over from the %s activity that ended < %d min before it\n", iv.Project, a.cfg.Settings.CarryoverMinutes)
	case attribute.RuleUnattributed:
		fmt.Fprintln(a.stdout, "  no rule, no carry-over")
	case attribute.RuleCallProject:
		fmt.Fprintln(a.stdout, "  whole call booked to this project by the meeting pop-up (stint calls)")
	case attribute.RuleCallMeeting:
		fmt.Fprintln(a.stdout, "  split call, no rule match: booked to the call's guessed project")
	case attribute.RuleStretch:
		fmt.Fprintln(a.stdout, "  booked by the stretch pop-up (stint stretches)")
	case attribute.RuleStretchLeave:
		fmt.Fprintln(a.stdout, "  left unattributed in the stretch pop-up")
	default:
		if n, err := strconv.Atoi(iv.RuleID); err == nil && n >= 0 && n < len(a.cfg.Rules) {
			fmt.Fprintf(a.stdout, "  claimed by rule %d: %s\n", n, describeRule(a.cfg.Rules[n]))
		} else {
			fmt.Fprintf(a.stdout, "  rule_id %q is not in the current rules file (run recompute?)\n", iv.RuleID)
		}
	}
	return nil
}

func (a *app) inBackgroundCall(s attribute.Sample) (bool, error) {
	p := ingest.Params(a.cfg, a.matcher, a.now)
	ds, err := ingest.Decisions(a.sq, s.TS, s.TS+1, p)
	if err != nil {
		return false, err
	}
	for _, d := range ds {
		if d.Decision != attribute.DecisionBackground || s.TS < d.FromTS || s.TS >= d.ToTS {
			continue
		}
		samples, err := db.ReadSamplesBetween(a.sq, d.FromTS, d.ToTS)
		if err != nil {
			return false, err
		}
		p.Decisions = []attribute.Decision{d}
		for i, app := range attribute.BackgroundApps(samples, p) {
			if samples[i].ID == s.ID && app != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *app) explainYesterday() error {
	to := localday.Of(a.now, a.cfg.Location)
	from := localday.Add(to, -1)
	if err := a.checkHorizon(from, to); err != nil {
		return err
	}
	p := ingest.Params(a.cfg, a.matcher, a.now)
	top, err := report.TopUnattributed(a.sq, p, from.Unix(), to.Unix(), explainTopN)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "unattributed on %s:\n", from.Format("2006-01-02"))
	if len(top) == 0 {
		fmt.Fprintln(a.stdout, "  nothing")
	}
	for _, e := range top {
		fmt.Fprintf(a.stdout, "  %5sh  %s\n", report.Hours(e.Seconds), e.Label)
	}
	return nil
}

// describeRule renders a rule's conditions, e.g. `project=acme url_regex=make\.com/`.
func describeRule(r config.Rule) string {
	s := "project=" + r.Project
	if r.Subproject != "" {
		s += " subproject=" + r.Subproject
	}
	if r.App != "" {
		s += " app=" + r.App
	}
	if r.TitleRegex != "" {
		s += " title_regex=" + r.TitleRegex
	}
	if r.URLRegex != "" {
		s += " url_regex=" + r.URLRegex
	}
	if r.CWDPrefix != "" {
		s += " cwd_prefix=" + r.CWDPrefix
	}
	if r.From != "" {
		s += " from=" + r.From
	}
	if r.Until != "" {
		s += " until=" + r.Until
	}
	return s
}
