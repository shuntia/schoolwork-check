package output

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"schoolwork-check/internal/model"
)

// csvHeader is the fixed column order of WriteCSV.
var csvHeader = []string{
	"id", "source", "kind", "course", "title", "url",
	"assigned_at", "due_at", "points", "state", "grade",
	"submitted_at", "late", "attachment_count", "attachment_names", "description",
}

// WriteCSV writes one row per task, dates as RFC3339 in UTC (empty when nil).
func WriteCSV(w io.Writer, tasks []model.Task) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return fmt.Errorf("output: write csv: %w", err)
	}
	for _, t := range sorted(tasks) {
		points := ""
		if t.Points != nil {
			points = strconv.FormatFloat(*t.Points, 'f', -1, 64)
		}
		row := []string{
			t.ID,
			string(t.Source),
			string(t.Kind),
			t.Course,
			t.Title,
			t.URL,
			formatRFC3339(t.AssignedAt),
			formatRFC3339(t.DueAt),
			points,
			string(t.Progress.State),
			t.Progress.Grade,
			formatRFC3339(t.Progress.SubmittedAt),
			strconv.FormatBool(t.Progress.Late),
			strconv.Itoa(len(t.Attachments)),
			strings.Join(attachmentNames(t.Attachments), ", "),
			t.Description,
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("output: write csv: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("output: write csv: %w", err)
	}
	return nil
}
