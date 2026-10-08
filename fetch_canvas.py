#!/usr/bin/env python3
"""Pull Canvas planner items and emit homework rows.

Usage:
  CANVAS_URL=https://school.instructure.com CANVAS_TOKEN=... ./fetch_canvas.py            # upsert into homework.db
  CANVAS_URL=... CANVAS_TOKEN=... ./fetch_canvas.py --sql                                   # print INSERT statements
  ./fetch_canvas.py --demo --sql                                                            # fake rows, no credentials

Options:
  --db PATH        SQLite file (default homework.db)
  --sql            print SQL to stdout instead of writing the db
  --days-back N    include items due up to N days ago (default 14)
  --days-ahead N   include items due up to N days ahead (default 120)
  --all            include completed items too (default: incomplete only)
  --demo           use built-in sample data instead of calling Canvas
"""
import argparse
import datetime as dt
import os
import sqlite3
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
COLUMNS = ("source", "source_id", "course", "title", "kind", "due_at",
           "status", "points", "url", "fetched_at")

KIND_MAP = {
    "assignment": "assignment",
    "quiz": "quiz",
    "discussion_topic": "discussion",
    "wiki_page": "page",
    "planner_note": "note",
    "calendar_event": "event",
    "announcement": "announcement",
}


def now_iso():
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()


def status_of(item):
    """Collapse Canvas's submissions object into one word."""
    sub = item.get("submissions") or {}
    if not isinstance(sub, dict):
        return "todo"
    if sub.get("excused"):
        return "excused"
    if sub.get("graded"):
        return "graded"
    if sub.get("submitted"):
        return "late" if sub.get("late") else "submitted"
    if sub.get("missing"):
        return "missing"
    return "todo"


def fetch_planner(base, token, start, end, incomplete_only):
    import requests
    url = f"{base.rstrip('/')}/api/v1/planner/items"
    params = {"start_date": start.isoformat(), "end_date": end.isoformat(), "per_page": 100}
    if incomplete_only:
        params["filter"] = "incomplete_items"
    headers = {"Authorization": f"Bearer {token}"}
    items = []
    while url:
        r = requests.get(url, headers=headers, params=params, timeout=30)
        if r.status_code == 401:
            sys.exit("Canvas returned 401: the access token is invalid or expired. "
                     "Student tokens expire within 120 days, generate a new one.")
        r.raise_for_status()
        items.extend(r.json())
        url = r.links.get("next", {}).get("url")
        params = None  # next-page URL already carries the query
    return items


def course_names(base, token):
    import requests
    url = f"{base.rstrip('/')}/api/v1/courses"
    params = {"enrollment_state": "active", "per_page": 100}
    headers = {"Authorization": f"Bearer {token}"}
    names = {}
    while url:
        r = requests.get(url, headers=headers, params=params, timeout=30)
        r.raise_for_status()
        for c in r.json():
            names[c["id"]] = c.get("course_code") or c.get("name") or str(c["id"])
        url = r.links.get("next", {}).get("url")
        params = None
    return names


def to_row(item, names, base, fetched):
    p = item.get("plannable") or {}
    plannable_type = item.get("plannable_type", "assignment")
    cid = item.get("course_id")
    html = item.get("html_url") or ""
    if html.startswith("/"):
        html = base.rstrip("/") + html
    return {
        "source": "canvas",
        "source_id": f"{plannable_type}:{item.get('plannable_id')}",
        "course": names.get(cid, item.get("context_name") or str(cid or "")),
        "title": p.get("title") or p.get("name") or "(untitled)",
        "kind": KIND_MAP.get(plannable_type, plannable_type),
        "due_at": p.get("due_at") or item.get("plannable_date"),
        "status": status_of(item),
        "points": p.get("points_possible"),
        "url": html or None,
        "fetched_at": fetched,
    }


def demo_rows(fetched):
    return [
        {"source": "canvas", "source_id": "assignment:5521", "course": "MATH 221", "title": "Problem Set 3",
         "kind": "assignment", "due_at": "2026-09-18T06:59:00Z", "status": "todo", "points": 20,
         "url": "https://school.instructure.com/courses/101/assignments/5521", "fetched_at": fetched},
        {"source": "canvas", "source_id": "quiz:918", "course": "CHEM 101", "title": "Chapter 4 Quiz",
         "kind": "quiz", "due_at": "2026-09-19T23:59:00Z", "status": "todo", "points": 10,
         "url": "https://school.instructure.com/courses/102/quizzes/918", "fetched_at": fetched},
        {"source": "canvas", "source_id": "discussion_topic:3302", "course": "ENGL 110", "title": "Week 3 Reading Response",
         "kind": "discussion", "due_at": "2026-09-16T23:59:00Z", "status": "missing", "points": 5,
         "url": "https://school.instructure.com/courses/103/discussion_topics/3302", "fetched_at": fetched},
        {"source": "canvas", "source_id": "assignment:5498", "course": "MATH 221", "title": "Problem Set 2",
         "kind": "assignment", "due_at": "2026-09-11T06:59:00Z", "status": "graded", "points": 20,
         "url": "https://school.instructure.com/courses/101/assignments/5498", "fetched_at": fetched},
    ]


def sql_literal(v):
    if v is None:
        return "NULL"
    if isinstance(v, (int, float)):
        return repr(v)
    return "'" + str(v).replace("'", "''") + "'"


def as_insert(row):
    cols = ", ".join(COLUMNS)
    vals = ", ".join(sql_literal(row[c]) for c in COLUMNS)
    updates = ", ".join(f"{c}=excluded.{c}" for c in COLUMNS if c not in ("source", "source_id"))
    return (f"INSERT INTO homework ({cols}) VALUES ({vals})\n"
            f"  ON CONFLICT(source, source_id) DO UPDATE SET {updates};")


def write_db(path, rows):
    con = sqlite3.connect(path)
    with open(os.path.join(HERE, "schema.sql")) as f:
        con.executescript(f.read())
    for row in rows:
        con.execute(as_insert(row).replace(";", ""))
    con.commit()
    con.close()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", default=os.path.join(HERE, "homework.db"))
    ap.add_argument("--sql", action="store_true")
    ap.add_argument("--days-back", type=int, default=14)
    ap.add_argument("--days-ahead", type=int, default=120)
    ap.add_argument("--all", action="store_true")
    ap.add_argument("--demo", action="store_true")
    a = ap.parse_args()

    fetched = now_iso()
    if a.demo:
        rows = demo_rows(fetched)
    else:
        base = os.environ.get("CANVAS_URL")
        token = os.environ.get("CANVAS_TOKEN")
        if not base or not token:
            sys.exit("Set CANVAS_URL and CANVAS_TOKEN (or use --demo).")
        today = dt.date.today()
        items = fetch_planner(base, token, today - dt.timedelta(days=a.days_back),
                              today + dt.timedelta(days=a.days_ahead), not a.all)
        names = course_names(base, token)
        rows = [to_row(i, names, base, fetched) for i in items
                if i.get("plannable_type") not in ("announcement", "calendar_event")]

    if a.sql:
        print("BEGIN;")
        for row in rows:
            print(as_insert(row))
        print("COMMIT;")
    else:
        write_db(a.db, rows)
        print(f"{len(rows)} rows upserted into {a.db}", file=sys.stderr)


if __name__ == "__main__":
    main()
