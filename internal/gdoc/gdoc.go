// Package gdoc reads course calendars that live in a document rather than in
// an LMS: a Google Doc or Sheet, or an uploaded PDF or DOCX, whose rows are
// dates and what happens on them. Teachers often keep the real schedule
// there and post only some of it to Canvas or Classroom.
//
// Each configured document is exported to text once per Drive revision, then
// a language model turns the schedule into dated rows. A row that asks
// something of the student becomes an assignment or quiz task; a row that is
// only information — a reading, "no school", a unit change — becomes an
// informational item, which the note sink hands to its agent inbox instead of
// the task list. The model makes that call, per row.
//
// With no model configured the document is not parsed: it goes downstream
// whole, as one informational item, so the calendar is never silently lost.
package gdoc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"schoolwork-check/internal/enrich"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// Doc is one document to read a calendar out of.
type Doc struct {
	// ID is the Drive file id.
	ID string
	// Course is the course name put on every task from this document.
	// Empty means the document's own name in Drive.
	Course string
	// Interval is how long a parse of this document stays good even after
	// the teacher edits it. A long calendar is edited constantly and read
	// rarely, and every re-read costs model calls; once a week is plenty.
	// Zero means Options.Interval.
	Interval time.Duration
}

// Options configures a Client.
type Options struct {
	// Docs are the documents to read, in order.
	Docs []Doc
	// Model is recorded on each parse and is part of the cache key, so
	// switching models re-parses every document.
	Model string
	// MaxExtractedText caps the text read out of one document. A calendar
	// is not an attachment appended to a brief — it is the whole point of
	// the fetch — so this defaults to DefaultMaxText rather than to the
	// much smaller per-attachment cap.
	MaxExtractedText int
	// MaxAttachmentBytes skips uploaded files larger than this.
	MaxAttachmentBytes int64
	// PastDays and FutureDays bound which dated rows become tasks.
	PastDays, FutureDays int
	// Interval is the default Doc.Interval. Zero re-reads a document as
	// soon as Drive says it changed.
	Interval time.Duration
	// SkipDocIDs are Drive file ids read as configured documents already.
	// Expand leaves LMS items that point at them alone, so one calendar
	// does not become two sets of rows.
	SkipDocIDs []string
	// FileCache remembers each document's text and its parsed rows, both
	// keyed by the Drive revision: an unedited calendar costs no model call.
	FileCache *filecache.Cache
	Logger    *slog.Logger
	// Location interprets dates the document writes without a time zone
	// (nil = time.Local), and Now overrides the clock in tests.
	Location *time.Location
	Now      func() time.Time
}

// Client reads the configured documents.
type Client struct {
	src docSource
	llm enrich.Completer
	log *slog.Logger

	opts Options
}

// New builds a client from the Google login google-login cached. llm may be
// nil, in which case documents are not parsed into rows.
func New(ctx context.Context, credentialsFile, tokenFile string, llm enrich.Completer, opts Options) (*Client, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	src, err := newDriveSource(ctx, credentialsFile, tokenFile, opts.MaxExtractedText, opts.MaxAttachmentBytes, log)
	if err != nil {
		return nil, err
	}
	return newWithSource(src, llm, opts), nil
}

func newWithSource(src docSource, llm enrich.Completer, opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Client{src: src, llm: llm, log: opts.Logger, opts: opts}
}

// Fetch reads every configured document. A document that fails is reported
// and the others still produce tasks; the error is non-nil when any did.
func (c *Client) Fetch(ctx context.Context) ([]model.Task, error) {
	if c.src == nil || len(c.opts.Docs) == 0 {
		return nil, nil // a parser built by NewParser reads no documents of its own
	}
	now := c.opts.Now()
	var (
		tasks []model.Task
		errs  []error
	)
	for _, d := range c.opts.Docs {
		if err := ctx.Err(); err != nil {
			return tasks, err
		}
		ts, err := c.fetchDoc(ctx, d, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("calendar %s: %w", d.ID, err))
			continue
		}
		c.log.Debug("gdoc: read calendar", "doc", d.ID, "tasks", len(ts))
		tasks = append(tasks, ts...)
	}
	c.log.Info("gdoc: calendars read", "docs", len(c.opts.Docs), "tasks", len(tasks), "failed", len(errs))
	return tasks, errors.Join(errs...)
}

// fetchDoc turns one document into tasks. The document's text is downloaded
// only when it is actually going to be read: a calendar whose parse is still
// good costs one metadata call and no model call at all.
func (c *Client) fetchDoc(ctx context.Context, d Doc, now time.Time) ([]model.Task, error) {
	meta, err := c.src.Meta(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	course := d.Course
	if course == "" {
		course = meta.Name
	}

	if cached, ok := c.freshParse(d, meta, now); ok {
		if len(cached) > 0 {
			c.log.Debug("gdoc: reusing the last parse", "doc", d.ID, "entries", len(cached))
			return c.tasks(meta, course, cached, now), nil
		}
		// The last parse found no rows: the document is prose, not a
		// table. It still goes on whole, which needs its text.
		if err := c.fill(ctx, &meta); err != nil {
			return nil, err
		}
		return []model.Task{c.wholeDoc(meta, course, now)}, nil
	}

	if err := c.fill(ctx, &meta); err != nil {
		return nil, err
	}
	if c.llm == nil {
		c.log.Info("gdoc: no model configured, passing the calendar on whole",
			"doc", d.ID, "name", meta.Name)
		return []model.Task{c.wholeDoc(meta, course, now)}, nil
	}
	entries, err := c.parse(ctx, meta, course, now)
	if err != nil {
		// Nothing is cached on failure, so the next run tries again. The
		// calendar is not dumped into the inbox meanwhile: that would send
		// the whole document as one fact and then contradict it tomorrow.
		return nil, err
	}
	if len(entries) == 0 {
		c.log.Info("gdoc: no dated rows found, passing the document on whole",
			"doc", d.ID, "name", meta.Name)
		return []model.Task{c.wholeDoc(meta, course, now)}, nil
	}
	return c.tasks(meta, course, entries, now), nil
}

// fill downloads the document's text, and rejects a document there is none of.
func (c *Client) fill(ctx context.Context, meta *doc) error {
	if err := c.src.Fill(ctx, meta); err != nil {
		return err
	}
	if strings.TrimSpace(meta.Text) == "" {
		return errors.New("no text could be read from it")
	}
	return nil
}

// interval is how long this document's parse stays good after an edit.
func (c *Client) interval(d Doc) time.Duration {
	if d.Interval > 0 {
		return d.Interval
	}
	return c.opts.Interval
}
