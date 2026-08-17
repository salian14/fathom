"""The MCP side: session handling and tool-result bounding.

Two decisions worth knowing about.

**Sessions are per-request, never cached.** The Go server holds MCP sessions with a
10 minute idle timeout (mcp/cmd/fathom-mcp/main.go). A long-lived session cached at
startup would work all through development and then start failing on an idle lab
cluster, in a way that looks like the MCP server being down. Opening a fresh session
per turn costs one initialize round-trip and removes that class of bug entirely.

**Results are truncated again on this side.** The server already caps a result at
FATHOM_MCP_MAX_BYTES (64 KiB by default), but the model's context is 16k tokens --
so a single maximal result is roughly the whole window, leaving nothing to reason
with. The cap here is about what the *model* can afford, which is a different budget
from what the server is willing to send.
"""

from __future__ import annotations

import contextlib
import functools
from collections.abc import AsyncIterator

from langchain_core.tools import BaseTool
from langchain_mcp_adapters.client import MultiServerMCPClient
from langchain_mcp_adapters.tools import load_mcp_tools

from . import telemetry
from .config import Config

TRUNCATION_NOTE = "[truncated: showing the last {kept} of {total} characters]\n"


def build_client(cfg: Config) -> MultiServerMCPClient:
    return MultiServerMCPClient(
        {
            "fathom": {
                "url": cfg.mcp_url,
                "transport": "streamable_http",
                "timeout": cfg.mcp_timeout,
            }
        }
    )


@contextlib.asynccontextmanager
async def tool_session(cfg: Config, client: MultiServerMCPClient) -> AsyncIterator[list[BaseTool]]:
    """Open a session and yield its tools, bounded. Closes the session on exit.

    The tool *schemas* come from the server's tools/list rather than being restated
    here -- that is what keeps this agent in lockstep with the Go tool definitions.
    Adding a tool there needs no change here.

    The span around the session is not decoration. The MCP client issues its
    request POSTs from a background anyio task, and anyio snapshots contextvars
    when that task is spawned -- which is when the session opens. So this is the
    span whose id ends up in the traceparent header that fathom-mcp reads, and
    naming it well is what keeps the server's spans attached to something
    meaningful rather than to whatever happened to be current.
    """
    with telemetry.tracer().start_as_current_span("mcp.session") as span:
        async with client.session("fathom") as session:
            tools = await load_mcp_tools(session)
            span.set_attribute("mcp.tool.count", len(tools))
            yield [_bounded(tool, cfg.max_tool_chars) for tool in tools]


def truncate(text: str, limit: int) -> str:
    """Keep the tail of `text`, prefixed with a note when anything was dropped.

    The tail, not the head: the useful end of a log is the most recent lines, and the
    useful end of a truncated table is no worse than the start. This mirrors
    truncate(s, max, keepTail) in mcp/internal/tools/tools.go.
    """
    if limit <= 0 or len(text) <= limit:
        return text

    note = TRUNCATION_NOTE.format(kept=limit, total=len(text))
    budget = max(limit - len(note), 0)
    tail = text[len(text) - budget :] if budget else ""

    # Drop the partial first line so the model never sees a half-record.
    newline = tail.find("\n")
    if 0 <= newline < len(tail) - 1:
        tail = tail[newline + 1 :]

    return note + tail


def _bounded(tool: BaseTool, limit: int) -> BaseTool:
    """Wrap `tool` so whatever it returns is truncated before it enters the history.

    The adapters hand back a StructuredTool whose `coroutine` returns
    ``(content, artifact)`` -- content_and_artifact response format. Only the content
    half reaches the model, so only that half is worth bounding; the artifact is left
    intact for anything that wants the unabridged result.

    Replacing `coroutine` rather than subclassing keeps the args schema that was just
    discovered from the server, which is the thing we most want not to rebuild.
    """
    inner = tool.coroutine
    if inner is None:  # pragma: no cover - the adapters always set it
        return tool

    @functools.wraps(inner)
    async def bounded(*args, **kwargs):
        # A sibling of the wire span, not its ancestor -- see tool_session on
        # why the HTTP call is parented to the session instead. Worth having
        # anyway: this is the only place that knows the tool's name alongside
        # how much of its result the model actually got to see.
        with telemetry.tracer().start_as_current_span(f"mcp.tool {tool.name}") as span:
            span.set_attribute("mcp.tool.name", tool.name)
            result = await inner(*args, **kwargs)

            if isinstance(result, tuple) and len(result) == 2:
                content, artifact = result
                bounded_content = _truncate_content(content, limit)
                _record_size(span, content, bounded_content)
                return bounded_content, artifact

            bounded_content = _truncate_content(result, limit)
            _record_size(span, result, bounded_content)
            return bounded_content

    tool.coroutine = bounded
    return tool


def _record_size(span, original, bounded) -> None:
    """Note how much of a tool result survived the agent-side cap.

    Truncation is invisible in the answer but often explains it: a model that
    keeps re-querying the same object is usually reacting to a result that was
    cut off before the part it needed.
    """
    before, after = len(str(original)), len(str(bounded))
    span.set_attribute("fathom.tool.result_chars", after)
    span.set_attribute("fathom.tool.truncated", after < before)


def _truncate_content(content, limit: int):
    """Bound a tool result, whether it arrived as a string or as content blocks."""
    if isinstance(content, str):
        return truncate(content, limit)

    if isinstance(content, list):
        # Multiple blocks share one budget, so a run of small blocks cannot add up
        # to more than a single large one would have been allowed.
        remaining = limit
        out = []
        for block in content:
            if isinstance(block, str):
                kept = truncate(block, remaining)
            elif isinstance(block, dict) and isinstance(block.get("text"), str):
                kept = {**block, "text": truncate(block["text"], remaining)}
            else:
                out.append(block)
                continue
            text = kept if isinstance(kept, str) else kept["text"]
            remaining -= len(text)
            out.append(kept)
            if remaining <= 0:
                break
        return out

    return content
