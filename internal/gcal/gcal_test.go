package gcal

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"

	"schoolwork-check/internal/model"
)

// fakeEvents replays pages of events without touching Google.
type fakeEvents struct {
	pages []*calendar.Events
	err   error
	calls int
	minT  time.Time
	maxT  time.Time
}

func (f *fakeEvents) list(_ context.Context, _ string, min, max time.Time, token string) (*calendar.Events, error) {
	f.calls++
	f.minT, f.maxT = min, max
	if f.err != nil {
		return nil, f.err
	}
	i := 0
	if token != "" {
		i = int(token[len(token)-1] - '0')
	}
	if i >= len(f.pages) {
		return &calendar.Events{}, nil
	}
	return f.pages[i], nil
}

var now = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

func at(s string) *calendar.EventDateTime { return &calendar.EventDateTime{DateTime: s} }

func newTestClient(ev events) *Client {
	return newWithEvents(ev, Options{
		Feeds:      []Feed{{ID: "c_abc@group.calendar.google.com"}},
		PastDays:   30,
		FutureDays: 120,
		Location:   time.UTC,
		Now:        func() time.Time { return now },
	})
}

func TestRecurringMeetingGivesOneItemPerSitting(t *testing.T) {
	ev := &fakeEvents{pages: []*calendar.Events{{
		Summary: "Robotics Team",
		Items: []*calendar.Event{
			{Id: "s_1", RecurringEventId: "s", Summary: "Robotics meeting", Location: "Room 204",
				Start: at("2026-09-22T15:30:00Z"), End: at("2026-09-22T16:30:00Z"), HtmlLink: "https://cal/1"},
			{Id: "s_2", RecurringEventId: "s", Summary: "Robotics meeting", Location: "Room 204",
				Start: at("2026-09-29T15:30:00Z"), End: at("2026-09-29T16:30:00Z")},
			{Id: "s_3", RecurringEventId: "s", Summary: "Robotics meeting", Location: "Room 204",
				Start: at("2026-10-06T15:30:00Z"), End: at("2026-10-06T16:30:00Z")},
		},
	}}}
	tasks, err := newTestClient(ev).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// A calendar wants every sitting on the day it happens.
	if len(tasks) != 3 {
		t.Fatalf("got %d items, want one per occurrence: %+v", len(tasks), tasks)
	}
	got := tasks[0]
	if got.ID != "gcal:c_abc:s:20260922" {
		t.Errorf("id = %q, want the series id and the date", got.ID)
	}
	if tasks[1].ID == got.ID {
		t.Error("two occurrences share an id; they would overwrite each other")
	}
	if got.Source != model.SourceGCal || !got.Kind.Informational() {
		t.Errorf("source/kind = %q/%q, want gcal and an informational kind", got.Source, got.Kind)
	}
	if got.Course != "Robotics Team" {
		t.Errorf("course = %q, want the calendar's own title", got.Course)
	}
	if got.DueAt == nil || !got.DueAt.Equal(time.Date(2026, 9, 22, 15, 30, 0, 0, time.UTC)) {
		t.Errorf("due = %v, want this occurrence's start", got.DueAt)
	}
	if got.EndsAt == nil || !got.EndsAt.Equal(time.Date(2026, 9, 22, 16, 30, 0, 0, time.UTC)) {
		t.Errorf("ends = %v, want the occurrence's end", got.EndsAt)
	}
	for _, want := range []string{"Tue 22 Sep 2026 15:30–16:30", "Room 204"} {
		if !strings.Contains(got.Description, want) {
			t.Errorf("description is missing %q:\n%s", want, got.Description)
		}
	}
}

func TestOccurrencesInThePastAreStillReported(t *testing.T) {
	ev := &fakeEvents{pages: []*calendar.Events{{Items: []*calendar.Event{
		{Id: "s_0", RecurringEventId: "s", Summary: "Club", Start: at("2026-09-01T15:30:00Z"), End: at("2026-09-01T16:00:00Z")},
		{Id: "s_2", RecurringEventId: "s", Summary: "Club", Start: at("2026-09-22T15:30:00Z"), End: at("2026-09-22T16:00:00Z")},
	}}}}
	tasks, err := newTestClient(ev).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d items, want both sittings inside the window", len(tasks))
	}
	if !tasks[0].DueAt.Before(*tasks[1].DueAt) {
		t.Error("occurrences are not in date order")
	}
}

func TestCancelledAndDeclinedEventsAreSkipped(t *testing.T) {
	ev := &fakeEvents{pages: []*calendar.Events{{Items: []*calendar.Event{
		{Id: "a", Summary: "Cancelled assembly", Status: "cancelled", Start: at("2026-09-22T09:00:00Z")},
		{Id: "b", Summary: "Meeting I declined", Start: at("2026-09-23T09:00:00Z"),
			Attendees: []*calendar.EventAttendee{{Self: true, ResponseStatus: "declined"}}},
		{Id: "c", Summary: "Meeting I accepted", Start: at("2026-09-24T09:00:00Z"),
			Attendees: []*calendar.EventAttendee{{Self: true, ResponseStatus: "accepted"}}},
	}}}}
	tasks, err := newTestClient(ev).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "Meeting I accepted" {
		t.Fatalf("got %+v, want only the accepted meeting", titles(tasks))
	}
}

func TestAllDayEventSitsAtTheEndOfItsDay(t *testing.T) {
	ev := &fakeEvents{pages: []*calendar.Events{{Items: []*calendar.Event{
		{Id: "a", Summary: "Field trip", Start: &calendar.EventDateTime{Date: "2026-09-22"},
			End: &calendar.EventDateTime{Date: "2026-09-23"}},
	}}}}
	tasks, err := newTestClient(ev).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC); !tasks[0].DueAt.Equal(want) {
		t.Errorf("due = %v, want %v", tasks[0].DueAt, want)
	}
	if !strings.Contains(tasks[0].Description, "(all day)") {
		t.Errorf("description does not say it is all day:\n%s", tasks[0].Description)
	}
}

func TestLabelOverridesTheCalendarTitle(t *testing.T) {
	ev := &fakeEvents{pages: []*calendar.Events{{Summary: "c_abc", Items: []*calendar.Event{
		{Id: "a", Summary: "Staff meeting", Start: at("2026-09-22T09:00:00Z")},
	}}}}
	c := newTestClient(ev)
	c.opts.Feeds = []Feed{{ID: "c_abc@group.calendar.google.com", Label: "Meetings"}}
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if tasks[0].Course != "Meetings" {
		t.Errorf("course = %q, want the configured label", tasks[0].Course)
	}
	if !strings.Contains(tasks[0].Description, "From the Meetings calendar.") {
		t.Errorf("description does not name the calendar:\n%s", tasks[0].Description)
	}
}

func TestWindowFollowsPastAndFutureDays(t *testing.T) {
	ev := &fakeEvents{pages: []*calendar.Events{{}}}
	if _, err := newTestClient(ev).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := now.AddDate(0, 0, -30); !ev.minT.Equal(want) {
		t.Errorf("timeMin = %v, want %v", ev.minT, want)
	}
	if want := now.AddDate(0, 0, 120); !ev.maxT.Equal(want) {
		t.Errorf("timeMax = %v, want %v", ev.maxT, want)
	}
}

func TestPagination(t *testing.T) {
	ev := &fakeEvents{pages: []*calendar.Events{
		{NextPageToken: "page1", Items: []*calendar.Event{{Id: "a", Summary: "One", Start: at("2026-09-22T09:00:00Z")}}},
		{Items: []*calendar.Event{{Id: "b", Summary: "Two", Start: at("2026-09-23T09:00:00Z")}}},
	}}
	tasks, err := newTestClient(ev).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 2 || ev.calls != 2 {
		t.Errorf("got %d items in %d calls, want 2 and 2", len(tasks), ev.calls)
	}
}

func TestMissingScopeSaysWhatToRun(t *testing.T) {
	ev := &fakeEvents{err: &googleapi.Error{Code: 403, Message: "Request had insufficient authentication scopes."}}
	_, err := newTestClient(ev).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "google-login") {
		t.Fatalf("err = %v, want it to name the command that fixes it", err)
	}
}

func TestCalendarNotSharedSaysSo(t *testing.T) {
	ev := &fakeEvents{err: &googleapi.Error{Code: 404, Message: "Not Found"}}
	_, err := newTestClient(ev).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not shared") {
		t.Fatalf("err = %v, want it to say the calendar is not shared", err)
	}
}

func titles(ts []model.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return out
}

func TestApiNotEnabledIsNotMistakenForAMissingScope(t *testing.T) {
	ev := &fakeEvents{err: &googleapi.Error{
		Code: 403,
		Body: `{"error":{"status":"PERMISSION_DENIED","message":"Google Calendar API has not been used in project 123 before or it is disabled."}}`,
		Errors: []googleapi.ErrorItem{{Reason: "accessNotConfigured",
			Message: "Google Calendar API has not been used in project 123 before or it is disabled."}},
	}}
	_, err := newTestClient(ev).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not enabled in the") {
		t.Fatalf("err = %v, want it to say the API is not enabled", err)
	}
	if strings.Contains(err.Error(), "google-login") {
		t.Errorf("err sends the user to google-login, which would not help: %v", err)
	}
}
