-- One row per homework item, from any LMS.
-- Re-running a fetch upserts on (source, source_id), so the table stays current.
CREATE TABLE IF NOT EXISTS homework (
    source      TEXT NOT NULL,            -- 'canvas' | 'classroom'
    source_id   TEXT NOT NULL,            -- id inside that LMS (assignment/coursework id)
    course      TEXT NOT NULL,            -- course display name
    title       TEXT NOT NULL,
    kind        TEXT NOT NULL,            -- assignment | quiz | discussion | page | material | question
    due_at      TEXT,                     -- ISO-8601 UTC, NULL if undated
    status      TEXT NOT NULL,            -- todo | submitted | graded | missing | late | excused
    points      REAL,                     -- max points, NULL if ungraded
    url         TEXT,
    fetched_at  TEXT NOT NULL,            -- ISO-8601 UTC, when this row was last refreshed
    PRIMARY KEY (source, source_id)
);

CREATE INDEX IF NOT EXISTS homework_due_idx    ON homework (due_at);
CREATE INDEX IF NOT EXISTS homework_status_idx ON homework (status);

-- Handy view: what is still owed, soonest first.
CREATE VIEW IF NOT EXISTS todo AS
SELECT source, course, title, kind, due_at, points, url
FROM homework
WHERE status IN ('todo', 'missing', 'late')
ORDER BY due_at IS NULL, due_at;
