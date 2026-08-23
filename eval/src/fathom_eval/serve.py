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
"""

from __future__ import annotations

import json
import logging
import os
import sqlite3
from collections.abc import Callable
from typing import Any

from aiohttp import web

from . import reporting
from .config import Config

log = logging.getLogger(__name__)

# Probes produce no spans anywhere in Fathom. The aiohttp instrumentation reads this
# through OTEL_PYTHON_AIOHTTP_SERVER_EXCLUDED_URLS, which _instrument sets rather than
# leaving to the chart -- the exclusion is a property of the code, not of a deployment.
UNTRACED = "healthz,readyz"


def _json(payload: Any, status: int = 200) -> web.Response:
    return web.json_response(
        payload,
        status=status,
        dumps=lambda o: json.dumps(o, default=str),
        # Unversioned data that changes nightly, read by a page that is itself no-store.
        headers={"Cache-Control": "no-store"},
    )


def _error(message: str, status: int) -> web.Response:
    # The same {"error": ...} envelope fathom-agent and fathom-front's proxy use, so the
    # browser has one shape to parse rather than one per failure origin.
    return _json({"error": message}, status)


def _int_param(request: web.Request, name: str, default: int, ceiling: int | None = None) -> int:
    raw = request.query.get(name)
    if raw is None or raw == "":
        return default
    try:
        value = int(raw)
    except ValueError as err:
        raise web.HTTPBadRequest(text=f"{name} must be an integer, got {raw!r}") from err
    if value < 0:
        raise web.HTTPBadRequest(text=f"{name} must not be negative, got {value}")
    return min(value, ceiling) if ceiling is not None else value


def _path_id(request: web.Request, name: str) -> int:
    raw = request.match_info[name]
    if not raw.isdigit():
        raise web.HTTPBadRequest(text=f"{name} must be an integer, got {raw!r}")
    return int(raw)


def _with_db(cfg: Config, handler: Callable[[web.Request, sqlite3.Connection], Any]):
    """Open the database per request, hand it to the handler, always close it.

    Per request rather than once at startup: SQLite connections are not thread-safe
    across threads, opening costs microseconds, and -- the argument that settles it --
    re-checking on every request means the view starts working the moment the first run
    lands, with no pod restart.
    """

    async def wrapped(request: web.Request) -> web.Response:
        try:
            conn = reporting.connect_ro(cfg.db_path)
        except reporting.MissingDatabase:
            # Not an error. Nothing has run yet, which is the normal state of a fresh
            # install, and it should render as an empty view rather than a red banner.
            return _json(
                {
                    "empty": True,
                    "reason": f"no results database yet at {cfg.db_path}",
                    "runs": [],
                    "results": [],
                }
            )
        except reporting.UnreadableDatabase as err:
            log.warning("results database unreadable: %s", err)
            return _error(f"results database unreadable: {err}", 503)
        try:
            reporting.check_schema(conn)
            return await handler(request, conn)
        except reporting.SchemaTooNew as err:
            return _error(str(err), 503)
        finally:
            conn.close()

    return wrapped


def build_app(cfg: Config) -> web.Application:
    app = web.Application()

    async def healthz(_: web.Request) -> web.Response:
        return web.Response(text="ok\n")

    async def readyz(_: web.Request) -> web.Response:
        # Reports on the mount, not the data. A database that does not exist yet is a
        # correct state; a /data that is not there is a deploy fault and should keep this
        # pod out of its Service.
        info = reporting.database_info(cfg.db_path)
        directory = os.path.dirname(cfg.db_path) or "."
        if not os.path.isdir(directory):
            return web.Response(status=503, text=f"{directory} is not mounted\n")
        if not os.access(directory, os.R_OK):
            return web.Response(status=503, text=f"{directory} is not readable\n")
        state = "database present" if info["present"] else "no database yet"
        return web.Response(text=f"ok ({state})\n")

    async def meta(_: web.Request) -> web.Response:
        info = reporting.database_info(cfg.db_path)
        payload: dict[str, Any] = {"database": info, "max_rows": cfg.serve_max_rows}
        if info["present"]:
            conn = reporting.connect_ro(cfg.db_path)
            try:
                payload["schema_version"] = reporting.schema_version(conn)
            finally:
                conn.close()
        return _json(payload)

    async def runs(request: web.Request, conn: sqlite3.Connection) -> web.Response:
        return _json(
            reporting.list_runs(
                conn,
                limit=_int_param(request, "limit", 50, cfg.serve_max_rows),
                offset=_int_param(request, "offset", 0),
            )
        )

    async def run_detail(request: web.Request, conn: sqlite3.Connection) -> web.Response:
        run_id = _path_id(request, "id")
        run = reporting.get_run(conn, run_id)
        if run is None:
            return _error(f"no run {run_id}", 404)
        versions = reporting.rubric_versions(conn, run_id)
        rubric = request.query.get("rubric") or (versions[0] if versions else None)
        listing = reporting.list_results(
            conn,
            run_id,
            fixture=request.query.get("fixture"),
            outcome=request.query.get("outcome"),
            order=request.query.get("order", "natural"),
            rubric=rubric,
            limit=_int_param(request, "limit", cfg.serve_max_rows, cfg.serve_max_rows),
            offset=_int_param(request, "offset", 0),
        )
        return _json(
            {
                "run": run,
                "fixtures": reporting.fixture_summary(conn, run_id),
                "checks": reporting.check_summary(conn, run_id),
                "category_order": list(reporting.CATEGORY_ORDER),
                "rubric_versions": versions,
                "rubric": rubric,
                **listing,
            }
        )

    async def result_detail(request: web.Request, conn: sqlite3.Connection) -> web.Response:
        result_id = _path_id(request, "id")
        detail = reporting.get_result(conn, result_id)
        if detail is None:
            return _error(f"no result {result_id}", 404)
        return _json(detail)

    app.router.add_get("/healthz", healthz)
    app.router.add_get("/readyz", readyz)
    app.router.add_get("/meta", meta)
    app.router.add_get("/runs", _with_db(cfg, runs))
    app.router.add_get("/runs/{id}", _with_db(cfg, run_detail))
    app.router.add_get("/results/{id}", _with_db(cfg, result_detail))

    @web.middleware
    async def envelope(request: web.Request, handler):
        """Turn aiohttp's own HTML error pages into the JSON envelope the UI parses."""
        try:
            return await handler(request)
        except web.HTTPException as exc:
            if exc.status == 405:
                return _error("only GET is accepted here", 405)
            if exc.status == 404:
                return _error(f"no such path: {request.path}", 404)
            return _error(exc.text or exc.reason, exc.status)
        except sqlite3.Error as err:
            log.exception("query failed")
            return _error(f"query failed: {err}", 500)

    app.middlewares.append(envelope)
    return app


def serve(cfg: Config) -> int:
    """Run the read service until SIGTERM."""
    _instrument()
    app = build_app(cfg)
    host, _, port = cfg.serve_addr.rpartition(":")
    log.info("serving results from %s on %s", cfg.db_path, cfg.serve_addr)
    # run_app installs its own SIGTERM/SIGINT handling and drains in-flight requests
    # before returning, which is what lets telemetry.shutdown() in __main__ flush the
    # span batch instead of being SIGKILLed with it still queued.
    web.run_app(app, host=host or None, port=int(port), print=None, access_log=None)
    return 0


def _instrument() -> None:
    """Wrap the inbound HTTP boundary once, at construction.

    Auto-instrumentation rather than hand-rolled spans, per the repo's telemetry rules.
    Degrades to no-op if the package is absent so a missing optional dependency cannot
    stop the service from serving.
    """
    os.environ.setdefault("OTEL_PYTHON_AIOHTTP_SERVER_EXCLUDED_URLS", UNTRACED)
    try:
        from opentelemetry.instrumentation.aiohttp_server import AioHttpServerInstrumentor
    except ImportError:  # pragma: no cover - exercised only on a partial install
        log.warning("aiohttp server instrumentation unavailable; serving untraced")
        return
    AioHttpServerInstrumentor().instrument()
