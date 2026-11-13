package report

import (
	"database/sql"
	"sort"
	"time"

	"stint/internal/config"
	"stint/internal/localday"
	"stint/internal/totals"
)

// pickerDays: sub-projects with time this recently enter the stretch pop-up's picker.
const pickerDays = 30

// Picker is meta.picker: every declared project in name order, each
// followed by its sub-projects with time in the last pickerDays days, folded as on the weekly
// page ("acme-app-pr12" -> "acme-app") and sorted, as "project" and "project/subproject".
func Picker(sq *sql.DB, cfg *config.Config, now time.Time) ([]string, error) {
	known, err := totals.Subprojects(sq)
	if err != nil {
		return nil, err
	}
	recent, err := totals.SubprojectsSince(sq, totals.DayKey(localday.Add(now.In(cfg.Location), -pickerDays), cfg.Location))
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, project := range cfg.ProjectNames() {
		out = append(out, project)
		seen := map[string]bool{}
		var subs []string
		for sub := range recent[project] {
			if f := fold(sub, known[project]); !seen[f] {
				seen[f] = true
				subs = append(subs, f)
			}
		}
		sort.Strings(subs)
		for _, sub := range subs {
			out = append(out, project+"/"+sub)
		}
	}
	return out, nil
}
