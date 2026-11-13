package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const valid = `
[settings]
ntfy_topic = "abcdefghijklmnopqrstuvwxyz"
vault_reports_dir = "~/stint-reports"
call_apps = ["us.zoom.xos"]
tune_until = "2026-10-16"

[projects.acme]
weekly_cap_hours = 30
paid = true

[projects.phd]

[[rules]]
project = "acme"
cwd_prefix = "~/dev-projects/acme/"

[[rules]]
project = "phd"
url_regex = 'overleaf\.com'
`

func TestParseDefaultsAndDerived(t *testing.T) {
	cfg, err := Parse([]byte(valid), "/Users/me")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Settings
	if s.IdleMinutes != 5 || s.CarryoverMinutes != 10 || s.GapMinutes != 2 || s.RawRetentionDays != 90 || s.NtfyTimeoutSeconds != 10 {
		t.Errorf("defaults not applied: %+v", s)
	}
	if s.VaultReportsDir != "/Users/me/stint-reports" {
		t.Errorf("reports dir = %q", s.VaultReportsDir)
	}
	if cfg.Rules[0].CWDPrefix != "/Users/me/dev-projects/acme" {
		t.Errorf("cwd_prefix not expanded/trimmed: %q", cfg.Rules[0].CWDPrefix)
	}
	if cfg.Location != time.Local {
		t.Errorf("location = %v, want time.Local", cfg.Location)
	}
	if cfg.FirstWeekday != time.Sunday {
		t.Errorf("first weekday = %v, want Sunday", cfg.FirstWeekday)
	}
	if cfg.ActiveFrom != 7*60 || cfg.ActiveUntil != 23*60 {
		t.Errorf("active hours = %d-%d", cfg.ActiveFrom, cfg.ActiveUntil)
	}
	if got := cfg.TuneUntil.Format("2006-01-02 15:04"); got != "2026-10-17 00:00" {
		t.Errorf("tune_until boundary = %s", got)
	}
	if cfg.Projects["acme"].WeeklyCapHours != 30 || !cfg.Projects["acme"].Paid {
		t.Errorf("project metadata = %+v", cfg.Projects["acme"])
	}
	if got := cfg.NtfyEndpoint(); got != "https://ntfy.sh/abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("ntfy endpoint = %q", got)
	}
	if !cfg.CallAppSet()["us.zoom.xos"] {
		t.Error("call app set missing zoom")
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ name, toml, want string }{
		{"unknown settings key", strings.Replace(valid, "tune_until", "tune_untill", 1), "unknown keys: settings.tune_untill"},
		{"unknown rule key", valid + "\n[[rules]]\nproject = \"phd\"\napp_id = \"x\"\n", "unknown keys: rules.app_id"},
		{"bad regex names rule index", valid + "\n[[rules]]\nproject = \"phd\"\ntitle_regex = '('\n", "rules[2]"},
		{"rule without conditions", valid + "\n[[rules]]\nproject = \"phd\"\n", "rules[2]: needs at least one"},
		{"undeclared project", valid + "\n[[rules]]\nproject = \"ghost\"\napp = \"x\"\n", "rules[2]: project \"ghost\" is not declared"},
		{"old timezone key says to delete the line", strings.Replace(valid, "[settings]", "[settings]\ntimezone = \"UTC\"", 1), "settings.timezone is no longer supported: stint uses the Mac's system timezone; delete the timezone line"},
		{"bad active hours", strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"7-23\"", 1), "settings.active_hours"},
		{"bad tune_until", strings.Replace(valid, "2026-10-16", "16/10/2026", 1), "settings.tune_until"},
		{"week start not a weekday", strings.Replace(valid, "[settings]", "[settings]\nweek_start = \"funday\"", 1), "settings.week_start: want a weekday name"},
		{"week start abbreviated", strings.Replace(valid, "[settings]", "[settings]\nweek_start = \"mon\"", 1), "settings.week_start: want a weekday name"},
		{"reserved project", valid + "\n[projects.unattributed]\n", "reserved"},
		{"reserved away project", valid + "\n[projects.away]\n", "reserved"},
		{"missing reports dir", strings.Replace(valid, "vault_reports_dir = \"~/stint-reports\"", "", 1), "vault_reports_dir"},
		{"active hours trailing garbage", strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"07:00-23:00x\"", 1), "settings.active_hours"},
		{"active hours single-digit hour", strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"7:00-23:00\"", 1), "settings.active_hours"},
		{"active hours 24 as end", strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"07:00-24:00\"", 1), "settings.active_hours"},
		{"active hours 24 as start", strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"24:00-23:00\"", 1), "settings.active_hours"},
		{"active hours minute out of range", strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"07:60-23:00\"", 1), "settings.active_hours"},
		{"active hours start not before end", strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"23:00-07:00\"", 1), "start must be before end"},
		{"idle_minutes must match the collector", strings.Replace(valid, "[settings]", "[settings]\nidle_minutes = 4", 1), "settings.idle_minutes must be 5"},
		{"idle_minutes zero names the contract", strings.Replace(valid, "[settings]", "[settings]\nidle_minutes = 0", 1), "collector/schema.sql"},
		{"ntfy_topic too long", strings.Replace(valid, "abcdefghijklmnopqrstuvwxyz", strings.Repeat("a", 65), 1), "settings.ntfy_topic"},
		{"subproject with cwd_prefix must match the pattern", valid + "\n[[rules]]\nproject = \"phd\"\ncwd_prefix = \"~/x\"\nsubproject = \"Bad Name\"\n", "rules[2]: subproject \"Bad Name\" must match"},
		{"from without zero padding", valid + "\n[[rules]]\nproject = \"phd\"\napp = \"x\"\nfrom = \"2026-11-8\"\n", "rules[2]: from: want YYYY-MM-DD, got \"2026-11-8\""},
		{"until not a calendar day", valid + "\n[[rules]]\nproject = \"phd\"\napp = \"x\"\nuntil = \"2026-02-30\"\n", "rules[2]: until: want YYYY-MM-DD, got \"2026-02-30\""},
		{"from after until", valid + "\n[[rules]]\nproject = \"phd\"\napp = \"x\"\nfrom = \"2026-11-09\"\nuntil = \"2026-11-08\"\n", "rules[2]: from \"2026-11-09\" is after until \"2026-11-08\""},
		{"a date window is not a condition", valid + "\n[[rules]]\nproject = \"phd\"\nfrom = \"2026-11-08\"\n", "rules[2]: needs at least one"},
		{"subproject with upper case and spaces", valid + "\n[[rules]]\nproject = \"phd\"\napp = \"x\"\nsubproject = \"Budget Tracker\"\n", "rules[2]: subproject \"Budget Tracker\" must match"},
		{"subproject starting with a dash", valid + "\n[[rules]]\nproject = \"phd\"\napp = \"x\"\nsubproject = \"-a\"\n", "rules[2]: subproject"},
		{"subproject of 41 characters", valid + "\n[[rules]]\nproject = \"phd\"\napp = \"x\"\nsubproject = \"" + strings.Repeat("a", 41) + "\"\n", "rules[2]: subproject"},
		{"ntfy_topic with slash", strings.Replace(valid, "abcdefghijklmnopqrstuvwxyz", "stint/topic", 1), "settings.ntfy_topic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.toml), "/Users/me")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestParseWeekStart(t *testing.T) {
	cases := []struct {
		in   string
		want time.Weekday
	}{
		{"sunday", time.Sunday},
		{"monday", time.Monday},
		{"tuesday", time.Tuesday},
		{"wednesday", time.Wednesday},
		{"thursday", time.Thursday},
		{"friday", time.Friday},
		{"saturday", time.Saturday},
		{"Monday", time.Monday},
		{"SATURDAY", time.Saturday},
	}
	for _, c := range cases {
		cfg, err := Parse([]byte(strings.Replace(valid, "[settings]", "[settings]\nweek_start = \""+c.in+"\"", 1)), "/h")
		if err != nil {
			t.Fatalf("week_start %q: %v", c.in, err)
		}
		if cfg.FirstWeekday != c.want {
			t.Errorf("week_start %q: first weekday = %v, want %v", c.in, cfg.FirstWeekday, c.want)
		}
	}
}

func TestNtfyURLOverrideAndDisabled(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(valid, "[settings]", "[settings]\nntfy_url = \"http://127.0.0.1:9/x\"", 1)), "/h")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NtfyEndpoint() != "http://127.0.0.1:9/x" {
		t.Errorf("override ignored: %q", cfg.NtfyEndpoint())
	}
	cfg, err = Parse([]byte(strings.Replace(valid, "ntfy_topic = \"abcdefghijklmnopqrstuvwxyz\"", "", 1)), "/h")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NtfyEndpoint() != "" {
		t.Errorf("no topic should disable pushes, got %q", cfg.NtfyEndpoint())
	}
}

func TestInActiveHours(t *testing.T) {
	cfg, err := Parse([]byte(valid), "/h")
	if err != nil {
		t.Fatal(err)
	}
	loc := cfg.Location
	if cfg.InActiveHours(time.Date(2026, 9, 16, 6, 59, 0, 0, loc)) {
		t.Error("06:59 should be outside")
	}
	if !cfg.InActiveHours(time.Date(2026, 9, 16, 7, 0, 0, 0, loc)) {
		t.Error("07:00 should be inside")
	}
	if cfg.InActiveHours(time.Date(2026, 9, 16, 23, 0, 0, 0, loc)) {
		t.Error("23:00 should be outside (exclusive end)")
	}
}

// The checked-in example must always load (pushes disabled because the topic is empty).
func TestExampleFileParses(t *testing.T) {
	data, err := os.ReadFile("../../../rules.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data, "/Users/me")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 6 || cfg.NtfyEndpoint() != "" || cfg.Projects["acme"].WeeklyCapHours != 20 {
		t.Fatalf("rules=%d endpoint=%q cap=%v", len(cfg.Rules), cfg.NtfyEndpoint(), cfg.Projects["acme"].WeeklyCapHours)
	}
}

func TestRuleDateWindowAndCWDSubproject(t *testing.T) {
	extra := `
[[rules]]
project = "acme"
cwd_prefix = "~/dev-projects/freelance"
subproject = "globex"
from = "2026-11-08"
until = "2026-11-08"

[[rules]]
project = "phd"
app = "x"
from = "2026-11-01"

[[rules]]
project = "phd"
app = "y"
until = "2026-03-27"
`
	cfg, err := Parse([]byte(valid+extra), "/Users/me")
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Rules[2]
	if r.CWDPrefix != "/Users/me/dev-projects/freelance" || r.Subproject != "globex" {
		t.Errorf("cwd rule = %+v", r)
	}
	day := func(tm time.Time) string { return tm.In(cfg.Location).Format("2006-01-02 15:04") }
	if day(r.Start) != "2026-11-08 00:00" || day(r.End) != "2026-11-09 00:00" {
		t.Errorf("one-day window = [%s, %s)", day(r.Start), day(r.End))
	}
	if open := cfg.Rules[3]; day(open.Start) != "2026-11-01 00:00" || !open.End.IsZero() {
		t.Errorf("from only: Start %s, End zero = %v", day(open.Start), open.End.IsZero())
	}
	// 2026-03-27 is the spring DST change in Asia/Jerusalem: the end is the next local midnight.
	if open := cfg.Rules[4]; !open.Start.IsZero() || day(open.End) != "2026-03-28 00:00" {
		t.Errorf("until only: Start zero = %v, End %s", open.Start.IsZero(), day(open.End))
	}
	if !cfg.Rules[0].Start.IsZero() || !cfg.Rules[0].End.IsZero() {
		t.Error("an undated rule must have no window")
	}
}

// Two invalid integer settings at once must always name the first one in file order.
func TestRequiredIntErrorIsDeterministic(t *testing.T) {
	toml := strings.Replace(valid, "[settings]", "[settings]\ncarryover_minutes = 0\ngap_minutes = 0", 1)
	for i := 0; i < 50; i++ {
		_, err := Parse([]byte(toml), "/h")
		if err == nil || !strings.Contains(err.Error(), "settings.carryover_minutes must be > 0") {
			t.Fatalf("iteration %d: want carryover_minutes named, got %v", i, err)
		}
	}
}

func TestActiveHoursBounds(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(valid, "[settings]", "[settings]\nactive_hours = \"00:00-23:59\"", 1)), "/h")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveFrom != 0 || cfg.ActiveUntil != 23*60+59 {
		t.Errorf("active hours = %d-%d", cfg.ActiveFrom, cfg.ActiveUntil)
	}
}

func TestNtfyTopicLimit(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(valid, "abcdefghijklmnopqrstuvwxyz", strings.Repeat("a", 64), 1)), "/h")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.NtfyEndpoint(); got != "https://ntfy.sh/"+strings.Repeat("a", 64) {
		t.Errorf("64-char topic rejected or mangled: %q", got)
	}
}

func TestDefaultPath(t *testing.T) {
	if got := DefaultPath("/Users/me"); got != "/Users/me/.config/stint/rules.toml" {
		t.Errorf("DefaultPath = %q", got)
	}
}

func TestProjectNamesSorted(t *testing.T) {
	cfg, err := Parse([]byte("[settings]\nvault_reports_dir=\"/tmp\"\n[projects.zeta]\n[projects.alpha]\n[projects.mid]\n[[rules]]\nproject = \"alpha\"\napp = \"A\"\n"), "/h")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProjectNames(); !reflect.DeepEqual(got, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("ProjectNames = %v", got)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.toml")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Home != home {
		t.Errorf("Home = %q, want %q", cfg.Home, home)
	}
	if cfg.Settings.VaultReportsDir != filepath.Join(home, "stint-reports") {
		t.Errorf("reports dir not expanded against the real home: %q", cfg.Settings.VaultReportsDir)
	}

	if _, err := Load(filepath.Join(dir, "missing.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: want ErrNotExist, got %v", err)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[settings]\nweek_start = \"funday\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "rules.toml: settings.week_start") {
		t.Errorf("invalid file: want a validation error, got %v", err)
	}
}

func TestParseRuleSubproject(t *testing.T) {
	long := strings.Repeat("a", 40)
	cfg, err := Parse([]byte(valid+"\n[[rules]]\nproject = \"phd\"\napp = \"x\"\nsubproject = \"budget-tracker\"\n\n[[rules]]\nproject = \"phd\"\napp = \"y\"\nsubproject = \""+long+"\"\n"), "/h")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[2].Subproject != "budget-tracker" || cfg.Rules[3].Subproject != long || cfg.Rules[0].Subproject != "" {
		t.Fatalf("subprojects: %q %q %q", cfg.Rules[2].Subproject, cfg.Rules[3].Subproject, cfg.Rules[0].Subproject)
	}
}
