package note

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"schoolwork-check/internal/model"
)

// fakeCalendar is note's calendar half: GET /api/calendar and the
// by-external upsert and delete.
type fakeCalendar struct {
	mu      sync.Mutex
	entries map[string]CalendarEntry // by external id
	nextID  int64
	puts    int
	deletes int
	// down makes GET /api/calendar 404, as an older note would.
	down bool
	// tasksCalled counts anything that is not a calendar route, so a test
	// can prove events never reached the task list or the agent inbox.
	tasksCalled int
	// capAt mimics note's 100-entry limit, which answers 409 on create.
	capAt int
	// listUnauthorized mimics today's deployment: an API token reaches the
	// by-external routes but not GET /api/calendar.
	listUnauthorized bool
}

func (f *fakeCalendar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/calendar" && r.Method == http.MethodGet:
		if f.listUnauthorized {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		if f.down {
			writeJSON(w, 404, map[string]string{"error": "no such route"})
			return
		}
		out := make([]CalendarEntry, 0, len(f.entries))
		for _, e := range f.entries {
			out = append(out, e)
		}
		// note wraps calendar rows in an object, unlike the task routes.
		writeJSON(w, 200, map[string]any{"entries": out})

	case strings.HasPrefix(r.URL.Path, "/api/calendar/by-external/"):
		ext := strings.TrimPrefix(r.URL.Path, "/api/calendar/by-external/")
		if unescaped, err := decodePath(ext); err == nil {
			ext = unescaped
		}
		switch r.Method {
		case http.MethodPut:
			var f2 CalendarFields
			if err := json.NewDecoder(r.Body).Decode(&f2); err != nil {
				writeJSON(w, 400, map[string]string{"error": "bad json"})
				return
			}
			if len([]rune(f2.Title)) > calendarMaxTitle || f2.Title == "" {
				writeJSON(w, 422, map[string]string{"error": "title"})
				return
			}
			f.puts++
			e, existed := f.entries[ext]
			if !existed && f.capAt > 0 && len(f.entries) >= f.capAt {
				writeJSON(w, 409, map[string]string{"error": "a calendar holds at most 100 entries"})
				return
			}
			if !existed {
				f.nextID++
				e = CalendarEntry{ID: f.nextID, ExternalID: ext}
			}
			e.Title, e.Kind = f2.Title, f2.Kind
			e.Quiet = f2.Quiet != nil && *f2.Quiet
			e.StartTime, e.EndTime, e.OnDate = f2.StartTime, f2.EndTime, f2.OnDate
			f.entries[ext] = e
			if existed {
				writeJSON(w, 200, e)
			} else {
				writeJSON(w, 201, e)
			}
		case http.MethodDelete:
			if _, ok := f.entries[ext]; !ok {
				writeJSON(w, 404, map[string]string{"error": "unknown"})
				return
			}
			f.deletes++
			delete(f.entries, ext)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}

	default:
		f.tasksCalled++
		if r.URL.Path == "/api/tasks" && r.Method == http.MethodGet {
			writeJSON(w, 200, []TaskNode{})
			return
		}
		writeJSON(w, 200, map[string]any{"id": 1, "title": "x", "state": "open"})
	}
}

func decodePath(s string) (string, error) {
	u, err := http.NewRequest("GET", "http://x/"+s, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(u.URL.Path, "/"), nil
}

func startCalendar(t *testing.T) (*fakeCalendar, *Client) {
	t.Helper()
	f := &fakeCalendar{entries: map[string]CalendarEntry{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, testToken, srv.Client(), nil)
	c.backoff = time.Millisecond
	return f, c
}

func meeting(id string, day int, hour int) model.Task {
	start := time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	return model.Task{
		ID: "gcal:c_abc:" + id, Source: model.SourceGCal, Kind: model.KindAnnouncement,
		Course: "Club Meeting", Title: "Club Meeting",
		DueAt: &start, EndsAt: &end,
		Progress: model.Progress{State: model.StateNotStarted},
	}
}

func calOpts(t *testing.T) Options {
	o := opts(t)
	o.Calendar = true
	o.InboxContext = func(model.Task) string { return "context" }
	return o
}

func TestEventsGoToTheCalendarNotTheTaskList(t *testing.T) {
	f, c := startCalendar(t)
	o := calOpts(t)

	res := mustSync(t, c, []model.Task{meeting("a", 19, 11), meeting("b", 26, 11)}, o)
	if res.CalendarWritten != 2 {
		t.Fatalf("result = %+v, want 2 calendar entries", res)
	}
	if res.Created != 0 || res.InboxRemembered != 0 || res.InboxNothing != 0 {
		t.Errorf("result = %+v, want no tasks and no inbox calls", res)
	}
	got := f.entries["gcal:c_abc:a"]
	if got.Title != "Club Meeting" || got.Kind != "busy" {
		t.Errorf("entry = %+v, want a busy entry", got)
	}
	if got.OnDate != "2026-09-19" || got.StartTime != "11:00" || got.EndTime != "12:00" {
		t.Errorf("entry = %+v, want the event's own day and hours", got)
	}
}

func TestSecondPushWritesNothing(t *testing.T) {
	f, c := startCalendar(t)
	o := calOpts(t)
	tasks := []model.Task{meeting("a", 19, 11)}

	mustSync(t, c, tasks, o)
	res := mustSync(t, c, tasks, o)
	if res.CalendarUnchanged != 1 || res.CalendarWritten != 0 {
		t.Errorf("result = %+v, want the entry left alone", res)
	}
	if f.puts != 1 {
		t.Errorf("wrote %d times, want 1: an unchanged event costs no call", f.puts)
	}
}

func TestAMovedMeetingIsRewritten(t *testing.T) {
	f, c := startCalendar(t)
	o := calOpts(t)
	mustSync(t, c, []model.Task{meeting("a", 19, 11)}, o)

	res := mustSync(t, c, []model.Task{meeting("a", 19, 14)}, o) // moved to 14:00
	if res.CalendarWritten != 1 {
		t.Fatalf("result = %+v, want the entry refreshed", res)
	}
	if got := f.entries["gcal:c_abc:a"]; got.StartTime != "14:00" {
		t.Errorf("start = %q, want the new time", got.StartTime)
	}
	if len(f.entries) != 1 {
		t.Errorf("%d entries, want the one entry moved rather than duplicated", len(f.entries))
	}
}

func TestAnEntryDeletedInNoteIsNeverRecreated(t *testing.T) {
	f, c := startCalendar(t)
	o := calOpts(t)
	tasks := []model.Task{meeting("a", 19, 11)}
	mustSync(t, c, tasks, o)

	// The user deletes it by hand. note keeps no tombstone.
	f.mu.Lock()
	delete(f.entries, "gcal:c_abc:a")
	f.mu.Unlock()

	res := mustSync(t, c, tasks, o)
	if res.CalendarDeclined != 1 || res.CalendarWritten != 0 {
		t.Fatalf("result = %+v, want the deletion respected", res)
	}
	res = mustSync(t, c, tasks, o)
	if res.CalendarDeclined != 1 || res.CalendarWritten != 0 {
		t.Fatalf("result = %+v, want it still respected on later runs", res)
	}
	if len(f.entries) != 0 {
		t.Errorf("entries = %+v, want none recreated", f.entries)
	}
}

func TestACancelledMeetingIsWithdrawn(t *testing.T) {
	f, c := startCalendar(t)
	o := calOpts(t)
	mustSync(t, c, []model.Task{meeting("a", 19, 11), meeting("b", 26, 11)}, o)

	res := mustSync(t, c, []model.Task{meeting("a", 19, 11)}, o) // b cancelled upstream
	if res.CalendarRemoved != 1 {
		t.Fatalf("result = %+v, want the gone meeting withdrawn", res)
	}
	if _, still := f.entries["gcal:c_abc:b"]; still {
		t.Error("the cancelled meeting is still on the calendar")
	}
	if f.deletes != 1 {
		t.Errorf("%d deletes, want 1", f.deletes)
	}
}

func TestTheCapLeavesRoomForTheUsersOwnEntries(t *testing.T) {
	_, c := startCalendar(t)
	o := calOpts(t)
	o.MaxCalendar = 2

	res := mustSync(t, c, []model.Task{meeting("a", 19, 11), meeting("b", 20, 11), meeting("c", 21, 11)}, o)
	if res.CalendarWritten != 2 || res.CalendarDeferred != 1 {
		t.Errorf("result = %+v, want 2 written and 1 left for next time", res)
	}
}

func TestAnOlderNoteSkipsEventsRatherThanFloodingTheInbox(t *testing.T) {
	f, c := startCalendar(t)
	f.down = true
	o := calOpts(t)

	res := mustSync(t, c, []model.Task{meeting("a", 19, 11), meeting("b", 26, 11)}, o)
	if res.CalendarWritten != 0 {
		t.Errorf("result = %+v, want nothing written", res)
	}
	if res.InboxRemembered+res.InboxNothing+res.InboxTask+res.InboxFailed != 0 {
		t.Errorf("result = %+v, want no inbox calls: a term of meetings must not go there", res)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f, c := startCalendar(t)
	o := calOpts(t)
	o.DryRun = true

	res := mustSync(t, c, []model.Task{meeting("a", 19, 11)}, o)
	if res.CalendarWritten != 1 {
		t.Errorf("result = %+v, want the write reported", res)
	}
	if f.puts != 0 || len(f.entries) != 0 {
		t.Errorf("a dry run wrote %d entries", f.puts)
	}
}

func TestCalendarFields(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 9, 19, 11, 0, 0, 0, loc)

	t.Run("an event with no end gets an hour", func(t *testing.T) {
		task := model.Task{DueAt: &start, Title: "X"}
		f, ok := calendarFields(task, Options{Location: loc})
		if !ok || f.StartTime != "11:00" || f.EndTime != "12:00" {
			t.Errorf("fields = %+v, ok = %v", f, ok)
		}
	})

	t.Run("an undated item is not a calendar entry", func(t *testing.T) {
		if _, ok := calendarFields(model.Task{Title: "X"}, Options{Location: loc}); ok {
			t.Error("ok = true, want an undated item refused")
		}
	})

	t.Run("an event running past midnight is clamped to its own day", func(t *testing.T) {
		end := start.Add(20 * time.Hour)
		f, ok := calendarFields(model.Task{DueAt: &start, EndsAt: &end, Title: "X"}, Options{Location: loc})
		if !ok || f.OnDate != "2026-09-19" || f.EndTime != "23:59" {
			t.Errorf("fields = %+v", f)
		}
	})

	t.Run("a long title is cut to what note accepts", func(t *testing.T) {
		long := strings.Repeat("なが", 60) // 120 runes
		f, _ := calendarFields(model.Task{DueAt: &start, Title: long}, Options{Location: loc})
		if n := len([]rune(f.Title)); n > calendarMaxTitle {
			t.Errorf("title is %d runes, want at most %d", n, calendarMaxTitle)
		}
		if !strings.HasSuffix(f.Title, "…") {
			t.Errorf("title = %q, want it marked as cut", f.Title)
		}
	})
}

func TestAFullCalendarIsNotAFailure(t *testing.T) {
	f, c := startCalendar(t)
	f.capAt = 1
	o := calOpts(t)

	res := mustSync(t, c, []model.Task{meeting("a", 19, 11), meeting("b", 20, 11), meeting("c", 21, 11)}, o)
	if res.CalendarWritten != 1 {
		t.Errorf("result = %+v, want the one that fitted", res)
	}
	if res.CalendarDeferred != 2 || len(res.Errors) != 0 {
		t.Errorf("result = %+v, want the rest waiting and no errors", res)
	}
	if f.puts != 2 {
		t.Errorf("%d writes attempted, want 2: one success, one 409, then stop asking", f.puts)
	}
}

func TestPushesBlindWhenTheListRefusesTheToken(t *testing.T) {
	f, c := startCalendar(t)
	f.listUnauthorized = true
	o := calOpts(t)
	tasks := []model.Task{meeting("a", 19, 11)}

	// Writing still works, so the events still land.
	res := mustSync(t, c, tasks, o)
	if res.CalendarWritten != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v, want the entry written anyway", res)
	}
	// And an unchanged event still costs nothing on the next run.
	res = mustSync(t, c, tasks, o)
	if res.CalendarUnchanged != 1 || f.puts != 1 {
		t.Errorf("result = %+v, writes = %d, want the entry left alone", res, f.puts)
	}
	// Withdrawal still works: it needs no list, only the id.
	res = mustSync(t, c, nil, o)
	if res.CalendarRemoved != 1 || f.deletes != 1 {
		t.Errorf("result = %+v, deletes = %d, want the gone event withdrawn", res, f.deletes)
	}
}

func TestImportedEventsDoNotHoldNotificationsUnlessAsked(t *testing.T) {
	f, c := startCalendar(t)
	o := calOpts(t)
	tasks := []model.Task{meeting("a", 19, 11)}

	// note defaults a "busy" entry to quiet on every write path, so the
	// value is always sent rather than left to the server.
	mustSync(t, c, tasks, o)
	if got := f.entries["gcal:c_abc:a"]; got.Quiet {
		t.Error("an imported meeting holds notifications; an import must not change what the app does")
	}

	// Turning it on reaches note, which means the setting is part of what
	// counts as changed.
	o.CalendarQuiet = true
	res := mustSync(t, c, tasks, o)
	if res.CalendarWritten != 1 {
		t.Fatalf("result = %+v, want the entry rewritten with the new setting", res)
	}
	if got := f.entries["gcal:c_abc:a"]; !got.Quiet {
		t.Error("CalendarQuiet did not reach note")
	}
}

func TestCalendarListDecodesEitherShape(t *testing.T) {
	cases := map[string]string{
		"note's wrapper": `{"entries":[{"id":2,"external_id":"gcal:a","title":"Club Meeting","kind":"busy"}]}`,
		"a bare array":   `[{"id":2,"external_id":"gcal:a","title":"Club Meeting","kind":"busy"}]`,
		"empty wrapper":  `{"entries":[]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var l calendarList
			if err := json.Unmarshal([]byte(raw), &l); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if strings.Contains(name, "empty") {
				if len(l.Entries) != 0 {
					t.Errorf("got %d entries, want none", len(l.Entries))
				}
				return
			}
			if len(l.Entries) != 1 || l.Entries[0].ExternalID != "gcal:a" {
				t.Errorf("entries = %+v", l.Entries)
			}
		})
	}
}
