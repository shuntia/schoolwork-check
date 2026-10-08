#!/usr/bin/env python3
"""A stand-in for note's task API that records everything sent to it.

Point schoolwork-check at it instead of a real note server to see exactly
what a push would write, without touching real data:

    python3 tools/fake_note.py --dir /tmp/pass --port 3999 &
    NOTE_BASE_URL=http://127.0.0.1:3999 schoolwork-check push --from tasks.json \\
        --state-file /tmp/pass/note-sync.json

It implements the routes schoolwork-check uses, with note's status codes and
limits (as of note's share-links branch, 2026-09-25):

    GET    /api/tasks              top-level, non-dropped tasks with "children"; every row carries
                                   urgency (low | normal | high, a step reports its parent's) and
                                   pressing (a live task overdue or due inside 48 h)
    PUT    /api/tasks/by-external/{external_id}
                                   note's NewTask body; 201 made, 200 refreshed (title, notes,
                                   due_at, url; state only open -> done), 410 deleted by the user
    DELETE /api/tasks/by-external/{external_id}
    POST   /api/tasks              note's NewTask body
    PATCH  /api/tasks/{id}         {title?, description?, state?, notes?, duration_min?, parent_id?,
                                   is_now?, due_at?, url?, external_id?, urgency?}; urgency on a step is a 422
    DELETE /api/tasks/{id}         buries the task's external_id, so a later PUT for it is a 410
    POST   /api/agent/inbox        {source_id, kind, context}: recorded, answered "nothing"
    POST   /api/tasks/{id}/agent   {context?}: with --agent record (default) only recorded
                                   and answered "unchanged"; with --agent llm, briefed the
                                   way note does it (below)

--agent llm mirrors note's import session: system prompt = note's import.md,
user message = the task fields plus the context, tools = task_update and
task_split only, scoped to that task (no Now, drop only an open task, split
only a task without steps), at most 8 model rounds. It calls an
OpenAI-compatible endpoint (default OpenRouter, deepseek/deepseek-v4-flash)
with the key in --key-file. A failed session restores the task and answers 502.

Files written under --dir:
    requests.jsonl  one line per request: time, method, path, body, status, response
    tasks.json      the task table after every write, so later runs continue from it

Any bearer token starting with "note_" is accepted; tokens are never logged.
Standard library only.
"""

import argparse
import copy
import json
import urllib.parse
import os
import re
import threading
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_TITLE = 500
MAX_TEXT = 16 * 1024
MAX_CONTEXT = 32 * 1024
STATES = {"open", "in_progress", "done", "dropped"}
URGENCY = {"low", "normal", "high"}
PRESSING_HOURS = 48
CREATE_KEYS = {"title", "description", "notes", "duration_min", "parent_id", "is_now", "state", "due_at", "url",
               "external_id", "source", "notify", "urgency"}
PATCH_KEYS = {"title", "description", "state", "notes", "duration_min", "parent_id", "is_now", "due_at", "url",
              "external_id", "urgency"}

lock = threading.Lock()

TOOLS = [
    {"type": "function", "function": {
        "name": "task_update",
        "description": "Update a task's title, description, state, notes, duration (whole 5-minute blocks), "
                       "or whether it sits in Now — the short list of at most 3, where a fourth pushes the "
                       "newest one back to Later. Steps are never in Now.",
        "parameters": {"type": "object", "required": ["task_id"], "properties": {
            "task_id": {"type": "integer"},
            "title": {"type": ["string", "null"]},
            "description": {"type": ["string", "null"]},
            "state": {"type": ["string", "null"], "description": "One of open, in_progress, done, dropped."},
            "notes": {"type": ["string", "null"]},
            "duration_min": {"type": ["integer", "null"], "description": "Rough estimate in whole 5-minute blocks."},
            "is_now": {"type": ["boolean", "null"], "description": "True moves the task into Now, false moves it back to Later."},
        }}}},
    {"type": "function", "function": {
        "name": "task_split",
        "description": "Break the existing task with this id into 2-5 short steps, each with a duration in "
                       "whole 5-minute blocks. The steps land under that task; this is not the way to create "
                       "new tasks. Only for a task that has no steps yet.",
        "parameters": {"type": "object", "required": ["task_id", "steps"], "properties": {
            "task_id": {"type": "integer"},
            "steps": {"type": "array", "description": "2 to 5 steps, each with a duration in whole 5-minute blocks.",
                      "items": {"type": "object", "required": ["title", "duration_min"], "properties": {
                          "title": {"type": "string"}, "duration_min": {"type": "integer"}}}},
        }}}},
]


class Agent:
    """note's import session, reimplemented against an OpenAI-compatible API."""

    def __init__(self, store, prompt_path, key, base_url, model):
        self.store = store
        with open(os.path.expanduser(prompt_path)) as f:
            self.system = f.read()
        self.key, self.base_url, self.model = key, base_url.rstrip("/"), model

    def chat(self, messages):
        body = json.dumps({"model": self.model, "messages": [{"role": "system", "content": self.system}] + messages,
                           "tools": TOOLS, "temperature": 0.2}).encode()
        req = urllib.request.Request(self.base_url + "/chat/completions", data=body, headers={
            "Authorization": "Bearer " + self.key, "Content-Type": "application/json", "X-Title": "fake-note"})
        with urllib.request.urlopen(req, timeout=180) as r:
            return json.load(r)["choices"][0]["message"]

    def message(self, t, context):
        s = self.store
        m = "Task id: %d\nTitle: %s\nState: %s\nDescription: %s\nNotes: %s\n" % (
            t["id"], t["title"], t["state"], t["description"], t["notes"])
        kids = s.node(t)["children"]
        if kids:
            m += "Steps:\n" + "".join("- %s\n" % c["title"] for c in kids)
        if context.strip():
            m += "\nContext:\n" + context + "\n"
        return m

    def tool(self, t, name, args):
        s = self.store
        if args.get("task_id") != t["id"]:
            return {"kind": "rejected", "message": "this session can only touch task %d" % t["id"]}, True
        if name == "task_update":
            if args.get("is_now") is not None:
                return {"kind": "rejected", "message": "this session cannot move a task in or out of Now"}, True
            st = args.get("state")
            if st is not None and st not in STATES:
                return {"kind": "invalid", "message": "invalid state"}, True
            if st == "dropped" and t["state"] != "open":
                return {"kind": "rejected", "message": "only an open task can be dropped"}, True
            for k in ("title", "description", "state", "notes"):
                if args.get(k) is not None:
                    t[k] = args[k]
            if args.get("duration_min") is not None:
                t["duration_min"], t["duration_source"] = int(args["duration_min"]), "agent"
            t["updated_at"] = now()
            return {"task_id": t["id"], "ok": True}, False
        if name == "task_split":
            if s.node(t)["children"]:
                return {"kind": "rejected", "message": "task already has steps"}, True
            steps = args.get("steps") or []
            if not 2 <= len(steps) <= 5:
                return {"kind": "invalid", "message": "2 to 5 steps"}, True
            ids = []
            for st in steps:
                c = {"id": s.next_id, "title": st["title"], "description": "", "state": "open", "source": "manual",
                     "notes": "", "duration_min": st.get("duration_min"), "duration_source": "agent",
                     "parent_id": t["id"], "is_now": False, "updated_at": now()}
                s.next_id += 1
                s.tasks[c["id"]] = c
                ids.append(c["id"])
            t["duration_min"] = sum(st.get("duration_min") or 0 for st in steps) or t["duration_min"]
            t["duration_source"] = "agent"
            return {"task_id": t["id"], "step_ids": ids}, False
        return {"kind": "unknown_tool", "message": name}, True

    def brief(self, t, context):
        messages = [{"role": "user", "content": self.message(t, context)}]
        steps, ok_calls = [], 0
        for _ in range(8):
            msg = self.chat(messages)
            calls = msg.get("tool_calls") or []
            if not calls:
                break
            messages.append({"role": "assistant", "content": msg.get("content") or "", "tool_calls": calls})
            for call in calls:
                name = call["function"]["name"]
                try:
                    args = json.loads(call["function"]["arguments"] or "{}")
                    result, is_error = self.tool(t, name, args)
                except (ValueError, KeyError, TypeError) as e:
                    args, result, is_error = None, {"kind": "invalid", "message": str(e)}, True
                ok_calls += 0 if is_error else 1
                steps.append({"name": name, "args": args, "result": json.dumps(result), "is_error": is_error})
                messages.append({"role": "tool", "tool_call_id": call["id"], "content": json.dumps(result)})
        if t["state"] == "dropped":
            outcome = "dropped"
        elif ok_calls:
            outcome = "briefed"
        else:
            outcome = "unchanged"
        return outcome, steps


CALENDAR_KEYS = {"title", "kind", "quiet", "start_time", "end_time", "days",
                 "on_date", "from_date", "until_date"}
CALENDAR_KINDS = ("fixed", "busy", "note", "free")
MAX_CAL_TITLE = 80
MAX_ENTRIES = 100


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class Store:
    def __init__(self, directory):
        self.dir = directory
        self.path = os.path.join(directory, "tasks.json")
        self.log_path = os.path.join(directory, "requests.jsonl")
        self.tasks = {}
        self.next_id = 1
        self.calendar = {}
        self.next_cal_id = 1
        # external_id -> deleted_at, as note's task_tombstones table.
        self.tombstones = {}
        if os.path.exists(self.path):
            with open(self.path) as f:
                data = json.load(f)
            self.tasks = {t["id"]: t for t in data["tasks"]}
            self.next_id = data["next_id"]
            self.calendar = {e["id"]: e for e in data.get("calendar", [])}
            self.next_cal_id = data.get("next_cal_id", 1)
            self.tombstones = data.get("tombstones", {})

    def save(self):
        tmp = self.path + ".tmp"
        with open(tmp, "w") as f:
            json.dump({"next_id": self.next_id,
                       "tasks": sorted(self.tasks.values(), key=lambda t: t["id"]),
                       "next_cal_id": self.next_cal_id,
                       "calendar": sorted(self.calendar.values(), key=lambda e: e["id"]),
                       "tombstones": self.tombstones}, f, indent=1)
        os.replace(tmp, self.path)

    def log(self, entry):
        with open(self.log_path, "a") as f:
            f.write(json.dumps(entry, ensure_ascii=False) + "\n")

    def row(self, t, parent=None):
        """A task as note renders it: a step reports its parent's urgency, and
        `pressing` is derived on read for a live task overdue or due inside 48 h."""
        urgency = (parent or t).get("urgency") or "normal"
        due = t.get("due_at")
        pressing = False
        if t["state"] in ("open", "in_progress") and due:
            try:
                due_at = datetime.fromisoformat(due.replace("Z", "+00:00"))
                pressing = due_at < datetime.now(timezone.utc) + timedelta(hours=PRESSING_HOURS)
            except ValueError:
                pressing = False
        return dict(t, urgency=urgency, pressing=pressing)

    def node(self, t):
        children = [c for c in self.tasks.values() if c["parent_id"] == t["id"] and c["state"] != "dropped"]
        return dict(self.row(t), children=[self.row(c, t) for c in sorted(children, key=lambda c: c["id"])])

    def new_task(self, body, source):
        """Builds and stores a task from a NewTask body; returns (task, None) or (None, (status, error))."""
        if body.get("state", "open") not in STATES:
            return None, (400, None)
        if body.get("parent_id") is not None and body.get("due_at") is not None:
            return None, (422, {"error": "a step carries no due date of its own; the task it belongs to holds it"})
        if body.get("urgency") is not None and body["urgency"] not in URGENCY:
            return None, (422, {"error": "urgency must be one of low, normal, high"})
        if body.get("parent_id") is not None and body.get("urgency") is not None:
            return None, (422, {"error": "a step reads its parent's urgency"})
        ext = body.get("external_id")
        if ext and self.by_external(ext) is not None:
            return None, (409, {"error": "external_id %s already belongs to task %d" % (ext, self.by_external(ext)["id"])})
        t = {"id": self.next_id, "title": body["title"], "description": body.get("description") or "",
             "state": body.get("state") or "open", "source": body.get("source") or source,
             "notes": body.get("notes") or "", "duration_min": body.get("duration_min"),
             "duration_source": "user" if body.get("duration_min") is not None else "none",
             "parent_id": body.get("parent_id"), "is_now": bool(body.get("is_now")), "updated_at": now(),
             "due_at": body.get("due_at"), "url": body.get("url") or "", "external_id": ext,
             "urgency": body.get("urgency") or "normal"}
        self.next_id += 1
        self.tasks[t["id"]] = t
        return t, None

    def by_external(self, ext):
        return next((t for t in self.tasks.values() if t.get("external_id") == ext), None)

    def delete_task(self, tid):
        """Removes a task and its steps, burying its external id as note does."""
        for c in [c for c in self.tasks.values() if c["parent_id"] == tid]:
            del self.tasks[c["id"]]
        t = self.tasks.pop(tid)
        if t.get("external_id"):
            self.tombstones[t["external_id"]] = now()


class Handler(BaseHTTPRequestHandler):
    store = None
    agent = None
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # quiet; requests.jsonl is the log
        pass

    def reply(self, status, body=None):
        raw = b"" if body is None else json.dumps(body).encode()
        self.send_response(status)
        if body is not None:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
        return status, body

    def handle_any(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        body, parse_error = None, None
        if raw:
            try:
                body = json.loads(raw)
            except ValueError as e:
                parse_error = str(e)

        auth = self.headers.get("Authorization", "")
        with lock:
            if not auth.startswith("Bearer note_"):
                status, resp = self.reply(401)
            elif parse_error is not None:
                status, resp = self.reply(400, {"error": "invalid JSON: " + parse_error})
            else:
                status, resp = self.route(body or {})
            self.store.log({
                "time": now(), "method": self.command, "path": self.path,
                "body": body, "status": status,
                # The listing is large and derivable from tasks.json; keep the log readable.
                "response": None if (self.command == "GET" and status == 200) else resp,
            })

    do_GET = do_POST = do_PATCH = do_PUT = do_DELETE = handle_any

    def route(self, body):
        s = self.store
        m = self.command
        p = self.path.split("?")[0]

        if p == "/api/tasks" and m == "GET":
            top = [t for t in s.tasks.values() if t["parent_id"] is None and t["state"] != "dropped"]
            return self.reply(200, [s.node(t) for t in sorted(top, key=lambda t: t["id"])])

        if p == "/api/tasks" and m == "POST":
            extra = set(body) - CREATE_KEYS
            if extra:
                return self.reply(422, {"error": "unknown field `%s`" % sorted(extra)[0]})
            title = body.get("title") or ""
            if not title.strip():
                return self.reply(422, {"error": "title must not be empty"})
            if len(title.encode()) > MAX_TITLE:
                return self.reply(422, {"error": "title too long"})
            t, err = s.new_task(body, "import")
            if err:
                return self.reply(*err)
            s.save()
            return self.reply(200, s.row(t))

        if p == "/api/agent/inbox" and m == "POST":
            if set(body) - {"source_id", "kind", "context"}:
                return self.reply(400, {"error": "unknown field"})
            src = body.get("source_id") or ""
            if not re.fullmatch(r"[A-Za-z0-9:._-]{1,200}", src) or body.get("kind") not in ("announcement", "material"):
                return self.reply(422, {"error": "bad source_id or kind"})
            if len((body.get("context") or "").encode()) > MAX_CONTEXT:
                return self.reply(422, {"error": "context over 32 KiB"})
            return self.reply(200, {"source_id": src, "outcome": "nothing", "reason": "fake note: recorded only",
                                    "memory_ids": [], "steps": []})

        # --- tasks keyed by the importer's id ---
        me = re.fullmatch(r"/api/tasks/by-external/(.+)", p)
        if me:
            ext = urllib.parse.unquote(me.group(1))
            if m == "DELETE":
                hit = s.by_external(ext)
                if hit is None:
                    return self.reply(404)
                s.delete_task(hit["id"])
                s.save()
                return self.reply(204)
            if m == "PUT":
                extra = set(body) - (CREATE_KEYS - {"external_id"})
                if body.get("external_id") not in (None, ext):
                    return self.reply(422, {"error": "external_id belongs to the path, not the body"})
                if extra:
                    return self.reply(422, {"error": "unknown field `%s`" % sorted(extra)[0]})
                if not (body.get("title") or "").strip():
                    return self.reply(422, {"error": "title must not be empty"})
                if ext in s.tombstones:
                    return self.reply(410, {"error": "the user deleted this task; it is not recreated",
                                            "external_id": ext, "deleted_at": s.tombstones[ext]})
                hit = s.by_external(ext)
                if hit is None:
                    t, err = s.new_task(dict(body, external_id=ext), "import")
                    if err:
                        return self.reply(*err)
                    s.save()
                    return self.reply(201, s.node(t))
                # The importer owns title, notes, due date and link; the
                # description, duration and steps stay; state only moves
                # forward to done.
                if hit["parent_id"] is not None and body.get("due_at") is not None:
                    return self.reply(422, {"error": "a step carries no due date of its own; the task it belongs to holds it"})
                hit["title"] = body["title"]
                for k in ("notes", "due_at", "url"):
                    if k in body:
                        hit[k] = body[k]
                if body.get("state") == "done" and hit["state"] not in ("done", "dropped"):
                    hit["state"] = "done"
                hit["updated_at"] = now()
                s.save()
                return self.reply(200, s.node(hit))
            return self.reply(404)

        # --- calendar (note's /api/calendar, plus the by-external upsert) ---
        if p == "/api/calendar" and m == "GET":
            return self.reply(200, sorted(s.calendar.values(), key=lambda e: e["id"]))

        mc = re.fullmatch(r"/api/calendar/by-external/(.+)", p)
        if mc:
            ext = urllib.parse.unquote(mc.group(1))
            if m == "DELETE":
                hit = next((e for e in s.calendar.values() if e["external_id"] == ext), None)
                if hit is None:
                    return self.reply(404, {"error": "no calendar entry for that external id"})
                del s.calendar[hit["id"]]
                s.save()
                return self.reply(204)
            if m == "PUT":
                extra = set(body) - CALENDAR_KEYS
                if extra:
                    return self.reply(422, {"error": "unknown field `%s`" % sorted(extra)[0]})
                title = (body.get("title") or "").strip()
                if not title:
                    return self.reply(422, {"error": "title must not be empty"})
                if len(title) > MAX_CAL_TITLE:
                    return self.reply(422, {"error": "a title is at most %d characters" % MAX_CAL_TITLE})
                if body.get("kind") not in CALENDAR_KINDS:
                    return self.reply(422, {"error": "kind must be one of %s" % (CALENDAR_KINDS,)})
                for field in ("start_time", "end_time"):
                    if not re.fullmatch(r"[0-2]\d:[0-5]\d", body.get(field) or ""):
                        return self.reply(422, {"error": "%s must be HH:MM" % field})
                if body.get("start_time") >= body.get("end_time"):
                    return self.reply(422, {"error": "end_time must be after start_time"})
                one_off = (body.get("on_date") or "").strip()
                if one_off and not re.fullmatch(r"\d{4}-\d{2}-\d{2}", one_off):
                    return self.reply(422, {"error": "on_date must be YYYY-MM-DD"})
                if not one_off and not body.get("days"):
                    return self.reply(422, {"error": "an entry needs on_date or days"})

                hit = next((e for e in s.calendar.values() if e["external_id"] == ext), None)
                created = hit is None
                if created and len(s.calendar) >= MAX_ENTRIES:
                    return self.reply(422, {"error": "a calendar holds at most %d entries" % MAX_ENTRIES})
                entry = hit or {"id": s.next_cal_id, "external_id": ext, "exceptions": []}
                if created:
                    s.next_cal_id += 1
                entry.update({k: body.get(k) for k in CALENDAR_KEYS})
                entry["days"] = body.get("days") or 0
                entry["updated_at"] = now()
                s.calendar[entry["id"]] = entry
                s.save()
                return self.reply(201 if created else 200, entry)
            return self.reply(404)

        mt = re.fullmatch(r"/api/tasks/(\d+)(/agent)?", p)
        if not mt:
            return self.reply(404)  # the router's empty-body 404
        tid, agent = int(mt.group(1)), mt.group(2)
        t = s.tasks.get(tid)

        if agent and m == "POST":
            if set(body) - {"context"}:
                return self.reply(400, {"error": "unknown field"})
            if t is None:
                return self.reply(404, {"error": "task not found"})
            if t["parent_id"] is not None:
                return self.reply(409, {"error": "only top-level tasks are briefed"})
            if len((body.get("context") or "").encode()) > MAX_CONTEXT:
                return self.reply(422, {"error": "context over 32 KiB"})
            if self.agent is None:
                return self.reply(200, {"task_id": tid, "outcome": "unchanged", "steps": [], "task": s.node(t)})
            snapshot = copy.deepcopy(s.tasks), s.next_id
            try:
                outcome, steps = self.agent.brief(t, body.get("context") or "")
            except (urllib.error.URLError, OSError, ValueError, KeyError) as e:
                s.tasks, s.next_id = snapshot
                return self.reply(502, {"error": "agent session failed: %s" % e})
            s.save()
            return self.reply(200, {"task_id": tid, "outcome": outcome, "steps": steps, "task": s.node(t)})

        if t is None:
            return self.reply(404)

        if m == "PATCH":
            extra = set(body) - PATCH_KEYS
            if extra:
                return self.reply(422, {"error": "unknown field `%s`" % sorted(extra)[0]})
            if "state" in body and body["state"] not in STATES:
                return self.reply(422, {"error": "invalid state"})
            if "urgency" in body and body["urgency"] not in URGENCY:
                return self.reply(422, {"error": "urgency must be one of low, normal, high"})
            if "urgency" in body and t["parent_id"] is not None:
                return self.reply(422, {"error": "a step reads its parent's urgency"})
            for k in ("description", "notes"):
                if k in body and len(body[k].encode()) > MAX_TEXT:
                    return self.reply(422, {"error": "%s over 16 KiB" % k})
            if "title" in body and (not body["title"].strip() or len(body["title"].encode()) > MAX_TITLE):
                return self.reply(422, {"error": "invalid title"})
            ext = body.get("external_id")
            if ext and s.by_external(ext) not in (None, t):
                return self.reply(409, {"error": "external_id %s already belongs to task %d" % (ext, s.by_external(ext)["id"])})
            t.update(body)
            if "duration_min" in body:
                t["duration_source"] = "user"
            t["updated_at"] = now()
            s.save()
            return self.reply(200, dict(s.row(t), demoted_from_now=None))

        if m == "DELETE":
            s.delete_task(tid)
            s.save()
            return self.reply(204)

        return self.reply(405)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dir", required=True, help="where requests.jsonl and tasks.json go")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=3999)
    ap.add_argument("--agent", choices=["record", "llm"], default="record")
    ap.add_argument("--prompt", default="~/Projects/note/config/defaults/prompts/import.md")
    ap.add_argument("--key-file", default="~/.config/schoolwork-check/llm-api-key")
    ap.add_argument("--base-url", default="https://openrouter.ai/api/v1")
    ap.add_argument("--model", default="deepseek/deepseek-v4-flash")
    a = ap.parse_args()
    os.makedirs(a.dir, mode=0o700, exist_ok=True)
    Handler.store = Store(a.dir)
    if a.agent == "llm":
        with open(os.path.expanduser(a.key_file)) as f:
            key = f.read().strip()
        Handler.agent = Agent(Handler.store, a.prompt, key, a.base_url, a.model)
    srv = ThreadingHTTPServer((a.host, a.port), Handler)
    print("fake note on http://%s:%d, recording to %s" % (a.host, a.port, a.dir), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
