package note

import (
	"net/http"
	"strings"
	"testing"

	"schoolwork-check/internal/model"
)

func material(id, title, text string) model.Task {
	t := lms(id, "AP Latin", title, model.StateNotStarted)
	t.Kind, t.Description = model.KindMaterial, text
	return t
}

// inboxOpts turns on both agent paths, as BRIEF=note does.
func inboxOpts(t *testing.T) Options {
	o := agentOpts(t)
	o.InboxContext = func(t model.Task) string { return "inbox: " + t.Description }
	return o
}

func (f *fakeNote) inboxCalls() int {
	n := 0
	for _, r := range f.requests() {
		if r.Path == "/api/agent/inbox" {
			n++
		}
	}
	return n
}

func TestInboxRemembersInsteadOfCreatingATask(t *testing.T) {
	f, c := startFake(t)
	var sent []string
	f.inbox = func(src, kind, ctx string) (int, string) {
		sent = append(sent, src+"|"+kind+"|"+ctx)
		return 0, "remembered"
	}
	o := inboxOpts(t)
	cal := material("classroom:1:material:9", "Unit 2 Calendar", "quiz Sept 10")
	work := lms("classroom:1:5", "AP Latin", "Translate 6.16", model.StateNotStarted)

	res := mustSync(t, c, []model.Task{cal, work}, o)
	if res.InboxRemembered != 1 || res.Created != 1 || f.count(http.MethodPut) != 1 || f.count(http.MethodPost) != 2 { // inbox + brief
		t.Fatalf("result = %+v, puts = %d, posts = %d", res, f.count(http.MethodPut), f.count(http.MethodPost))
	}
	if len(sent) != 1 || sent[0] != "classroom:1:material:9|material|inbox: quiz Sept 10" {
		t.Errorf("inbox body = %v", sent)
	}
	for _, tk := range f.tasks {
		if strings.Contains(tk.Title, "Calendar") {
			t.Error("an informational item became a task")
		}
	}

	// Unchanged: no call. Changed text: sent again (note supersedes).
	if res := mustSync(t, c, []model.Task{cal, work}, o); res.InboxUnchanged != 1 || f.inboxCalls() != 1 {
		t.Fatalf("second run = %+v, inbox calls = %d", res, f.inboxCalls())
	}
	cal.Description = "quiz moved to Sept 12"
	if res := mustSync(t, c, []model.Task{cal, work}, o); res.InboxRemembered != 1 || f.inboxCalls() != 2 {
		t.Fatalf("third run = %+v, inbox calls = %d", res, f.inboxCalls())
	}
}

func TestInboxTaskOutcomeCreatesAndBriefsOnce(t *testing.T) {
	f, c := startFake(t)
	f.inbox = func(string, string, string) (int, string) { return 0, "task" }
	calls := 0
	briefAgent(f, &calls)
	o := inboxOpts(t)
	guide := material("classroom:1:material:7", "Required Vocab", "know every word")

	res := mustSync(t, c, []model.Task{guide}, o)
	if res.InboxTask != 1 || res.Created != 1 || res.Briefed != 1 {
		t.Fatalf("result = %+v", res)
	}
	// From now on it is a task: no more inbox calls, normal task sync.
	guide.Description = "know every word, quiz Friday"
	res = mustSync(t, c, []model.Task{guide}, o)
	if f.inboxCalls() != 1 || res.Created != 0 || res.Updated != 1 || res.Briefed != 1 {
		t.Fatalf("second run = %+v, inbox calls = %d", res, f.inboxCalls())
	}
}

func TestInboxRouteMissingCreatesNothingAndRetries(t *testing.T) {
	f, c := startFake(t) // f.inbox nil: empty-body 404
	o := inboxOpts(t)
	items := []model.Task{
		material("classroom:1:material:1", "Calendar", "a"),
		material("classroom:1:material:2", "Rubric", "b"),
	}
	res := mustSync(t, c, items, o)
	if res.InboxFailed != 1 || res.InboxDeferred != 1 || res.Created != 0 || f.inboxCalls() != 1 {
		t.Fatalf("result = %+v, inbox calls = %d", res, f.inboxCalls())
	}
	f.inbox = func(string, string, string) (int, string) { return 0, "nothing" }
	if res := mustSync(t, c, items, o); res.InboxNothing != 2 {
		t.Fatalf("next run = %+v", res)
	}
}

func TestInboxBusyStopsAllAgentCalls(t *testing.T) {
	f, c := startFake(t)
	f.inbox = func(string, string, string) (int, string) { return http.StatusTooManyRequests, "" }
	calls := 0
	briefAgent(f, &calls)
	o := inboxOpts(t)
	items := []model.Task{
		material("classroom:1:material:1", "Calendar", "a"),
		lms("classroom:1:5", "AP Latin", "Translate", model.StateNotStarted),
	}
	res := mustSync(t, c, items, o)
	if res.InboxFailed != 1 || res.Created != 1 || calls != 0 {
		t.Fatalf("429 on the inbox must also stop briefs: %+v, brief calls = %d", res, calls)
	}
}

func TestInboxRejectedIsNotResentUntilChanged(t *testing.T) {
	f, c := startFake(t)
	f.inbox = func(string, string, string) (int, string) { return http.StatusUnprocessableEntity, "" }
	o := inboxOpts(t)
	item := material("classroom:1:material:1", "Huge", "x")
	mustSync(t, c, []model.Task{item}, o)
	if res := mustSync(t, c, []model.Task{item}, o); res.InboxUnchanged != 1 || f.inboxCalls() != 1 {
		t.Fatalf("422 must not be resent: %+v, calls = %d", res, f.inboxCalls())
	}
}

func TestInformationalItemsAlreadyTasksStayTasks(t *testing.T) {
	f, c := startFake(t)
	f.inbox = func(string, string, string) (int, string) { return 0, "remembered" }
	item := material("classroom:1:material:1", "Study Guide", "read")

	// Created as a task before the inbox existed (inbox off).
	mustSync(t, c, []model.Task{item}, agentOpts(t))
	o := inboxOpts(t)
	o.StateFile = t.TempDir() + "/state.json" // fresh state: identity comes from the sentinel alone
	item.Description = "read chapters 1-2"
	res := mustSync(t, c, []model.Task{item}, o)
	if f.inboxCalls() != 0 || res.Updated != 1 {
		t.Fatalf("existing task was rerouted: %+v, inbox calls = %d", res, f.inboxCalls())
	}
}

func TestInboxOffKeepsMaterialsAsTasks(t *testing.T) {
	f, c := startFake(t)
	res := mustSync(t, c, []model.Task{material("classroom:1:material:1", "Calendar", "a")}, opts(t))
	if res.Created != 1 || f.inboxCalls() != 0 {
		t.Fatalf("result = %+v", res)
	}
}
