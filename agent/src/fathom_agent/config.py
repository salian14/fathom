"""Configuration, loaded from the environment and validated once at startup.

Mirrors mcp/internal/config/config.go: one prefix, fail-fast validation, and error
messages that name the offending variable so a bad deploy is diagnosable from the
first line of the pod's logs.

Nothing here has a default that only works on one machine. The two endpoints are
the whole point of the topology -- the MCP server is in-cluster, Ollama is on the
host on the other side of a network boundary -- so both arrive as configuration.
"""

from __future__ import annotations

import logging
import os
import re
from dataclasses import dataclass
from urllib.parse import urlparse

ENV_PREFIX = "FATHOM_AGENT_"

DEFAULT_SYSTEM_PROMPT = """\
You are Fathom, an assistant that answers questions about one Kubernetes cluster.

You have read-only tools. Nothing you can call will change the cluster, so you never
need to ask permission before looking -- but you also cannot fix anything, and you
should not claim otherwise. If the answer is an action, describe the action; do not
pretend to have taken it.

How to use the tools well:

- k8s_get without a name returns a table. That is the cheap way to survey before you
  drill in. Start there when you do not already know the object's name.
- k8s_get with a name returns the full object as YAML. Use it when you need spec
  details, not to browse.
- k8s_describe carries the object's recent events. Events are what explain Pending,
  CrashLoopBackOff, ImagePullBackOff and failed mounts -- the YAML alone does not.
  Reach for describe whenever the question is "why is this unhealthy".
- k8s_log reads the tail of a container's log. After a restart or in a crash loop,
  the current log is often empty or has started over: pass previous=true to read the
  instance that actually died.

Namespaces: if the user does not name one, the tools use the default namespace. When
a question is about "the cluster" rather than a specific app, list across namespaces
rather than guessing one.

Tool output is truncated when it is large. If a result looks cut off, narrow the
query -- a label selector, a specific name, fewer log lines -- rather than asking for
the same thing again.

Answer concisely, and name the objects your conclusion rests on so the user can check
you. If the tools do not show enough to answer, say what is missing instead of
guessing.\
"""


@dataclass(frozen=True)
class Config:
    """Every knob the agent has. Built by :func:`load`, immutable afterwards."""

    # Listen address for the HTTP API, in ":8080" or "0.0.0.0:8080" form.
    addr: str

    # The in-cluster MCP endpoint, including its path (default /mcp).
    mcp_url: str

    # Ollama's base URL. In-cluster this points at the ExternalName Service that
    # aliases the host; outside it points at the host directly.
    ollama_url: str

    # The model *alias* (see ollama/config.env), never the base tag. Serving
    # parameters -- num_ctx, temperature, top_p -- are pinned in the alias's
    # Modelfile on purpose, so the agent must not send them per request.
    model: str

    # Ceiling on tool-calling rounds in a single turn. Guards against a model that
    # keeps calling tools without converging on an answer.
    max_iterations: int

    # Per-tool-result cap, applied on top of the MCP server's own byte cap. The
    # server allows 64 KiB, which is roughly the model's entire 16k context -- one
    # large result would leave no room to reason about it.
    max_tool_chars: int

    # Whole-turn budget for POST /chat.
    request_timeout: float

    # Bound on a single MCP call.
    mcp_timeout: float

    # How long a /readyz result is reused, so a 10s probe interval does not open an
    # MCP session every tick.
    ready_cache: float

    log_level: int
    system_prompt: str

    @property
    def host(self) -> str:
        host, _, _ = self.addr.rpartition(":")
        return host or "0.0.0.0"

    @property
    def port(self) -> int:
        _, _, port = self.addr.rpartition(":")
        return int(port)


def load() -> Config:
    """Read the environment and validate it. Raises ValueError on bad input."""
    cfg = Config(
        addr=_env("ADDR", ":8080"),
        mcp_url=_env("MCP_URL", "http://fathom-mcp.fathom.svc.cluster.local:8080/mcp"),
        ollama_url=_env("OLLAMA_URL", "http://fathom-ollama.fathom.svc.cluster.local:11434"),
        model=_env("MODEL", "fathom-agent"),
        max_iterations=_env_int("MAX_ITERATIONS", 8),
        max_tool_chars=_env_int("MAX_TOOL_CHARS", 8000),
        request_timeout=_env_duration("REQUEST_TIMEOUT", 180.0),
        mcp_timeout=_env_duration("MCP_TIMEOUT", 30.0),
        ready_cache=_env_duration("READY_CACHE", 30.0),
        log_level=_env_level("LOG_LEVEL", logging.INFO),
        system_prompt=_env("SYSTEM_PROMPT", DEFAULT_SYSTEM_PROMPT),
    )
    _validate(cfg)
    return cfg


def _validate(c: Config) -> None:
    _require_addr(c.addr)
    _require_url("MCP_URL", c.mcp_url)
    _require_url("OLLAMA_URL", c.ollama_url)

    if not c.model.strip():
        raise ValueError(f"{ENV_PREFIX}MODEL must not be empty")

    for name, value in (
        ("MAX_ITERATIONS", c.max_iterations),
        ("MAX_TOOL_CHARS", c.max_tool_chars),
        ("REQUEST_TIMEOUT", c.request_timeout),
        ("MCP_TIMEOUT", c.mcp_timeout),
    ):
        if value <= 0:
            raise ValueError(f"{ENV_PREFIX}{name} must be > 0, got {value}")

    # Zero is meaningful here: check the dependencies on every probe.
    if c.ready_cache < 0:
        raise ValueError(f"{ENV_PREFIX}READY_CACHE must be >= 0, got {c.ready_cache}")

    if c.mcp_timeout > c.request_timeout:
        raise ValueError(
            f"{ENV_PREFIX}MCP_TIMEOUT ({c.mcp_timeout}s) exceeds "
            f"{ENV_PREFIX}REQUEST_TIMEOUT ({c.request_timeout}s): a single tool call "
            f"could outlive the turn that requested it"
        )

    if not c.system_prompt.strip():
        raise ValueError(f"{ENV_PREFIX}SYSTEM_PROMPT must not be empty")


_ADDR_RE = re.compile(r"^[^:]*:\d+$")


def _require_addr(addr: str) -> None:
    if not _ADDR_RE.match(addr):
        raise ValueError(f'{ENV_PREFIX}ADDR must be "host:port" or ":port", got {addr!r}')
    if not 1 <= int(addr.rpartition(":")[2]) <= 65535:
        raise ValueError(f"{ENV_PREFIX}ADDR: port out of range in {addr!r}")


def _require_url(name: str, raw: str) -> None:
    parsed = urlparse(raw)
    if parsed.scheme not in ("http", "https") or not parsed.netloc:
        raise ValueError(f"{ENV_PREFIX}{name} must be an absolute http(s) URL, got {raw!r}")


def _env(name: str, default: str) -> str:
    return os.environ.get(ENV_PREFIX + name, default)


def _env_int(name: str, default: int) -> int:
    raw = _env(name, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError as err:
        raise ValueError(f"{ENV_PREFIX}{name}: {err}") from err


def _env_duration(name: str, default: float) -> float:
    """Accept a bare number of seconds or a Go-style "30s" / "2m" / "1h"."""
    raw = _env(name, "").strip().lower()
    if not raw:
        return default

    units = {"s": 1.0, "m": 60.0, "h": 3600.0}
    multiplier = units.get(raw[-1:], 1.0)
    if raw[-1:] in units:
        raw = raw[:-1]

    try:
        return float(raw) * multiplier
    except ValueError as err:
        raise ValueError(
            f'{ENV_PREFIX}{name}: want seconds or a duration like "30s", got {raw!r}'
        ) from err


def _env_level(name: str, default: int) -> int:
    raw = _env(name, "").strip().upper()
    if not raw:
        return default
    level = logging.getLevelNamesMapping().get(raw)
    if level is None:
        raise ValueError(
            f"{ENV_PREFIX}{name}: want debug, info, warn(ing), error or critical, "
            f"got {raw.lower()!r}"
        )
    return level
