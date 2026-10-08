// Package output renders []model.Task as JSON, Markdown or CSV.
//
// All three writers emit tasks in the same order (see Sort): by due date
// ascending with undated tasks last, then by course, then by title.
package output

import (
	"slices"
	"strings"
	"time"

	"schoolwork-check/internal/model"
)

// Location is the time zone used for human-readable output (Markdown).
// Tests set it to a fixed location; it defaults to the machine's local zone.
var Location = time.Local

// TimeLayout is the layout used for human-readable dates in Markdown.
const TimeLayout = "Mon Jan 2 15:04"

// missing is printed for absent values in Markdown.
const missing = "—"

// Sort orders tasks the way every writer in this package emits them:
// due date ascending (undated last), then course, then title, then ID.
func Sort(tasks []model.Task) {
	slices.SortStableFunc(tasks, compareTasks)
}

func compareTasks(a, b model.Task) int {
	switch {
	case a.DueAt == nil && b.DueAt != nil:
		return 1
	case a.DueAt != nil && b.DueAt == nil:
		return -1
	case a.DueAt != nil && b.DueAt != nil:
		if c := a.DueAt.Compare(*b.DueAt); c != 0 {
			return c
		}
	}
	if c := strings.Compare(a.Course, b.Course); c != 0 {
		return c
	}
	if c := strings.Compare(a.Title, b.Title); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// sorted returns a sorted copy so callers' slices keep their own order.
func sorted(tasks []model.Task) []model.Task {
	out := slices.Clone(tasks)
	Sort(out)
	return out
}

func location() *time.Location {
	if Location == nil {
		return time.Local
	}
	return Location
}

// formatTime renders a timestamp for humans, or "—" when absent.
func formatTime(t *time.Time) string {
	if t == nil {
		return missing
	}
	return t.In(location()).Format(TimeLayout)
}

// formatRFC3339 renders a timestamp in UTC for machines, empty when absent.
func formatRFC3339(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func attachmentNames(as []model.Attachment) []string {
	names := make([]string, 0, len(as))
	for _, a := range as {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = strings.TrimSpace(a.URL)
		}
		if name == "" {
			name = "(unnamed)"
		}
		names = append(names, name)
	}
	return names
}

// truncateRunes shortens s to at most n runes, appending an ellipsis.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " \t\n") + "…"
}

// oneLine flattens text so it can live inside a table cell.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	return s
}
