"""The read service: routes, envelopes, and the empty state.

Driven through FastAPI's own test client -- no new dependency and no real socket. What
is checked here is the HTTP contract the browser depends on: the {"error": ...}
envelope, the status vocabulary, and that a database which does not exist yet is an
empty view rather than an outage.
"""

from __future__ import annotations

from dataclasses import replace

import pytest
from fastapi.testclient import TestClient

from fathom_eval import config, serve, store
from fathom_eval.grading import Check
from test_reporting import RUBRIC_V2, a_result


@pytest.fixture
def cfg(tmp_path):
    base = config.load()
    return replace(base, db_path=str(tmp_path / "results.db"))


@pytest.fixture
def populated(cfg):
    with store.opened(cfg.db_path) as conn:
        run = store.start_run(conn, harness_version="0.5.4", agent_model="fathom-agent")
        result = store.record_result(
            conn,
            run,
            a_result(),
            [Check("naming", "names_resource", True), Check("scope", "no_secret_leak", True)],
        )
        store.record_judgement(
            conn,
            result,
            judge_model="fathom-judge",
            rubric_version=RUBRIC_V2,
            verdict="pass",
            score=5,
            remediation_score=4,
            rationale="named the pod and the cause",
            raw_json='{"verdict": "pass"}',
        )
        store.finish_run(conn, run, "ok")
    return cfg


def get(cfg, path, method="GET"):
    """One request. Returns (status, parsed-json-or-text).

    `raise_server_exceptions=False` so an exception handler is exercised as the client
    would see it -- a sqlite3.Error has to become a 500 carrying the envelope, and a
    test client that re-raises instead would never prove that.
    """
    with TestClient(serve.build_app(cfg), raise_server_exceptions=False) as client:
        resp = client.request(method, path)
        try:
            return resp.status_code, resp.json()
        except Exception:
            return resp.status_code, resp.text


# ------------------------------------------------------------- the empty state


def test_healthz_is_ok_without_a_database(cfg):
    assert get(cfg, "/healthz")[0] == 200


def test_readyz_is_ok_without_a_database(cfg):
    """A cluster whose CronJob has never run is a correct state, not an unready pod."""
    status, text = get(cfg, "/readyz")
    assert status == 200
    assert "no database yet" in text


def test_readyz_fails_when_the_volume_is_not_mounted(cfg):
    status, text = get(replace(cfg, db_path="/definitely/not/mounted/results.db"), "/readyz")
    assert status == 503
    assert "not mounted" in text


def test_runs_is_an_empty_view_without_a_database(cfg):
    status, body = get(cfg, "/runs")
    assert status == 200
    assert body["empty"] is True and body["runs"] == []
    assert cfg.db_path in body["reason"]


def test_reading_does_not_create_the_database(cfg):
    import os

    get(cfg, "/runs")
    assert not os.path.exists(cfg.db_path)


# ------------------------------------------------------------------- the routes


def test_runs_lists_the_run(populated):
    _, body = get(populated, "/runs")
    assert body["total"] == 1
    assert body["runs"][0]["status"] == "ok"
    assert body["runs"][0]["outcomes"]["ok"] == 1


def test_run_detail_carries_the_aggregates(populated):
    _, body = get(populated, "/runs/1")
    assert body["run"]["id"] == 1
    assert body["fixtures"][0]["fixture"] == "crashloop"
    assert body["checks"][0]["name"]
    assert body["rubric_versions"] == [RUBRIC_V2]
    assert body["category_order"][0] == "termination"


def test_run_detail_carries_no_free_text(populated):
    _, body = get(populated, "/runs/1")
    for row in body["results"]:
        assert "answer" not in row and "question_text" not in row


def test_result_detail_carries_the_prose(populated):
    _, body = get(populated, "/results/1")
    assert body["result"]["answer"].startswith("crashloop-app")
    assert body["result"]["remediation"]["action_required"] is True
    assert body["result"]["steps"][0]["tool"] == "k8s_describe"
    assert body["judgements"][0]["rationale"]


def test_raw_json_is_never_served(populated):
    """The judge's unparsed reply is large, and rationale is the parsed form of it."""
    _, body = get(populated, "/results/1")
    assert "raw_json" not in body["judgements"][0]


# ---------------------------------------------------------------- the envelope


def test_unknown_path_is_a_json_error(populated):
    status, body = get(populated, "/nope")
    assert status == 404
    # The same envelope fathom-front's proxy emits, so app.js's errorMessage() parses
    # a failure from here identically to one from the agent.
    assert "error" in body


def test_post_is_rejected(populated):
    status, body = get(populated, "/runs", method="POST")
    assert status == 405
    assert "error" in body


def test_a_non_integer_id_is_a_422(populated):
    """422 rather than 400 since 2026-08-23: the parameter is validated by FastAPI, so
    the published spec can declare a status that actually happens. The envelope is
    unchanged, which is the part evals.js depends on."""
    status, body = get(populated, "/results/abc")
    assert status == 422
    assert "integer" in body["error"]


def test_a_missing_run_is_a_404(populated):
    status, body = get(populated, "/runs/999")
    assert status == 404
    assert "no run 999" in body["error"]


def test_a_bad_limit_is_a_422(populated):
    """Both the unparseable and the negative case, and both carry the envelope."""
    status, body = get(populated, "/runs?limit=lots")
    assert status == 422
    assert "error" in body
    assert get(populated, "/runs?limit=-1")[0] == 422


def test_limit_is_clamped_to_max_rows(populated):
    _, body = get(replace(populated, serve_max_rows=1), "/runs?limit=9999")
    assert body["limit"] == 1


def test_a_newer_schema_is_refused(populated):
    import sqlite3

    with sqlite3.connect(populated.db_path) as w:
        w.execute("UPDATE meta SET value='99' WHERE key='schema_version'")
    status, body = get(populated, "/runs")
    assert status == 503
    assert "older than the harness" in body["error"]


# ------------------------------------------------------------------------ cli


def test_cli_parses_serve():
    from fathom_eval.cli import _parser

    args = _parser().parse_args(["serve", "--db", "/tmp/x.db", "--addr", ":9999"])
    assert args.command == "serve" and args.serve_addr == ":9999"


def test_a_bad_serve_addr_fails_the_same_way_as_the_env_var():
    """The CLI must not be a way around the checks -- override re-validates."""
    with pytest.raises(ValueError, match="SERVE_ADDR"):
        config.override(config.load(), serve_addr="not-an-address")


# ------------------------------------------------------- the published contract


def test_the_openapi_spec_describes_every_route(cfg):
    """`contracts/eval/openapi.json` is scraped from a running container's own
    `/openapi.json`, so an undocumented route is an absence in the published contract
    rather than a visible bug. This asserts the shape the scraper depends on, without a
    second copy of it anywhere in the service."""
    spec = serve.build_app(cfg).openapi()

    assert sorted(spec["paths"]) == [
        "/healthz",
        "/meta",
        "/readyz",
        "/results/{id}",
        "/runs",
        "/runs/{id}",
    ]

    for path in ("/runs", "/runs/{id}", "/results/{id}"):
        responses = spec["paths"][path]["get"]["responses"]
        # The two states a data route can answer 200 with, and the two it can fail with.
        # 422 not 400: the parameters are FastAPI-validated, so the declared status is
        # one that can actually occur.
        assert "200" in responses
        assert {"422", "503"} <= set(responses), path
    # The empty-database state is part of the 200, not an error status.
    runs_ok = spec["paths"]["/runs"]["get"]["responses"]["200"]
    assert "anyOf" in runs_ok["content"]["application/json"]["schema"]


def test_response_models_drop_nothing(populated):
    """A response_model filters the body it serialises, so it can silently delete a
    field `evals.js` reads. That failure is invisible in the spec -- the spec would
    simply stop mentioning it -- so it is pinned here instead.

    The row objects are `dict[str, Any]` precisely so this cannot happen inside them;
    this proves the envelope does not narrow either.
    """
    from fathom_eval import reporting

    conn = reporting.connect_ro(populated.db_path)
    try:
        expected = reporting.list_runs(conn, limit=50, offset=0)
    finally:
        conn.close()

    status, body = get(populated, "/runs")

    assert status == 200
    assert set(body) == set(expected)
    assert set(body["runs"][0]) == set(expected["runs"][0])

    # And the detail envelope, which is the one that carries the SELECT * row.
    status, detail = get(populated, "/runs/1")
    assert status == 200
    assert {
        "run",
        "fixtures",
        "checks",
        "category_order",
        "rubric_versions",
        "rubric",
        "results",
        "limit",
        "offset",
        "total",
    } <= set(detail)
