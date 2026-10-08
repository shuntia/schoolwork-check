# schoolwork-check

Pulls every assignment, quiz, discussion and reading from Canvas, Google
Classroom, the course calendars your teachers keep in a Google Doc and the
meetings on a shared Google Calendar into one table, with the teacher's attachments and your own
submitted work extracted to plain text. Built to run unattended from a timer
and feed a downstream normaliser and task manager.

## Build

```sh
go build ./cmd/schoolwork-check
```

## Configure

Copy `.env.example` to `.env` and fill in what you use. Either source alone is fine.

**Canvas.** Account → Settings → New Access Token. Student tokens must expire
within 120 days (Instructure policy since Oct 2025); the tool exits with a
clear 401 message when it is time to make a new one.

**Google Classroom.** Create an OAuth client of type *Desktop app* in a Google
Cloud project, enable the Classroom and Drive APIs, set the consent screen's
publishing status to *In production* (in *Testing*, refresh tokens die after
7 days), download the client JSON to the path in `GOOGLE_CREDENTIALS_FILE`, then:

```sh
./schoolwork-check google-login
```

A browser opens once; the refresh token is cached in `GOOGLE_TOKEN_FILE`.

**Course calendars in a Google Doc.** Some teachers keep the real schedule in
a document — a Doc, a Sheet, or an uploaded PDF — and post only part of it to
the LMS. List those documents in `CALENDAR_DOCS` and each is read into your task list
(and re-read when it changes, at most weekly):

```sh
CALENDAR_DOCS=https://docs.google.com/document/d/1AbC.../edit|Biology, 1XyZ...|AP Gov
```

Entries are comma- or newline-separated; each is the document's URL (or bare
file id) and optionally `|Course Name`, which defaults to the document's own
name. They use the Google login above — `drive.readonly` is already consented
to, so no second `google-login` — and any document the account can open works,
including one a teacher only shared by link. Open it once in the browser as
that account if Drive answers 403.

The document is exported to text (Docs as Markdown, so the calendar's tables
keep their rows; Sheets as CSV; uploads through the same extractor as
attachments), then the model reads the schedule into dated rows, several
pages at a time in parallel. Google inlines every image in a Markdown export
as base64, which is most of the file — a real classroom calendar exports as
689 KB of which 17 KB is words — so the image data is stripped before the
size limit is applied, and the limit never falls on the calendar itself.
**The model decides what each row is:** a row that asks something of you
becomes an assignment or quiz task, while a row that is only information — a
reading, "no school", a unit change — goes to note's agent inbox as a fact,
the same route materials and announcements take. Rows are filtered by
`PAST_DAYS`/`FUTURE_DAYS` like everything else.

Parsing is an extraction, not a brief, so it uses the `LLM_*` settings below
whatever `BRIEF` is set to. With no LLM key the document is not parsed — it is
handed to note whole, as one piece of information, so the calendar is never
silently dropped.

A whole semester of rows is more text than an attachment ever is, so calendars
have their own size cap, `CALENDAR_MAX_TEXT` (512 KiB), rather than the much
smaller `MAX_EXTRACTED_TEXT`. They also get their own patience: reading a
schedule streams back far more JSON than a brief does, so the calendar client
waits up to ten minutes per request where a brief waits three.
`CALENDAR_MODEL` points calendars at a faster model than `LLM_MODEL` without
changing who writes the briefs.

Reading a calendar is transcription, not judgement, so these calls ask the
provider to skip its chain of thought. On a real 8 KB chunk that is the
difference between 375 seconds and 58 seconds, and between $0.0038 and
$0.0007, for the same 35 rows: the model was spending 13,934 reasoning tokens
to copy a table. Briefs keep their reasoning. The whole eight-page calendar,
parse and duplicate check included, runs in about two minutes.

**Re-read at most once a week (`CALENDAR_INTERVAL`, default `7d`).** A long
calendar is edited constantly and read rarely, and every read costs model
calls — an eight-page document is several. So a parse is kept until the
document changes *and* the interval is up; until then the run costs one
Drive metadata call and does not even download the text. `CALENDAR_INTERVAL=0`
re-reads on every edit, and a third field on an entry overrides it for one
document (`…/edit|Biology|14d`).

**Calendars already posted in the LMS (`CALENDAR_MATERIALS`, default on).**
Teachers post the same schedule to Classroom or Canvas and name it plainly.
Any material whose title contains "calendar" is read into rows the same way,
instead of arriving as eight pages of text in the inbox — no configuration,
and a material pointing at a document already in `CALENDAR_DOCS` is left to
that entry so it is not read twice. Items with "calendar" in the title that
are *work* (a quiz about calendars) are never touched.

**Duplicates (`CALENDAR_DEDUP`, default on).** A calendar row and an LMS
assignment are routinely the same work under two names — "Lab write-up" and
"Osmosis Lab Report — Unit 2", dated a couple of days apart. Rows whose date
is within five days of an existing task and which share a word with it are put
to the model, in batches, and a row it calls the same is dropped. Only
calendar rows are ever dropped: the Canvas or Classroom copy has the id,
points and submission state, so it always wins. Verdicts are cached by the
pair's own text, so a pair is judged once. When the model cannot be reached,
nothing is dropped.

A row's id is its date plus its title, since a calendar has no ids of its own.
Reword a row in the document and note sees a new task; the old one stays until
you drop it.

**Shared Google Calendars.** The meetings — a club, a team, a tutor, the
school's own events calendar — usually live on a calendar shared with you, not
in the LMS. List them in `GOOGLE_CALENDARS`, pasting whichever address the
*Integrate calendar* panel gave you; the transport follows from the URL:

```sh
# A published .ics feed: no login, no OAuth scope, no Cloud project.
GOOGLE_CALENDARS=https://calendar.google.com/calendar/ical/c_0123...%40group.calendar.google.com/public/basic.ics|Club Meeting
# A calendar id or CalDAV URL: read through the API, needs the Google login.
GOOGLE_CALENDARS=c_0123...@group.calendar.google.com|Meetings
```

Prefer the `.ics` address. It is read with a plain HTTPS GET, so it works
without `google-login`, without the calendar scope, and without enabling the
Calendar API on the Cloud project — the whole class of problem that makes the
API path awkward to set up. A secret address (`…/private-<hex>/basic.ics`)
works the same way and shows event details without publishing the calendar;
treat it as a password. The one thing a feed cannot do is expand recurrence
rules, so an event carrying an `RRULE` arrives as its first occurrence with
the rule quoted in its text. Calendars written by an app — which is most
shared ones — list every occurrence separately and lose nothing.

Events go to **note's calendar**, not its task list and not its agent inbox: a
meeting has a time and a place and nothing to hand in, so it belongs on the
dayline. Each sitting becomes one entry through
`PUT /api/calendar/by-external/{external_id}`, `kind: "busy"`, on the day it
happens. Cancelled events and meetings you have declined are skipped, and the
window is the usual `PAST_DAYS`/`FUTURE_DAYS`.

Because the entries carry our own id, a second run recognises them: an
unchanged meeting costs nothing, a meeting moved to another hour is rewritten
in place rather than duplicated, and a meeting that disappears upstream is
withdrawn from note. note keeps no tombstones, so an entry **you** delete by
hand is remembered as declined in the sync state and never written again —
the same bargain tasks have. `NOTE_MAX_CALENDAR` (60) caps how many of note's
100 entries this tool may occupy, so your own entries always have room, and
`NOTE_CALENDAR=false` turns the whole thing off. If note has no calendar
route — an older build — events wait for the next run instead of being
redirected into the agent inbox, which would spend a model call per meeting.

If note ever refuses the list route to an API token — it did until
`main c283319` — entries are pushed *blind* instead: they still land, a moved
meeting is still rewritten in place and a cancelled one still withdrawn, but
an entry you delete by hand cannot be noticed, so it stays deleted rather than
being remembered as declined. The run is never failed over it; the task half
authenticates on its own routes.

Entries are written with `quiet: false`. note makes a `busy` entry quiet by
default on every write path, and importing someone else's calendar should not
quietly stop your notifications for nineteen hours of the term.
`NOTE_CALENDAR_QUIET=true` asks for the other behaviour.

A calendar published as free/busy hides every title behind "Busy". The label
after the `|` is what such events are called instead, so `|Club Meeting`
turns nineteen "Busy" blocks into one named appointment. An event that has a
real title of its own keeps it.

The API path — and only that path — needs a scope the earlier logins did not
ask for, so run `google-login` once more **with a build that includes this
feature** — a login
only ever grants the scopes the binary asks for, so re-running an older build
changes nothing, which is the confusing part. On NixOS, where the package is
built from the committed tip of master, that means commit, `nix flake update
schoolwork-check`, rebuild, and only then log in. The Google Calendar API also
has to be enabled in the Cloud project the OAuth client belongs to. Until both
are true this source fails with a message naming which of the two is missing,
and the other sources carry on.

**Local LLM brief (`BRIEF=local`).** Put an OpenRouter (or any OpenAI-compatible) key in
`~/.config/schoolwork-check/llm-api-key` (mode 600). Each open task then gets
a summary, what to hand in, steps, stated requirements and a time estimate,
written by `deepseek/deepseek-v4-flash` by default (`LLM_MODEL`,
`LLM_BASE_URL`). Only the teacher's text and attachments are sent, never
your own submissions. Answers are cached by input hash in
`~/.local/state/schoolwork-check/enrich-cache.json`, so a task costs one call
(about $0.0002) until the teacher edits it. Finished work is never sent. A
rate limit or provider outage costs only the brief, never the run. The model
also marks items that ask nothing of you (optional Q&A forums,
announcements); `push` does not create those unless you pass
`--include-non-homework`, and a task already in note is never hidden.

**Let note brief (default).** With `BRIEF=note`, `push` skips the local model and
hands each new task, and each task whose teacher text changed, to note's own
agent (`POST /api/tasks/{id}/agent`, note's `import` prompt). That agent owns
the description, time estimate and steps, and drops non-homework (never
recreated). schoolwork-check keeps the title, the state, and notes, which
then hold the LMS text. Calls are sequential, capped by `NOTE_MAX_BRIEFS`,
and a failed call is retried on the next run.

**Information goes to note's inbox.** Materials (readings, calendars, study
guides, rubrics) and announcements are not tasks. With `BRIEF=note`, `push`
sends each to note's agent inbox (`POST /api/agent/inbox`), which saves the
durable facts (quiz dates, join codes, schedule changes) to note's memory or
ignores the item. If the agent answers that it is really work (a study guide
for an upcoming quiz), it becomes a normal task and is briefed. Items are sent
once and again only when their text changes; an item that is already a task in
note stays a task. Classroom announcements need the announcements scope: rerun
`schoolwork-check google-login` once after upgrading.

## Run

```sh
./schoolwork-check fetch --format md --out tasks.md
./schoolwork-check fetch --format json > tasks.json
./schoolwork-check fetch --sources canvas --no-extract --no-enrich
./schoolwork-check fetch --sources gdoc --format md     # just the document calendars
./schoolwork-check fetch --sources gcal --format md     # just the meetings
./schoolwork-check push --dry-run
```

## NixOS

The flake exports a module that runs `push` on a timer as its own user, with
tokens handed over through systemd `LoadCredential` (never the Nix store) and
sync state, caches and the Google refresh token under
`/var/lib/schoolwork-check`:

```nix
inputs.schoolwork-check.url = "github:<you>/schoolwork-check";

imports = [ inputs.schoolwork-check.nixosModules.default ];

services.schoolwork-check = {
  enable = true;
  settings.CANVAS_BASE_URL = "https://school.instructure.com";
  environmentFile = "/run/secrets/schoolwork-check/env";  # CALENDAR_DOCS, GOOGLE_CALENDARS, ...
  credentials = {
    canvasToken       = "/run/secrets/schoolwork-check/canvas-token";
    noteToken         = "/run/secrets/schoolwork-check/note-token";
    llmApiKey         = "/run/secrets/schoolwork-check/llm-api-key";
    googleCredentials = "/run/secrets/schoolwork-check/google-credentials.json";
  };
};
```

`schoolwork-check-ctl` runs the CLI inside the same sandbox, credentials
included: `sudo schoolwork-check-ctl google-login`, `sudo schoolwork-check-ctl
push --dry-run`. On an impermanent root, persist `/var/lib/schoolwork-check`.

## Pushing into note

`push` mirrors the table into note, creating one task per
assignment and patching it in place on later runs. It fetches first, or reads a
table you already have with `--from tasks.json`. Set `NOTE_TOKEN` (mint one
under *Settings > API tokens* in note); `--dry-run` shows what it would send.
Finished homework is skipped unless you pass `--include-done`, non-homework
unless `--include-non-homework`, and a task you
drop or delete in note is never recreated. See `docs/note-sink.md` for the
field mapping and the sync state file.

## The table

One row per task; see `internal/model/task.go` for the exact schema.

| Column | Meaning |
|---|---|
| `id` | stable `<source>:<native id>`, safe to dedup on across runs |
| `source`, `kind` | `canvas`/`classroom`/`gdoc`/`gcal`; assignment, quiz, discussion, material, question, announcement |
| `course`, `title`, `url` | as shown in the LMS |
| `assigned_at`, `due_at`, `points` | nil when the LMS has none |
| `description` | plain text, HTML stripped, list structure kept |
| `attachments[]` | teacher files and links, with `content` extracted from PDF, DOCX, PPTX, text, HTML and Google Docs/Sheets/Slides |
| `enrichment` | LLM brief: `summary`, `deliverable`, `steps[]`, `requirements[]`, `estimate_min`, `model`; absent for finished work or without a key |
| `progress` | `state` (not_started, in_progress, submitted, graded, missing, excused), `grade`, `submitted_at`, your typed `text`, and your submitted `attachments[]` with content extracted |

Extracted attachment text is cached in
`~/.local/state/schoolwork-check/attachments.json`, keyed by file id and the
version the LMS reports (Canvas `updated_at`, Drive `version`/`modifiedTime`).
A file is downloaded again only when it changes; failed downloads are not
cached and retry next run.

Output formats: `json` (default, for machines), `md` (table plus per-task
details), `csv`.

## Layout

```
cmd/schoolwork-check   CLI
internal/model         the shared row type (the contract)
internal/canvas        Canvas REST adapter (planner items + assignments + submissions)
internal/classroom     Classroom + Drive adapter (coursework, materials, submissions)
internal/gdoc          course calendars in a Google Doc/Sheet/file, parsed into dated rows
internal/gcal          shared Google Calendars: meetings and events, as information
internal/dedup         drops a calendar row the LMS already lists under another name
internal/extract       PDF / DOCX / PPTX / HTML / text → plain text
internal/enrich        LLM brief per task (OpenAI-compatible client, input-hash cache)
internal/filecache     extracted attachment text by file version, so unchanged files are not re-downloaded
internal/output        JSON / Markdown / CSV writers
internal/note          the note sink: render, HTTP client, idempotent sync
internal/config        environment configuration
```
