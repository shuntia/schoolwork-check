package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"schoolwork-check/internal/model"
)

const goodAnswer = `{"homework":true,"skip_reason":"ignored when homework","summary":"Write a close reading.","deliverable":"PDF","steps":["Pick passage","Write"],"requirements":["600-800 words"],"estimate_min":90}`

// fake answers from a queue per call, then repeats the last answer.
type fake struct {
	mu      sync.Mutex
	calls   int
	prompts []string
	answers []string
	err     error
}

func (f *fake) Complete(_ context.Context, _, user string) (string, error) {
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

func task(id string, state model.State) model.Task {
	return model.Task{
		ID: id, Kind: model.KindAssignment, Course: "English", Title: "Essay " + id,
		Description: "Write it.", URL: "https://c/" + id,
		Attachments: []model.Attachment{{Name: "rubric.pdf", Content: "600-800 words"}},
		Progress:    model.Progress{State: state},
	}
}

func opts(t *testing.T) Options {
	return Options{Model: "m1", CacheFile: filepath.Join(t.TempDir(), "cache.json")}
}

func TestRunEnrichesOpenTasksAndCaches(t *testing.T) {
	f := &fake{answers: []string{goodAnswer}}
	o := opts(t)
	tasks := []model.Task{task("a", model.StateNotStarted), task("b", model.StateMissing), task("c", model.StateGraded)}

	st, err := Run(context.Background(), f, tasks, o)
	if err != nil {
		t.Fatal(err)
	}
	if st.Enriched != 2 || st.Skipped != 1 || f.calls != 2 {
		t.Fatalf("stats = %+v, calls = %d", st, f.calls)
	}
	e := tasks[0].Enrichment
	if e == nil || e.Summary != "Write a close reading." || *e.EstimateMin != 90 || e.Model != "m1" || len(e.Steps) != 2 {
		t.Fatalf("enrichment = %+v", e)
	}
	if tasks[2].Enrichment != nil {
		t.Error("finished work must not be enriched")
	}

	// Second run: volatile fields change, teacher text does not → no calls.
	again := []model.Task{task("a", model.StateInProgress), task("b", model.StateMissing)}
	again[0].URL = "https://changed"
	st, err = Run(context.Background(), f, again, o)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cached != 2 || f.calls != 2 || again[0].Enrichment == nil {
		t.Fatalf("second run stats = %+v, calls = %d", st, f.calls)
	}

	// Teacher edits the description → one fresh call.
	again[1].Description = "Write it. Now 1000 words."
	st, _ = Run(context.Background(), f, again, o)
	if st.Cached != 1 || st.Enriched != 1 || f.calls != 3 {
		t.Fatalf("third run stats = %+v, calls = %d", st, f.calls)
	}

	// Different model → cache miss.
	o.Model = "m2"
	st, _ = Run(context.Background(), f, again, o)
	if st.Enriched != 2 {
		t.Fatalf("model switch stats = %+v", st)
	}
}

func TestRunRetriesBadAnswerOnce(t *testing.T) {
	f := &fake{answers: []string{"sorry, here you go", "```json\n" + goodAnswer + "\n```"}}
	tasks := []model.Task{task("a", model.StateNotStarted)}
	st, _ := Run(context.Background(), f, tasks, opts(t))
	if st.Enriched != 1 || f.calls != 2 || tasks[0].Enrichment == nil {
		t.Fatalf("stats = %+v, calls = %d", st, f.calls)
	}
	if !strings.Contains(f.prompts[1], "previous reply was rejected") {
		t.Errorf("retry prompt = %q", f.prompts[1])
	}

	f = &fake{answers: []string{`{"summary":""}`}}
	tasks = []model.Task{task("b", model.StateNotStarted)}
	st, _ = Run(context.Background(), f, tasks, opts(t))
	if st.Failed != 1 || f.calls != 2 || tasks[0].Enrichment != nil {
		t.Fatalf("stats = %+v, calls = %d", st, f.calls)
	}
}

func TestRunStopKeepsStaleBriefAndIsNotAnError(t *testing.T) {
	o := opts(t)
	o.Concurrency = 1
	tasks := []model.Task{task("a", model.StateNotStarted), task("b", model.StateNotStarted)}
	if _, err := Run(context.Background(), &fake{answers: []string{goodAnswer}}, tasks, o); err != nil {
		t.Fatal(err)
	}

	tasks = []model.Task{task("a", model.StateNotStarted), task("b", model.StateNotStarted)}
	tasks[0].Description, tasks[1].Description = "changed", "changed"
	f := &fake{err: fmt.Errorf("%w: HTTP 429", ErrStop)}
	st, err := Run(context.Background(), f, tasks, o)
	if err != nil {
		t.Fatalf("a refused provider must not fail the run: %v", err)
	}
	if st.Deferred != 2 || f.calls != 1 {
		t.Fatalf("stats = %+v, calls = %d (should stop after the first refusal)", st, f.calls)
	}
	for _, tk := range tasks {
		if tk.Enrichment == nil || tk.Enrichment.Summary != "Write a close reading." {
			t.Errorf("%s: stale brief not kept: %+v", tk.ID, tk.Enrichment)
		}
	}
}

func TestRunMaxCalls(t *testing.T) {
	o := opts(t)
	o.MaxCalls = 1
	f := &fake{answers: []string{goodAnswer}}
	tasks := []model.Task{task("a", model.StateNotStarted), task("b", model.StateNotStarted), task("c", model.StateNotStarted)}
	st, _ := Run(context.Background(), f, tasks, o)
	if st.Enriched != 1 || st.Deferred != 2 || f.calls != 1 {
		t.Fatalf("stats = %+v, calls = %d", st, f.calls)
	}
	st, _ = Run(context.Background(), f, tasks, o)
	if st.Cached != 1 || st.Enriched != 1 || st.Deferred != 1 {
		t.Fatalf("next run stats = %+v", st)
	}
}

func TestPromptLeavesOutVolatileAndStudentFields(t *testing.T) {
	tk := task("a", model.StateSubmitted)
	tk.Progress.Text = "MY SECRET DRAFT"
	tk.Progress.Attachments = []model.Attachment{{Name: "draft.docx", Content: "MY DRAFT FILE"}}
	p := TaskContext(tk)
	for _, bad := range []string{"https://c/a", "MY SECRET DRAFT", "MY DRAFT FILE", "submitted"} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt contains %q:\n%s", bad, p)
		}
	}
	for _, want := range []string{"Course: English", "Title: Essay a", "Write it.", "--- Attachment: rubric.pdf ---\n600-800 words"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestParseBriefLooseShapes(t *testing.T) {
	e, err := parseBrief(`Sure! {"homework":true,"summary":" Read ch. 4 ","steps":"Read it","requirements":null,"estimate_min":"45"} hope that helps`)
	if err != nil {
		t.Fatal(err)
	}
	if e.Summary != "Read ch. 4" || len(e.Steps) != 1 || e.Requirements != nil || *e.EstimateMin != 45 {
		t.Fatalf("%+v", e)
	}
	for raw, want := range map[string]*int{`0`: nil, `-5`: nil, `99999999`: nil, `null`: nil, `"soon"`: nil} {
		if got := minutes(json.RawMessage(raw)); (got == nil) != (want == nil) {
			t.Errorf("minutes(%s) = %v", raw, got)
		}
	}
	long := make([]string, 20)
	for i := range long {
		long[i] = fmt.Sprintf("step %d", i)
	}
	b, _ := json.Marshal(map[string]any{"homework": true, "summary": "x", "steps": long})
	if e, _ := parseBrief(string(b)); len(e.Steps) != maxSteps {
		t.Errorf("steps not capped: %d", len(e.Steps))
	}
}

func TestClientRequestAndErrors(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil || req.Model != "m" || req.ResponseFormat["type"] != "json_object" || len(req.Messages) != 2 {
			t.Errorf("request = %s", body)
		}
		w.WriteHeader(int(status.Load()))
		if status.Load() == 200 {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"summary\":\"ok\"}"}}]}`)
		} else {
			fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/v1/", "k", "m", nil)
	c.backoff = 0
	out, err := c.Complete(context.Background(), "s", "u")
	if err != nil || out != `{"summary":"ok"}` {
		t.Fatalf("out %q err %v", out, err)
	}

	status.Store(429)
	hits.Store(0)
	if _, err := c.Complete(context.Background(), "s", "u"); !errors.Is(err, ErrStop) || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("429 err = %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("429 must not be retried, hits = %d", hits.Load())
	}

	status.Store(503)
	hits.Store(0)
	if _, err := c.Complete(context.Background(), "s", "u"); err == nil || errors.Is(err, ErrStop) {
		t.Fatalf("503 err = %v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("503 should be retried to 3 attempts, hits = %d", hits.Load())
	}
}

func TestParseBriefHomeworkFlag(t *testing.T) {
	e, err := parseBrief(`{"homework":false,"skip_reason":"optional Q&A forum","summary":"A help forum."}`)
	if err != nil || e.Homework == nil || *e.Homework || e.SkipReason != "optional Q&A forum" {
		t.Fatalf("%+v %v", e, err)
	}
	e, _ = parseBrief(goodAnswer)
	if e.Homework == nil || !*e.Homework || e.SkipReason != "" {
		t.Fatalf("homework answer = %+v", e)
	}
	if _, err := parseBrief(`{"summary":"no flag"}`); err == nil {
		t.Fatal("missing homework flag must be rejected so the retry asks again")
	}
}

func TestInboxContextAndByteBudget(t *testing.T) {
	posted := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	tk := task("a", model.StateNotStarted)
	tk.Kind, tk.AssignedAt = model.KindAnnouncement, &posted
	c := InboxContext(tk)
	if !strings.Contains(c, "Kind: announcement\nPosted: ") || !strings.Contains(c, "2026") || !strings.Contains(c, "\nTitle: Essay a\n") {
		t.Errorf("inbox context:\n%s", c)
	}
	if InboxContext(tk) != c {
		t.Error("inbox context must be stable")
	}

	big := strings.Repeat("é", 40<<10) // 80 KiB of 2-byte runes
	tk.Description = big
	tk.Attachments = []model.Attachment{{Name: "x", Content: big}, {Name: "y", Content: big}, {Name: "z", Content: big}, {Name: "w", Content: big}, {Name: "v", Content: big}}
	for name, s := range map[string]string{"task": TaskContext(tk), "inbox": InboxContext(tk)} {
		if len(s) > maxPromptBytes {
			t.Errorf("%s context is %d bytes, over the %d limit", name, len(s), maxPromptBytes)
		}
		if !utf8.ValidString(s) {
			t.Errorf("%s context cut mid-rune", name)
		}
	}
}
