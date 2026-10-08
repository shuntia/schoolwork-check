package canvas

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"schoolwork-check/internal/model"
)

func TestPlannerSubmissionsUnmarshal(t *testing.T) {
	t.Run("false means not submittable", func(t *testing.T) {
		var s plannerSubmissions
		if err := json.Unmarshal([]byte(`false`), &s); err != nil {
			t.Fatal(err)
		}
		if s.Present || s.Submitted {
			t.Fatalf("got %+v, want zero value", s)
		}
	})

	t.Run("object of flags", func(t *testing.T) {
		var s plannerSubmissions
		body := `{"submitted":true,"excused":false,"graded":true,"late":true,"missing":false,"needs_grading":false,"has_feedback":true,"redo_request":false}`
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatal(err)
		}
		if !s.Present || !s.Submitted || !s.Graded || !s.Late || !s.HasFeedback {
			t.Fatalf("flags not decoded: %+v", s)
		}
		if s.Excused || s.Missing || s.NeedsGrading || s.RedoRequest {
			t.Fatalf("unexpected true flag: %+v", s)
		}
	})

	t.Run("null and true", func(t *testing.T) {
		var s plannerSubmissions
		if err := json.Unmarshal([]byte(`null`), &s); err != nil {
			t.Fatal(err)
		}
		if s.Present {
			t.Fatalf("null should not be present: %+v", s)
		}
		if err := json.Unmarshal([]byte(`true`), &s); err != nil {
			t.Fatal(err)
		}
		if !s.Present || s.Submitted {
			t.Fatalf("true should be present with no flags: %+v", s)
		}
	})

	t.Run("inside a planner item", func(t *testing.T) {
		body := `[{"plannable_type":"quiz","plannable_id":301,"course_id":101,"submissions":false},
		          {"plannable_type":"assignment","plannable_id":201,"course_id":101,"submissions":{"submitted":true}}]`
		var items []plannerItem
		dec := json.NewDecoder(strings.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&items); err != nil {
			t.Fatal(err)
		}
		if items[0].Submissions.Present {
			t.Error("quiz submissions should be absent")
		}
		if !items[1].Submissions.Submitted {
			t.Error("assignment submissions should be submitted")
		}
	})
}

func TestCTime(t *testing.T) {
	var v struct {
		A ctime `json:"a"`
		B ctime `json:"b"`
		C ctime `json:"c"`
		D ctime `json:"d"`
	}
	body := `{"a":"2026-09-20T23:59:00Z","b":null,"c":"","d":"not a date"}`
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 20, 23, 59, 0, 0, time.UTC)
	if v.A.Ptr() == nil || !v.A.Ptr().Equal(want) {
		t.Errorf("a = %v, want %v", v.A.Ptr(), want)
	}
	for name, got := range map[string]*time.Time{"b": v.B.Ptr(), "c": v.C.Ptr(), "d": v.D.Ptr()} {
		if got != nil {
			t.Errorf("%s = %v, want nil", name, got)
		}
	}
}

func TestSubmissionStateTrustsCanvasMissingFlag(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	future := now.Add(48 * time.Hour)
	cases := []struct {
		name string
		sub  apiSubmission
		due  *time.Time
		want model.State
	}{
		{"past due, done outside Canvas", apiSubmission{WorkflowState: "unsubmitted", Missing: false}, &past, model.StateNotStarted},
		{"past due, Canvas says missing", apiSubmission{WorkflowState: "unsubmitted", Missing: true}, &past, model.StateMissing},
		{"no submission record, past due", apiSubmission{}, &past, model.StateMissing},
		{"no submission record, not due", apiSubmission{}, &future, model.StateNotStarted},
		{"graded", apiSubmission{WorkflowState: "graded"}, &past, model.StateGraded},
	}
	for _, c := range cases {
		if got := submissionState(c.sub, c.due, now); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if got := plannerState(stub{sub: plannerSubmissions{Present: true}, dueAt: &past}, now); got != model.StateNotStarted {
		t.Errorf("planner, flags say not missing: got %s", got)
	}
}
