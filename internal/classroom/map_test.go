package classroom

import (
	"io"
	"strings"
	"testing"
	"time"

	gclassroom "google.golang.org/api/classroom/v1"

	"schoolwork-check/internal/model"
)

func TestCombineDue(t *testing.T) {
	tests := []struct {
		name string
		date *gclassroom.Date
		clk  *gclassroom.TimeOfDay
		want *time.Time
	}{
		{"no date", nil, &gclassroom.TimeOfDay{Hours: 23}, nil},
		{"partial date", &gclassroom.Date{Year: 2026, Month: 3}, nil, nil},
		{
			"date and time are UTC",
			&gclassroom.Date{Year: 2026, Month: 3, Day: 10},
			&gclassroom.TimeOfDay{Hours: 23, Minutes: 59},
			ptr(time.Date(2026, 3, 10, 23, 59, 0, 0, time.UTC)),
		},
		{
			"midnight when dueTime is absent",
			&gclassroom.Date{Year: 2026, Month: 12, Day: 31},
			nil,
			ptr(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)),
		},
		{
			"seconds and nanos are carried",
			&gclassroom.Date{Year: 2026, Month: 1, Day: 2},
			&gclassroom.TimeOfDay{Hours: 3, Minutes: 4, Seconds: 5, Nanos: 6},
			ptr(time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := combineDue(tc.date, tc.clk)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %v, want nil", got)
			case tc.want == nil:
				return
			case got == nil:
				t.Fatalf("got nil, want %v", tc.want)
			case !got.Equal(*tc.want):
				t.Fatalf("got %v, want %v", got, tc.want)
			case got.Location() != time.UTC:
				t.Fatalf("location = %v, want UTC", got.Location())
			}
		})
	}
}

func TestMapState(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	past := ptr(now.Add(-24 * time.Hour))
	future := ptr(now.Add(24 * time.Hour))

	withAttachment := &gclassroom.AssignmentSubmission{
		Attachments: []*gclassroom.Attachment{{DriveFile: &gclassroom.DriveFile{Id: "x"}}},
	}

	tests := []struct {
		name string
		sub  *gclassroom.StudentSubmission
		due  *time.Time
		want model.State
	}{
		{"no submission, not yet due", nil, future, model.StateNotStarted},
		{"no submission, overdue", nil, past, model.StateMissing},
		{"no submission, undated", nil, nil, model.StateNotStarted},

		{"turned in", &gclassroom.StudentSubmission{State: subStateTurnedIn}, past, model.StateSubmitted},
		{
			"returned with a grade",
			&gclassroom.StudentSubmission{State: subStateReturned, AssignedGrade: 8},
			past, model.StateGraded,
		},
		{
			"returned with an explicit zero grade",
			&gclassroom.StudentSubmission{
				State: subStateReturned,
				SubmissionHistory: []*gclassroom.SubmissionHistory{{
					GradeHistory: &gclassroom.GradeHistory{GradeChangeType: "ASSIGNED_GRADE_POINTS_EARNED_CHANGE"},
				}},
			},
			past, model.StateGraded,
		},
		{
			"returned without a grade is back with the student",
			&gclassroom.StudentSubmission{State: subStateReturned},
			past, model.StateSubmitted,
		},

		{
			"draft attachments count as in progress",
			&gclassroom.StudentSubmission{State: subStateCreated, AssignmentSubmission: withAttachment},
			past, model.StateInProgress,
		},
		{
			"typed answer counts as in progress",
			&gclassroom.StudentSubmission{
				State:                 subStateNew,
				ShortAnswerSubmission: &gclassroom.ShortAnswerSubmission{Answer: "42"},
			},
			future, model.StateInProgress,
		},
		{
			"multiple choice answer counts as in progress",
			&gclassroom.StudentSubmission{
				State:                    subStateReclaimed,
				MultipleChoiceSubmission: &gclassroom.MultipleChoiceSubmission{Answer: "b"},
			},
			future, model.StateInProgress,
		},
		{
			"reclaimed and empty, overdue",
			&gclassroom.StudentSubmission{State: subStateReclaimed},
			past, model.StateMissing,
		},
		{
			"created and empty, not yet due",
			&gclassroom.StudentSubmission{State: subStateCreated},
			future, model.StateNotStarted,
		},
		{
			"unknown state falls back on contents",
			&gclassroom.StudentSubmission{State: "SOMETHING_NEW", AssignmentSubmission: withAttachment},
			past, model.StateInProgress,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapState(tc.sub, tc.due, now); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkKind(t *testing.T) {
	for in, want := range map[string]model.Kind{
		"ASSIGNMENT":                   model.KindAssignment,
		"SHORT_ANSWER_QUESTION":        model.KindQuestion,
		"MULTIPLE_CHOICE_QUESTION":     model.KindQuestion,
		"":                             model.KindOther,
		"COURSE_WORK_TYPE_UNSPECIFIED": model.KindOther,
	} {
		if got := workKind(in); got != want {
			t.Errorf("workKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatGrade(t *testing.T) {
	tests := []struct {
		grade, max float64
		want       string
	}{
		{8, 10, "8/10"},
		{8.5, 10, "8.5/10"},
		{0, 10, "0/10"},
		{7, 0, "7"},
		{100, 100, "100/100"},
	}
	for _, tc := range tests {
		if got := formatGrade(tc.grade, tc.max); got != tc.want {
			t.Errorf("formatGrade(%v, %v) = %q, want %q", tc.grade, tc.max, got, tc.want)
		}
	}
}

func TestAssignedGrade(t *testing.T) {
	if _, ok := assignedGrade(nil); ok {
		t.Error("nil submission should have no grade")
	}
	if _, ok := assignedGrade(&gclassroom.StudentSubmission{State: subStateTurnedIn}); ok {
		t.Error("ungraded submission should have no grade")
	}
	g, ok := assignedGrade(&gclassroom.StudentSubmission{AssignedGrade: 3.5})
	if !ok || g != 3.5 {
		t.Errorf("got %v/%v, want 3.5/true", g, ok)
	}
	// An assigned zero is only visible through the grade history.
	g, ok = assignedGrade(&gclassroom.StudentSubmission{
		SubmissionHistory: []*gclassroom.SubmissionHistory{{
			GradeHistory: &gclassroom.GradeHistory{
				GradeChangeType: "ASSIGNED_GRADE_POINTS_EARNED_CHANGE",
				PointsEarned:    0,
			},
		}},
	})
	if !ok || g != 0 {
		t.Errorf("got %v/%v, want 0/true", g, ok)
	}
	// A draft grade is not an assigned grade.
	if _, ok := assignedGrade(&gclassroom.StudentSubmission{
		DraftGrade: 9,
		SubmissionHistory: []*gclassroom.SubmissionHistory{{
			GradeHistory: &gclassroom.GradeHistory{GradeChangeType: "DRAFT_GRADE_POINTS_EARNED_CHANGE", PointsEarned: 9},
		}},
	}); ok {
		t.Error("draft grade must not count as assigned")
	}
}

func TestLastTurnedIn(t *testing.T) {
	sub := &gclassroom.StudentSubmission{
		State:      subStateReturned,
		UpdateTime: "2026-03-05T00:00:00Z",
		SubmissionHistory: []*gclassroom.SubmissionHistory{
			{StateHistory: &gclassroom.StateHistory{State: subStateTurnedIn, StateTimestamp: "2026-02-01T10:00:00Z"}},
			{StateHistory: &gclassroom.StateHistory{State: subStateReclaimed, StateTimestamp: "2026-02-02T10:00:00Z"}},
			{StateHistory: &gclassroom.StateHistory{State: subStateTurnedIn, StateTimestamp: "2026-02-03T10:00:00Z"}},
			{StateHistory: &gclassroom.StateHistory{State: subStateReturned, StateTimestamp: "2026-03-05T00:00:00Z"}},
		},
	}
	got := lastTurnedIn(sub)
	want := time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Fatalf("got %v, want %v (latest TURNED_IN)", got, want)
	}

	// No usable history: fall back to UpdateTime for a turned-in submission.
	got = lastTurnedIn(&gclassroom.StudentSubmission{State: subStateTurnedIn, UpdateTime: "2026-01-01T00:00:00Z"})
	if got == nil || !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("fallback got %v", got)
	}
	// Nothing turned in at all.
	if got := lastTurnedIn(&gclassroom.StudentSubmission{State: subStateCreated, UpdateTime: "2026-01-01T00:00:00Z"}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	if got := lastTurnedIn(nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestInWindow(t *testing.T) {
	lo := time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)
	inside := ptr(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	tooOld := ptr(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tooNew := ptr(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	if !inWindow(nil, nil, lo, hi) || !inWindow(nil, inside, lo, hi) || !inWindow(nil, tooNew, lo, hi) {
		t.Error("undated task posted recently (or with no post date) must be kept")
	}
	if inWindow(nil, tooOld, lo, hi) {
		t.Error("undated task posted before the window must be dropped")
	}
	if !inWindow(nil, tooOld, time.Time{}, hi) {
		t.Error("undated: zero lower bound should not filter")
	}
	if !inWindow(inside, tooOld, lo, hi) {
		t.Error("in-window task dropped (post date must not matter when dated)")
	}
	if inWindow(tooOld, nil, lo, hi) || inWindow(tooNew, nil, lo, hi) {
		t.Error("out-of-window task kept")
	}
	// A zero bound means unbounded on that side.
	if !inWindow(tooOld, nil, time.Time{}, hi) || !inWindow(tooNew, nil, lo, time.Time{}) {
		t.Error("zero bound should not filter")
	}
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	c := &Client{opts: Options{PastDays: 30, FutureDays: 120}}
	lo, hi := c.window(now)
	if !lo.Equal(now.AddDate(0, 0, -30)) || !hi.Equal(now.AddDate(0, 0, 120)) {
		t.Fatalf("window = %v..%v", lo, hi)
	}
	// Zero/negative means unbounded.
	c = &Client{opts: Options{}}
	if lo, hi = c.window(now); !lo.IsZero() || !hi.IsZero() {
		t.Fatalf("zero options should give an unbounded window, got %v..%v", lo, hi)
	}
}

func TestNormalizeText(t *testing.T) {
	if got := normalizeText("  a\r\nb\rc  "); got != "a\nb\nc" {
		t.Errorf("got %q", got)
	}
	if got := normalizeText(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestCourseName(t *testing.T) {
	tests := []struct {
		course *gclassroom.Course
		want   string
	}{
		{&gclassroom.Course{Id: "C1", Name: "Bio", Section: "P3"}, "Bio"},
		{&gclassroom.Course{Id: "C1", Section: "P3", DescriptionHeading: "Intro"}, "P3"},
		{&gclassroom.Course{Id: "C1", DescriptionHeading: "Intro"}, "Intro"},
		{&gclassroom.Course{Id: "C1", Name: "   "}, "C1"},
	}
	for _, tc := range tests {
		if got := courseName(tc.course); got != tc.want {
			t.Errorf("courseName(%+v) = %q, want %q", tc.course, got, tc.want)
		}
	}
}

func TestReadCapped(t *testing.T) {
	s, truncated, err := readCapped(stringReader("hello"), 100)
	if err != nil || s != "hello" || truncated {
		t.Fatalf("got %q/%v/%v", s, truncated, err)
	}
	s, truncated, err = readCapped(stringReader("hello"), 3)
	if err != nil || s != "hel" || !truncated {
		t.Fatalf("got %q/%v/%v", s, truncated, err)
	}
}

func ptr[T any](v T) *T { return &v }

func stringReader(s string) io.Reader { return strings.NewReader(s) }
