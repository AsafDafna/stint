// Package config loads and validates ~/.config/stint/rules.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"stint/internal/localday"
)

// Settings mirrors the [settings] table. Unknown keys are rejected on load.
type Settings struct {
	IdleMinutes        int      `toml:"idle_minutes"`
	CarryoverMinutes   int      `toml:"carryover_minutes"`
	GapMinutes         int      `toml:"gap_minutes"`
	RawRetentionDays   int      `toml:"raw_retention_days"`
	NtfyTopic          string   `toml:"ntfy_topic"`
	NtfyURL            string   `toml:"ntfy_url"`
	NtfyTimeoutSeconds int      `toml:"ntfy_timeout_seconds"`
	WeekStart          string   `toml:"week_start"`
	VaultReportsDir    string   `toml:"vault_reports_dir"`
	CallApps           []string `toml:"call_apps"`
	ActiveHours        string   `toml:"active_hours"`
	TuneUntil          string   `toml:"tune_until"`
}

// Project mirrors one [projects.<name>] table.
type Project struct {
	WeeklyCapHours float64 `toml:"weekly_cap_hours"`
	Paid           bool    `toml:"paid"`
}

// Rule mirrors one [[rules]] entry; conditions left empty are not checked.
type Rule struct {
	Project    string `toml:"project"`
	Subproject string `toml:"subproject"` // fixed sub-project; with cwd_prefix it replaces the one the cwd names
	App        string `toml:"app"`
	TitleRegex string `toml:"title_regex"`
	URLRegex   string `toml:"url_regex"`
	CWDPrefix  string `toml:"cwd_prefix"`
	From       string `toml:"from"`  // YYYY-MM-DD local, inclusive
	Until      string `toml:"until"` // YYYY-MM-DD local, inclusive

	TitleRE *regexp.Regexp `toml:"-"` // compiled TitleRegex, nil when unset
	URLRE   *regexp.Regexp `toml:"-"` // compiled URLRegex, nil when unset
	Start   time.Time      `toml:"-"` // local midnight of From; zero when unset
	End     time.Time      `toml:"-"` // local midnight after Until, exclusive; zero when unset
}

// Config is the validated rules file plus values derived from it.
type Config struct {
	Settings Settings           `toml:"settings"`
	Projects map[string]Project `toml:"projects"`
	Rules    []Rule             `toml:"rules"`

	Location     *time.Location `toml:"-"` // time.Local, the Mac's system timezone
	FirstWeekday time.Weekday   `toml:"-"` // Settings.WeekStart, parsed
	Home         string         `toml:"-"` // os.UserHomeDir at load time
	ActiveFrom   int            `toml:"-"` // minutes after local midnight
	ActiveUntil  int            `toml:"-"` // minutes after local midnight, exclusive
	TuneUntil    time.Time      `toml:"-"` // zero when unset; local midnight of the day after tune_until
}

// SubprojectRE is the name pattern for a rule's fixed sub-project and for sub-projects named in the
// stretch pop-up.
var SubprojectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// DefaultPath is the rules file location when --config is not given.
func DefaultPath(home string) string { return filepath.Join(home, ".config", "stint", "rules.toml") }

// Load reads and validates the rules file at path.
func Load(path string) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, home)
}

// Parse validates TOML bytes; home is used for ~ expansion.
func Parse(data []byte, home string) (*Config, error) {
	cfg := &Config{
		Settings: Settings{
			IdleMinutes:        5,
			CarryoverMinutes:   10,
			GapMinutes:         2,
			RawRetentionDays:   90,
			NtfyTimeoutSeconds: 10,
			WeekStart:          "sunday",
			ActiveHours:        "07:00-23:00",
		},
		Location: time.Local,
		Home:     home,
	}
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("rules.toml: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			if k.String() == "settings.timezone" {
				return nil, fmt.Errorf("rules.toml: settings.timezone is no longer supported: stint uses the Mac's system timezone; delete the timezone line")
			}
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("rules.toml: unknown keys: %s", strings.Join(keys, ", "))
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("rules.toml: %w", err)
	}
	return cfg, nil
}

func (c *Config) validate() error {
	s := &c.Settings
	// The collector writes the idle row when idle crosses 300 s (collector/schema.sql);
	// the brain's back-dating assumes the same threshold, so the value is pinned.
	if s.IdleMinutes != 5 {
		return fmt.Errorf("settings.idle_minutes must be 5 (the collector's 300 s idle threshold, see collector/schema.sql), got %d", s.IdleMinutes)
	}
	// A slice, not a map: the first offending setting must be named deterministically.
	for _, setting := range []struct {
		name  string
		value int
	}{
		{"carryover_minutes", s.CarryoverMinutes},
		{"gap_minutes", s.GapMinutes},
		{"raw_retention_days", s.RawRetentionDays},
		{"ntfy_timeout_seconds", s.NtfyTimeoutSeconds},
	} {
		if setting.value <= 0 {
			return fmt.Errorf("settings.%s must be > 0", setting.name)
		}
	}
	// ntfy caps topics at 64 characters; a "/" would change the endpoint path.
	if len(s.NtfyTopic) > 64 || strings.Contains(s.NtfyTopic, "/") {
		return fmt.Errorf("settings.ntfy_topic must be at most 64 characters with no \"/\"")
	}
	var ok bool
	if c.FirstWeekday, ok = parseWeekday(s.WeekStart); !ok {
		return fmt.Errorf("settings.week_start: want a weekday name (\"sunday\" ... \"saturday\"), got %q", s.WeekStart)
	}
	if s.VaultReportsDir == "" {
		return fmt.Errorf("settings.vault_reports_dir is required")
	}
	s.VaultReportsDir = ExpandHome(s.VaultReportsDir, c.Home)
	var err error
	if c.ActiveFrom, c.ActiveUntil, err = parseActiveHours(s.ActiveHours); err != nil {
		return err
	}
	if s.TuneUntil != "" {
		d, err := localday.Parse(s.TuneUntil, c.Location)
		if err != nil {
			return fmt.Errorf("settings.tune_until: want YYYY-MM-DD, got %q", s.TuneUntil)
		}
		c.TuneUntil = localday.Add(d, 1)
	}
	if c.Projects == nil {
		c.Projects = map[string]Project{}
	}
	// The values of attribute.Unattributed and attribute.AwayLine (config cannot import attribute).
	for _, name := range []string{"unattributed", "away"} {
		if _, clash := c.Projects[name]; clash {
			return fmt.Errorf("projects: %q is reserved", name)
		}
	}
	if len(c.Rules) == 0 {
		return fmt.Errorf("at least one [[rules]] entry is required")
	}
	for i, r := range c.Rules {
		if r.Project == "" {
			return fmt.Errorf("rules[%d]: project is required", i)
		}
		if _, ok := c.Projects[r.Project]; !ok {
			return fmt.Errorf("rules[%d]: project %q is not declared under [projects]", i, r.Project)
		}
		if r.App == "" && r.TitleRegex == "" && r.URLRegex == "" && r.CWDPrefix == "" {
			return fmt.Errorf("rules[%d]: needs at least one of app, title_regex, url_regex, cwd_prefix", i)
		}
		if r.Subproject != "" && !SubprojectRE.MatchString(r.Subproject) {
			return fmt.Errorf("rules[%d]: subproject %q must match %s", i, r.Subproject, SubprojectRE)
		}
		var err error
		if r.From != "" {
			if c.Rules[i].Start, err = localday.Parse(r.From, c.Location); err != nil {
				return fmt.Errorf("rules[%d]: from: want YYYY-MM-DD, got %q", i, r.From)
			}
		}
		if r.Until != "" {
			if c.Rules[i].End, err = localday.Parse(r.Until, c.Location); err != nil {
				return fmt.Errorf("rules[%d]: until: want YYYY-MM-DD, got %q", i, r.Until)
			}
			if c.Rules[i].Start.After(c.Rules[i].End) {
				return fmt.Errorf("rules[%d]: from %q is after until %q", i, r.From, r.Until)
			}
			c.Rules[i].End = localday.Add(c.Rules[i].End, 1)
		}
		if r.CWDPrefix != "" {
			c.Rules[i].CWDPrefix = strings.TrimRight(ExpandHome(r.CWDPrefix, c.Home), "/")
		}
		if r.TitleRegex != "" {
			if c.Rules[i].TitleRE, err = regexp.Compile(r.TitleRegex); err != nil {
				return fmt.Errorf("rules[%d]: title_regex: %w", i, err)
			}
		}
		if r.URLRegex != "" {
			if c.Rules[i].URLRE, err = regexp.Compile(r.URLRegex); err != nil {
				return fmt.Errorf("rules[%d]: url_regex: %w", i, err)
			}
		}
	}
	return nil
}

// ProjectNames lists the [projects.*] names sorted; the brain writes them to
// meta.projects for the collector's pop-up picker (Projects is a map: no file order).
func (c *Config) ProjectNames() []string {
	out := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// NtfyEndpoint is the URL pushes are POSTed to; "" disables pushes.
func (c *Config) NtfyEndpoint() string {
	if c.Settings.NtfyURL != "" {
		return c.Settings.NtfyURL
	}
	if c.Settings.NtfyTopic == "" {
		return ""
	}
	return "https://ntfy.sh/" + c.Settings.NtfyTopic
}

// CallAppSet is Settings.CallApps as a lookup set.
func (c *Config) CallAppSet() map[string]bool {
	m := make(map[string]bool, len(c.Settings.CallApps))
	for _, a := range c.Settings.CallApps {
		m[a] = true
	}
	return m
}

// InActiveHours reports whether t (converted to c.Location) falls in settings.active_hours.
func (c *Config) InActiveHours(t time.Time) bool {
	lt := t.In(c.Location)
	m := lt.Hour()*60 + lt.Minute()
	return m >= c.ActiveFrom && m < c.ActiveUntil
}

func parseWeekday(name string) (time.Weekday, bool) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.EqualFold(name, d.String()) {
			return d, true
		}
	}
	return 0, false
}

// ExpandHome replaces a leading "~" with home.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return home + p[1:]
	}
	return p
}

// activeHoursRE carries the whole grammar: two digits each, hours 00-23, minutes 00-59,
// nothing before or after.
var activeHoursRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])-([01][0-9]|2[0-3]):([0-5][0-9])$`)

// parseActiveHours parses a strict "HH:MM-HH:MM" into minutes after local midnight.
func parseActiveHours(s string) (from, until int, err error) {
	sub := activeHoursRE.FindStringSubmatch(s)
	if sub == nil {
		return 0, 0, fmt.Errorf("settings.active_hours: want HH:MM-HH:MM, got %q", s)
	}
	var n [4]int
	for i, p := range sub[1:] {
		n[i], _ = strconv.Atoi(p) // two digits matched by the regex, cannot fail
	}
	from, until = n[0]*60+n[1], n[2]*60+n[3]
	if from >= until {
		return 0, 0, fmt.Errorf("settings.active_hours: start must be before end, got %q", s)
	}
	return from, until, nil
}
