// calendar.go pushes events into note's calendar rather than its task list.
//
// A meeting is not homework: it has a time and a place, nothing is handed in,
// and it belongs on note's dayline. note grew /api/calendar/by-external/{id}
// for exactly this, so an event is upserted by its own id and a second run
// refreshes the entry in place instead of making another one.
//
// Only calendar sources land here. Everything with a deliverable stays a task.

package note

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"schoolwork-check/internal/model"
)

// note's own limits, from server/src/calendar.rs.
const (
	calendarMaxTitle = 80
	// CalendarKind is what these entries are: time that is spoken for.
	CalendarKind = "busy"
)

// CalendarEntry is one row of note's calendar.
type CalendarEntry struct {
	ID         int64  `json:"id"`
	Quiet      bool   `json:"quiet,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	StartTime  string `json:"start_time"`
	EndTime    string `json:"end_time"`
	Days       int64  `json:"days"`
	OnDate     string `json:"on_date,omitempty"`
	FromDate   string `json:"from_date,omitempty"`
	UntilDate  string `json:"until_date,omitempty"`
}

// CalendarFields is the body both POST /api/calendar and the by-external PUT
// take. note rejects unknown fields, so this mirrors its Fields exactly.
type CalendarFields struct {
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Quiet     *bool  `json:"quiet,omitempty"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Days      *int64 `json:"days,omitempty"`
	OnDate    string `json:"on_date,omitempty"`
	FromDate  string `json:"from_date,omitempty"`
	UntilDate string `json:"until_date,omitempty"`
}

// calendarList is note's list response. The task routes answer with a bare
// array and the calendar wraps its rows in an object, so this decodes the
// wrapper and falls back to the array shape rather than assuming either.
type calendarList struct {
	Entries []CalendarEntry `json:"entries"`
}

func (l *calendarList) UnmarshalJSON(b []byte) error {
	type wrapper calendarList
	var w wrapper
	if err := json.Unmarshal(b, &w); err == nil && w.Entries != nil {
		l.Entries = w.Entries
		return nil
	}
	return json.Unmarshal(b, &l.Entries)
}

// Calendar lists every entry, ours and the user's. note caps a calendar at
// 100 entries, so there is no paging to do.
func (c *Client) Calendar(ctx context.Context) ([]CalendarEntry, error) {
	var out calendarList
	if err := c.do(ctx, http.MethodGet, "/api/calendar", nil, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// PutCalendar creates or refreshes the entry with this external id. note
// keeps the entry's skip dates across a refresh.
func (c *Client) PutCalendar(ctx context.Context, externalID string, f CalendarFields) (CalendarEntry, error) {
	var out CalendarEntry
	err := c.do(ctx, http.MethodPut, "/api/calendar/by-external/"+url.PathEscape(externalID), f, &out)
	return out, err
}

// DeleteCalendar removes the entry with this external id. An entry that is
// already gone is not an error: the end state is what was asked for.
func (c *Client) DeleteCalendar(ctx context.Context, externalID string) error {
	err := c.do(ctx, http.MethodDelete, "/api/calendar/by-external/"+url.PathEscape(externalID), nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// calendarFields renders an event as note wants it. It returns false when the
// event cannot be expressed as a calendar entry — without a date there is
// nothing to put on a day.
func calendarFields(t model.Task, o Options) (CalendarFields, bool) {
	loc := o.Location
	if loc == nil {
		loc = time.Local
	}
	if t.DueAt == nil {
		return CalendarFields{}, false
	}
	start := t.DueAt.In(loc)
	end := start.Add(time.Hour)
	if t.EndsAt != nil {
		if e := t.EndsAt.In(loc); e.After(start) {
			end = e
		}
	}
	// An entry lives on one day. An event running past midnight is clamped
	// rather than dropped: the day it starts is the day it matters on.
	if end.Day() != start.Day() || end.Sub(start) > 24*time.Hour {
		end = time.Date(start.Year(), start.Month(), start.Day(), 23, 59, 0, 0, loc)
	}
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = "Event"
	}
	// quiet is sent explicitly. note defaults a "busy" entry to quiet on
	// every write path, which would mean importing someone else's calendar
	// silently stops notifications for nineteen hours of the term. An
	// import should not change how the app behaves; a user who wants these
	// meetings to hold notifications asks for it.
	quiet := o.CalendarQuiet
	return CalendarFields{
		Title:     clipRunes(title, calendarMaxTitle),
		Kind:      CalendarKind,
		Quiet:     &quiet,
		StartTime: start.Format("15:04"),
		EndTime:   end.Format("15:04"),
		OnDate:    start.Format("2006-01-02"),
	}, true
}

// clipRunes cuts a title to at most n runes, which is what note counts.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// calendarHash is what a pushed entry looked like, so an unchanged event
// costs no call on the next run.
func calendarHash(f CalendarFields) string {
	quiet := "default"
	if f.Quiet != nil {
		quiet = fmt.Sprint(*f.Quiet)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		f.Title, f.Kind, quiet, f.StartTime, f.EndTime, f.OnDate, f.FromDate, f.UntilDate,
	}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func fmtCalendar(f CalendarFields) string {
	return fmt.Sprintf("%s %s-%s %s", f.OnDate, f.StartTime, f.EndTime, f.Title)
}

// calendarSink owns the events half of a sync: which tasks belong on note's
// calendar, what is already there, and what has to be withdrawn.
type calendarSink struct {
	// enabled is the configuration; on is whether note actually answered
	// the calendar route. Events are claimed on the first and only written
	// on the second — an unreachable calendar means they wait, never that
	// a term of meetings is redirected into the agent inbox one call each.
	enabled bool
	on      bool
	// existing is note's calendar keyed by external id, so an entry the
	// user deleted by hand can be told from one we have never pushed.
	existing map[string]CalendarEntry
	// blind is set when note will not list the calendar for us — today an
	// API token reaches the by-external routes but not GET /api/calendar.
	// Writing still works; what is lost is the ability to notice that the
	// user deleted one of our entries by hand, so in this mode we simply
	// never rewrite an entry whose event has not changed.
	blind bool
	// pushed counts entries written this run, against MaxCalendar.
	pushed int
	// live is every event id seen this run; anything remembered but not
	// live has left the calendar and is withdrawn at the end.
	live map[string]bool
	// full is set once note refuses a create for want of room. The rest of
	// the events wait for the next run rather than each earning their own
	// 409: the calendar holds 100 entries in all and the user's come first.
	full bool
}

// newCalendarSink reads note's calendar once, and only when there is an event
// to push: a run with no calendar source must not call the route at all.
func newCalendarSink(ctx context.Context, c *Client, tasks []model.Task, state *SyncState, o Options, log *slog.Logger) (*calendarSink, error) {
	s := &calendarSink{enabled: o.Calendar, live: map[string]bool{}}
	if !o.Calendar {
		return s, nil
	}
	var any bool
	for _, t := range tasks {
		if isEvent(t) {
			any = true
			break
		}
	}
	if !any && len(state.Calendar) == 0 {
		return s, nil
	}

	entries, err := c.Calendar(ctx)
	if err != nil {
		var apiErr *APIError
		unauthorized := errors.Is(err, ErrUnauthorized) ||
			(errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized)
		if unauthorized {
			// The token is good enough to write entries but not to read
			// the list. Push blind rather than refusing to push at all,
			// and say what is lost. Never fatal: the task half of this
			// sync authenticates fine on its own routes.
			log.Warn("note: the calendar list refused this token, pushing without it — "+
				"an entry you delete in note by hand will not be noticed", "err", err)
			s.on, s.blind = true, true
			return s, nil
		}
		// An older note without the calendar routes must not fail the run,
		// and must not send a term of meetings to the agent inbox instead.
		log.Warn("note: calendar unavailable, events are skipped this run", "err", err)
		return s, nil
	}
	s.on = true
	s.existing = make(map[string]CalendarEntry, len(entries))
	for _, e := range entries {
		if e.ExternalID != "" {
			s.existing[e.ExternalID] = e
		}
	}
	log.Debug("note: read the calendar", "entries", len(entries), "ours", len(s.existing))
	return s, nil
}

// isEvent reports whether a task belongs on the calendar rather than in the
// task list: something that happens at a time, with nothing to hand in.
func isEvent(t model.Task) bool {
	return t.Source == model.SourceGCal && t.DueAt != nil
}

// handles reports whether this sink takes the task instead of the task list.
// It answers for the configuration, not for whether note is reachable: an
// event belongs on a calendar either way.
func (s *calendarSink) handles(t model.Task) bool { return s.enabled && isEvent(t) }

// push writes one event into note's calendar. It reports whether the state
// file needs saving.
func (s *calendarSink) push(ctx context.Context, c *Client, t model.Task, state *SyncState, o Options, log *slog.Logger, r *Result) (bool, error) {
	if !s.on {
		// note has no calendar route this run. The event waits for the
		// next one rather than being rewritten as something it is not.
		r.CalendarDeferred++
		return false, nil
	}
	fields, ok := calendarFields(t, o)
	if !ok {
		return false, nil
	}
	s.live[t.ID] = true
	known, remembered := state.Calendar[t.ID]

	// An entry we pushed before that is no longer in note was deleted by
	// hand. That is an answer, not an error: it is never rewritten.
	if remembered && known.Declined {
		r.CalendarDeclined++
		return false, nil
	}
	if remembered && !s.blind {
		if _, still := s.existing[t.ID]; !still {
			r.CalendarDeclined++
			log.Info("note: calendar entry deleted in note, leaving it alone", "external_id", t.ID, "title", fields.Title)
			state.SetCalendar(t.ID, CalendarState{Hash: known.Hash, Declined: true, PushedAt: known.PushedAt})
			return true, nil
		}
	}

	hash := calendarHash(fields)
	if remembered && known.Hash == hash {
		if _, still := s.existing[t.ID]; still || s.blind {
			r.CalendarUnchanged++
			return false, nil
		}
	}
	if o.MaxCalendar > 0 && s.pushed >= o.MaxCalendar {
		r.CalendarDeferred++
		return false, nil
	}
	if o.DryRun {
		log.Info("note: would write calendar entry", "external_id", t.ID, "entry", fmtCalendar(fields))
		r.CalendarWritten++
		s.pushed++
		return false, nil
	}

	if s.full {
		r.CalendarDeferred++
		return false, nil
	}
	entry, err := c.PutCalendar(ctx, t.ID, fields)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			// note's calendar is full. That is the user's calendar being
			// busy, not a failure of ours.
			s.full = true
			r.CalendarDeferred++
			log.Warn("note: the calendar is full, the remaining events wait for room",
				"external_id", t.ID, "err", apiErr.Message)
			return false, nil
		}
		return false, err
	}
	s.pushed++
	r.CalendarWritten++
	log.Info("note: calendar entry written", "external_id", t.ID, "note_id", entry.ID, "entry", fmtCalendar(fields))
	state.SetCalendar(t.ID, CalendarState{Hash: hash, PushedAt: time.Now().UTC().Format(time.RFC3339)})
	return true, nil
}

// sweep withdraws entries for events that are no longer on the calendar —
// a cancelled meeting, or one that has fallen out of the date window.
func (s *calendarSink) sweep(ctx context.Context, c *Client, state *SyncState, o Options, log *slog.Logger, r *Result) bool {
	if !s.on || len(state.Calendar) == 0 {
		return false
	}
	var dirty bool
	for id, known := range state.Calendar {
		if s.live[id] || known.Declined {
			continue
		}
		if _, inNote := s.existing[id]; !inNote && !s.blind {
			// Already gone from note; just forget it.
			state.ForgetCalendar(id)
			dirty = true
			continue
		}
		if o.DryRun {
			log.Info("note: would withdraw calendar entry", "external_id", id)
			r.CalendarRemoved++
			continue
		}
		if err := c.DeleteCalendar(ctx, id); err != nil {
			r.Errors = append(r.Errors, fmt.Errorf("withdrawing %s: %w", id, err))
			continue
		}
		log.Info("note: calendar entry withdrawn, the event is gone", "external_id", id)
		state.ForgetCalendar(id)
		r.CalendarRemoved++
		dirty = true
	}
	return dirty
}
