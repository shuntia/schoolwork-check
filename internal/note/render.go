// Package note pushes the unified task table into the user's note server
// (~/Projects/note). render.go turns a model.Task into the three text fields
// note stores; it has no dependency on the wire protocol, which lives in
// client.go once the server contract is settled.
package note

import (
	"fmt"
	"strings"
	"time"

	"schoolwork-check/internal/model"
)

// Sentinel is the last line of notes for tasks created before note had an
// external_id column. Kept so old rows remain matchable.
const Sentinel = "schoolwork-check-id: "

// Limits mirror the caps note enforces (16 KiB on description and notes,
// 500 bytes on title) with headroom for the server's own byte counting.
const (
	maxTitleBytes = 480
	maxTextBytes  = 15 * 1024
	maxAttachText = 6 * 1024 // per attachment, so several fit under maxTextBytes
)

// Rendered is what the sink writes for one task.
type Rendered struct {
	ExternalID  string
	Title       string
	Description string
	Notes       string
	DueAt       *time.Time
	URL         string
	State       string // open | in_progress | done
	// OwnsDescription is false when note's agent owns the description; sync
	// then never sends or compares it.
	OwnsDescription bool
	// NotHomework is the model's reason when it judged the item asks nothing
	// of the student; "" otherwise (including when nothing was judged).
	NotHomework string
}

// RenderOptions controls formatting.
type RenderOptions struct {
	CoursePrefix bool           // "Course — Title" instead of bare title
	Location     *time.Location // for human dates in description; nil = UTC
	// AgentOwnsDescription is on when note's agent writes the description
	// (Options.BriefContext set). The LMS header and teacher text then open
	// notes instead, and handout text stays out of notes because the agent
	// receives it as context.
	AgentOwnsDescription bool
}

// Render converts one task row into note fields.
func Render(t model.Task, o RenderOptions) Rendered {
	loc := o.Location
	if loc == nil {
		loc = time.UTC
	}

	// Titles are one line in note; Classroom allows line breaks in them.
	title := strings.Join(strings.Fields(t.Title), " ")
	if title == "" {
		title = "(untitled)"
	}
	if course := strings.Join(strings.Fields(t.Course), " "); o.CoursePrefix && course != "" {
		title = course + " — " + title
	}
	title = truncateBytes(title, maxTitleBytes, "…")

	var d strings.Builder
	if t.DueAt != nil {
		// Absolute only. A relative "in 3 days" would change the rendering
		// as the clock moves and make every run PATCH every dated task.
		fmt.Fprintf(&d, "Due: %s\n", t.DueAt.In(loc).Format("Mon Jan 2 15:04"))
	} else {
		d.WriteString("Due: none\n")
	}
	if t.Course != "" {
		fmt.Fprintf(&d, "Course: %s\n", t.Course)
	}
	fmt.Fprintf(&d, "Kind: %s", t.Kind)
	if t.Points != nil {
		fmt.Fprintf(&d, " · %g pts", *t.Points)
	}
	d.WriteString("\n")
	if e := t.Enrichment; e != nil && e.EstimateMin != nil && !o.AgentOwnsDescription {
		fmt.Fprintf(&d, "Estimate: ~%s\n", FormatMinutes(*e.EstimateMin))
	}
	if t.URL != "" {
		fmt.Fprintf(&d, "Link: %s\n", t.URL)
	}
	brief := t.Enrichment != nil && !o.AgentOwnsDescription
	if brief {
		writeBrief(&d, *t.Enrichment)
	}
	if desc := strings.TrimSpace(t.Description); desc != "" {
		if brief {
			d.WriteString("\nFrom the teacher:")
		}
		d.WriteString("\n" + desc + "\n")
	}
	if len(t.Attachments) > 0 {
		d.WriteString("\nAttachments:\n")
		for _, a := range t.Attachments {
			d.WriteString("- " + attachmentLine(a) + "\n")
		}
	}
	description := truncateBytes(strings.TrimRight(d.String(), "\n"), maxTextBytes, "\n…")

	var n strings.Builder
	if o.AgentOwnsDescription {
		n.WriteString(description + "\n\n")
		description = ""
	}
	n.WriteString(progressLine(t.Progress, loc))
	if txt := strings.TrimSpace(t.Progress.Text); txt != "" {
		n.WriteString("\nSubmitted text:\n" + txt + "\n")
	}
	if len(t.Progress.Attachments) > 0 {
		n.WriteString("\nSubmitted files:\n")
		for _, a := range t.Progress.Attachments {
			n.WriteString("- " + attachmentLine(a) + "\n")
		}
	}
	// Extracted content: the student's own work first (progress), then the
	// teacher's handouts, each capped so several fit.
	for _, a := range t.Progress.Attachments {
		writeContent(&n, "Your file", a)
	}
	if !o.AgentOwnsDescription {
		for _, a := range t.Attachments {
			writeContent(&n, "Handout", a)
		}
	}
	notes := strings.TrimRight(n.String(), "\n")
	sentinel := "\n" + Sentinel + t.ID
	notes = truncateBytes(notes, maxTextBytes-len(sentinel), "\n…") + sentinel

	return Rendered{
		ExternalID:      t.ID,
		Title:           title,
		Description:     description,
		Notes:           notes,
		DueAt:           t.DueAt,
		URL:             t.URL,
		State:           MapState(t.Progress.State),
		OwnsDescription: !o.AgentOwnsDescription,
		NotHomework:     notHomework(t.Enrichment),
	}
}

func notHomework(e *model.Enrichment) string {
	if e == nil || e.Homework == nil || *e.Homework {
		return ""
	}
	if e.SkipReason == "" {
		return "not homework"
	}
	return e.SkipReason
}

// writeBrief puts the model's reading aid above the teacher's own text. It is
// labelled as generated so nobody mistakes it for the assignment itself.
func writeBrief(b *strings.Builder, e model.Enrichment) {
	b.WriteString("\n" + strings.TrimSpace(e.Summary) + "\n")
	if e.Deliverable != "" {
		b.WriteString("Hand in: " + e.Deliverable + "\n")
	}
	if len(e.Steps) > 0 {
		b.WriteString("\nSteps:\n")
		for i, s := range e.Steps {
			fmt.Fprintf(b, "%d. %s\n", i+1, s)
		}
	}
	if len(e.Requirements) > 0 {
		b.WriteString("\nRequirements:\n")
		for _, r := range e.Requirements {
			b.WriteString("- " + r + "\n")
		}
	}
	b.WriteString("(brief generated by " + e.Model + ")\n")
}

// FormatMinutes renders an estimate as "45 min", "2 h" or "2.5 h".
func FormatMinutes(m int) string {
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	h := float64(m) / 60
	return strings.TrimSuffix(fmt.Sprintf("%.1f", h), ".0") + " h"
}

// MapState collapses the LMS progress state onto note's task states.
// Missing work is still open work; excused work is finished as far as the
// student is concerned.
func MapState(s model.State) string {
	switch s {
	case model.StateSubmitted, model.StateGraded, model.StateExcused:
		return "done"
	case model.StateInProgress:
		return "in_progress"
	default:
		return "open"
	}
}

func progressLine(p model.Progress, loc *time.Location) string {
	parts := []string{"Progress: " + string(p.State)}
	if p.Grade != "" {
		parts = append(parts, "grade "+p.Grade)
	}
	if p.SubmittedAt != nil {
		parts = append(parts, "submitted "+p.SubmittedAt.In(loc).Format("Mon Jan 2 15:04"))
	}
	if p.Late {
		parts = append(parts, "late")
	}
	return strings.Join(parts, ", ") + "\n"
}

func attachmentLine(a model.Attachment) string {
	name := a.Name
	if name == "" {
		name = a.URL
	}
	if a.URL != "" && a.URL != name {
		return fmt.Sprintf("%s (%s)", name, a.URL)
	}
	return name
}

func writeContent(b *strings.Builder, label string, a model.Attachment) {
	c := strings.TrimSpace(a.Content)
	if c == "" {
		return
	}
	c = truncateBytes(c, maxAttachText, "\n…")
	fmt.Fprintf(b, "\n--- %s: %s ---\n%s\n", label, a.Name, c)
}

// truncateBytes cuts s to at most n bytes on a rune boundary, appending tail
// (which counts toward n) when it cut anything.
func truncateBytes(s string, n int, tail string) string {
	if len(s) <= n {
		return s
	}
	cut := n - len(tail)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + tail
}
