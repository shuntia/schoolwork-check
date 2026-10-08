// live_test.go is the one test here that talks to a model, so that the prompt
// can be checked against a realistic calendar after it is edited. It skips
// unless SWC_LIVE_LLM is set, which keeps `go test ./...` offline and free.
//
//	SWC_LIVE_LLM=1 go test ./internal/gdoc -run TestLiveParse -v
package gdoc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"schoolwork-check/internal/enrich"
	"schoolwork-check/internal/filecache"
)

const sampleCalendar = `# Biology 10 — Fall 2026 Course Calendar

| Week | Date | Topic / In class | Homework |
| --- | --- | --- | --- |
| 3 | Mon 9/21 | Cell transport notes | Read pp. 40–58, take Cornell notes |
| 3 | Wed 9/23 | Osmosis lab | Lab write-up due Fri 9/25 at 3:30pm |
| 3 | Fri 9/25 | Review | Study guide posted |
| 4 | Mon 9/28 | **QUIZ: Unit 2** (cells + transport) |  |
| 4 | Wed 9/30 | NO SCHOOL — teacher in-service |  |
| 4 | Fri 10/2 | Start Unit 3: Genetics | Bring your textbook |
| 5 | Mon 10/5 |  | Genetics problem set #1 due |
`

func TestLiveParse(t *testing.T) {
	if os.Getenv("SWC_LIVE_LLM") == "" {
		t.Skip("set SWC_LIVE_LLM=1 to call the real model")
	}
	keyFile := os.Getenv("LLM_API_KEY_FILE")
	if keyFile == "" {
		keyFile = filepath.Join(os.Getenv("HOME"), ".config", "schoolwork-check", "llm-api-key")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	model := envOr("LLM_MODEL", "deepseek/deepseek-v4-flash")
	llm := enrich.NewClient(envOr("LLM_BASE_URL", "https://openrouter.ai/api/v1"), strings.TrimSpace(string(key)), model, nil)

	cache, _ := filecache.Open(filepath.Join(t.TempDir(), "c.json"))
	src := &fakeSource{doc: doc{
		ID: "LIVE", Name: "Biology 10 — Fall 2026 Course Calendar",
		URL: "https://docs.google.com/document/d/LIVE/edit", Version: "1",
		Text: sampleCalendar,
	}}
	c := newWithSource(src, llm, Options{
		Docs: []Doc{{ID: "LIVE"}}, Model: model,
		PastDays: 30, FutureDays: 120, FileCache: cache, Location: time.UTC,
		Now: func() time.Time { return time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC) },
	})

	tasks, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, task := range tasks {
		due := ""
		if task.DueAt != nil {
			due = task.DueAt.Format("Mon 2006-01-02 15:04")
		}
		t.Logf("%-12s %-24s %-34s | %s", task.Kind, due, task.Title,
			strings.ReplaceAll(firstLines(task.Description, 1), "\n", " "))
	}
	t.Logf("%d tasks", len(tasks))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestLiveExport dumps what Drive actually gives us for a document, to see
// why a "short" calendar arrives as half a megabyte.
//
//	SWC_LIVE_DOC=<file id> go test ./internal/gdoc -run TestLiveExport -v
func TestLiveExport(t *testing.T) {
	id := os.Getenv("SWC_LIVE_DOC")
	if id == "" {
		t.Skip("set SWC_LIVE_DOC=<drive file id>")
	}
	home := os.Getenv("HOME")
	src, err := newDriveSource(context.Background(),
		home+"/.config/schoolwork-check/google-credentials.json",
		home+"/.config/schoolwork-check/google-token.json",
		4<<20, 20<<20, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	d, err := src.Meta(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("name=%q mime=%s", d.Name, d.MimeType)
	for _, mime := range []string{"text/markdown", "text/plain"} {
		text, truncated, err := src.export(context.Background(), id, []string{mime})
		if err != nil {
			t.Logf("%s: %v", mime, err)
			continue
		}
		lines := strings.Split(text, "\n")
		t.Logf("%s: %d bytes, %d lines, truncated=%v", mime, len(text), len(lines), truncated)
		var shown int
		for _, l := range lines {
			if strings.TrimSpace(l) == "" {
				continue
			}
			t.Logf("  | %s", oneLine(l, 160))
			if shown++; shown >= 12 {
				break
			}
		}
		if out := os.Getenv("SWC_LIVE_DUMP"); out != "" {
			if err := os.WriteFile(out+"."+strings.TrimPrefix(mime, "text/"), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
