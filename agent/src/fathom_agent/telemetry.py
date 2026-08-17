"""Telemetry: OTLP traces and metrics, and the structured-log formatter.

Log formatting lives here rather than in api.py because correlation is the
whole point of it -- every record carries the ids of the span it happened
inside, so a line found in `kubectl logs` can be pivoted to its trace.

Logs themselves stay on stdout. There is no OTLP log signal: the collector
picks them up from the container runtime, and shipping them twice would only
double the storage.

Two rules govern this module:

* Telemetry never takes the agent down. The collector lives in another
  namespace and may be absent or unreachable; every failure path degrades to
  no-op providers.
* Configuration comes from the standard OTEL_* variables, read by the SDK
  itself. The single fathom-prefixed knob is FATHOM_AGENT_TELEMETRY, because
  it decides *which exporter is built* rather than how one behaves.
"""

from __future__ import annotations

import atexit
import json
import logging
import sys
from collections.abc import Callable

from openinference.instrumentation.langchain import LangChainInstrumentor
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
from opentelemetry.instrumentation.logging import LoggingInstrumentor
from opentelemetry.instrumentation.system_metrics import SystemMetricsInstrumentor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import (
    ConsoleMetricExporter,
    PeriodicExportingMetricReader,
)
from opentelemetry.sdk.resources import SERVICE_NAME, SERVICE_VERSION, Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, ConsoleSpanExporter

from . import __version__
from .config import Config

SCOPE = "fathom_agent"

# Deliberately narrow. The default set is ~25 metric families including one
# series per disk and per network interface, which on a 3-VM lab cluster is
# mostly noise. Memory is the agent's real operational risk -- the pod limit
# exists because the LangChain import tree is large -- so measure that.
_SYSTEM_METRICS = {
    "process.memory.usage": None,
    "process.memory.virtual": None,
    "process.cpu.utilization": None,
    "process.thread.count": None,
}

_shutdown: Callable[[], None] | None = None


def configure(cfg: Config) -> None:
    """Install providers and instrumentors. Idempotent; never raises."""
    global _shutdown

    if _shutdown is not None:
        return

    # Always instrument logging, even with telemetry off: the record factory is
    # what puts otelTraceID on every record, and JsonFormatter reads it. With no
    # active span those ids are zeros, which the formatter omits.
    LoggingInstrumentor().instrument(set_logging_format=False)

    if cfg.telemetry == "off":
        return

    resource = Resource.create({SERVICE_NAME: "fathom-agent", SERVICE_VERSION: __version__})

    if cfg.telemetry == "console":
        # stderr, not stdout: keeps span dumps out of anything parsing the
        # agent's own log stream, and mirrors what the Go side does.
        span_exporter = ConsoleSpanExporter(out=sys.stderr)
        metric_exporter = ConsoleMetricExporter()
    else:
        # Endpoint and protocol come from OTEL_EXPORTER_OTLP_*; nothing about
        # the collector's address is hardcoded here. Connection is lazy, so a
        # collector that is down cannot block startup.
        span_exporter = OTLPSpanExporter()
        metric_exporter = OTLPMetricExporter()

    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(BatchSpanProcessor(span_exporter))
    trace.set_tracer_provider(tracer_provider)

    meter_provider = MeterProvider(
        resource=resource,
        metric_readers=[PeriodicExportingMetricReader(metric_exporter)],
    )
    metrics.set_meter_provider(meter_provider)

    # Patches httpx's transport classes, so it covers every client built
    # afterwards -- including the ones the ollama SDK and the MCP streamable
    # HTTP client construct internally. This is what injects traceparent into
    # the requests that reach fathom-mcp.
    HTTPXClientInstrumentor().instrument()
    SystemMetricsInstrumentor(config=_SYSTEM_METRICS).instrument()
    LangChainInstrumentor().instrument(tracer_provider=tracer_provider)

    def _stop() -> None:
        tracer_provider.shutdown()
        meter_provider.shutdown()

    _shutdown = _stop
    # Backstop for exits that skip the lifespan shutdown. _stop is idempotent
    # through the guard in shutdown(), so running twice is harmless.
    atexit.register(shutdown)


def shutdown() -> None:
    """Flush spans and metrics. Safe to call more than once."""
    global _shutdown
    if _shutdown is not None:
        stop, _shutdown = _shutdown, None
        stop()


def tracer() -> trace.Tracer:
    return trace.get_tracer(SCOPE)


def meter() -> metrics.Meter:
    return metrics.get_meter(SCOPE)


# Attributes every LogRecord carries. Anything outside this set arrived through
# `extra=` and belongs in the output.
_STANDARD = frozenset(logging.LogRecord("", 0, "", 0, "", None, None).__dict__) | {
    "message",
    "asctime",
    "taskName",
    "otelTraceID",
    "otelSpanID",
    "otelServiceName",
    "otelTraceSampled",
}


class JsonFormatter(logging.Formatter):
    """One JSON object per line, escaped by json.dumps rather than by hope.

    This replaces a format string that interpolated %(message)s straight into a
    JSON literal. That had two consequences worth remembering: fields passed as
    `extra=` were dropped, because the format string never named them; and any
    message containing a quote or a newline produced invalid JSON -- which the
    readiness path did routinely, since ConnectError's repr contains quotes.
    """

    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, object] = {
            "time": self.formatTime(record, "%Y-%m-%dT%H:%M:%S%z"),
            "level": record.levelname.lower(),
            "logger": record.name,
            "msg": record.getMessage(),
        }

        # Zeros mean "no active span"; omitting them keeps startup and probe
        # lines from carrying meaningless ids.
        trace_id = getattr(record, "otelTraceID", "0")
        if trace_id and trace_id != "0":
            payload["trace_id"] = trace_id
            payload["span_id"] = getattr(record, "otelSpanID", "")

        payload.update({k: v for k, v in record.__dict__.items() if k not in _STANDARD})

        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)

        return json.dumps(payload, default=str)
