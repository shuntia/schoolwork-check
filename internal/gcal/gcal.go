// Package gcal reads a Google Calendar the school shares — the one the
// meetings live on — and puts each upcoming event into the table as
// information, not as work. Events carry a time and a place, never a
// deliverable, so they go to note's agent inbox, which keeps the durable
// facts ("robotics meets Tuesdays at 15:30 in room 204") and drops the rest.
//
// A recurring meeting is one item, not seventeen: the occurrences inside the
// window collapse to the next one, with the repeat noted in its text.
package gcal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"schoolwork-check/internal/classroom"
	"schoolwork-check/internal/model"
)

// Feed is one calendar to read.
type Feed struct {
	// ID is the calendar id, e.g. "c_0123…@group.calendar.google.com".
	ID string
	// Label names the calendar in the table. Empty means the calendar's own
	// title as Google reports it. It is also what an event with no title of
	// its own is called — a calendar published as free/busy has every event
	// down as "Busy", and "Club Meeting" is more use than "Busy".
	Label string
	// ICSURL, when set, is a published iCalendar URL to read instead of
	// calling the API. It needs no login, no scope and no Cloud project.
	ICSURL string
}

// Options configures a Client.
type Options struct {
	Feeds []Feed
	// PastDays and FutureDays bound the window of events read.
	PastDays, FutureDays int
	// MaxEvents caps the events read per calendar (0 = 500).
	MaxEvents int
	Logger    *slog.Logger
	Location  *time.Location
	Now       func() time.Time
}

// events is the slice of the Calendar API this package uses. Tests fake it.
type events interface {
	list(ctx context.Context, calendarID string, min, max time.Time, pageToken string) (*calendar.Events, error)
}

// Client reads the configured calendars.
type Client struct {
	// api is nil when every feed is an ICS URL, which is the point: those
	// need no Google login, so none is demanded.
	api  events
	log  *slog.Logger
	opts Options
}

// New builds a client for the configured feeds. The Google login is only
// loaded when some feed actually needs the API: a run whose calendars are all
// published URLs must not fail for want of a token.
func New(ctx context.Context, credentialsFile, tokenFile string, opts Options) (*Client, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	needsAPI := false
	for _, f := range opts.Feeds {
		if f.ICSURL == "" {
			needsAPI = true
		}
	}
	if !needsAPI {
		return newWithEvents(nil, opts), nil
	}
	hc, err := classroom.HTTPClient(ctx, credentialsFile, tokenFile, log)
	if err != nil {
		return nil, err
	}
	svc, err := calendar.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("gcal: building Calendar client: %w", err)
	}
	return newWithEvents(&apiEvents{svc: svc}, opts), nil
}

func newWithEvents(ev events, opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 500
	}
	return &Client{api: ev, log: opts.Logger, opts: opts}
}

// apiEvents is the real Calendar API.
type apiEvents struct{ svc *calendar.Service }

func (a *apiEvents) list(ctx context.Context, calendarID string, min, max time.Time, pageToken string) (*calendar.Events, error) {
	call := a.svc.Events.List(calendarID).
		// Recurrences are expanded so that each occurrence has a real date;
		// they are collapsed again below, once, per series.
		SingleEvents(true).
		OrderBy("startTime").
		ShowDeleted(false).
		TimeMin(min.Format(time.RFC3339)).
		TimeMax(max.Format(time.RFC3339)).
		MaxResults(250).
		Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Do()
}

// Fetch reads every configured calendar. A calendar that fails is reported
// and the others still produce items.
func (c *Client) Fetch(ctx context.Context) ([]model.Task, error) {
	now := c.opts.Now()
	var (
		tasks []model.Task
		errs  []error
	)
	for _, f := range c.opts.Feeds {
		ts, err := c.fetchFeed(ctx, f, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("calendar %s: %w", f.ID, err))
			continue
		}
		tasks = append(tasks, ts...)
	}
	c.log.Info("gcal: calendars read", "calendars", len(c.opts.Feeds), "events", len(tasks), "failed", len(errs))
	return tasks, errors.Join(errs...)
}

// source is where one feed's events come from: its published URL, or the API.
func (c *Client) source(f Feed) (events, error) {
	if f.ICSURL != "" {
		return newICSEvents(f.ICSURL, nil, c.opts.Location), nil
	}
	if c.api == nil {
		return nil, errors.New("no Google login for a calendar read through the API")
	}
	return c.api, nil
}

func (c *Client) fetchFeed(ctx context.Context, f Feed, now time.Time) ([]model.Task, error) {
	min, max := now.AddDate(0, 0, -c.opts.PastDays), now.AddDate(0, 0, c.opts.FutureDays)
	ev, err := c.source(f)
	if err != nil {
		return nil, err
	}

	var (
		items    []*calendar.Event
		calName  string
		pageTok  string
		capped   bool
		pageSeen int
	)
	for {
		page, err := ev.list(ctx, f.ID, min, max, pageTok)
		if err != nil {
			return nil, scopeHint(err)
		}
		if calName == "" {
			calName = page.Summary
		}
		items = append(items, page.Items...)
		pageSeen++
		if len(items) >= c.opts.MaxEvents {
			items, capped = items[:c.opts.MaxEvents], true
			break
		}
		if page.NextPageToken == "" || pageSeen > 20 {
			break
		}
		pageTok = page.NextPageToken
	}
	if capped {
		c.log.Warn("gcal: stopped at the event limit", "calendar", f.ID, "limit", c.opts.MaxEvents)
	}

	label := f.Label
	if label == "" {
		label = calName
	}
	if label == "" {
		label = "Calendar"
	}
	return c.tasks(f, label, items, now), nil
}

// tasks converts events into informational items, one per occurrence.
//
// They are deliberately not collapsed into a series. A calendar wants every
// sitting of a standing appointment on the day it happens, and note's
// calendar takes them by external id, one row each, at no cost per row. The
// summarising this code used to do was for the agent inbox, which charges a
// model call per item; the calendar charges an HTTP PUT.
func (c *Client) tasks(f Feed, label string, items []*calendar.Event, now time.Time) []model.Task {
	out := make([]model.Task, 0, len(items))
	seen := map[string]bool{}
	for _, e := range items {
		if e == nil || e.Status == "cancelled" || declined(e) {
			continue
		}
		start, ok := eventTime(e.Start, c.opts.Location)
		if !ok {
			continue
		}
		id := "gcal:" + shortID(f.ID) + ":" + idPart(occurrenceKey(e, start))
		if seen[id] {
			continue
		}
		seen[id] = true

		title := eventTitle(e, label)
		task := model.Task{
			ID:          id,
			Source:      model.SourceGCal,
			Kind:        model.KindAnnouncement, // information, never work
			Course:      label,
			CourseID:    f.ID,
			Title:       clip(title, 200),
			URL:         e.HtmlLink,
			AssignedAt:  &start,
			DueAt:       &start,
			Description: c.describe(e, label),
			Progress:    model.Progress{State: model.StateNotStarted},
			FetchedAt:   now.UTC(),
		}
		if end, ok := eventTime(e.End, c.opts.Location); ok && end.After(start) {
			task.EndsAt = &end
		}
		out = append(out, task)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DueAt.Before(*out[j].DueAt) })
	return out
}

// occurrenceKey identifies one sitting. A recurring event's instances share a
// series id, so the date is what tells them apart; an event written out on its
// own is identified by its uid alone.
func occurrenceKey(e *calendar.Event, start time.Time) string {
	if e.RecurringEventId != "" {
		return e.RecurringEventId + ":" + start.Format("20060102")
	}
	if e.Id != "" {
		return e.Id
	}
	return strings.ToLower(strings.Join(strings.Fields(e.Summary), "-")) + ":" + start.Format("20060102")
}

// describe writes the one thing an event is: when, where, and what it says.
func (c *Client) describe(e *calendar.Event, label string) string {
	var b strings.Builder
	start, _ := eventTime(e.Start, c.opts.Location)
	b.WriteString(start.In(c.opts.Location).Format("Mon 2 Jan 2006"))
	if !allDay(e.Start) {
		fmt.Fprintf(&b, " %s", start.In(c.opts.Location).Format("15:04"))
		if end, ok := eventTime(e.End, c.opts.Location); ok {
			fmt.Fprintf(&b, "–%s", end.In(c.opts.Location).Format("15:04"))
		}
	} else {
		b.WriteString(" (all day)")
	}
	if loc := strings.TrimSpace(e.Location); loc != "" {
		fmt.Fprintf(&b, " · %s", clip(oneLine(loc), 200))
	}
	b.WriteString("\n")
	if d := strings.TrimSpace(e.Description); d != "" {
		b.WriteString(clip(oneLine(d), 2000) + "\n")
	}
	fmt.Fprintf(&b, "From the %s calendar.", label)
	return b.String()
}

// placeholderTitles are what a calendar published as free/busy calls every
// event: the real name is hidden, or there never was one.
var placeholderTitles = map[string]bool{
	"": true, "busy": true, "free": true, "private": true,
	"unavailable": true, "blocked": true, "(no title)": true, "untitled": true,
}

// eventTitle is the event's own name, or the calendar's label when the feed
// gives every event the same placeholder.
func eventTitle(e *calendar.Event, label string) string {
	title := strings.TrimSpace(e.Summary)
	if !placeholderTitles[strings.ToLower(title)] {
		return title
	}
	if label != "" {
		return label
	}
	if title == "" {
		return "(untitled event)"
	}
	return title
}

// declined reports whether the student said no to this meeting.
func declined(e *calendar.Event) bool {
	for _, a := range e.Attendees {
		if a != nil && a.Self && a.ResponseStatus == "declined" {
			return true
		}
	}
	return false
}

func allDay(t *calendar.EventDateTime) bool {
	return t != nil && t.DateTime == "" && t.Date != ""
}

// eventTime reads either shape of a Calendar timestamp: an instant, or an
// all-day date, which is placed at the end of its day like every other
// all-day item in the table.
func eventTime(t *calendar.EventDateTime, loc *time.Location) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	if t.DateTime != "" {
		parsed, err := time.Parse(time.RFC3339, t.DateTime)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	}
	if t.Date == "" {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", t.Date, loc)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 0, 0, loc), true
}

// idPart makes a grouping key safe to carry in a task id.
func idPart(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ':', r == '-', r == '_', r == '@', r == '.':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// shortID keeps ids readable: the calendar's local part is enough to tell
// two calendars apart.
func shortID(id string) string {
	if local, _, ok := strings.Cut(id, "@"); ok && local != "" {
		return local
	}
	return id
}

// scopeHint turns Google's three ways of saying no into the one thing that
// fixes each. They are easy to confuse: all three are a 403 on the same call.
func scopeHint(err error) error {
	var ae *googleapi.Error
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Code == 403 && apiNotEnabled(ae):
		return fmt.Errorf("%w — the Google Calendar API is not enabled in the "+
			"Cloud project this OAuth client belongs to: enable it in the console, "+
			"then try again", err)
	case ae.Code == 401 || ae.Code == 403:
		return fmt.Errorf("%w — the cached Google token has no calendar scope. "+
			"Run `schoolwork-check google-login` again *with a build that includes "+
			"this feature*: a login only ever grants the scopes the binary asks for, "+
			"so re-running an older build changes nothing", err)
	case ae.Code == 404:
		return fmt.Errorf("%w — the calendar is not shared with the account you ran google-login as", err)
	}
	return err
}

// apiNotEnabled distinguishes "this project has no Calendar API" from "this
// token has no calendar scope". Google words the first one several ways.
func apiNotEnabled(ae *googleapi.Error) bool {
	hay := strings.ToLower(ae.Message + " " + ae.Body)
	for _, e := range ae.Errors {
		hay += " " + strings.ToLower(e.Reason+" "+e.Message)
	}
	for _, needle := range []string{"accessnotconfigured", "service_disabled", "has not been used in project"} {
		if strings.Contains(hay, needle) {
			return true
		}
	}
	return false
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}
