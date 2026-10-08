package gcal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"schoolwork-check/internal/model"
)

// A free/busy feed: every event is called "Busy", and the calendar is one an
// app writes, so there are no recurrence rules — each session is its own
// event. This is the shape of a real published Google feed, folded lines and
// all.
const busyFeed = "BEGIN:VCALENDAR\r\n" +
	"PRODID:-//Google Inc//Google Calendar 70.9054//EN\r\n" +
	"VERSION:2.0\r\n" +
	"X-WR-CALNAME:club-calendar\r\n" +
	"X-WR-TIMEZONE:America/Los_Angeles\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART:20260919T170000Z\r\n" +
	"DTEND:20260919T180000Z\r\n" +
	"UID:auth0-66fd36e1-3330764\r\n" +
	"ATTENDEE;X-NUM-GUESTS=0:mailto:c_52db96@group.calendar.google.com\r\n" +
	"SUMMARY:Busy\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART:20260729T120000Z\r\n" +
	"DTEND:20260729T130000Z\r\n" +
	"UID:auth0-66fd36e1-3210238\r\n" +
	"SUMMARY:Busy\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func serve(t *testing.T, body string, code int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/basic.ics"
}

func icsClient(t *testing.T, url, label string) *Client {
	t.Helper()
	c := newWithEvents(nil, Options{
		Feeds:      []Feed{{ID: "c_52db96@group.calendar.google.com", Label: label, ICSURL: url}},
		PastDays:   30,
		FutureDays: 120,
		Location:   time.UTC,
		Now:        func() time.Time { return now },
	})
	return c
}

func TestFeedNeedsNoGoogleLogin(t *testing.T) {
	// api is nil: a published feed must work with no token at all.
	c := icsClient(t, serve(t, busyFeed, 200), "Club Meeting")
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d events, want 1 inside the window: %+v", len(tasks), tasks)
	}
	got := tasks[0]
	if got.Title != "Club Meeting" {
		t.Errorf("title = %q, want the label standing in for \"Busy\"", got.Title)
	}
	if got.Source != model.SourceGCal || !got.Kind.Informational() {
		t.Errorf("source/kind = %q/%q, want gcal and informational", got.Source, got.Kind)
	}
	if want := time.Date(2026, 9, 19, 17, 0, 0, 0, time.UTC); !got.DueAt.Equal(want) {
		t.Errorf("due = %v, want %v", got.DueAt, want)
	}
	if got.ID != "gcal:c_52db96:auth0-66fd36e1-3330764" {
		t.Errorf("id = %q, want it built from the event uid", got.ID)
	}
}

func TestFeedKeepsARealTitleWhenThereIsOne(t *testing.T) {
	feed := strings.Replace(busyFeed, "SUMMARY:Busy\r\nEND:VEVENT\r\nBEGIN:VEVENT",
		"SUMMARY:College essay review\r\nEND:VEVENT\r\nBEGIN:VEVENT", 1)
	c := icsClient(t, serve(t, feed, 200), "Club Meeting")
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if tasks[0].Title != "College essay review" {
		t.Errorf("title = %q, want the event's own name", tasks[0].Title)
	}
}

func TestFeedLabelNamesTheCalendar(t *testing.T) {
	c := icsClient(t, serve(t, busyFeed, 200), "")
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if tasks[0].Course != "club-calendar" {
		t.Errorf("course = %q, want X-WR-CALNAME when no label is configured", tasks[0].Course)
	}
	if tasks[0].Title != "(untitled event)" && tasks[0].Title != "Busy" {
		t.Logf("title without a label: %q", tasks[0].Title)
	}
}

func TestFeedHTTPFailuresExplainThemselves(t *testing.T) {
	for code, want := range map[int]string{404: "not published", 403: "stopped being public"} {
		c := icsClient(t, serve(t, "", code), "X")
		_, err := c.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("HTTP %d gave %v, want it to mention %q", code, err, want)
		}
	}
}

func TestParseICSHandlesEveryDateShape(t *testing.T) {
	feed := "BEGIN:VCALENDAR\n" +
		"X-WR-TIMEZONE:America/Los_Angeles\n" +
		"BEGIN:VEVENT\nUID:utc\nSUMMARY:UTC event\nDTSTART:20260922T170000Z\nDTEND:20260922T180000Z\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:zoned\nSUMMARY:Zoned event\nDTSTART;TZID=America/New_York:20260922T090000\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:allday\nSUMMARY:Field trip\nDTSTART;VALUE=DATE:20260923\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:folded\nSUMMARY:A title that was wrapped \n onto a second line\nDTSTART:20260924T170000Z\n" +
		"DESCRIPTION:Bring a pen\\, a notebook\\; and questions\nLOCATION:Room 204\nEND:VEVENT\n" +
		"END:VCALENDAR\n"
	out, err := parseICS(feed, now.AddDate(0, 0, -30), now.AddDate(0, 0, 120), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 4 {
		t.Fatalf("got %d events, want 4", len(out.Items))
	}
	byID := map[string]string{}
	for _, e := range out.Items {
		if e.Start.DateTime != "" {
			byID[e.Id] = e.Start.DateTime
		} else {
			byID[e.Id] = "date:" + e.Start.Date
		}
	}
	if got := byID["utc"]; got != "2026-09-22T17:00:00Z" {
		t.Errorf("utc start = %q", got)
	}
	if got := byID["zoned"]; !strings.HasPrefix(got, "2026-09-22T09:00:00-04:00") {
		t.Errorf("zoned start = %q, want it kept in its own offset", got)
	}
	if got := byID["allday"]; got != "date:2026-09-23" {
		t.Errorf("all-day start = %q", got)
	}
	for _, e := range out.Items {
		if e.Id != "folded" {
			continue
		}
		if e.Summary != "A title that was wrapped onto a second line" {
			t.Errorf("folded summary = %q", e.Summary)
		}
		if e.Description != "Bring a pen, a notebook; and questions" {
			t.Errorf("description = %q, want it unescaped", e.Description)
		}
		if e.Location != "Room 204" {
			t.Errorf("location = %q", e.Location)
		}
	}
}

func TestParseICSKeepsARecurrenceRuleVisible(t *testing.T) {
	feed := "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:r\nSUMMARY:Club\n" +
		"DTSTART:20260922T170000Z\nRRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=8\nEND:VEVENT\nEND:VCALENDAR\n"
	out, err := parseICS(feed, now.AddDate(0, 0, -30), now.AddDate(0, 0, 120), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("got %d events, want 1: a feed expands nothing", len(out.Items))
	}
	if !strings.Contains(out.Items[0].Description, "Repeats: FREQ=WEEKLY") {
		t.Errorf("the rule is not shown to the reader: %q", out.Items[0].Description)
	}
}

func TestParseICSDropsEventsOutsideTheWindow(t *testing.T) {
	out, err := parseICS(busyFeed, now.AddDate(0, 0, -30), now.AddDate(0, 0, 120), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Errorf("got %d events, want the July one dropped", len(out.Items))
	}
	if out.Summary != "club-calendar" {
		t.Errorf("calendar name = %q", out.Summary)
	}
}

func TestEverySessionComesThroughSeparately(t *testing.T) {
	// What the real feed looks like: many one-hour "Busy" blocks, each its
	// own event, no recurrence rule. Each is a thing in the week.
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nX-WR-CALNAME:club-calendar\r\n")
	for _, day := range []string{"20260912", "20260919", "20260926", "20261017", "20260830"} {
		b.WriteString("BEGIN:VEVENT\r\nUID:auth0-" + day + "\r\nSUMMARY:Busy\r\n" +
			"DTSTART:" + day + "T180000Z\r\nDTEND:" + day + "T190000Z\r\nEND:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")

	c := icsClient(t, serve(t, b.String(), 200), "Club Meeting")
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 5 {
		t.Fatalf("got %d items, want all five sessions: %v", len(tasks), titles(tasks))
	}
	for _, task := range tasks {
		if task.Title != "Club Meeting" {
			t.Errorf("title = %q, want the label standing in for \"Busy\"", task.Title)
		}
		if task.EndsAt == nil || !task.EndsAt.After(*task.DueAt) {
			t.Errorf("%s has no end time; note's calendar needs one", task.ID)
		}
	}
	// Sorted, earliest first, and each its own id.
	if !tasks[0].DueAt.Equal(time.Date(2026, 8, 30, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("first = %v, want the August session: a feed arrives unsorted", tasks[0].DueAt)
	}
	ids := map[string]bool{}
	for _, task := range tasks {
		if ids[task.ID] {
			t.Errorf("duplicate id %q", task.ID)
		}
		ids[task.ID] = true
	}
}

func TestDifferentMeetingsStayApart(t *testing.T) {
	feed := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VEVENT\r\nUID:a\r\nSUMMARY:Essay review\r\nDTSTART:20260919T180000Z\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:b\r\nSUMMARY:Interview prep\r\nDTSTART:20260920T180000Z\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:c\r\nSUMMARY:Essay review\r\nLOCATION:Room 2\r\nDTSTART:20260921T180000Z\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	c := icsClient(t, serve(t, feed, 200), "Club Meeting")
	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("got %v, want three separate meetings", titles(tasks))
	}
}
