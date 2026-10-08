# Pushing tasks into `note`

`note` (`~/Projects/note`) is the downstream sink. It is a Rust/axum server on
port 3271 with SQLite, one `tasks` table, and a React PWA. Mapping of its API
as of 2026-09-16, and the change request we filed, live in
`~/Projects/note/docs/superpowers/specs/2026-09-16-external-tasks-design.md`.

## What exists today

| Route | Notes |
|---|---|
| `POST /api/login` | cookie session, 30 days |
| `GET /api/tasks` | every non-dropped top-level task with steps, unpaginated |
| `PUT /api/tasks/by-external/{external_id}` | how we create: the whole task in one call, keyed by our id. 201 made it, 200 refreshed title/notes/due_at/url (description, steps and state stay the user's, except open → done), 410 the user deleted it and note keeps a tombstone so it is never made again |
| `PATCH /api/tasks/{id}` | how we update: `{title?, description?, state?, notes?, duration_min?, parent_id?, is_now?, due_at?, url?, external_id?, urgency?}`, unknown keys rejected; `due_at` or `urgency` on a step is a 422, an `external_id` another task holds is a 409 |
| `POST /api/tasks` | unused here since 2026-09-21 |
| `DELETE /api/tasks/{id}` | in the api-tokens worktree, not yet on main |
| `POST/GET/DELETE /api/tokens` | in the api-tokens worktree; bearer `note_<43 chars>` works on task routes only |

Task `state` is `open | in_progress | done | dropped`. Timestamps are RFC 3339 UTC.
`due_at` (RFC 3339, top-level tasks only), `url` and `external_id` exist since
2026-09-21 and we send all three. Attachments and tags still have no home.

Since 2026-09-25 every task row also carries `urgency` (`low | normal | high`,
default `normal`; a step reports its parent's) and `pressing` (true for a live
task that is overdue or due inside 48 hours, derived by note on read). `PUT
/api/tasks/by-external` and `PATCH` accept `urgency`; we do not send it, so the
user's setting in note stands. Both fields are additive: our decoder ignores
what it does not name, and nothing we send was renamed or removed.

## Identity and the migration

A note task is ours when its `external_id` is our task id
(`canvas:assignment:12345`, `gdoc:<doc>:<date>-<slug>`). Tasks made before
2026-09-21 have no `external_id`; they carry the `schoolwork-check-id: <id>`
sentinel on the last line of `notes` instead, and we still read it. The first
run after the upgrade stamps each such task's `external_id` through `PATCH`
(counted as "migrated" in the run summary) and note keys it from then on. The
sentinel is still written, so a note older than 2026-09-21 keeps working.

When both keys point somewhere, `external_id` wins. A task the user dropped
stays out of `GET /api/tasks` but keeps its `external_id`, so a `PUT` for it
refreshes the text and answers 200 with `state: dropped`; we count it as
declined and leave it. A task the user deleted answers 410 forever.

## Events

Anything from a calendar source (`source: "gcal"`) goes to note's calendar
rather than its task list:

| Route | Use |
|---|---|
| `GET /api/calendar` | read every entry, ours (with `external_id`) and the user's (null). Answers `{"entries": [...]}`, not the bare array the task routes use |
| `PUT /api/calendar/by-external/{external_id}` | create (201) or refresh (200) one entry; skip dates survive |
| `DELETE /api/calendar/by-external/{external_id}` | withdraw an event that is gone; 404 means already gone |

One entry per sitting: `kind: "busy"`, `quiet: false` (note defaults a busy
entry to quiet, and an import must not change notification behaviour),
`on_date` the day it happens,
`start_time`/`end_time` as `HH:MM` local, title cut to note's 80 runes.
note keeps no tombstones, so `SyncState.Calendar` records what we pushed; an
entry missing from note that we remember pushing was deleted by hand and is
marked `declined`, never rewritten.

## Mapping we use until the request lands

| schoolwork-check | note |
|---|---|
| `Course — Title` | `title` |
| due date | `due_at` (also repeated as a "Due:" line below) |
| link | `url` (also repeated as a "Link:" line below) |
| due, kind, points, estimate, link, LLM brief, description, attachment list | `description` |
| progress line, extracted attachment text (capped), `schoolwork-check-id: <id>` sentinel | `notes` |
| submitted / graded / excused | `state: done` (one-way, never reopened) |
| in_progress | `state: in_progress` |
| not_started / missing | `state: open` |
| `is_now` | never sent |

Idempotency: scan `GET /api/tasks` for the sentinel, plus a local map at
`~/.local/state/schoolwork-check/note-sync.json` so tasks the user dropped
(hidden from the list) are not recreated.

## Agent briefs (`BRIEF=note`)

| schoolwork-check | note |
|---|---|
| `Course — Title` | `title` |
| due date, link | `due_at`, `url` |
| due, kind, points, link, teacher text, attachment list, progress, your files | `notes` (then the sentinel) |
| `TaskContext`: course, kind/pts, title, description, attachment text | body of `POST /api/tasks/{id}/agent` |
| — | `description`, `duration_min`, steps, `dropped`: written by note's agent |

The agent is called after a task is created and whenever the sha256 of that
context differs from `brief_hash` in the sync state file. Not for tasks done or
dropped in note. 502 and network errors keep the old hash so the next run
retries; 429 (a session already running, or the daily agent cap) and 503
(server-wide session capacity full) do the same and also stop briefs for the
rest of the run; 409 (the task became a step) and 422 store the hash so they are not
asked again. A 404 with an empty body is the router's (route not deployed) and
stops briefs for the run; a 404 with a JSON `{"error"}` body is the route's own
(the task was deleted after the listing) and is skipped for the next listing to
reconcile.

## Agent inbox (`BRIEF=note`)

Items whose kind is informational (`material`, `announcement`) do not become
tasks. Each is sent to `POST /api/agent/inbox` as
`{source_id, kind, context}`, where `context` is the same text as a brief plus
a `Posted:` line with the absolute post date. note's agent answers:

| outcome | schoolwork-check does |
|---|---|
| `remembered` | nothing more; note wrote facts to memory (superseding earlier ones for this source) |
| `nothing` | nothing more |
| `task` | creates the task and briefs it, and syncs it as a task from then on |

The decision and the sha256 of the context are kept under `inbox` in the sync
state file; the item is sent again only when the context changes. An item that
already exists as a note task (by sentinel or state) stays a task. Errors
follow the brief route: an empty-body 404 means the route is not deployed and
nothing is created; 429/503 stop all agent calls for the run; 422 is recorded
as `rejected` and not resent until the text changes; anything else retries next
run. Briefs and inbox calls share `NOTE_MAX_BRIEFS`.
