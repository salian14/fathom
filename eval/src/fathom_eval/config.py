"""Configuration for the eval harness.

Mirrors agent/src/fathom_agent/config.py deliberately: one frozen dataclass, one
`load()` that reads `FATHOM_EVAL_*` and validates fail-fast, and error messages that
name the offending variable. A batch job that dies on its first line is far cheaper to
diagnose than one that writes half a run and then fails.

The CLI can override most of these per invocation -- see cli.py -- because the same
binary runs both as a nightly CronJob and by hand against a workstation.
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass, replace
from urllib.parse import urlparse

ENV_PREFIX = "FATHOM_EVAL_"

# Every fixture namespace starts with this. It is the harness's own guard rail: the
# ServiceAccount can technically create any namespace (RBAC cannot scope `create` by
# name), so the second limit lives here, in code, the same way fathom-mcp checks
# allowedNamespaces itself rather than trusting RBAC alone.
NAMESPACE_PREFIX = "fathom-eval-"


@dataclass(frozen=True)
class Config:
    agent_url: str
    db_path: str
    iterations: int
    fixtures: tuple[str, ...]
    max_iterations: int
    request_timeout: float
    setup_timeout: float
    teardown_timeout: float
    keep_namespaces: bool
    kubeconfig: str
    prometheus_url: str

    judge_url: str
    judge_model: str
    judge_sample: int
    judge_timeout: float

    log_level: int
    telemetry: str

    @property
    def chat_url(self) -> str:
        return self.agent_url.rstrip("/") + "/chat"


def load() -> Config:
    c = Config(
        agent_url=_env("AGENT_URL", "http://fathom-agent.fathom.svc.cluster.local:8080"),
        db_path=_env("DB_PATH", "/data/results.db"),
        iterations=_env_int("ITERATIONS", 10),
        fixtures=tuple(f for f in _env("FIXTURES", "").split(",") if f.strip()),
        # The agent's own FATHOM_AGENT_MAX_ITERATIONS, mirrored here only to size the
        # tool-call sanity bound. The authoritative ceiling verdict comes from the
        # LangGraph sentinel, not from this number, so a drift between the two is
        # harmless -- which is why it is not read from the agent at runtime.
        max_iterations=_env_int("AGENT_MAX_ITERATIONS", 8),
        # Above the agent's own 180s request timeout, so its 504 reaches us as a 504
        # rather than as a client-side disconnect that loses the outcome. Same reason
        # fathom-front's agent.timeout exceeds the agent's requestTimeout.
        request_timeout=_env_duration("REQUEST_TIMEOUT", 240.0),
        setup_timeout=_env_duration("SETUP_TIMEOUT", 300.0),
        teardown_timeout=_env_duration("TEARDOWN_TIMEOUT", 180.0),
        keep_namespaces=_env_bool("KEEP_NAMESPACES", False),
        kubeconfig=_env("KUBECONFIG_PATH", ""),
        prometheus_url=_env(
            "PROMETHEUS_URL", "http://prometheus-server.observability.svc.cluster.local:80"
        ),
        judge_url=_env("JUDGE_URL", "http://192.168.56.1:11435"),
        judge_model=_env("JUDGE_MODEL", "fathom-judge"),
        judge_sample=_env_int("JUDGE_SAMPLE", 3),
        # A 31B on CPU is slow by design; a judgment that takes four minutes is
        # working, not hung.
        judge_timeout=_env_duration("JUDGE_TIMEOUT", 900.0),
        log_level=_env_level("LOG_LEVEL", logging.INFO),
        telemetry=_env("TELEMETRY", "otlp"),
    )
    _validate(c)
    return c


def override(c: Config, **kwargs) -> Config:
    """Apply CLI overrides and re-validate.

    Re-validating matters: `--iterations 0` from the command line has to fail the same
    way FATHOM_EVAL_ITERATIONS=0 does, or the CLI becomes a way around the checks.
    """
    updated = replace(c, **{k: v for k, v in kwargs.items() if v is not None})
    _validate(updated)
    return updated


def _validate(c: Config) -> None:
    _require_url("AGENT_URL", c.agent_url)
    _require_url("JUDGE_URL", c.judge_url)
    if c.prometheus_url:
        _require_url("PROMETHEUS_URL", c.prometheus_url)

    if c.max_iterations < 1:
        raise ValueError(
            f"{ENV_PREFIX}AGENT_MAX_ITERATIONS must be at least 1, got {c.max_iterations}"
        )
    if c.iterations < 1:
        raise ValueError(f"{ENV_PREFIX}ITERATIONS must be at least 1, got {c.iterations}")
    if c.judge_sample < 0:
        raise ValueError(f"{ENV_PREFIX}JUDGE_SAMPLE must not be negative, got {c.judge_sample}")
    if not c.db_path.strip():
        raise ValueError(f"{ENV_PREFIX}DB_PATH must not be empty")
    if not c.judge_model.strip():
        raise ValueError(f"{ENV_PREFIX}JUDGE_MODEL must not be empty")

    for name, value in (
        ("REQUEST_TIMEOUT", c.request_timeout),
        ("SETUP_TIMEOUT", c.setup_timeout),
        ("TEARDOWN_TIMEOUT", c.teardown_timeout),
        ("JUDGE_TIMEOUT", c.judge_timeout),
    ):
        if value <= 0:
            raise ValueError(f"{ENV_PREFIX}{name} must be positive, got {value:g}s")

    if c.telemetry not in ("otlp", "console", "off"):
        raise ValueError(
            f"{ENV_PREFIX}TELEMETRY must be one of otlp, console, off -- got {c.telemetry!r}"
        )


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
        raise ValueError(f"{ENV_PREFIX}{name} must be an integer, got {raw!r}") from err


def _env_bool(name: str, default: bool) -> bool:
    raw = _env(name, "").strip().lower()
    if not raw:
        return default
    if raw in ("1", "true", "yes", "on"):
        return True
    if raw in ("0", "false", "no", "off"):
        return False
    raise ValueError(f"{ENV_PREFIX}{name} must be a boolean, got {raw!r}")


def _env_duration(name: str, default: float) -> float:
    """Bare seconds or a Go-style duration, matching the other components' knobs."""
    raw = _env(name, "").strip().lower()
    if not raw:
        return default
    multiplier = 1.0
    if raw.endswith("ms"):
        raw, multiplier = raw[:-2], 0.001
    elif raw.endswith("s"):
        raw, multiplier = raw[:-1], 1.0
    elif raw.endswith("m"):
        raw, multiplier = raw[:-1], 60.0
    elif raw.endswith("h"):
        raw, multiplier = raw[:-1], 3600.0
    try:
        return float(raw) * multiplier
    except ValueError as err:
        raise ValueError(
            f"{ENV_PREFIX}{name} must be a duration like 30s, 5m or a number of seconds"
        ) from err


def _env_level(name: str, default: int) -> int:
    raw = _env(name, "").strip().upper()
    if not raw:
        return default
    levels = logging.getLevelNamesMapping()
    if raw not in levels:
        raise ValueError(f"{ENV_PREFIX}{name} must be a logging level name, got {raw!r}")
    return levels[raw]
