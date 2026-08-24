"""A read-only HTTP view of the results database.

SQLite is an embedded library, not a server, so `/data/results.db` is readable only by a
process that has the volume mounted. fathom-front lives in another namespace and a PVC
cannot be mounted across one. This module is what gives the file a server -- the thing a
real database pod would have provided for free.

It reports rows, not conclusions. Aggregation that the client cannot do without being
sent thousands of rows happens in SQL (see `reporting`); the rendering lives in
fathom-front. `eval/README.md`'s "reporting is deliberately not here" survives that split.

Two rules the routes follow:

- **List endpoints carry no free text.** A run is ~200 results and an answer is
  kilobytes, so `answer`, `question_text` and `rationale` appear only in the detail
  endpoint, one result at a time.
- **A missing database is not an error.** A cluster whose CronJob has never run is a
  correct state of the system and renders as an empty view. `/readyz` therefore reports
  on the mount, not on the data.

## Why FastAPI here and not aiohttp

This was aiohttp until 2026-08-23, chosen because the kubernetes client already pulls it
and FastAPI would add starlette, uvicorn and pydantic to a batch image for six read-only
routes. That reasoning was sound about image weight and wrong about what this component
is: it is the only pure JSON API in the repo, its responses are consumed across a
namespace boundary by `front/internal/web/static/evals.js`, and aiohttp publishes no
machine-readable description of any of it.

FastAPI publishes `/openapi.json` itself, which `scripts/contracts.py` scrapes from a
running container exactly as it does for `fathom-agent` -- so `contracts/eval/openapi.json`
costs this service no describe-myself code at all. That is the trade: three wheels in the
image, in exchange for the response contract being generated rather than hand-written
prose that drifts from `evals.js` with nothing to notice.

**The response models below must never narrow a payload.** They exist to publish the
envelope, not to filter it. Every one carries `extra="allow"`, and
`test_response_models_drop_nothing` drives a real request and asserts that what
`reporting` produced is what the client receives -- because a `response_model` that
silently deletes a field is invisible in the spec, which would simply stop mentioning it.
The row objects inside each envelope are `dict[str, Any]` on purpose: they are SQL
projections, and restating their columns here would be the second source of truth this
repo keeps avoiding.
"""

from __future__ import annotations

import logging
import os
import sqlite3
from typing import Annotated, Any

import uvicorn
from fastapi import Depends, FastAPI, HTTPException, Query, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, PlainTextResponse
from pydantic import BaseModel, ConfigDict, Field
from starlette.exceptions import HTTPException as StarletteHTTPException

from . import reporting
from .config import Config

log = logging.getLogger(__name__)

# Probes produce no spans anywhere in Fathom. Passed to the instrumentor at construction
# rather than left to the chart -- the exclusion is a property of the code, not of a
# deployment. Spelled the same way fathom-agent spells it, deliberately.
UNTRACED = "healthz,readyz"


# --- the published envelopes -----------------------------------------------------
#
# Optional-but-required-together is what discriminates the two 200 shapes each list
# route can return: a normal payload always carries `limit`, and an empty-database
# payload always carries `empty` and `reason`. Keeping those required in their own
# models is what lets the union resolve without a discriminator field.


class Error(BaseModel):
    """The one error envelope. `evals.js` parses this for every failing status, and so
    do fathom-agent and fathom-front's proxy -- one shape per client, not one per
    failure origin."""

    error: str


class EmptyState(BaseModel):
    """No results database yet.

    **A 200, not a 404 and not a 503.** A cluster whose CronJob has never run is a
    correct state of the system and renders as an empty view; a client that treats this
    as an outage shows a red banner on every fresh install.
    """

    model_config = ConfigDict(extra="allow")

    empty: bool
    reason: str
    runs: list[dict[str, Any]] = Field(default_factory=list)
    results: list[dict[str, Any]] = Field(default_factory=list)


class Meta(BaseModel):
    """What database this service is reading and what it will serve."""

    model_config = ConfigDict(extra="allow")

    # Path, presence, size and mtime. Present even when the file is not.
    database: dict[str, Any]
    # The ceiling this service applies to any `limit`, silently.
    max_rows: int
    # Absent until there is a database to read it from.
    schema_version: int | None = None


class RunList(BaseModel):
    """The eval runs, newest first."""

    model_config = ConfigDict(extra="allow")

    runs: list[dict[str, Any]]
    limit: int
    offset: int
    total: int


class RunDetail(BaseModel):
    """One run, its per-fixture and per-check summaries, and a page of its results."""

    model_config = ConfigDict(extra="allow")

    run: dict[str, Any]
    fixtures: list[dict[str, Any]]
    checks: list[dict[str, Any]]
    # grading.CATEGORY_ORDER: the reporting priority the UI renders checks in.
    category_order: list[str]
    # Every rubric version present in this run, most recently written first.
    rubric_versions: list[str]
    # The one being reported, from ?rubric or the newest. Null when nothing was judged.
    rubric: str | None = None
    results: list[dict[str, Any]]
    limit: int
    offset: int
    total: int


class ResultDetail(BaseModel):
    """One result in full -- the only endpoint carrying free text.

    The rows inside stay open on purpose. `result` is a `results` row (`SELECT *`) plus
    the decoded `steps` and `remediation`, and `json_errors` when a stored JSON column
    failed to parse -- its presence is the signal, and the row is still served, because
    a bad column should not hide the result. Those columns are the database's to declare
    and `schema.sql` is where they are published.

    **`raw_json` is deliberately absent** from the judgements here: it is the judge
    model's unparsed reply, and it is not part of this interface.
    """

    model_config = ConfigDict(extra="allow")

    result: dict[str, Any]
    run: dict[str, Any]
    checks: list[dict[str, Any]]
    # grading.CATEGORY_ORDER: the reporting priority the UI renders checks in.
    category_order: list[str]
    judgements: list[dict[str, Any]]


# --- helpers ---------------------------------------------------------------------


def _json(payload: Any, status: int = 200) -> JSONResponse:
    return JSONResponse(payload, status_code=status)


def _error(message: str, status: int) -> JSONResponse:
    return _json({"error": message}, status)


def _capped(value: int | None, default: int, ceiling: int) -> int:
    """Apply the row ceiling. Validation itself is FastAPI's job -- see below.

    The cap is silent by design: a client asking for more than `serve_max_rows` gets the
    cap rather than an error, so `total` is the only honest way to learn how many rows
    exist.
    """
    return min(default if value is None else value, ceiling)


# Validated by FastAPI rather than by hand, which is what keeps the published spec
# honest: the parameter is typed `integer` with `minimum: 0` because that is what is
# enforced, and the 422 the spec declares is one that can actually happen. The handler
# in build_app translates it into this service's `{"error": ...}` envelope, so a client
# still parses one shape for every failing status -- see evals.js.
Rows = Annotated[int | None, Query(ge=0, description="Silently capped at meta.max_rows.")]
Offset = Annotated[int | None, Query(ge=0)]


def build_app(cfg: Config) -> FastAPI:
    app = FastAPI(
        title="fathom-eval-serve",
        summary="A read-only HTTP view of the eval results database.",
    )
    app.state.cfg = cfg

    # --- the {"error": ...} envelope on every failure path -----------------------
    #
    # Starlette's defaults are `{"detail": ...}` for an HTTPException and an HTML page
    # for an unhandled one. `evals.js` parses one envelope for every status it sees, so
    # these translate rather than letting two shapes reach it.

    # Registered on Starlette's HTTPException, not FastAPI's subclass: a 404 for an
    # unknown path and a 405 for a wrong method are raised by the router itself, and a
    # handler bound to the subclass would never see them.
    @app.exception_handler(StarletteHTTPException)
    async def _http_error(request: Request, exc: StarletteHTTPException) -> JSONResponse:
        if exc.status_code == 405:
            return _error("only GET is accepted here", 405)
        if exc.status_code == 404 and exc.detail == "Not Found":
            return _error(f"no such path: {request.url.path}", 404)
        return _error(str(exc.detail), exc.status_code)

    @app.exception_handler(RequestValidationError)
    async def _validation_error(request: Request, exc: RequestValidationError) -> JSONResponse:
        """FastAPI's own 422 body is `{"detail": [...]}`. This service answers with the
        one envelope every other status uses, so `evals.js` parses one shape."""
        first = exc.errors()[0] if exc.errors() else {}
        where = ".".join(str(p) for p in first.get("loc", ())[1:]) or "request"
        return _error(f"{where}: {first.get('msg', 'invalid')}", 422)

    @app.exception_handler(sqlite3.Error)
    async def _sqlite_error(request: Request, exc: sqlite3.Error) -> JSONResponse:
        log.exception("query failed")
        return _error(f"query failed: {exc}", 500)

    @app.middleware("http")
    async def _no_store(request: Request, call_next):
        response = await call_next(request)
        response.headers["Cache-Control"] = "no-store"
        return response

    # --- probes ------------------------------------------------------------------

    @app.get(
        "/healthz",
        response_class=PlainTextResponse,
        summary="Liveness: this process is up. Never traced.",
    )
    async def healthz() -> str:
        return "ok\n"

    @app.get(
        "/readyz",
        response_class=PlainTextResponse,
        summary="Readiness: /data is mounted and readable.",
        responses={503: {"description": "The data directory is not mounted, or not readable."}},
    )
    async def readyz():
        # Reports on the mount, not the data. A database that does not exist yet is a
        # correct state; a /data that is not there is a deploy fault and should keep this
        # pod out of its Service.
        info = reporting.database_info(cfg.db_path)
        directory = os.path.dirname(cfg.db_path) or "."
        if not os.path.isdir(directory):
            return PlainTextResponse(f"{directory} is not mounted\n", status_code=503)
        if not os.access(directory, os.R_OK):
            return PlainTextResponse(f"{directory} is not readable\n", status_code=503)
        state = "database present" if info["present"] else "no database yet"
        return PlainTextResponse(f"ok ({state})\n")

    # --- data --------------------------------------------------------------------

    @app.get("/meta", summary="What database this service is reading and what it will serve.")
    async def meta() -> Meta:
        info = reporting.database_info(cfg.db_path)
        payload: dict[str, Any] = {"database": info, "max_rows": cfg.serve_max_rows}
        if info["present"]:
            conn = reporting.connect_ro(cfg.db_path)
            try:
                payload["schema_version"] = reporting.schema_version(conn)
            finally:
                conn.close()
        return Meta(**payload)

    @app.get(
        "/runs",
        response_model=RunList | EmptyState,
        summary="The eval runs, newest first.",
        responses=_DATA_RESPONSES,
    )
    async def runs(
        conn: DbConn,
        limit: Rows = None,
        offset: Offset = None,
    ):
        if conn is None:
            return _empty(cfg)
        return reporting.list_runs(
            conn,
            limit=_capped(limit, 50, cfg.serve_max_rows),
            offset=offset or 0,
        )

    @app.get(
        "/runs/{id}",
        response_model=RunDetail | EmptyState,
        summary="One run, with its fixture and check summaries and a page of results.",
        responses={**_DATA_RESPONSES, 404: {"model": Error, "description": "No such run."}},
    )
    async def run_detail(
        id: int,
        conn: DbConn,
        rubric: Annotated[str | None, Query(description="Defaults to the newest.")] = None,
        fixture: str | None = None,
        outcome: str | None = None,
        order: Annotated[str, Query(description='"natural" or "interesting".')] = "natural",
        limit: Rows = None,
        offset: Offset = None,
    ):
        if conn is None:
            return _empty(cfg)
        run_id = id
        run = reporting.get_run(conn, run_id)
        if run is None:
            raise HTTPException(status_code=404, detail=f"no run {run_id}")
        versions = reporting.rubric_versions(conn, run_id)
        chosen = rubric or (versions[0] if versions else None)
        listing = reporting.list_results(
            conn,
            run_id,
            fixture=fixture,
            outcome=outcome,
            order=order,
            rubric=chosen,
            limit=_capped(limit, cfg.serve_max_rows, cfg.serve_max_rows),
            offset=offset or 0,
        )
        return {
            "run": run,
            "fixtures": reporting.fixture_summary(conn, run_id),
            "checks": reporting.check_summary(conn, run_id),
            "category_order": list(reporting.CATEGORY_ORDER),
            "rubric_versions": versions,
            "rubric": chosen,
            **listing,
        }

    @app.get(
        "/results/{id}",
        response_model=ResultDetail | EmptyState,
        summary="One result in full. The only endpoint carrying free text.",
        responses={**_DATA_RESPONSES, 404: {"model": Error, "description": "No such result."}},
    )
    async def result_detail(
        id: int,
        conn: DbConn,
    ):
        if conn is None:
            return _empty(cfg)
        result_id = id
        detail = reporting.get_result(conn, result_id)
        if detail is None:
            raise HTTPException(status_code=404, detail=f"no result {result_id}")
        return detail

    return app


# Shared by the three data routes. Declared once so the three cannot drift apart.
_DATA_RESPONSES: dict[int | str, dict[str, Any]] = {
    422: {
        "model": Error,
        "description": "A query or path parameter did not validate. "
        "Carries this service's {\"error\": ...} envelope, not FastAPI's default "
        '{"detail": [...]}.',
    },
    503: {
        "model": Error,
        "description": "The database is unreadable, or its schema is newer than this "
        "reader's. A schema-too-new 503 will not resolve on retry: roll the reader "
        "forward. Best-effort parsing would report wrong numbers, which is worse than "
        "reporting nothing.",
    },
}


def _empty(cfg: Config) -> dict[str, Any]:
    return {
        "empty": True,
        "reason": f"no results database yet at {cfg.db_path}",
        "runs": [],
        "results": [],
    }


async def _db(request: Request):
    """Open the database per request, hand it to the route, always close it.

    Per request rather than once at startup: SQLite connections are not thread-safe
    across threads, opening costs microseconds, and -- the argument that settles it --
    re-checking on every request means the view starts working the moment the first run
    lands, with no pod restart.

    Yields None rather than raising when there is no database, because that is the empty
    state above and not a failure.

    Reads its config off `app.state` rather than closing over it. It has to be a plain
    module-level function: this module uses postponed annotations, so a route's
    `Depends(...)` marker is evaluated as a string in module scope, where a closure
    variable does not exist -- and FastAPI then silently treats the parameter as a query
    parameter and answers 422 to every request.

    **`async def`, and that is load-bearing.** A sync dependency is run in a threadpool,
    so the connection would be opened in one thread and used by the route handler in
    another -- which SQLite refuses outright ("objects created in a thread can only be
    used in that same thread"). Async keeps the open, the queries and the close on the
    event loop thread, which is the same single-threaded model the aiohttp version had.
    The blocking reads stay on the loop for the same reason they did there: they are
    local file reads bounded by `serve_max_rows`, not network calls.
    """
    cfg: Config = request.app.state.cfg
    try:
        conn = reporting.connect_ro(cfg.db_path)
    except reporting.MissingDatabase:
        yield None
        return
    except reporting.UnreadableDatabase as err:
        log.warning("results database unreadable: %s", err)
        raise HTTPException(status_code=503, detail=f"results database unreadable: {err}") from err
    try:
        reporting.check_schema(conn)
    except reporting.SchemaTooNew as err:
        conn.close()
        raise HTTPException(status_code=503, detail=str(err)) from err
    try:
        yield conn
    finally:
        conn.close()


# `Any` rather than `sqlite3.Connection | None`: the value is supplied by the dependency
# and never validated, and naming a non-pydantic type here only invites a schema build.
DbConn = Annotated[Any, Depends(_db)]


def serve(cfg: Config) -> int:
    """Run the read service until SIGTERM."""
    app = build_app(cfg)
    _instrument(app)
    host, _, port = cfg.serve_addr.rpartition(":")
    log.info("serving results from %s on %s", cfg.db_path, cfg.serve_addr)
    # uvicorn installs its own SIGTERM/SIGINT handling and drains in-flight requests
    # before returning, which is what lets telemetry.shutdown() in __main__ flush the
    # span batch instead of being SIGKILLed with it still queued.
    uvicorn.run(app, host=host or "0.0.0.0", port=int(port), log_config=None, access_log=False)
    return 0


def _instrument(app: FastAPI) -> None:
    """Wrap the inbound HTTP boundary once, at construction.

    Auto-instrumentation rather than hand-rolled spans, per the repo's telemetry rules,
    and the same call fathom-agent makes. Degrades to no-op if the package is absent so
    a missing optional dependency cannot stop the service from serving.
    """
    try:
        from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
    except ImportError:  # pragma: no cover - exercised only on a partial install
        log.warning("fastapi instrumentation unavailable; serving untraced")
        return
    FastAPIInstrumentor.instrument_app(app, excluded_urls=UNTRACED)
