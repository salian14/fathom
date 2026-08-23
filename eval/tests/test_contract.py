"""The published contract, and the guard that makes it safe to publish.

`fathom-eval-serve` has no response models to introspect -- its handlers build dicts
straight out of SQL rows -- so the body shapes are declared in `serve.RESPONSE_SCHEMAS`
by hand. A hand-written schema with no guard is the second source of truth this repo
keeps avoiding, and these tests are the guard.
"""

from __future__ import annotations

import json

from fathom_eval import config, contract, store
from fathom_eval.serve import RESPONSE_SCHEMAS, build_app


def _app():
    return build_app(config.load())


def test_every_route_is_described():
    """A route added without an entry in RESPONSE_SCHEMAS would be published with no
    shape, or silently dropped from the spec. Either way the contract lies."""
    registered = {route.resource.canonical for route in _app().router.routes()}
    missing = registered - set(RESPONSE_SCHEMAS)
    assert not missing, f"routes with no declared response shape: {sorted(missing)}"


def test_nothing_is_described_that_is_not_served():
    """The other direction: a stale entry advertises an endpoint that 404s."""
    registered = {route.resource.canonical for route in _app().router.routes()}
    stale = set(RESPONSE_SCHEMAS) - registered
    assert not stale, f"described but not registered: {sorted(stale)}"


def test_the_contract_publishes_every_route_and_the_schema():
    doc = contract.build()

    assert doc["service"] == "fathom-eval-serve"
    assert sorted(doc["openapi"]["paths"]) == [
        "/healthz",
        "/meta",
        "/readyz",
        "/results/{id}",
        "/runs",
        "/runs/{id}",
    ]
    # The database is a contract in its own right: the harness writes it and the
    # read service reads it, and check_schema refuses one newer than the reader.
    assert doc["storage"]["schema_version"] == store.SCHEMA_VERSION
    assert "CREATE TABLE IF NOT EXISTS results" in doc["storage"]["ddl"]


def test_path_ids_become_parameters():
    """A templated path with no declared parameter is an unusable spec."""
    run_detail = contract.build()["openapi"]["paths"]["/runs/{id}"]["get"]
    names = {p["name"] for p in run_detail["parameters"] if p["in"] == "path"}
    assert names == {"id"}


def test_the_contract_is_byte_stable():
    """Regenerating without changing anything must produce identical bytes, or every
    commit becomes a contract commit and people stop reading the diff."""
    first = json.dumps(contract.build(), indent=2, sort_keys=True)
    second = json.dumps(contract.build(), indent=2, sort_keys=True)
    assert first == second
