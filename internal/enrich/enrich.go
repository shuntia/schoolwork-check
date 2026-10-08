package enrich

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"schoolwork-check/internal/model"
)

// Options configures one Run.
type Options struct {
	// Model is recorded on each Enrichment and is part of the cache key, so
	// switching models re-enriches.
	Model string
	// MaxCalls caps model calls per run (0 = no cap). Cached tasks are free;
	// whatever the cap leaves out is picked up by the next run.
	MaxCalls int
	// Concurrency is the number of calls in flight (0 = 4).
	Concurrency int
	// CacheFile overrides DefaultCacheFile().
	CacheFile string
	Logger    *slog.Logger
}

// Stats counts what one run did.
type Stats struct {
	Cached   int // answer reused
	Enriched int // fresh answer
	Skipped  int // finished work, never sent
	Deferred int // over MaxCalls or after the provider said stop
	Failed   int // bad answers or errors
}

func (s Stats) String() string {
	return fmt.Sprintf("enrich: %d cached, %d enriched, %d skipped (done), %d deferred, %d failed",
		s.Cached, s.Enriched, s.Skipped, s.Deferred, s.Failed)
}

// Run sets Enrichment on every open task in place. It never fails the run
// over the model: provider errors are logged and those tasks go without a
// brief. The only error returned is a cache file that cannot be written.
func Run(ctx context.Context, c Completer, tasks []model.Task, o Options) (Stats, error) {
	var st Stats
	log := o.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	workers := o.Concurrency
	if workers <= 0 {
		workers = 4
	}

	path := o.CacheFile
	if path == "" {
		p, err := DefaultCacheFile()
		if err != nil {
			return st, err
		}
		path = p
	}
	cache, err := LoadCache(path)
	if err != nil {
		log.Warn("enrich: ignoring unreadable cache", "path", path, "err", err)
	}
	now := time.Now().UTC()

	// Resolve cache hits first so the call budget goes to real misses.
	type job struct {
		i      int
		prompt string
		key    string
	}
	var jobs []job
	for i := range tasks {
		t := &tasks[i]
		if finished(t.Progress.State) {
			st.Skipped++
			continue
		}
		prompt := TaskContext(*t)
		key := inputHash(o.Model, prompt)
		if e, ok := cache.Get(t.ID, key, now); ok {
			t.Enrichment = &e
			st.Cached++
			continue
		}
		jobs = append(jobs, job{i, prompt, key})
	}
	if o.MaxCalls > 0 && len(jobs) > o.MaxCalls {
		st.Deferred += len(jobs) - o.MaxCalls
		log.Info("enrich: over the per-run call cap, rest next run", "cap", o.MaxCalls, "deferred", len(jobs)-o.MaxCalls)
		for _, j := range jobs[o.MaxCalls:] {
			keepStale(cache, &tasks[j.i])
		}
		jobs = jobs[:o.MaxCalls]
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		stopped bool
		sem     = make(chan struct{}, workers)
	)
	for _, j := range jobs {
		// Take the slot first: a refusal that lands while we wait for one
		// must stop this job too.
		sem <- struct{}{}
		mu.Lock()
		if stopped || ctx.Err() != nil {
			st.Deferred++
			keepStale(cache, &tasks[j.i])
			mu.Unlock()
			<-sem
			continue
		}
		mu.Unlock()
		wg.Add(1)
		go func(j job) {
			defer func() { <-sem; wg.Done() }()
			t := &tasks[j.i]
			e, err := ask(ctx, c, j.prompt)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				e.Model = o.Model
				t.Enrichment = &e
				cache.Put(t.ID, j.key, e, now)
				st.Enriched++
				log.Debug("enrich: ok", "id", t.ID, "title", t.Title)
			case errors.Is(err, ErrStop):
				if !stopped {
					log.Warn("enrich: provider refused, stopping for this run", "err", err)
				}
				stopped = true
				st.Deferred++
				keepStale(cache, t)
			case ctx.Err() != nil:
				st.Deferred++
				keepStale(cache, t)
			default:
				st.Failed++
				keepStale(cache, t)
				log.Warn("enrich: task failed", "id", t.ID, "title", t.Title, "err", err)
			}
		}(j)
	}
	wg.Wait()

	cache.Prune(now)
	if err := SaveCache(path, cache); err != nil {
		return st, err
	}
	return st, nil
}

// ask makes one call and, if the answer does not parse, one more with the
// problem spelled out.
func ask(ctx context.Context, c Completer, prompt string) (model.Enrichment, error) {
	answer, err := c.Complete(ctx, systemPrompt, prompt)
	if err != nil {
		return model.Enrichment{}, err
	}
	e, perr := parseBrief(answer)
	if perr == nil {
		return e, nil
	}
	retry := prompt + "\n\nYour previous reply was rejected (" + perr.Error() + "). Reply with only the JSON object."
	answer, err = c.Complete(ctx, systemPrompt, retry)
	if err != nil {
		return model.Enrichment{}, err
	}
	return parseBrief(answer)
}

func keepStale(c *Cache, t *model.Task) {
	if e, ok := c.Stale(t.ID); ok {
		t.Enrichment = &e
	}
}

func finished(s model.State) bool {
	return s == model.StateSubmitted || s == model.StateGraded || s == model.StateExcused
}

// inputHash keys the cache: prompt wording, model and task text together.
func inputHash(modelID, prompt string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{systemPrompt, modelID, prompt}, "\x00")))
	return hex.EncodeToString(sum[:])
}
