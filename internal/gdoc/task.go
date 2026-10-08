// task.go turns parsed rows into the unified task table.

package gdoc

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"schoolwork-check/internal/model"
)

// Caps on what one row may carry into the table.
const (
	maxTitleBytes       = 200
	maxDescriptionBytes = 4 << 10
	maxSlugLen          = 40
)

// tasks converts parsed rows to tasks, dropping rows outside the date window
// and rows the model returned twice.
func (c *Client) tasks(d doc, course string, entries []entry, now time.Time) []model.Task {
	var (
		out  []model.Task
		seen = map[string]bool{}
	)
	for _, e := range entries {
		day, ok := parseDate(e.Date, c.opts.Location)
		if !ok {
			c.log.Debug("gdoc: row without a usable date, skipped", "doc", d.ID, "date", e.Date, "title", e.Title)
			continue
		}
		title := clip(strings.TrimSpace(e.Title), maxTitleBytes)
		if title == "" {
			continue
		}
		due := day
		if d, ok := parseDate(e.DueDate, c.opts.Location); ok {
			due = d
		}
		due = atTime(due, e.Time)
		if !c.inWindow(due, now) {
			continue
		}

		id := fmt.Sprintf("gdoc:%s:%s-%s", d.ID, due.Format("2006-01-02"), slug(title))
		if seen[id] {
			continue
		}
		seen[id] = true

		out = append(out, model.Task{
			ID:          id,
			Source:      model.SourceGDoc,
			Kind:        kindOf(e.Kind),
			Course:      course,
			CourseID:    d.ID,
			Title:       title,
			URL:         d.URL,
			DueAt:       &due,
			Description: description(e, d, day, c.opts.Location),
			Progress:    model.Progress{State: model.StateNotStarted},
			FetchedAt:   now.UTC(),
		})
	}
	return out
}

// wholeDoc is the fallback item: the document itself, as one piece of
// information. The note sink hands it to its agent inbox, which keeps
// whatever in it is worth remembering.
func (c *Client) wholeDoc(d doc, course string, now time.Time) model.Task {
	name := d.Name
	if name == "" {
		name = "Course calendar"
	}
	var b strings.Builder
	b.WriteString("Course calendar document, not broken into rows.\n\n")
	b.WriteString(clip(strings.TrimSpace(d.Text), maxDescriptionBytes))
	if d.Truncated {
		b.WriteString("\n\n[cut off at the extraction size limit]")
	}
	return model.Task{
		ID:          "gdoc:" + d.ID,
		Source:      model.SourceGDoc,
		Kind:        model.KindMaterial,
		Course:      course,
		CourseID:    d.ID,
		Title:       name,
		URL:         d.URL,
		Description: b.String(),
		Progress:    model.Progress{State: model.StateNotStarted},
		FetchedAt:   now.UTC(),
	}
}

// description is the row's own text plus where it came from, so a reader —
// or note's agent — can tell a calendar row from an LMS assignment.
func description(e entry, d doc, day time.Time, loc *time.Location) string {
	var b strings.Builder
	if t := strings.TrimSpace(e.Description); t != "" {
		b.WriteString(clip(t, maxDescriptionBytes))
		b.WriteString("\n\n")
	}
	name := d.Name
	if name == "" {
		name = "the course calendar"
	}
	fmt.Fprintf(&b, "From %s, row dated %s.", name, day.In(loc).Format("Mon 2 Jan 2006"))
	return b.String()
}

// inWindow applies PastDays and FutureDays to a row's date. A zero bound
// means unbounded on that side.
func (c *Client) inWindow(due, now time.Time) bool {
	if c.opts.PastDays > 0 && due.Before(now.AddDate(0, 0, -c.opts.PastDays)) {
		return false
	}
	if c.opts.FutureDays > 0 && due.After(now.AddDate(0, 0, c.opts.FutureDays)) {
		return false
	}
	return true
}

// kindOf maps the model's kind to the table's. Anything unrecognised is
// material: information, which goes to the inbox rather than the task list.
func kindOf(s string) model.Kind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "assignment", "homework", "task":
		return model.KindAssignment
	case "quiz", "test", "exam":
		return model.KindQuiz
	case "discussion":
		return model.KindDiscussion
	case "announcement", "event", "note":
		return model.KindAnnouncement
	default:
		return model.KindMaterial
	}
}

// parseDate reads the YYYY-MM-DD the prompt asks for, in the local zone.
func parseDate(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// atTime places a date at the time of day the row stated, or at the end of
// the day when it stated none: a row dated Friday is due by Friday.
func atTime(day time.Time, hhmm string) time.Time {
	loc := day.Location()
	if t, err := time.Parse("15:04", strings.TrimSpace(hhmm)); err == nil {
		return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 0, 0, loc)
}

// slug is the stable part of a row's id: lowercase words of the title. A
// teacher rewording a row moves the row to a new id, which note sees as a
// new task; that is the price of a calendar with no ids of its own.
func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			dash = true
			continue
		}
		if dash && b.Len() > 0 {
			if b.Len()+1 >= maxSlugLen {
				break
			}
			b.WriteByte('-')
		}
		dash = false
		if b.Len()+utf8.RuneLen(r) > maxSlugLen {
			break
		}
		b.WriteRune(r)
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "row"
	}
	return s
}

// clip cuts a string to at most n bytes, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
