package filecache

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGetPutVersionAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "attachments.json")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("drive:1", "v1"); ok {
		t.Fatal("empty cache hit")
	}
	c.Put("drive:1", "v1", Entry{Content: "hello", Truncated: true})
	c.Put("drive:2", "", Entry{Content: "no version, not stored"})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}

	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := c.Get("drive:1", "v1"); !ok || e.Content != "hello" || !e.Truncated {
		t.Fatalf("reopened entry = %+v, %v", e, ok)
	}
	if _, ok := c.Get("drive:1", "v2"); ok {
		t.Error("changed version must miss")
	}
	if _, ok := c.Get("drive:2", ""); ok {
		t.Error("empty version must never hit")
	}
	if h, m := c.Stats(); h != 1 || m != 1 {
		t.Errorf("stats = %d hits, %d misses", h, m)
	}
}

func TestSavePrunesUnseen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	c, _ := Open(path)
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.Put("old", "v", Entry{Content: "x"})
	c.Put("fresh", "v", Entry{Content: "y"})

	now = now.Add(ttl + time.Hour)
	c.Get("fresh", "v") // seen again
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c, _ = Open(path)
	if _, ok := c.Get("old", "v"); ok {
		t.Error("entry unseen past ttl should be pruned")
	}
	if _, ok := c.Get("fresh", "v"); !ok {
		t.Error("recently seen entry pruned")
	}
}

func TestNilCacheAndCorruptFile(t *testing.T) {
	var c *Cache
	c.Put("k", "v", Entry{})
	if _, ok := c.Get("k", "v"); ok {
		t.Error("nil cache hit")
	}
	if err := c.Save(); err != nil {
		t.Error(err)
	}

	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	c, err := Open(path)
	if err == nil || c == nil {
		t.Fatalf("corrupt file: cache %v err %v, want empty cache and an error", c, err)
	}
	c.Put("k", "v", Entry{Content: "ok"})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentUse(t *testing.T) {
	c, _ := Open(filepath.Join(t.TempDir(), "a.json"))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%5))
			c.Put(key, "v", Entry{Content: key})
			c.Get(key, "v")
		}(i)
	}
	wg.Wait()
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}
