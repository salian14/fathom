CREATE TABLE checks (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    result_id INTEGER NOT NULL REFERENCES results(id),
    -- termination | remediation | naming | content | structural | scope, in
    -- grading.CATEGORY_ORDER's reporting priority.
    category  TEXT NOT NULL,
    name      TEXT NOT NULL,
    passed    INTEGER NOT NULL,
    detail    TEXT
);

CREATE TABLE judgements (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    result_id      INTEGER NOT NULL REFERENCES results(id),
    judge_model    TEXT NOT NULL,
    rubric_version TEXT NOT NULL,
    verdict        TEXT,
    score          INTEGER,
    -- Scored separately from `score` rather than folded into it: a confident, well
    -- evidenced diagnosis with a useless fix and the reverse are different failures,
    -- and averaging them into one number hides which one happened.
    remediation_score INTEGER,
    rationale      TEXT,
    raw_json       TEXT,
    error          TEXT,
    created_at     TEXT NOT NULL
);

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE results (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          INTEGER NOT NULL REFERENCES runs(id),
    fixture         TEXT NOT NULL,
    -- shared | exclusive. Recorded per result so a later analysis can ask whether the
    -- exclusive fixtures behave differently from the rest.
    isolation       TEXT NOT NULL,
    namespace       TEXT NOT NULL,
    question_id     TEXT NOT NULL,
    -- The prompt as actually sent, namespace substituted. Stored rather than derived:
    -- the judge must grade what was asked, and a later fixture edit must not silently
    -- rewrite the history of what an old run was tested on.
    question_text   TEXT,
    iteration       INTEGER NOT NULL,
    http_status     INTEGER,
    -- ok | ceiling | no_answer | timeout | error | fixture_error. Anything but
    -- 'ok' is a non-functional failure and is reported as a bug, not as a low
    -- score. 'fixture_error' is the runner's own: the fixture never reached its
    -- broken state, so nothing was measured at all.
    outcome         TEXT NOT NULL,
    answer          TEXT,
    -- The structured remediation from /chat, verbatim. Stored rather than derived
    -- because the judge grades the fix and reads only from this table -- by the time
    -- it runs, the fixture namespace is long gone.
    remediation_json TEXT,
    steps_json      TEXT,
    tool_calls      INTEGER,
    elapsed_seconds REAL,
    error           TEXT,
    passed          INTEGER,        -- 1 only when every check passed
    created_at      TEXT NOT NULL
);

CREATE TABLE runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at       TEXT NOT NULL,
    finished_at      TEXT,
    -- running | ok | failed | abandoned. 'abandoned' is written by the sweep below,
    -- never by the run itself -- a process that is killed cannot record its own death.
    status           TEXT NOT NULL,
    git_sha          TEXT,
    harness_version  TEXT,
    agent_url        TEXT,
    agent_model      TEXT,
    iterations       INTEGER,
    host             TEXT,
    note             TEXT
);

CREATE TABLE sqlite_sequence(name,seq);

CREATE INDEX idx_checks_category ON checks(category, passed);

CREATE INDEX idx_checks_name     ON checks(name);

CREATE INDEX idx_checks_result   ON checks(result_id);

CREATE UNIQUE INDEX idx_judge_unique
    ON judgements(result_id, rubric_version);

CREATE INDEX idx_results_fixture ON results(fixture, question_id);

CREATE INDEX idx_results_run     ON results(run_id);
