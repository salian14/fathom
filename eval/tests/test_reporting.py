"""Read-only queries: what they return, and what they must never do.

The load-bearing test in here is the first one. Everything else checks a shape; that one
checks that a reader cannot bring a database into existence and shadow the writer's.
"""

from __future__ import annotations

import sqlite3

import pytest

from fathom_eval import reporting, store
from fathom_eval.grading import CATEGORY_ORDER, Check

RUBRIC_V2 = "answer_v2-1a2b3c4d"
RUBRIC_V1 = "answer_v1-99887766"


def a_result(**over):
    base = {
        "fixture": "crashloop",
        "isolation": "shared",
        "namespace": "fathom-eval-crashloop",
        "question_id": "q1",
        "question_text": "why is it broken?",
        "iteration": 1,
        "http_status": 200,
        "outcome": "ok",
        "answer": "crashloop-app is in CrashLoopBackOff",
        "remediation": {
            "action_required": True,
            "summary": "fix the command",
            "confidence": "high",
        },
        "steps": [
            {
                "tool": "k8s_describe",
                "args": {"kind": "pod"},
                "result_chars": 900,
                "status": "success",
            }
        ],
        "tool_calls": 1,
        "elapsed_seconds": 42.0,
    }
    base.update(over)
    return base


@pytest.fixture
def db(tmp_path):
    """A database with two runs: one populated, one that died writing nothing."""
    path = str(tmp_path / "results.db")
    with store.opened(path) as conn:
        run = store.start_run(
            conn,
            harness_version="0.5.4",
            agent_model="fathom-agent",
            git_sha="8957f22",
            iterations=10,
        )

        passing = store.record_result(
            conn,
            run,
            a_result(),
            [
                Check("naming", "names_resource", True),
                Check("remediation", "remediation_present", True),
                Check("termination", "terminated_cleanly", True),
            ],
        )
        # Contested: some checks passed, some failed -- and the failure is a remediation,
        # which is what the "interesting" ordering leads with.
        contested = store.record_result(
            conn,
            run,
            a_result(iteration=2),
            [
                Check("naming", "names_resource", True),
                Check("remediation", "remediation_keywords", False, "matched=[]"),
                Check("scope", "no_secret_leak", True),
            ],
        )
        # A different fixture, with a different check set entirely.
        store.record_result(
            conn,
            run,
            a_result(fixture="healthy", outcome="ceiling", iteration=1, remediation=None),
            [
                Check("termination", "within_turn_budget", False, "hit the ceiling"),
            ],
        )
        # The undocumented sixth outcome, written when a fixture never breaks properly.
        store.record_result(
            conn,
            run,
            a_result(fixture="pending", outcome="fixture_error", question_id="-", iteration=0),
            [
                Check("termination", "fixture_ready", False, "timed out"),
            ],
        )

        store.record_judgement(
            conn,
            passing,
            judge_model="fathom-judge",
            rubric_version=RUBRIC_V2,
            verdict="pass",
            score=5,
            remediation_score=4,
            rationale="named the pod",
        )
        # Judged under the older rubric, which had no remediation_score at all.
        store.record_judgement(
            conn,
            passing,
            judge_model="fathom-judge",
            rubric_version=RUBRIC_V1,
            verdict="pass",
            score=4,
            remediation_score=None,
            rationale="older rubric",
        )
        # A judgement whose model call failed: NULL verdict, populated error.
        store.record_judgement(
            conn,
            contested,
            judge_model="fathom-judge",
            rubric_version=RUBRIC_V2,
            verdict=None,
            score=None,
            remediation_score=None,
            error="HTTPError: timed out",
        )

        store.finish_run(conn, run, "ok")
        # A run that was killed before writing anything -- the LEFT JOIN case.
        store.start_run(conn, harness_version="0.5.4")
        store.sweep_abandoned(conn)
    return path


@pytest.fixture
def conn(db):
    c = reporting.connect_ro(db)
    yield c
    c.close()


# ------------------------------------------------------- the read-only property


def test_connect_ro_refuses_to_create_a_missing_file(tmp_path):
    missing = tmp_path / "nope.db"
    with pytest.raises(reporting.MissingDatabase):
        reporting.connect_ro(str(missing))
    # The assertion that matters. A reader that can create the file can shadow the
    # harness's database with a schemaless one.
    assert not missing.exists()


def test_connect_ro_refuses_writes(conn):
    with pytest.raises(sqlite3.OperationalError):
        conn.execute("INSERT INTO runs(started_at, status) VALUES('now', 'ok')")


def test_connect_ro_reads_a_wal_database_with_no_sidecars(db):
    """The harness sets journal_mode=WAL, and between runs nothing holds the file open.

    A read-only open of a WAL database can need to create the -shm index. This is the
    case that would fail if the results volume were ever mounted readOnly.
    """
    import os

    assert not os.path.exists(db + "-wal"), "writer closed cleanly, so no -wal remains"
    c = reporting.connect_ro(db)
    assert c.execute("SELECT COUNT(*) c FROM runs").fetchone()["c"] == 2
    c.close()


def test_schema_newer_than_the_code_is_refused(db):
    with sqlite3.connect(db) as w:
        w.execute(
            "UPDATE meta SET value=? WHERE key='schema_version'", (str(store.SCHEMA_VERSION + 1),)
        )
    c = reporting.connect_ro(db)
    with pytest.raises(reporting.SchemaTooNew) as err:
        reporting.check_schema(c)
    assert str(store.SCHEMA_VERSION) in str(err.value)
    c.close()


def test_database_info_is_safe_when_absent(tmp_path):
    assert reporting.database_info(str(tmp_path / "gone.db")) == {
        "present": False,
        "path": str(tmp_path / "gone.db"),
    }


# -------------------------------------------------------------------- level one


def test_runs_list_includes_a_run_that_wrote_nothing(conn):
    runs = reporting.list_runs(conn)["runs"]
    assert len(runs) == 2
    empty = [r for r in runs if r["results"] == 0]
    assert len(empty) == 1, "the abandoned run must not be hidden by the join"
    assert empty[0]["status"] == "abandoned"


def test_outcome_counts_include_fixture_error(conn):
    run = reporting.list_runs(conn)["runs"][1]
    assert run["outcomes"]["ok"] == 2
    assert run["outcomes"]["ceiling"] == 1
    # Absent from the enum in store.py's column comment; it is a real outcome.
    assert run["outcomes"]["fixture_error"] == 1


def test_judge_aggregates_are_grouped_by_rubric_version(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    by_version = {j["rubric_version"]: j for j in run["judge"]}
    assert set(by_version) == {RUBRIC_V1, RUBRIC_V2}
    # v1 carried no remediation_score. Averaged together with v2 this would silently
    # report v2's mean while claiming to cover both.
    assert by_version[RUBRIC_V1]["mean_remediation_score"] is None
    assert by_version[RUBRIC_V2]["mean_remediation_score"] == 4


def test_a_failed_judgement_counts_as_an_error_and_not_as_a_score(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    v2 = [j for j in run["judge"] if j["rubric_version"] == RUBRIC_V2][0]
    assert v2["judged"] == 2 and v2["errors"] == 1
    assert v2["mean_score"] == 5, "the NULL-verdict row must not drag the mean"


def test_get_run_and_list_runs_agree(conn):
    listed = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    assert reporting.get_run(conn, listed["id"]) == listed


def test_get_run_returns_none_for_a_missing_id(conn):
    assert reporting.get_run(conn, 9999) is None


# -------------------------------------------------------------------- level two


def test_fixture_summary_splits_by_fixture(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    rows = {f["fixture"]: f for f in reporting.fixture_summary(conn, run["id"])}
    assert set(rows) == {"crashloop", "healthy", "pending"}
    assert rows["crashloop"]["results"] == 2
    assert rows["healthy"]["outcomes"]["ceiling"] == 1


def test_check_summary_is_sparse_across_fixtures(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    rows = reporting.check_summary(conn, run["id"])
    names = {(r["fixture"], r["name"]) for r in rows}
    # The check set is conditional on each fixture's expect block, so this is deliberately
    # not a grid: healthy has no naming check at all.
    assert ("crashloop", "names_resource") in names
    assert ("healthy", "names_resource") not in names


def test_checks_are_ordered_by_category_order(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    seen = [r["category"] for r in reporting.check_summary(conn, run["id"])]
    rank = {c: i for i, c in enumerate(CATEGORY_ORDER)}
    assert seen == sorted(seen, key=lambda c: rank[c])


def test_unknown_category_sorts_last_rather_than_raising():
    ordered = reporting.order_checks(
        [
            {"category": "invented", "name": "z", "passed": 1},
            {"category": "termination", "name": "a", "passed": 1},
        ]
    )
    assert [r["category"] for r in ordered] == ["termination", "invented"]


def test_results_list_carries_no_free_text(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    row = reporting.list_results(conn, run["id"])["results"][0]
    for field in ("answer", "question_text", "rationale"):
        assert field not in row, f"{field} would make a list response megabytes"


def test_results_can_be_filtered(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    only = reporting.list_results(conn, run["id"], fixture="crashloop")
    assert only["total"] == 2
    assert {r["fixture"] for r in only["results"]} == {"crashloop"}


def test_interesting_ordering_leads_with_a_failed_remediation(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    rows = reporting.list_results(conn, run["id"], order="interesting")["results"]
    assert rows[0]["failed_remediation"] == 1


def test_latest_judgement_is_by_id_not_by_rubric_string(conn):
    """rubric_version is content-addressed, so it sorts lexically and would pick wrong."""
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    rows = {r["id"]: r for r in reporting.list_results(conn, run["id"])["results"]}
    judged = [r for r in rows.values() if r["judge_rubric_version"]]
    # RUBRIC_V1 was written last for the passing result, so "latest by id" picks it --
    # ordering by the version string would pick v2 because "answer_v2" > "answer_v1".
    passing = [r for r in judged if r["judge_score"] is not None][0]
    assert passing["judge_rubric_version"] == RUBRIC_V1


def test_results_can_be_pinned_to_one_rubric(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    rows = reporting.list_results(conn, run["id"], rubric=RUBRIC_V2)["results"]
    versions = {r["judge_rubric_version"] for r in rows if r["judge_rubric_version"]}
    assert versions == {RUBRIC_V2}


def test_rubric_versions_are_listed_newest_first(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    assert reporting.rubric_versions(conn, run["id"])[0] == RUBRIC_V2


# ------------------------------------------------------------------ level three


def test_result_detail_parses_remediation_and_steps(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    first = reporting.list_results(conn, run["id"])["results"][0]
    detail = reporting.get_result(conn, first["id"])

    assert detail["result"]["answer"].startswith("crashloop-app")
    assert detail["result"]["remediation"]["action_required"] is True
    assert detail["result"]["steps"][0]["tool"] == "k8s_describe"
    # The raw JSON columns are replaced, not duplicated.
    assert "steps_json" not in detail["result"]
    assert detail["run"]["harness_version"] == "0.5.4"
    assert detail["category_order"] == list(CATEGORY_ORDER)


def test_no_remediation_stays_null(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    healthy = reporting.list_results(conn, run["id"], fixture="healthy")["results"][0]
    assert reporting.get_result(conn, healthy["id"])["result"]["remediation"] is None


def test_raw_json_is_not_returned(conn):
    """The judge's unparsed reply is large and `rationale` is the parsed form of it."""
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    for row in reporting.list_results(conn, run["id"])["results"]:
        for judgement in reporting.get_result(conn, row["id"])["judgements"]:
            assert "raw_json" not in judgement


def test_a_failed_judgement_surfaces_its_error(conn):
    run = [r for r in reporting.list_runs(conn)["runs"] if r["results"]][0]
    rows = reporting.list_results(conn, run["id"], fixture="crashloop")["results"]
    errored = [
        j
        for row in rows
        for j in reporting.get_result(conn, row["id"])["judgements"]
        if j["verdict"] is None
    ]
    assert errored and errored[0]["error"].startswith("HTTPError")


def test_unparseable_steps_json_does_not_raise(db):
    with sqlite3.connect(db) as w:
        w.execute("UPDATE results SET steps_json='{not json' WHERE id=1")
    c = reporting.connect_ro(db)
    detail = c and reporting.get_result(c, 1)
    assert detail["result"]["steps"] == []
    assert any("steps_json" in e for e in detail["result"]["json_errors"])
    c.close()


def test_missing_result_returns_none(conn):
    assert reporting.get_result(conn, 9999) is None
