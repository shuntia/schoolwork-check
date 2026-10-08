package note

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fake note server -------------------------------------------------------

const testToken = "note_" + "0123456789012345678901234567890123456789012"

type recordedReq struct {
	Method string
	Path   string
	Body   map[string]any
	Auth   string
}

// fakeNote is enough of note's task routes to drive the sink: an in-memory
// task table, the same status codes, and a log of every request.
type fakeNote struct {
	mu     sync.Mutex
	nextID int64
	tasks  []*Task
	reqs   []recordedReq

	// failPatch, when set, can short-circuit a PATCH with (status, body).
	// call is 1 for the first PATCH of that id, 2 for the second, ...
	failPatch func(id int64, call int) (int, string)
	patchN    map[int64]int
	// failPut does the same for PUT /api/tasks/by-external/{ext}.
	failPut func(ext string, call int) (int, string)
	putN    map[string]int

	// agent answers POST /api/tasks/{id}/agent for a task that exists; nil
	// means the route is not deployed (404). It may mutate t. A non-zero
	// status is returned as-is with an empty body.
	agent func(t *Task, context string) (status int, outcome string)
	// agentGone makes the route answer its own JSON 404 for these ids even
	// though the listing still has them (deleted between list and brief).
	agentGone map[int64]bool

	// inbox answers POST /api/agent/inbox; nil means the route is not
	// deployed (empty-body 404). A non-zero status is returned with a JSON
	// error body.
	inbox func(sourceID, kind, context string) (status int, outcome string)

	// tombstones are external ids of tasks the user deleted; a by-external
	// PUT for one is a 410, as note answers.
	tombstones map[string]string
}

func newFakeNote() *fakeNote {
	return &fakeNote{nextID: 1, patchN: map[int64]int{}, putN: map[string]int{}, tombstones: map[string]string{}}
}

func (f *fakeNote) findExternal(ext string) *Task {
	for _, t := range f.tasks {
		if t.ExternalID == ext {
			return t
		}
	}
	return nil
}

// seed inserts a task directly, as if the user or an earlier run made it.
func (f *fakeNote) seed(t Task) *Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.ID == 0 {
		t.ID = f.nextID
	}
	if t.ID >= f.nextID {
		f.nextID = t.ID + 1
	}
	if t.State == "" {
		t.State = "open"
	}
	if t.Source == "" {
		t.Source = "manual"
	}
	f.tasks = append(f.tasks, &t)
	return &t
}

func (f *fakeNote) find(id int64) *Task {
	for _, t := range f.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (f *fakeNote) requests() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedReq(nil), f.reqs...)
}

func (f *fakeNote) count(method string) int {
	n := 0
	for _, r := range f.requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

// nth returns the nth request with the given method, for body assertions.
func (f *fakeNote) nth(method string, n int) recordedReq {
	i := 0
	for _, r := range f.requests() {
		if r.Method == method {
			i++
			if i == n {
				return r
			}
		}
	}
	return recordedReq{}
}

var patchAllowed = map[string]bool{
	"title": true, "description": true, "state": true, "notes": true,
	"duration_min": true, "parent_id": true, "is_now": true, "due_at": true, "url": true, "external_id": true,
}

// upsertAllowed is note's NewTask minus external_id, which the path carries.
var upsertAllowed = map[string]bool{
	"title": true, "description": true, "notes": true, "duration_min": true, "parent_id": true,
	"is_now": true, "state": true, "due_at": true, "url": true, "source": true, "notify": true,
}

func (f *fakeNote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		json.Unmarshal(raw, &body)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedReq{r.Method, r.URL.Path, body, r.Header.Get("Authorization")})
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized) // empty body, like note
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/tasks":
		var out []TaskNode
		for _, t := range f.tasks {
			if t.ParentID != nil || t.State == "dropped" {
				continue
			}
			node := TaskNode{Task: *t, Children: []Task{}}
			for _, c := range f.tasks {
				if c.ParentID != nil && *c.ParentID == t.ID && c.State != "dropped" {
					node.Children = append(node.Children, *c)
				}
			}
			out = append(out, node)
		}
		if out == nil {
			out = []TaskNode{}
		}
		writeJSON(w, 200, out)

	case r.Method == http.MethodPost && r.URL.Path == "/api/tasks":
		title, _ := body["title"].(string)
		if strings.TrimSpace(title) == "" {
			writeJSON(w, 422, map[string]string{"error": "title must not be empty"})
			return
		}
		t := &Task{ID: f.nextID, Title: title, State: "open", Source: "manual",
			UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
		f.nextID++
		f.tasks = append(f.tasks, t)
		writeJSON(w, 200, t) // note answers 200, not 201
		return

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/tasks/by-external/"):
		ext, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/tasks/by-external/"))
		f.putN[ext]++
		if f.failPut != nil {
			if status, msg := f.failPut(ext, f.putN[ext]); status != 0 {
				w.WriteHeader(status)
				io.WriteString(w, msg)
				return
			}
		}
		for k := range body {
			if !upsertAllowed[k] {
				writeJSON(w, 422, map[string]string{"error": "unknown field `" + k + "`"})
				return
			}
		}
		title, _ := body["title"].(string)
		if strings.TrimSpace(title) == "" {
			writeJSON(w, 422, map[string]string{"error": "title must not be empty"})
			return
		}
		if at, gone := f.tombstones[ext]; gone {
			writeJSON(w, 410, map[string]string{"error": "the user deleted this task; it is not recreated",
				"external_id": ext, "deleted_at": at})
			return
		}
		due, dueSent := body["due_at"]
		dueStr, _ := due.(string)
		urlStr, urlSent := body["url"].(string)
		notes, notesSent := body["notes"].(string)
		if t := f.findExternal(ext); t != nil {
			// The importer owns title, notes, due date and link; description
			// and steps stay; state only moves forward to done.
			t.Title = title
			if notesSent {
				t.Notes = notes
			}
			if dueSent {
				if t.ParentID != nil && due != nil {
					writeJSON(w, 422, map[string]string{"error": "a step carries no due date of its own"})
					return
				}
				t.DueAt = nil
				if due != nil {
					t.DueAt = &dueStr
				}
			}
			if urlSent {
				t.URL = urlStr
			}
			if st, _ := body["state"].(string); st == "done" && t.State != "dropped" {
				t.State = "done"
			}
			t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			writeJSON(w, 200, TaskNode{Task: *t, Children: []Task{}})
			return
		}
		t := &Task{ID: f.nextID, Title: title, State: "open", Source: "import", ExternalID: ext,
			Notes: notes, URL: urlStr, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
		if d, _ := body["description"].(string); d != "" {
			t.Description = d
		}
		if st, _ := body["state"].(string); st != "" {
			t.State = st
		}
		if due != nil {
			t.DueAt = &dueStr
		}
		f.nextID++
		f.tasks = append(f.tasks, t)
		writeJSON(w, 201, TaskNode{Task: *t, Children: []Task{}})
		return

	case r.Method == http.MethodPost && r.URL.Path == "/api/agent/inbox":
		if f.inbox == nil {
			w.WriteHeader(404)
			return
		}
		src, _ := body["source_id"].(string)
		kind, _ := body["kind"].(string)
		ctxText, _ := body["context"].(string)
		status, outcome := f.inbox(src, kind, ctxText)
		if status != 0 {
			writeJSON(w, status, map[string]string{"error": "inbox refused"})
			return
		}
		ids := []string{}
		if outcome == "remembered" {
			ids = []string{"semantic/" + src}
		}
		writeJSON(w, 200, map[string]any{"source_id": src, "outcome": outcome, "reason": "fake",
			"memory_ids": ids, "steps": []any{}})

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/tasks/") && strings.HasSuffix(r.URL.Path, "/agent"):
		if f.agent == nil {
			w.WriteHeader(404)
			return
		}
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/agent"), 10, 64)
		t := f.find(id)
		if t == nil || f.agentGone[id] {
			writeJSON(w, 404, map[string]string{"error": "task not found"}) // the route's own 404
			return
		}
		ctxText, _ := body["context"].(string)
		status, outcome := f.agent(t, ctxText)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, 200, map[string]any{"task_id": id, "outcome": outcome,
			"steps": []map[string]any{{"name": "task_update", "args": map[string]any{}, "result": "ok", "is_error": false}},
			"task":  TaskNode{Task: *t, Children: []Task{}}})

	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/tasks/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), 10, 64)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		f.patchN[id]++
		if f.failPatch != nil {
			if status, msg := f.failPatch(id, f.patchN[id]); status != 0 {
				w.WriteHeader(status)
				io.WriteString(w, msg)
				return
			}
		}
		t := f.find(id)
		if t == nil {
			w.WriteHeader(404) // empty body
			return
		}
		for k := range body {
			if !patchAllowed[k] {
				writeJSON(w, 422, map[string]string{"error": "unknown field `" + k + "`"})
				return
			}
		}
		if v, ok := body["title"].(string); ok {
			t.Title = v
		}
		if v, ok := body["description"].(string); ok {
			t.Description = v
		}
		if v, ok := body["due_at"]; ok {
			t.DueAt = nil
			if s, ok := v.(string); ok {
				t.DueAt = &s
			}
		}
		if v, ok := body["url"].(string); ok {
			t.URL = v
		}
		if v, ok := body["external_id"]; ok {
			ext, _ := v.(string)
			if held := f.findExternal(ext); ext != "" && held != nil && held.ID != t.ID {
				writeJSON(w, 409, map[string]string{"error": "external_id " + ext + " already belongs to task " + strconv.FormatInt(held.ID, 10)})
				return
			}
			t.ExternalID = ext
		}
		if v, ok := body["notes"].(string); ok {
			t.Notes = v
		}
		if v, ok := body["state"].(string); ok {
			t.State = v
		}
		t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		// Flattened task fields plus the extras a real PATCH may add.
		out := map[string]any{"id": t.ID, "title": t.Title, "description": t.Description,
			"state": t.State, "source": t.Source, "notes": t.Notes, "duration_min": t.DurationMin,
			"duration_source": t.DurationSource, "parent_id": t.ParentID, "is_now": t.IsNow,
			"updated_at": t.UpdatedAt, "demoted_from_now": nil}
		writeJSON(w, 200, out)

	default:
		w.WriteHeader(404)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// startFake returns a client wired to a fresh fake server.
func startFake(t *testing.T) (*fakeNote, *Client) {
	t.Helper()
	f := newFakeNote()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, testToken, srv.Client(), nil)
	c.backoff = time.Millisecond
	return f, c
}

// --- client tests -----------------------------------------------------------

// TestTaskNodeDecoding pins down that encoding/json promotes the embedded
// Task's fields, so note's flattened TaskNode decodes without a custom
// UnmarshalJSON.
func TestTaskNodeDecoding(t *testing.T) {
	const raw = `[{"id":7,"title":"Essay","description":"d","state":"in_progress",
	  "source":"manual","notes":"n","duration_min":45,"duration_source":"user",
	  "parent_id":null,"is_now":false,"updated_at":"2026-09-16T12:00:00Z",
	  "children":[{"id":8,"title":"Outline","description":"","state":"done","source":"manual",
	    "notes":"schoolwork-check-id: canvas:assignment:1","duration_min":null,"duration_source":"none",
	    "parent_id":7,"is_now":false,"updated_at":"2026-09-16T12:01:00Z"}]}]`

	var nodes []TaskNode
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes", len(nodes))
	}
	n := nodes[0]
	if n.ID != 7 || n.Title != "Essay" || n.State != "in_progress" || n.Notes != "n" {
		t.Errorf("flattened fields not promoted: %+v", n.Task)
	}
	if n.DurationMin == nil || *n.DurationMin != 45 || n.ParentID != nil || n.IsNow {
		t.Errorf("nullable fields: %+v", n.Task)
	}
	if len(n.Children) != 1 || n.Children[0].ID != 8 || n.Children[0].State != "done" {
		t.Fatalf("children: %+v", n.Children)
	}
	if n.Children[0].ParentID == nil || *n.Children[0].ParentID != 7 {
		t.Errorf("child parent_id: %+v", n.Children[0])
	}
	// And it round-trips back to the same flattened shape.
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	json.Unmarshal(out, &back)
	if _, ok := back["title"]; !ok {
		t.Errorf("marshal did not flatten: %s", out)
	}
	if _, ok := back["Task"]; ok {
		t.Errorf("marshal nested the embedded struct: %s", out)
	}
}

func TestClientUpsertSendsOnlyAllowedKeys(t *testing.T) {
	f, c := startFake(t)
	due := "2026-09-21T14:59:00Z"
	got, made, err := c.Upsert(context.Background(), "canvas:assignment:4",
		UpsertFields{Title: "Problem Set 4", Notes: "n", DueAt: &due, URL: "https://c/4"})
	if err != nil {
		t.Fatal(err)
	}
	if !made || got.ID == 0 || got.Title != "Problem Set 4" || got.ExternalID != "canvas:assignment:4" {
		t.Fatalf("created = %+v (made=%v)", got, made)
	}
	req := f.nth(http.MethodPut, 1)
	if req.Path != "/api/tasks/by-external/canvas:assignment:4" {
		t.Errorf("PUT path = %q", req.Path)
	}
	for k := range req.Body {
		if !upsertAllowed[k] {
			t.Errorf("PUT sent unexpected key %q (body %v)", k, req.Body)
		}
	}
	if _, sent := req.Body["external_id"]; sent {
		t.Error("PUT body carried external_id, which belongs to the path (422)")
	}
	for _, k := range []string{"description", "state"} {
		if _, sent := req.Body[k]; sent {
			t.Errorf("PUT sent empty %s, which would clear what note's agent wrote", k)
		}
	}
	if auth := req.Auth; auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", auth)
	}

	// The second PUT refreshes rather than creates.
	_, made, err = c.Upsert(context.Background(), "canvas:assignment:4", UpsertFields{Title: "PS4", Notes: "n2"})
	if err != nil || made {
		t.Fatalf("second upsert: made=%v err=%v", made, err)
	}
	if n := len(f.tasks); n != 1 || f.tasks[0].Title != "PS4" {
		t.Errorf("tasks after refresh = %+v", f.tasks)
	}
}

func TestClientUpsertDeletedIsDeclined(t *testing.T) {
	f, c := startFake(t)
	f.tombstones["canvas:assignment:9"] = "2026-09-20T00:00:00Z"
	_, _, err := c.Upsert(context.Background(), "canvas:assignment:9", UpsertFields{Title: "x"})
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("err = %v, want ErrDeclined", err)
	}
	if len(f.tasks) != 0 {
		t.Error("a deleted task was made again")
	}
}

func TestClientUnauthorized(t *testing.T) {
	f := newFakeNote()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := NewClient(srv.URL, "note_wrong", srv.Client(), nil)
	c.backoff = time.Millisecond

	if _, err := c.List(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("List err = %v, want ErrUnauthorized", err)
	}
	if _, _, err := c.Upsert(context.Background(), "x:1", UpsertFields{Title: "x"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Upsert err = %v, want ErrUnauthorized", err)
	}
	if n := len(f.tasks); n != 0 {
		t.Errorf("created %d tasks despite 401", n)
	}
}

func TestClientAPIErrorCarriesMessage(t *testing.T) {
	f, c := startFake(t)
	f.seed(Task{ID: 1, Title: "Essay"})

	// deny_unknown_fields: an unknown key is a 422 with a JSON message.
	_, err := c.Patch(context.Background(), 1, map[string]any{"deadline": "2026-09-19"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != 422 || !strings.Contains(apiErr.Message, "deadline") {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "422") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
}

func TestClientAPIErrorEmptyBody(t *testing.T) {
	_, c := startFake(t)
	// 404 on a missing id, empty body: no message to report.
	_, err := c.Patch(context.Background(), 999, map[string]any{"title": "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != 404 || apiErr.Message != "" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestClientRetriesServerErrors(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		attempt := n
		mu.Unlock()
		if attempt <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, 200, []TaskNode{{Task: Task{ID: 3, Title: "ok"}}})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, srv.Client(), nil)
	c.backoff = time.Millisecond

	nodes, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Title != "ok" {
		t.Fatalf("nodes = %+v", nodes)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 3 {
		t.Errorf("made %d requests, want exactly 3", n)
	}
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		writeJSON(w, 409, map[string]string{"error": "external_id x:1 already belongs to task 7"})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, testToken, srv.Client(), nil)
	c.backoff = time.Millisecond

	_, err := c.Patch(context.Background(), 1, map[string]any{"external_id": "x:1"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Message != "external_id x:1 already belongs to task 7" {
		t.Fatalf("err = %v", err)
	}
	if n != 1 {
		t.Errorf("made %d requests, want 1 (4xx is final)", n)
	}
}

func TestClientGivesUpAfterRetries(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, testToken, srv.Client(), nil)
	c.backoff = time.Millisecond

	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if n != 3 {
		t.Errorf("made %d requests, want 3", n)
	}
}

func TestClientRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, testToken, srv.Client(), nil)
	c.backoff = time.Hour // long enough that only cancellation ends the wait

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.List(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %v, should have bailed on the context", d)
	}
}

// --- state tests ------------------------------------------------------------

func TestStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deeper")
	path := filepath.Join(dir, "note-sync.json")

	empty, err := LoadState(path)
	if err != nil {
		t.Fatalf("missing file should load empty: %v", err)
	}
	if len(empty.Tasks) != 0 {
		t.Errorf("missing file gave %d entries", len(empty.Tasks))
	}

	s := SyncState{}
	s.Set("canvas:assignment:501", TaskState{NoteID: 42, CreatedAt: "2026-09-16T00:00:00Z",
		LastPushedState: "open", Hash: Hash("t", "d", "n", "open")})
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	back, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Version != 1 {
		t.Errorf("version = %d", back.Version)
	}
	got, ok := back.Get("canvas:assignment:501")
	if !ok {
		t.Fatalf("entry lost: %+v", back)
	}
	if got != s.Tasks["canvas:assignment:501"] {
		t.Errorf("got %+v, want %+v", got, s.Tasks["canvas:assignment:501"])
	}

	// The on-disk shape is the documented one.
	raw, _ := os.ReadFile(path)
	for _, want := range []string{`"version": 1`, `"note_id": 42`, `"last_pushed_state": "open"`, `"hash":`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state file missing %s:\n%s", want, raw)
		}
	}
}

func TestSaveStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note-sync.json")
	s := SyncState{}
	s.Set("a:1", TaskState{NoteID: 1})
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	s.Set("a:2", TaskState{NoteID: 2})
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "note-sync.json" {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}

func TestHashChangesWithEveryField(t *testing.T) {
	base := Hash("t", "d", "n", "open")
	for i, h := range []string{
		Hash("T", "d", "n", "open"),
		Hash("t", "D", "n", "open"),
		Hash("t", "d", "N", "open"),
		Hash("t", "d", "n", "done"),
	} {
		if h == base {
			t.Errorf("field %d does not affect the hash", i)
		}
	}
	if Hash("t", "d", "n", "open") != base {
		t.Error("hash is not stable")
	}
	if len(base) != 64 {
		t.Errorf("hash length %d, want 64 hex chars", len(base))
	}
}

func TestDefaultStateFileUsesXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	p, err := DefaultStateFile()
	if err != nil {
		t.Fatal(err)
	}
	if p != "/tmp/xdg-state/schoolwork-check/note-sync.json" {
		t.Errorf("path = %q", p)
	}

	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/testuser")
	p, err = DefaultStateFile()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/testuser", ".local", "state", "schoolwork-check", "note-sync.json"); p != want {
		t.Errorf("path = %q, want %q", p, want)
	}
}
