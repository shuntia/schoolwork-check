// ics.go reads a calendar straight off its published iCalendar URL, with no
// Google login at all. It exists because a public .ics is the one way into a
// shared calendar that needs no OAuth scope, no consent and no Cloud project.
//
// Events are parsed into the same struct the API returns, so everything
// downstream — collapsing recurrences, describing, the date window — is the
// same code for both transports.
//
// One limit is worth stating plainly: a feed expands nothing, so an event
// carrying an RRULE arrives as its first occurrence with the rule quoted in
// its text, where the API would have handed back every occurrence. For feeds
// that write out individual events (most app-generated calendars do) there is
// no difference; for a calendar built on recurrence rules, use the API.

package gcal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"
)

// maxFeedBytes bounds the download. A year of events is tens of kilobytes.
const maxFeedBytes = 8 << 20

// icsEvents fetches and parses one published calendar.
type icsEvents struct {
	url  string
	http *http.Client
	loc  *time.Location
}

func newICSEvents(url string, hc *http.Client, loc *time.Location) *icsEvents {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &icsEvents{url: url, http: hc, loc: loc}
}

// list satisfies the same interface the API client does. A feed is one page
// and is filtered here rather than by the server.
func (s *icsEvents) list(ctx context.Context, _ string, min, max time.Time, pageToken string) (*calendar.Events, error) {
	if pageToken != "" {
		return &calendar.Events{}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, feedError(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the feed: %w", err)
	}
	return parseICS(string(body), min, max, s.loc)
}

// feedError explains the two ways a published URL stops working.
func feedError(code int) error {
	switch code {
	case http.StatusNotFound:
		return errors.New("the feed URL returns 404 — the calendar is not published, " +
			"or the address changed (Calendar settings > Integrate calendar)")
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the feed URL returns %d — a secret address was reset, or the calendar "+
			"stopped being public", code)
	default:
		return fmt.Errorf("the feed URL returns HTTP %d", code)
	}
}

// parseICS turns a VCALENDAR into events inside [min, max].
func parseICS(body string, min, max time.Time, fallback *time.Location) (*calendar.Events, error) {
	lines := unfold(body)
	out := &calendar.Events{}
	loc := fallback
	if loc == nil {
		loc = time.UTC
	}

	var (
		cur    *calendar.Event
		inside bool
		start  time.Time
		ok     bool
	)
	for _, line := range lines {
		name, params, value := property(line)
		switch {
		case name == "BEGIN" && value == "VEVENT":
			cur, inside = &calendar.Event{}, true
			continue
		case name == "END" && value == "VEVENT":
			if cur != nil && ok && !start.Before(min) && !start.After(max) {
				out.Items = append(out.Items, cur)
			}
			cur, inside, ok = nil, false, false
			continue
		case !inside:
			// Calendar-level properties.
			switch name {
			case "X-WR-CALNAME":
				out.Summary = unescape(value)
			case "X-WR-TIMEZONE":
				if l, err := time.LoadLocation(value); err == nil {
					loc = l
				}
			}
			continue
		case cur == nil:
			continue
		}

		switch name {
		case "UID":
			cur.Id = value
		case "SUMMARY":
			cur.Summary = unescape(value)
		case "LOCATION":
			cur.Location = unescape(value)
		case "DESCRIPTION":
			cur.Description = unescape(value)
		case "STATUS":
			cur.Status = strings.ToLower(value)
		case "URL":
			cur.HtmlLink = value
		case "RECURRENCE-ID":
			// An override of one occurrence of a series: keep them apart
			// rather than letting them collapse into each other.
			cur.RecurringEventId = ""
		case "RRULE":
			// Not expanded; say so where a reader will see it.
			cur.Description = strings.TrimSpace(cur.Description + "\nRepeats: " + unescape(value))
		case "DTSTART":
			var t time.Time
			var allDay bool
			t, allDay, ok = icsTime(value, params, loc)
			if ok {
				start = t
				cur.Start = eventDateTime(t, allDay, loc)
			}
		case "DTEND":
			if t, allDay, good := icsTime(value, params, loc); good {
				cur.End = eventDateTime(t, allDay, loc)
			}
		}
	}
	// A feed is in no particular order; downstream reads nicer sorted.
	sort.SliceStable(out.Items, func(i, j int) bool {
		a, _, _ := icsTime(startValue(out.Items[i]), nil, loc)
		b, _, _ := icsTime(startValue(out.Items[j]), nil, loc)
		return a.Before(b)
	})
	return out, nil
}

// startValue re-reads an event's start in the compact form icsTime parses.
func startValue(e *calendar.Event) string {
	if e.Start == nil {
		return ""
	}
	if e.Start.DateTime != "" {
		if t, err := time.Parse(time.RFC3339, e.Start.DateTime); err == nil {
			return t.UTC().Format("20060102T150405Z")
		}
	}
	return strings.ReplaceAll(e.Start.Date, "-", "")
}

// eventDateTime writes a time back in the shape the API would have used.
func eventDateTime(t time.Time, allDay bool, loc *time.Location) *calendar.EventDateTime {
	if allDay {
		return &calendar.EventDateTime{Date: t.In(loc).Format("2006-01-02")}
	}
	return &calendar.EventDateTime{DateTime: t.Format(time.RFC3339)}
}

// icsTime reads the three shapes a date-time comes in: UTC with a Z, a local
// time in a named zone, and a bare date for an all-day event.
func icsTime(value string, params map[string]string, loc *time.Location) (time.Time, bool, bool) {
	value = strings.TrimSpace(value)
	if params["VALUE"] == "DATE" || (len(value) == 8 && !strings.Contains(value, "T")) {
		t, err := time.ParseInLocation("20060102", value, loc)
		return t, true, err == nil
	}
	if strings.HasSuffix(value, "Z") {
		t, err := time.Parse("20060102T150405Z", value)
		return t, false, err == nil
	}
	in := loc
	if tz := params["TZID"]; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			in = l
		}
	}
	t, err := time.ParseInLocation("20060102T150405", value, in)
	return t, false, err == nil
}

// unfold joins continuation lines, which iCalendar wraps at 75 octets by
// starting the next line with a space or a tab.
func unfold(body string) []string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += line[1:]
			continue
		}
		out = append(out, line)
	}
	return out
}

// property splits "NAME;PARAM=V;PARAM=V:value".
func property(line string) (name string, params map[string]string, value string) {
	head, value, _ := strings.Cut(line, ":")
	parts := strings.Split(head, ";")
	name = strings.ToUpper(strings.TrimSpace(parts[0]))
	params = map[string]string{}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		params[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return name, params, value
}

// unescape undoes iCalendar's text escaping.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)
	return r.Replace(s)
}
