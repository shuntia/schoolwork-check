// Command schoolwork-check fetches every task from the configured LMS
// sources and writes a unified table.
//
// Usage:
//
//	schoolwork-check fetch [--out tasks.json] [--format json|md|csv] [--sources canvas,classroom,gdoc,gcal] [--no-enrich]
//	schoolwork-check push [--from tasks.json] [--dry-run]   # mirror into note
//	schoolwork-check google-login    # one-time OAuth consent for Google Classroom
//
// Configuration comes from environment variables (see .env.example); a
// .env file in the working directory is loaded if present.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"schoolwork-check/internal/canvas"
	"schoolwork-check/internal/classroom"
	"schoolwork-check/internal/config"
	"schoolwork-check/internal/dedup"
	"schoolwork-check/internal/enrich"
	"schoolwork-check/internal/filecache"
	"schoolwork-check/internal/gcal"
	"schoolwork-check/internal/gdoc"
	"schoolwork-check/internal/model"
	"schoolwork-check/internal/note"
	"schoolwork-check/internal/output"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	loadDotEnv(".env")

	if len(args) == 0 {
		usage()
		return errors.New("missing command")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "fetch":
		return cmdFetch(ctx, args[1:])
	case "push":
		return cmdPush(ctx, args[1:])
	case "google-login":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return classroom.Login(ctx, cfg.GoogleCredentialsFile, cfg.GoogleTokenFile)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  schoolwork-check fetch [--out FILE] [--format json|md|csv] [--sources canvas,classroom,gdoc,gcal] [--no-extract] [--no-enrich] [-v]
  schoolwork-check push [--from FILE] [--dry-run] [--include-done] [--include-non-homework] [--no-course-prefix] [--state-file PATH] [--sources canvas,classroom,gdoc,gcal] [--no-extract] [--no-enrich] [-v]
  schoolwork-check google-login
`)
}

func cmdFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	out := fs.String("out", "-", "output file, or - for stdout")
	format := fs.String("format", "json", "json, md or csv")
	sources := fs.String("sources", "", "comma-separated subset of canvas,classroom,gdoc,gcal (default: all configured)")
	noExtract := fs.Bool("no-extract", false, "do not download attachments or extract their text")
	noEnrich := fs.Bool("no-enrich", false, "do not ask the LLM for a brief on each task")
	verbose := fs.Bool("v", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := setupLogging(*verbose)
	cfg, err := loadConfig(*sources, *noExtract)
	if err != nil {
		return err
	}

	files := openFileCache(cfg, logger)
	tasks, fetchErr := fetchTasks(ctx, cfg, files, logger)
	if errors.Is(fetchErr, config.ErrNothingConfigured) {
		return fetchErr
	}
	tasks = expandCalendars(ctx, cfg, tasks, files, logger)
	tasks = dedupCalendar(ctx, cfg, tasks, files, logger)
	saveFileCache(files, logger)
	if !*noEnrich {
		if err := enrichTasks(ctx, cfg, tasks, logger); err != nil {
			return err
		}
	}

	var w io.Writer = os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		bw := bufio.NewWriter(f)
		defer bw.Flush()
		w = bw
	}
	switch *format {
	case "json":
		err = output.WriteJSON(w, tasks)
	case "md", "markdown":
		err = output.WriteMarkdown(w, tasks)
	case "csv":
		err = output.WriteCSV(w, tasks)
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
	if err != nil {
		return err
	}
	// A source failing is reported but does not discard the others' results;
	// fetchTasks only returns an error when every source failed.
	return fetchErr
}

// cmdPush mirrors the task table into note. Without --from it runs the same
// fetch as cmdFetch; with it, it reads a JSON array of model.Task (what
// `fetch --format json` writes), which is where a future normalisation stage
// between fetch and push will plug in.
func cmdPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	from := fs.String("from", "", "read tasks from this JSON file instead of fetching (- for stdin)")
	dryRun := fs.Bool("dry-run", false, "log what would be sent and send nothing")
	includeDone := fs.Bool("include-done", false, "also create tasks the LMS already marks submitted, graded or excused")
	includeNonHomework := fs.Bool("include-non-homework", false, "also create items the LLM judged ask nothing of you (optional forums, announcements)")
	noCoursePrefix := fs.Bool("no-course-prefix", false, `title is the bare assignment name, not "Course — Title"`)
	stateFile := fs.String("state-file", "", "override the sync state file")
	sources := fs.String("sources", "", "comma-separated subset of canvas,classroom,gdoc,gcal (default: all configured)")
	noExtract := fs.Bool("no-extract", false, "do not download attachments or extract their text")
	noEnrich := fs.Bool("no-enrich", false, "no brief on any task (neither BRIEF=local nor BRIEF=note)")
	verbose := fs.Bool("v", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := setupLogging(*verbose)
	cfg, err := loadConfig(*sources, *noExtract)
	if err != nil {
		return err
	}
	if !cfg.NoteEnabled() {
		return errors.New("note: set NOTE_TOKEN (mint one under Settings > API tokens in note)")
	}

	files := openFileCache(cfg, logger)
	var tasks []model.Task
	if *from != "" {
		tasks, err = readTasksFile(*from)
		if err != nil {
			return err
		}
		logger.Info("read tasks", "file", *from, "tasks", len(tasks))
	} else {
		var fetchErr error
		tasks, fetchErr = fetchTasks(ctx, cfg, files, logger)
		if fetchErr != nil {
			return fetchErr
		}
	}
	tasks = expandCalendars(ctx, cfg, tasks, files, logger)
	tasks = dedupCalendar(ctx, cfg, tasks, files, logger)
	saveFileCache(files, logger)
	// Tasks read with --from may already carry a brief; Run replaces it from
	// the cache, which costs nothing when the input is unchanged.
	if !*noEnrich {
		if err := enrichTasks(ctx, cfg, tasks, logger); err != nil {
			return err
		}
	}

	var briefContext, inboxContext func(model.Task) string
	if cfg.Brief == "note" && !*noEnrich {
		briefContext, inboxContext = enrich.TaskContext, enrich.InboxContext
	}
	if cfg.Brief == "note" {
		// note's agent judges and briefs; a local verdict carried in a
		// --from file must not pre-empt it.
		for i := range tasks {
			tasks[i].Enrichment = nil
		}
	}
	client := note.NewClient(cfg.NoteBaseURL, cfg.NoteToken, nil, logger.With("sink", "note"))
	res, err := note.Sync(ctx, client, tasks, note.Options{
		BriefContext:       briefContext,
		InboxContext:       inboxContext,
		MaxBriefs:          cfg.MaxBriefs,
		Calendar:           cfg.NoteCalendar,
		MaxCalendar:        cfg.MaxCalendarEntries,
		CalendarQuiet:      cfg.NoteCalendarQuiet,
		StateFile:          *stateFile,
		DryRun:             *dryRun,
		IncludeDone:        *includeDone,
		IncludeNonHomework: *includeNonHomework,
		CoursePrefix:       !*noCoursePrefix,
		Location:           time.Local,
		Logger:             logger.With("sink", "note"),
	})
	fmt.Fprintln(os.Stderr, res.String())
	if err != nil {
		return err
	}
	for _, e := range res.Errors {
		logger.Error("note: task failed", "err", e)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("note: %d of %d tasks failed", len(res.Errors), len(tasks))
	}
	return nil
}

// setupLogging installs a stderr logger and returns it.
func setupLogging(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	return logger
}

// loadConfig loads the environment configuration and applies the flags that
// fetch and push share.
func loadConfig(sources string, noExtract bool) (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, err
	}
	if sources != "" {
		cfg.Sources = nil
		for _, s := range strings.Split(sources, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.Sources = append(cfg.Sources, s)
			}
		}
	}
	if noExtract {
		cfg.ExtractAttachments = false
	}
	return cfg, nil
}

// openFileCache opens the cache shared by every source, by the calendar
// parse and by the duplicate check. Losing it only costs a slower, more
// expensive run, so failures are warnings and a nil cache works everywhere.
// It is opened even with --no-extract: extraction is not the only thing in
// it any more, and what is in it is what keeps an unedited calendar and an
// already-judged pair of tasks from costing model calls on every run.
func openFileCache(cfg config.Config, logger *slog.Logger) *filecache.Cache {
	path := cfg.FileCacheFile
	if path == "" {
		path, _ = filecache.DefaultPath()
	}
	if path == "" {
		return nil
	}
	files, err := filecache.Open(path)
	if err != nil {
		logger.Warn("attachment cache unreadable, starting empty", "err", err)
	}
	return files
}

func saveFileCache(files *filecache.Cache, logger *slog.Logger) {
	if err := files.Save(); err != nil {
		logger.Warn("attachment cache not saved", "err", err)
	}
}

// fetchTasks runs every configured source concurrently and returns the
// sorted union. A source that fails is logged and its results are simply
// missing; the error is non-nil only when nothing was configured or every
// source failed.
func fetchTasks(ctx context.Context, cfg config.Config, files *filecache.Cache, logger *slog.Logger) ([]model.Task, error) {
	type fetcher struct {
		name string
		fn   func(context.Context) ([]model.Task, error)
	}
	var fetchers []fetcher

	if cfg.CanvasEnabled() {
		c := canvas.New(cfg.CanvasBaseURL, cfg.CanvasToken, canvas.Options{
			ExtractAttachments: cfg.ExtractAttachments,
			MaxAttachmentBytes: cfg.MaxAttachmentBytes,
			MaxExtractedText:   cfg.MaxExtractedText,
			PastDays:           cfg.PastDays,
			FutureDays:         cfg.FutureDays,
			Logger:             logger.With("source", "canvas"),
			FileCache:          files,
		})
		fetchers = append(fetchers, fetcher{"canvas", c.Fetch})
	}
	if cfg.ClassroomEnabled() {
		c, err := classroom.New(ctx, cfg.GoogleCredentialsFile, cfg.GoogleTokenFile, classroom.Options{
			ExtractAttachments: cfg.ExtractAttachments,
			MaxAttachmentBytes: cfg.MaxAttachmentBytes,
			MaxExtractedText:   cfg.MaxExtractedText,
			PastDays:           cfg.PastDays,
			FutureDays:         cfg.FutureDays,
			Logger:             logger.With("source", "classroom"),
			FileCache:          files,
		})
		if err != nil {
			return nil, err
		}
		fetchers = append(fetchers, fetcher{"classroom", c.Fetch})
	}
	if cfg.CalendarEnabled() {
		// The calendar parse is an extraction, not a brief, so it runs on
		// the LLM settings whatever BRIEF is set to. Without a key the
		// documents still come through, whole and unparsed.
		var llm enrich.Completer
		if cfg.CalendarLLMEnabled() {
			llm = calendarLLM(cfg)
		} else {
			logger.Warn("gdoc: no LLM key, calendar documents are passed on whole instead of parsed into rows")
		}
		docs := make([]gdoc.Doc, 0, len(cfg.CalendarDocs))
		for _, d := range cfg.CalendarDocs {
			docs = append(docs, gdoc.Doc{ID: d.ID, Course: d.Course, Interval: d.Interval})
		}
		c, err := gdoc.New(ctx, cfg.GoogleCredentialsFile, cfg.GoogleTokenFile, llm, gdoc.Options{
			Docs:               docs,
			Model:              cfg.CalendarParseModel(),
			MaxExtractedText:   cfg.CalendarMaxText,
			MaxAttachmentBytes: cfg.MaxAttachmentBytes,
			PastDays:           cfg.PastDays,
			FutureDays:         cfg.FutureDays,
			Interval:           cfg.CalendarInterval,
			FileCache:          files,
			Location:           time.Local,
			Logger:             logger.With("source", "gdoc"),
		})
		if err != nil {
			return nil, err
		}
		fetchers = append(fetchers, fetcher{"gdoc", c.Fetch})
	}
	if cfg.CalendarsEnabled() {
		feeds := make([]gcal.Feed, 0, len(cfg.GoogleCalendars))
		for _, f := range cfg.GoogleCalendars {
			feeds = append(feeds, gcal.Feed{ID: f.ID, Label: f.Label, ICSURL: f.ICSURL})
		}
		c, err := gcal.New(ctx, cfg.GoogleCredentialsFile, cfg.GoogleTokenFile, gcal.Options{
			Feeds:      feeds,
			PastDays:   cfg.PastDays,
			FutureDays: cfg.FutureDays,
			Location:   time.Local,
			Logger:     logger.With("source", "gcal"),
		})
		if err != nil {
			return nil, err
		}
		fetchers = append(fetchers, fetcher{"gcal", c.Fetch})
	}
	if len(fetchers) == 0 {
		return nil, config.ErrNothingConfigured
	}

	start := time.Now()
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		tasks []model.Task
		errs  []error
	)
	for _, f := range fetchers {
		wg.Add(1)
		go func(f fetcher) {
			defer wg.Done()
			ts, err := f.fn(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", f.name, err))
			}
			tasks = append(tasks, ts...)
		}(f)
	}
	wg.Wait()

	for _, e := range errs {
		logger.Error("source failed", "err", e)
	}
	hits, misses := files.Stats()
	logger.Info("fetch complete", "tasks", len(tasks), "sources", len(fetchers), "failed", len(errs),
		"files_cached", hits, "files_fetched", misses, "took", time.Since(start).Round(time.Millisecond))

	output.Sort(tasks)
	if len(errs) == len(fetchers) {
		return tasks, errors.Join(errs...)
	}
	return tasks, nil
}

// calendarLLM is the model client the calendar parse uses. Reading a
// schedule streams back far more JSON than a brief does, so it gets a longer
// patience than the shared default; a run that takes ten minutes once a week
// is fine, a calendar that never parses is not.
func calendarLLM(cfg config.Config) *enrich.Client {
	return enrich.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.CalendarParseModel(),
		&http.Client{Timeout: 10 * time.Minute}).NoReasoning()
}

// expandCalendars turns LMS materials that are really calendars into their
// rows. It runs before the duplicate check, so rows it produces are checked
// against the LMS assignments they may repeat.
func expandCalendars(ctx context.Context, cfg config.Config, tasks []model.Task, files *filecache.Cache, logger *slog.Logger) []model.Task {
	if !cfg.CalendarMaterials || !cfg.CalendarLLMEnabled() {
		return tasks
	}
	skip := make([]string, 0, len(cfg.CalendarDocs))
	for _, d := range cfg.CalendarDocs {
		skip = append(skip, d.ID)
	}
	parser := gdoc.NewParser(calendarLLM(cfg), gdoc.Options{
		Model:      cfg.CalendarParseModel(),
		PastDays:   cfg.PastDays,
		FutureDays: cfg.FutureDays,
		Interval:   cfg.CalendarInterval,
		SkipDocIDs: skip,
		FileCache:  files,
		Location:   time.Local,
		Logger:     logger.With("stage", "calendars"),
	})
	out, st := parser.Expand(ctx, tasks)
	if st.Found > 0 {
		logger.Info(st.String(), "model", cfg.CalendarParseModel())
	}
	output.Sort(out) // rows land where the material was; put them in date order
	return out
}

// dedupCalendar drops calendar rows that are work Canvas or Classroom already
// lists under a different name. It runs before the brief, so a row that is
// dropped is never briefed or pushed. A model failure costs nothing but the
// check: the rows are kept.
func dedupCalendar(ctx context.Context, cfg config.Config, tasks []model.Task, files *filecache.Cache, logger *slog.Logger) []model.Task {
	if !cfg.CalendarDedup {
		return tasks
	}
	var calendar bool
	for _, t := range tasks {
		if t.Source == model.SourceGDoc {
			calendar = true
			break
		}
	}
	if !calendar {
		return tasks
	}
	if !cfg.CalendarLLMEnabled() {
		logger.Warn("dedup: no LLM key, calendar rows are not checked against Canvas and Classroom")
		return tasks
	}
	client := enrich.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, nil).NoReasoning()
	out, st := dedup.Run(ctx, client, tasks, dedup.Options{
		Model:     cfg.LLMModel,
		FileCache: files,
		Logger:    logger.With("stage", "dedup"),
	})
	if st.Pairs > 0 {
		logger.Info(st.String(), "model", cfg.LLMModel)
	}
	return out
}

// enrichTasks adds the LLM brief in place. Without a key it is a no-op, and a
// provider failure only costs the brief: the error returned is limited to a
// cache file that cannot be written.
func enrichTasks(ctx context.Context, cfg config.Config, tasks []model.Task, logger *slog.Logger) error {
	if !cfg.EnrichEnabled() {
		logger.Debug("enrich: local brief off (BRIEF is not local, no LLM key, or ENRICH=false)", "brief", cfg.Brief)
		return nil
	}
	start := time.Now()
	client := enrich.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, nil)
	st, err := enrich.Run(ctx, client, tasks, enrich.Options{
		Model:     cfg.LLMModel,
		MaxCalls:  cfg.LLMMaxCalls,
		CacheFile: cfg.LLMCacheFile,
		Logger:    logger.With("stage", "enrich"),
	})
	logger.Info(st.String(), "model", cfg.LLMModel, "took", time.Since(start).Round(time.Millisecond))
	return err
}

// readTasksFile reads a JSON array of model.Task, as written by
// `fetch --format json`. "-" reads stdin.
func readTasksFile(path string) ([]model.Task, error) {
	r := io.Reader(os.Stdin)
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = bufio.NewReader(f)
	}
	var tasks []model.Task
	if err := json.NewDecoder(r).Decode(&tasks); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return tasks, nil
}

// loadDotEnv sets KEY=VALUE lines from path into the environment without
// overriding variables that are already set. Missing file is fine.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}
