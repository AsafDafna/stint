package rulesfile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"stint/internal/attribute"
	"stint/internal/config"
)

// base has comments, a trailing comment line and no final newline: Append must keep every
// byte of it and start its own text on a new line.
const base = `# my rules
[settings]
vault_reports_dir = "/tmp/x"   # keep me

[projects.phd]
[projects.personal-dev]

[[rules]]
project = "phd"
url_regex = 'overleaf\.com'
# trailing note`

func TestHostRegex(t *testing.T) {
	re := regexp.MustCompile(HostRegex("grok.com"))
	for _, u := range []string{"https://grok.com/", "https://grok.com", "http://GROK.COM/chat", "https://x.grok.com:8443/a", "https://grok.com?q=1", "https://grok.com#top"} {
		if !re.MatchString(u) {
			t.Errorf("%s should match", u)
		}
	}
	for _, u := range []string{"https://notgrok.com/", "https://grok.com.evil.io/", "https://grokxcom/", "ftp://grok.com/", "https://evil.io/?u=https://grok.com/", "https://evil.io?.grok.com", "https://evil.io#.grok.com", "https://evil.io?x=.grok.com/"} {
		if re.MatchString(u) {
			t.Errorf("%s must not match", u)
		}
	}
	if !regexp.MustCompile(HostRegex("acme.example")).MatchString("https://app.acme.example/x") {
		t.Error("sub-domain under an acme.example rule should match")
	}
}

// The rule learned from a row matches the URLs that make up the row, so an "always" sticks.
func TestLearnedHostRuleMatchesItsRowsURLs(t *testing.T) {
	for _, u := range []string{"https://grok.com/chat", "https://WWW.Grok.com/", "https://grok.com:8443/x", "http://app.acme.example/x", "https://grok.com./chat", "https://www.grok.com.:443/x"} {
		host, ok := strings.CutPrefix(attribute.RowKey(attribute.Sample{App: "com.google.Chrome", URL: u}), attribute.KeyHost)
		if !ok {
			t.Fatalf("%s: not a host row", u)
		}
		if !regexp.MustCompile(HostRegex(host)).MatchString(u) {
			t.Errorf("%s: the rule learned from host %q does not match it", u, host)
		}
	}
}

func TestBlockFormat(t *testing.T) {
	got, err := Block(Learned{Key: "host:grok.com", Project: "personal-dev", Subproject: "budget-tracker", Label: "grok\n.com\t"}, "2026-10-06", "Mon 5 Oct 11:57")
	if err != nil {
		t.Fatal(err)
	}
	want := "\n# added by stint pop-up 2026-10-06 | stretch Mon 5 Oct 11:57 | grok.com\n[[rules]]\nproject = \"personal-dev\"\nsubproject = \"budget-tracker\"\nurl_regex = '(?i)^https?://([^/?#]+\\.)?grok\\.com(:\\d+)?([/?#]|$)'\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	got, err = Block(Learned{Key: "app:net.whatsapp.WhatsApp", Project: "phd", Label: "WhatsApp"}, "2026-10-06", "Mon 5 Oct 11:57")
	if err != nil {
		t.Fatal(err)
	}
	if want := "\n# added by stint pop-up 2026-10-06 | stretch Mon 5 Oct 11:57 | WhatsApp\n[[rules]]\nproject = \"phd\"\napp = \"net.whatsapp.WhatsApp\"\n"; got != want {
		t.Fatalf("app rule:\n%s", got)
	}
	for _, key := range []string{"host:a'#b", "host:Grok.com", "app:", "url:x"} {
		if _, err := Block(Learned{Key: key, Project: "phd"}, "d", "w"); err == nil {
			t.Errorf("key %q must be refused", key)
		}
	}
}

func TestAppendKeepsFileAndSkipsDuplicates(t *testing.T) {
	groups := []Group{
		{Day: "2026-10-06", Window: "Mon 5 Oct 11:57", Rules: []Learned{{Key: "host:grok.com", Project: "personal-dev", Label: "grok.com"}}},
		{Day: "2026-10-06", Window: "Mon 5 Oct 14:02", Rules: []Learned{{Key: "host:grok.com", Project: "personal-dev", Label: "grok.com"}}},
		{Day: "2026-10-06", Window: "Mon 5 Oct 15:10", Rules: []Learned{{Key: "app:com.x.Ok", Project: "phd"}, {Key: "app:com.x.Ghost", Project: "ghost"}}},
		{Day: "2026-10-06", Window: "Mon 5 Oct 16:40", Rules: []Learned{{Key: "app:com.x.Notes", Project: "phd", Subproject: "thesis", Label: "Notes"}}},
	}
	out, oc := Append([]byte(base), "/h", groups)
	if !strings.HasPrefix(string(out), base+"\n") {
		t.Fatalf("existing bytes changed:\n%s", out)
	}
	if oc[0].Added != 1 || oc[0].Err != nil || oc[1].Added != 0 || oc[1].Err != nil || oc[3].Added != 1 || oc[3].Err != nil {
		t.Fatalf("outcomes: %+v", oc)
	}
	if oc[2].Added != 0 || oc[2].Err == nil || !strings.Contains(oc[2].Err.Error(), `"ghost" is not declared`) {
		t.Fatalf("undeclared project must reject the whole group: %+v", oc[2])
	}
	if n := strings.Count(string(out), "grok"); n != 2 { // one marker label, one url_regex
		t.Fatalf("grok.com rule written more than once:\n%s", out)
	}
	if strings.Contains(string(out), "com.x.Ok") {
		t.Fatal("a rejected group's other rules must not be written")
	}
	cfg, err := config.Parse(out, "/h")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 3 || cfg.Rules[2].Subproject != "thesis" || cfg.Rules[2].App != "com.x.Notes" {
		t.Fatalf("rules: %+v", cfg.Rules)
	}
	again, oc := Append(out, "/h", groups[:1])
	if string(again) != string(out) || oc[0].Added != 0 || oc[0].Err != nil {
		t.Fatalf("a retry must change nothing: %+v", oc)
	}
	if same, _ := Append([]byte(base), "/h", groups[2:3]); string(same) != base {
		t.Fatal("nothing added: content must come back unchanged, no newline appended")
	}
}

// A hand-written dated rule covers only its window, so it is no duplicate of the learned rule.
func TestAppendIgnoresDatedTwin(t *testing.T) {
	dated := base + "\n[[rules]]\nproject = \"phd\"\napp = \"com.x.Notes\"\nfrom = \"2026-11-08\"\nuntil = \"2026-11-08\"\n"
	out, oc := Append([]byte(dated), "/h", []Group{{Day: "2026-11-09", Window: "Sun 8 Nov 10:00", Rules: []Learned{{Key: "app:com.x.Notes", Project: "phd", Label: "Notes"}}}})
	if oc[0].Added != 1 || oc[0].Err != nil {
		t.Fatalf("outcome: %+v", oc[0])
	}
	cfg, err := config.Parse(out, "/h")
	if err != nil {
		t.Fatal(err)
	}
	if l := cfg.Rules[len(cfg.Rules)-1]; l.App != "com.x.Notes" || l.From != "" || l.Until != "" {
		t.Fatalf("learned rule: %+v", l)
	}
}

func TestWriteKeepsModeAndBak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("old"), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("file = %q", b)
	}
	if b, _ := os.ReadFile(path + ".bak"); string(b) != "old" {
		t.Fatalf(".bak = %q", b)
	}
	for _, p := range []string{path, path + ".bak"} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", p, fi.Mode().Perm())
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

// A hand edit saved after the brain read the file must survive: nothing is written.
func TestWriteRefusesAConcurrentEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.toml")
	if err := os.WriteFile(path, []byte("edited by hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("old"), []byte("new")); err == nil || !strings.Contains(err.Error(), "changed while learned rules were being written") {
		t.Fatalf("want a changed-file error, got %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "edited by hand" {
		t.Fatalf("hand edit lost: %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left files behind: %v", entries)
	}
}

// The directory flush comes after the rename: when it fails the file is already replaced,
// so Write must not report the write as failed. A write+search-only directory makes it fail.
func TestWriteSucceedsWhenOnlyTheDirectoryFlushFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	err := Write(path, []byte("old"), []byte("new"))
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("file = %q", b)
	}
	if err != nil {
		t.Fatalf("the file was replaced, yet Write = %v", err)
	}
}

func TestWriteFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "rules.toml")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte("old"), 0o644)
	link := filepath.Join(dir, "rules.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, []byte("old"), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Fatalf("target = %q", b)
	}
}

func TestReadMarkers(t *testing.T) {
	content, _ := Append([]byte(base), "/h", []Group{{Day: "2026-10-06", Window: "Mon 5 Oct 11:57", Rules: []Learned{
		{Key: "host:grok.com", Project: "personal-dev", Label: "grok | chat"},
		{Key: "app:com.anthropic.claudefordesktop", Project: "phd", Label: "Claude"},
	}}})
	markers, headers := ReadMarkers(content)
	if headers != 3 || len(markers) != 2 || markers[0] != (Marker{1, "grok | chat"}) || markers[1] != (Marker{2, "Claude"}) {
		t.Fatalf("headers=%d markers=%+v", headers, markers)
	}
	// A hand-written rule inserted above shifts every learned index.
	shifted := strings.Replace(string(content), "[[rules]]\nproject = \"phd\"\nurl_regex", "[[rules]]\nproject = \"phd\"\napp = \"x\"\n\n[[rules]]\nproject = \"phd\"\nurl_regex", 1)
	markers, headers = ReadMarkers([]byte(shifted))
	if headers != 4 || markers[0].Index != 2 || markers[1].Index != 3 {
		t.Fatalf("after an insert above: headers=%d markers=%+v", headers, markers)
	}
}
