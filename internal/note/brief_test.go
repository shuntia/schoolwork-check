package note

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"schoolwork-check/internal/model"
)

// agentOpts turns on agent briefs with the task description as context.
func agentOpts(t *testing.T) Options {
	o := opts(t)
	o.BriefContext = func(t model.Task) string { return "ctx: " + t.Description }
	return o
}

// briefAgent is a well-behaved agent that writes the context into the
// description and counts calls.
func briefAgent(f *fakeNote, calls *int) {
	f.agent = func(t *Task, c string) (int, string) {
		*calls++
		t.Description = "BRIEF " + c
		return 0, "briefed"
	}
}

func (f *fakeNote) agentCalls() int {
	n := 0
	for _, r := range f.requests() {
		if strings.HasSuffix(r.Path, "/agent") {
			n++
		}
	}
	return n
}

func TestBriefAfterCreateAndOnlyWhenTextChanges(t *testing.T) {
	f, c := startFake(t)
	calls := 0
	briefAgent(f, &calls)
	o := agentOpts(t)
	task := lms("canvas:assignment:1", "CS", "Lab", model.StateNotStarted)

	res := mustSync(t, c, []model.Task{task}, o)
	if res.Created != 1 || res.Briefed != 1 || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", res, calls)
	}
	got := f.find(1)
	if got.Description != "BRIEF ctx: do the thing" {
		t.Errorf("description = %q: sync must not overwrite the agent's description", got.Description)
	}
	if !strings.Contains(got.Notes, "do the thing") || !strings.Contains(got.Notes, "Course: CS") {
		t.Errorf("LMS text belongs in notes now:\n%s", got.Notes)
	}
	for i := 1; i <= f.count(http.MethodPatch); i++ {
		if _, sent := f.nth(http.MethodPatch, i).Body["description"]; sent {
			t.Errorf("PATCH %d sent description", i)
		}
	}

	// Unchanged text: no call, and the agent's description survives.
	res = mustSync(t, c, []model.Task{task}, o)
	if calls != 1 || res.Briefed != 0 || res.Updated != 0 {
		t.Fatalf("second run result = %+v, calls = %d", res, calls)
	}

	// Teacher edits the assignment: briefed again.
	task.Description = "do the thing twice"
	res = mustSync(t, c, []model.Task{task}, o)
	if calls != 2 || res.Briefed != 1 {
		t.Fatalf("third run result = %+v, calls = %d", res, calls)
	}
}

func TestBriefDroppedIsDeclinedNextRun(t *testing.T) {
	f, c := startFake(t)
	f.agent = func(t *Task, _ string) (int, string) {
		t.State = "dropped"
		t.Description = "Not homework: optional forum"
		return 0, "dropped"
	}
	o := agentOpts(t)
	forum := lms("canvas:discussion:2", "CS", "Q&A", model.StateNotStarted)

	if res := mustSync(t, c, []model.Task{forum}, o); res.BriefDropped != 1 {
		t.Fatalf("result = %+v", res)
	}
	res := mustSync(t, c, []model.Task{forum}, o)
	if res.SkippedDeclined != 1 || f.count(http.MethodPut) != 1 || f.count(http.MethodPost) != 1 { // 1 create + 1 agent
		t.Fatalf("result = %+v, puts = %d, posts = %d", res, f.count(http.MethodPut), f.count(http.MethodPost))
	}
}

func TestBriefFailureRetriesNextRun(t *testing.T) {
	f, c := startFake(t)
	fail := true
	calls := 0
	f.agent = func(t *Task, c string) (int, string) {
		calls++
		if fail {
			return http.StatusBadGateway, ""
		}
		return 0, "briefed"
	}
	o := agentOpts(t)
	task := lms("canvas:assignment:3", "CS", "Lab", model.StateNotStarted)

	res, err := Sync(context.Background(), c, []model.Task{task}, o)
	if err != nil || res.BriefFailed != 1 || res.Created != 1 || len(res.Errors) != 0 {
		t.Fatalf("a failed brief must not fail the sync: %+v, %v", res, err)
	}
	if calls != 1 {
		t.Errorf("502 must not be retried within the run, calls = %d", calls)
	}
	fail = false
	if res := mustSync(t, c, []model.Task{task}, o); res.Briefed != 1 || calls != 2 {
		t.Fatalf("next run result = %+v, calls = %d", res, calls)
	}
}

func TestBriefRouteMissingStopsForTheRun(t *testing.T) {
	f, c := startFake(t) // f.agent nil: route answers 404
	o := agentOpts(t)
	tasks := []model.Task{
		lms("canvas:assignment:4", "CS", "A", model.StateNotStarted),
		lms("canvas:assignment:5", "CS", "B", model.StateNotStarted),
	}
	res := mustSync(t, c, tasks, o)
	if res.Created != 2 || res.BriefFailed != 1 || f.agentCalls() != 1 {
		t.Fatalf("result = %+v, agent calls = %d", res, f.agentCalls())
	}
}

func TestBriefSkipsFinishedAndRespectsCapAndDryRun(t *testing.T) {
	f, c := startFake(t)
	calls := 0
	briefAgent(f, &calls)
	o := agentOpts(t)
	o.IncludeDone = true
	o.MaxBriefs = 1
	tasks := []model.Task{
		lms("canvas:assignment:6", "CS", "Done", model.StateGraded),
		lms("canvas:assignment:7", "CS", "Open 1", model.StateNotStarted),
		lms("canvas:assignment:8", "CS", "Open 2", model.StateNotStarted),
	}
	res := mustSync(t, c, tasks, o)
	if res.Created != 3 || res.Briefed != 1 || res.BriefDeferred != 1 || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", res, calls)
	}
	// Finished in note (user ticked it): never briefed.
	f.find(3).State = "done"
	o.DryRun = true
	o.MaxBriefs = 0
	res = mustSync(t, c, tasks, o)
	if res.Briefed != 0 || calls != 1 {
		t.Fatalf("dry run / done task result = %+v, calls = %d", res, calls)
	}
}

func TestBriefJSON404IsTaskGoneNotRouteMissing(t *testing.T) {
	f, c := startFake(t)
	calls := 0
	briefAgent(f, &calls)
	o := agentOpts(t)
	tasks := []model.Task{
		lms("canvas:assignment:11", "CS", "A", model.StateNotStarted),
		lms("canvas:assignment:12", "CS", "B", model.StateNotStarted),
	}
	mustSync(t, c, tasks, o) // both created and briefed

	// The teacher edits both, and the user hard-deletes A between the
	// listing and its brief call: the route answers with its JSON 404.
	tasks[0].Description, tasks[1].Description = "v2", "v2"
	f.agentGone = map[int64]bool{1: true}
	res := mustSync(t, c, tasks, o)
	if res.Briefed != 1 || res.BriefFailed != 0 || calls != 3 {
		t.Fatalf("a deleted task must not stop the other briefs: %+v, calls = %d", res, calls)
	}
	if f.agentCalls() != 4 {
		t.Errorf("agent requests = %d, want 4", f.agentCalls())
	}
}

func TestBrief429StopsForTheRunAndRetriesNext(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) { testBriefBusy(t, status) })
	}
}

func testBriefBusy(t *testing.T, status int) {
	f, c := startFake(t)
	limited := true
	calls := 0
	f.agent = func(tk *Task, _ string) (int, string) {
		calls++
		if limited {
			return status, ""
		}
		return 0, "briefed"
	}
	o := agentOpts(t)
	tasks := []model.Task{
		lms("canvas:assignment:13", "CS", "A", model.StateNotStarted),
		lms("canvas:assignment:14", "CS", "B", model.StateNotStarted),
	}
	res := mustSync(t, c, tasks, o)
	if res.Created != 2 || res.BriefFailed != 1 || calls != 1 {
		t.Fatalf("429 must stop briefs for the run: %+v, calls = %d", res, calls)
	}
	limited = false
	if res := mustSync(t, c, tasks, o); res.Briefed != 2 || calls != 3 {
		t.Fatalf("next run should brief both: %+v, calls = %d", res, calls)
	}
}
