package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"stint/internal/attribute"
	"stint/internal/db"
	"stint/internal/report"
	"stint/internal/rulesfile"
	"stint/internal/stretches"
)

// metaPicker is the meta key holding the stretch pop-up's picker (JSON array).
const metaPicker = "picker"

// writePicker publishes meta.picker for the collector's stretch panel.
func (a *app) writePicker() error {
	picker, err := report.Picker(a.sq, a.cfg, a.now)
	if err != nil {
		return err
	}
	js, err := json.Marshal(picker)
	if err != nil {
		return err
	}
	return db.SetMeta(a.sq, metaPicker, string(js))
}

// stretchesAfterIngest refreshes the unanswered stretch rows from fromID on and, with a
// watermark, inserts the stretches closed by rows newer than sinceTS. fromID and sinceTS
// come from captureDetectStart.
func (a *app) stretchesAfterIngest(fromID, sinceTS int64, haveWM bool, p attribute.Params) error {
	samples, err := db.ReadSamples(a.sq, fromID, 0)
	if err != nil {
		return err
	}
	// Both run even when Refresh fails: the watermark already moved, so skipping Detect loses these pop-ups.
	_, deleted, refreshErr := stretches.Refresh(a.sq, samples, p)
	if deleted > 0 {
		a.logf("dropped %d pending stretch(es) that rules now cover", deleted)
	}
	if !haveWM {
		return refreshErr
	}
	inserted, detectErr := stretches.Detect(a.sq, samples, p, sinceTS)
	for _, s := range inserted {
		a.logf("stretch %s–%s: %d min unattributed, awaiting the pop-up", a.fmtTS(s.FromTS), a.fmtTS(s.ToTS), (s.UnattributedS+30)/60)
	}
	return errors.Join(refreshErr, detectErr)
}

// stretchesDays: `stint stretches` lists rows from this many days back.
const stretchesDays = 30

// stretchesCmd: `stretches` lists recent rows and the learned rules; `stretches reopen
// <from_ts>` clears an answer so the panel asks again after the next run.
func (a *app) stretchesCmd(args []string) error {
	if len(args) == 0 {
		return a.listStretches()
	}
	if args[0] != "reopen" {
		return fmt.Errorf("stretches: unknown subcommand %q (usage: stretches | stretches reopen <from_ts>)", args[0])
	}
	if len(args) != 2 {
		return fmt.Errorf("stretches reopen: give one from_ts (see `stint stretches`)")
	}
	fromTS, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("stretches reopen: from_ts must be unix seconds, got %q", args[1])
	}
	_, found, err := db.ReadStretch(a.sq, fromTS)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("stretches reopen: no stretch starts at %d (see `stint stretches`)", fromTS)
	}
	changed, err := db.ReopenStretch(a.sq, fromTS, a.now.Unix())
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("stretches reopen: stretch %d has no answer to reopen", fromTS)
	}
	fmt.Fprintf(a.stdout, "stretch %s: re-opened; the panel asks again after the next run (learned rules stay in rules.toml)\n", a.fmtTS(fromTS))
	return nil
}

func (a *app) listStretches() error {
	rows, err := db.ReadStretches(a.sq, a.now.Unix()-stretchesDays*86400)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%-11s %-22s %5s  %-40s %s\n", "from_ts", "window", "unatt", "top keys", "state")
	for _, s := range rows {
		from, to := time.Unix(s.FromTS, 0).In(a.cfg.Location), time.Unix(s.ToTS, 0).In(a.cfg.Location)
		window := from.Format("Mon 2 Jan 15:04") + "–" + to.Format("15:04")
		unatt := fmt.Sprintf("%dm", (s.UnattributedS+30)/60)
		fmt.Fprintf(a.stdout, "%-11d %-22s %5s  %-40s %s\n", s.FromTS, window, unatt, topKeys(s.Breakdown, 3), stretchState(s))
	}
	if len(rows) == 0 {
		fmt.Fprintf(a.stdout, "no stretches in the last %d days\n", stretchesDays)
	}
	fmt.Fprintln(a.stdout)
	return a.listLearned()
}

// topKeys is the first n breakdown keys as "grok.com, net.whatsapp.WhatsApp (app)".
func topKeys(breakdown string, n int) string {
	var keys []stretches.KeySeconds
	if json.Unmarshal([]byte(breakdown), &keys) != nil {
		return "?"
	}
	var out []string
	for _, k := range keys[:min(n, len(keys))] {
		if host, ok := strings.CutPrefix(k.Key, attribute.KeyHost); ok {
			out = append(out, host)
		} else {
			out = append(out, strings.TrimPrefix(k.Key, attribute.KeyApp)+" (app)")
		}
	}
	return strings.Join(out, ", ")
}

func stretchState(s db.Stretch) string {
	switch {
	case s.RulesError != "":
		return "error: " + s.RulesError
	case s.Answer == "" && s.DecidedAt == 0:
		return "pending"
	case s.Answer == "":
		return "re-opened"
	case s.AppliedAt < s.DecidedAt:
		return "answered"
	}
	return "applied"
}

// listLearned prints every learned rule with its hours over the last 7 and 30 days, the way
// to spot a wrong "always". Rule indexes are re-derived from the file every call.
func (a *app) listLearned() error {
	content, err := os.ReadFile(a.cfgPath)
	if err != nil {
		return err
	}
	markers, headers := rulesfile.ReadMarkers(content)
	switch {
	case headers != len(a.cfg.Rules):
		fmt.Fprintln(a.stdout, "learned rules: cannot map rule indexes")
		return nil
	case len(markers) == 0:
		fmt.Fprintln(a.stdout, "learned rules: none")
		return nil
	}
	type line struct{ label, target, d7, d30 string }
	lines := []line{{"learned rule", "project", "7d", "30d"}}
	now := a.now.Unix()
	for _, m := range markers {
		r := a.cfg.Rules[m.Index]
		label, target := m.Label, r.Project
		if r.App != "" {
			label += " (app)"
		}
		if r.Subproject != "" {
			target += "/" + r.Subproject
		}
		var hours [2]string
		for i, days := range []int64{7, 30} {
			sec, err := db.RuleSeconds(a.sq, strconv.Itoa(m.Index), now-days*86400, now)
			if err != nil {
				return err
			}
			hours[i] = report.Hours(sec) + "h"
		}
		lines = append(lines, line{label, target, hours[0], hours[1]})
	}
	lw, tw := 0, 0
	for _, l := range lines {
		lw, tw = max(lw, len([]rune(l.label))), max(tw, len([]rune(l.target)))
	}
	for _, l := range lines {
		fmt.Fprintf(a.stdout, "%-*s → %-*s  %5s  %5s\n", lw, l.label, tw, l.target, l.d7, l.d30)
	}
	return nil
}
