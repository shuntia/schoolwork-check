package gdoc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// fakeSource serves one document without touching Drive, counting metadata
// reads and text downloads separately: skipping the download is the point of
// the re-read interval.
type fakeSource struct {
	doc   doc
	err   error
	metas int
	fills int
}

func (f *fakeSource) Meta(context.Context, string) (doc, error) {
	f.metas++
	if f.err != nil {
		return doc{}, f.err
	}
	d := f.doc
	d.Text = "" // metadata carries no text
	return d, nil
}

func (f *fakeSource) Fill(_ context.Context, d *doc) error {
	f.fills++
	if f.err != nil {
		return f.err
	}
	d.Text, d.Truncated = f.doc.Text, f.doc.Truncated
	return nil
}

// fakeLLM replays canned answers, one per call. Chunks are parsed
// concurrently, so it locks.
type fakeLLM struct {
	mu      sync.Mutex
	answers []string
	err     error
	calls   int
	prompts []string
}

func (f *fakeLLM) Complete(_ context.Context, _, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.prompts = append(f.prompts, user)
	if f.err != nil {
		return "", f.err
	}
	i := f.calls - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i], nil
}

// saidPart reports whether any prompt announced this part of the document.
func (f *fakeLLM) saidPart(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prompts {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

var now = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

func testDoc() doc {
	return doc{
		ID:      "FILEID",
		Name:    "Biology — Fall calendar",
		URL:     "https://docs.google.com/document/d/FILEID/edit",
		Version: "7|2026-09-01T00:00:00Z||65536",
		Text:    "| Sep 18 | Read ch. 3 |\n| Sep 25 | Lab report due |\n",
	}
}

func newTestClient(t *testing.T, src docSource, llm *fakeLLM) (*Client, *filecache.Cache) {
	t.Helper()
	cache, err := filecache.Open(filepath.Join(t.TempDir(), "cache.json"))
	if err != nil {
		t.Fatalf("filecache.Open: %v", err)
	}
	var completer *fakeLLM
	if llm != nil {
		completer = llm
	}
	c := newWithSource(src, nil, Options{
		Docs:       []Doc{{ID: "FILEID"}},
		Model:      "test-model",
		PastDays:   30,
		FutureDays: 120,
		FileCache:  cache,
		Location:   time.UTC,
		Now:        func() time.Time { return now },
	})
	if completer != nil {
		c.llm = completer
	}
	return c, cache
}

const twoRows = `{"entries":[
 {"date":"2026-09-18","due_date":null,"time":null,"kind":"material","title":"Read chapter 3","description":"Pages 40-58."},
 {"date":"2026-09-25","due_date":null,"time":"15:30","kind":"assignment","title":"Lab report","description":""}
]}`

func TestFetchParsesRowsIntoTasks(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	llm := &fakeLLM{answers: []string{twoRows}}
	c, _ := newTestClient(t, src, llm)

	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2: %+v", len(tasks), tasks)
	}

	reading, lab := tasks[0], tasks[1]
	if reading.Kind != model.KindMaterial || !reading.Kind.Informational() {
		t.Errorf("reading kind = %q, want an informational material", reading.Kind)
	}
	if lab.Kind != model.KindAssignment {
		t.Errorf("lab kind = %q, want assignment", lab.Kind)
	}
	if lab.Source != model.SourceGDoc {
		t.Errorf("source = %q, want gdoc", lab.Source)
	}
	if lab.Course != "Biology — Fall calendar" {
		t.Errorf("course = %q, want the document name", lab.Course)
	}
	if lab.URL != testDoc().URL {
		t.Errorf("url = %q, want the document link", lab.URL)
	}
	if want := "gdoc:FILEID:2026-09-25-lab-report"; lab.ID != want {
		t.Errorf("id = %q, want %q", lab.ID, want)
	}
	if lab.DueAt == nil || !lab.DueAt.Equal(time.Date(2026, 9, 25, 15, 30, 0, 0, time.UTC)) {
		t.Errorf("due = %v, want the stated time of day", lab.DueAt)
	}
	if reading.DueAt == nil || !reading.DueAt.Equal(time.Date(2026, 9, 18, 23, 59, 0, 0, time.UTC)) {
		t.Errorf("due = %v, want end of the row's day", reading.DueAt)
	}
	if !strings.Contains(reading.Description, "Pages 40-58.") {
		t.Errorf("description lost the row text: %q", reading.Description)
	}
	if !strings.Contains(reading.Description, "row dated Fri 18 Sep 2026") {
		t.Errorf("description lost its provenance: %q", reading.Description)
	}
	if reading.Progress.State != model.StateNotStarted {
		t.Errorf("state = %q, want not_started", reading.Progress.State)
	}
	if got := llm.prompts[0]; !strings.Contains(got, "Today: 2026-09-17") {
		t.Errorf("prompt does not carry today's date: %q", got)
	}
}

func TestFetchSkipsTheDownloadWhenTheParseIsFresh(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	llm := &fakeLLM{answers: []string{twoRows}}
	c, _ := newTestClient(t, src, llm)

	for i := 0; i < 3; i++ {
		if _, err := c.Fetch(context.Background()); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if src.metas != 3 {
		t.Errorf("read metadata %d times, want 3", src.metas)
	}
	if src.fills != 1 {
		t.Errorf("downloaded the document %d times, want 1: a fresh parse needs no text", src.fills)
	}
}

func TestIntervalHoldsAParseAcrossAnEdit(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	llm := &fakeLLM{answers: []string{twoRows}}
	c, _ := newTestClient(t, src, llm)
	c.opts.Interval = 7 * 24 * time.Hour
	clock := now
	c.opts.Now = func() time.Time { return clock }

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	// The teacher edits the calendar two days later: too soon to re-read.
	edited := testDoc()
	edited.Version = "8|2026-09-19T09:00:00Z||65536"
	src.doc = edited
	clock = now.AddDate(0, 0, 2)
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch after the edit: %v", err)
	}
	if llm.calls != 1 {
		t.Errorf("model called %d times, want 1: the weekly re-read is not due", llm.calls)
	}
	if src.fills != 1 {
		t.Errorf("downloaded the document %d times, want 1", src.fills)
	}
	if len(tasks) != 2 {
		t.Errorf("got %d tasks, want the 2 from the last parse", len(tasks))
	}

	// Eight days on, it is due.
	clock = now.AddDate(0, 0, 8)
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch after a week: %v", err)
	}
	if llm.calls != 2 {
		t.Errorf("model called %d times, want 2 once the interval is up", llm.calls)
	}
}

func TestPerDocumentIntervalWins(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	c, _ := newTestClient(t, src, &fakeLLM{answers: []string{twoRows}})
	c.opts.Interval = 7 * 24 * time.Hour
	c.opts.Docs = []Doc{{ID: "FILEID", Interval: time.Hour}}

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	edited := testDoc()
	edited.Version = "9|2026-09-17T11:00:00Z||65536"
	src.doc = edited
	c.opts.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got := c.llm.(*fakeLLM).calls; got != 2 {
		t.Errorf("model called %d times, want 2: this document re-reads hourly", got)
	}
}

func TestFetchReusesParseUntilTheDocumentChanges(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	llm := &fakeLLM{answers: []string{twoRows}}
	c, _ := newTestClient(t, src, llm)

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if llm.calls != 1 {
		t.Errorf("model called %d times, want 1: an unedited calendar must cost nothing", llm.calls)
	}

	// A new revision must be parsed again.
	d := testDoc()
	d.Version = "8|2026-09-17T09:00:00Z||65536"
	src.doc = d
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("third Fetch: %v", err)
	}
	if llm.calls != 2 {
		t.Errorf("model called %d times, want 2 after an edit", llm.calls)
	}
}

func TestFetchWithoutModelPassesTheDocumentOnWhole(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	c, _ := newTestClient(t, src, nil)

	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1 whole document", len(tasks))
	}
	got := tasks[0]
	if got.ID != "gdoc:FILEID" {
		t.Errorf("id = %q, want the stable document id", got.ID)
	}
	if !got.Kind.Informational() {
		t.Errorf("kind = %q, want an informational item for the inbox", got.Kind)
	}
	if !strings.Contains(got.Description, "Lab report due") {
		t.Errorf("description lost the document text: %q", got.Description)
	}
}

func TestFetchWithNoDatedRowsFallsBackToTheWholeDocument(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	llm := &fakeLLM{answers: []string{`{"entries":[]}`}}
	c, _ := newTestClient(t, src, llm)

	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "gdoc:FILEID" {
		t.Fatalf("got %+v, want the whole document as one item", tasks)
	}
}

func TestFetchModelFailureIsReportedAndNotCached(t *testing.T) {
	src := &fakeSource{doc: testDoc()}
	llm := &fakeLLM{err: errors.New("429 rate limited")}
	c, cache := newTestClient(t, src, llm)

	tasks, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch: want an error when the model fails")
	}
	if len(tasks) != 0 {
		t.Errorf("got %d tasks, want none: a failed parse must not dump the document", len(tasks))
	}
	if _, ok := cache.Get(cacheKey("FILEID"), c.cacheVersion()); ok {
		t.Error("a failed parse was cached; the next run would never retry")
	}
}

func TestFetchKeepsGoingWhenOneDocumentFails(t *testing.T) {
	src := &fakeSource{err: errors.New("403 no access")}
	c, _ := newTestClient(t, src, &fakeLLM{answers: []string{twoRows}})
	c.opts.Docs = []Doc{{ID: "BROKEN"}}

	tasks, err := c.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "BROKEN") {
		t.Fatalf("err = %v, want it to name the document", err)
	}
	if len(tasks) != 0 {
		t.Errorf("got %d tasks, want none", len(tasks))
	}
}

func TestTasksHonourTheDateWindowAndDeduplicate(t *testing.T) {
	c, _ := newTestClient(t, &fakeSource{doc: testDoc()}, nil)
	entries := []entry{
		{Date: "2026-01-05", Kind: "assignment", Title: "Long past"},
		{Date: "2027-09-01", Kind: "assignment", Title: "Far future"},
		{Date: "2026-09-25", Kind: "assignment", Title: "Lab report"},
		{Date: "2026-09-25", Kind: "quiz", Title: "Lab Report!"}, // same day, same words
		{Date: "not a date", Kind: "assignment", Title: "Undated"},
		{Date: "2026-09-26", Kind: "assignment", Title: "   "},
	}
	got := c.tasks(testDoc(), "Biology", entries, now)
	if len(got) != 1 {
		t.Fatalf("got %d tasks, want 1: %+v", len(got), ids(got))
	}
	if got[0].Title != "Lab report" {
		t.Errorf("kept %q, want the first of the duplicate rows", got[0].Title)
	}
}

func TestCourseNameOverridesTheDocumentName(t *testing.T) {
	c, _ := newTestClient(t, &fakeSource{doc: testDoc()}, &fakeLLM{answers: []string{twoRows}})
	c.opts.Docs = []Doc{{ID: "FILEID", Course: "AP Biology"}}
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, task := range tasks {
		if task.Course != "AP Biology" {
			t.Errorf("course = %q, want the configured name", task.Course)
		}
	}
}

func TestKindOf(t *testing.T) {
	cases := map[string]model.Kind{
		"assignment":   model.KindAssignment,
		"Homework":     model.KindAssignment,
		"quiz":         model.KindQuiz,
		"EXAM":         model.KindQuiz,
		"material":     model.KindMaterial,
		"announcement": model.KindAnnouncement,
		"nonsense":     model.KindMaterial,
		"":             model.KindMaterial,
	}
	for in, want := range cases {
		if got := kindOf(in); got != want {
			t.Errorf("kindOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeEntries(t *testing.T) {
	cases := []struct {
		name, in string
		want     int
		wantErr  bool
	}{
		{"object", `{"entries":[{"date":"2026-09-18","title":"a"}]}`, 1, false},
		{"bare array", `[{"date":"2026-09-18","title":"a"}]`, 1, false},
		{"fenced", "```json\n{\"entries\":[{\"date\":\"2026-09-18\",\"title\":\"a\"}]}\n```", 1, false},
		{"empty list", `{"entries":[]}`, 0, false},
		{"prose", `I could not find a calendar.`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeEntries(tc.in)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && len(got) != tc.want {
				t.Errorf("got %d entries, want %d", len(got), tc.want)
			}
		})
	}
}

func TestSplitLinesNeverCutsMidLine(t *testing.T) {
	text := strings.Repeat("a line of calendar text\n", 100)
	chunks := splitLines(text, 200)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the text split", len(chunks))
	}
	for i, c := range chunks {
		if i < len(chunks)-1 && !strings.HasSuffix(c, "\n") {
			t.Errorf("chunk %d ends mid-line: %q", i, c[len(c)-20:])
		}
	}
	if strings.Join(chunks, "") != text {
		t.Error("splitting lost or reordered text")
	}
}

func TestSplitLinesKeepsShortTextWhole(t *testing.T) {
	if got := splitLines("one\ntwo\n", 1024); len(got) != 1 || got[0] != "one\ntwo\n" {
		t.Errorf("got %q, want the text unsplit", got)
	}
}

func TestLongCalendarIsParsedInSeveralPasses(t *testing.T) {
	d := testDoc()
	d.Text = strings.Repeat("| Sep 18 | a row of the calendar |\n", 2000) // > maxChunkBytes
	src := &fakeSource{doc: d}
	llm := &fakeLLM{answers: []string{`{"entries":[{"date":"2026-09-18","kind":"material","title":"Row"}]}`}}
	c, _ := newTestClient(t, src, llm)

	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if llm.calls < 2 {
		t.Errorf("model called %d times, want one call per chunk", llm.calls)
	}
	if len(tasks) != 1 {
		t.Errorf("got %d tasks, want the identical rows merged into 1", len(tasks))
	}
	if !llm.saidPart("part 1 of") || !llm.saidPart("part 2 of") {
		t.Errorf("chunked prompts do not say which part they are: %q", firstLines(llm.prompts[0], 5))
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Lab report":                    "lab-report",
		"Lab Report!":                   "lab-report",
		"  Essay #2: the New Deal  ":    "essay-2-the-new-deal",
		"読書感想文":                         "読書感想文",
		"!!!":                           "row",
		strings.Repeat("very long ", 9): "very-long-very-long-very-long-very-long",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClipCutsOnRuneBoundaries(t *testing.T) {
	got := clip("読書感想文", 7)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clip(%q) = %q, want an ellipsis", "読書感想文", got)
	}
	if !json.Valid([]byte(`"` + got + `"`)) {
		t.Errorf("clip produced invalid UTF-8: %q", got)
	}
}

func ids(ts []model.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// --- calendars posted in the LMS ---

func material(title, text string) model.Task {
	return model.Task{
		ID: "classroom:c1:m1", Source: model.SourceClassroom, Kind: model.KindMaterial,
		Course: "Film Analysis", CourseID: "c1", Title: title,
		URL:         "https://classroom.google.com/c/c1/m/m1",
		Attachments: []model.Attachment{{Name: "Calendar.gdoc", URL: "https://docs.google.com/document/d/DOCID/edit", Content: text}},
	}
}

var longCalendar = strings.Repeat("| Sep 18 | Watch Rear Window | Analysis due Sep 25 |\n", 8)

func newParser(t *testing.T, llm *fakeLLM) *Client {
	t.Helper()
	cache, err := filecache.Open(filepath.Join(t.TempDir(), "cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewParser(llm, Options{
		Model: "test-model", PastDays: 30, FutureDays: 120,
		FileCache: cache, Location: time.UTC,
		Now: func() time.Time { return now },
	})
}

func TestExpandReadsACalendarPostedInTheLMS(t *testing.T) {
	llm := &fakeLLM{answers: []string{twoRows}}
	p := newParser(t, llm)
	in := []model.Task{
		{ID: "canvas:1", Source: model.SourceCanvas, Kind: model.KindAssignment, Title: "Essay"},
		material("Film Analysis Calendar", longCalendar),
	}
	out, st := p.Expand(context.Background(), in)

	if st.Found != 1 || st.Expanded != 1 || st.Rows != 2 {
		t.Fatalf("stats = %+v, want 1 found, 1 expanded, 2 rows", st)
	}
	if len(out) != 3 {
		t.Fatalf("got %d tasks, want the essay plus 2 rows: %v", len(out), ids(out))
	}
	for _, task := range out {
		if task.ID == "classroom:c1:m1" {
			t.Error("the material itself is still in the list; its rows replace it")
		}
	}
	row := out[2]
	if row.Source != model.SourceGDoc {
		t.Errorf("row source = %q, want gdoc so the duplicate check sees it", row.Source)
	}
	if row.Course != "Film Analysis" || row.CourseID != "c1" {
		t.Errorf("row course = %q/%q, want the material's course", row.Course, row.CourseID)
	}
	if !strings.HasPrefix(row.ID, "gdoc:classroom:c1:m1:") {
		t.Errorf("row id = %q, want it derived from the material", row.ID)
	}
}

func TestExpandLeavesEverythingElseAlone(t *testing.T) {
	llm := &fakeLLM{answers: []string{twoRows}}
	p := newParser(t, llm)
	in := []model.Task{
		material("Unit 3 reading list", longCalendar), // no "calendar" in the title
		{ID: "canvas:9", Source: model.SourceCanvas, Kind: model.KindQuiz,
			Title: "Calendar and timeline quiz", Description: longCalendar}, // work, not a resource
		material("Short calendar", "see the link"), // nothing to parse
	}
	out, st := p.Expand(context.Background(), in)
	if len(out) != 3 {
		t.Fatalf("got %d tasks, want all 3 untouched", len(out))
	}
	if llm.calls != 0 {
		t.Errorf("made %d model calls, want none", llm.calls)
	}
	if st.Found != 1 || st.Skipped != 1 {
		t.Errorf("stats = %+v, want only the short one found and skipped", st)
	}
}

func TestExpandSkipsACalendarAlreadyReadAsADocument(t *testing.T) {
	llm := &fakeLLM{answers: []string{twoRows}}
	p := newParser(t, llm)
	p.opts.SkipDocIDs = []string{"DOCID"}

	out, st := p.Expand(context.Background(), []model.Task{material("Film Analysis Calendar", longCalendar)})
	if len(out) != 0 {
		t.Fatalf("got %v, want the material dropped: its rows come from the configured document", ids(out))
	}
	if llm.calls != 0 || st.Skipped != 1 {
		t.Errorf("calls = %d, stats = %+v, want no call and 1 skipped", llm.calls, st)
	}
}

func TestExpandKeepsTheMaterialWhenTheModelFails(t *testing.T) {
	llm := &fakeLLM{err: errors.New("503")}
	p := newParser(t, llm)
	in := []model.Task{material("Film Analysis Calendar", longCalendar)}
	out, st := p.Expand(context.Background(), in)
	if len(out) != 1 || out[0].ID != "classroom:c1:m1" {
		t.Fatalf("got %v, want the material left exactly as it was", ids(out))
	}
	if st.Failed != 1 {
		t.Errorf("stats = %+v, want 1 failure", st)
	}
}

func TestExpandReusesItsParseUntilTheTextChanges(t *testing.T) {
	llm := &fakeLLM{answers: []string{twoRows}}
	p := newParser(t, llm)
	in := []model.Task{material("Film Analysis Calendar", longCalendar)}

	if _, st := p.Expand(context.Background(), in); st.Expanded != 1 {
		t.Fatalf("first Expand: %+v", st)
	}
	if _, st := p.Expand(context.Background(), in); st.Expanded != 1 {
		t.Fatalf("second Expand: %+v", st)
	}
	if llm.calls != 1 {
		t.Errorf("model called %d times, want 1: the text has not changed", llm.calls)
	}

	edited := []model.Task{material("Film Analysis Calendar", longCalendar+"| Oct 2 | Watch Vertigo | |\n")}
	if _, st := p.Expand(context.Background(), edited); st.Expanded != 1 {
		t.Fatalf("third Expand: %+v", st)
	}
	if llm.calls != 2 {
		t.Errorf("model called %d times, want 2 after the teacher edited it", llm.calls)
	}
}
