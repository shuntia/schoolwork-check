package output

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"schoolwork-check/internal/model"
)

const (
	attachmentsCellWidth = 60
	descriptionLimit     = 800
	contentLimit         = 1500
)

var markdownColumns = []string{
	"Due", "Course", "Title", "Kind", "State", "Grade", "Assigned", "Attachments", "Progress",
}

// WriteMarkdown writes a human-readable table of the tasks followed by a
// "## Details" section with descriptions and extracted attachment text.
func WriteMarkdown(w io.Writer, tasks []model.Task) error {
	bw := bufio.NewWriter(w)
	list := sorted(tasks)

	bw.WriteString("| " + strings.Join(markdownColumns, " | ") + " |\n")
	bw.WriteString("|" + strings.Repeat(" --- |", len(markdownColumns)) + "\n")

	if len(list) == 0 {
		bw.WriteString("| " + strings.Repeat(missing+" | ", len(markdownColumns)-1) + missing + " |\n")
	}
	for _, t := range list {
		bw.WriteString("| " + strings.Join(markdownRow(t), " | ") + " |\n")
	}

	if len(list) > 0 {
		bw.WriteString("\n## Details\n")
		for _, t := range list {
			writeDetails(bw, t)
		}
	}

	if err := bw.Flush(); err != nil {
		return fmt.Errorf("output: write markdown: %w", err)
	}
	return nil
}

func markdownRow(t model.Task) []string {
	return []string{
		escapeCell(formatTime(t.DueAt)),
		escapeCell(orMissing(t.Course)),
		escapeCell(orMissing(t.Title)),
		escapeCell(orMissing(string(t.Kind))),
		escapeCell(orMissing(string(t.Progress.State))),
		escapeCell(orMissing(t.Progress.Grade)),
		escapeCell(formatTime(t.AssignedAt)),
		escapeCell(attachmentsCell(t.Attachments)),
		escapeCell(progressCell(t.Progress)),
	}
}

// attachmentsCell is "2: notes.pdf, rubric.docx", clipped to a readable width.
func attachmentsCell(as []model.Attachment) string {
	if len(as) == 0 {
		return missing
	}
	names := truncateRunes(strings.Join(attachmentNames(as), ", "), attachmentsCellWidth)
	return fmt.Sprintf("%d: %s", len(as), names)
}

// progressCell is "submitted, text, 2 files".
func progressCell(p model.Progress) string {
	parts := []string{}
	if p.State != "" {
		parts = append(parts, string(p.State))
	}
	if strings.TrimSpace(p.Text) != "" {
		parts = append(parts, "text")
	}
	if n := len(p.Attachments); n > 0 {
		unit := "files"
		if n == 1 {
			unit = "file"
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, unit))
	}
	if len(parts) == 0 {
		return missing
	}
	return strings.Join(parts, ", ")
}

func writeDetails(w *bufio.Writer, t model.Task) {
	fmt.Fprintf(w, "\n### %s\n\n", oneLine(orMissing(t.Title)))

	meta := []string{}
	if t.Course != "" {
		meta = append(meta, t.Course)
	}
	if t.Kind != "" {
		meta = append(meta, string(t.Kind))
	}
	meta = append(meta, "due "+formatTime(t.DueAt))
	if t.URL != "" {
		meta = append(meta, "["+string(t.Source)+"]("+t.URL+")")
	}
	fmt.Fprintf(w, "%s\n", strings.Join(meta, " · "))

	if e := t.Enrichment; e != nil {
		if e.Homework != nil && !*e.Homework {
			fmt.Fprintf(w, "\n> **Not homework:** %s\n>", oneLine(orMissing(e.SkipReason)))
		}
		fmt.Fprintf(w, "\n> %s\n", oneLine(e.Summary))
		if e.Deliverable != "" {
			fmt.Fprintf(w, ">\n> **Hand in:** %s\n", oneLine(e.Deliverable))
		}
		if e.EstimateMin != nil {
			fmt.Fprintf(w, ">\n> **Estimate:** ~%d min\n", *e.EstimateMin)
		}
		if len(e.Steps) > 0 {
			w.WriteString(">\n")
		}
		for i, s := range e.Steps {
			fmt.Fprintf(w, "> %d. %s\n", i+1, oneLine(s))
		}
		if len(e.Requirements) > 0 {
			fmt.Fprintf(w, ">\n> **Requirements:** %s\n", oneLine(strings.Join(e.Requirements, "; ")))
		}
	}

	if d := strings.TrimSpace(t.Description); d != "" {
		fmt.Fprintf(w, "\n%s\n", truncateRunes(d, descriptionLimit))
	}

	writeAttachmentList(w, "Attachments", t.Attachments)
	writeAttachmentList(w, "Submitted", t.Progress.Attachments)

	if s := strings.TrimSpace(t.Progress.Text); s != "" {
		fmt.Fprintf(w, "\nSubmission text:\n\n%s\n", truncateRunes(s, contentLimit))
	}
}

func writeAttachmentList(w *bufio.Writer, heading string, as []model.Attachment) {
	if len(as) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n\n", heading)
	for _, a := range as {
		name := oneLine(a.Name)
		if name == "" {
			name = "(unnamed)"
		}
		if a.URL != "" {
			fmt.Fprintf(w, "- [%s](%s)", name, a.URL)
		} else {
			fmt.Fprintf(w, "- %s", name)
		}
		if a.ContentError != "" {
			fmt.Fprintf(w, " — %s", oneLine(a.ContentError))
		}
		w.WriteString("\n")

		content := strings.TrimSpace(a.Content)
		if content == "" {
			continue
		}
		body := truncateRunes(content, contentLimit)
		fence := fenceFor(body)
		fmt.Fprintf(w, "\n<details><summary>%s</summary>\n\n%s\n%s\n%s\n\n</details>\n",
			name, fence, body, fence)
	}
}

// fenceFor returns a code fence long enough to contain body.
func fenceFor(body string) string {
	fence := "```"
	for strings.Contains(body, fence) {
		fence += "`"
	}
	return fence
}

func orMissing(s string) string {
	if strings.TrimSpace(s) == "" {
		return missing
	}
	return s
}

// escapeCell makes text safe inside a Markdown table cell.
func escapeCell(s string) string {
	s = oneLine(s)
	return strings.ReplaceAll(s, "|", "\\|")
}
