// Package canvas fetches a student's own coursework from the Canvas LMS REST
// API and maps it onto the shared model.Task row type.
//
// The primary source is the planner (GET /api/v1/planner/items), which already
// collapses assignments, quizzes, discussions and pages into one dated feed.
// Because the planner omits undated work, the per-course assignment lists are
// merged in as well, deduplicated by task ID with the planner winning.
//
// Usage:
//
//	c := canvas.New(cfg.CanvasBaseURL, cfg.CanvasToken, canvas.Options{...})
//	tasks, err := c.Fetch(ctx)
package canvas

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// Options configures a Client. The zero value is usable: see New for defaults.
type Options struct {
	// ExtractAttachments enables downloading attachments and extracting their
	// text through the extract package.
	ExtractAttachments bool
	// MaxAttachmentBytes skips downloading files larger than this.
	MaxAttachmentBytes int64
	// MaxExtractedText caps extracted text per attachment.
	MaxExtractedText int
	// PastDays and FutureDays bound the planner window around now.
	PastDays   int
	FutureDays int

	// Logger receives warnings and the run summary; nil means slog.Default().
	Logger *slog.Logger
	// HTTPClient is used for every call; nil means a client with a 60s timeout.
	HTTPClient *http.Client
	// FileCache skips downloading files whose extraction is already known
	// at the same version. nil caches nothing.
	FileCache *filecache.Cache
}

// Client talks to one Canvas instance as one student.
type Client struct {
	baseURL    string
	token      string
	opts       Options
	log        *slog.Logger
	httpClient *http.Client

	// retryBase is the first backoff step; later attempts double it.
	retryBase time.Duration
	// workers bounds concurrent per-task detail fetches.
	workers int
	// now is overridable in tests.
	now func() time.Time
}

// defaults applied by New when the corresponding option is zero.
const (
	defaultMaxAttachmentBytes = 20 << 20
	defaultMaxExtractedText   = 64 << 10
	defaultPastDays           = 30
	defaultFutureDays         = 120
	defaultWorkers            = 6
	defaultRetryBase          = 500 * time.Millisecond
	defaultTimeout            = 60 * time.Second
)

// New returns a Client for baseURL (e.g. https://school.instructure.com, with
// or without a trailing slash) authenticating with the given student token.
//
// Zero-valued options take defaults: 20 MiB per attachment, 64 KiB of extracted
// text, slog.Default(), and an http.Client with a 60s timeout. PastDays and
// FutureDays default to 30 and 120 only when both are zero, so an explicit
// one-sided window is honoured.
func New(baseURL, token string, opts Options) *Client {
	if opts.MaxAttachmentBytes <= 0 {
		opts.MaxAttachmentBytes = defaultMaxAttachmentBytes
	}
	if opts.MaxExtractedText <= 0 {
		opts.MaxExtractedText = defaultMaxExtractedText
	}
	if opts.PastDays == 0 && opts.FutureDays == 0 {
		opts.PastDays, opts.FutureDays = defaultPastDays, defaultFutureDays
	}
	if opts.PastDays < 0 {
		opts.PastDays = 0
	}
	if opts.FutureDays < 0 {
		opts.FutureDays = 0
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		opts:       opts,
		log:        log,
		httpClient: hc,
		retryBase:  defaultRetryBase,
		workers:    defaultWorkers,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// stub is the merged, pre-detail view of one task.
type stub struct {
	id         string // model.Task.ID
	kind       model.Kind
	ptype      string // Canvas plannable_type
	nativeID   int64
	courseID   int64
	title      string
	url        string
	dueAt      *time.Time
	assignedAt *time.Time
	points     *float64
	sub        plannerSubmissions
	planner    bool
}

// Fetch returns every task in the configured window for the authenticated
// student. Per-task failures are logged and degrade to whatever the planner
// already knew; only a rejected token or a total loss of both listings is
// fatal.
func (c *Client) Fetch(ctx context.Context) ([]model.Task, error) {
	now := c.now().UTC()
	start := now.AddDate(0, 0, -c.opts.PastDays)
	end := now.AddDate(0, 0, c.opts.FutureDays)

	var firstErr error
	note := func(err error) error {
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		if firstErr == nil {
			firstErr = err
		}
		return nil
	}

	courses, err := c.listCourses(ctx)
	if err != nil {
		if fatal := note(err); fatal != nil {
			return nil, fatal
		}
		c.log.Warn("canvas: listing courses failed", "err", err)
	}
	names := make(map[int64]string, len(courses))
	for _, cr := range courses {
		names[num(cr.ID)] = cr.displayName()
	}

	byID := map[string]*stub{}
	var order []string
	add := func(s stub) {
		if s.id == "" {
			return
		}
		if prev, ok := byID[s.id]; ok {
			mergeStub(prev, s)
			return
		}
		cp := s
		byID[s.id] = &cp
		order = append(order, s.id)
	}

	planner, err := c.fetchPlanner(ctx, start, end)
	if err != nil {
		if fatal := note(err); fatal != nil {
			return nil, fatal
		}
		c.log.Warn("canvas: planner fetch failed", "err", err)
	}
	for _, s := range planner {
		add(s)
	}

	// Undated and planner-omitted assignments, per course.
	for _, cr := range courses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cid := num(cr.ID)
		if cid == 0 {
			continue
		}
		assigns, err := c.listCourseAssignments(ctx, cid)
		if err != nil {
			if fatal := note(err); fatal != nil {
				return nil, fatal
			}
			c.log.Warn("canvas: listing assignments failed", "course_id", cid, "err", err)
			continue
		}
		for _, a := range assigns {
			s, ok := assignmentStub(a, cid, start)
			if !ok {
				continue
			}
			// A quiz or graded discussion also shows up as an assignment;
			// keep the planner's identity for it rather than duplicating.
			if qid := num(a.QuizID); qid != 0 {
				if _, dup := byID[taskID("quiz", qid)]; dup {
					continue
				}
			}
			if a.DiscussionTopic != nil {
				if _, dup := byID[taskID("discussion", num(a.DiscussionTopic.ID))]; dup {
					continue
				}
			}
			add(s)
		}
	}

	if len(order) == 0 && firstErr != nil {
		return nil, firstErr
	}

	stubs := make([]*stub, 0, len(order))
	for _, id := range order {
		stubs = append(stubs, byID[id])
	}

	var (
		extracted atomic.Int64
		failures  atomic.Int64
	)
	tasks := make([]model.Task, len(stubs))
	sem := make(chan struct{}, c.workers)
	var wg sync.WaitGroup
	for i, s := range stubs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, s *stub) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				tasks[i] = c.baseTask(*s, names, now)
				return
			}
			defer func() { <-sem }()
			tasks[i] = c.buildTask(ctx, *s, names, now, &extracted, &failures)
		}(i, s)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sortTasks(tasks)
	c.log.Info("canvas: fetch complete",
		"courses", len(courses),
		"tasks", len(tasks),
		"attachments_extracted", extracted.Load(),
		"failures", failures.Load(),
	)
	return tasks, nil
}

// mergeStub folds b into a. The planner wins on dates and submission state;
// anything the planner left empty is filled from the course listing.
func mergeStub(a *stub, b stub) {
	if !a.planner && b.planner {
		sub := *a
		*a = b
		a.planner = true
		if a.dueAt == nil {
			a.dueAt = sub.dueAt
		}
		if a.points == nil {
			a.points = sub.points
		}
		if a.assignedAt == nil {
			a.assignedAt = sub.assignedAt
		}
		if a.url == "" {
			a.url = sub.url
		}
		if a.title == "" {
			a.title = sub.title
		}
		return
	}
	if a.dueAt == nil {
		a.dueAt = b.dueAt
	}
	if a.points == nil {
		a.points = b.points
	}
	if a.assignedAt == nil {
		a.assignedAt = b.assignedAt
	}
	if a.url == "" {
		a.url = b.url
	}
	if a.title == "" {
		a.title = b.title
	}
	if a.courseID == 0 {
		a.courseID = b.courseID
	}
	if !a.sub.Present && b.sub.Present {
		a.sub = b.sub
	}
}

// listCourses returns the student's active courses.
func (c *Client) listCourses(ctx context.Context) ([]apiCourse, error) {
	q := url.Values{}
	q.Set("enrollment_state", "active")
	q.Set("per_page", "100")
	return getPaged[apiCourse](ctx, c, c.url("/api/v1/courses", q.Encode()))
}

// listCourseAssignments returns every assignment in a course, with the
// student's submission attached.
func (c *Client) listCourseAssignments(ctx context.Context, courseID int64) ([]apiAssignment, error) {
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("order_by", "due_at")
	q.Add("include[]", "submission")
	path := "/api/v1/courses/" + strconv.FormatInt(courseID, 10) + "/assignments"
	return getPaged[apiAssignment](ctx, c, c.url(path, q.Encode()))
}

// fetchPlanner pulls the planner feed for the window and turns it into stubs.
func (c *Client) fetchPlanner(ctx context.Context, start, end time.Time) ([]stub, error) {
	q := url.Values{}
	q.Set("start_date", start.Format(time.RFC3339))
	q.Set("end_date", end.Format(time.RFC3339))
	q.Set("per_page", "100")
	items, err := getPaged[plannerItem](ctx, c, c.url("/api/v1/planner/items", q.Encode()))
	stubs := make([]stub, 0, len(items))
	for _, it := range items {
		s, ok := plannerStub(it)
		if !ok {
			continue
		}
		stubs = append(stubs, s)
	}
	return stubs, err
}

// skipped planner plannable types: not coursework.
var skipPlannable = map[string]bool{
	"planner_note":   true,
	"calendar_event": true,
}

func kindFor(plannableType string) model.Kind {
	switch plannableType {
	case "assignment", "sub_assignment", "assessment_request":
		return model.KindAssignment
	case "quiz":
		return model.KindQuiz
	case "discussion_topic":
		return model.KindDiscussion
	case "wiki_page":
		return model.KindMaterial
	case "announcement":
		return model.KindAnnouncement
	default:
		return model.KindOther
	}
}

// idPrefix maps a plannable type onto the "canvas:<prefix>:<id>" namespace.
func idPrefix(plannableType string) string {
	switch plannableType {
	case "quiz":
		return "quiz"
	case "discussion_topic":
		return "discussion"
	case "wiki_page":
		return "page"
	case "assignment", "sub_assignment", "assessment_request":
		return "assignment"
	default:
		if plannableType == "" {
			return "item"
		}
		return plannableType
	}
}

func taskID(prefix string, id int64) string {
	return "canvas:" + prefix + ":" + strconv.FormatInt(id, 10)
}

func plannerStub(it plannerItem) (stub, bool) {
	if skipPlannable[it.PlannableType] {
		return stub{}, false
	}
	id := num(it.PlannableID)
	if id == 0 {
		id = num(it.Plannable.ID)
	}
	if id == 0 {
		return stub{}, false
	}
	title := it.Plannable.Title
	if title == "" {
		title = it.Plannable.Name
	}
	due := it.Plannable.DueAt.Ptr()
	if due == nil {
		due = it.Plannable.TodoDate.Ptr()
	}
	if due == nil {
		due = it.PlannableDate.Ptr()
	}
	assigned := it.Plannable.UnlockAt.Ptr()
	if assigned == nil {
		assigned = it.Plannable.CreatedAt.Ptr()
	}
	return stub{
		id:         taskID(idPrefix(it.PlannableType), id),
		kind:       kindFor(it.PlannableType),
		ptype:      it.PlannableType,
		nativeID:   id,
		courseID:   num(it.CourseID),
		title:      title,
		url:        it.HTMLURL,
		dueAt:      due,
		assignedAt: assigned,
		points:     it.Plannable.PointsPossible,
		sub:        it.Submissions,
		planner:    true,
	}, true
}

// assignmentStub converts a course-listing assignment into a stub, dropping
// anything that was due before the start of the window. Undated assignments
// are always kept.
func assignmentStub(a apiAssignment, courseID int64, start time.Time) (stub, bool) {
	id := num(a.ID)
	if id == 0 {
		return stub{}, false
	}
	due := a.DueAt.Ptr()
	if due != nil && due.Before(start) {
		return stub{}, false
	}
	assigned := a.UnlockAt.Ptr()
	if assigned == nil {
		assigned = a.CreatedAt.Ptr()
	}
	cid := courseID
	if cid == 0 {
		cid = num(a.CourseID)
	}
	return stub{
		id:         taskID("assignment", id),
		kind:       model.KindAssignment,
		ptype:      "assignment",
		nativeID:   id,
		courseID:   cid,
		title:      a.Name,
		url:        a.HTMLURL,
		dueAt:      due,
		assignedAt: assigned,
		points:     a.PointsPossible,
	}, true
}

// baseTask is the task as known before any detail fetch.
func (c *Client) baseTask(s stub, names map[int64]string, now time.Time) model.Task {
	t := model.Task{
		ID:         s.id,
		Source:     model.SourceCanvas,
		Kind:       s.kind,
		Course:     names[s.courseID],
		Title:      s.title,
		URL:        s.url,
		AssignedAt: s.assignedAt,
		DueAt:      s.dueAt,
		Points:     s.points,
		FetchedAt:  now,
	}
	if s.courseID != 0 {
		t.CourseID = strconv.FormatInt(s.courseID, 10)
	}
	if t.URL == "" && s.courseID != 0 {
		t.URL = c.webURL(s)
	}
	t.Progress = model.Progress{State: plannerState(s, now)}
	return t
}

// webURL reconstructs a deep link when the planner did not supply one.
func (c *Client) webURL(s stub) string {
	cid := strconv.FormatInt(s.courseID, 10)
	nid := strconv.FormatInt(s.nativeID, 10)
	switch s.ptype {
	case "quiz":
		return c.baseURL + "/courses/" + cid + "/quizzes/" + nid
	case "discussion_topic", "announcement":
		return c.baseURL + "/courses/" + cid + "/discussion_topics/" + nid
	case "wiki_page":
		return c.baseURL + "/courses/" + cid + "/pages/" + nid
	case "assignment", "sub_assignment", "assessment_request":
		return c.baseURL + "/courses/" + cid + "/assignments/" + nid
	default:
		return c.baseURL + "/courses/" + cid
	}
}

func sortTasks(tasks []model.Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		a, b := tasks[i], tasks[j]
		switch {
		case a.DueAt != nil && b.DueAt == nil:
			return true
		case a.DueAt == nil && b.DueAt != nil:
			return false
		case a.DueAt != nil && b.DueAt != nil && !a.DueAt.Equal(*b.DueAt):
			return a.DueAt.Before(*b.DueAt)
		}
		if a.Course != b.Course {
			return a.Course < b.Course
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.ID < b.ID
	})
}
