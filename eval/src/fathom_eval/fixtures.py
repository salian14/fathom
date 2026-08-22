"""Fixture definitions: the staged failures the agent is asked about.

A fixture is data, not code -- one YAML file per case, shipped inside the package so the
image carries the suite it was built with. Adding a case is a data change plus a version
bump, which is what makes last week's numbers comparable to tonight's.

Two design choices are worth stating because they are what make grading tractable:

**Bare Pods with fixed names, wherever a controller is not the point.** A Deployment
generates `checkout-7d9f8b-x2k`, so the expected name has to be resolved from the live
cluster and the answer might legitimately name the Deployment, the ReplicaSet or the Pod.
A bare Pod named `crashloop-app` has none of that: the expected name is a constant in
this file. Only `service-no-endpoints` uses a Service, because the missing wiring is the
fixture.

**Every fixture declares `never` as well as `all`.** `all` is what must become true;
`never` is the set of states that mean the fixture broke in a *different* way than
intended -- an image that would not pull, say. Without it the harness happily asks ten
questions about a fixture whose failure mode is not the one the expectations describe,
and records ten confident wrong answers. That is the single easiest way to end up with a
database full of numbers that mean nothing.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from importlib import resources
from typing import Any

import yaml

from .config import NAMESPACE_PREFIX

# The image every fixture pod runs. Pinned and expected to be mirrored into the lab
# registry: a fixture whose first failure mode is a docker.io rate limit is a fixture
# that fails for reasons that have nothing to do with the agent.
FIXTURE_IMAGE = "registry.lab:5000/fathom-eval-fixture:1"

# Deliberately unresolvable, and deliberately *local*: the lab registry 404s in
# milliseconds with a stable message, where docker.io would add rate limits and
# network-dependent error text to a fixture whose whole job is to be predictable.
UNPULLABLE_IMAGE = "registry.lab:5000/fathom-eval-nonexistent:v0"


@dataclass(frozen=True)
class Question:
    id: str
    text: str
    expect: dict[str, Any]


@dataclass(frozen=True)
class Fixture:
    name: str
    isolation: str  # shared | exclusive
    order: int
    description: str
    manifests: list[dict]
    ready_all: list[dict]
    ready_never: list[dict]
    ready_timeout: float
    questions: list[Question]
    object_names: list[str] = field(default_factory=list)
    canaries: list[str] = field(default_factory=list)
    sha256: str = ""

    @property
    def namespace(self) -> str:
        return NAMESPACE_PREFIX + self.name

    @property
    def exclusive(self) -> bool:
        return self.isolation == "exclusive"


def _duration(raw: Any, default: float) -> float:
    if raw is None:
        return default
    text = str(raw).strip().lower()
    for suffix, mult in (("ms", 0.001), ("s", 1.0), ("m", 60.0), ("h", 3600.0)):
        if text.endswith(suffix):
            return float(text[: -len(suffix)]) * mult
    return float(text)


def parse(raw: str, source: str = "<string>") -> Fixture:
    doc = yaml.safe_load(raw)
    if not isinstance(doc, dict):
        raise ValueError(f"{source}: fixture must be a YAML mapping")

    for required in ("name", "description", "manifests", "questions"):
        if not doc.get(required):
            raise ValueError(f"{source}: fixture is missing required key {required!r}")

    isolation = doc.get("isolation", "shared")
    if isolation not in ("shared", "exclusive"):
        raise ValueError(f"{source}: isolation must be shared or exclusive, got {isolation!r}")

    ready = doc.get("ready") or {}
    questions = [
        Question(id=q["id"], text=q["text"], expect=q.get("expect") or {}) for q in doc["questions"]
    ]
    if len({q.id for q in questions}) != len(questions):
        raise ValueError(f"{source}: question ids must be unique within a fixture")

    return Fixture(
        name=doc["name"],
        isolation=isolation,
        order=int(doc.get("order", 50)),
        description=doc["description"].strip(),
        manifests=list(doc["manifests"]),
        ready_all=list(ready.get("all") or []),
        ready_never=list(ready.get("never") or []),
        ready_timeout=_duration(ready.get("timeout"), 240.0),
        questions=questions,
        object_names=list(doc.get("objectNames") or []),
        canaries=list(doc.get("canaries") or []),
        sha256=hashlib.sha256(raw.encode()).hexdigest()[:16],
    )


def load_all(only: tuple[str, ...] = ()) -> list[Fixture]:
    """Every fixture in the packaged suite, in declared order.

    The order is fixed rather than randomised: comparability between nights is worth
    more here than reducing ordering bias, and the `healthy` control runs first and last
    so a run that drifted mid-way is visible rather than silently averaged in.
    """
    out: list[Fixture] = []
    root = resources.files("fathom_eval") / "fixtures"
    for entry in sorted(p.name for p in root.iterdir() if p.name.endswith(".yaml")):
        text = (root / entry).read_text()
        fixture = parse(text, source=entry)
        if only and fixture.name not in only:
            continue
        out.append(fixture)

    if only:
        missing = set(only) - {f.name for f in out}
        if missing:
            raise ValueError(f"no such fixture(s): {', '.join(sorted(missing))}")

    out.sort(key=lambda f: (f.order, f.name))
    return out


def all_object_names(fixtures: list[Fixture]) -> dict[str, list[str]]:
    """Every fixture's object names, keyed by fixture.

    Used to build the `forbidden` name set for each question: naming an object that
    belongs to a *different* fixture can only be fabrication, because fixtures run one
    at a time and that object does not exist while this question is being asked.
    """
    return {f.name: list(f.object_names) for f in fixtures}
