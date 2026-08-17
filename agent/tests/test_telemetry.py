"""Telemetry: probe exclusion and the JSON log formatter.

The formatter tests are regressions. The format string these replace dropped
every `extra=` field and emitted invalid JSON whenever a message contained a
quote -- which the readiness path did routinely, since ConnectError's repr is
full of them.
"""

from __future__ import annotations

import json
import logging

import pytest
from fastapi.testclient import TestClient
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from fathom_agent.api import create_app
from fathom_agent.config import load
from fathom_agent.telemetry import JsonFormatter


@pytest.fixture
def exporter():
    """A tracer provider recording into memory, installed for one test."""
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))

    # The global provider can only be set once per process, so swap the
    # module-level reference the SDK reads rather than going through the API.
    saved = trace._TRACER_PROVIDER
    trace._TRACER_PROVIDER = provider
    yield exporter
    trace._TRACER_PROVIDER = saved


def record(msg: str, **extra) -> logging.LogRecord:
    rec = logging.LogRecord("fathom_agent", logging.INFO, __file__, 1, msg, None, None)
    for key, value in extra.items():
        setattr(rec, key, value)
    return rec


# Probes run every 10-20s for the life of the pod. Tracing them would drown
# real work without saying anything a span could add.
def test_probes_produce_no_spans(exporter, monkeypatch):
    monkeypatch.setenv("FATHOM_AGENT_TELEMETRY", "off")
    client = TestClient(create_app(load()))

    client.get("/healthz")

    assert exporter.get_finished_spans() == ()


def test_chat_is_traced(exporter, monkeypatch):
    monkeypatch.setenv("FATHOM_AGENT_TELEMETRY", "off")
    client = TestClient(create_app(load()))

    # No MCP server or model here, so the turn fails -- but it must still be
    # traced, since a failing turn is exactly the one worth looking at.
    client.post("/chat", json={"question": "what pods are running?"})

    names = {span.name for span in exporter.get_finished_spans()}
    assert "agent.turn" in names, f"no turn span recorded; got {names}"


def test_formatter_keeps_extra_fields():
    payload = json.loads(JsonFormatter().format(record("starting", model="fathom-agent")))

    assert payload["msg"] == "starting"
    assert payload["model"] == "fathom-agent", "extra= fields were dropped"


def test_formatter_escapes_quotes_and_newlines():
    # The shape the readiness path actually produces.
    msg = 'mcp server unreachable: ConnectError("[Errno 111] refused")\nretrying'

    payload = json.loads(JsonFormatter().format(record(msg)))

    assert payload["msg"] == msg


def test_formatter_omits_ids_without_a_span():
    payload = json.loads(JsonFormatter().format(record("no span here")))

    assert "trace_id" not in payload
    assert "span_id" not in payload


def test_formatter_includes_ids_inside_a_span():
    rec = record("in a span", otelTraceID="4bf92f3577b34da6a3ce929d0e0e4736")

    payload = json.loads(JsonFormatter().format(rec))

    assert payload["trace_id"] == "4bf92f3577b34da6a3ce929d0e0e4736"


def test_formatter_renders_exceptions():
    try:
        raise ValueError('boom "quoted"')
    except ValueError:
        import sys

        rec = record("chat failed")
        rec.exc_info = sys.exc_info()

    payload = json.loads(JsonFormatter().format(rec))

    assert "ValueError" in payload["exception"]
