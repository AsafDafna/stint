// Package rulesfile appends the stretch pop-up's learned rules to rules.toml and finds them
// again. It edits text and never re-serializes, so comments, order and
// formatting of the file survive.
package rulesfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"stint/internal/attribute"
	"stint/internal/config"
)

// MarkerPrefix starts the comment line directly above every learned [[rules]] header.
const MarkerPrefix = "# added by stint pop-up "

// Learned is one "always" answer entry to turn into a rule.
type Learned struct {
	Key        string // attribute.RowKey: "host:<host>" or "app:<bundle id>"
	Project    string
	Subproject string // "" = none
	Label      string // the panel row's label, recorded in the marker
}

// Group is one stretch row's learned rules; Append accepts or rejects a group whole.
type Group struct {
	Day    string // "2026-10-06": the day the rules are written
	Window string // "Mon 5 Oct 11:57": the stretch's start, local
	Rules  []Learned
}

// Outcome is Append's verdict on one group: the rules it appended, or why it was left out.
type Outcome struct {
	Added int
	Err   error
}

// hostRE: a host the url_regex literal can carry safely (no quote, no TOML or regex surprises).
var hostRE = regexp.MustCompile(`^[a-z0-9._-]+$`)

// HostRegex matches http(s) URLs on host and its sub-domains, in any case and on any port;
// a look-alike such as notgrok.com does not match.
func HostRegex(host string) string {
	return `(?i)^https?://([^/?#]+\.)?` + regexp.QuoteMeta(host) + `(:\d+)?([/?#]|$)`
}

// Block renders one learned rule: a blank line, the marker, the [[rules]] table.
func Block(l Learned, day, window string) (string, error) {
	var cond string
	if host, ok := strings.CutPrefix(l.Key, attribute.KeyHost); ok {
		if !hostRE.MatchString(host) {
			return "", fmt.Errorf("host %q cannot become a url_regex rule", host)
		}
		cond = "url_regex = '" + HostRegex(host) + "'"
	} else if app, ok := strings.CutPrefix(l.Key, attribute.KeyApp); ok && app != "" {
		cond = "app = " + basic(app)
	} else {
		return "", fmt.Errorf("key %q is not host:<host> or app:<bundle id>", l.Key)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s%s | stretch %s | %s\n[[rules]]\nproject = %s\n", MarkerPrefix, day, window, cleanLabel(l.Label), basic(l.Project))
	if l.Subproject != "" {
		fmt.Fprintf(&b, "subproject = %s\n", basic(l.Subproject))
	}
	b.WriteString(cond + "\n")
	return b.String(), nil
}

// Append adds the groups' rules to content (rules.toml as read) and returns the new content
// and one Outcome per group. Each group is checked against content plus the groups accepted
// before it: a rule identical to one already there is skipped, so a retry never duplicates,
// and a group whose rules make config.Parse fail is left out whole. With nothing added the
// content comes back unchanged.
func Append(content []byte, home string, groups []Group) ([]byte, []Outcome) {
	outcomes := make([]Outcome, len(groups))
	cur := bytes.Clone(content)
	if len(cur) > 0 && cur[len(cur)-1] != '\n' {
		cur = append(cur, '\n')
	}
	cfg, err := config.Parse(cur, home)
	if err != nil {
		for i := range outcomes {
			outcomes[i].Err = err
		}
		return content, outcomes
	}
	changed := false
	for i, g := range groups {
		var add strings.Builder
		n := 0
		for _, l := range g.Rules {
			if has(cfg.Rules, l) {
				continue
			}
			blk, err := Block(l, g.Day, g.Window)
			if err != nil {
				outcomes[i].Err = err
				break
			}
			add.WriteString(blk)
			n++
		}
		if outcomes[i].Err != nil || n == 0 {
			continue
		}
		next := append(bytes.Clone(cur), add.String()...)
		ncfg, err := config.Parse(next, home)
		if err != nil {
			outcomes[i].Err = err
			continue
		}
		cur, cfg, changed = next, ncfg, true
		outcomes[i].Added = n
	}
	if !changed {
		return content, outcomes
	}
	return cur, outcomes
}

// has reports whether rs already holds the rule l would write.
func has(rs []config.Rule, l Learned) bool {
	app, url := "", ""
	if host, ok := strings.CutPrefix(l.Key, attribute.KeyHost); ok {
		url = HostRegex(host)
	} else {
		app = strings.TrimPrefix(l.Key, attribute.KeyApp)
	}
	for _, r := range rs {
		if r.Project == l.Project && r.Subproject == l.Subproject && r.App == app && r.URLRegex == url && r.TitleRegex == "" && r.CWDPrefix == "" && r.From == "" && r.Until == "" {
			return true
		}
	}
	return false
}

// basic renders s as a TOML basic string.
func basic(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// cleanLabel drops control characters, so the marker stays one comment line.
func cleanLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Write replaces the file at path with content in one atomic step (a temp file renamed in the
// same directory, mode kept), after copying old to path+".bak", so the .bak is the file as it
// was before this run. A symlinked rules.toml stays a symlink: its target is replaced. When the
// file no longer holds old (a hand edit saved since it was read), nothing is replaced and the
// error says so; the caller retries on its next run.
func Write(path string, old, content []byte) error {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if err := unchanged(target, old); err != nil {
		return err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return err
	}
	perm := fi.Mode().Perm()
	if err := writeSynced(target+".bak", old, perm); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".rules.toml.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // fails harmlessly after the rename
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // APFS orders no data before the rename without it
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := unchanged(target, old); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	syncDir(filepath.Dir(target)) // the file is already replaced: failing here would fail a run that wrote its rules
	return nil
}

// writeSynced writes data to path with mode perm (an existing file's mode is reset too) and
// flushes it to disk before returning.
func writeSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir flushes a directory entry change (the rename) to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// unchanged fails when the file at path no longer holds old.
func unchanged(path string, old []byte) error {
	cur, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(cur, old) {
		return fmt.Errorf("%s changed while learned rules were being written", path)
	}
	return nil
}

// Marker is one learned rule found in rules.toml.
type Marker struct {
	Index int    // position among all [[rules]] headers in file order: the rule_id Match returns
	Label string // everything after the marker's second " | "
}

// ReadMarkers lists the learned rules in content (a [[rules]] header on the line directly below
// a marker) and counts every [[rules]] header. A count that differs from the parsed rule count
// means rules written another way (inline tables) and the indexes cannot be trusted.
func ReadMarkers(content []byte) (markers []Marker, headers int) {
	prev := ""
	for _, line := range strings.Split(string(content), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[[rules]]") {
			if strings.HasPrefix(prev, MarkerPrefix) {
				label := ""
				if parts := strings.SplitN(prev, " | ", 3); len(parts) == 3 {
					label = parts[2]
				}
				markers = append(markers, Marker{Index: headers, Label: label})
			}
			headers++
		}
		prev = t
	}
	return markers, headers
}
