// attached.go finds the calendars that are already in the LMS. A teacher who
// keeps the schedule in a document usually also posts it as a material, and
// names it plainly: "Film Analysis Calendar", "Unit 3 Calendar". Those items
// are parsed into dated rows exactly like a configured document, instead of
// going to the inbox as eight pages of text nobody re-reads.

package gdoc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"schoolwork-check/internal/enrich"
	"schoolwork-check/internal/model"
)

// minCalendarText is the shortest text worth a model call: below it the item
// is a link or a sentence, not a schedule.
const minCalendarText = 200

// ExpandStats counts what one Expand did.
type ExpandStats struct {
	Found    int // LMS items whose title says "calendar"
	Expanded int // items replaced by their rows
	Rows     int // rows those items produced
	Skipped  int // too short, or already read as a configured document
	Failed   int // the model could not read it; the item was left alone
}

func (s ExpandStats) String() string {
	return fmt.Sprintf("gdoc: %d calendars posted in the LMS became %d rows (%d found, %d skipped, %d unreadable)",
		s.Expanded, s.Rows, s.Found, s.Skipped, s.Failed)
}

// NewParser returns a Client that can only parse text it is handed — no
// Drive, no configured documents. It is what Expand needs.
func NewParser(llm enrich.Completer, opts Options) *Client {
	c := newWithSource(nil, llm, opts)
	return c
}

// Expand replaces every LMS item that is really a calendar with the rows
// inside it. An item it cannot read is left exactly as it was, so the worst
// case is the behaviour from before this existed.
func (c *Client) Expand(ctx context.Context, tasks []model.Task) ([]model.Task, ExpandStats) {
	var st ExpandStats
	if c.llm == nil {
		return tasks, st
	}
	now := c.opts.Now()
	skip := map[string]bool{}
	for _, id := range c.opts.SkipDocIDs {
		skip[id] = true
	}

	out := make([]model.Task, 0, len(tasks))
	for _, t := range tasks {
		if !looksLikeCalendar(t) {
			out = append(out, t)
			continue
		}
		st.Found++
		text, ok := calendarText(t)
		switch {
		case !ok:
			st.Skipped++
			out = append(out, t)
			continue
		case alreadyConfigured(t, skip):
			// The same document is read directly, with its own revision
			// and its own re-read interval. Reading it twice would make
			// two rows for every assignment.
			st.Skipped++
			c.log.Debug("gdoc: calendar material is already a configured document", "task", t.ID, "title", t.Title)
			continue
		}

		d := Doc{ID: t.ID}
		meta := doc{
			ID:      t.ID,
			Name:    t.Title,
			URL:     t.URL,
			Version: textVersion(text),
			Text:    text,
		}
		entries, ok := c.freshParse(d, meta, now)
		if !ok {
			var err error
			entries, err = c.parse(ctx, meta, t.Course, now)
			if err != nil {
				c.log.Warn("gdoc: could not read this calendar; leaving it as it is",
					"task", t.ID, "title", t.Title, "err", err)
				st.Failed++
				out = append(out, t)
				continue
			}
		}
		rows := c.tasks(meta, t.Course, entries, now)
		if len(rows) == 0 {
			out = append(out, t)
			continue
		}
		for i := range rows {
			rows[i].CourseID = t.CourseID
		}
		st.Expanded++
		st.Rows += len(rows)
		c.log.Info("gdoc: read a calendar posted in the LMS", "task", t.ID, "title", t.Title,
			"course", t.Course, "rows", len(rows))
		out = append(out, rows...)
	}
	return out, st
}

// looksLikeCalendar reports whether an LMS item is a calendar resource. Only
// information is considered: an assignment called "Calendar quiz" is work in
// its own right, not a schedule to be taken apart.
func looksLikeCalendar(t model.Task) bool {
	return t.Kind.Informational() && strings.Contains(strings.ToLower(t.Title), "calendar")
}

// calendarText is everything the item says: its own text and the text of
// every file attached to it.
func calendarText(t model.Task) (string, bool) {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(t.Description))
	for _, a := range t.Attachments {
		if strings.TrimSpace(a.Content) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("--- " + a.Name + " ---\n" + a.Content)
	}
	text := b.String()
	return text, len(text) >= minCalendarText
}

// alreadyConfigured reports whether this item is a document CALENDAR_DOCS
// already names, by the file id in any of its links.
func alreadyConfigured(t model.Task, skip map[string]bool) bool {
	if len(skip) == 0 {
		return false
	}
	for id := range skip {
		if strings.Contains(t.URL, id) {
			return true
		}
		for _, a := range t.Attachments {
			if strings.Contains(a.URL, id) {
				return true
			}
		}
	}
	return false
}

// textVersion stands in for a Drive revision: an LMS item's text is all the
// identity it has, so a parse is reused until the teacher edits the words.
func textVersion(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "text:" + hex.EncodeToString(sum[:12])
}
