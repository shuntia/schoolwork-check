// Package classroom is the Google Classroom source adapter.
//
// It turns a student's active courses, their published course work, course
// work materials and the student's own submissions into []model.Task.
//
// Authentication uses the installed-app ("Desktop app") OAuth flow:
//
//	classroom.Login(ctx, credentialsFile, tokenFile)   // one-time consent
//	c, err := classroom.New(ctx, credentialsFile, tokenFile, classroom.Options{...})
//	tasks, err := c.Fetch(ctx)
//
// Every scope requested is read-only and student-side; the adapter never
// writes to Classroom or Drive.
package classroom

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gclassroom "google.golang.org/api/classroom/v1"
	"google.golang.org/api/drive/v3"

	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// Options configures a Client. The zero value is usable: extraction is off,
// the date window is unbounded and logging goes to slog.Default().
type Options struct {
	// ExtractAttachments enables downloading/exporting attachment bytes and
	// turning them into plain text. When false, attachments carry only
	// name/URL metadata.
	ExtractAttachments bool
	// MaxAttachmentBytes skips downloading uploaded files larger than this.
	// Zero or negative means no size limit.
	MaxAttachmentBytes int64
	// MaxExtractedText caps extracted text per attachment, in bytes.
	// Zero or negative means extract.DefaultLimit.
	MaxExtractedText int
	// PastDays and FutureDays bound the due-date window. A task due before
	// now-PastDays or after now+FutureDays is dropped; an undated task
	// (materials, open-ended work) is dropped when it was posted before
	// now-PastDays. Zero or negative means unbounded on that side.
	PastDays, FutureDays int
	// Logger receives progress and non-fatal failures. nil means slog.Default().
	Logger *slog.Logger
	// FileCache skips downloading Drive files whose extraction is already
	// known at the same Drive version. nil caches nothing.
	FileCache *filecache.Cache
}

// submissionWorkers bounds concurrent per-course-work submission fetches.
const submissionWorkers = 6

// Client fetches tasks from Google Classroom. It is safe for concurrent use.
type Client struct {
	cls  *gclassroom.Service
	drv  *drive.Service
	opts Options
	log  *slog.Logger

	// now and retryDelay exist so tests can control time.
	now        func() time.Time
	retryDelay time.Duration
}

// newWithServices builds a Client around already-constructed Google API
// services. It is the seam used by tests (which point the services at an
// httptest.Server via option.WithEndpoint).
func newWithServices(cls *gclassroom.Service, drv *drive.Service, opts Options) *Client {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cls:        cls,
		drv:        drv,
		opts:       opts,
		log:        log,
		now:        time.Now,
		retryDelay: 500 * time.Millisecond,
	}
}

// stats counts what one Fetch did, for the summary log line.
type stats struct {
	courses       atomic.Int64
	coursework    atomic.Int64
	materials     atomic.Int64
	announcements atomic.Int64
	submissions   atomic.Int64
	extracted     atomic.Int64
	failures      atomic.Int64
}

// Fetch returns every task visible to the signed-in student across their
// active courses. Individual courses, attachments and submissions that fail
// are logged and skipped; only an authentication failure aborts the run.
func (c *Client) Fetch(ctx context.Context) ([]model.Task, error) {
	start := time.Now()
	st := &stats{}
	now := c.now().UTC()
	lo, hi := c.window(now)

	courses, err := c.listCourses(ctx)
	if err != nil {
		return nil, err
	}
	st.courses.Store(int64(len(courses)))

	var tasks []model.Task
	for _, course := range courses {
		if err := ctx.Err(); err != nil {
			return tasks, err
		}
		ts, err := c.fetchCourse(ctx, course, now, lo, hi, st)
		tasks = append(tasks, ts...)
		if err != nil {
			if ae := authError(err); ae != nil {
				return tasks, ae
			}
			st.failures.Add(1)
			c.log.Error("classroom: course failed", "course_id", course.Id, "course", courseName(course), "err", err)
		}
		as, err := c.fetchAnnouncements(ctx, course, now, lo, st)
		tasks = append(tasks, as...)
		if err != nil {
			return tasks, err
		}
	}

	c.log.Info("classroom: fetch complete",
		"courses", st.courses.Load(),
		"coursework", st.coursework.Load(),
		"materials", st.materials.Load(),
		"announcements", st.announcements.Load(),
		"submissions", st.submissions.Load(),
		"attachments_extracted", st.extracted.Load(),
		"failures", st.failures.Load(),
		"tasks", len(tasks),
		"took", time.Since(start).Round(time.Millisecond),
	)
	return tasks, nil
}

// window turns PastDays/FutureDays into an absolute due-date range.
// A zero time means "unbounded on this side".
func (c *Client) window(now time.Time) (lo, hi time.Time) {
	if c.opts.PastDays > 0 {
		lo = now.AddDate(0, 0, -c.opts.PastDays)
	}
	if c.opts.FutureDays > 0 {
		hi = now.AddDate(0, 0, c.opts.FutureDays)
	}
	return lo, hi
}

// inWindow reports whether a task should be kept. Dated tasks are judged by
// due date. Undated ones fall back to when they were posted, and only against
// the lower bound: a classroom keeps every material since the course began,
// often last school year, and none of that is current work.
func inWindow(due, assigned *time.Time, lo, hi time.Time) bool {
	if due == nil {
		return assigned == nil || lo.IsZero() || !assigned.Before(lo)
	}
	if !lo.IsZero() && due.Before(lo) {
		return false
	}
	if !hi.IsZero() && due.After(hi) {
		return false
	}
	return true
}

func (c *Client) listCourses(ctx context.Context) ([]*gclassroom.Course, error) {
	var out []*gclassroom.Course
	err := c.retry(ctx, "courses.list", func() error {
		out = out[:0]
		return c.cls.Courses.List().
			StudentId("me").
			CourseStates("ACTIVE").
			PageSize(100).
			Pages(ctx, func(r *gclassroom.ListCoursesResponse) error {
				out = append(out, r.Courses...)
				return nil
			})
	})
	if err != nil {
		if ae := authError(err); ae != nil {
			return nil, ae
		}
		return nil, fmt.Errorf("classroom: listing courses: %w", err)
	}
	return out, nil
}

// courseName picks the most human-readable label available for a course.
func courseName(c *gclassroom.Course) string {
	for _, s := range []string{c.Name, c.Section, c.DescriptionHeading} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return c.Id
}

func (c *Client) fetchCourse(ctx context.Context, course *gclassroom.Course, now, lo, hi time.Time, st *stats) ([]model.Task, error) {
	name := courseName(course)

	work, err := c.listCourseWork(ctx, course.Id)
	if err != nil {
		return nil, err
	}
	st.coursework.Add(int64(len(work)))

	// Build the task skeletons first so the submission fetches below can be
	// run concurrently against a fixed slice.
	type pending struct {
		task model.Task
		work *gclassroom.CourseWork
	}
	var pendings []pending
	for _, cw := range work {
		due := combineDue(cw.DueDate, cw.DueTime)
		if !inWindow(due, assignedAt(cw.ScheduledTime, cw.CreationTime), lo, hi) {
			continue
		}
		t := model.Task{
			ID:          fmt.Sprintf("classroom:%s:%s", course.Id, cw.Id),
			Source:      model.SourceClassroom,
			Kind:        workKind(cw.WorkType),
			Course:      name,
			CourseID:    course.Id,
			Title:       cw.Title,
			URL:         cw.AlternateLink,
			AssignedAt:  assignedAt(cw.ScheduledTime, cw.CreationTime),
			DueAt:       due,
			Description: normalizeText(cw.Description),
			FetchedAt:   now,
		}
		if cw.MaxPoints > 0 {
			p := cw.MaxPoints
			t.Points = &p
		}
		pendings = append(pendings, pending{task: t, work: cw})
	}

	// Teacher-attached materials (sequential; usually few per course work).
	for i := range pendings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pendings[i].task.Attachments = c.attachments(ctx, fromMaterials(pendings[i].work.Materials), st)
	}

	// Student submissions, bounded worker pool.
	var wg sync.WaitGroup
	sem := make(chan struct{}, submissionWorkers)
	for i := range pendings {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			p := &pendings[i]
			sub, err := c.studentSubmission(ctx, course.Id, p.work.Id)
			if err != nil {
				st.failures.Add(1)
				c.log.Warn("classroom: submission fetch failed",
					"course_id", course.Id, "coursework_id", p.work.Id, "err", err)
			}
			if sub != nil {
				st.submissions.Add(1)
			}
			p.task.Progress = c.progress(ctx, sub, p.task.DueAt, now, p.work.MaxPoints, st)
		}(i)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tasks := make([]model.Task, 0, len(pendings))
	for i := range pendings {
		tasks = append(tasks, pendings[i].task)
	}

	// Course work materials (readings). Some domains forbid this call for
	// students; that is not fatal.
	mats, err := c.listCourseWorkMaterials(ctx, course.Id)
	if err != nil {
		if ae := authError(err); ae != nil {
			return tasks, ae
		}
		if isForbidden(err) {
			c.log.Info("classroom: course work materials not available for this course", "course_id", course.Id, "err", err)
		} else {
			st.failures.Add(1)
			c.log.Warn("classroom: listing course work materials failed", "course_id", course.Id, "err", err)
		}
		return tasks, nil
	}
	st.materials.Add(int64(len(mats)))
	for _, m := range mats {
		if !inWindow(nil, assignedAt(m.ScheduledTime, m.CreationTime), lo, hi) {
			continue
		}
		t := model.Task{
			ID:          fmt.Sprintf("classroom:%s:material:%s", course.Id, m.Id),
			Source:      model.SourceClassroom,
			Kind:        model.KindMaterial,
			Course:      name,
			CourseID:    course.Id,
			Title:       m.Title,
			URL:         m.AlternateLink,
			AssignedAt:  assignedAt(m.ScheduledTime, m.CreationTime),
			Description: normalizeText(m.Description),
			Attachments: c.attachments(ctx, fromMaterials(m.Materials), st),
			Progress:    model.Progress{State: model.StateNotStarted},
			FetchedAt:   now,
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// fetchAnnouncements lists a course's published announcements posted inside
// the window. A 403 (a token from before the announcements scope was added,
// or a domain that restricts it) is logged and skipped; only an auth failure
// is returned.
func (c *Client) fetchAnnouncements(ctx context.Context, course *gclassroom.Course, now, lo time.Time, st *stats) ([]model.Task, error) {
	var anns []*gclassroom.Announcement
	err := c.retry(ctx, "announcements.list", func() error {
		anns = anns[:0]
		return c.cls.Courses.Announcements.List(course.Id).
			AnnouncementStates("PUBLISHED").
			PageSize(100).
			Pages(ctx, func(r *gclassroom.ListAnnouncementsResponse) error {
				anns = append(anns, r.Announcements...)
				return nil
			})
	})
	if err != nil {
		if ae := authError(err); ae != nil {
			return nil, ae
		}
		if isForbidden(err) {
			c.log.Info("classroom: announcements not available (rerun google-login to grant the scope)", "course_id", course.Id)
		} else {
			st.failures.Add(1)
			c.log.Warn("classroom: listing announcements failed", "course_id", course.Id, "err", err)
		}
		return nil, nil
	}
	st.announcements.Add(int64(len(anns)))

	var tasks []model.Task
	for _, a := range anns {
		posted := assignedAt(a.ScheduledTime, a.CreationTime)
		if !inWindow(nil, posted, lo, time.Time{}) {
			continue
		}
		text := normalizeText(a.Text)
		tasks = append(tasks, model.Task{
			ID:          fmt.Sprintf("classroom:%s:announcement:%s", course.Id, a.Id),
			Source:      model.SourceClassroom,
			Kind:        model.KindAnnouncement,
			Course:      courseName(course),
			CourseID:    course.Id,
			Title:       announcementTitle(text),
			URL:         a.AlternateLink,
			AssignedAt:  posted,
			Description: text,
			Attachments: c.attachments(ctx, fromMaterials(a.Materials), st),
			Progress:    model.Progress{State: model.StateNotStarted},
			FetchedAt:   now,
		})
	}
	return tasks, nil
}

// announcementTitle makes a title from an announcement's first line, since
// Classroom announcements have none.
func announcementTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.Join(strings.Fields(line), " ")
	if r := []rune(line); len(r) > 80 {
		line = string(r[:79]) + "…"
	}
	if line == "" {
		return "Announcement"
	}
	return line
}

func (c *Client) listCourseWork(ctx context.Context, courseID string) ([]*gclassroom.CourseWork, error) {
	var out []*gclassroom.CourseWork
	err := c.retry(ctx, "courseWork.list", func() error {
		out = out[:0]
		return c.cls.Courses.CourseWork.List(courseID).
			CourseWorkStates("PUBLISHED").
			OrderBy("dueDate asc").
			PageSize(100).
			Pages(ctx, func(r *gclassroom.ListCourseWorkResponse) error {
				out = append(out, r.CourseWork...)
				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("listing course work: %w", err)
	}
	return out, nil
}

func (c *Client) listCourseWorkMaterials(ctx context.Context, courseID string) ([]*gclassroom.CourseWorkMaterial, error) {
	var out []*gclassroom.CourseWorkMaterial
	err := c.retry(ctx, "courseWorkMaterials.list", func() error {
		out = out[:0]
		return c.cls.Courses.CourseWorkMaterials.List(courseID).
			CourseWorkMaterialStates("PUBLISHED").
			PageSize(100).
			Pages(ctx, func(r *gclassroom.ListCourseWorkMaterialResponse) error {
				out = append(out, r.CourseWorkMaterial...)
				return nil
			})
	})
	return out, err
}

// studentSubmission returns the signed-in student's submission for one course
// work, or nil when Classroom reports none.
func (c *Client) studentSubmission(ctx context.Context, courseID, courseWorkID string) (*gclassroom.StudentSubmission, error) {
	var subs []*gclassroom.StudentSubmission
	err := c.retry(ctx, "studentSubmissions.list", func() error {
		subs = subs[:0]
		return c.cls.Courses.CourseWork.StudentSubmissions.List(courseID, courseWorkID).
			UserId("me").
			PageSize(20).
			Pages(ctx, func(r *gclassroom.ListStudentSubmissionsResponse) error {
				subs = append(subs, r.StudentSubmissions...)
				return nil
			})
	})
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, nil
	}
	return subs[0], nil
}

// progress maps one StudentSubmission onto model.Progress, extracting the
// student's own attachments (the part downstream cares about most).
func (c *Client) progress(ctx context.Context, sub *gclassroom.StudentSubmission, due *time.Time, now time.Time, maxPoints float64, st *stats) model.Progress {
	p := model.Progress{State: mapState(sub, due, now)}
	if sub == nil {
		return p
	}
	p.Late = sub.Late
	p.SubmittedAt = lastTurnedIn(sub)
	if g, ok := assignedGrade(sub); ok {
		p.Grade = formatGrade(g, maxPoints)
	} else if sub.State == "RETURNED" {
		// Returned to the student without a grade: sent back for revision.
		p.Grade = "returned"
	}
	if sub.ShortAnswerSubmission != nil && sub.ShortAnswerSubmission.Answer != "" {
		p.Text = normalizeText(sub.ShortAnswerSubmission.Answer)
	} else if sub.MultipleChoiceSubmission != nil && sub.MultipleChoiceSubmission.Answer != "" {
		p.Text = normalizeText(sub.MultipleChoiceSubmission.Answer)
	}
	if sub.AssignmentSubmission != nil {
		p.Attachments = c.attachments(ctx, fromAttachments(sub.AssignmentSubmission.Attachments), st)
	}
	return p
}
