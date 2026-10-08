package canvas

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"schoolwork-check/internal/extract"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

const testToken = "test-token"

// fixed "now" for every test; fixtures are dated around it.
var testNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return tt.UTC()
}

// stubExtractors installs deterministic, local extract hooks so these tests do
// not depend on the extract package being implemented yet.
func stubExtractors(t *testing.T) {
	t.Helper()
	origReady, origText, origHTML := extractorReady, extractText, htmlToTextHook
	extractorReady = func() bool { return true }
	extractText = func(ctx context.Context, in extract.Input) (extract.Result, error) {
		if !strings.HasPrefix(in.MimeType, "text/") && !strings.HasSuffix(in.Name, ".txt") {
			return extract.Result{}, &extract.UnsupportedError{MimeType: in.MimeType, Name: in.Name}
		}
		b, err := io.ReadAll(in.Reader)
		if err != nil {
			return extract.Result{}, err
		}
		s := string(b)
		trunc := false
		if in.Limit > 0 && len(s) > in.Limit {
			s, trunc = s[:in.Limit], true
		}
		return extract.Result{Text: strings.TrimSpace(s), Truncated: trunc}, nil
	}
	htmlToTextHook = fallbackHTMLToText
	t.Cleanup(func() { extractorReady, extractText, htmlToTextHook = origReady, origText, origHTML })
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fixtureServer serves a small recorded-style Canvas instance.
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srvURL string
	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"message":"Invalid access token."}]}`)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	srvURL = srv.URL
	t.Cleanup(srv.Close)

	json := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}

	mux.HandleFunc("/api/v1/courses", func(w http.ResponseWriter, r *http.Request) {
		json(w, `[
		  {"id":101,"name":"Biology 201","course_code":"BIO201"},
		  {"id":102,"name":"","course_code":"HIST100"}
		]`)
	})

	// Planner, paginated via a Link header.
	mux.HandleFunc("/api/v1/planner/items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			json(w, `[
			  {"context_type":"Course","course_id":101,"plannable_type":"quiz","plannable_id":301,
			   "plannable_date":"2026-09-18T23:59:00Z","html_url":"/courses/101/quizzes/301",
			   "submissions":false,
			   "plannable":{"id":301,"title":"Genetics Quiz","due_at":"2026-09-18T23:59:00Z","points_possible":10}},
			  {"context_type":"Course","course_id":101,"plannable_type":"discussion_topic","plannable_id":401,
			   "plannable_date":null,"html_url":"/courses/101/discussion_topics/401",
			   "submissions":{"submitted":true,"excused":false,"graded":false,"late":false,"missing":false,
			                  "needs_grading":true,"has_feedback":false,"redo_request":false},
			   "plannable":{"id":401,"title":"Intro thread","due_at":null,"created_at":"2026-09-02T10:00:00Z"}},
			  {"context_type":"Course","course_id":101,"plannable_type":"assignment","plannable_id":202,
			   "plannable_date":"2026-09-10T23:59:00Z","html_url":"/courses/101/assignments/202",
			   "submissions":{"submitted":true,"excused":false,"graded":true,"late":true,"missing":false,
			                  "needs_grading":false,"has_feedback":true,"redo_request":false},
			   "plannable":{"id":202,"title":"Essay Draft","due_at":"2026-09-10T23:59:00Z","points_possible":20}},
			  {"context_type":"Course","course_id":101,"plannable_type":"calendar_event","plannable_id":900,
			   "plannable_date":"2026-09-17T09:00:00Z","submissions":false,
			   "plannable":{"id":900,"title":"Field trip"}}
			]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/planner/items?page=2&per_page=100>; rel="next",<%s/api/v1/planner/items?page=2&per_page=100>; rel="last"`, srvURL, srvURL))
		json(w, `[
		  {"context_type":"Course","course_id":101,"plannable_type":"assignment","plannable_id":201,
		   "plannable_date":"2026-09-20T23:59:00Z","html_url":"/courses/101/assignments/201",
		   "submissions":{"submitted":false,"excused":false,"graded":false,"late":false,"missing":false,
		                  "needs_grading":false,"has_feedback":false,"redo_request":false},
		   "plannable":{"id":201,"title":"Cell Structure Lab","due_at":"2026-09-20T23:59:00Z","points_possible":20}},
		  {"context_type":"User","plannable_type":"planner_note","plannable_id":701,
		   "plannable_date":"2026-09-19T00:00:00Z","submissions":false,
		   "plannable":{"id":701,"title":"Buy notebook","todo_date":"2026-09-19T00:00:00Z"}},
		  {"context_type":"Course","course_id":101,"plannable_type":"announcement","plannable_id":801,
		   "plannable_date":"2026-09-14T00:00:00Z","submissions":false,
		   "plannable":{"id":801,"title":"Welcome back"}}
		]`)
	})

	mux.HandleFunc("/api/v1/courses/101/assignments", func(w http.ResponseWriter, r *http.Request) {
		json(w, `[
		  {"id":201,"course_id":101,"name":"Cell Structure Lab","due_at":"2026-09-20T23:59:00Z","points_possible":20,
		   "html_url":"/courses/101/assignments/201"},
		  {"id":202,"course_id":101,"name":"Essay Draft","due_at":"2026-09-10T23:59:00Z","points_possible":20,
		   "html_url":"/courses/101/assignments/202"},
		  {"id":203,"course_id":101,"name":"Reading log","due_at":null,"points_possible":5,
		   "created_at":"2026-08-25T00:00:00Z","html_url":"/courses/101/assignments/203"},
		  {"id":204,"course_id":101,"name":"Ancient history essay","due_at":"2026-01-05T23:59:00Z","points_possible":30,
		   "html_url":"/courses/101/assignments/204"},
		  {"id":205,"course_id":101,"name":"Genetics Quiz","due_at":"2026-09-18T23:59:00Z","points_possible":10,
		   "quiz_id":301,"html_url":"/courses/101/assignments/205"}
		]`)
	})

	mux.HandleFunc("/api/v1/courses/102/assignments", func(w http.ResponseWriter, r *http.Request) {
		json(w, `[
		  {"id":206,"course_id":102,"name":"Timeline worksheet","due_at":"2026-09-25T23:59:00Z","points_possible":null,
		   "unlock_at":"2026-09-11T00:00:00Z","html_url":"/courses/102/assignments/206"}
		]`)
	})

	mux.HandleFunc("/api/v1/courses/101/assignments/201", func(w http.ResponseWriter, r *http.Request) {
		json(w, fmt.Sprintf(`{
		  "id":201,"course_id":101,"name":"Cell Structure Lab",
		  "description":"<p>Draw the organelles.</p><ul><li>Use the handout: <a class=\"instructure_file_link\" href=\"/courses/101/files/501/download?verifier=abc123&amp;wrap=1\" data-api-endpoint=\"%s/api/v1/files/501\">Lab handout</a></li><li>Background reading: <a href=\"https://example.org/cells\">Cell biology primer</a></li></ul>",
		  "due_at":"2026-09-20T23:59:00Z","unlock_at":"2026-09-05T00:00:00Z","created_at":"2026-09-01T00:00:00Z",
		  "points_possible":20,"html_url":"/courses/101/assignments/201",
		  "submission_types":["online_upload"],
		  "submission":{"workflow_state":"unsubmitted","submitted_at":null,"late":false,"missing":false,
		                "excused":false,"score":null,"grade":null,"attachments":[]}
		}`, srvURL))
	})

	mux.HandleFunc("/api/v1/courses/101/assignments/202", func(w http.ResponseWriter, r *http.Request) {
		json(w, fmt.Sprintf(`{
		  "id":202,"course_id":101,"name":"Essay Draft",
		  "description":"<p>Write 500 words on mitosis.</p>",
		  "due_at":"2026-09-10T23:59:00Z","created_at":"2026-09-01T00:00:00Z","points_possible":20,
		  "html_url":"/courses/101/assignments/202","submission_types":["online_text_entry"],
		  "submission":{"workflow_state":"graded","submitted_at":"2026-09-11T02:10:00Z","late":true,
		                "missing":false,"excused":false,"score":17,"grade":null,
		                "submission_type":"online_text_entry",
		                "body":"<p>Mitosis has four phases.</p>",
		                "attachments":[{"id":502,"display_name":"draft.txt","filename":"draft.txt",
		                                "content-type":"text/plain","size":21,
		                                "url":"%s/files/502/download?verifier=zzz"}]}
		}`, srvURL))
	})

	mux.HandleFunc("/api/v1/courses/101/assignments/203", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"id":203,"course_id":101,"name":"Reading log","description":"","due_at":null,
		          "created_at":"2026-08-25T00:00:00Z","points_possible":5,"html_url":"/courses/101/assignments/203",
		          "submission":{"workflow_state":"unsubmitted"}}`)
	})

	mux.HandleFunc("/api/v1/courses/102/assignments/206", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"id":206,"course_id":102,"name":"Timeline worksheet","description":"<p>Fill the timeline.</p>",
		          "due_at":"2026-09-25T23:59:00Z","unlock_at":"2026-09-11T00:00:00Z","points_possible":null,
		          "html_url":"/courses/102/assignments/206","submission":{"workflow_state":"unsubmitted"}}`)
	})

	mux.HandleFunc("/api/v1/courses/101/quizzes/301", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"id":301,"title":"Genetics Quiz","description":"<p>Closed book. 20 minutes.</p>",
		          "due_at":"2026-09-18T23:59:00Z","unlock_at":"2026-09-16T00:00:00Z","points_possible":10,
		          "html_url":"/courses/101/quizzes/301"}`)
	})

	mux.HandleFunc("/api/v1/courses/101/discussion_topics/401", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"id":401,"title":"Intro thread","message":"<p>Say hi and name one organelle.</p>",
		          "html_url":"/courses/101/discussion_topics/401","posted_at":"2026-09-02T10:00:00Z"}`)
	})

	mux.HandleFunc("/api/v1/files/501", func(w http.ResponseWriter, r *http.Request) {
		json(w, fmt.Sprintf(`{"id":501,"display_name":"lab-handout.txt","filename":"lab-handout.txt",
		          "content-type":"text/plain","size":23,"url":"%s/files/501/download?verifier=abc123"}`, srvURL))
	})

	mux.HandleFunc("/files/501/download", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Read chapters 3 and 4.\n")
	})
	mux.HandleFunc("/files/502/download", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Mitosis has four phases.\n")
	})

	return srv
}

func testClient(t *testing.T, srv *httptest.Server, mutate func(*Options)) *Client {
	t.Helper()
	opts := Options{
		ExtractAttachments: true,
		MaxAttachmentBytes: 1 << 20,
		MaxExtractedText:   4096,
		PastDays:           30,
		FutureDays:         120,
		Logger:             quietLogger(),
		HTTPClient:         srv.Client(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	c := New(srv.URL, testToken, opts)
	c.now = func() time.Time { return testNow }
	c.retryBase = time.Millisecond
	return c
}

func byID(tasks []model.Task) map[string]model.Task {
	m := make(map[string]model.Task, len(tasks))
	for _, t := range tasks {
		m[t.ID] = t
	}
	return m
}

func TestFetch(t *testing.T) {
	stubExtractors(t)
	srv := fixtureServer(t)
	c := testClient(t, srv, nil)

	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got := byID(tasks)
	wantIDs := []string{
		"canvas:assignment:201",
		"canvas:assignment:202",
		"canvas:assignment:203",
		"canvas:assignment:206",
		"canvas:quiz:301",
		"canvas:discussion:401",
		"canvas:announcement:801", // information: routed to note's inbox, not a task
	}
	if len(tasks) != len(wantIDs) {
		var ids []string
		for _, tk := range tasks {
			ids = append(ids, tk.ID)
		}
		t.Fatalf("got %d tasks %v, want %d %v", len(tasks), ids, len(wantIDs), wantIDs)
	}
	for _, id := range wantIDs {
		if _, ok := got[id]; !ok {
			t.Fatalf("missing task %s", id)
		}
	}
	for _, skipped := range []string{"canvas:assignment:204", "canvas:assignment:205", "canvas:page:701"} {
		if _, ok := got[skipped]; ok {
			t.Errorf("task %s should have been skipped", skipped)
		}
	}

	// Sorted earliest due first, undated last.
	if tasks[0].ID != "canvas:assignment:202" {
		t.Errorf("first task = %s, want canvas:assignment:202", tasks[0].ID)
	}
	if tasks[len(tasks)-1].DueAt != nil {
		t.Errorf("last task should be undated, got %v", tasks[len(tasks)-1].DueAt)
	}

	t.Run("assignment with teacher attachments", func(t *testing.T) {
		a := got["canvas:assignment:201"]
		if a.Source != model.SourceCanvas || a.Kind != model.KindAssignment {
			t.Errorf("source/kind = %s/%s", a.Source, a.Kind)
		}
		if a.Course != "Biology 201" || a.CourseID != "101" {
			t.Errorf("course = %q/%q", a.Course, a.CourseID)
		}
		if a.Title != "Cell Structure Lab" {
			t.Errorf("title = %q", a.Title)
		}
		if a.DueAt == nil || !a.DueAt.Equal(mustTime(t, "2026-09-20T23:59:00Z")) {
			t.Errorf("due = %v", a.DueAt)
		}
		if a.AssignedAt == nil || !a.AssignedAt.Equal(mustTime(t, "2026-09-05T00:00:00Z")) {
			t.Errorf("assigned = %v, want unlock_at", a.AssignedAt)
		}
		if a.Points == nil || *a.Points != 20 {
			t.Errorf("points = %v", a.Points)
		}
		if a.Progress.State != model.StateNotStarted {
			t.Errorf("state = %s, want not_started", a.Progress.State)
		}
		if !strings.Contains(a.Description, "Draw the organelles.") {
			t.Errorf("description = %q", a.Description)
		}
		if strings.Contains(a.Description, "<p>") {
			t.Errorf("description still contains HTML: %q", a.Description)
		}
		if len(a.Attachments) != 2 {
			t.Fatalf("attachments = %+v", a.Attachments)
		}
		file := a.Attachments[0]
		if file.Name != "lab-handout.txt" {
			t.Errorf("attachment name = %q", file.Name)
		}
		if file.MimeType != "text/plain" || file.SizeBytes != 23 {
			t.Errorf("attachment meta = %q/%d", file.MimeType, file.SizeBytes)
		}
		if file.Content != "Read chapters 3 and 4." {
			t.Errorf("attachment content = %q", file.Content)
		}
		if file.ContentError != "" {
			t.Errorf("unexpected content error %q", file.ContentError)
		}
		link := a.Attachments[1]
		if link.URL != "https://example.org/cells" || link.MimeType != "text/uri-list" {
			t.Errorf("link attachment = %+v", link)
		}
		if link.Name != "Cell biology primer" {
			t.Errorf("link name = %q", link.Name)
		}
		if link.Content != "" {
			t.Errorf("link should not be extracted: %q", link.Content)
		}
		if !strings.HasPrefix(a.URL, srv.URL) && !strings.HasPrefix(a.URL, "/courses/") {
			t.Errorf("url = %q", a.URL)
		}
		if a.FetchedAt.IsZero() {
			t.Error("FetchedAt not set")
		}
	})

	t.Run("submitted and graded assignment", func(t *testing.T) {
		a := got["canvas:assignment:202"]
		p := a.Progress
		if p.State != model.StateGraded {
			t.Errorf("state = %s, want graded", p.State)
		}
		if !p.Late {
			t.Error("expected late")
		}
		if p.Grade != "17/20" {
			t.Errorf("grade = %q, want 17/20", p.Grade)
		}
		if p.SubmittedAt == nil || !p.SubmittedAt.Equal(mustTime(t, "2026-09-11T02:10:00Z")) {
			t.Errorf("submitted_at = %v", p.SubmittedAt)
		}
		if p.Text != "Mitosis has four phases." {
			t.Errorf("progress text = %q", p.Text)
		}
		if len(p.Attachments) != 1 {
			t.Fatalf("submission attachments = %+v", p.Attachments)
		}
		if p.Attachments[0].Name != "draft.txt" || p.Attachments[0].Content != "Mitosis has four phases." {
			t.Errorf("submission attachment = %+v", p.Attachments[0])
		}
	})

	t.Run("undated assignment from the course listing", func(t *testing.T) {
		a := got["canvas:assignment:203"]
		if a.DueAt != nil {
			t.Errorf("due = %v, want nil", a.DueAt)
		}
		if a.Progress.State != model.StateNotStarted {
			t.Errorf("state = %s", a.Progress.State)
		}
		if a.Title != "Reading log" {
			t.Errorf("title = %q", a.Title)
		}
	})

	t.Run("course_code fallback for an unnamed course", func(t *testing.T) {
		a := got["canvas:assignment:206"]
		if a.Course != "HIST100" || a.CourseID != "102" {
			t.Errorf("course = %q/%q", a.Course, a.CourseID)
		}
		if a.Points != nil {
			t.Errorf("points = %v, want nil", a.Points)
		}
		if a.AssignedAt == nil || !a.AssignedAt.Equal(mustTime(t, "2026-09-11T00:00:00Z")) {
			t.Errorf("assigned = %v", a.AssignedAt)
		}
	})

	t.Run("quiz", func(t *testing.T) {
		q := got["canvas:quiz:301"]
		if q.Kind != model.KindQuiz {
			t.Errorf("kind = %s", q.Kind)
		}
		if q.Title != "Genetics Quiz" {
			t.Errorf("title = %q", q.Title)
		}
		if q.Description != "Closed book. 20 minutes." {
			t.Errorf("description = %q", q.Description)
		}
		if q.DueAt == nil || !q.DueAt.Equal(mustTime(t, "2026-09-18T23:59:00Z")) {
			t.Errorf("due = %v", q.DueAt)
		}
		if q.Points == nil || *q.Points != 10 {
			t.Errorf("points = %v", q.Points)
		}
		if q.Progress.State != model.StateNotStarted {
			t.Errorf("state = %s", q.Progress.State)
		}
	})

	t.Run("discussion", func(t *testing.T) {
		d := got["canvas:discussion:401"]
		if d.Kind != model.KindDiscussion {
			t.Errorf("kind = %s", d.Kind)
		}
		if d.Description != "Say hi and name one organelle." {
			t.Errorf("description = %q", d.Description)
		}
		if d.Progress.State != model.StateSubmitted {
			t.Errorf("state = %s, want submitted (from planner)", d.Progress.State)
		}
		if d.DueAt != nil {
			t.Errorf("due = %v, want nil", d.DueAt)
		}
	})
}

func TestFetchWithoutExtraction(t *testing.T) {
	stubExtractors(t)
	srv := fixtureServer(t)
	c := testClient(t, srv, func(o *Options) { o.ExtractAttachments = false })

	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	a := byID(tasks)["canvas:assignment:201"]
	if len(a.Attachments) != 2 {
		t.Fatalf("attachments = %+v", a.Attachments)
	}
	if a.Attachments[0].Content != "" || a.Attachments[0].ContentError != "" {
		t.Errorf("attachment should be untouched: %+v", a.Attachments[0])
	}
	if a.Attachments[0].Name != "lab-handout.txt" {
		t.Errorf("metadata should still be fetched: %+v", a.Attachments[0])
	}
}

func TestFetchUnauthorized(t *testing.T) {
	stubExtractors(t)
	srv := fixtureServer(t)
	c := testClient(t, srv, nil)
	c.token = "expired"

	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "120 days") {
		t.Errorf("error = %v", err)
	}
}

func TestDetailFailureStillEmitsTask(t *testing.T) {
	stubExtractors(t)
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/courses", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":101,"name":"Biology 201"}]`)
	})
	mux.HandleFunc("/api/v1/courses/101/assignments", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/api/v1/planner/items", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"context_type":"Course","course_id":101,"plannable_type":"assignment","plannable_id":201,
		   "html_url":"/courses/101/assignments/201","submissions":{"missing":true},
		   "plannable":{"id":201,"title":"Cell Structure Lab","due_at":"2026-09-20T23:59:00Z","points_possible":20}}]`)
	})
	mux.HandleFunc("/api/v1/courses/101/assignments/201", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"errors":"boom"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := testClient(t, srv, nil)
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if tasks[0].Title != "Cell Structure Lab" || tasks[0].Progress.State != model.StateMissing {
		t.Errorf("degraded task = %+v", tasks[0])
	}
	if n := hits.Load(); n != maxRetries+1 {
		t.Errorf("5xx attempts = %d, want %d", n, maxRetries+1)
	}
}

func TestRateLimitRetry(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/courses", func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.Header().Set("X-Rate-Limit-Remaining", "0.0")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "403 Forbidden (Rate Limit Exceeded)")
			return
		}
		io.WriteString(w, `[{"id":101,"name":"Biology 201"}]`)
	})
	mux.HandleFunc("/api/v1/courses/101/assignments", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/api/v1/planner/items", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := testClient(t, srv, nil)
	c.token = ""
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks = %+v", tasks)
	}
	if hits.Load() != 3 {
		t.Errorf("attempts = %d, want 3", hits.Load())
	}
}

func TestExtractIntoLimitsAndFallbacks(t *testing.T) {
	stubExtractors(t)
	srv := fixtureServer(t)

	t.Run("too large", func(t *testing.T) {
		c := testClient(t, srv, func(o *Options) { o.MaxAttachmentBytes = 5 })
		att := model.Attachment{Name: "big.txt", URL: srv.URL + "/files/501/download", MimeType: "text/plain", SizeBytes: 999}
		var a, f atomic.Int64
		c.extractInto(context.Background(), &att, &a, &f)
		if att.Content != "" || !strings.Contains(att.ContentError, "too large") {
			t.Errorf("att = %+v", att)
		}
	})

	t.Run("extractor unavailable", func(t *testing.T) {
		orig := extractorReady
		extractorReady = func() bool { return false }
		defer func() { extractorReady = orig }()
		c := testClient(t, srv, nil)
		att := model.Attachment{Name: "x.txt", URL: srv.URL + "/files/501/download", MimeType: "text/plain"}
		var a, f atomic.Int64
		c.extractInto(context.Background(), &att, &a, &f)
		if att.ContentError != "extractor unavailable" {
			t.Errorf("content error = %q", att.ContentError)
		}
	})

	t.Run("unsupported type", func(t *testing.T) {
		c := testClient(t, srv, nil)
		att := model.Attachment{Name: "diagram.png", URL: srv.URL + "/files/501/download", MimeType: "image/png"}
		var a, f atomic.Int64
		c.extractInto(context.Background(), &att, &a, &f)
		if !strings.Contains(att.ContentError, "unsupported type") {
			t.Errorf("content error = %q", att.ContentError)
		}
		if f.Load() != 0 {
			t.Errorf("unsupported types should not count as failures")
		}
	})

	t.Run("truncation", func(t *testing.T) {
		c := testClient(t, srv, func(o *Options) { o.MaxExtractedText = 4 })
		att := model.Attachment{Name: "x.txt", URL: srv.URL + "/files/501/download", MimeType: "text/plain"}
		var a, f atomic.Int64
		c.extractInto(context.Background(), &att, &a, &f)
		if att.Content != "Read" || !att.Truncated {
			t.Errorf("att = %+v", att)
		}
	})
}

func TestFileIDFrom(t *testing.T) {
	cases := map[string]int64{
		"/files/501":          501,
		"/files/501/download": 501,
		"/courses/101/files/501/download?verifier=abc": 501,
		"https://x.instructure.com/api/v1/files/501":   501,
		"/courses/101/files/501/preview":               501,
		"https://example.org/cells":                    0,
		"/courses/101/assignments/201":                 0,
		"":                                             0,
		"https://x.instructure.com/courses/101/files":  0,
	}
	for in, want := range cases {
		got, ok := fileIDFrom(in)
		if want == 0 {
			if ok {
				t.Errorf("%q: got file id %d, want none", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q: got %d/%v, want %d", in, got, ok, want)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	stubExtractors(t)
	srv := fixtureServer(t)
	c := testClient(t, srv, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Fetch(ctx); err == nil {
		t.Fatal("expected a context error")
	}
}

func TestAttachmentFromUsesFileCache(t *testing.T) {
	stubExtractors(t)
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		io.WriteString(w, "Read chapter 3")
	}))
	t.Cleanup(srv.Close)
	cache, err := filecache.Open(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := testClient(t, srv, func(o *Options) { o.FileCache = cache })
	var a, f atomic.Int64
	file := apiAttachment{ID: "501", DisplayName: "notes.txt", URL: srv.URL + "/files/501/download",
		ContentType: "text/plain", Size: 14, UpdatedAt: "2026-09-01T10:00:00Z"}

	for i := 0; i < 2; i++ {
		if att := c.attachmentFrom(context.Background(), file, &a, &f); att.Content != "Read chapter 3" {
			t.Fatalf("run %d: att = %+v", i, att)
		}
	}
	if downloads.Load() != 1 {
		t.Errorf("unchanged file downloaded %d times, want 1", downloads.Load())
	}

	file.UpdatedAt = "2026-09-02T10:00:00Z" // teacher replaced the file
	c.attachmentFrom(context.Background(), file, &a, &f)
	if downloads.Load() != 2 {
		t.Errorf("replaced file not downloaded again (downloads = %d)", downloads.Load())
	}

	file.ID, file.UpdatedAt, file.ModifiedAt = "502", "", "" // no version: never cached
	c.attachmentFrom(context.Background(), file, &a, &f)
	c.attachmentFrom(context.Background(), file, &a, &f)
	if downloads.Load() != 4 {
		t.Errorf("unversioned file should always download (downloads = %d)", downloads.Load())
	}

	// Unsupported types are permanent for a version and cached too.
	png := apiAttachment{ID: "503", DisplayName: "d.png", URL: srv.URL + "/files/503/download",
		ContentType: "image/png", UpdatedAt: "2026-09-01T10:00:00Z"}
	c.attachmentFrom(context.Background(), png, &a, &f)
	if att := c.attachmentFrom(context.Background(), png, &a, &f); !strings.Contains(att.ContentError, "unsupported") {
		t.Errorf("cached unsupported att = %+v", att)
	}
	if downloads.Load() != 5 {
		t.Errorf("unsupported file downloaded again (downloads = %d)", downloads.Load())
	}
}
