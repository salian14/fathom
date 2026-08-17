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
from pydantic import BaseModel, Field

from . import __version__
from .agent import build_graph, build_messages, summarise
from .config import Config, load
from .llm import build_model
from .mcp_client import build_client, tool_session

log = logging.getLogger("fathom_agent")


class ChatRequest(BaseModel):
    question: str = Field(min_length=1)
    history: list[dict[str, str]] | None = None


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
            return JSONResponse(
                {"error": f"turn exceeded {cfg.request_timeout:g}s"}, status_code=504
            )
        except Exception as err:  # noqa: BLE001 - surfaced to the caller, not swallowed
            log.exception("chat failed")
            return JSONResponse({"error": f"{type(err).__name__}: {err}"}, status_code=502)

        answer, steps = summarise(result)
        return {
            "answer": answer,
            "steps": steps,
            "iterations": len(steps),
            "model": cfg.model,
            "elapsed_seconds": round(time.monotonic() - started, 2),
        }

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
    logging.basicConfig(
        level=cfg.log_level,
        format='{"time":"%(asctime)s","level":"%(levelname)s","logger":"%(name)s","msg":"%(message)s"}',
    )


def main() -> None:
    import uvicorn

    cfg = load()
    _configure_logging(cfg)
    uvicorn.run(create_app(cfg), host=cfg.host, port=cfg.port, log_level=cfg.log_level)
