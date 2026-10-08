// Package config loads runtime settings from environment variables
// (optionally via a .env file loaded by main).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Canvas
	CanvasBaseURL string // e.g. https://school.instructure.com (no trailing slash)
	CanvasToken   string

	// Google Classroom (installed-app OAuth flow, token cached on disk)
	GoogleCredentialsFile string // OAuth client JSON downloaded from Cloud Console
	GoogleTokenFile       string // where the refresh token is cached

	// CalendarDocs are course calendars that live in a Google Doc, Sheet or
	// uploaded file instead of an LMS (CALENDAR_DOCS). They use the same
	// Google login as Classroom.
	CalendarDocs []CalendarDoc
	// CalendarInterval is how long a calendar's parse stays good after the
	// teacher edits it (CALENDAR_INTERVAL). Zero re-reads on every edit.
	CalendarInterval time.Duration
	// CalendarDedup asks the model whether a calendar row is work that
	// Canvas or Classroom already lists under a different name.
	CalendarDedup bool
	// CalendarMaterials reads LMS materials whose title says "calendar"
	// into dated rows, instead of passing them on as a wall of text.
	CalendarMaterials bool
	// CalendarModel is the model that reads calendars. Calendars stream
	// back far more output than a brief does, so it is worth being able to
	// point them at a faster model than LLM_MODEL without changing what
	// writes the briefs. Empty means LLMModel.
	CalendarModel string
	// CalendarMaxText caps the text read out of one calendar document. It
	// is its own setting because a whole semester of rows runs well past
	// MAX_EXTRACTED_TEXT, which exists to bound a brief's input.
	CalendarMaxText int

	// GoogleCalendars are shared Google Calendars to read meetings and
	// events from (GOOGLE_CALENDARS). Same Google login; needs the calendar
	// scope, so a token minted earlier has to be renewed once.
	GoogleCalendars []CalendarFeed

	// note (downstream sink, ~/Projects/note)
	NoteBaseURL string // e.g. http://127.0.0.1:3271
	NoteToken   string // bearer token minted in note's Settings

	// Brief selects who writes the per-task brief: "local" (internal/enrich
	// calls the LLM here), "note" (push hands each task to note's agent
	// route, which owns the description), or "off".
	Brief     string
	MaxBriefs int // note agent calls per push; the rest wait for the next run
	// NoteCalendar sends events to note's calendar instead of its agent
	// inbox; MaxCalendarEntries caps how many of note's 100 entries this
	// tool may occupy.
	NoteCalendar       bool
	MaxCalendarEntries int
	// NoteCalendarQuiet asks note to hold notifications during imported
	// events. Off by default.
	NoteCalendarQuiet bool

	// LLM enrichment (internal/enrich), any OpenAI-compatible endpoint
	Enrich       bool   // off with ENRICH=false even when a key is present
	LLMBaseURL   string // e.g. https://openrouter.ai/api/v1
	LLMModel     string
	LLMAPIKey    string // LLM_API_KEY, or the contents of LLM_API_KEY_FILE
	LLMMaxCalls  int    // model calls per run; cached tasks cost none
	LLMCacheFile string // "" = default under the state directory

	// Behaviour
	Sources            []string // subset of {"canvas","classroom","gdoc"}; empty = all configured
	ExtractAttachments bool
	FileCacheFile      string // attachment text cache; "" = default under the state directory
	MaxAttachmentBytes int64  // skip downloading files larger than this
	MaxExtractedText   int    // cap on extracted text per attachment
	PastDays           int    // include tasks due up to this many days ago
	FutureDays         int    // include tasks due up to this many days ahead
}

// CalendarDoc is one document to read a course calendar out of.
type CalendarDoc struct {
	// ID is the Drive file id, extracted from the URL when one was given.
	ID string
	// Course is the course name put on every task from this document.
	// Empty means the document's own name in Drive.
	Course string
	// Interval overrides CalendarInterval for this document.
	Interval time.Duration
}

// CalendarFeed is one Google Calendar to read events from.
type CalendarFeed struct {
	// ID is the calendar id, e.g. "c_0123…@group.calendar.google.com".
	ID string
	// Label names it in the table, and names events the feed gives no title
	// of their own; empty means the calendar's own title.
	Label string
	// ICSURL is set when the entry was a published iCalendar URL, which is
	// read directly instead of through the API — no login, no scope.
	ICSURL string
}

func Load() (Config, error) {
	home, _ := os.UserHomeDir()
	cfgDir := filepath.Join(home, ".config", "schoolwork-check")

	c := Config{
		CanvasBaseURL:         strings.TrimRight(os.Getenv("CANVAS_BASE_URL"), "/"),
		CanvasToken:           os.Getenv("CANVAS_TOKEN"),
		GoogleCredentialsFile: expandHome(envOr("GOOGLE_CREDENTIALS_FILE", filepath.Join(cfgDir, "google-credentials.json")), home),
		GoogleTokenFile:       expandHome(envOr("GOOGLE_TOKEN_FILE", filepath.Join(cfgDir, "google-token.json")), home),
		NoteBaseURL:           strings.TrimRight(envOr("NOTE_BASE_URL", "http://127.0.0.1:3271"), "/"),
		NoteToken:             os.Getenv("NOTE_TOKEN"),
		Brief:                 strings.ToLower(strings.TrimSpace(envOr("BRIEF", "note"))),
		MaxBriefs:             int(envInt64("NOTE_MAX_BRIEFS", 20)),
		NoteCalendar:          envBool("NOTE_CALENDAR", true),
		MaxCalendarEntries:    int(envInt64("NOTE_MAX_CALENDAR", 60)),
		NoteCalendarQuiet:     envBool("NOTE_CALENDAR_QUIET", false),
		Enrich:                envBool("ENRICH", true),
		LLMBaseURL:            strings.TrimRight(envOr("LLM_BASE_URL", "https://openrouter.ai/api/v1"), "/"),
		LLMModel:              envOr("LLM_MODEL", "deepseek/deepseek-v4-flash"),
		LLMAPIKey:             os.Getenv("LLM_API_KEY"),
		LLMMaxCalls:           int(envInt64("LLM_MAX_CALLS", 60)),
		LLMCacheFile:          expandHome(os.Getenv("LLM_CACHE_FILE"), home),
		ExtractAttachments:    envBool("EXTRACT_ATTACHMENTS", true),
		FileCacheFile:         expandHome(os.Getenv("FILE_CACHE_FILE"), home),
		MaxAttachmentBytes:    envInt64("MAX_ATTACHMENT_BYTES", 20<<20),
		MaxExtractedText:      int(envInt64("MAX_EXTRACTED_TEXT", 64<<10)),
		PastDays:              int(envInt64("PAST_DAYS", 30)),
		FutureDays:            int(envInt64("FUTURE_DAYS", 120)),
		CalendarDedup:         envBool("CALENDAR_DEDUP", true),
		CalendarMaterials:     envBool("CALENDAR_MATERIALS", true),
		CalendarMaxText:       int(envInt64("CALENDAR_MAX_TEXT", 512<<10)),
		CalendarModel:         strings.TrimSpace(os.Getenv("CALENDAR_MODEL")),
	}
	interval, err := ParseInterval(envOr("CALENDAR_INTERVAL", "7d"))
	if err != nil {
		return c, fmt.Errorf("CALENDAR_INTERVAL: %w", err)
	}
	c.CalendarInterval = interval
	if c.CanvasToken == "" {
		if c.CanvasToken, err = readSecretFile("CANVAS_TOKEN_FILE", "", home); err != nil {
			return c, err
		}
	}
	if c.NoteToken == "" {
		if c.NoteToken, err = readSecretFile("NOTE_TOKEN_FILE", "", home); err != nil {
			return c, err
		}
	}
	if c.LLMAPIKey == "" {
		if c.LLMAPIKey, err = readSecretFile("LLM_API_KEY_FILE", filepath.Join(cfgDir, "llm-api-key"), home); err != nil {
			return c, err
		}
	}
	switch c.Brief {
	case "local", "note", "off":
	default:
		return c, fmt.Errorf("BRIEF must be local, note or off, not %q", c.Brief)
	}
	docs, err := parseCalendarDocs(os.Getenv("CALENDAR_DOCS"))
	if err != nil {
		return c, err
	}
	c.CalendarDocs = docs
	feeds, err := parseCalendarFeeds(os.Getenv("GOOGLE_CALENDARS"))
	if err != nil {
		return c, err
	}
	c.GoogleCalendars = feeds
	if s := os.Getenv("SOURCES"); s != "" {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.Sources = append(c.Sources, p)
			}
		}
	}
	return c, nil
}

// CanvasEnabled reports whether Canvas is configured and selected.
func (c Config) CanvasEnabled() bool {
	return c.CanvasBaseURL != "" && c.CanvasToken != "" && c.wants("canvas")
}

// ClassroomEnabled reports whether Google Classroom is configured and selected.
func (c Config) ClassroomEnabled() bool {
	if !c.wants("classroom") {
		return false
	}
	_, err := os.Stat(c.GoogleCredentialsFile)
	return err == nil
}

func (c Config) wants(src string) bool {
	if len(c.Sources) == 0 {
		return true
	}
	for _, s := range c.Sources {
		if s == src {
			return true
		}
	}
	return false
}

// CalendarEnabled reports whether any calendar document is configured and
// selected, and the Google login it needs exists.
func (c Config) CalendarEnabled() bool {
	if len(c.CalendarDocs) == 0 || !c.wants("gdoc") {
		return false
	}
	_, err := os.Stat(c.GoogleCredentialsFile)
	return err == nil
}

// CalendarsEnabled reports whether any Google Calendar is configured and
// usable. A published .ics feed needs no login, so it is enough on its own;
// a calendar read through the API needs the Google credentials to exist.
func (c Config) CalendarsEnabled() bool {
	if len(c.GoogleCalendars) == 0 || !c.wants("gcal") {
		return false
	}
	for _, f := range c.GoogleCalendars {
		if f.ICSURL != "" {
			return true
		}
	}
	_, err := os.Stat(c.GoogleCredentialsFile)
	return err == nil
}

// CalendarParseModel is the model calendars are read with.
func (c Config) CalendarParseModel() string {
	if c.CalendarModel != "" {
		return c.CalendarModel
	}
	return c.LLMModel
}

// CalendarLLMEnabled reports whether a calendar document can be parsed into
// dated rows. Without a key the document is still fetched, but it goes
// downstream whole, as one piece of information, rather than as rows.
func (c Config) CalendarLLMEnabled() bool {
	return c.LLMAPIKey != "" && c.LLMModel != ""
}

// EnrichEnabled reports whether LLM enrichment is on and has a key.
func (c Config) EnrichEnabled() bool {
	return c.Brief == "local" && c.Enrich && c.LLMAPIKey != "" && c.LLMModel != ""
}

// NoteEnabled reports whether the note sink has a token to authenticate with.
func (c Config) NoteEnabled() bool { return c.NoteToken != "" }

var ErrNothingConfigured = errors.New("no sources configured: set CANVAS_BASE_URL+CANVAS_TOKEN and/or GOOGLE_CREDENTIALS_FILE")

// parseCalendarDocs reads CALENDAR_DOCS: comma- or newline-separated
// entries, each a Google Docs/Sheets/Drive URL or a bare file id, optionally
// followed by "|Course Name".
//
//	CALENDAR_DOCS=https://docs.google.com/document/d/1AbC.../edit|Biology|14d, 1XyZ...|AP Gov
//
// The third field is that document's own re-read interval, overriding
// CALENDAR_INTERVAL.
func parseCalendarDocs(s string) ([]CalendarDoc, error) {
	var out []CalendarDoc
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		ref, rest, _ := strings.Cut(field, "|")
		id, err := driveFileID(strings.TrimSpace(ref))
		if err != nil {
			return nil, err
		}
		course, every, _ := strings.Cut(rest, "|")
		d := CalendarDoc{ID: id, Course: strings.TrimSpace(course)}
		if every = strings.TrimSpace(every); every != "" {
			if d.Interval, err = ParseInterval(every); err != nil {
				return nil, fmt.Errorf("CALENDAR_DOCS: %s: %w", id, err)
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// parseCalendarFeeds reads GOOGLE_CALENDARS: comma- or newline-separated
// entries, each a calendar id or a URL that contains one, optionally
// followed by "|Label".
//
//	GOOGLE_CALENDARS=c_0123...@group.calendar.google.com|Meetings
func parseCalendarFeeds(s string) ([]CalendarFeed, error) {
	var out []CalendarFeed
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		ref, label, _ := strings.Cut(field, "|")
		ref = strings.TrimSpace(ref)
		id, err := calendarID(ref)
		if err != nil {
			return nil, err
		}
		feed := CalendarFeed{ID: id, Label: strings.TrimSpace(label)}
		if isICSURL(ref) {
			feed.ICSURL = ref
		}
		out = append(out, feed)
	}
	return out, nil
}

// isICSURL reports whether an entry is a published iCalendar feed rather
// than a calendar to reach through the API. Paste whichever URL Google's
// "Integrate calendar" panel gave you and the right transport is used: the
// .ics addresses need no login at all, the CalDAV one does.
func isICSURL(ref string) bool {
	if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".ics")
}

// calendarID accepts a calendar id or any of the URLs Google hands out for
// one: the CalDAV endpoint, the iCal feed, and the embed link.
func calendarID(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("GOOGLE_CALENDARS: empty calendar reference")
	}
	if !strings.Contains(ref, "/") && !strings.Contains(ref, "?") {
		return unescape(ref), nil
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("GOOGLE_CALENDARS: %q is not a calendar id or URL: %w", ref, err)
	}
	if src := u.Query().Get("src"); src != "" {
		return unescape(src), nil
	}
	// .../caldav/v2/<id>/events/ and .../calendar/ical/<id>/public/basic.ics
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i, p := range parts {
		if (p == "v2" || p == "ical") && i+1 < len(parts) && parts[i+1] != "" {
			return unescape(parts[i+1]), nil
		}
	}
	// A bare "<id>/events" style path.
	for _, p := range parts {
		if strings.Contains(p, "%40") || strings.Contains(p, "@") {
			return unescape(p), nil
		}
	}
	return "", fmt.Errorf("GOOGLE_CALENDARS: cannot find a calendar id in %q "+
		"(use the id from Calendar settings, or the URL it shows you)", ref)
}

// unescape undoes the percent-encoding a copied URL carries ("%40" is "@").
func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// ParseInterval reads a Go duration, with "d" and "w" added because a
// calendar's re-read is measured in days, not hours. "" and "0" mean zero.
func ParseInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	unit := time.Duration(0)
	switch {
	case strings.HasSuffix(s, "d"):
		unit = 24 * time.Hour
	case strings.HasSuffix(s, "w"):
		unit = 7 * 24 * time.Hour
	}
	if unit > 0 {
		n, err := strconv.ParseFloat(strings.TrimRight(s, "dw"), 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration (try 7d, 2w or 168h)", s)
		}
		return time.Duration(n * float64(unit)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (try 7d, 2w or 168h)", s)
	}
	return d, nil
}

// driveFileID accepts a Drive file id or any of the URL shapes Google hands
// out for one: /document/d/<id>/edit, /spreadsheets/d/<id>, /file/d/<id>/view,
// and ...?id=<id>.
func driveFileID(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("CALENDAR_DOCS: empty document reference")
	}
	if !strings.Contains(ref, "/") && !strings.Contains(ref, "?") {
		return ref, nil
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("CALENDAR_DOCS: %q is not a document id or URL: %w", ref, err)
	}
	if id := u.Query().Get("id"); id != "" {
		return id, nil
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "d" && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("CALENDAR_DOCS: cannot find a file id in %q "+
		"(use the URL from the document's address bar, or its id)", ref)
}

// readSecretFile returns the trimmed contents of the file named by env var k
// (or def when k is unset); a missing file is an empty secret.
func readSecretFile(k, def, home string) (string, error) {
	path := expandHome(envOr(k, def), home)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		return strings.TrimSpace(string(b)), nil
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	default:
		return "", fmt.Errorf("reading %s: %w", k, err)
	}
}

// expandHome replaces a leading "~/" with the user's home directory.
func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt64(k string, def int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
