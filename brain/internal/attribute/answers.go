package attribute

import "encoding/json"

// AnswerItem is one entry of a stretches.answer JSON array, written by
// the collector's panel.
type AnswerItem struct {
	Key        string `json:"key"`     // RowKey
	Project    string `json:"project"` // Unattributed = "Leave unattributed"
	Subproject string `json:"subproject,omitempty"`
	Always     bool   `json:"always,omitempty"`
	Label      string `json:"label,omitempty"` // the row's label; set on always entries
}

// Answer is a stretch answer as attribution uses it: inside [FromTS, ToTS), an unmatched
// span whose RowKey has an entry goes where the entry says.
type Answer struct {
	FromTS, ToTS int64
	Entries      map[string]AnswerItem // by Key
}

// ParseAnswer decodes a stretches.answer JSON array for the window [fromTS, toTS).
func ParseAnswer(fromTS, toTS int64, js string) (Answer, error) {
	var items []AnswerItem
	if err := json.Unmarshal([]byte(js), &items); err != nil {
		return Answer{}, err
	}
	a := Answer{FromTS: fromTS, ToTS: toTS, Entries: make(map[string]AnswerItem, len(items))}
	for _, it := range items {
		a.Entries[it.Key] = it
	}
	return a, nil
}

// answerFor finds the entry for s's RowKey in the answer whose window [FromTS, ToTS) contains
// from, the span's start. The key is derived only inside a window.
func answerFor(from int64, s Sample, as []Answer) (AnswerItem, bool) {
	key := ""
	for _, a := range as {
		if a.FromTS <= from && from < a.ToTS {
			if key == "" {
				key = RowKey(s)
			}
			if e, ok := a.Entries[key]; ok {
				return e, true
			}
		}
	}
	return AnswerItem{}, false
}
