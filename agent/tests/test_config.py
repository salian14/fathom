"""Smoke tests for config loading and its error messages.

The point of these is the failure path: a bad value must produce an error that names
the variable, because that string is all an operator gets from `kubectl logs`.
"""

from __future__ import annotations

import logging

import pytest

from fathom_agent import config


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for key in list(os_environ_keys()):
        monkeypatch.delenv(key, raising=False)


def os_environ_keys():
    import os

    return [k for k in os.environ if k.startswith(config.ENV_PREFIX)]


def test_defaults_are_valid():
    cfg = config.load()

    assert cfg.model == "fathom-agent", "must request the alias, not the base model tag"
    assert cfg.mcp_url.endswith("/mcp")
    assert cfg.host == "0.0.0.0"
    assert cfg.port == 8080
    assert cfg.log_level == logging.INFO


def test_env_overrides(monkeypatch):
    monkeypatch.setenv("FATHOM_AGENT_ADDR", "127.0.0.1:9090")
    monkeypatch.setenv("FATHOM_AGENT_OLLAMA_URL", "http://192.168.56.1:11434")
    monkeypatch.setenv("FATHOM_AGENT_LOG_LEVEL", "debug")
    monkeypatch.setenv("FATHOM_AGENT_MAX_TOOL_CHARS", "1234")

    cfg = config.load()

    assert (cfg.host, cfg.port) == ("127.0.0.1", 9090)
    assert cfg.ollama_url == "http://192.168.56.1:11434"
    assert cfg.log_level == logging.DEBUG
    assert cfg.max_tool_chars == 1234


@pytest.mark.parametrize(
    ("raw", "expected"),
    [("30s", 30.0), ("2m", 120.0), ("1h", 3600.0), ("45", 45.0), ("0.5m", 30.0)],
)
def test_duration_forms(monkeypatch, raw, expected):
    # Raised alongside so the long forms do not trip the "a tool call must not
    # outlive its turn" rule, which is what test_tool_call_cannot_outlive_its_turn
    # covers.
    monkeypatch.setenv("FATHOM_AGENT_REQUEST_TIMEOUT", "24h")
    monkeypatch.setenv("FATHOM_AGENT_MCP_TIMEOUT", raw)
    assert config.load().mcp_timeout == expected


@pytest.mark.parametrize(
    ("var", "value", "fragment"),
    [
        ("ADDR", "8080", "ADDR"),
        ("OLLAMA_URL", "192.168.56.1:11434", "absolute http(s) URL"),
        ("MCP_URL", "", "absolute http(s) URL"),
        ("MODEL", "  ", "MODEL must not be empty"),
        ("MAX_ITERATIONS", "0", "must be > 0"),
        ("MAX_TOOL_CHARS", "-1", "must be > 0"),
        ("MCP_TIMEOUT", "nope", "MCP_TIMEOUT"),
        ("LOG_LEVEL", "chatty", "LOG_LEVEL"),
    ],
)
def test_bad_values_name_the_variable(monkeypatch, var, value, fragment):
    monkeypatch.setenv(f"FATHOM_AGENT_{var}", value)

    with pytest.raises(ValueError) as err:
        config.load()

    assert fragment in str(err.value)


def test_tool_call_cannot_outlive_its_turn(monkeypatch):
    monkeypatch.setenv("FATHOM_AGENT_REQUEST_TIMEOUT", "10s")
    monkeypatch.setenv("FATHOM_AGENT_MCP_TIMEOUT", "30s")

    with pytest.raises(ValueError, match="exceeds"):
        config.load()
