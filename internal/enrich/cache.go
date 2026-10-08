package enrich

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"schoolwork-check/internal/model"
)

// cacheTTL drops entries for tasks not seen in this long (past the fetch
// window, deleted by the teacher, ...).
const cacheTTL = 60 * 24 * time.Hour

// Cache maps a task id to the last answer and the input hash it answered.
type Cache struct {
	Version int                   `json:"version"`
	Tasks   map[string]CacheEntry `json:"tasks"`
}

type CacheEntry struct {
	InputHash  string           `json:"input_hash"`
	Enrichment model.Enrichment `json:"enrichment"`
	SeenAt     time.Time        `json:"seen_at"`
}

// Get returns the cached answer when the input is unchanged, and marks it seen.
func (c *Cache) Get(id, hash string, now time.Time) (model.Enrichment, bool) {
	e, ok := c.Tasks[id]
	if !ok || e.InputHash != hash {
		return model.Enrichment{}, false
	}
	e.SeenAt = now
	c.Tasks[id] = e
	return e.Enrichment, true
}

// Stale returns the last answer for id whatever input it answered. Used when
// a fresh call is not possible this run, so a brief does not vanish from note
// only to reappear next run.
func (c *Cache) Stale(id string) (model.Enrichment, bool) {
	e, ok := c.Tasks[id]
	return e.Enrichment, ok
}

func (c *Cache) Put(id, hash string, e model.Enrichment, now time.Time) {
	c.Tasks[id] = CacheEntry{InputHash: hash, Enrichment: e, SeenAt: now}
}

// Prune forgets entries older than cacheTTL.
func (c *Cache) Prune(now time.Time) {
	for id, e := range c.Tasks {
		if now.Sub(e.SeenAt) > cacheTTL {
			delete(c.Tasks, id)
		}
	}
}

// DefaultCacheFile is $XDG_STATE_HOME/schoolwork-check/enrich-cache.json,
// falling back to ~/.local/state/schoolwork-check/enrich-cache.json.
func DefaultCacheFile() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "schoolwork-check", "enrich-cache.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("enrich: locating cache file: %w", err)
	}
	return filepath.Join(home, ".local", "state", "schoolwork-check", "enrich-cache.json"), nil
}

// LoadCache reads path; a missing or unreadable file is an empty cache (the
// error is still returned so the caller can log it).
func LoadCache(path string) (*Cache, error) {
	c := &Cache{Version: 1, Tasks: map[string]CacheEntry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("enrich: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, c); err != nil {
		return &Cache{Version: 1, Tasks: map[string]CacheEntry{}}, fmt.Errorf("enrich: parsing %s: %w", path, err)
	}
	if c.Tasks == nil {
		c.Tasks = map[string]CacheEntry{}
	}
	return c, nil
}

// SaveCache writes path atomically with 0600.
func SaveCache(path string, c *Cache) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("enrich: encoding cache: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("enrich: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".enrich-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("enrich: creating temp cache file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("enrich: writing cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("enrich: closing cache: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("enrich: renaming into %s: %w", path, err)
	}
	return nil
}
