// Package rules matches a sample's frontmost fields against the ordered rule list.
package rules

import (
	"slices"
	"strings"

	"stint/internal/config"
)

// Fields are the frontmost-window facts a rule can look at. "" means NULL.
type Fields struct {
	App   string
	Title string
	URL   string
	CWD   string
	TS    int64 // the sample's time, Unix seconds
}

// Match is the outcome of a successful rule match.
type Match struct {
	Project    string
	Subproject string // the rule's fixed subproject, else the cwd segment after cwd_prefix ("" on an exact prefix match)
	RuleID     int    // index of the rule in the file
}

// Matcher applies the ordered rules; first match wins.
type Matcher struct {
	rules []config.Rule
	home  string
}

// New builds a Matcher from a validated config (regexes already compiled, prefixes expanded).
// The rule list is copied so later changes to cfg.Rules do not reach the matcher.
func New(cfg *config.Config) *Matcher {
	return &Matcher{rules: slices.Clone(cfg.Rules), home: cfg.Home}
}

// Match returns the first rule whose present conditions all hold and whose date window, if
// any, holds f.TS. A regex condition never matches a NULL field, even a regex that accepts
// the empty string.
func (m *Matcher) Match(f Fields) (Match, bool) {
	cwd := config.ExpandHome(f.CWD, m.home)
	for i, r := range m.rules {
		if !r.Start.IsZero() && f.TS < r.Start.Unix() || !r.End.IsZero() && f.TS >= r.End.Unix() {
			continue
		}
		if r.App != "" && r.App != f.App {
			continue
		}
		if r.TitleRE != nil && (f.Title == "" || !r.TitleRE.MatchString(f.Title)) {
			continue
		}
		if r.URLRE != nil && (f.URL == "" || !r.URLRE.MatchString(f.URL)) {
			continue
		}
		sub := r.Subproject
		if r.CWDPrefix != "" {
			s, ok := Subproject(r.CWDPrefix, cwd)
			if !ok {
				continue
			}
			if sub == "" {
				sub = s
			}
		}
		return Match{Project: r.Project, Subproject: sub, RuleID: i}, true
	}
	return Match{}, false
}

// Subproject reports whether cwd is prefix or lies under it, and returns the
// path segment right after the prefix ("" on an exact match).
func Subproject(prefix, cwd string) (string, bool) {
	if cwd == prefix {
		return "", true
	}
	if !strings.HasPrefix(cwd, prefix+"/") {
		return "", false
	}
	rest := strings.TrimPrefix(cwd, prefix+"/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest, true
}
