package canvas

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// ctime is a lenient RFC3339 timestamp. Canvas emits null, "" and occasionally
// date-only values; none of those may fail a whole page decode, so parse errors
// simply yield a nil time.
type ctime struct{ t *time.Time }

func (c *ctime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		c.t = nil
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		c.t = nil
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02"} {
		if tt, err := time.Parse(layout, s); err == nil {
			u := tt.UTC()
			c.t = &u
			return nil
		}
	}
	c.t = nil
	return nil
}

func (c ctime) MarshalJSON() ([]byte, error) {
	if c.t == nil {
		return []byte("null"), nil
	}
	return json.Marshal(c.t.Format(time.RFC3339))
}

// Ptr returns the parsed time, or nil when the field was absent/null/unparseable.
func (c ctime) Ptr() *time.Time { return c.t }

// plannerSubmissions models the planner item "submissions" field, which Canvas
// returns either as the literal false (the plannable cannot be submitted) or as
// an object of boolean flags.
type plannerSubmissions struct {
	// Present is true when Canvas sent an object (or the literal true).
	Present      bool
	Submitted    bool
	Excused      bool
	Graded       bool
	Late         bool
	Missing      bool
	NeedsGrading bool
	HasFeedback  bool
	RedoRequest  bool
}

func (s *plannerSubmissions) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*s = plannerSubmissions{}
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case 't', 'f':
		var v bool
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		// false => not submittable. true carries no detail, but does say the
		// plannable is submittable.
		s.Present = v
		return nil
	case '{':
		var raw struct {
			Submitted    bool `json:"submitted"`
			Excused      bool `json:"excused"`
			Graded       bool `json:"graded"`
			Late         bool `json:"late"`
			Missing      bool `json:"missing"`
			NeedsGrading bool `json:"needs_grading"`
			HasFeedback  bool `json:"has_feedback"`
			RedoRequest  bool `json:"redo_request"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		*s = plannerSubmissions{
			Present:      true,
			Submitted:    raw.Submitted,
			Excused:      raw.Excused,
			Graded:       raw.Graded,
			Late:         raw.Late,
			Missing:      raw.Missing,
			NeedsGrading: raw.NeedsGrading,
			HasFeedback:  raw.HasFeedback,
			RedoRequest:  raw.RedoRequest,
		}
		return nil
	default:
		// Anything else (array, string, number) is ignored rather than fatal.
		return nil
	}
}

// plannerItem is one row of GET /api/v1/planner/items.
type plannerItem struct {
	ContextType   string             `json:"context_type"`
	ContextName   string             `json:"context_name"`
	CourseID      json.Number        `json:"course_id"`
	PlannableType string             `json:"plannable_type"`
	PlannableID   json.Number        `json:"plannable_id"`
	PlannableDate ctime              `json:"plannable_date"`
	HTMLURL       string             `json:"html_url"`
	Submissions   plannerSubmissions `json:"submissions"`
	Plannable     struct {
		ID             json.Number `json:"id"`
		Title          string      `json:"title"`
		Name           string      `json:"name"`
		DueAt          ctime       `json:"due_at"`
		TodoDate       ctime       `json:"todo_date"`
		CreatedAt      ctime       `json:"created_at"`
		UnlockAt       ctime       `json:"unlock_at"`
		PointsPossible *float64    `json:"points_possible"`
	} `json:"plannable"`
}

// apiCourse is one row of GET /api/v1/courses.
type apiCourse struct {
	ID         json.Number `json:"id"`
	Name       string      `json:"name"`
	CourseCode string      `json:"course_code"`
}

func (c apiCourse) displayName() string {
	if strings.TrimSpace(c.Name) != "" {
		return c.Name
	}
	return c.CourseCode
}

// apiAttachment is a Canvas file, as embedded in a submission or returned by
// GET /api/v1/files/{id}.
type apiAttachment struct {
	ID          json.Number `json:"id"`
	DisplayName string      `json:"display_name"`
	Filename    string      `json:"filename"`
	URL         string      `json:"url"`
	ContentType string      `json:"content-type"`
	MimeClass   string      `json:"mime_class"`
	Size        int64       `json:"size"`
	UpdatedAt   string      `json:"updated_at"`
	ModifiedAt  string      `json:"modified_at"`
}

func (a apiAttachment) name() string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	if a.Filename != "" {
		return a.Filename
	}
	return "file-" + a.ID.String()
}

// apiSubmission is the student's own submission on an assignment.
type apiSubmission struct {
	WorkflowState  string          `json:"workflow_state"`
	SubmittedAt    ctime           `json:"submitted_at"`
	Late           bool            `json:"late"`
	Missing        bool            `json:"missing"`
	Excused        bool            `json:"excused"`
	Score          *float64        `json:"score"`
	Grade          string          `json:"grade"`
	Body           string          `json:"body"`
	URL            string          `json:"url"`
	SubmissionType string          `json:"submission_type"`
	Attachments    []apiAttachment `json:"attachments"`
}

// apiAssignment is GET /api/v1/courses/{cid}/assignments/{id}.
type apiAssignment struct {
	ID              json.Number `json:"id"`
	CourseID        json.Number `json:"course_id"`
	Name            string      `json:"name"`
	Description     string      `json:"description"`
	DueAt           ctime       `json:"due_at"`
	UnlockAt        ctime       `json:"unlock_at"`
	CreatedAt       ctime       `json:"created_at"`
	PointsPossible  *float64    `json:"points_possible"`
	HTMLURL         string      `json:"html_url"`
	SubmissionTypes []string    `json:"submission_types"`
	QuizID          json.Number `json:"quiz_id"`
	DiscussionTopic *struct {
		ID json.Number `json:"id"`
	} `json:"discussion_topic"`
	Submission *apiSubmission `json:"submission"`
}

// apiQuiz is GET /api/v1/courses/{cid}/quizzes/{id}.
type apiQuiz struct {
	ID             json.Number `json:"id"`
	Title          string      `json:"title"`
	Description    string      `json:"description"`
	DueAt          ctime       `json:"due_at"`
	UnlockAt       ctime       `json:"unlock_at"`
	PointsPossible *float64    `json:"points_possible"`
	HTMLURL        string      `json:"html_url"`
}

// apiDiscussion is GET /api/v1/courses/{cid}/discussion_topics/{id}.
type apiDiscussion struct {
	ID          json.Number     `json:"id"`
	Title       string          `json:"title"`
	Message     string          `json:"message"`
	HTMLURL     string          `json:"html_url"`
	PostedAt    ctime           `json:"posted_at"`
	CreatedAt   ctime           `json:"created_at"`
	Attachments []apiAttachment `json:"attachments"`
}

// num returns a json.Number as an int64, 0 when empty or malformed.
func num(n json.Number) int64 {
	if n == "" {
		return 0
	}
	v, err := n.Int64()
	if err != nil {
		return 0
	}
	return v
}
