package rules

import (
	"testing"
	"time"

	"stint/internal/config"
)

const rulesTOML = `
[settings]
vault_reports_dir = "~/stint-reports"

[projects.acme]
weekly_cap_hours = 30
[projects.phd]
[projects.personal-dev]

[[rules]]
project = "acme"
cwd_prefix = "~/dev-projects/acme"

[[rules]]
project = "acme"
app = "com.tinyspeck.slackmacgap"

[[rules]]
project = "acme"
url_regex = 'make\.com/100000/|monday\.com/'

[[rules]]
project = "phd"
app = "com.bookry.wavebox"
title_regex = '(?i)overleaf'

[[rules]]
project = "personal-dev"
cwd_prefix = "~/dev-projects"

[[rules]]
project = "phd"
app = "com.apple.Preview"
title_regex = '.*'
`

func matcher(t *testing.T) *Matcher {
	t.Helper()
	cfg, err := config.Parse([]byte(rulesTOML), "/Users/me")
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg)
}

func TestMatchTable(t *testing.T) {
	m := matcher(t)
	cases := []struct {
		name string
		in   Fields
		want Match
		ok   bool
	}{
		{"cwd prefix with subproject", Fields{App: "com.mitchellh.ghostty", CWD: "/Users/me/dev-projects/acme/automations/src"}, Match{"acme", "automations", 0}, true},
		{"cwd exact prefix -> NULL subproject", Fields{CWD: "/Users/me/dev-projects/acme"}, Match{"acme", "", 0}, true},
		{"cwd with ~ on the sample side", Fields{CWD: "~/dev-projects/acme/webapp"}, Match{"acme", "webapp", 0}, true},
		{"cwd sibling prefix does not match segment-wise", Fields{CWD: "/Users/me/dev-projects/acme-old/x"}, Match{"personal-dev", "acme-old", 4}, true},
		{"first match wins over later cwd rule", Fields{CWD: "/Users/me/dev-projects/acme/x"}, Match{"acme", "x", 0}, true},
		{"app exact bundle id", Fields{App: "com.tinyspeck.slackmacgap", Title: "anything"}, Match{"acme", "", 1}, true},
		{"app substring is not a match", Fields{App: "com.tinyspeck.slackmacgap.helper"}, Match{}, false},
		{"url regex", Fields{App: "com.bookry.wavebox", URL: "https://eu2.make.com/100000/scenarios/1/edit"}, Match{"acme", "", 2}, true},
		{"all conditions must hold: app ok, title no", Fields{App: "com.bookry.wavebox", Title: "Gmail"}, Match{}, false},
		{"all conditions must hold: title ok, app no", Fields{App: "com.google.Chrome", Title: "Overleaf, Online LaTeX"}, Match{}, false},
		{"both hold", Fields{App: "com.bookry.wavebox", Title: "Overleaf, Online LaTeX"}, Match{"phd", "", 3}, true},
		{"leftover dev-projects -> personal-dev subproject", Fields{CWD: "/Users/me/dev-projects/stint/brain"}, Match{"personal-dev", "stint", 4}, true},
		{"no fields, no match", Fields{}, Match{}, false},
		{"empty url never matches a url rule", Fields{App: "com.google.Chrome"}, Match{}, false},
		{"NULL title never matches a title regex, even one accepting the empty string", Fields{App: "com.apple.Preview"}, Match{}, false},
		{"present title matches that regex", Fields{App: "com.apple.Preview", Title: "paper.pdf"}, Match{"phd", "", 5}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := m.Match(c.in)
			if ok != c.ok || got != c.want {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestSubproject(t *testing.T) {
	cases := []struct {
		prefix, cwd, want string
		ok                bool
	}{
		{"/a/b", "/a/b", "", true},
		{"/a/b", "/a/b/c", "c", true},
		{"/a/b", "/a/b/c/d/e", "c", true},
		{"/a/b", "/a/bc", "", false},
		{"/a/b", "/a", "", false},
		{"/a/b", "", "", false},
	}
	for _, c := range cases {
		got, ok := Subproject(c.prefix, c.cwd)
		if got != c.want || ok != c.ok {
			t.Errorf("Subproject(%q,%q) = %q,%v want %q,%v", c.prefix, c.cwd, got, ok, c.want, c.ok)
		}
	}
}

// New must snapshot the rule list: a later mutation of cfg.Rules must not leak into the matcher.
func TestNewCopiesRules(t *testing.T) {
	cfg, err := config.Parse([]byte(rulesTOML), "/Users/me")
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg)
	cfg.Rules[1].Project = "phd"
	got, ok := m.Match(Fields{App: "com.tinyspeck.slackmacgap"})
	if !ok || got.Project != "acme" {
		t.Fatalf("matcher aliases cfg.Rules: got %+v ok=%v, want acme", got, ok)
	}
}

const datedTOML = `
[settings]
vault_reports_dir = "~/stint-reports"

[projects.freelance]
[projects.personal-dev]

[[rules]]
project = "personal-dev"
app = "com.example.Notes"

[[rules]]
project = "freelance"
cwd_prefix = "~/dev-projects/freelance"
subproject = "globex"
from = "2026-11-08"
until = "2026-11-08"

[[rules]]
project = "freelance"
cwd_prefix = "~/dev-projects/freelance"

[[rules]]
project = "personal-dev"
cwd_prefix = "~/dev-projects"

[[rules]]
project = "personal-dev"
app = "com.example.Old"
until = "2026-11-08"
`

// November is DST-free in Asia/Jerusalem (UTC+2), and near local midnight the UTC date is
// the other day, so these cases show that the local date decides.
func TestMatchDateWindow(t *testing.T) {
	cfg, err := config.Parse([]byte(datedTOML), "/Users/me")
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg)
	at := func(day, hour, min, sec int) int64 {
		return time.Date(2026, 11, day, hour, min, sec, 0, cfg.Location).Unix()
	}
	const prefix = "/Users/me/dev-projects/freelance"
	cases := []struct {
		name string
		in   Fields
		want Match
		ok   bool
	}{
		{"day before, 23:30 local", Fields{CWD: prefix, TS: at(7, 23, 30, 0)}, Match{"freelance", "", 2}, true},
		{"00:30 local, UTC still the day before", Fields{CWD: prefix, TS: at(8, 0, 30, 0)}, Match{"freelance", "globex", 1}, true},
		{"first second of the day", Fields{CWD: prefix, TS: at(8, 0, 0, 0)}, Match{"freelance", "globex", 1}, true},
		{"last second of the day", Fields{CWD: prefix, TS: at(8, 23, 59, 59)}, Match{"freelance", "globex", 1}, true},
		{"day after, local midnight", Fields{CWD: prefix, TS: at(9, 0, 0, 0)}, Match{"freelance", "", 2}, true},
		{"01:00 local, UTC still inside the day", Fields{CWD: prefix, TS: at(9, 1, 0, 0)}, Match{"freelance", "", 2}, true},
		{"fixed subproject replaces the cwd segment", Fields{CWD: prefix + "/site/src", TS: at(8, 12, 0, 0)}, Match{"freelance", "globex", 1}, true},
		{"outside the window the cwd names it", Fields{CWD: prefix + "/site/src", TS: at(9, 12, 0, 0)}, Match{"freelance", "site", 2}, true},
		{"an earlier rule wins over a dated one", Fields{App: "com.example.Notes", CWD: prefix, TS: at(8, 12, 0, 0)}, Match{"personal-dev", "", 0}, true},
		{"open start: long before until", Fields{App: "com.example.Old", TS: time.Date(2020, 1, 1, 12, 0, 0, 0, cfg.Location).Unix()}, Match{"personal-dev", "", 4}, true},
		{"open start: until's last second", Fields{App: "com.example.Old", TS: at(8, 23, 59, 59)}, Match{"personal-dev", "", 4}, true},
		{"open start: after until", Fields{App: "com.example.Old", TS: at(9, 0, 0, 0)}, Match{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := m.Match(c.in)
			if ok != c.ok || got != c.want {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestMatchFixedSubproject(t *testing.T) {
	cfg, err := config.Parse([]byte(rulesTOML+"\n[[rules]]\nproject = \"personal-dev\"\nurl_regex = 'grok\\.com'\nsubproject = \"budget-tracker\"\n"), "/Users/me")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := New(cfg).Match(Fields{App: "com.google.Chrome", URL: "https://grok.com/c/1"})
	if !ok || got != (Match{"personal-dev", "budget-tracker", 6}) {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}
