package output

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"schoolwork-check/internal/model"
)

func ptrTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func ptrFloat(f float64) *float64 { return &f }

// fixedLocation pins Markdown formatting to UTC for the duration of a test.
func fixedLocation(t *testing.T) {
	t.Helper()
	prev := Location
	Location = time.UTC
	t.Cleanup(func() { Location = prev })
}

// sample returns three tasks in deliberately wrong order.
func sample() []model.Task {
	fetched := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return []model.Task{
		{
			ID:       "canvas:assignment:3",
			Source:   model.SourceCanvas,
			Kind:     model.KindMaterial,
			Course:   "Zoology",
			CourseID: "z1",
			Title:    "Reading: cells",
			URL:      "https://canvas.example.edu/courses/1/assignments/3",
			// No due date: must sort last.
			Description: "Read chapter 2.",
			Progress:    model.Progress{State: model.StateNotStarted},
			FetchedAt:   fetched,
		},
		{
			ID:         "classroom:c9:w2",
			Source:     model.SourceClassroom,
			Kind:       model.KindQuiz,
			Course:     "Biology 101",
			CourseID:   "c9",
			Title:      "Quiz 2",
			URL:        "https://classroom.google.com/c/c9/a/w2",
			AssignedAt: ptrTime("2026-03-02T09:00:00Z"),
			DueAt:      ptrTime("2026-03-10T23:59:00Z"),
			Points:     ptrFloat(20),
			Progress: model.Progress{
				State:       model.StateSubmitted,
				SubmittedAt: ptrTime("2026-03-09T18:30:00Z"),
				Text:        "my answers",
			},
			FetchedAt: fetched,
		},
		{
			ID:          "canvas:assignment:1",
			Source:      model.SourceCanvas,
			Kind:        model.KindAssignment,
			Course:      "Algebra | II",
			CourseID:    "a7",
			Title:       "Problem set 1",
			URL:         "https://canvas.example.edu/courses/2/assignments/1",
			AssignedAt:  ptrTime("2026-03-01T08:00:00Z"),
			DueAt:       ptrTime("2026-03-05T17:00:00Z"),
			Points:      ptrFloat(12.5),
			Description: "Do problems 1-10.\nShow your work.",
			Attachments: []model.Attachment{
				{Name: "worksheet.pdf", URL: "https://files.example.edu/worksheet.pdf",
					MimeType: "application/pdf", Content: "Problem 1: solve for x."},
				{Name: "rubric.docx", URL: "https://files.example.edu/rubric.docx",
					ContentError: "extract: unsupported type"},
			},
			Progress: model.Progress{
				State: model.StateGraded,
				Grade: "11/12.5",
				Late:  true,
				Attachments: []model.Attachment{
					{Name: "my-answers.pdf", URL: "https://files.example.edu/mine.pdf"},
				},
				SubmittedAt: ptrTime("2026-03-05T20:00:00Z"),
			},
			FetchedAt: fetched,
		},
	}
}

func TestSortOrder(t *testing.T) {
	tasks := sample()
	Sort(tasks)
	want := []string{"canvas:assignment:1", "classroom:c9:w2", "canvas:assignment:3"}
	for i, id := range want {
		if tasks[i].ID != id {
			t.Fatalf("position %d = %s, want %s", i, tasks[i].ID, id)
		}
	}
}

func TestSortTieBreakOnCourseThenTitle(t *testing.T) {
	due := ptrTime("2026-03-05T17:00:00Z")
	tasks := []model.Task{
		{ID: "d", Course: "B", Title: "b", DueAt: due},
		{ID: "c", Course: "B", Title: "a", DueAt: due},
		{ID: "b", Course: "A", Title: "z", DueAt: due},
		{ID: "a", Course: "A", Title: "y"},
	}
	Sort(tasks)
	want := []string{"b", "c", "d", "a"}
	for i, id := range want {
		if tasks[i].ID != id {
			t.Fatalf("position %d = %s, want %s (%v)", i, tasks[i].ID, id, tasks)
		}
	}
}

func TestWritersDoNotReorderCallerSlice(t *testing.T) {
	tasks := sample()
	first := tasks[0].ID
	var buf bytes.Buffer
	if err := WriteJSON(&buf, tasks); err != nil {
		t.Fatal(err)
	}
	if tasks[0].ID != first {
		t.Fatalf("caller slice was reordered")
	}
}

func TestWriteMarkdownHeaderAndRow(t *testing.T) {
	fixedLocation(t)
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(buf.String(), "\n")

	wantHeader := "| Due | Course | Title | Kind | State | Grade | Assigned | Attachments | Progress |"
	if lines[0] != wantHeader {
		t.Fatalf("header = %q", lines[0])
	}
	wantSep := "| --- | --- | --- | --- | --- | --- | --- | --- | --- |"
	if lines[1] != wantSep {
		t.Fatalf("separator = %q", lines[1])
	}
	wantRow := "| Thu Mar 5 17:00 | Algebra \\| II | Problem set 1 | assignment | graded | 11/12.5 | " +
		"Sun Mar 1 08:00 | 2: worksheet.pdf, rubric.docx | graded, 1 file |"
	if lines[2] != wantRow {
		t.Fatalf("row = %q\nwant  %q", lines[2], wantRow)
	}
	wantUndated := "| — | Zoology | Reading: cells | material | not_started | — | — | — | not_started |"
	if lines[4] != wantUndated {
		t.Fatalf("undated row = %q\nwant %q", lines[4], wantUndated)
	}
}

func TestWriteMarkdownDetails(t *testing.T) {
	fixedLocation(t)
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"\n## Details\n",
		"### Problem set 1",
		"Do problems 1-10.\nShow your work.",
		"- [worksheet.pdf](https://files.example.edu/worksheet.pdf)",
		"<details><summary>worksheet.pdf</summary>",
		"```\nProblem 1: solve for x.\n```",
		"</details>",
		"Submitted:",
		"- [my-answers.pdf](https://files.example.edu/mine.pdf)",
		"rubric.docx](https://files.example.edu/rubric.docx) — extract: unsupported type",
		"Submission text:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, out)
		}
	}
}

func TestWriteMarkdownTruncatesLongText(t *testing.T) {
	fixedLocation(t)
	tasks := []model.Task{{
		ID: "x", Title: "Long", Course: "C",
		Description: strings.Repeat("d", 1000),
		Attachments: []model.Attachment{{Name: "a.txt", Content: strings.Repeat("c", 2000)}},
	}}
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, tasks); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, strings.Repeat("d", descriptionLimit)+"…") {
		t.Error("description not truncated to 800 chars with ellipsis")
	}
	if strings.Contains(out, strings.Repeat("d", descriptionLimit+1)) {
		t.Error("description longer than limit")
	}
	if !strings.Contains(out, strings.Repeat("c", contentLimit)+"…") {
		t.Error("content not truncated to 1500 chars with ellipsis")
	}
}

func TestWriteMarkdownEmpty(t *testing.T) {
	fixedLocation(t)
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "| Due | Course |") {
		t.Fatalf("got %q", buf.String())
	}
	if strings.Contains(buf.String(), "## Details") {
		t.Fatal("empty output should not have a details section")
	}
}

func TestWriteCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("got %d records, want 4", len(records))
	}
	wantHeader := []string{
		"id", "source", "kind", "course", "title", "url",
		"assigned_at", "due_at", "points", "state", "grade",
		"submitted_at", "late", "attachment_count", "attachment_names", "description",
	}
	if !reflect.DeepEqual(records[0], wantHeader) {
		t.Fatalf("header = %v", records[0])
	}
	wantRow := []string{
		"canvas:assignment:1", "canvas", "assignment", "Algebra | II", "Problem set 1",
		"https://canvas.example.edu/courses/2/assignments/1",
		"2026-03-01T08:00:00Z", "2026-03-05T17:00:00Z", "12.5", "graded", "11/12.5",
		"2026-03-05T20:00:00Z", "true", "2", "worksheet.pdf, rubric.docx",
		"Do problems 1-10.\nShow your work.",
	}
	if !reflect.DeepEqual(records[1], wantRow) {
		t.Fatalf("row = %#v\nwant  %#v", records[1], wantRow)
	}
	// Undated task last, with empty date columns.
	last := records[3]
	if last[0] != "canvas:assignment:3" || last[6] != "" || last[7] != "" || last[8] != "" {
		t.Fatalf("last row = %#v", last)
	}
	if last[12] != "false" || last[13] != "0" || last[14] != "" {
		t.Fatalf("last row attachment/late columns = %#v", last)
	}
}

func TestWriteJSONRoundTrip(t *testing.T) {
	tasks := sample()
	var buf bytes.Buffer
	if err := WriteJSON(&buf, tasks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\n  {\n") {
		t.Fatal("expected pretty-printed JSON")
	}

	var got []model.Task
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := sample()
	Sort(want)
	for i := range want {
		// WriteJSON normalises nil attachment lists to [] so consumers never see null.
		if want[i].Attachments == nil {
			want[i].Attachments = []model.Attachment{}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d", len(got), len(want))
	}
	for i := range want {
		if !tasksEqual(got[i], want[i]) {
			t.Fatalf("task %d differs:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}

	// Field order follows the struct definition.
	first := strings.Index(buf.String(), `"id"`)
	second := strings.Index(buf.String(), `"source"`)
	third := strings.Index(buf.String(), `"kind"`)
	if !(first < second && second < third) {
		t.Fatal("unexpected JSON field order")
	}
}

func TestWriteJSONEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("got %q, want []", buf.String())
	}
}

// tasksEqual compares tasks with time values normalized to UTC.
func tasksEqual(a, b model.Task) bool {
	return reflect.DeepEqual(normalize(a), normalize(b))
}

func normalize(t model.Task) model.Task {
	t.FetchedAt = t.FetchedAt.UTC()
	t.AssignedAt = utcPtr(t.AssignedAt)
	t.DueAt = utcPtr(t.DueAt)
	t.Progress.SubmittedAt = utcPtr(t.Progress.SubmittedAt)
	return t
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
