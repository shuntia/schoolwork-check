// state.go keeps the one thing this sink cannot rederive: which note task a
// given LMS task became. Practically nothing else persists. The file exists
// only so that (a) a task the user dropped or deleted in note — it vanishes
// from GET /api/tasks either way — is not recreated on the next run, and
// (b) an unchanged task can be recognised without a round trip.
//
// It goes away the day note grows external_id and an upsert route.
package note

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// stateVersion is bumped only if the on-disk shape changes incompatibly.
const stateVersion = 1

// SyncState is the whole file: a map from our external id to what we know
// about the note task we made for it.
type SyncState struct {
	Version int                  `json:"version"`
	Tasks   map[string]TaskState `json:"tasks"`
	// Inbox records informational items sent to note's agent inbox instead
	// of becoming tasks, keyed by the same external id.
	Inbox map[string]InboxState `json:"inbox,omitempty"`
	// Calendar records events pushed to note's calendar. note keeps no
	// tombstones, so without this a entry the user deleted by hand would be
	// recreated on the next run.
	Calendar map[string]CalendarState `json:"calendar,omitempty"`
}

// CalendarState is one event we put on note's calendar.
type CalendarState struct {
	// Hash is the entry we last wrote; a match means nothing to do.
	Hash string `json:"hash"`
	// Declined is set once the user deletes the entry in note. It is then
	// never written again, the way a dropped task is never recreated.
	Declined bool   `json:"declined,omitempty"`
	PushedAt string `json:"pushed_at,omitempty"`
}

// InboxState is what the inbox last decided for one item.
type InboxState struct {
	// Hash is the sha256 of the context last sent; a mismatch re-sends.
	Hash string `json:"hash"`
	// Outcome is remembered, nothing, task, or rejected (a 422: the item
	// is not sent again until its text changes). "task" makes the item a
	// normal task from then on.
	Outcome string `json:"outcome"`
	SentAt  string `json:"sent_at"`
}

// SetCalendar records what we pushed for one event.
func (s *SyncState) SetCalendar(externalID string, cs CalendarState) {
	if s.Calendar == nil {
		s.Calendar = map[string]CalendarState{}
	}
	s.Calendar[externalID] = cs
}

// ForgetCalendar drops an event we no longer track.
func (s *SyncState) ForgetCalendar(externalID string) {
	delete(s.Calendar, externalID)
}

// TaskState is one remembered task.
type TaskState struct {
	NoteID int64 `json:"note_id"`
	// CreatedAt is RFC 3339, purely for a human reading the file.
	CreatedAt string `json:"created_at"`
	// LastPushedState is the note state we last sent (or "" if we never
	// sent one), so the file explains itself without the server.
	LastPushedState string `json:"last_pushed_state"`
	// Hash is Hash(title, description, notes, state, due_at, url) of the rendering we
	// last pushed successfully. Empty means "unknown, push again".
	Hash string `json:"hash"`
	// BriefHash is the sha256 of the context note's agent last briefed this
	// task from. Empty means never briefed; a mismatch means the teacher's
	// text changed and the task is briefed again.
	BriefHash string `json:"brief_hash,omitempty"`
}

// Get reads one entry.
func (s SyncState) Get(externalID string) (TaskState, bool) {
	ts, ok := s.Tasks[externalID]
	return ts, ok
}

// Set writes one entry, allocating the map on first use.
func (s *SyncState) Set(externalID string, ts TaskState) {
	if s.Tasks == nil {
		s.Tasks = make(map[string]TaskState)
	}
	s.Tasks[externalID] = ts
}

// SetInbox records one inbox decision.
func (s *SyncState) SetInbox(externalID string, is InboxState) {
	if s.Inbox == nil {
		s.Inbox = make(map[string]InboxState)
	}
	s.Inbox[externalID] = is
}

// Hash digests the four fields we own on a note task. A change in any of
// them means the rendering moved and the task needs a PATCH.
func Hash(title, description, notes, state string, extra ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{title, description, notes, state}, extra...), "|")))
	return hex.EncodeToString(sum[:])
}

// DefaultStateFile is $XDG_STATE_HOME/schoolwork-check/note-sync.json,
// falling back to ~/.local/state/schoolwork-check/note-sync.json.
func DefaultStateFile() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "schoolwork-check", "note-sync.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("note: locating state file: %w", err)
	}
	return filepath.Join(home, ".local", "state", "schoolwork-check", "note-sync.json"), nil
}

// LoadState reads path. A missing file is an empty state, not an error: the
// first run has nothing to remember.
func LoadState(path string) (SyncState, error) {
	s := SyncState{Version: stateVersion, Tasks: map[string]TaskState{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, fmt.Errorf("note: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return SyncState{Version: stateVersion, Tasks: map[string]TaskState{}},
			fmt.Errorf("note: parsing %s: %w", path, err)
	}
	if s.Tasks == nil {
		s.Tasks = map[string]TaskState{}
	}
	s.Version = stateVersion
	return s, nil
}

// SaveState writes path atomically (temp file in the same directory, then
// rename) with 0600, creating parent directories as needed.
func SaveState(path string, s SyncState) error {
	s.Version = stateVersion
	if s.Tasks == nil {
		s.Tasks = map[string]TaskState{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("note: encoding state: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("note: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".note-sync-*.tmp")
	if err != nil {
		return fmt.Errorf("note: creating temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("note: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("note: writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("note: syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("note: closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("note: renaming into %s: %w", path, err)
	}
	return nil
}
