"""OTLP wiring, matching the other three components.

Deliberately thinner than agent/telemetry.py: this is a batch job, not a server, so
there is no inbound instrumentation and no metric stream worth exporting on an interval
that a job outlives. What matters is that a run's turns appear in the same trace backend
as the agent's, so a slow or failed evaluation turn can be followed straight through
fathom-agent into fathom-mcp.

`console` writes to stderr and needs no collector, which is what makes `make run-local`
usable; `off` still installs the propagator so a disabled harness does not break the
trace for the services it calls.
"""

from __future__ import annotations

import logging

from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
from opentelemetry.instrumentation.logging import LoggingInstrumentor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import (
    BatchSpanProcessor,
    ConsoleSpanExporter,
    SimpleSpanProcessor,
)

from . import __version__

SERVICE_NAME = "fathom-eval"


def configure(mode: str, endpoint: str = "", environment: str = "lab") -> None:
    LoggingInstrumentor().instrument(set_logging_format=False)

    if mode == "off":
        return

    resource = Resource.create(
        {
            "service.name": SERVICE_NAME,
            "service.version": __version__,
            "deployment.environment.name": environment,
        }
    )
    provider = TracerProvider(resource=resource)

    if mode == "console":
        # stderr, not stdout: a run's own log output is the useful thing on stdout.
        import sys

        provider.add_span_processor(SimpleSpanProcessor(ConsoleSpanExporter(out=sys.stderr)))
    else:
        provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint or None)))

    trace.set_tracer_provider(provider)
    # One wrap at construction, never per call -- the same rule the other components
    # follow. This is what puts the eval's /chat call at the root of the agent's trace.
    HTTPXClientInstrumentor().instrument()
    logging.getLogger("fathom_eval").debug("telemetry configured: %s", mode)


def shutdown() -> None:
    """Flush spans before the process exits.

    A batch job exits promptly, which is exactly when a BatchSpanProcessor loses the
    tail of a run if nobody asks it to flush.
    """
    provider = trace.get_tracer_provider()
    if hasattr(provider, "shutdown"):
        provider.shutdown()


def tracer():
    return trace.get_tracer(SERVICE_NAME, __version__)
