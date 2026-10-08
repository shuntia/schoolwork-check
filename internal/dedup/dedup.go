// Package dedup keeps a calendar row from becoming a second copy of work that
// Canvas or Classroom already lists. The teacher's calendar says "Lab
// write-up" where Canvas says "Osmosis Lab Report — Unit 2"; the dates line
// up, and the student ends up with two tasks for one piece of work.
//
// String matching cannot see that those are the same, and matching on dates
// alone would collapse genuinely different work due the same day. So pairs
// worth considering are found cheaply — a few days apart, at least one word
// in common — and a language model decides each pair. Every verdict is cached
// by the pair's own text, so a pair is judged once and never again.
//
// Only calendar rows are ever dropped. Canvas and Classroom tasks carry ids,
// points and submission state that a calendar row does not, so the LMS copy
// is always the one that survives.
package dedup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"schoolwork-check/internal/enrich"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/model"
)

// Options configures one Run.
type Options struct {
	// Model is recorded in the cache key, so switching models re-judges.
	Model string
	// Window is how far apart two dates may be and still be the same work
	// (0 = 5 days). A calendar often dates work the day it is set, the LMS
	// the day it is due.
	Window time.Duration
	// MaxPairs caps the pairs judged by the model per run (0 = 100). What
	// is left over is kept, not dropped, and judged on the next run.
	MaxPairs int
	// FileCache holds the verdicts. Without it every run re-judges.
	FileCache *filecache.Cache
	Logger    *slog.Logger
	Now       func() time.Time
}

// Stats counts what one run did.
type Stats struct {
	Pairs      int // pairs worth judging
	Cached     int // verdict already known
	Asked      int // model calls made
	Obvious    int // identical titles, no model needed
	Duplicates int // calendar rows dropped
	Deferred   int // over MaxPairs, kept for the next run
	Failed     int // the model could not be reached; the rows were kept
}

func (s Stats) String() string {
	return fmt.Sprintf("dedup: %d calendar rows dropped as duplicates (%d pairs: %d obvious, %d cached, %d judged in %d calls, %d deferred, %d failed)",
		s.Duplicates, s.Pairs, s.Obvious, s.Cached, s.Pairs-s.Obvious-s.Cached-s.Deferred, s.Asked, s.Deferred, s.Failed)
}

// Run returns tasks with duplicated calendar rows removed. It never returns
// an error for a model failure: the rows are simply kept.
func Run(ctx context.Context, c enrich.Completer, tasks []model.Task, o Options) ([]model.Task, Stats) {
	var st Stats
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Window <= 0 {
		o.Window = 5 * 24 * time.Hour
	}
	if o.MaxPairs <= 0 {
		o.MaxPairs = 100
	}

	pairs := candidates(tasks, o.Window)
	st.Pairs = len(pairs)
	if len(pairs) == 0 {
		return tasks, st
	}

	// Identical titles need no model, and a cached verdict needs no call.
	var ask []pair
	verdict := map[string]bool{}
	for _, p := range pairs {
		switch {
		case sameTitle(p.a, p.b):
			verdict[p.key] = true
			st.Obvious++
		default:
			if e, ok := o.FileCache.Get("dedup:"+p.key, o.Model+"|d"+promptVersion); ok {
				verdict[p.key] = e.Content == "same"
				st.Cached++
				continue
			}
			ask = append(ask, p)
		}
	}
	if len(ask) > o.MaxPairs {
		st.Deferred = len(ask) - o.MaxPairs
		ask = ask[:o.MaxPairs]
	}

	done := 0
	for _, batch := range batches(ask, pairsPerCall) {
		if ctx.Err() != nil {
			st.Failed += len(batch)
			break
		}
		answers, err := judge(ctx, c, batch)
		st.Asked++
		if err != nil {
			o.Logger.Warn("dedup: the model could not judge these rows; keeping them", "pairs", len(batch), "err", err)
			st.Failed += len(batch)
			if errors.Is(err, enrich.ErrStop) {
				// Rate limited or out of credit: stop asking, and count
				// what was never attempted as waiting, not as failed.
				st.Deferred += len(ask) - (done + len(batch))
				break
			}
			done += len(batch)
			continue
		}
		done += len(batch)
		for _, p := range batch {
			same, ok := answers[p.id]
			if !ok {
				st.Failed++
				continue
			}
			verdict[p.key] = same
			content := "different"
			if same {
				content = "same"
			}
			o.FileCache.Put("dedup:"+p.key, o.Model+"|d"+promptVersion, filecache.Entry{Content: content})
		}
	}

	// Drop the calendar row of every pair judged the same. Pairs are in
	// task order, so a row dropped here cannot take another with it.
	drop := map[string]bool{}
	for _, p := range pairs {
		if !verdict[p.key] || drop[p.b.ID] || drop[p.a.ID] {
			continue
		}
		drop[p.b.ID] = true
		st.Duplicates++
		o.Logger.Info("dedup: calendar row already in the list under another name",
			"dropped", p.b.Title, "kept", p.a.Title, "kept_source", p.a.Source,
			"dropped_id", p.b.ID, "kept_id", p.a.ID)
	}
	if len(drop) == 0 {
		return tasks, st
	}
	out := make([]model.Task, 0, len(tasks))
	for _, t := range tasks {
		if !drop[t.ID] {
			out = append(out, t)
		}
	}
	return out, st
}

// pair is one comparison: b is a calendar row, a is what it may duplicate.
type pair struct {
	id   string // "p0", "p1", ... within one request
	key  string // hash of both rows' text: the cache key
	a, b model.Task
}

// candidates finds the pairs worth judging: a calendar row and another task
// within Window of it that share at least one meaningful word. Both an LMS
// task and an earlier calendar row can be the thing duplicated — a long
// calendar often lists the same work on the day it is set and the day it is
// due.
func candidates(tasks []model.Task, window time.Duration) []pair {
	order := make([]model.Task, len(tasks))
	copy(order, tasks)
	sort.SliceStable(order, func(i, j int) bool {
		// LMS tasks first, so a calendar row is always the b side.
		if (order[i].Source == model.SourceGDoc) != (order[j].Source == model.SourceGDoc) {
			return order[j].Source == model.SourceGDoc
		}
		return due(order[i]).Before(due(order[j]))
	})

	df, rare := docFreq(tasks), rareIn(len(tasks))
	var out []pair
	seen := map[string]bool{}
	for i, b := range order {
		if b.Source != model.SourceGDoc || b.DueAt == nil {
			continue
		}
		bw := words(b.Title)
		for _, a := range order[:i] {
			if a.DueAt == nil || a.ID == b.ID {
				continue
			}
			if gap := due(a).Sub(due(b)); gap > window || gap < -window {
				continue
			}
			if !overlap(bw, words(a.Title), df, rare) {
				continue
			}
			key := pairKey(a, b)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, pair{id: fmt.Sprintf("p%d", len(out)), key: key, a: a, b: b})
		}
	}
	return out
}

func due(t model.Task) time.Time {
	if t.DueAt == nil {
		return time.Time{}
	}
	return *t.DueAt
}

// pairKey identifies a pair by what the model would see, so a verdict
// survives a re-fetch but not a reworded title.
func pairKey(a, b model.Task) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		string(a.Source), norm(a.Title), due(a).Format("2006-01-02"),
		string(b.Source), norm(b.Title), due(b).Format("2006-01-02"),
	}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// stopwords carry no signal about which piece of work a title names.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"for": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"due": true, "your": true, "you": true, "is": true, "be": true,
	"assignment": true, "homework": true, "hw": true, "task": true, "work": true,
	"class": true, "period": true, "week": true, "day": true, "no": true,
}

// words is a title's meaningful words, lowercased.
func words(title string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r > 127)
	}) {
		if len(w) < 3 && !hasDigit(w) {
			continue
		}
		if stopwords[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// overlap reports whether two titles are close enough to be worth a model
// call.
//
// One shared word cannot be the test on its own: a term's calendar of film
// work has "film", "read" and "notes" in half its rows, and every such pair
// would be a model call — hundreds of them, to find a handful of duplicates.
// Nor can two shared words be required: "Lab write-up" and "Osmosis Lab
// Report — Unit 2" share exactly one, and that is the pair this whole package
// exists for.
//
// So a word counts for as much as it is rare in this particular run. A word
// in a handful of titles is distinctive enough on its own; a word in many is
// not, and needs a second. A number is always distinctive: "chapter 9" and
// "chapter 12" are not the same homework.
func overlap(a, b map[string]bool, df map[string]int, rare int) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	shared := 0
	for w := range a {
		if !b[w] {
			continue
		}
		if hasDigit(w) || df[w] <= rare {
			return true
		}
		shared++
		if shared >= 2 {
			return true
		}
	}
	return false
}

// docFreq counts how many titles each meaningful word appears in.
func docFreq(tasks []model.Task) map[string]int {
	df := map[string]int{}
	for _, t := range tasks {
		for w := range words(t.Title) {
			df[w]++
		}
	}
	return df
}

// rareIn is how many titles a word may appear in and still be distinctive:
// a twentieth of them, and never fewer than two.
func rareIn(n int) int {
	if r := n / 20; r > 2 {
		return r
	}
	return 2
}

// sameTitleWindow is how far apart two identically named rows may be and
// still be one thing. A weekly calendar grid puts one cell under
// "Tuesday/Wednesday" and the row comes out twice, a day apart; a genuinely
// recurring task repeats every seven days, outside this window, and is left
// alone.
const sameTitleWindow = 3 * 24 * time.Hour

// sameTitle is the case no model is needed for: the same words, near enough
// the same day.
func sameTitle(a, b model.Task) bool {
	if norm(a.Title) != norm(b.Title) {
		return false
	}
	gap := due(a).Sub(due(b))
	return gap < sameTitleWindow && gap > -sameTitleWindow
}

// norm strips the punctuation and spacing that make two spellings of one
// title look different.
func norm(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9', r > 127:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}

func hasDigit(s string) bool {
	return strings.ContainsAny(s, "0123456789")
}

// batches cuts pairs into request-sized groups.
func batches(ps []pair, n int) [][]pair {
	var out [][]pair
	for i := 0; i < len(ps); i += n {
		end := i + n
		if end > len(ps) {
			end = len(ps)
		}
		out = append(out, ps[i:end])
	}
	return out
}
