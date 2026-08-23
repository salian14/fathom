"""Print this service's published interface as JSON on stdout.

    python -m fathom_agent.contract

A separate module rather than an argument to ``__main__``: the entrypoint's job is
to load config, wire telemetry and serve, in that order and with no logic of its own,
and a second mode belongs beside it rather than inside it.

The spec is FastAPI's own ``app.openapi()``. Nothing here restates a route or a body
shape, which is the point -- a contract that can disagree with the code it describes
is worse than no contract, because it is believed. That also means the two gaps this
closed live in ``api.py`` where they belong: ``/chat`` carries a ``response_model``,
and ``REMEDIATION_SCHEMA`` is pointed at rather than copied.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

from . import __version__
from .api import create_app
from .config import ENV_PREFIX, describe, load

# The version of the document this module emits, not of anything it describes.
# Bump it when the shape below changes, so a reader that caches contracts can
# tell a reshape from a real interface change.
SCHEMA_VERSION = 1


def build() -> dict[str, Any]:
    """Assemble the contract document."""
    # A dump must not depend on the environment of whoever ran it, or two machines
    # produce two different contracts for one commit. Clearing the prefix and then
    # calling the real loader gives the shipped defaults without duplicating them.
    saved = {k: v for k, v in os.environ.items() if k.startswith(ENV_PREFIX)}
    for key in saved:
        del os.environ[key]
    try:
        cfg = load()
        spec = create_app(cfg).openapi()
    finally:
        os.environ.update(saved)

    return {
        "schema_version": SCHEMA_VERSION,
        "component": "agent",
        "service": "fathom-agent",
        "version": __version__,
        "openapi": spec,
        "probes": {"liveness": "/healthz", "readiness": "/readyz"},
        # Recorded by the config helpers during the load() above, so it is what
        # load() reads rather than a table kept beside it.
        "config": describe(),
        "telemetry": {
            "traces": True,
            "metrics": True,
            "exporter_var": "FATHOM_AGENT_TELEMETRY",
            "instruments": [
                {
                    "name": "fathom.chat.turn.duration",
                    "kind": "histogram",
                    "unit": "s",
                    "description": "Wall time of a POST /chat turn, by outcome.",
                    # A closed set. Question text, session ids and tool arguments
                    # are span attributes and must stay that way.
                    "attributes": ["outcome"],
                }
            ],
        },
        "defaults": {
            "addr": cfg.addr,
            "model": cfg.model,
            "request_timeout": cfg.request_timeout,
            "remediation_timeout": cfg.remediation_timeout,
            "max_iterations": cfg.max_iterations,
        },
        "depends_on": [
            {
                "name": "fathom-mcp",
                "protocol": "mcp streamable-http",
                "contract": "contracts/mcp/",
                "required": True,
                "note": "Every cluster read goes through it. The agent holds no "
                "Kubernetes credentials and no RBAC of its own.",
            },
            {
                "name": "ollama",
                "protocol": "http",
                "contract": None,
                "required": True,
                "note": "On the host, outside the cluster. Reached at the host's "
                "address on the libvirt network, never localhost and never a Service.",
            },
        ],
    }


def main() -> int:
    # sort_keys and a trailing newline so a regeneration that changed nothing
    # produces a byte-identical file. A generator that is not stable makes every
    # commit a contract commit, which trains people to stop reading the diff.
    json.dump(build(), sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
