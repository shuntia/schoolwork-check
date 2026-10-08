package canvas

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"schoolwork-check/internal/model"
)

// buildTask enriches a stub with its detail record, description text and
// attachments. It never fails the task: a detail fetch that errors is logged
// and the planner's view is emitted instead.
func (c *Client) buildTask(ctx context.Context, s stub, names map[int64]string, now time.Time, extracted, failures *atomic.Int64) model.Task {
	t := c.baseTask(s, names, now)
	if s.courseID == 0 {
		return t
	}

	var descHTML string
	switch s.ptype {
	case "assignment", "sub_assignment", "assessment_request":
		a, err := c.getAssignment(ctx, s.courseID, s.nativeID)
		if err != nil {
			failures.Add(1)
			c.log.Warn("canvas: assignment detail fetch failed", "id", s.id, "err", err)
			break
		}
		descHTML = a.Description
		if a.Name != "" {
			t.Title = a.Name
		}
		if a.HTMLURL != "" {
			t.URL = a.HTMLURL
		}
		if t.DueAt == nil {
			t.DueAt = a.DueAt.Ptr()
		}
		if t.Points == nil {
			t.Points = a.PointsPossible
		}
		if at := firstTime(a.UnlockAt.Ptr(), a.CreatedAt.Ptr()); at != nil {
			t.AssignedAt = at
		}
		if a.Submission != nil {
			t.Progress = c.progressFrom(ctx, *a.Submission, t.DueAt, t.Points, now, extracted, failures)
		}

	case "quiz":
		q, err := c.getQuiz(ctx, s.courseID, s.nativeID)
		if err != nil {
			failures.Add(1)
			c.log.Warn("canvas: quiz detail fetch failed", "id", s.id, "err", err)
			break
		}
		descHTML = q.Description
		if q.Title != "" {
			t.Title = q.Title
		}
		if q.HTMLURL != "" {
			t.URL = q.HTMLURL
		}
		if t.DueAt == nil {
			t.DueAt = q.DueAt.Ptr()
		}
		if t.Points == nil {
			t.Points = q.PointsPossible
		}
		if at := q.UnlockAt.Ptr(); at != nil {
			t.AssignedAt = at
		}

	case "discussion_topic", "announcement":
		// Canvas announcements are discussion topics with is_announcement set.
		d, err := c.getDiscussion(ctx, s.courseID, s.nativeID)
		if err != nil {
			failures.Add(1)
			c.log.Warn("canvas: discussion detail fetch failed", "id", s.id, "err", err)
			break
		}
		descHTML = d.Message
		if d.Title != "" {
			t.Title = d.Title
		}
		if d.HTMLURL != "" {
			t.URL = d.HTMLURL
		}
		if at := firstTime(d.PostedAt.Ptr(), d.CreatedAt.Ptr()); at != nil && t.AssignedAt == nil {
			t.AssignedAt = at
		}
		for _, a := range d.Attachments {
			t.Attachments = append(t.Attachments, c.attachmentFrom(ctx, a, extracted, failures))
		}
	}

	t.Description = htmlToText(descHTML)
	t.Attachments = append(t.Attachments, c.descriptionAttachments(ctx, descHTML, s.courseID, extracted, failures)...)
	return t
}

// progressFrom maps a Canvas submission onto model.Progress, extracting the
// student's own attachments because they say how far along the work is.
func (c *Client) progressFrom(ctx context.Context, sub apiSubmission, dueAt *time.Time, points *float64, now time.Time, extracted, failures *atomic.Int64) model.Progress {
	p := model.Progress{
		State:       submissionState(sub, dueAt, now),
		SubmittedAt: sub.SubmittedAt.Ptr(),
		Late:        sub.Late,
		Grade:       gradeString(sub.Grade, sub.Score, points),
		Text:        htmlToText(sub.Body),
	}
	for _, a := range sub.Attachments {
		p.Attachments = append(p.Attachments, c.attachmentFrom(ctx, a, extracted, failures))
	}
	if sub.URL != "" && (sub.SubmissionType == "online_url" || sub.SubmissionType == "basic_lti_launch") {
		p.Attachments = append(p.Attachments, model.Attachment{
			Name:     sub.URL,
			URL:      sub.URL,
			MimeType: "text/uri-list",
		})
	}
	return p
}

// submissionState collapses workflow_state plus the boolean flags into a State.
func submissionState(sub apiSubmission, dueAt *time.Time, now time.Time) model.State {
	switch {
	case sub.Excused:
		return model.StateExcused
	case sub.WorkflowState == "graded":
		return model.StateGraded
	case sub.WorkflowState == "submitted", sub.WorkflowState == "pending_review":
		return model.StateSubmitted
	case sub.Missing:
		return model.StateMissing
	}
	if sub.SubmittedAt.Ptr() != nil {
		return model.StateSubmitted
	}
	// Canvas's own missing flag is authoritative whenever it sent a
	// submission: it is false for work handed in outside Canvas
	// (submission_types "none", "on_paper"), which is past due and
	// unsubmitted here without being missing. Guess from the date only when
	// there is no submission record at all.
	if sub.WorkflowState == "" && dueAt != nil && dueAt.Before(now) {
		return model.StateMissing
	}
	return model.StateNotStarted
}

// plannerState collapses the planner's boolean submissions object into a State.
func plannerState(s stub, now time.Time) model.State {
	switch {
	case s.sub.Excused:
		return model.StateExcused
	case s.sub.Graded:
		return model.StateGraded
	case s.sub.Submitted:
		return model.StateSubmitted
	case s.sub.Missing:
		return model.StateMissing
	}
	// No date guess: when the flags are present Canvas already decided
	// missing (see submissionState), and when they are absent there is
	// nothing to submit.
	return model.StateNotStarted
}

// gradeString renders the grade the way the LMS displays it.
func gradeString(grade string, score, points *float64) string {
	if g := strings.TrimSpace(grade); g != "" && g != "null" {
		return g
	}
	if score == nil {
		return ""
	}
	if points == nil {
		return formatNum(*score)
	}
	return formatNum(*score) + "/" + formatNum(*points)
}

func formatNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func firstTime(ts ...*time.Time) *time.Time {
	for _, t := range ts {
		if t != nil {
			return t
		}
	}
	return nil
}

func (c *Client) getAssignment(ctx context.Context, courseID, id int64) (apiAssignment, error) {
	var a apiAssignment
	path := "/api/v1/courses/" + strconv.FormatInt(courseID, 10) + "/assignments/" + strconv.FormatInt(id, 10)
	err := c.getJSON(ctx, c.url(path, "include[]=submission"), &a)
	return a, err
}

func (c *Client) getQuiz(ctx context.Context, courseID, id int64) (apiQuiz, error) {
	var q apiQuiz
	path := "/api/v1/courses/" + strconv.FormatInt(courseID, 10) + "/quizzes/" + strconv.FormatInt(id, 10)
	err := c.getJSON(ctx, c.url(path, ""), &q)
	return q, err
}

func (c *Client) getDiscussion(ctx context.Context, courseID, id int64) (apiDiscussion, error) {
	var d apiDiscussion
	path := "/api/v1/courses/" + strconv.FormatInt(courseID, 10) + "/discussion_topics/" + strconv.FormatInt(id, 10)
	err := c.getJSON(ctx, c.url(path, ""), &d)
	return d, err
}
