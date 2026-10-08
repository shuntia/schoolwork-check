package classroom

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gclassroom "google.golang.org/api/classroom/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"schoolwork-check/internal/extract"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// testNow pins "now" so due-date windows and missing/not-started decisions are
// deterministic.
var testNow = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

const exportedDocText = "hello doc"

// fixtureServer serves the recorded Classroom and Drive responses in testdata.
// Classroom lives at /v1/..., Drive at /drive/v3/... .
type fixtureServer struct {
	// withAnnouncements serves testdata/announcements.json; otherwise the course has none.
	withAnnouncements bool
	// materialsStatus, when non-zero, is returned instead of the course work
	// materials fixture (used to simulate a domain that forbids the call).
	materialsStatus int
	// flaky makes the first N course work list requests fail with 503.
	flaky atomic.Int32

	mu   sync.Mutex
	hits map[string]int
}

func newFixtureServer() *fixtureServer {
	return &fixtureServer{hits: map[string]int{}}
}

func (f *fixtureServer) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func (f *fixtureServer) bump(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[key]++
}

func (f *fixtureServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	q := r.URL.Query()
	f.bump(p)

	switch {
	case p == "/v1/courses":
		if got := q.Get("studentId"); got != "me" {
			http.Error(w, "expected studentId=me, got "+got, http.StatusBadRequest)
			return
		}
		if got := q.Get("courseStates"); got != "ACTIVE" {
			http.Error(w, "expected courseStates=ACTIVE, got "+got, http.StatusBadRequest)
			return
		}
		writeFixture(w, "courses.json")

	case p == "/v1/courses/C1/courseWork":
		if n := f.flaky.Load(); n > 0 {
			f.flaky.Add(-1)
			http.Error(w, `{"error":{"code":503,"message":"backend error"}}`, http.StatusServiceUnavailable)
			return
		}
		if got := q.Get("courseWorkStates"); got != "PUBLISHED" {
			http.Error(w, "expected courseWorkStates=PUBLISHED, got "+got, http.StatusBadRequest)
			return
		}
		writeFixture(w, "coursework.json")

	case p == "/v1/courses/C1/announcements":
		if got := q.Get("announcementStates"); got != "PUBLISHED" {
			http.Error(w, "expected announcementStates=PUBLISHED, got "+got, http.StatusBadRequest)
			return
		}
		if !f.withAnnouncements {
			io.WriteString(w, "{}")
			return
		}
		writeFixture(w, "announcements.json")

	case p == "/v1/courses/C1/courseWorkMaterials":
		if f.materialsStatus != 0 {
			http.Error(w, `{"error":{"code":403,"message":"restricted"}}`, f.materialsStatus)
			return
		}
		writeFixture(w, "coursework_materials.json")

	case p == "/v1/courses/C1/courseWork/CW1/studentSubmissions":
		if got := q.Get("userId"); got != "me" {
			http.Error(w, "expected userId=me, got "+got, http.StatusBadRequest)
			return
		}
		writeFixture(w, "submission_cw1.json")

	case p == "/v1/courses/C1/courseWork/CW2/studentSubmissions":
		writeFixture(w, "submission_cw2.json")

	case p == "/drive/v3/files/DOC1/export":
		if got := q.Get("mimeType"); got != "text/plain" {
			http.Error(w, "expected mimeType=text/plain, got "+got, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, exportedDocText)

	case p == "/drive/v3/files/DOC1":
		writeFixture(w, "drive_doc1.json")

	case p == "/drive/v3/files/PDF1":
		if q.Get("alt") == "media" {
			w.Header().Set("Content-Type", "application/pdf")
			io.WriteString(w, "%PDF-1.4 fake bytes")
			return
		}
		writeFixture(w, "drive_pdf1.json")

	default:
		http.Error(w, "unexpected path "+p, http.StatusNotFound)
	}
}

func writeFixture(w http.ResponseWriter, name string) {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func newTestClient(t *testing.T, h http.Handler, opts Options) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cls, err := gclassroom.NewService(ctx,
		option.WithEndpoint(srv.URL+"/"),
		option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("gclassroom.NewService: %v", err)
	}
	drv, err := drive.NewService(ctx,
		option.WithEndpoint(srv.URL+"/drive/v3/"),
		option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("drive.NewService: %v", err)
	}
	c := newWithServices(cls, drv, opts)
	c.now = func() time.Time { return testNow }
	c.retryDelay = 0
	return c
}

// withExtractor installs a fake extract.Text for the duration of one test.
func withExtractor(t *testing.T, fn func(context.Context, extract.Input) (extract.Result, error)) {
	t.Helper()
	prev := extract.Text
	extract.Text = fn
	t.Cleanup(func() { extract.Text = prev })
}

func byID(tasks []model.Task, id string) *model.Task {
	for i := range tasks {
		if tasks[i].ID == id {
			return &tasks[i]
		}
	}
	return nil
}

func TestFetch(t *testing.T) {
	// extract.Text is nil (the extraction package has not registered an
	// implementation), so an uploaded PDF must surface a ContentError rather
	// than blowing up.
	withExtractor(t, nil)

	c := newTestClient(t, newFixtureServer(), Options{ExtractAttachments: true})
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3: %+v", len(tasks), tasks)
	}

	// --- assignment ---------------------------------------------------
	cw1 := byID(tasks, "classroom:C1:CW1")
	if cw1 == nil {
		t.Fatal("missing task classroom:C1:CW1")
	}
	if cw1.Source != model.SourceClassroom {
		t.Errorf("Source = %q", cw1.Source)
	}
	if cw1.Kind != model.KindAssignment {
		t.Errorf("Kind = %q, want assignment", cw1.Kind)
	}
	if cw1.Course != "Biology 101" || cw1.CourseID != "C1" {
		t.Errorf("Course = %q / %q", cw1.Course, cw1.CourseID)
	}
	if cw1.Title != "Cell essay" {
		t.Errorf("Title = %q", cw1.Title)
	}
	if cw1.URL != "https://classroom.google.com/c/C1/a/CW1/details" {
		t.Errorf("URL = %q", cw1.URL)
	}
	if cw1.Description != "Write 500 words on the cell.\nCite two sources." {
		t.Errorf("Description = %q (CRLF should be normalised)", cw1.Description)
	}
	wantDue := time.Date(2026, 3, 10, 23, 59, 0, 0, time.UTC)
	if cw1.DueAt == nil || !cw1.DueAt.Equal(wantDue) {
		t.Errorf("DueAt = %v, want %v", cw1.DueAt, wantDue)
	}
	if cw1.DueAt != nil && cw1.DueAt.Location() != time.UTC {
		t.Errorf("DueAt location = %v, want UTC", cw1.DueAt.Location())
	}
	wantAssigned := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	if cw1.AssignedAt == nil || !cw1.AssignedAt.Equal(wantAssigned) {
		t.Errorf("AssignedAt = %v, want %v (creationTime)", cw1.AssignedAt, wantAssigned)
	}
	if cw1.Points == nil || *cw1.Points != 100 {
		t.Errorf("Points = %v, want 100", cw1.Points)
	}
	if !cw1.FetchedAt.Equal(testNow) {
		t.Errorf("FetchedAt = %v, want %v", cw1.FetchedAt, testNow)
	}

	// teacher materials: a Google Doc (exported) and a link
	if len(cw1.Attachments) != 2 {
		t.Fatalf("got %d attachments, want 2: %+v", len(cw1.Attachments), cw1.Attachments)
	}
	doc := cw1.Attachments[0]
	if doc.Name != "Reading: mitochondria" {
		t.Errorf("attachment name = %q", doc.Name)
	}
	if doc.URL != "https://docs.google.com/document/d/DOC1/edit" {
		t.Errorf("attachment URL = %q", doc.URL)
	}
	if doc.MimeType != mimeGoogleDoc {
		t.Errorf("attachment mime = %q", doc.MimeType)
	}
	if doc.Content != exportedDocText {
		t.Errorf("attachment content = %q, want %q", doc.Content, exportedDocText)
	}
	if doc.ContentError != "" {
		t.Errorf("unexpected ContentError %q", doc.ContentError)
	}
	link := cw1.Attachments[1]
	if link.Name != "Syllabus" || link.URL != "https://example.org/syllabus" || link.MimeType != mimeLink {
		t.Errorf("link attachment = %+v", link)
	}

	// submission
	if cw1.Progress.State != model.StateSubmitted {
		t.Errorf("state = %q, want submitted", cw1.Progress.State)
	}
	if !cw1.Progress.Late {
		t.Error("Late = false, want true")
	}
	wantSubmitted := time.Date(2026, 2, 28, 18, 2, 0, 0, time.UTC)
	if cw1.Progress.SubmittedAt == nil || !cw1.Progress.SubmittedAt.Equal(wantSubmitted) {
		t.Errorf("SubmittedAt = %v, want %v", cw1.Progress.SubmittedAt, wantSubmitted)
	}
	if len(cw1.Progress.Attachments) != 1 {
		t.Fatalf("got %d submission attachments, want 1", len(cw1.Progress.Attachments))
	}
	pdf := cw1.Progress.Attachments[0]
	if pdf.Name != "essay.pdf" || pdf.URL != "https://drive.google.com/file/d/PDF1/view" {
		t.Errorf("pdf attachment = %+v", pdf)
	}
	if pdf.MimeType != "application/pdf" || pdf.SizeBytes != 2048 {
		t.Errorf("pdf metadata = %q / %d", pdf.MimeType, pdf.SizeBytes)
	}
	if pdf.ContentError != "extractor unavailable" {
		t.Errorf("ContentError = %q, want %q", pdf.ContentError, "extractor unavailable")
	}
	if pdf.Content != "" {
		t.Errorf("Content = %q, want empty", pdf.Content)
	}

	// --- short answer question ----------------------------------------
	cw2 := byID(tasks, "classroom:C1:CW2")
	if cw2 == nil {
		t.Fatal("missing task classroom:C1:CW2")
	}
	if cw2.Kind != model.KindQuestion {
		t.Errorf("Kind = %q, want question", cw2.Kind)
	}
	if cw2.Progress.State != model.StateGraded {
		t.Errorf("state = %q, want graded", cw2.Progress.State)
	}
	if cw2.Progress.Grade != "8/10" {
		t.Errorf("Grade = %q, want 8/10", cw2.Progress.Grade)
	}
	if cw2.Progress.Text != "About 30 to 32 ATP." {
		t.Errorf("Progress.Text = %q", cw2.Progress.Text)
	}
	wantScheduled := time.Date(2026, 2, 12, 8, 0, 0, 0, time.UTC)
	if cw2.AssignedAt == nil || !cw2.AssignedAt.Equal(wantScheduled) {
		t.Errorf("AssignedAt = %v, want %v (scheduledTime wins)", cw2.AssignedAt, wantScheduled)
	}
	if len(cw2.Attachments) != 0 {
		t.Errorf("want no attachments, got %+v", cw2.Attachments)
	}

	// --- course work material -----------------------------------------
	m1 := byID(tasks, "classroom:C1:material:M1")
	if m1 == nil {
		t.Fatal("missing task classroom:C1:material:M1")
	}
	if m1.Kind != model.KindMaterial {
		t.Errorf("Kind = %q, want material", m1.Kind)
	}
	if m1.DueAt != nil {
		t.Errorf("DueAt = %v, want nil", m1.DueAt)
	}
	if m1.Progress.State != model.StateNotStarted {
		t.Errorf("state = %q, want not_started", m1.Progress.State)
	}
	if len(m1.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(m1.Attachments))
	}
	yt := m1.Attachments[0]
	if yt.Name != "Photosynthesis in 5 minutes" || yt.URL != "https://youtu.be/YT1" || yt.MimeType != mimeYouTube {
		t.Errorf("youtube attachment = %+v", yt)
	}
	if yt.Content != "" || yt.ContentError != "" {
		t.Errorf("youtube attachment should not be extracted: %+v", yt)
	}
}

func TestFetchWithExtractor(t *testing.T) {
	var gotInput extract.Input
	withExtractor(t, func(_ context.Context, in extract.Input) (extract.Result, error) {
		b, _ := io.ReadAll(in.Reader)
		gotInput = in
		gotInput.Reader = nil
		if !strings.HasPrefix(string(b), "%PDF") {
			t.Errorf("extractor got %q, want PDF bytes", b)
		}
		return extract.Result{Text: "extracted essay text", Truncated: true}, nil
	})

	c := newTestClient(t, newFixtureServer(), Options{
		ExtractAttachments: true,
		MaxAttachmentBytes: 1 << 20,
		MaxExtractedText:   4096,
	})
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cw1 := byID(tasks, "classroom:C1:CW1")
	if cw1 == nil {
		t.Fatal("missing CW1")
	}
	pdf := cw1.Progress.Attachments[0]
	if pdf.Content != "extracted essay text" {
		t.Errorf("Content = %q", pdf.Content)
	}
	if !pdf.Truncated {
		t.Error("Truncated = false, want true")
	}
	if pdf.ContentError != "" {
		t.Errorf("ContentError = %q", pdf.ContentError)
	}
	if gotInput.Name != "essay.pdf" || gotInput.MimeType != "application/pdf" || gotInput.Limit != 4096 {
		t.Errorf("extract.Input = %+v", gotInput)
	}
}

func TestFetchNoExtraction(t *testing.T) {
	withExtractor(t, func(context.Context, extract.Input) (extract.Result, error) {
		t.Error("extractor must not be called when ExtractAttachments is false")
		return extract.Result{}, nil
	})
	fs := newFixtureServer()
	c := newTestClient(t, fs, Options{ExtractAttachments: false})
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cw1 := byID(tasks, "classroom:C1:CW1")
	doc := cw1.Attachments[0]
	if doc.Name != "Reading: mitochondria" || doc.URL == "" {
		t.Errorf("attachment metadata lost: %+v", doc)
	}
	if doc.Content != "" || doc.ContentError != "" || doc.MimeType != "" {
		t.Errorf("no Drive call should have been made: %+v", doc)
	}
	if n := fs.count("/drive/v3/files/DOC1"); n != 0 {
		t.Errorf("Drive metadata fetched %d times, want 0", n)
	}
}

func TestFetchAttachmentTooLarge(t *testing.T) {
	withExtractor(t, func(context.Context, extract.Input) (extract.Result, error) {
		t.Error("extractor must not be called for an oversized file")
		return extract.Result{}, nil
	})
	c := newTestClient(t, newFixtureServer(), Options{
		ExtractAttachments: true,
		MaxAttachmentBytes: 100, // the PDF fixture reports 2048 bytes
	})
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	pdf := byID(tasks, "classroom:C1:CW1").Progress.Attachments[0]
	if !strings.Contains(pdf.ContentError, "too large") {
		t.Errorf("ContentError = %q, want a size complaint", pdf.ContentError)
	}
}

func TestFetchDueWindow(t *testing.T) {
	withExtractor(t, nil)
	// now = 2026-03-01; CW1 is due 03-10, CW2 on 02-20, and the undated
	// material M1 was posted 02-01.
	ids := func(opts Options) []string {
		c := newTestClient(t, newFixtureServer(), opts)
		tasks, err := c.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		var out []string
		for _, tk := range tasks {
			out = append(out, tk.ID)
		}
		sort.Strings(out)
		return out
	}
	// +/-5 days: both dated tasks are out, and so is the material, posted
	// 28 days before now.
	if got := ids(Options{PastDays: 5, FutureDays: 5}); len(got) != 0 {
		t.Errorf("+/-5 days: got %v, want nothing", got)
	}
	// 30 days back, 5 ahead: CW2 and the material are in, CW1 is too far out.
	if got := strings.Join(ids(Options{PastDays: 30, FutureDays: 5}), ","); got != "classroom:C1:CW2,classroom:C1:material:M1" {
		t.Errorf("30/5 days: got %s", got)
	}
}

func TestFetchMaterialsForbidden(t *testing.T) {
	withExtractor(t, nil)
	fs := newFixtureServer()
	fs.materialsStatus = http.StatusForbidden
	c := newTestClient(t, fs, Options{})
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch should tolerate a forbidden courseWorkMaterials call, got %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2 course work tasks: %+v", len(tasks), tasks)
	}
}

func TestFetchRetriesServerErrors(t *testing.T) {
	withExtractor(t, nil)
	fs := newFixtureServer()
	fs.flaky.Store(2) // two 503s, then success — within the 3-attempt budget
	c := newTestClient(t, fs, Options{})
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(tasks))
	}
	if n := fs.count("/v1/courses/C1/courseWork"); n != 3 {
		t.Errorf("courseWork requested %d times, want 3 (2 failures + 1 success)", n)
	}
}

func TestFetchUnauthorized(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":401,"message":"Invalid Credentials"}}`, http.StatusUnauthorized)
	}), Options{})
	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "google-login") {
		t.Errorf("error %q does not tell the user how to recover", err)
	}
	if !strings.Contains(err.Error(), "7 days") {
		t.Errorf("error %q does not mention the Testing-mode 7-day expiry", err)
	}
}

func TestFetchCancelledContext(t *testing.T) {
	withExtractor(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTestClient(t, newFixtureServer(), Options{})
	if _, err := c.Fetch(ctx); err == nil {
		t.Fatal("want a context error")
	}
}

func TestFetchUsesFileCache(t *testing.T) {
	withExtractor(t, func(_ context.Context, in extract.Input) (extract.Result, error) {
		b, err := io.ReadAll(in.Reader)
		return extract.Result{Text: "pdf: " + string(b)}, err
	})
	cache, err := filecache.Open(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	fs := newFixtureServer()
	fetch := func() []model.Task {
		c := newTestClient(t, fs, Options{ExtractAttachments: true, MaxAttachmentBytes: 1 << 20, FileCache: cache})
		tasks, err := c.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		return tasks
	}

	first := fetch()
	exports, pdfCalls := fs.count("/drive/v3/files/DOC1/export"), fs.count("/drive/v3/files/PDF1")
	if exports == 0 || pdfCalls < 2 {
		t.Fatalf("first run should export and download: exports %d, pdf calls %d", exports, pdfCalls)
	}

	second := fetch()
	if n := fs.count("/drive/v3/files/DOC1/export"); n != exports {
		t.Errorf("unchanged Doc exported again: %d → %d", exports, n)
	}
	// Metadata is still read every run (it carries the version); the
	// download is not.
	if n := fs.count("/drive/v3/files/PDF1"); n != pdfCalls+1 {
		t.Errorf("PDF calls %d → %d, want only one more (metadata)", pdfCalls, n)
	}
	a, b := byID(first, "classroom:C1:CW1"), byID(second, "classroom:C1:CW1")
	compared := 0
	for _, pair := range [][2][]model.Attachment{{a.Attachments, b.Attachments}, {a.Progress.Attachments, b.Progress.Attachments}} {
		for i := range pair[0] {
			if pair[0][i].Content == "" { // links carry no content
				continue
			}
			compared++
			if pair[0][i].Content != pair[1][i].Content {
				t.Errorf("cached content differs for %s: %q vs %q", pair[0][i].Name, pair[0][i].Content, pair[1][i].Content)
			}
		}
	}
	if compared < 2 {
		t.Errorf("expected the Doc and the PDF to carry content, compared %d", compared)
	}
	if hits, _ := cache.Stats(); hits == 0 {
		t.Error("no cache hits recorded")
	}
}

func TestFetchAnnouncements(t *testing.T) {
	withExtractor(t, nil)
	// now = 2026-03-01: A1 was posted 02-25 (kept), A2 on 2025-12-01 (outside 30 days).
	fs := newFixtureServer()
	fs.withAnnouncements = true
	c := newTestClient(t, fs, Options{PastDays: 30, FutureDays: 30})
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	a := byID(tasks, "classroom:C1:announcement:A1")
	if a == nil {
		t.Fatal("announcement A1 missing")
	}
	if a.Kind != model.KindAnnouncement || !a.Kind.Informational() {
		t.Errorf("kind = %q", a.Kind)
	}
	if a.Title != "Quiz on chapter 6 moved to Thursday Mar 5." {
		t.Errorf("title = %q", a.Title)
	}
	if !strings.Contains(a.Description, "Bring a pencil") || a.AssignedAt == nil || a.URL == "" {
		t.Errorf("announcement = %+v", a)
	}
	if byID(tasks, "classroom:C1:announcement:A2") != nil {
		t.Error("announcement posted before the window was kept")
	}
}

func TestAnnouncementTitle(t *testing.T) {
	long := strings.Repeat("word ", 30)
	for in, want := range map[string]string{
		"":                       "Announcement",
		"  Hello\tclass  \nmore": "Hello class",
		long:                     string([]rune(strings.TrimSpace(long))[:79]) + "…",
	} {
		if got := announcementTitle(in); got != want {
			t.Errorf("announcementTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
