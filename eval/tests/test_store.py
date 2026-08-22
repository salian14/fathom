"""The results store: schema, crash safety, and the judge's work queue."""

from __future__ import annotations

import json

import pytest

from fathom_eval import store
from fathom_eval.grading import Check


@pytest.fixture
def conn(tmp_path):
    with store.opened(str(tmp_path / "results.db")) as c:
        yield c


def a_result(**over):
    base = {
        "fixture": "crashloop",
        "concurrency": "shared",
        "namespace": "fathom-eval-crashloop",
        "question_id": "q1",
        "question_text": "why?",
        "iteration": 1,
        "http_status": 200,
        "outcome": "ok",
        "answer": "crashloop-app is in CrashLoopBackOff",
        "steps": [{"tool": "k8s_describe", "args": {}}],
        "tool_calls": 1,
        "elapsed_seconds": 42.0,
    }
    base.update(over)
    return base


def test_schema_is_created_and_versioned(conn):
    version = conn.execute("SELECT value FROM meta WHERE key='schema_version'").fetchone()
    assert int(version["value"]) == store.SCHEMA_VERSION


def test_a_result_and_its_checks_land_together(conn):
    run_id = store.start_run(conn, harness_version="0.5.1")
    checks = [
        Check("naming", "names_resource", True, ""),
        Check("scope", "no_mutation_claim", True),
    ]
    result_id = store.record_result(conn, run_id, a_result(), checks)

    row = conn.execute("SELECT * FROM results WHERE id=?", (result_id,)).fetchone()
    assert row["passed"] == 1
    assert row["question_text"] == "why?"
    assert json.loads(row["steps_json"])[0]["tool"] == "k8s_describe"
    assert (
        conn.execute("SELECT COUNT(*) c FROM checks WHERE result_id=?", (result_id,)).fetchone()[
            "c"
        ]
        == 2
    )


def test_one_failed_check_fails_the_result(conn):
    run_id = store.start_run(conn)
    result_id = store.record_result(
        conn, run_id, a_result(), [Check("naming", "names_resource", False, "missed")]
    )
    assert (
        conn.execute("SELECT passed FROM results WHERE id=?", (result_id,)).fetchone()["passed"]
        == 0
    )


def test_unfinished_runs_are_swept_on_the_next_start(conn):
    # A killed CronJob cannot record its own death, so the next run does it. Otherwise
    # the row sits as 'running' forever and poisons every "latest run" query.
    store.start_run(conn)
    assert store.sweep_abandoned(conn) == 1
    assert conn.execute("SELECT status FROM runs").fetchone()["status"] == "abandoned"
    # Idempotent: a second sweep finds nothing.
    assert store.sweep_abandoned(conn) == 0


def test_finished_runs_are_not_swept(conn):
    run_id = store.start_run(conn)
    store.finish_run(conn, run_id, "ok")
    assert store.sweep_abandoned(conn) == 0


def test_judge_queue_excludes_already_judged_rows_at_this_rubric(conn):
    run_id = store.start_run(conn)
    result_id = store.record_result(conn, run_id, a_result(), [Check("naming", "n", True)])

    assert len(store.pending_judgements(conn, "v1", sample=0)) == 1
    store.record_judgement(conn, result_id, judge_model="gemma4:31b", rubric_version="v1", score=5)
    assert store.pending_judgements(conn, "v1", sample=0) == []
    # A new rubric makes the same rows pending again, which is what lets a rubric edit
    # re-judge exactly the work it invalidated.
    assert len(store.pending_judgements(conn, "v2", sample=0)) == 1


def test_judgement_write_is_idempotent(conn):
    run_id = store.start_run(conn)
    result_id = store.record_result(conn, run_id, a_result(), [Check("naming", "n", True)])
    for score in (3, 5):
        store.record_judgement(
            conn, result_id, judge_model="gemma4:31b", rubric_version="v1", score=score
        )
    rows = conn.execute("SELECT score FROM judgements WHERE result_id=?", (result_id,)).fetchall()
    assert [r["score"] for r in rows] == [5]


def test_contested_results_are_judged_before_unanimous_ones(conn):
    run_id = store.start_run(conn)
    unanimous = store.record_result(
        conn, run_id, a_result(iteration=1), [Check("a", "x", True), Check("a", "y", True)]
    )
    contested = store.record_result(
        conn, run_id, a_result(iteration=2), [Check("a", "x", True), Check("a", "y", False)]
    )
    order = [r["id"] for r in store.pending_judgements(conn, "v1", sample=0)]
    assert order.index(contested) < order.index(unanimous)


def test_sampling_caps_per_fixture_and_question(conn):
    run_id = store.start_run(conn)
    for i in range(1, 11):
        store.record_result(conn, run_id, a_result(iteration=i), [Check("a", "x", True)])
    assert len(store.pending_judgements(conn, "v1", sample=3)) == 3
    assert len(store.pending_judgements(conn, "v1", sample=0)) == 10
