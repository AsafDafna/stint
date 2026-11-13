package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"stint/internal/attribute"
	"stint/internal/config"
	"stint/internal/db"
	"stint/internal/ingest"
	"stint/internal/localday"
	"stint/internal/report"
	"stint/internal/rules"
	"stint/internal/rulesfile"
	"stint/internal/stretches"
	"stint/internal/totals"
)

// applyAnswers applies the pop-up answers decided since the last run, calls and stretches
// together, with one recompute. Answered stretch rows
// are validated and their "always" entries appended to rules.toml first (learnRules); a row
// that fails sits out with rules_error. Then one rebuild from the earliest remaining row's day
// honours the rows being applied, followed by totals, the pending refreshes and the weekly
// reports, and every remaining row is stamped applied. Log-and-continue: an error is returned
// for the run's error list and the rows stay unapplied for the next run, except a horizon miss,
// where nothing can be rebuilt (rules are still written) and the rows are stamped anyway.
func (a *app) applyAnswers() error {
	callRows, err := db.UnappliedCalls(a.sq)
	if err != nil {
		return fmt.Errorf("pop-up answers: %w", err)
	}
	stRows, err := db.UnappliedStretches(a.sq)
	if err != nil {
		return fmt.Errorf("pop-up answers: %w", err)
	}
	stRows = a.learnRules(stRows)
	if len(callRows) == 0 && len(stRows) == 0 {
		return nil
	}
	first := int64(1<<62 - 1)
	for _, c := range callRows {
		first = min(first, c.FromTS)
	}
	applying := map[int64]bool{}
	for _, s := range stRows {
		first = min(first, s.FromTS)
		applying[s.FromTS] = true
	}
	day := localday.Of(time.Unix(first, 0), a.cfg.Location)
	var reportErr error
	res, err := ingest.RecomputeApplying(a.sq, a.cfg, a.matcher, day, a.now, applying)
	switch {
	case errors.Is(err, db.ErrBeforeHorizon), errors.Is(err, ingest.ErrEmpty):
		a.logf("pop-up answers: %v; intervals left as they are", err)
	case err != nil:
		return fmt.Errorf("pop-up answers: %w", err)
	default:
		if err := totals.Recompute(a.sq, a.cfg.Location, a.cfg.FirstWeekday, a.now); err != nil {
			return fmt.Errorf("pop-up answers: totals: %w", err)
		}
		a.logf("applied %s: recomputed from %s (%d samples)", appliedText(len(callRows), len(stRows)), a.fmtTS(res.FromTS), res.Rows)
		p := ingest.Params(a.cfg, a.matcher, a.now)
		a.refreshPending(res.FromID, p, applying)
		written, werr := report.WriteWeeks(a.sq, a.cfg, p, day, a.now)
		for _, path := range written {
			a.logf("regenerated %s", path)
		}
		if werr != nil {
			reportErr = fmt.Errorf("pop-up answers: report: %w", werr) // intervals and totals are consistent: still mark applied
		}
	}
	for _, c := range callRows {
		if err := db.MarkApplied(a.sq, c.FromTS, c.DecidedAt, a.now.Unix()); err != nil {
			return fmt.Errorf("pop-up answers: %w", err)
		}
	}
	for _, s := range stRows {
		if err := db.MarkStretchApplied(a.sq, s.FromTS, s.DecidedAt, a.now.Unix()); err != nil {
			return fmt.Errorf("pop-up answers: %w", err)
		}
	}
	return reportErr
}

// appliedText is "1 call decision(s)", "2 stretch answer(s)" or both joined with "and".
func appliedText(calls, stretches int) string {
	var parts []string
	if calls > 0 {
		parts = append(parts, fmt.Sprintf("%d call decision(s)", calls))
	}
	if stretches > 0 {
		parts = append(parts, fmt.Sprintf("%d stretch answer(s)", stretches))
	}
	return strings.Join(parts, " and ")
}

// learnRules validates the answered rows among rows and appends their "always" entries to
// rules.toml in one write (one .bak), reloading the config after it. It
// returns the rows that go on to the recompute: re-opened rows (no answer: recomputed without
// one) and the answered rows that passed, with their rules_error cleared. A row that fails
// validation, the rules check, the write or the reload gets rules_error and is left out.
func (a *app) learnRules(rows []db.Stretch) []db.Stretch {
	var keep, answered []db.Stretch
	var groups []rulesfile.Group
	for _, s := range rows {
		if s.Answer == "" {
			keep = append(keep, s)
			continue
		}
		g, err := a.learnedGroup(s)
		if err != nil {
			a.failStretch(s, err)
			continue
		}
		answered = append(answered, s)
		groups = append(groups, g)
	}
	if len(answered) == 0 {
		return keep
	}
	old, err := os.ReadFile(a.cfgPath)
	if err != nil {
		for _, s := range answered {
			a.failStretch(s, err)
		}
		return keep
	}
	content, outcomes := rulesfile.Append(old, a.cfg.Home, groups)
	var writeErr error
	if !bytes.Equal(content, old) {
		if writeErr = rulesfile.Write(a.cfgPath, old, content); writeErr == nil {
			n := 0
			for _, o := range outcomes {
				n += o.Added
			}
			a.logf("rules.toml: appended %d learned rule(s); the previous file is rules.toml.bak", n)
			writeErr = a.reload()
		}
	}
	for i, s := range answered {
		switch {
		case outcomes[i].Err != nil:
			a.failStretch(s, outcomes[i].Err)
		case len(groups[i].Rules) > 0 && writeErr != nil:
			a.failStretch(s, writeErr) // also a rule deduped against an earlier row; the retry skips whatever already reached the file
		default:
			if s.RulesError != "" {
				if err := db.SetStretchRulesError(a.sq, s.FromTS, ""); err != nil {
					a.logf("stretch %s: clearing rules_error: %v", a.fmtTS(s.FromTS), err)
				}
			}
			keep = append(keep, s)
		}
	}
	return keep
}

// learnedGroup validates a stretch answer (every entry names a declared
// project or unattributed, and a well-formed sub-project) and returns its "always" entries.
// An "always" on a no_always key (the row's breakdown flag, or a key that is no_always by
// itself) is applied as a one-off: such a rule at the end of rules.toml would claim every
// later unmatched minute of a browser, a terminal or a dev server.
func (a *app) learnedGroup(s db.Stretch) (rulesfile.Group, error) {
	var items []attribute.AnswerItem
	if err := json.Unmarshal([]byte(s.Answer), &items); err != nil {
		return rulesfile.Group{}, fmt.Errorf("answer is not valid JSON: %w", err)
	}
	noAlways := map[string]bool{}
	var keys []stretches.KeySeconds
	if json.Unmarshal([]byte(s.Breakdown), &keys) == nil {
		for _, k := range keys {
			noAlways[k.Key] = k.NoAlways
		}
	}
	g := rulesfile.Group{
		Day:    a.now.In(a.cfg.Location).Format("2006-01-02"),
		Window: time.Unix(s.FromTS, 0).In(a.cfg.Location).Format("Mon 2 Jan 15:04"),
	}
	last := map[string]int{} // attribution keeps the last entry of a repeated key (ParseAnswer)
	for i, it := range items {
		last[it.Key] = i
	}
	for i, it := range items {
		if it.Project == attribute.Unattributed {
			continue
		}
		if _, ok := a.cfg.Projects[it.Project]; !ok {
			return g, fmt.Errorf("project %q is not in rules.toml", it.Project)
		}
		if it.Subproject != "" && !config.SubprojectRE.MatchString(it.Subproject) {
			return g, fmt.Errorf("sub-project %q must match %s", it.Subproject, config.SubprojectRE)
		}
		if !it.Always || last[it.Key] != i {
			continue
		}
		if noAlways[it.Key] || attribute.NoAlways(it.Key, nil) {
			a.logf("stretch %s: always ignored for %s (no_always key); applied as a one-off", a.fmtTS(s.FromTS), it.Key)
			continue
		}
		g.Rules = append(g.Rules, rulesfile.Learned{Key: it.Key, Project: it.Project, Subproject: it.Subproject, Label: it.Label})
	}
	return g, nil
}

// failStretch records why a row's answer was not applied; the run log gets a line only when
// the reason changes, since the row is retried on every run.
func (a *app) failStretch(s db.Stretch, err error) {
	if s.RulesError == err.Error() {
		return
	}
	a.logf("stretch %s: answer not applied: %v", a.fmtTS(s.FromTS), err)
	if serr := db.SetStretchRulesError(a.sq, s.FromTS, err.Error()); serr != nil {
		a.logf("stretch %s: recording rules_error: %v", a.fmtTS(s.FromTS), serr)
	}
}

// reload re-reads rules.toml after a learned-rules write, so everything after the apply step
// (recompute, ingest, detection, health, notify, reports) sees the learned rules.
func (a *app) reload() error {
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return err
	}
	a.cfg, a.matcher = cfg, rules.New(cfg)
	return nil
}
