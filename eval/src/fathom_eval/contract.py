"""Build this component's published contract.

Reached as `fathom-eval contract`, and through `make contract`.

Three things are published here, because `fathom-eval` publishes three:

* the read service's HTTP surface, as OpenAPI;
* the results database schema, which is a contract in its own right -- the harness
  writes it and `fathom-eval-serve` reads it, and `reporting.check_schema` refuses a
  database newer than the reader rather than half-reading it;
* the fixture namespaces, because they are one half of a two-file agreement with
  `eval/helm` -> `rbac.fixtureNamespaces`.

The paths and methods come from the live aiohttp router rather than from a list kept
alongside it. The response shapes cannot: these handlers build dicts out of SQL rows,
so there is nothing to introspect. They live in `serve.RESPONSE_SCHEMAS`, next to the
handlers, and `test_every_route_is_described` is what keeps them honest.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

from . import __version__, store
from .config import ENV_PREFIX, describe, load
from .fixtures import load_all
from .serve import RESPONSE_SCHEMAS, build_app

# The version of the document this module emits, not of anything it describes.
SCHEMA_VERSION = 1


def _openapi(cfg) -> dict[str, Any]:
    """Render the read service's routes as OpenAPI 3.1."""
    app = build_app(cfg)

    paths: dict[str, Any] = {}
    for route in app.router.routes():
        path = route.resource.canonical
        described = RESPONSE_SCHEMAS.get(path)
        if described is None:
            # Unreachable while the test below passes; loud rather than silent if
            # it ever is reached, because a route published with no shape is the
            # failure this whole file exists to prevent.
            raise KeyError(f"route {path} has no entry in serve.RESPONSE_SCHEMAS")

        media = described.get("media_type", "application/json")
        responses: dict[str, Any] = {}
        for status, spec in described["responses"].items():
            body: dict[str, Any] = {"description": spec["description"]}
            schema = spec.get("schema", {"type": "string"} if media == "text/plain" else None)
            if schema is not None:
                body["content"] = {media: {"schema": schema}}
            responses[status] = body

        # Every non-GET is a 405 and every unknown path a 404, both in the JSON
        # envelope. Stated per-route because a caller handles them per-call.
        responses.setdefault(
            "405",
            {
                "description": "Only GET is accepted here.",
                "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}},
            },
        )

        operation: dict[str, Any] = {"summary": described["summary"], "responses": responses}
        if "description" in described:
            operation["description"] = described["description"]

        parameters = []
        for name in _path_params(path):
            parameters.append(
                {
                    "name": name,
                    "in": "path",
                    "required": True,
                    "schema": {"type": "integer"},
                }
            )
        for name, spec in sorted(described.get("query", {}).items()):
            schema = {k: v for k, v in spec.items() if k != "description"}
            parameters.append(
                {
                    "name": name,
                    "in": "query",
                    "required": False,
                    "description": spec.get("description", ""),
                    "schema": schema,
                }
            )
        if parameters:
            operation["parameters"] = parameters

        paths.setdefault(path, {})[route.method.lower()] = operation

    return {
        "openapi": "3.1.0",
        "info": {
            "title": "fathom-eval-serve",
            "version": __version__,
            "description": (
                "A read-only HTTP view of the eval results database. It exists because "
                "SQLite is a library rather than a server, and fathom-front's eval view "
                "is in another namespace and cannot mount the volume. Unauthenticated "
                "and ClusterIP: never expose it."
            ),
        },
        "paths": paths,
        "components": {
            "schemas": {
                "Error": {
                    "type": "object",
                    "required": ["error"],
                    "description": (
                        "The same envelope fathom-agent and fathom-front use, so a "
                        "caller has one shape to parse rather than one per origin."
                    ),
                    "properties": {"error": {"type": "string"}},
                }
            }
        },
    }


def _path_params(path: str) -> list[str]:
    """Names inside {} in a canonical aiohttp path."""
    out, depth, current = [], 0, ""
    for ch in path:
        if ch == "{":
            depth, current = depth + 1, ""
        elif ch == "}" and depth:
            depth -= 1
            out.append(current)
        elif depth:
            current += ch
    return out


def build() -> dict[str, Any]:
    """Assemble the contract document."""
    # A dump must not depend on the environment of whoever ran it, or two machines
    # produce two different contracts for one commit.
    saved = {k: v for k, v in os.environ.items() if k.startswith(ENV_PREFIX)}
    for key in saved:
        del os.environ[key]
    try:
        cfg = load()
        spec = _openapi(cfg)
    finally:
        os.environ.update(saved)

    fixtures = load_all()

    return {
        "schema_version": SCHEMA_VERSION,
        "component": "eval",
        "service": "fathom-eval-serve",
        "version": __version__,
        "openapi": spec,
        "probes": {"liveness": "/healthz", "readiness": "/readyz"},
        # Recorded by the config helpers during the load() above, so it is what
        # load() reads rather than a table kept beside it.
        "config": describe(),
        "telemetry": {
            "traces": True,
            # Deliberate, and not an omission to be fixed: this component builds a
            # TracerProvider and no MeterProvider. A batch job has no steady state
            # for an export interval to describe, and the numbers it produces belong
            # in results.db, which is published here as storage rather than as a
            # metric stream.
            "metrics": False,
            "instruments": [],
            "exporter_var": "FATHOM_EVAL_TELEMETRY",
        },
        "defaults": {
            "serve_addr": cfg.serve_addr,
            "serve_max_rows": cfg.serve_max_rows,
            "db_path": cfg.db_path,
            "request_timeout": cfg.request_timeout,
        },
        "storage": {
            "engine": "sqlite",
            "schema_version": store.SCHEMA_VERSION,
            "note": (
                "There is no migration tool. The version is stored in the meta table "
                "and served at /meta; reporting.connect_ro opens mode=ro with "
                "PRAGMA query_only and refuses to create the file, rather than "
                "conjuring an empty database that would shadow the real one."
            ),
            "ddl": store.SCHEMA.strip(),
        },
        # One half of a two-file agreement: these namespaces must appear in
        # eval/helm -> rbac.fixtureNamespaces or the harness cannot stage them.
        "fixture_namespaces": sorted({f.namespace for f in fixtures}),
        "depends_on": [
            {
                "name": "fathom-agent",
                "protocol": "http",
                "contract": "contracts/agent/",
                "required": True,
                "note": "Dialled as an ordinary client, one fresh POST /chat per question.",
            },
            {
                "name": "ollama-judge",
                "protocol": "http",
                "contract": None,
                "required": False,
                "note": "CPU-only second Ollama on the host, :11435. Only the judge "
                "CronJob needs it; the read service does not.",
            },
        ],
    }


def main() -> int:
    # sort_keys and a trailing newline so a regeneration that changed nothing
    # produces a byte-identical file.
    json.dump(build(), sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
