"""Config validation. Every error must name the variable that caused it."""

from __future__ import annotations

import pytest

from fathom_eval import config


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for key in list(__import__("os").environ):
        if key.startswith(config.ENV_PREFIX):
            monkeypatch.delenv(key, raising=False)


def test_defaults_load():
    c = config.load()
    assert c.iterations == 10
    assert c.chat_url.endswith("/chat")
    assert c.judge_url.endswith(":11435")


@pytest.mark.parametrize(
    ("var", "value", "message"),
    [
        ("ITERATIONS", "0", "ITERATIONS"),
        ("ITERATIONS", "nope", "ITERATIONS"),
        ("AGENT_URL", "not-a-url", "AGENT_URL"),
        ("JUDGE_URL", "ftp://x", "JUDGE_URL"),
        ("REQUEST_TIMEOUT", "0", "REQUEST_TIMEOUT"),
        ("TELEMETRY", "loud", "TELEMETRY"),
        ("JUDGE_MODEL", "  ", "JUDGE_MODEL"),
        ("AGENT_MAX_ITERATIONS", "0", "AGENT_MAX_ITERATIONS"),
    ],
)
def test_invalid_values_fail_and_name_the_variable(monkeypatch, var, value, message):
    monkeypatch.setenv(config.ENV_PREFIX + var, value)
    with pytest.raises(ValueError, match=message):
        config.load()


def test_durations_accept_go_style_suffixes(monkeypatch):
    monkeypatch.setenv(config.ENV_PREFIX + "REQUEST_TIMEOUT", "4m")
    assert config.load().request_timeout == 240.0


def test_cli_overrides_are_revalidated():
    # --iterations 0 must fail exactly the way the environment variable does, or the
    # flags become a way around the checks.
    with pytest.raises(ValueError, match="ITERATIONS"):
        config.override(config.load(), iterations=0)


def test_override_ignores_none_so_unset_flags_do_not_clobber_env(monkeypatch):
    monkeypatch.setenv(config.ENV_PREFIX + "ITERATIONS", "4")
    assert config.override(config.load(), iterations=None).iterations == 4


def test_request_timeout_exceeds_the_agents_own_budget():
    # The agent enforces 180s itself and answers 504. A shorter client timeout would
    # record a transport error and lose the agent's own verdict.
    assert config.load().request_timeout > 180.0
