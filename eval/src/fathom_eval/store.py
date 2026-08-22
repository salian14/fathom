"""The results store: SQLite, written as the run proceeds.

Two properties drive the shape here.

**A killed run keeps what it finished.** Every result and its checks are committed as
soon as they are graded, rather than batched at the end. A CronJob that hits its
activeDeadlineSeconds two thirds of the way through still leaves two thirds of the
evidence behind, which is the difference between a wasted night and a partial answer.

**Checks are rows, not columns.** `checks` is one row per check per result rather than a
wide table with a column per check. Adding a check is then a code change with no
migration, and reporting is a GROUP BY rather than a hand-maintained list of columns.
That matters because the check set is expected to grow.

Reporting is deliberately not implemented here -- this module's job ends at "the data is
queryable".
"""

from __future__ import annotations

import json
import os
import sqlite3
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime

SCHEMA_VERSION = 1

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
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

CREATE TABLE IF NOT EXISTS results (
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
    -- ok | ceiling | no_answer | timeout | error. Anything but 'ok' is a
    -- non-functional failure and is reported as a bug, not as a low score.
    outcome         TEXT NOT NULL,
    answer          TEXT,
    steps_json      TEXT,
    tool_calls      INTEGER,
    elapsed_seconds REAL,
    error           TEXT,
    passed          INTEGER,        -- 1 only when every check passed
    created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS checks (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    result_id INTEGER NOT NULL REFERENCES results(id),
    category  TEXT NOT NULL,        -- naming | structural | scope | termination
    name      TEXT NOT NULL,
    passed    INTEGER NOT NULL,
    detail    TEXT
);

CREATE TABLE IF NOT EXISTS judgements (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    result_id      INTEGER NOT NULL REFERENCES results(id),
    judge_model    TEXT NOT NULL,
    rubric_version TEXT NOT NULL,
    verdict        TEXT,
    score          INTEGER,
    rationale      TEXT,
    raw_json       TEXT,
    error          TEXT,
    created_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_results_run     ON results(run_id);
CREATE INDEX IF NOT EXISTS idx_results_fixture ON results(fixture, question_id);
CREATE INDEX IF NOT EXISTS idx_checks_result   ON checks(result_id);
CREATE INDEX IF NOT EXISTS idx_checks_name     ON checks(name);
-- The judge finds its work through this: results with no judgement at the current
-- rubric version. Without the index that is a scan of every result ever recorded.
CREATE UNIQUE INDEX IF NOT EXISTS idx_judge_unique
    ON judgements(result_id, rubric_version);
"""


def now() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds")


def connect(path: str) -> sqlite3.Connection:
    """Open (creating if needed) the results database.

    WAL is set because the judge pass reads the same file a run may be writing. It is a
    no-op on filesystems that cannot support it, which is why the pragma is not asserted.
    """
    parent = os.path.dirname(os.path.abspath(path))
    if parent:
        os.makedirs(parent, exist_ok=True)
    conn = sqlite3.connect(path, timeout=30.0)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.executescript(SCHEMA)
    conn.execute(
        "INSERT INTO meta(key, value) VALUES('schema_version', ?) "
        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
        (str(SCHEMA_VERSION),),
    )
    conn.commit()
    return conn


@contextmanager
def opened(path: str) -> Iterator[sqlite3.Connection]:
    conn = connect(path)
    try:
        yield conn
    finally:
        conn.close()


def sweep_abandoned(conn: sqlite3.Connection) -> int:
    """Mark runs still 'running' as abandoned, and say how many.

    A run that is OOM-killed or hits activeDeadlineSeconds cannot update its own row, so
    it would otherwise sit as 'running' forever and quietly pollute every "latest run"
    query. This is called at the start of the next run, which is the only moment we can
    be sure the previous one is not still going -- concurrencyPolicy: Forbid guarantees
    it.
    """
    cur = conn.execute(
        "UPDATE runs SET status='abandoned', finished_at=? WHERE status='running'",
        (now(),),
    )
    conn.commit()
    return cur.rowcount


def start_run(conn: sqlite3.Connection, **fields) -> int:
    cur = conn.execute(
        """INSERT INTO runs(started_at, status, git_sha, harness_version,
                            agent_url, agent_model, iterations, host)
           VALUES(?, 'running', ?, ?, ?, ?, ?, ?)""",
        (
            now(),
            fields.get("git_sha"),
            fields.get("harness_version"),
            fields.get("agent_url"),
            fields.get("agent_model"),
            fields.get("iterations"),
            fields.get("host"),
        ),
    )
    conn.commit()
    return int(cur.lastrowid)


def finish_run(conn: sqlite3.Connection, run_id: int, status: str, note: str = "") -> None:
    conn.execute(
        "UPDATE runs SET status=?, finished_at=?, note=? WHERE id=?",
        (status, now(), note or None, run_id),
    )
    conn.commit()


def record_result(conn: sqlite3.Connection, run_id: int, result: dict, checks: list) -> int:
    """Write one graded result and its checks in a single transaction.

    Single transaction on purpose: a result row without its checks would read as a
    result that passed nothing, which is indistinguishable from a real total failure.
    """
    with conn:  # commits on success, rolls back on exception
        cur = conn.execute(
            """INSERT INTO results(run_id, fixture, isolation, namespace, question_id,
                                   question_text, iteration, http_status, outcome, answer,
                                   steps_json, tool_calls, elapsed_seconds, error, passed,
                                   created_at)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (
                run_id,
                result["fixture"],
                result["isolation"],
                result["namespace"],
                result["question_id"],
                result.get("question_text"),
                result["iteration"],
                result.get("http_status"),
                result["outcome"],
                result.get("answer"),
                json.dumps(result.get("steps") or []),
                result.get("tool_calls"),
                result.get("elapsed_seconds"),
                result.get("error"),
                1 if all(c.passed for c in checks) else 0,
                now(),
            ),
        )
        result_id = int(cur.lastrowid)
        conn.executemany(
            "INSERT INTO checks(result_id, category, name, passed, detail) VALUES(?,?,?,?,?)",
            [(result_id, c.category, c.name, 1 if c.passed else 0, c.detail) for c in checks],
        )
    return result_id


def pending_judgements(
    conn: sqlite3.Connection, rubric_version: str, sample: int, limit: int | None = None
) -> list[sqlite3.Row]:
    """Results with no judgement at this rubric version.

    Ordered so the interesting ones are judged first when sampling: results whose
    deterministic checks disagreed (some passed, some failed) come before unanimous
    ones, because that is exactly where a human would want a second opinion. Within a
    fixture the sample is capped so one noisy fixture cannot consume a whole night.
    """
    rows = conn.execute(
        """
        SELECT r.*,
               (SELECT COUNT(*) FROM checks c WHERE c.result_id = r.id AND c.passed = 0)
                   AS failed_checks,
               (SELECT COUNT(*) FROM checks c WHERE c.result_id = r.id) AS total_checks
        FROM results r
        WHERE NOT EXISTS (
            SELECT 1 FROM judgements j
            WHERE j.result_id = r.id AND j.rubric_version = ?
        )
        ORDER BY r.id DESC
        """,
        (rubric_version,),
    ).fetchall()

    def contested(row: sqlite3.Row) -> int:
        failed, total = row["failed_checks"], row["total_checks"]
        return 0 if (total and 0 < failed < total) else 1

    rows.sort(key=contested)

    if sample:
        seen: dict[str, int] = {}
        kept = []
        for row in rows:
            key = f"{row['run_id']}/{row['fixture']}/{row['question_id']}"
            if seen.get(key, 0) >= sample:
                continue
            seen[key] = seen.get(key, 0) + 1
            kept.append(row)
        rows = kept

    return rows[:limit] if limit else rows


def record_judgement(conn: sqlite3.Connection, result_id: int, **fields) -> None:
    conn.execute(
        """INSERT INTO judgements(result_id, judge_model, rubric_version, verdict,
                                  score, rationale, raw_json, error, created_at)
           VALUES(?,?,?,?,?,?,?,?,?)
           ON CONFLICT(result_id, rubric_version) DO UPDATE SET
               judge_model=excluded.judge_model, verdict=excluded.verdict,
               score=excluded.score, rationale=excluded.rationale,
               raw_json=excluded.raw_json, error=excluded.error,
               created_at=excluded.created_at""",
        (
            result_id,
            fields["judge_model"],
            fields["rubric_version"],
            fields.get("verdict"),
            fields.get("score"),
            fields.get("rationale"),
            fields.get("raw_json"),
            fields.get("error"),
            now(),
        ),
    )
    conn.commit()
