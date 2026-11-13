// Package attribute turns an ordered slice of collector samples into project
// intervals. It is pure: no clock, no database.
package attribute

import (
	"sort"
	"strings"

	"stint/internal/rules"
)

// Unattributed is the project name used when no rule, carry-over or call applies.
const Unattributed = "unattributed"

// intervals.rule_id labels. A rule match stores the rule's index as a string ("3").
const (
	RuleCarryover    = "carryover"     // no match; previous project continued for < carryover_minutes
	RuleCall         = "call"          // in a call with no rule match; project carried over from before the call
	RuleUnattributed = "unattributed"  // nothing applied
	RuleCallProject  = "call:project"  // whole call booked to the project chosen in the pop-up
	RuleCallMeeting  = "call:meeting"  // split call, no rule match: booked to the call's guess
	RuleStretch      = "stretch"       // no rule match: booked by the stretch pop-up's answer
	RuleStretchLeave = "stretch:leave" // no rule match: left unattributed in the stretch pop-up
)

// calls.decision values.
const (
	DecisionProject    = "project"
	DecisionSplit      = "split"
	DecisionBackground = "background"
)

// Sample is one samples row. String fields use "" for SQL NULL. CallApp is the
// collector's comma-joined, deduplicated list of bundle ids holding the mic.
type Sample struct {
	ID      int64  // samples.id (aliases rowid; the ingest watermark)
	TS      int64  // Unix seconds, UTC
	Kind    string // sample|heartbeat|sleep|wake|resign|active|term
	App     string
	Title   string
	URL     string
	CWD     string
	IdleS   int64
	CallApp string
}

// Interval is one intervals row: [From, To) belongs to Project.
type Interval struct {
	From       int64
	To         int64
	Project    string
	Subproject string // "" = NULL
	RuleID     string // rule index as text ("3"), or RuleCarryover / RuleCall / RuleUnattributed
}

// Decision is a decided calls row. DecisionProject and DecisionSplit apply to every in-call
// block whose first span starts inside [FromTS, ToTS): for DecisionSplit, Project and
// Subproject hold the row's guess (where unmatched spans go); for DecisionProject, the
// chosen project. DecisionBackground applies to each sample whose TS is inside
// [FromTS, ToTS) (see BackgroundApps), with Project and Subproject empty.
type Decision struct {
	FromTS, ToTS int64
	Decision     string
	Project      string
	Subproject   string
	// For DecisionBackground: the call app BackgroundApp picked over the whole call, so every
	// rebuild chunk uses the same app. "" makes the decision cover nothing.
	BackgroundApp string
}

// Params are the settings the attribution needs, in seconds.
type Params struct {
	GapSeconds       int64
	IdleSeconds      int64
	CarryoverSeconds int64
	Now              int64 // caps the coverage of the last sample
	CallApps         map[string]bool
	Match            func(rules.Fields) (rules.Match, bool)
	Decisions        []Decision // decided calls rows overlapping the samples, any order
	Answers          []Answer   // applied stretch answers overlapping the samples, any order
}

// IsWorkRow reports whether a row carries frontmost fields (sample or heartbeat).
func IsWorkRow(kind string) bool { return kind == "sample" || kind == "heartbeat" }

// IsResetRow reports whether a boundary row ends everything: the interval, the
// carry-over and any call (sleep, crash/restart and session resign never credit time).
func IsResetRow(kind string) bool { return kind == "sleep" || kind == "resign" || kind == "term" }

// NonWorkApps are frontmost apps in which no work can happen: the lock screen and the
// screen saver. A sample/heartbeat row with such a frontmost app is treated exactly like
// an idle row (covers nothing, resets carry-over), except inside a call, where the idle
// rule is suspended.
var NonWorkApps = map[string]bool{
	"com.apple.loginwindow":        true,
	"com.apple.ScreenSaver.Engine": true,
}

// InCall reports whether any bundle id in a call_app list is in settings.call_apps.
func InCall(callApp string, callApps map[string]bool) bool {
	for _, id := range strings.Split(callApp, ",") {
		if callApps[strings.TrimSpace(id)] {
			return true
		}
	}
	return false
}

// CallKey canonicalizes a call_app list (sorted, deduplicated, trimmed) so two rows
// with the same set of mic holders compare equal whatever the collector's order.
func CallKey(callApp string) string {
	seen := map[string]bool{}
	var ids []string
	for _, id := range strings.Split(callApp, ",") {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func fields(s Sample) rules.Fields {
	return rules.Fields{App: s.App, Title: s.Title, URL: s.URL, CWD: s.CWD, TS: s.TS}
}

// decisionFor finds the decision whose half-open span [FromTS, ToTS) contains from: a
// block starting exactly where a call ended belongs to whatever follows, not to that call.
func decisionFor(from int64, ds []Decision) (Decision, bool) {
	for _, d := range ds {
		if d.FromTS <= from && from < d.ToTS {
			return d, true
		}
	}
	return Decision{}, false
}
