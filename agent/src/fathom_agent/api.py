"""The HTTP surface: POST /chat, plus the two probes.

Probe contract matches mcp/cmd/fathom-mcp/main.go so both components behave the same
way under the same Deployment settings:

  /healthz  the process is up. Nothing else.
  /readyz   the process is up AND its dependencies answered.

For the MCP server "dependencies" meant the Kubernetes API. Here it means Ollama and
the MCP server -- the agent is useless without either, and a 503 that names which one
failed is the difference between a two-minute fix and an afternoon.
"""

from __future__ import annotations

import asyncio
import logging
import time
from contextlib import asynccontextmanager

import httpx
from fastapi import FastAPI
from fastapi.responses import JSONResponse, PlainTextResponse
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.instrumentation.utils import suppress_instrumentation
from pydantic import BaseModel, Field

from . import __version__, telemetry
from .agent import build_graph, build_messages, summarise
from .config import Config, load
from .llm import build_model
from .mcp_client import build_client, tool_session

log = logging.getLogger("fathom_agent")


class ChatRequest(BaseModel):
    question: str = Field(min_length=1)
    history: list[dict[str, str]] | None = None


# Built once at import. The global meter is a no-op until telemetry.configure
# installs a real one, and instruments resolve against it lazily.
_turn_duration = telemetry.meter().create_histogram(
    "fathom.chat.turn.duration",
    unit="s",
    description="Wall time of a POST /chat turn, by outcome.",
)


def _record_turn(span, outcome: str, started: float, err: Exception | None = None) -> None:
    """Close out a turn on both the span and the metric stream.

    `outcome` is one of ok/timeout/error -- a closed set, so it is safe as a
    metric dimension. Nothing from the question or the answer goes here.
    """
    span.set_attribute("fathom.chat.outcome", outcome)
    if err is not None:
        span.record_exception(err)
    _turn_duration.record(time.monotonic() - started, {"outcome": outcome})


@asynccontextmanager
async def lifespan(app: FastAPI):
    cfg: Config = app.state.cfg
    app.state.model = build_model(cfg)
    app.state.mcp = build_client(cfg)

    log.info(
        "starting",
        extra={
            "version": __version__,
            "addr": cfg.addr,
            "mcp_url": cfg.mcp_url,
            "ollama_url": cfg.ollama_url,
            "model": cfg.model,
        },
    )
    yield
    # uvicorn runs the lifespan shutdown on SIGTERM, which is the flush point:
    # without it a pod's last batch of spans dies with the process.
    telemetry.shutdown()


def create_app(cfg: Config | None = None) -> FastAPI:
    app = FastAPI(title="fathom-agent", version=__version__, lifespan=lifespan)
    app.state.cfg = cfg or load()
    # (checked_at, problem). Seeded here rather than in the lifespan so a test client
    # that skips startup still finds the attribute.
    app.state.ready = (0.0, None)

    @app.get("/healthz", response_class=PlainTextResponse)
    async def healthz() -> str:
        return "ok\n"

    @app.get("/readyz", response_class=PlainTextResponse)
    async def readyz():
        problem = await _readiness(app)
        if problem:
            log.warning("readiness check failed: %s", problem)
            return PlainTextResponse(problem + "\n", status_code=503)
        return "ok\n"

    @app.post("/chat")
    async def chat(request: ChatRequest):
        cfg: Config = app.state.cfg
        try:
            messages = build_messages(request.question, request.history)
        except ValueError as err:
            return JSONResponse({"error": str(err)}, status_code=400)

        started = time.monotonic()
        with telemetry.tracer().start_as_current_span("agent.turn") as span:
            span.set_attribute("gen_ai.request.model", cfg.model)
            span.set_attribute("gen_ai.system", "ollama")
            try:
                async with asyncio.timeout(cfg.request_timeout):
                    async with tool_session(cfg, app.state.mcp) as tools:
                        graph = build_graph(cfg, app.state.model, tools)
                        result = await graph.ainvoke(
                            {"messages": messages},
                            # LangGraph counts every node it runs, and one tool-calling
                            # round is two nodes (model, then tools), plus the final
                            # model turn that answers.
                            {"recursion_limit": 2 * cfg.max_iterations + 1},
                        )
            except TimeoutError:
                _record_turn(span, "timeout", started)
                return JSONResponse(
                    {"error": f"turn exceeded {cfg.request_timeout:g}s"}, status_code=504
                )
            except Exception as err:  # noqa: BLE001 - surfaced to the caller, not swallowed
                _record_turn(span, "error", started, err)
                log.exception("chat failed")
                return JSONResponse({"error": f"{type(err).__name__}: {err}"}, status_code=502)

            answer, steps = summarise(result)
            span.set_attribute("fathom.chat.iterations", len(steps))
            _record_turn(span, "ok", started)

        return {
            "answer": answer,
            "steps": steps,
            "iterations": len(steps),
            "model": cfg.model,
            "elapsed_seconds": round(time.monotonic() - started, 2),
        }

    # Unconditional, including under pytest: with no provider configured the
    # global tracer resolves to a no-op, so tests exercise the same path that
    # runs in production. The probes are excluded here rather than in the chart
    # because which routes are probes is a property of the app, and a Helm
    # mistake should not be able to flood the collector with them.
    FastAPIInstrumentor.instrument_app(app, excluded_urls="healthz,readyz")

    return app


async def _readiness(app: FastAPI) -> str | None:
    """Return None when ready, else a sentence naming what failed.

    Cached for FATHOM_AGENT_READY_CACHE so a 10s probe interval does not open an MCP
    session -- and wake the model server -- on every tick.
    """
    cfg: Config = app.state.cfg
    checked_at, cached = app.state.ready
    now = time.monotonic()
    if cfg.ready_cache and now - checked_at < cfg.ready_cache and checked_at:
        return cached

    problem = await _check_dependencies(cfg, app.state.mcp)
    app.state.ready = (now, problem)
    return problem


async def _check_dependencies(cfg: Config, mcp) -> str | None:
    # Suppressed rather than URL-excluded. Excluding the /readyz *server* span
    # would not stop these outbound calls, and without a parent each would
    # start its own root trace -- including a full MCP session open, which also
    # drags fathom-mcp into emitting initialize and tools/list spans. URL
    # exclusion cannot help either: the probe's POST /mcp is the same URL as a
    # real tool call's, so filtering it would delete the MCP client story too.
    #
    # Suppression is context-based, so it follows into the anyio task the MCP
    # client spawns to issue the request.
    with suppress_instrumentation():
        return await _probe_dependencies(cfg, mcp)


async def _probe_dependencies(cfg: Config, mcp) -> str | None:
    try:
        async with httpx.AsyncClient(timeout=cfg.mcp_timeout) as http:
            response = await http.get(f"{cfg.ollama_url.rstrip('/')}/api/version")
            response.raise_for_status()
    except Exception as err:  # noqa: BLE001
        return f"ollama unreachable at {cfg.ollama_url}: {err}"

    try:
        async with tool_session(cfg, mcp) as tools:
            if not tools:
                return f"mcp server at {cfg.mcp_url} advertised no tools"
    except Exception as err:  # noqa: BLE001
        return f"mcp server unreachable at {cfg.mcp_url}: {err}"

    return None


def _configure_logging(cfg: Config) -> None:
    handler = logging.StreamHandler()
    handler.setFormatter(telemetry.JsonFormatter())
    logging.basicConfig(level=cfg.log_level, handlers=[handler], force=True)


def main() -> None:
    import uvicorn

    cfg = load()
    # Before logging is configured: JsonFormatter reads the trace ids that
    # LoggingInstrumentor's record factory attaches, and the factory has to be
    # installed before the first record is made.
    telemetry.configure(cfg)
    _configure_logging(cfg)
    # Single process, no reload, no workers -- so the batch span processor's
    # background thread is safe. Adding workers= later would fork after this
    # point and telemetry setup would have to move into a post-fork hook.
    uvicorn.run(create_app(cfg), host=cfg.host, port=cfg.port, log_level=cfg.log_level)
