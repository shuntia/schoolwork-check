# Package contract

All packages live under `internal/`. Ownership during initial implementation:

| Package | Owner | Exposes |
|---|---|---|
| `model` | shared, frozen | `Task`, `Attachment`, `Progress`, enums |
| `config` | shared | `Config`, `Load()` |
| `extract` | extraction agent | `Text(ctx, Input) (Result, error)`, `HTMLToText(string) string` (assign the package-level vars, or replace them with funcs of the same signature) |
| `canvas` | Canvas agent | `New(baseURL, token string, opts Options) *Client`, `(*Client).Fetch(ctx) ([]model.Task, error)` |
| `classroom` | Classroom agent | `New(ctx, credentialsFile, tokenFile string, opts Options) (*Client, error)`, `(*Client).Fetch(ctx) ([]model.Task, error)`, `Login(ctx, credentialsFile, tokenFile string) error`, `HTTPClient(ctx, credentialsFile, tokenFile string, log *slog.Logger) (*http.Client, error)` (the shared Google login) |
| `gdoc` | Classroom agent | `New(ctx, credentialsFile, tokenFile string, llm enrich.Completer, opts Options) (*Client, error)`, `(*Client).Fetch(ctx) ([]model.Task, error)`, `NewParser(llm, opts) *Client`, `(*Client).Expand(ctx, []model.Task) ([]model.Task, ExpandStats)` — course calendars kept in a document, parsed into dated rows |
| `gcal` | Classroom agent | `New(ctx, credentialsFile, tokenFile string, opts Options) (*Client, error)`, `(*Client).Fetch(ctx) ([]model.Task, error)` — shared Google Calendars, read from a published .ics feed or the API; events are informational |
| `dedup` | integration | `Run(ctx, enrich.Completer, []model.Task, Options) ([]model.Task, Stats)` — drops calendar rows the LMS already lists |
| `output` | extraction agent | `WriteJSON(w, []Task)`, `WriteMarkdown(w, []Task)`, `WriteCSV(w, []Task)` |
| `cmd/schoolwork-check` | integration | CLI wiring |

Rules:
- Only stdlib plus these deps: `golang.org/x/oauth2`, `google.golang.org/api`, `golang.org/x/net/html`, and one pure-Go PDF text extractor (`github.com/ledongthuc/pdf` or equivalent). DOCX via `archive/zip` + `encoding/xml`.
- No global state, no panics on bad data: a broken attachment becomes `ContentError`, a broken task is logged and skipped.
- Every HTTP call takes a `context.Context`. Respect `Options.MaxAttachmentBytes`, `Options.MaxExtractedText`, `Options.ExtractAttachments`, `Options.PastDays`, `Options.FutureDays` where relevant. Options structs are defined per package with exactly those fields (and `Logger *slog.Logger`).
- `go vet ./...` and `go build ./...` must pass. Unit tests use recorded fixtures, never the network.
