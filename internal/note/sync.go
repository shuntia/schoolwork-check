// sync.go is the diff engine: render every LMS task, work out which note
// task it already is, and send the smallest PATCH that makes note agree.
//
// Ownership: for a task this tool created, note's title, description and
// notes belong to us and are overwritten from the LMS on every run — except
// with Options.BriefContext set, where note's agent owns the description
// (plus duration, steps, and dropping non-homework) and the LMS text moves
// into notes — notes
// in particular, because it carries the sentinel line that identifies the
// task. A user who edits those fields in note will see the edit replaced;
// their state changes (drop, or marking something done) are respected, and
// so is deleting the task outright. duration_min, parent_id and is_now are
// never sent, so whatever the user sets there survives.
package note

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"schoolwork-check/internal/model"
)

// Options configures one Sync run.
type Options struct {
	// StateFile overrides DefaultStateFile().
	StateFile string
	// DryRun logs every planned call and makes none, and leaves the state
	// file untouched.
	DryRun bool
	// IncludeDone creates note tasks even for work the LMS already marks
	// submitted, graded or excused. Off by default: finished homework has
	// no business cluttering a task list. Tasks that already exist in note
	// are updated regardless.
	IncludeDone bool
	// IncludeNonHomework creates note tasks even for items the LLM judged
	// ask nothing of the student (optional forums, announcements). Off by
	// default. Like IncludeDone it only gates creation: a task already in
	// note keeps updating whatever the model later says.
	IncludeNonHomework bool
	// CoursePrefix and Location are passed through to RenderOptions.
	CoursePrefix bool
	Location     *time.Location
	Logger       *slog.Logger

	// BriefContext, when set, hands each open task to note's agent
	// (POST /api/tasks/{id}/agent) with this text as context: after it is
	// created, and again whenever the text changes. The agent then owns the
	// description, so sync stops writing it.
	BriefContext func(model.Task) string
	// InboxContext, when set, sends informational items (materials and
	// announcements) to note's agent inbox (POST /api/agent/inbox) instead
	// of creating tasks for them: once, and again when the text changes. An
	// item the inbox answers "task" for becomes a normal task. Items already
	// in note as tasks stay tasks.
	InboxContext func(model.Task) string
	// Calendar sends events — items with a time and a place rather than a
	// deliverable — to note's calendar instead of its task list. Off when
	// nil; MaxCalendar caps how many entries this tool may occupy (note
	// allows 100 in all, and the user's own entries come first).
	Calendar    bool
	MaxCalendar int
	// CalendarQuiet asks note to hold notifications while these events are
	// happening. Off by default: importing a calendar should not quietly
	// change what the app does during nineteen hours of the term.
	CalendarQuiet bool
	// MaxBriefs caps agent calls per run, briefs and inbox together (0 = no
	// cap); the rest wait for the next run. Calls are sequential: note
	// serialises them anyway.
	MaxBriefs int
}

// Result counts what one run did.
type Result struct {
	Created   int
	Updated   int
	Unchanged int
	// SkippedDone is work the LMS already considers finished, with
	// IncludeDone off.
	SkippedDone int
	// SkippedNotHomework is an item the LLM judged asks nothing of the
	// student, with IncludeNonHomework off.
	SkippedNotHomework int
	// SkippedDeclined is a task we made before that the user has since
	// dropped or deleted in note. It is never recreated.
	SkippedDeclined int
	// Migrated is a task from before 2026-09-21 (matched by the sentinel in
	// its notes) that this run stamped with its external_id, so note keys it
	// from now on.
	Migrated int
	// Briefed, BriefDropped and BriefFailed count agent calls: a brief
	// written, a task the agent dropped as not homework, and a call that
	// failed and will be retried next run. BriefDeferred is over MaxBriefs.
	Briefed        int
	BriefUnchanged int // the agent looked and changed nothing
	BriefDropped   int
	BriefFailed    int
	BriefDeferred  int
	// Calendar counts: entries written to note's calendar, entries left
	// alone because nothing changed, entries withdrawn when the event went
	// away, entries the user deleted by hand (never rewritten), and entries
	// past the cap.
	CalendarWritten   int
	CalendarUnchanged int
	CalendarRemoved   int
	CalendarDeclined  int
	CalendarDeferred  int
	// Inbox counts: facts remembered, nothing worth keeping, promoted to a
	// task, text unchanged since the last send, failed (retried next run),
	// and deferred (over MaxBriefs, or the agent was busy).
	InboxRemembered int
	InboxNothing    int
	InboxTask       int
	InboxUnchanged  int
	InboxFailed     int
	InboxDeferred   int
	Errors          []error
}

func (r Result) String() string {
	s := fmt.Sprintf("note: %d created, %d updated, %d unchanged, %d skipped (done), %d skipped (not homework), %d skipped (declined), %d errors",
		r.Created, r.Updated, r.Unchanged, r.SkippedDone, r.SkippedNotHomework, r.SkippedDeclined, len(r.Errors))
	if r.Migrated > 0 {
		s += fmt.Sprintf(", %d migrated to external ids", r.Migrated)
	}
	if n := r.Briefed + r.BriefUnchanged + r.BriefDropped + r.BriefFailed + r.BriefDeferred; n > 0 {
		s += fmt.Sprintf("; agent: %d briefed, %d unchanged, %d dropped, %d failed, %d deferred",
			r.Briefed, r.BriefUnchanged, r.BriefDropped, r.BriefFailed, r.BriefDeferred)
	}
	if n := r.InboxRemembered + r.InboxNothing + r.InboxTask + r.InboxFailed + r.InboxDeferred; n > 0 || r.InboxUnchanged > 0 {
		s += fmt.Sprintf("; inbox: %d remembered, %d nothing, %d became tasks, %d unchanged, %d failed, %d deferred",
			r.InboxRemembered, r.InboxNothing, r.InboxTask, r.InboxUnchanged, r.InboxFailed, r.InboxDeferred)
	}
	if n := r.CalendarWritten + r.CalendarUnchanged + r.CalendarRemoved + r.CalendarDeclined + r.CalendarDeferred; n > 0 {
		s += fmt.Sprintf("; calendar: %d written, %d unchanged, %d withdrawn, %d declined, %d deferred",
			r.CalendarWritten, r.CalendarUnchanged, r.CalendarRemoved, r.CalendarDeclined, r.CalendarDeferred)
	}
	return s
}

// Sync pushes tasks into note. It returns a non-nil error only for a fatal
// problem — the list call failed, the token was rejected, or the state file
// could not be written. Per-task failures land in Result.Errors and the run
// continues.
func Sync(ctx context.Context, c *Client, tasks []model.Task, o Options) (Result, error) {
	var r Result

	log := o.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	statePath := o.StateFile
	if statePath == "" {
		p, err := DefaultStateFile()
		if err != nil {
			return r, err
		}
		statePath = p
	}
	state, err := LoadState(statePath)
	if err != nil {
		// A corrupt state file is recoverable: we fall back to matching by
		// sentinel, which costs us only the dropped-task memory.
		log.Warn("note: ignoring unreadable sync state", "path", statePath, "err", err)
	}

	nodes, err := c.List(ctx)
	if err != nil {
		return r, err
	}
	byExternal, byID := index(nodes)

	dirty := false
	b := briefer{on: o.BriefContext != nil}
	cal, err := newCalendarSink(ctx, c, tasks, &state, o, log)
	if err != nil {
		return r, err
	}
	for _, t := range tasks {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if cal.handles(t) {
			changed, err := cal.push(ctx, c, t, &state, o, log, &r)
			if errors.Is(err, ErrUnauthorized) {
				return r, err
			}
			if err != nil {
				r.Errors = append(r.Errors, fmt.Errorf("%s: %w", t.ID, err))
			}
			dirty = dirty || changed
			continue
		}
		if o.InboxContext != nil && t.Kind.Informational() && !isNoteTask(t.ID, byExternal, &state) {
			promote, changed, err := b.inbox(ctx, c, t, &state, o, log, &r)
			if errors.Is(err, ErrUnauthorized) {
				return r, err
			}
			dirty = dirty || changed
			if !promote {
				continue
			}
		}
		rendered := Render(t, RenderOptions{CoursePrefix: o.CoursePrefix, Location: o.Location, AgentOwnsDescription: b.on})
		changed, err := syncOne(ctx, c, rendered, byExternal, byID, &state, o, log, &r)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return r, err
			}
			r.Errors = append(r.Errors, fmt.Errorf("%s: %w", rendered.ExternalID, err))
		}
		dirty = dirty || changed

		if b.on && err == nil {
			changed, err := b.brief(ctx, c, t, rendered, byID, &state, o, log, &r)
			if errors.Is(err, ErrUnauthorized) {
				return r, err
			}
			dirty = dirty || changed
		}
	}

	// Events that have left the calendar are withdrawn from note's.
	if changed := cal.sweep(ctx, c, &state, o, log, &r); changed {
		dirty = true
	}

	if o.DryRun {
		log.Info("note: dry run, nothing written", "state_file", statePath)
		return r, nil
	}
	if dirty {
		if err := SaveState(statePath, state); err != nil {
			return r, err
		}
	}
	return r, nil
}

// syncOne handles one rendered task. It reports whether the state file needs
// saving, which is true even when the call that followed failed: a note task
// we created must be remembered immediately or the next run makes a second.
func syncOne(ctx context.Context, c *Client, r Rendered, byExternal map[string]Task, byID map[int64]Task,
	state *SyncState, o Options, log *slog.Logger, res *Result) (bool, error) {

	existing, found := byExternal[r.ExternalID]
	remembered, known := state.Get(r.ExternalID)

	if !found && known {
		// A task from before external ids whose sentinel was edited out of
		// notes; the id we stored is the other way in.
		if t, live := byID[remembered.NoteID]; live {
			existing, found = t, true
		} else {
			// Not in the list under either key: dropped or deleted in note.
			// That is the user's decision and we do not undo it. (A deleted
			// task also leaves a tombstone in note, so the PUT below would
			// answer 410 anyway; this check covers a dropped one, which the
			// listing hides but note still holds.)
			res.SkippedDeclined++
			log.Debug("note: declined, not recreating", "external_id", r.ExternalID, "note_id", remembered.NoteID, "title", r.Title)
			return false, nil
		}
	}

	if !found {
		return create(ctx, c, r, state, o, log, res)
	}

	// Adopt a task we found in note but have no memory of (fresh state
	// file, or a task created by an older run).
	if !known || remembered.NoteID != existing.ID {
		remembered = TaskState{NoteID: existing.ID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	return update(ctx, c, r, existing, remembered, state, o, log, res)
}

func create(ctx context.Context, c *Client, r Rendered, state *SyncState, o Options, log *slog.Logger, res *Result) (bool, error) {
	if r.State == "done" && !o.IncludeDone {
		res.SkippedDone++
		log.Debug("note: skipping finished work", "external_id", r.ExternalID, "title", r.Title)
		return false, nil
	}
	if r.NotHomework != "" && !o.IncludeNonHomework {
		res.SkippedNotHomework++
		log.Info("note: skipping, not homework", "external_id", r.ExternalID, "title", r.Title, "reason", r.NotHomework)
		return false, nil
	}
	if o.DryRun {
		res.Created++
		log.Info("note: would create", "external_id", r.ExternalID, "title", r.Title, "state", r.State)
		return false, nil
	}

	fields := UpsertFields{Title: r.Title, Notes: r.Notes, DueAt: dueAt(r), URL: r.URL}
	if r.OwnsDescription {
		fields.Description = r.Description
	}
	if r.State != "open" {
		// open is what note sets anyway; in_progress and done are ours to say.
		fields.State = r.State
	}
	created, made, err := c.Upsert(ctx, r.ExternalID, fields)
	if errors.Is(err, ErrDeclined) {
		res.SkippedDeclined++
		log.Debug("note: declined, note remembers the user deleting it", "external_id", r.ExternalID, "title", r.Title)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("upsert: %w", err)
	}
	switch {
	case made:
		res.Created++
		log.Info("note: created", "external_id", r.ExternalID, "note_id", created.ID, "title", r.Title)
	case created.State == "dropped":
		// note keeps a dropped task out of the listing but still keyed by
		// our id: it refreshed the text and left it dropped.
		res.SkippedDeclined++
		log.Debug("note: declined, dropped in note", "external_id", r.ExternalID, "note_id", created.ID, "title", r.Title)
	default:
		res.Updated++
		log.Info("note: updated", "external_id", r.ExternalID, "note_id", created.ID, "title", r.Title)
	}
	state.Set(r.ExternalID, TaskState{
		NoteID:          created.ID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		Hash:            hashRendered(r),
		LastPushedState: r.State,
	})
	return true, nil
}

func update(ctx context.Context, c *Client, r Rendered, existing Task, entry TaskState,
	state *SyncState, o Options, log *slog.Logger, res *Result) (bool, error) {

	fields := map[string]any{}
	if existing.Title != r.Title {
		fields["title"] = r.Title
	}
	if r.OwnsDescription && existing.Description != r.Description {
		fields["description"] = r.Description
	}
	if existing.Notes != r.Notes {
		fields["notes"] = r.Notes
	}
	if s, ok := forwardState(existing.State, r.State); ok {
		fields["state"] = s
	}
	// A step cannot hold a deadline (422); when the user demoted our task
	// under a parent, the parent keeps it and we leave it alone.
	if due := dueAt(r); existing.ParentID == nil && !sameInstant(existing.DueAt, due) {
		fields["due_at"] = due // nil clears it
	}
	if existing.URL != r.URL {
		fields["url"] = r.URL
	}
	// A task from a run older than 2026-09-21 carries only our sentinel in
	// its notes; stamping the external id lets note key it from now on.
	migrate := existing.ExternalID == ""
	if migrate {
		fields["external_id"] = r.ExternalID
	}

	// The stored hash is an optimisation, not the decision: what note holds
	// right now is the source of truth, so a hand edit to our description or
	// notes is rewritten from the LMS on the next run (intended — those two
	// fields are ours, and notes carries the sentinel). The hash only lets
	// us confirm the no-op cheaply.
	hash := hashRendered(r)
	content := len(fields) - btoi(migrate) // fields that change what the user sees
	if len(fields) == 0 {
		res.Unchanged++
		if entry.Hash != hash || entry.NoteID != existing.ID {
			entry.Hash, entry.LastPushedState, entry.NoteID = hash, existing.State, existing.ID
			state.Set(r.ExternalID, entry)
			return true, nil
		}
		return false, nil
	}

	if o.DryRun {
		if content > 0 {
			res.Updated++
		} else {
			res.Unchanged++
		}
		res.Migrated += btoi(migrate)
		log.Info("note: would update", "external_id", r.ExternalID, "note_id", existing.ID,
			"title", r.Title, "fields", strings.Join(keys(fields), ","))
		return false, nil
	}

	if _, err := c.Patch(ctx, existing.ID, fields); err != nil {
		// Remember the id anyway when we only just adopted this task.
		state.Set(r.ExternalID, entry)
		return true, fmt.Errorf("patch (note id %d): %w", existing.ID, err)
	}
	if content > 0 {
		res.Updated++
	} else {
		res.Unchanged++
	}
	res.Migrated += btoi(migrate)
	log.Info("note: updated", "external_id", r.ExternalID, "note_id", existing.ID,
		"title", r.Title, "fields", strings.Join(keys(fields), ","))

	entry.NoteID = existing.ID
	entry.Hash = hash
	entry.LastPushedState = r.State
	state.Set(r.ExternalID, entry)
	return true, nil
}

// forwardState reports the state to send, if any. Progress from the LMS only
// ever moves forward: open → in_progress → done. A task the user dropped, or
// already finished in note, is never touched.
func forwardState(existing, rendered string) (string, bool) {
	if existing == "done" || existing == "dropped" {
		return "", false
	}
	if stateRank(rendered) > stateRank(existing) {
		return rendered, true
	}
	return "", false
}

func stateRank(s string) int {
	switch s {
	case "in_progress":
		return 1
	case "done":
		return 2
	default: // "open", and anything unrecognised
		return 0
	}
}

// index maps our external ids to note tasks: by note's external_id, or for
// a task from before 2026-09-21 by the sentinel in its notes. Both levels
// are read (a task of ours may have been demoted to a step), and a by-id
// map is kept as the fallback lookup.
func index(nodes []TaskNode) (map[string]Task, map[int64]Task) {
	byExternal := make(map[string]Task)
	byID := make(map[int64]Task)
	add := func(t Task) {
		byID[t.ID] = t
		if t.ExternalID != "" {
			byExternal[t.ExternalID] = t
			return
		}
		// A task from before 2026-09-21 carries only the sentinel in its
		// notes; update() stamps its external id on the next write.
		if id, ok := externalIDOf(t.Notes); ok {
			if _, taken := byExternal[id]; !taken {
				byExternal[id] = t
			}
		}
	}
	for _, n := range nodes {
		add(n.Task)
		for _, ch := range n.Children {
			add(ch)
		}
	}
	return byExternal, byID
}

// externalIDOf pulls our id out of a notes field. Render puts the sentinel on
// the last line; we take the last sentinel line anywhere in the text so that
// a user who typed something underneath it is still matched.
func externalIDOf(notes string) (string, bool) {
	var id string
	var ok bool
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, cut := strings.CutPrefix(line, Sentinel); cut {
			if rest = strings.TrimSpace(rest); rest != "" {
				id, ok = rest, true
			}
		}
	}
	return id, ok
}

// dueAt is the rendering's deadline as note stores it: RFC 3339 in UTC.
func dueAt(r Rendered) *string {
	if r.DueAt == nil {
		return nil
	}
	s := r.DueAt.UTC().Format(time.RFC3339)
	return &s
}

// sameInstant compares two RFC 3339 strings as instants, so note's UTC
// rendering of a +09:00 deadline is not mistaken for a change.
func sameInstant(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, errA := time.Parse(time.RFC3339, *a)
	tb, errB := time.Parse(time.RFC3339, *b)
	if errA != nil || errB != nil {
		return *a == *b
	}
	return ta.Equal(tb)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func hashRendered(r Rendered) string {
	due := ""
	if d := dueAt(r); d != nil {
		due = *d
	}
	return Hash(r.Title, r.Description, r.Notes, r.State, due, r.URL)
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for _, k := range []string{"title", "description", "notes", "state", "due_at", "url", "external_id"} {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// briefer runs note's agent over tasks whose LMS text is new or changed.
type briefer struct {
	on           bool
	used         int
	stopped      bool // no more briefs this run: route missing (404) or agent busy (429/503)
	inboxStopped bool // no more inbox calls this run: route missing or agent busy
}

// isNoteTask reports whether an item already exists as a note task (listed
// with our sentinel, or remembered in the state file). Such an item keeps
// being synced as a task even if its kind is informational.
func isNoteTask(externalID string, byExternal map[string]Task, state *SyncState) bool {
	if _, ok := byExternal[externalID]; ok {
		return true
	}
	_, ok := state.Get(externalID)
	return ok
}

// inbox sends one informational item to note's agent inbox when it is new or
// its text changed. It reports whether the item should now be synced as a
// task (the inbox answered "task", now or on an earlier run) and whether the
// state file changed. Only ErrUnauthorized is returned.
func (b *briefer) inbox(ctx context.Context, c *Client, t model.Task, state *SyncState, o Options, log *slog.Logger, res *Result) (bool, bool, error) {
	prev := state.Inbox[t.ID]
	if prev.Outcome == "task" {
		return true, false, nil
	}
	text := o.InboxContext(t)
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])
	if prev.Hash == hash {
		res.InboxUnchanged++
		return false, false, nil
	}
	if b.inboxStopped || (o.MaxBriefs > 0 && b.used >= o.MaxBriefs) {
		res.InboxDeferred++
		return false, false, nil
	}
	b.used++
	if o.DryRun {
		res.InboxDeferred++
		log.Info("note: would send to inbox", "external_id", t.ID, "kind", t.Kind, "title", t.Title)
		return false, false, nil
	}

	start := time.Now()
	out, err := c.Inbox(ctx, t.ID, string(t.Kind), text)
	var apiErr *APIError
	switch {
	case err == nil:
		state.SetInbox(t.ID, InboxState{Hash: hash, Outcome: out.Outcome, SentAt: time.Now().UTC().Format(time.RFC3339)})
		switch out.Outcome {
		case "remembered":
			res.InboxRemembered++
		case "task":
			res.InboxTask++
		default:
			res.InboxNothing++
		}
		log.Info("note: inbox "+out.Outcome, "external_id", t.ID, "kind", t.Kind, "title", t.Title,
			"facts", len(out.MemoryIDs), "reason", out.Reason, "took", time.Since(start).Round(time.Second))
		return out.Outcome == "task", true, nil
	case errors.Is(err, ErrUnauthorized):
		return false, false, err
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Message == "":
		b.inboxStopped = true
		res.InboxFailed++
		log.Warn("note: agent inbox route missing (404), informational items wait for a note that has it")
		return false, false, nil
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusTooManyRequests || apiErr.Status == http.StatusServiceUnavailable):
		b.inboxStopped, b.stopped = true, true
		res.InboxFailed++
		log.Warn("note: agent busy or capped, skipping agent calls this run", "status", apiErr.Status, "err", err)
		return false, false, nil
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity:
		state.SetInbox(t.ID, InboxState{Hash: hash, Outcome: "rejected", SentAt: time.Now().UTC().Format(time.RFC3339)})
		res.InboxFailed++
		log.Warn("note: inbox rejected this item, not resending until it changes", "external_id", t.ID, "err", err)
		return false, true, nil
	default:
		res.InboxFailed++
		log.Warn("note: inbox call failed, retrying next run", "external_id", t.ID, "err", err)
		return false, false, nil
	}
}

// brief calls the agent for one task when it is due. It reports whether the
// state file changed; only ErrUnauthorized is returned, every other failure
// is counted and retried next run.
func (b *briefer) brief(ctx context.Context, c *Client, t model.Task, r Rendered, byID map[int64]Task,
	state *SyncState, o Options, log *slog.Logger, res *Result) (bool, error) {

	entry, ok := state.Get(r.ExternalID)
	if !ok || entry.NoteID == 0 || r.State == "done" || b.stopped {
		return false, nil
	}
	// What note holds now: the listing from the start of the run, or, for a
	// task created this run, the state we pushed.
	noteState := entry.LastPushedState
	if existing, live := byID[entry.NoteID]; live {
		noteState = existing.State
	}
	if noteState == "done" || noteState == "dropped" {
		return false, nil
	}

	text := o.BriefContext(t)
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])
	if entry.BriefHash == hash {
		return false, nil
	}
	if o.MaxBriefs > 0 && b.used >= o.MaxBriefs {
		res.BriefDeferred++
		return false, nil
	}
	b.used++
	if o.DryRun {
		res.Briefed++
		log.Info("note: would brief", "external_id", r.ExternalID, "note_id", entry.NoteID, "title", r.Title)
		return false, nil
	}

	start := time.Now()
	out, err := c.Brief(ctx, entry.NoteID, text)
	var apiErr *APIError
	switch {
	case err == nil:
		entry.BriefHash = hash
		state.Set(r.ExternalID, entry)
		switch out.Outcome {
		case "dropped":
			res.BriefDropped++
		case "unchanged":
			res.BriefUnchanged++
		default:
			res.Briefed++
		}
		log.Info("note: agent "+out.Outcome, "external_id", r.ExternalID, "note_id", entry.NoteID,
			"title", r.Title, "tool_calls", len(out.Steps), "took", time.Since(start).Round(time.Second))
		return true, nil
	case errors.Is(err, ErrUnauthorized):
		return false, err
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Message != "":
		// The route's own 404 carries {"error": ...}: the task was deleted
		// in note after the listing. The next run's listing reconciles it.
		log.Info("note: task deleted before its brief", "external_id", r.ExternalID, "note_id", entry.NoteID)
		return false, nil
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		// An empty-body 404 is the router's: this note build has no agent
		// route.
		b.stopped = true
		res.BriefFailed++
		log.Warn("note: agent route missing (404), skipping briefs this run", "note_id", entry.NoteID)
		return false, nil
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusTooManyRequests || apiErr.Status == http.StatusServiceUnavailable):
		// 429: a session already in flight or the daily agent cap. 503:
		// note's server-wide session capacity is full. Either way the rest
		// of this run would be refused too; keep the old hash so the next
		// run retries.
		b.stopped, b.inboxStopped = true, true
		res.BriefFailed++
		log.Warn("note: agent busy or capped, skipping agent calls this run", "status", apiErr.Status, "note_id", entry.NoteID, "err", err)
		return false, nil
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusUnprocessableEntity):
		// A step (409) or oversized context (422) will not change by
		// retrying: remember the hash so we do not ask every run.
		entry.BriefHash = hash
		state.Set(r.ExternalID, entry)
		res.BriefFailed++
		log.Warn("note: agent refused this task", "external_id", r.ExternalID, "note_id", entry.NoteID, "err", err)
		return true, nil
	default:
		res.BriefFailed++
		log.Warn("note: agent call failed, retrying next run", "external_id", r.ExternalID, "note_id", entry.NoteID, "err", err)
		return false, nil
	}
}
