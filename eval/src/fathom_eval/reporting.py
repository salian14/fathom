"""Read-only queries over the results database.

Deliberately not part of `store`. That module's docstring disclaims reporting, and its
public entry point -- `connect()` -- creates directories, runs DDL, migrates and INSERTs.
Handing a read-only service that function as the obvious way in is a footgun; a reader
that accidentally calls it brings a schemaless database into existence and shadows the
one the harness writes.

There is still exactly one definition of the schema. Nothing here creates a table. This
module imports `store.SCHEMA_VERSION` to notice a database newer than itself, and
`grading.CATEGORY_ORDER` to order a failure list, and defines neither.

Everything takes an open connection as its first argument and returns plain dicts, so the
queries are testable without a socket and the HTTP layer in `serve` knows no SQL.
"""

from __future__ import annotations

import json
import os
import sqlite3
from typing import Any

from . import store
from .grading import CATEGORY_ORDER

# Outcomes, in the order a summary should present them. `fixture_error` is written by
# runner.py when a fixture never reaches its broken state -- nothing was measured, so it
# is a harness fault rather than a low score. It is absent from the enum in store.py's
# column comment; this tuple is the complete list.
OUTCOMES = ("ok", "ceiling", "no_answer", "timeout", "error", "fixture_error")


class MissingDatabase(Exception):
    """No results database exists yet -- no run has ever finished."""


class UnreadableDatabase(Exception):
    """The file is there and cannot be opened. A real fault, unlike MissingDatabase."""


class SchemaTooNew(Exception):
    """Written by a newer harness than this code understands."""


def connect_ro(path: str) -> sqlite3.Connection:
    """Open the results database read-only. Never creates it.

    `mode=ro` on a URI rather than a plain path: `sqlite3.connect()` on a missing path
    creates an empty file, and a reader that can do that can shadow the writer's database
    with a schemaless one. This raises instead, and `serve` turns that into an empty view
    rather than an error -- a cluster whose CronJob has not run yet is a correct state.

    `query_only` is belt and braces on top of `mode=ro`. The URI already refuses writes;
    the pragma makes the intent visible at the line where someone would otherwise
    "simplify" the URI back to a bare path.

    Deliberately NOT set here: `journal_mode`. Setting it is a write.

    Note the database is in WAL mode, so a read-only open may need to create the `-shm`
    index -- which requires write permission on the *directory*, not the file. That is why
    the chart does not mount the results volume `readOnly: true`; see the comment there.
    """
    if not os.path.exists(path):
        raise MissingDatabase(path)
    try:
        conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=5.0)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA query_only=ON")
    except sqlite3.Error as err:
        raise UnreadableDatabase(f"{path}: {err}") from err
    return conn


def schema_version(conn: sqlite3.Connection) -> int:
    row = conn.execute("SELECT value FROM meta WHERE key='schema_version'").fetchone()
    return int(row["value"]) if row else 0


def check_schema(conn: sqlite3.Connection) -> None:
    """Refuse a database a newer harness wrote.

    An old reader against a new schema renders as mysteriously empty or half-populated.
    Naming both numbers turns that into an obvious "roll the image" message. The reverse
    -- a new reader on an old file -- is fine: `_migrate` is additive and every query here
    names its columns.
    """
    found = schema_version(conn)
    if found > store.SCHEMA_VERSION:
        raise SchemaTooNew(
            f"database is at schema version {found}, this build understands "
            f"{store.SCHEMA_VERSION} -- the reader image is older than the harness"
        )


def database_info(path: str) -> dict[str, Any]:
    """File-level facts, without opening the database. Safe when it does not exist."""
    if not os.path.exists(path):
        return {"present": False, "path": path}
    stat = os.stat(path)
    return {"present": True, "path": path, "bytes": stat.st_size, "mtime": int(stat.st_mtime)}


# --------------------------------------------------------------------- helpers


def _rows(cur: sqlite3.Cursor) -> list[dict[str, Any]]:
    return [dict(r) for r in cur.fetchall()]


def _outcome_columns(prefix: str = "") -> str:
    """`SUM(outcome='x') AS x` for every outcome.

    Generated rather than written out so that adding an outcome to OUTCOMES is the only
    edit -- the same reason `checks` is rows and not columns.
    """
    col = f"{prefix}outcome" if prefix else "outcome"
    return ",\n       ".join(f"COALESCE(SUM({col} = '{o}'), 0) AS {o}" for o in OUTCOMES)


def _split_outcomes(row: dict[str, Any]) -> dict[str, Any]:
    """Move the generated outcome columns into a nested dict."""
    row["outcomes"] = {o: row.pop(o, 0) for o in OUTCOMES}
    return row


def _loads(raw: str | None, default: Any, field: str) -> tuple[Any, str | None]:
    """Parse stored JSON without letting a bad row take down the request.

    A malformed steps_json is a harness bug worth seeing, not worth a 500 that hides
    every other field of the result.
    """
    if raw is None:
        return default, None
    try:
        return json.loads(raw), None
    except (ValueError, TypeError) as err:
        return default, f"{field}: {err}"


def order_checks(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """CATEGORY_ORDER, then failures before passes within a category.

    Ordered in Python against grading.CATEGORY_ORDER rather than by a SQL CASE, so
    reordering the categories there moves this with no second edit. An unknown category
    sorts last rather than raising -- a check added ahead of this code should still show.
    """
    rank = {c: i for i, c in enumerate(CATEGORY_ORDER)}
    return sorted(
        rows,
        key=lambda r: (rank.get(r["category"], len(rank)), r["passed"], r["name"]),
    )


# ----------------------------------------------------------------- level one


def _runs_sql(where: str, tail: str) -> str:
    """The run aggregate, with a caller-supplied WHERE and ORDER/LIMIT tail.

    One definition for the list and the single-run lookup, because the two must agree:
    a run's counts in the list and on its own page being computed differently is the kind
    of discrepancy nobody debugs, they just stop trusting the numbers.
    """
    return f"""
SELECT r.id, r.started_at, r.finished_at, r.status, r.git_sha, r.harness_version,
       r.agent_url, r.agent_model, r.iterations, r.host, r.note,
       COUNT(res.id)                       AS results,
       COALESCE(SUM(res.passed), 0)        AS passed,
       COUNT(DISTINCT res.fixture)         AS fixtures,
       {_outcome_columns("res.")},
       AVG(res.elapsed_seconds)            AS mean_elapsed
FROM runs r
-- LEFT JOIN, not JOIN: a run that died before writing a single result is exactly the one
-- worth seeing in this list, and an inner join would hide it. That is the usual shape of
-- an 'abandoned' run.
LEFT JOIN results res ON res.run_id = r.id
{where}
GROUP BY r.id
{tail}
"""


def list_runs(conn: sqlite3.Connection, limit: int = 50, offset: int = 0) -> dict[str, Any]:
    sql = _runs_sql("", "ORDER BY r.id DESC LIMIT ? OFFSET ?")
    runs = [_split_outcomes(r) for r in _rows(conn.execute(sql, (limit, offset)))]
    by_run = _judge_by_run(conn, [r["id"] for r in runs])
    for run in runs:
        run["judge"] = by_run.get(run["id"], [])
    total = conn.execute("SELECT COUNT(*) AS c FROM runs").fetchone()["c"]
    return {"runs": runs, "limit": limit, "offset": offset, "total": total}


def get_run(conn: sqlite3.Connection, run_id: int) -> dict[str, Any] | None:
    rows = _rows(conn.execute(_runs_sql("WHERE r.id = ?", ""), (run_id,)))
    if not rows:
        return None
    run = _split_outcomes(rows[0])
    run["judge"] = _judge_by_run(conn, [run_id]).get(run_id, [])
    return run


def _judge_by_run(conn: sqlite3.Connection, run_ids: list[int]) -> dict[int, list[dict[str, Any]]]:
    """Judge aggregates per run, grouped by rubric version.

    A separate query rather than another join: a result can carry several judgements
    across rubric versions, and joining them into the run aggregate above would multiply
    every count in it.

    Grouped by rubric_version on purpose. A score's scale is defined by its rubric, so
    averaging across versions is meaningless -- and rows judged at v1 have
    remediation_score IS NULL, which AVG() skips, so a v1/v2 mix would silently report a
    remediation mean over the v2 subset while claiming to cover both.
    """
    if not run_ids:
        return {}
    marks = ",".join("?" * len(run_ids))
    sql = f"""
        SELECT res.run_id, j.rubric_version,
               COUNT(*)                 AS judged,
               -- A judgement whose model call failed or whose reply would not parse is
               -- stored with a NULL verdict and a populated error. It must be visible and
               -- must not drag the mean, hence a counter of its own.
               SUM(j.verdict IS NULL)   AS errors,
               SUM(j.verdict = 'pass')  AS passes,
               AVG(j.score)             AS mean_score,
               AVG(j.remediation_score) AS mean_remediation_score
        FROM judgements j
        JOIN results res ON res.id = j.result_id
        WHERE res.run_id IN ({marks})
        GROUP BY res.run_id, j.rubric_version
        ORDER BY res.run_id, j.rubric_version
    """
    out: dict[int, list[dict[str, Any]]] = {}
    for row in _rows(conn.execute(sql, run_ids)):
        out.setdefault(row.pop("run_id"), []).append(row)
    return out


# ----------------------------------------------------------------- level two


def fixture_summary(conn: sqlite3.Connection, run_id: int) -> list[dict[str, Any]]:
    sql = f"""
        SELECT fixture, isolation,
               COUNT(*)                    AS results,
               COALESCE(SUM(passed), 0)    AS passed,
               COUNT(DISTINCT question_id) AS questions,
               {_outcome_columns()},
               AVG(elapsed_seconds)        AS mean_elapsed,
               AVG(tool_calls)             AS mean_tool_calls
        FROM results WHERE run_id = ?
        GROUP BY fixture, isolation ORDER BY fixture
    """
    return [_split_outcomes(r) for r in _rows(conn.execute(sql, (run_id,)))]


def check_summary(conn: sqlite3.Connection, run_id: int) -> list[dict[str, Any]]:
    """Pass counts per fixture and check.

    This is the aggregate the browser cannot compute without being sent every `checks`
    row -- roughly 2000 of them for one run -- which is the whole reason it is here.

    Deliberately sparse rather than a fixed grid: the check set varies per fixture,
    because which checks run is conditional on that fixture's `expect` block. A caller
    that assumes every fixture has every check will be wrong.
    """
    sql = """
        SELECT r.fixture, c.category, c.name,
               COUNT(*) AS total, COALESCE(SUM(c.passed), 0) AS passed
        FROM checks c JOIN results r ON r.id = c.result_id
        WHERE r.run_id = ?
        GROUP BY r.fixture, c.category, c.name
    """
    return order_checks(_rows(conn.execute(sql, (run_id,))))


# The three check subqueries are lifted from store.pending_judgements: the same shape,
# served by the same idx_checks_result index, and the same signals the judge queue uses to
# decide what deserves a second opinion. A reader wanting the interesting results first
# and the judge wanting them first are the same question.
_RESULTS_SQL = """
SELECT r.id, r.fixture, r.question_id, r.iteration, r.outcome, r.passed,
       r.elapsed_seconds, r.tool_calls, r.http_status, r.error,
       (SELECT COUNT(*) FROM checks c WHERE c.result_id = r.id AND c.passed = 0)
           AS failed_checks,
       (SELECT COUNT(*) FROM checks c WHERE c.result_id = r.id) AS total_checks,
       (SELECT COUNT(*) FROM checks c WHERE c.result_id = r.id
          AND c.category = 'remediation' AND c.passed = 0) AS failed_remediation,
       -- Most recently WRITTEN judgement, by id -- never ORDER BY rubric_version. That
       -- column is content-addressed ("answer_v2-1a2b3c4d"), so it sorts lexically and
       -- "answer_v10-..." would beat "answer_v2-...".
       (SELECT j.verdict FROM judgements j WHERE j.result_id = r.id
         {rubric} ORDER BY j.id DESC LIMIT 1) AS judge_verdict,
       (SELECT j.score FROM judgements j WHERE j.result_id = r.id
         {rubric} ORDER BY j.id DESC LIMIT 1) AS judge_score,
       (SELECT j.remediation_score FROM judgements j WHERE j.result_id = r.id
         {rubric} ORDER BY j.id DESC LIMIT 1) AS judge_remediation_score,
       (SELECT j.rubric_version FROM judgements j WHERE j.result_id = r.id
         {rubric} ORDER BY j.id DESC LIMIT 1) AS judge_rubric_version
FROM results r
WHERE r.run_id = ?
"""


def list_results(
    conn: sqlite3.Connection,
    run_id: int,
    *,
    fixture: str | None = None,
    outcome: str | None = None,
    passed: int | None = None,
    order: str = "natural",
    rubric: str | None = None,
    limit: int = 500,
    offset: int = 0,
) -> dict[str, Any]:
    """Result summaries for one run.

    Carries no free text -- no answer, no question_text, no rationale. A run is ~200
    results and an answer is kilobytes, so including them would make a list view
    megabytes. The prose arrives from get_result, one result at a time.
    """
    clause = "AND j.rubric_version = ?" if rubric else ""
    sql = _RESULTS_SQL.replace("{rubric}", clause)
    args: list[Any] = [rubric] * (4 if rubric else 0) + [run_id]

    for column, value in (("r.fixture", fixture), ("r.outcome", outcome), ("r.passed", passed)):
        if value is not None:
            sql += f" AND {column} = ?"
            args.append(value)

    if order == "interesting":
        # store.pending_judgements' ranking, expressed in SQL rather than re-sorted in
        # Python: a failed remediation first (the headline category), then results whose
        # checks disagreed with each other, then the unanimous ones.
        sql += """
        ORDER BY (failed_remediation > 0) DESC,
                 (failed_checks > 0 AND failed_checks < total_checks) DESC,
                 r.id DESC
        """
    else:
        sql += " ORDER BY r.fixture, r.question_id, r.iteration"

    sql += " LIMIT ? OFFSET ?"
    args += [limit, offset]

    count_args = [run_id] + [v for v in (fixture, outcome, passed) if v is not None]
    count_sql = "SELECT COUNT(*) AS c FROM results r WHERE r.run_id = ?" + "".join(
        f" AND {c} = ?"
        for c, v in (("r.fixture", fixture), ("r.outcome", outcome), ("r.passed", passed))
        if v is not None
    )
    total = conn.execute(count_sql, count_args).fetchone()["c"]
    return {
        "results": _rows(conn.execute(sql, args)),
        "limit": limit,
        "offset": offset,
        "total": total,
    }


def rubric_versions(conn: sqlite3.Connection, run_id: int) -> list[str]:
    """Rubric versions present in this run, most recently written first."""
    sql = """
        SELECT j.rubric_version, MAX(j.id) AS latest
        FROM judgements j JOIN results r ON r.id = j.result_id
        WHERE r.run_id = ? GROUP BY j.rubric_version ORDER BY latest DESC
    """
    return [r["rubric_version"] for r in _rows(conn.execute(sql, (run_id,)))]


# --------------------------------------------------------------- level three


def get_result(conn: sqlite3.Connection, result_id: int) -> dict[str, Any] | None:
    row = conn.execute("SELECT * FROM results WHERE id = ?", (result_id,)).fetchone()
    if row is None:
        return None
    result = dict(row)

    errors: list[str] = []
    steps, err = _loads(result.pop("steps_json", None), [], "steps_json")
    if err:
        errors.append(err)
    remediation, err = _loads(result.pop("remediation_json", None), None, "remediation_json")
    if err:
        errors.append(err)
    result["steps"] = steps
    result["remediation"] = remediation
    if errors:
        result["json_errors"] = errors

    checks = _rows(
        conn.execute(
            "SELECT category, name, passed, detail FROM checks WHERE result_id = ?",
            (result_id,),
        )
    )
    # raw_json is deliberately absent: it is the judge model's unparsed reply, it is
    # large, and `rationale` is the parsed form of the same content.
    judgements = _rows(
        conn.execute(
            """SELECT judge_model, rubric_version, verdict, score, remediation_score,
                      rationale, error, created_at
               FROM judgements WHERE result_id = ? ORDER BY id DESC""",
            (result_id,),
        )
    )
    run = conn.execute(
        """SELECT id, started_at, finished_at, status, agent_model, harness_version, git_sha
           FROM runs WHERE id = ?""",
        (result["run_id"],),
    ).fetchone()

    return {
        "result": result,
        "run": dict(run) if run else None,
        "checks": order_checks(checks),
        "category_order": list(CATEGORY_ORDER),
        "judgements": judgements,
    }
