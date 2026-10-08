// Package filecache remembers the text extracted from each attachment across
// runs, keyed by the file's identity and a version string the source supplies
// (Canvas updated_at, Drive version/modifiedTime). An unchanged file is not
// downloaded or parsed again, which is most of a fetch's time and API calls.
//
// A nil *Cache is valid and caches nothing, so callers need no branches.
package filecache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ttl forgets files no task has referenced for this long.
const ttl = 30 * 24 * time.Hour

// Entry is what an extraction produced. Only deterministic outcomes belong
// here: text, or a permanent refusal such as an unsupported type. Network
// and server failures must not be cached.
type Entry struct {
	Content      string `json:"content,omitempty"`
	ContentError string `json:"content_error,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

type record struct {
	Version string    `json:"version"`
	Entry   Entry     `json:"entry"`
	SeenAt  time.Time `json:"seen_at"`
}

type Cache struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	records map[string]record

	hits, misses atomic.Int64
}

// DefaultPath is $XDG_STATE_HOME/schoolwork-check/attachments.json, falling
// back to ~/.local/state/schoolwork-check/attachments.json.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "schoolwork-check", "attachments.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("filecache: locating cache file: %w", err)
	}
	return filepath.Join(home, ".local", "state", "schoolwork-check", "attachments.json"), nil
}

// Open loads path. A missing file is an empty cache. An unreadable one is
// also an empty cache (the error is returned for logging): the cost is one
// slow run, never a failed one.
func Open(path string) (*Cache, error) {
	c := &Cache{path: path, now: time.Now, records: map[string]record{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("filecache: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c.records); err != nil {
		c.records = map[string]record{}
		return c, fmt.Errorf("filecache: parsing %s: %w", path, err)
	}
	return c, nil
}

// Get returns the stored extraction when key was stored at this version.
// An empty version never matches: without one there is no way to tell a
// changed file from an unchanged one.
func (c *Cache) Get(key, version string) (Entry, bool) {
	if c == nil || version == "" {
		return Entry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.records[key]
	if !ok || r.Version != version {
		c.misses.Add(1)
		return Entry{}, false
	}
	r.SeenAt = c.now().UTC()
	c.records[key] = r
	c.hits.Add(1)
	return r.Entry, true
}

// Put stores an extraction. It is a no-op without a version.
func (c *Cache) Put(key, version string, e Entry) {
	if c == nil || version == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records[key] = record{Version: version, Entry: e, SeenAt: c.now().UTC()}
}

// Stats reports cache hits and misses since Open.
func (c *Cache) Stats() (hits, misses int64) {
	if c == nil {
		return 0, 0
	}
	return c.hits.Load(), c.misses.Load()
}

// Save prunes entries unseen for ttl and writes the file atomically, 0600.
func (c *Cache) Save() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	cutoff := c.now().UTC().Add(-ttl)
	for k, r := range c.records {
		if r.SeenAt.Before(cutoff) {
			delete(c.records, k)
		}
	}
	b, err := json.Marshal(c.records)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("filecache: encoding: %w", err)
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("filecache: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".attachments-*.tmp")
	if err != nil {
		return fmt.Errorf("filecache: creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("filecache: writing: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("filecache: closing: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		return fmt.Errorf("filecache: renaming into %s: %w", c.path, err)
	}
	return nil
}
