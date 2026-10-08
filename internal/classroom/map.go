package classroom

import (
	"strconv"
	"strings"
	"time"

	gclassroom "google.golang.org/api/classroom/v1"

	"schoolwork-check/internal/model"
)

// Classroom course work types.
const (
	workTypeAssignment = "ASSIGNMENT"
	workTypeShortQ     = "SHORT_ANSWER_QUESTION"
	workTypeMultiQ     = "MULTIPLE_CHOICE_QUESTION"
)

// Classroom student submission states.
const (
	subStateNew       = "NEW"
	subStateCreated   = "CREATED"
	subStateTurnedIn  = "TURNED_IN"
	subStateReturned  = "RETURNED"
	subStateReclaimed = "RECLAIMED_BY_STUDENT"
)

// workKind maps a Classroom workType onto the shared Kind enum.
func workKind(workType string) model.Kind {
	switch workType {
	case workTypeAssignment:
		return model.KindAssignment
	case workTypeShortQ, workTypeMultiQ:
		return model.KindQuestion
	default:
		return model.KindOther
	}
}

// combineDue folds a Classroom dueDate + dueTime into a UTC instant.
// Classroom stores due times in UTC, so no timezone conversion is needed.
// Returns nil when there is no due date (undated work).
func combineDue(d *gclassroom.Date, t *gclassroom.TimeOfDay) *time.Time {
	if d == nil || d.Year == 0 || d.Month == 0 || d.Day == 0 {
		return nil
	}
	var h, m, s, ns int64
	if t != nil {
		h, m, s, ns = t.Hours, t.Minutes, t.Seconds, t.Nanos
	}
	due := time.Date(int(d.Year), time.Month(d.Month), int(d.Day),
		int(h), int(m), int(s), int(ns), time.UTC)
	return &due
}

// assignedAt prefers the scheduled post time, falling back to creation time.
func assignedAt(scheduled, created string) *time.Time {
	if t := parseRFC3339(scheduled); t != nil {
		return t
	}
	return parseRFC3339(created)
}

func parseRFC3339(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// hasWork reports whether the student has put anything into the submission.
func hasWork(s *gclassroom.StudentSubmission) bool {
	if s == nil {
		return false
	}
	if s.AssignmentSubmission != nil && len(s.AssignmentSubmission.Attachments) > 0 {
		return true
	}
	if s.ShortAnswerSubmission != nil && s.ShortAnswerSubmission.Answer != "" {
		return true
	}
	if s.MultipleChoiceSubmission != nil && s.MultipleChoiceSubmission.Answer != "" {
		return true
	}
	return false
}

// assignedGrade reports the grade the teacher assigned, if any.
//
// The generated StudentSubmission carries AssignedGrade as a plain float64,
// so a legitimately assigned 0 is indistinguishable from "absent". The grade
// history disambiguates it: an explicit points-earned change proves a grade
// was assigned even when it is zero.
func assignedGrade(s *gclassroom.StudentSubmission) (float64, bool) {
	if s == nil {
		return 0, false
	}
	if s.AssignedGrade != 0 {
		return s.AssignedGrade, true
	}
	for i := len(s.SubmissionHistory) - 1; i >= 0; i-- {
		gh := s.SubmissionHistory[i].GradeHistory
		if gh != nil && gh.GradeChangeType == "ASSIGNED_GRADE_POINTS_EARNED_CHANGE" {
			return gh.PointsEarned, true
		}
	}
	return 0, false
}

// formatGrade renders a grade the way Classroom displays it: "8/10", or just
// "8" when the course work is ungraded/unpointed.
func formatGrade(grade, maxPoints float64) string {
	if maxPoints > 0 {
		return trimFloat(grade) + "/" + trimFloat(maxPoints)
	}
	return trimFloat(grade)
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// lastTurnedIn returns the most recent TURNED_IN timestamp from the
// submission history, which is when the student actually handed the work in.
func lastTurnedIn(s *gclassroom.StudentSubmission) *time.Time {
	if s == nil {
		return nil
	}
	var latest *time.Time
	for _, h := range s.SubmissionHistory {
		if h == nil || h.StateHistory == nil || h.StateHistory.State != subStateTurnedIn {
			continue
		}
		t := parseRFC3339(h.StateHistory.StateTimestamp)
		if t == nil {
			continue
		}
		if latest == nil || t.After(*latest) {
			latest = t
		}
	}
	if latest == nil && (s.State == subStateTurnedIn || s.State == subStateReturned) {
		// No history (or unparsable): fall back to the last update.
		latest = parseRFC3339(s.UpdateTime)
	}
	return latest
}

// mapState folds a Classroom submission state, its contents and the due date
// into the shared State enum.
func mapState(s *gclassroom.StudentSubmission, due *time.Time, now time.Time) model.State {
	overdue := due != nil && due.Before(now)
	if s == nil {
		if overdue {
			return model.StateMissing
		}
		return model.StateNotStarted
	}
	switch s.State {
	case subStateReturned:
		if _, ok := assignedGrade(s); ok {
			return model.StateGraded
		}
		// Returned without a grade means "have another go".
		return model.StateSubmitted
	case subStateTurnedIn:
		return model.StateSubmitted
	case subStateNew, subStateCreated, subStateReclaimed:
		if hasWork(s) {
			return model.StateInProgress
		}
		if overdue {
			return model.StateMissing
		}
		return model.StateNotStarted
	default:
		if hasWork(s) {
			return model.StateInProgress
		}
		if overdue {
			return model.StateMissing
		}
		return model.StateNotStarted
	}
}

// normalizeText tidies Classroom's plain-text bodies: CRLF to LF, no trailing
// whitespace. Classroom descriptions are already plain text, so there is no
// HTML to strip.
func normalizeText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}
