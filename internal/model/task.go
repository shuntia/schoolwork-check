// Package model defines the unified task table shared by every source.
// Every source adapter (Canvas, Google Classroom, ...) must produce []Task.
// This file is the contract between packages: change it deliberately.
package model

import "time"

// Source identifies which LMS a task came from.
type Source string

const (
	SourceCanvas    Source = "canvas"
	SourceClassroom Source = "classroom"
	// SourceGDoc is a course calendar kept in a document (Google Doc,
	// Sheet, or an uploaded file) rather than in an LMS.
	SourceGDoc Source = "gdoc"
	// SourceGCal is a shared Google Calendar: meetings and events, which
	// are information rather than work.
	SourceGCal Source = "gcal"
)

// Kind is the coarse type of the learning object.
type Kind string

const (
	KindAssignment   Kind = "assignment"
	KindQuiz         Kind = "quiz"
	KindDiscussion   Kind = "discussion"
	KindMaterial     Kind = "material"     // reading / reference, nothing to submit
	KindQuestion     Kind = "question"     // Classroom short-answer / multiple choice
	KindAnnouncement Kind = "announcement" // a post to the class, not work in itself
	KindOther        Kind = "other"
)

// Informational reports whether items of this kind are information rather
// than work: they go to note's agent inbox instead of becoming tasks.
func (k Kind) Informational() bool {
	return k == KindMaterial || k == KindAnnouncement
}

// State is the student's progress on a task.
type State string

const (
	StateNotStarted State = "not_started" // nothing submitted, not yet due
	StateInProgress State = "in_progress" // draft / partial work exists but not submitted
	StateSubmitted  State = "submitted"   // turned in, awaiting grade
	StateGraded     State = "graded"      // graded / returned
	StateMissing    State = "missing"     // past due, nothing submitted
	StateExcused    State = "excused"
)

// Task is one row of the unified table.
type Task struct {
	// ID is stable across runs: "<source>:<native id>", e.g. "canvas:assignment:12345"
	// or "classroom:<courseId>:<courseWorkId>". Used for dedup downstream.
	ID     string `json:"id"`
	Source Source `json:"source"`
	Kind   Kind   `json:"kind"`

	Course     string     `json:"course"`    // human course name
	CourseID   string     `json:"course_id"` // native course id
	Title      string     `json:"title"`
	URL        string     `json:"url"`                   // deep link into the LMS
	AssignedAt *time.Time `json:"assigned_at,omitempty"` // when it became visible / was posted
	DueAt      *time.Time `json:"due_at,omitempty"`      // nil when undated
	// EndsAt is set only by sources whose items occupy a span rather than
	// falling due at an instant — a meeting on a calendar. Nil everywhere
	// else: homework ends when it is handed in.
	EndsAt *time.Time `json:"ends_at,omitempty"`
	Points *float64   `json:"points,omitempty"` // max points, nil when ungraded

	// Description is plain text (HTML already stripped). Keep formatting
	// like lists as newlines so an LLM downstream can read it.
	Description string `json:"description"`

	// Attachments are files/links the teacher attached to the task.
	Attachments []Attachment `json:"attachments"`

	// Progress is the student's own work on the task.
	Progress Progress `json:"progress"`

	// Enrichment is the language-model brief written by internal/enrich.
	// Nil when enrichment is off, failed, or skipped (finished work).
	Enrichment *Enrichment `json:"enrichment,omitempty"`

	FetchedAt time.Time `json:"fetched_at"`
}

// Attachment is a file or link attached to a task or a submission.
type Attachment struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	MimeType  string `json:"mime_type,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`

	// Content is extracted plain text (PDF, DOCX, Google Docs, text files, ...).
	// Empty when extraction was not attempted or not possible.
	Content string `json:"content,omitempty"`
	// ContentError explains why Content is empty, when extraction was attempted.
	ContentError string `json:"content_error,omitempty"`
	// Truncated is true when Content was cut at the configured byte limit.
	Truncated bool `json:"truncated,omitempty"`
}

// Progress is the student's state and work on a task.
type Progress struct {
	State       State      `json:"state"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	Late        bool       `json:"late,omitempty"`
	Grade       string     `json:"grade,omitempty"` // as displayed by the LMS, e.g. "8/10" or "A"

	// Text is the student's typed submission body (plain text), if any.
	Text string `json:"text,omitempty"`
	// Attachments are the student's submitted files / linked Google Docs,
	// with Content extracted where possible so downstream can judge how far along the work is.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Enrichment is derived by a language model from the teacher-side text of a
// task: its description and attachment contents. It only adds reading aids;
// every deterministic field (ids, dates, points, state) stays on Task.
type Enrichment struct {
	// Homework is false when the model judged the item asks nothing of the
	// student (optional forum, announcement, reference only). Sinks may
	// decline to create such tasks; nil means not judged.
	Homework     *bool    `json:"homework,omitempty"`
	SkipReason   string   `json:"skip_reason,omitempty"`
	Summary      string   `json:"summary"`
	Deliverable  string   `json:"deliverable,omitempty"`  // what is handed in, and how
	Steps        []string `json:"steps,omitempty"`        // ordered concrete actions
	Requirements []string `json:"requirements,omitempty"` // constraints stated by the teacher
	EstimateMin  *int     `json:"estimate_min,omitempty"` // minutes of focused work, nil when unknowable
	Model        string   `json:"model"`
}
