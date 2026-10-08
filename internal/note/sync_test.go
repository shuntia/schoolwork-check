package note

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"schoolwork-check/internal/model"
)

var dueFixture = time.Date(2026, 9, 21, 23, 59, 0, 0, time.FixedZone("JST", 9*3600))

// lms builds a minimal LMS row.
func lms(id, course, title string, state model.State) model.Task {
	return model.Task{
		ID: id, Source: model.SourceCanvas, Kind: model.KindAssignment,
		Course: course, Title: title, URL: "https://c/" + id,
		DueAt:       &dueFixture,
		Description: "do the thing",
		Progress:    model.Progress{State: state},
	}
}

// opts returns Options pointed at a state file inside a fresh temp dir.
func opts(t *testing.T) Options {
	t.Helper()
	return Options{
		StateFile:    filepath.Join(t.TempDir(), "note-sync.json"),
		CoursePrefix: true,
		Location:     time.UTC,
	}
}

func render(task model.Task, o Options) Rendered {
	return Render(task, RenderOptions{CoursePrefix: o.CoursePrefix, Location: o.Location})
}

func mustSync(t *testing.T, c *Client, tasks []model.Task, o Options) Result {
	t.Helper()
	res, err := Sync(context.Background(), c, tasks, o)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return res
}

// --- create path ------------------------------------------------------------

func TestSyncCreatesByExternalID(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateInProgress)

	res := mustSync(t, c, []model.Task{task}, o)
	if res.Created != 1 || res.Updated != 0 || res.Unchanged != 0 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}

	// One PUT keyed by our id does the whole job: no POST, no PATCH after.
	if f.count(http.MethodPut) != 1 || f.count(http.MethodPost) != 0 || f.count(http.MethodPatch) != 0 {
		t.Fatalf("calls = %d PUT, %d POST, %d PATCH", f.count(http.MethodPut), f.count(http.MethodPost), f.count(http.MethodPatch))
	}
	req := f.nth(http.MethodPut, 1)
	if req.Path != "/api/tasks/by-external/canvas:assignment:501" {
		t.Errorf("PUT path = %q", req.Path)
	}
	want := render(task, o)
	put := req.Body
	if put["title"] != want.Title || put["description"] != want.Description || put["notes"] != want.Notes {
		t.Errorf("PUT body = %v", put)
	}
	if put["state"] != "in_progress" {
		t.Errorf("PUT state = %v, want in_progress", put["state"])
	}
	if got, want := put["due_at"], task.DueAt.UTC().Format(time.RFC3339); got != want {
		t.Errorf("PUT due_at = %v, want %q", got, want)
	}
	if put["url"] != task.URL {
		t.Errorf("PUT url = %v, want %q", put["url"], task.URL)
	}
	for k := range put {
		if !upsertAllowed[k] {
			t.Errorf("PUT carried unknown key %q (would be a 422)", k)
		}
	}

	// The server holds what we rendered, keyed by our id, and the state
	// file remembers it.
	created := f.find(1)
	if created == nil || created.ExternalID != task.ID || created.Description != want.Description ||
		created.Notes != want.Notes || created.State != "in_progress" || created.Source != "import" {
		t.Errorf("server task = %+v", created)
	}
	st, err := LoadState(o.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := st.Get(task.ID)
	if !ok || entry.NoteID != 1 || entry.LastPushedState != "in_progress" || entry.Hash != hashRendered(want) {
		t.Errorf("state entry = %+v (ok=%v)", entry, ok)
	}
}

func TestSyncOmitsStateOnOpenCreate(t *testing.T) {
	f, c := startFake(t)
	mustSync(t, c, []model.Task{lms("canvas:assignment:1", "Bio", "Reading", model.StateNotStarted)}, opts(t))
	put := f.nth(http.MethodPut, 1).Body
	if _, ok := put["state"]; ok {
		t.Errorf("PUT sent state for an open task: %v", put)
	}
	if _, ok := put["description"]; !ok {
		t.Errorf("PUT missing description: %v", put)
	}
}

func TestSyncNeverSendsScheduleFields(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	mustSync(t, c, []model.Task{lms("canvas:assignment:1", "Bio", "Reading", model.StateInProgress)}, o)
	// A second run with a changed title makes it patch again.
	task := lms("canvas:assignment:1", "Bio", "Reading, revised", model.StateInProgress)
	mustSync(t, c, []model.Task{task}, o)

	for _, r := range f.requests() {
		for _, banned := range []string{"duration_min", "parent_id"} {
			if _, ok := r.Body[banned]; ok {
				t.Errorf("%s %s sent %s: %v", r.Method, r.Path, banned, r.Body)
			}
		}
		if v, ok := r.Body["is_now"]; ok && (r.Method != http.MethodPost || v != false) {
			t.Errorf("%s %s sent is_now=%v", r.Method, r.Path, v)
		}
	}
}

// --- matching ---------------------------------------------------------------

func TestSyncFindsExistingBySentinelInChild(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)
	want := render(task, o)

	parent := f.seed(Task{Title: "This week"})
	// A step never carries a due date of its own (the API answers 422), so
	// the match must hold without one.
	child := f.seed(Task{Title: want.Title, Description: want.Description, Notes: want.Notes,
		URL: want.URL, State: "open", ParentID: &parent.ID})

	res := mustSync(t, c, []model.Task{task}, o)
	if res.Created != 0 || res.Unchanged != 1 || res.Migrated != 1 {
		t.Fatalf("result = %+v (a task demoted to a step must still be matched)", res)
	}
	// The only write is the migration: the step gets its external id and
	// nothing else (a due date on a step is a 422).
	if f.count(http.MethodPut) != 0 || f.count(http.MethodPatch) != 1 {
		t.Fatalf("wrote to the server: %d PUT, %d PATCH", f.count(http.MethodPut), f.count(http.MethodPatch))
	}
	if body := f.nth(http.MethodPatch, 1).Body; len(body) != 1 || body["external_id"] != task.ID {
		t.Errorf("migration PATCH = %v, want only external_id", body)
	}
	if child.ExternalID != task.ID {
		t.Errorf("step external_id = %q after migration", child.ExternalID)
	}

	// The next run finds it by external id and is silent.
	res = mustSync(t, c, []model.Task{task}, o)
	if res.Unchanged != 1 || res.Migrated != 0 || f.count(http.MethodPatch) != 1 {
		t.Errorf("second run: %+v, patches = %d", res, f.count(http.MethodPatch))
	}
	// It is adopted into the state file even though we never created it.
	st, _ := LoadState(o.StateFile)
	if e, ok := st.Get(task.ID); !ok || e.NoteID != child.ID {
		t.Errorf("not adopted: %+v (ok=%v)", e, ok)
	}
}

func TestSyncMatchesSentinelWithUserTextBelow(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:7", "Bio", "Lab", model.StateNotStarted)
	want := render(task, o)
	f.seed(Task{Title: want.Title, Description: want.Description,
		Notes: want.Notes + "\nremember to ask about the deadline"})

	res := mustSync(t, c, []model.Task{task}, o)
	if res.Created != 0 {
		t.Fatalf("created a duplicate: %+v", res)
	}
	// notes is ours (it holds the sentinel), so the hand-written line is
	// rewritten rather than preserved.
	if res.Updated != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := f.nth(http.MethodPatch, 1).Body; got["notes"] != want.Notes {
		t.Errorf("PATCH notes = %v", got["notes"])
	}
}

// --- update path ------------------------------------------------------------

func TestSyncUnchangedAndChangedField(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)

	if res := mustSync(t, c, []model.Task{task}, o); res.Created != 1 {
		t.Fatalf("first run: %+v", res)
	}
	posts, patches := f.count(http.MethodPost), f.count(http.MethodPatch)

	// Same input, second run: nothing to say.
	res := mustSync(t, c, []model.Task{task}, o)
	if res.Unchanged != 1 || res.Updated != 0 || res.Created != 0 {
		t.Fatalf("second run = %+v", res)
	}
	if f.count(http.MethodPost) != posts || f.count(http.MethodPatch) != patches {
		t.Errorf("second run wrote to the server")
	}

	// Description moves; only that key is sent.
	task.Description = "do the thing, but better"
	res = mustSync(t, c, []model.Task{task}, o)
	if res.Updated != 1 || res.Created != 0 {
		t.Fatalf("third run = %+v", res)
	}
	body := f.nth(http.MethodPatch, patches+1).Body
	if len(body) != 1 {
		t.Fatalf("PATCH sent %v, want description only", body)
	}
	if got := body["description"]; got != render(task, o).Description {
		t.Errorf("PATCH description = %v", got)
	}
}

// TestSyncOverwritesManualEdits: the three fields we write are ours. What
// note holds right now is the comparison, not the stored hash, so a hand
// edit to description is restored on the next run.
func TestSyncOverwritesManualEdits(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)
	mustSync(t, c, []model.Task{task}, o)

	f.find(1).Description = "I typed over this"
	before := f.count(http.MethodPatch)

	res := mustSync(t, c, []model.Task{task}, o)
	if res.Updated != 1 {
		t.Fatalf("result = %+v, want the edit overwritten", res)
	}
	if got := f.nth(http.MethodPatch, before+1).Body["description"]; got != render(task, o).Description {
		t.Errorf("PATCH description = %v", got)
	}
}

func TestSyncStateIsOneWayForward(t *testing.T) {
	t.Run("done in note stays done", func(t *testing.T) {
		f, c := startFake(t)
		o := opts(t)
		task := lms("canvas:assignment:9", "Bio", "Lab", model.StateNotStarted) // renders "open"
		want := render(task, o)
		f.seed(Task{Title: want.Title, Description: want.Description, Notes: want.Notes, State: "done"})

		mustSync(t, c, []model.Task{task}, o)
		for _, r := range f.requests() {
			if _, ok := r.Body["state"]; ok {
				t.Errorf("sent state to a done task: %v", r.Body)
			}
		}
		if got := f.find(1).State; got != "done" {
			t.Errorf("state = %q, want done", got)
		}
	})

	t.Run("open in note moves to done", func(t *testing.T) {
		f, c := startFake(t)
		o := opts(t)
		task := lms("canvas:assignment:9", "Bio", "Lab", model.StateGraded) // renders "done"
		want := render(task, o)
		f.seed(Task{Title: want.Title, Description: want.Description, Notes: want.Notes, State: "open"})

		res := mustSync(t, c, []model.Task{task}, o)
		if res.Updated != 1 {
			t.Fatalf("result = %+v", res)
		}
		body := f.nth(http.MethodPatch, 1).Body
		if body["state"] != "done" {
			t.Errorf("PATCH = %v, want state done", body)
		}
		if got := f.find(1).State; got != "done" {
			t.Errorf("state = %q", got)
		}
	})

	t.Run("in_progress in note is not pushed back to open", func(t *testing.T) {
		f, c := startFake(t)
		o := opts(t)
		task := lms("canvas:assignment:9", "Bio", "Lab", model.StateMissing) // renders "open"
		want := render(task, o)
		f.seed(Task{Title: want.Title, Description: want.Description, Notes: want.Notes, State: "in_progress"})

		mustSync(t, c, []model.Task{task}, o)
		for _, r := range f.requests() {
			if _, ok := r.Body["state"]; ok {
				t.Errorf("sent state: %v", r.Body)
			}
		}
	})
}

// --- the user's decisions ---------------------------------------------------

func TestSyncDoesNotRecreateDeclinedTasks(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)

	// We made note task 99 last time; it is gone from the list (dropped or
	// deleted), which is the user saying no.
	seed := SyncState{}
	seed.Set(task.ID, TaskState{NoteID: 99, CreatedAt: "2026-09-15T00:00:00Z", LastPushedState: "open"})
	if err := SaveState(o.StateFile, seed); err != nil {
		t.Fatal(err)
	}

	res := mustSync(t, c, []model.Task{task}, o)
	if res.SkippedDeclined != 1 || res.Created != 0 {
		t.Fatalf("result = %+v", res)
	}
	if f.count(http.MethodPost) != 0 {
		t.Errorf("recreated a task the user declined")
	}
	// The memory must survive, or the next run recreates it.
	st, _ := LoadState(o.StateFile)
	if e, ok := st.Get(task.ID); !ok || e.NoteID != 99 {
		t.Errorf("state entry dropped: %+v (ok=%v)", e, ok)
	}
}

func TestSyncDroppedInNoteIsDeclined(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:2", "Bio", "Lab", model.StateNotStarted)

	// First run creates it, then the user drops it in note.
	mustSync(t, c, []model.Task{task}, o)
	f.find(1).State = "dropped"
	posts := f.count(http.MethodPost)

	res := mustSync(t, c, []model.Task{task}, o)
	if res.SkippedDeclined != 1 || res.Created != 0 {
		t.Fatalf("result = %+v", res)
	}
	if f.count(http.MethodPost) != posts {
		t.Errorf("recreated a dropped task")
	}
}

func TestSyncSkipsFinishedWorkUnlessAsked(t *testing.T) {
	task := lms("canvas:assignment:3", "Bio", "Old quiz", model.StateGraded)

	t.Run("default", func(t *testing.T) {
		f, c := startFake(t)
		o := opts(t)
		res := mustSync(t, c, []model.Task{task}, o)
		if res.SkippedDone != 1 || res.Created != 0 {
			t.Fatalf("result = %+v", res)
		}
		if f.count(http.MethodPost) != 0 {
			t.Errorf("created finished homework")
		}
		if _, err := os.Stat(o.StateFile); !os.IsNotExist(err) {
			t.Errorf("wrote state for a run that did nothing")
		}
	})

	t.Run("include-done", func(t *testing.T) {
		f, c := startFake(t)
		o := opts(t)
		o.IncludeDone = true
		res := mustSync(t, c, []model.Task{task}, o)
		if res.Created != 1 || res.SkippedDone != 0 {
			t.Fatalf("result = %+v", res)
		}
		if got := f.find(1).State; got != "done" {
			t.Errorf("state = %q, want done", got)
		}
	})
}

func TestSyncSkipsNonHomeworkOnCreateOnly(t *testing.T) {
	no, yes := false, true
	forum := lms("canvas:discussion:7", "CS", "Q&A board", model.StateNotStarted)
	forum.Enrichment = &model.Enrichment{Homework: &no, SkipReason: "optional Q&A forum", Summary: "A forum.", Model: "m"}
	unjudged := lms("canvas:assignment:8", "CS", "Lab", model.StateNotStarted)

	t.Run("default", func(t *testing.T) {
		f, c := startFake(t)
		res := mustSync(t, c, []model.Task{forum, unjudged}, opts(t))
		if res.SkippedNotHomework != 1 || res.Created != 1 || f.count(http.MethodPut) != 1 {
			t.Fatalf("result = %+v, puts = %d", res, f.count(http.MethodPut))
		}
	})

	t.Run("include-non-homework", func(t *testing.T) {
		_, c := startFake(t)
		o := opts(t)
		o.IncludeNonHomework = true
		if res := mustSync(t, c, []model.Task{forum}, o); res.Created != 1 || res.SkippedNotHomework != 0 {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("existing task keeps updating", func(t *testing.T) {
		f, c := startFake(t)
		o := opts(t)
		judgedYes := forum
		judgedYes.Enrichment = &model.Enrichment{Homework: &yes, Summary: "Post once.", Model: "m"}
		mustSync(t, c, []model.Task{judgedYes}, o)
		forum.Title = "Q&A board (renamed)"
		res := mustSync(t, c, []model.Task{forum}, o)
		if res.Updated != 1 || res.SkippedNotHomework != 0 {
			t.Fatalf("result = %+v", res)
		}
		if got := f.find(1).Title; !strings.Contains(got, "renamed") {
			t.Errorf("title = %q", got)
		}
	})
}

// --- failure handling -------------------------------------------------------

// TestSyncRepairsAfterPatchFailure is the crash-between-two-calls case the
// spec worries about: the create landed, the patch did not. The next run
// must finish the job rather than create a second task.
func TestSyncRetriesFailedCreateNextRun(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	// The server is down for the create (every retry included), then comes
	// back. Creation is one PUT keyed by our id, so a failure leaves nothing
	// behind to remember and the next run simply tries again.
	var down atomic.Bool
	down.Store(true)
	f.failPut = func(ext string, call int) (int, string) {
		if down.Load() {
			return 500, ""
		}
		return 0, ""
	}
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)

	res, err := Sync(context.Background(), c, []model.Task{task}, o)
	if err != nil {
		t.Fatalf("a per-task failure must not be fatal: %v", err)
	}
	if len(res.Errors) != 1 || res.Created != 0 {
		t.Fatalf("result = %+v errors = %v", res, res.Errors)
	}
	if !strings.Contains(res.Errors[0].Error(), task.ID) {
		t.Errorf("error does not name the task: %v", res.Errors[0])
	}
	if st, _ := LoadState(o.StateFile); len(st.Tasks) != 0 {
		t.Fatalf("state remembers a task that was never made: %+v", st.Tasks)
	}

	// Second run: creates it, once.
	down.Store(false)
	res = mustSync(t, c, []model.Task{task}, o)
	if res.Created != 1 || res.Updated != 0 || len(res.Errors) != 0 {
		t.Fatalf("second run = %+v errs=%v", res, res.Errors)
	}
	want := render(task, o)
	if got := f.find(1); got == nil || got.Description != want.Description || got.Notes != want.Notes || len(f.tasks) != 1 {
		t.Errorf("task = %+v (of %d)", got, len(f.tasks))
	}
}

func TestSyncPatchFailureKeepsTheAdoptedID(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)
	want := render(task, o)
	// A task an older run made: matched by sentinel, still to be stamped.
	f.seed(Task{Title: "old title", Description: want.Description, Notes: want.Notes})
	var down atomic.Bool
	down.Store(true)
	f.failPatch = func(id int64, call int) (int, string) {
		if down.Load() {
			return 500, ""
		}
		return 0, ""
	}

	res, err := Sync(context.Background(), c, []model.Task{task}, o)
	if err != nil || len(res.Errors) != 1 || f.count(http.MethodPut) != 0 {
		t.Fatalf("result = %+v err=%v puts=%d", res, err, f.count(http.MethodPut))
	}
	st, _ := LoadState(o.StateFile)
	if e, ok := st.Get(task.ID); !ok || e.NoteID != 1 || e.Hash != "" {
		t.Fatalf("state must remember the id with no hash: %+v (ok=%v)", e, ok)
	}

	// Second run: patches the existing task (title and the id stamp), no PUT.
	down.Store(false)
	res = mustSync(t, c, []model.Task{task}, o)
	if res.Created != 0 || res.Updated != 1 || res.Migrated != 1 || len(res.Errors) != 0 || f.count(http.MethodPut) != 0 {
		t.Fatalf("second run = %+v errs=%v puts=%d", res, res.Errors, f.count(http.MethodPut))
	}
	if got := f.find(1); got.Title != want.Title || got.ExternalID != task.ID || len(f.tasks) != 1 {
		t.Errorf("task not repaired: %+v (of %d)", got, len(f.tasks))
	}
}

func TestSyncMatchesByExternalIDWithoutSentinel(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)
	want := render(task, o)
	// The user edited the sentinel out of notes; note's own key still holds.
	f.seed(Task{Title: want.Title, Description: want.Description, Notes: "my own notes",
		ExternalID: task.ID, URL: want.URL, DueAt: dueAt(want)})

	res := mustSync(t, c, []model.Task{task}, o)
	if res.Created != 0 || res.Updated != 1 || res.Migrated != 0 || f.count(http.MethodPut) != 0 {
		t.Fatalf("result = %+v, puts = %d", res, f.count(http.MethodPut))
	}
	if body := f.nth(http.MethodPatch, 1).Body; len(body) != 1 || body["notes"] != want.Notes {
		t.Errorf("PATCH = %v, want only notes", body)
	}
}

func TestSyncDeletedInNoteIsNotRecreated(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	task := lms("canvas:assignment:501", "AP Calculus", "Problem Set 4", model.StateNotStarted)
	// Deleted in note by hand, with a fresh state file that knows nothing.
	f.tombstones[task.ID] = "2026-09-20T00:00:00Z"

	res := mustSync(t, c, []model.Task{task}, o)
	if res.SkippedDeclined != 1 || res.Created != 0 || len(res.Errors) != 0 || len(f.tasks) != 0 {
		t.Fatalf("result = %+v, tasks = %d", res, len(f.tasks))
	}
}

func TestSyncContinuesPastOneBadTask(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	f.failPut = func(ext string, call int) (int, string) {
		if ext == "canvas:assignment:1" {
			return 500, ""
		}
		return 0, ""
	}
	tasks := []model.Task{
		lms("canvas:assignment:1", "Bio", "First", model.StateNotStarted),
		lms("canvas:assignment:2", "Bio", "Second", model.StateNotStarted),
	}
	res, err := Sync(context.Background(), c, tasks, o)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Errors) != 1 || res.Created != 1 {
		t.Fatalf("result = %+v errs=%v", res, res.Errors)
	}
	if got := f.findExternal("canvas:assignment:2"); got == nil || got.Description == "" {
		t.Errorf("second task was not made: %+v", got)
	}
}

func TestSyncUnauthorizedIsFatal(t *testing.T) {
	f := newFakeNote()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := NewClient(srv.URL, "note_wrong", srv.Client(), nil)
	c.backoff = time.Millisecond

	o := opts(t)
	res, err := Sync(context.Background(), c, []model.Task{lms("canvas:assignment:1", "Bio", "Lab", model.StateNotStarted)}, o)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if res.Created != 0 || f.count(http.MethodPost) != 0 {
		t.Errorf("created something despite a bad token: %+v", res)
	}
	if _, err := os.Stat(o.StateFile); !os.IsNotExist(err) {
		t.Errorf("wrote a state file for a rejected token")
	}
}

func TestSyncListFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, testToken, srv.Client(), nil)
	c.backoff = time.Millisecond

	if _, err := Sync(context.Background(), c, []model.Task{lms("a:1", "Bio", "Lab", model.StateNotStarted)}, opts(t)); err == nil {
		t.Fatal("want a fatal error when the list call fails")
	}
}

// --- dry run ----------------------------------------------------------------

func TestSyncDryRunWritesNothing(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	o.DryRun = true

	// One task to create, one that exists and needs an update.
	newTask := lms("canvas:assignment:1", "Bio", "New lab", model.StateNotStarted)
	oldTask := lms("canvas:assignment:2", "Bio", "Old lab", model.StateNotStarted)
	stale := render(oldTask, o)
	f.seed(Task{Title: stale.Title, Description: "stale", Notes: stale.Notes})

	res := mustSync(t, c, []model.Task{newTask, oldTask}, o)
	if res.Created != 1 || res.Updated != 1 {
		t.Fatalf("result = %+v", res)
	}
	if f.count(http.MethodPost) != 0 || f.count(http.MethodPatch) != 0 {
		t.Errorf("dry run wrote: %d POST, %d PATCH", f.count(http.MethodPost), f.count(http.MethodPatch))
	}
	if f.count(http.MethodGet) != 1 {
		t.Errorf("GET count = %d, want 1", f.count(http.MethodGet))
	}
	if _, err := os.Stat(o.StateFile); !os.IsNotExist(err) {
		t.Errorf("dry run created %s", o.StateFile)
	}
}

// --- misc -------------------------------------------------------------------

func TestResultString(t *testing.T) {
	r := Result{Created: 2, Updated: 1, Unchanged: 3, SkippedDone: 4, SkippedNotHomework: 6, SkippedDeclined: 5,
		Errors: []error{errors.New("x")}}
	s := r.String()
	for _, want := range []string{"2 created", "1 updated", "3 unchanged", "4 skipped (done)", "6 skipped (not homework)", "5 skipped (declined)", "1 errors"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}

func TestExternalIDOf(t *testing.T) {
	cases := []struct {
		notes string
		want  string
		ok    bool
	}{
		{"body\n" + Sentinel + "canvas:assignment:1", "canvas:assignment:1", true},
		{Sentinel + "x:1", "x:1", true},
		{"body\n" + Sentinel + "x:1\nuser typed this", "x:1", true},
		{"body\n" + Sentinel + "x:1\r", "x:1", true},
		{"no sentinel here", "", false},
		{"", "", false},
		{Sentinel, "", false},
		{"  " + Sentinel + "x:1", "", false}, // must be at the start of the line
	}
	for _, tc := range cases {
		got, ok := externalIDOf(tc.notes)
		if got != tc.want || ok != tc.ok {
			t.Errorf("externalIDOf(%q) = %q,%v; want %q,%v", tc.notes, got, ok, tc.want, tc.ok)
		}
	}
}

func TestForwardState(t *testing.T) {
	cases := []struct {
		existing, rendered, want string
		ok                       bool
	}{
		{"open", "open", "", false},
		{"open", "in_progress", "in_progress", true},
		{"open", "done", "done", true},
		{"in_progress", "open", "", false},
		{"in_progress", "done", "done", true},
		{"done", "open", "", false},
		{"done", "done", "", false},
		{"dropped", "done", "", false},
	}
	for _, tc := range cases {
		got, ok := forwardState(tc.existing, tc.rendered)
		if got != tc.want || ok != tc.ok {
			t.Errorf("forwardState(%q,%q) = %q,%v; want %q,%v", tc.existing, tc.rendered, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSyncCorruptStateFileIsNotFatal(t *testing.T) {
	f, c := startFake(t)
	o := opts(t)
	if err := os.WriteFile(o.StateFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := mustSync(t, c, []model.Task{lms("canvas:assignment:1", "Bio", "Lab", model.StateNotStarted)}, o)
	if res.Created != 1 {
		t.Fatalf("result = %+v", res)
	}
	if f.count(http.MethodPut) != 1 {
		t.Errorf("PUT count = %d", f.count(http.MethodPut))
	}
}
