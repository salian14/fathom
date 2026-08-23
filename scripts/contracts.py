#!/usr/bin/env python3
"""Write contracts/<component>/ from what the components say about themselves.

    make contracts          regenerate everything
    make contracts COMPONENT=agent
    make contracts-check    regenerate to a temp dir and diff; non-zero on drift

Two sources are merged per component, and neither is parsed out of source code:

  * `make -C <component> contract` -- the component runs itself and prints its own
    interface. Routes, tool schemas and body shapes come from here, so they are what
    the code actually does rather than what a parser believed about it.
  * `helm template <component>/helm` -- the facts the process cannot know about
    itself: which namespace it lands in, what Service type fronts it, what nodePort
    if any, and what RBAC it is granted.

Where the two overlap they are **cross-checked, not merged**: the port the binary
listens on by default and the port the chart publishes have to agree, and a
disagreement fails the run naming both. They agree today by construction -- every
deployment template sets FATHOM_<C>_ADDR from the same service.port it uses for
containerPort -- and this is what keeps it that way.

Runs on a component venv's interpreter, for PyYAML. There is no root Python project
here and `yq` is not installed, so this is the YAML reader the repo already has.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

import yaml

REPO = Path(__file__).resolve().parent.parent
CONTRACTS = REPO / "contracts"

# Build order, not alphabetical: the same order the root Makefile uses, so a full
# run reads in the order a deploy would.
COMPONENTS = ["mcp", "agent", "front", "eval"]

# The Helm release name each chart is rendered under. Matches BINARY in each
# component Makefile, because that is the name `make render-chart` uses and a
# contract rendered under a different release name would carry different object
# names than the one anybody actually installs.
RELEASE = {
    "mcp": "fathom-mcp",
    "agent": "fathom-agent",
    "front": "fathom-front",
    "eval": "fathom-eval",
}

# The version of the layout this writer produces, independent of each component's
# own contract schema_version.
LAYOUT_VERSION = 1

INTEGRATION_STUB = """\
# {service} -- integration notes

**Hand-written. `make contracts` never touches this file.**

The generated files beside it describe the shape of the interface. This one describes
how to use it without causing an incident -- the part a spec cannot express.

## Retries

_Which failures are worth retrying, and which are not._

## Timeouts

_What this service's budget is, and whose budget it sits inside._

## Idempotency

_Whether repeating a call is safe, and what makes two calls the same call._

## Ordering and concurrency

_What may run at the same time, and what must not._

## Auth

_There is no auth between services in this repo -- the cluster is the trust boundary.
Record here anything that depends on that staying true._
"""


class Drift(Exception):
    """A fact the component and its chart disagree about."""


def run(cmd: list[str], cwd: Path | None = None) -> str:
    """Run a command, returning stdout. stderr is left attached to the terminal so
    a build failure is legible instead of swallowed."""
    proc = subprocess.run(cmd, cwd=cwd or REPO, stdout=subprocess.PIPE, text=True)
    if proc.returncode != 0:
        raise SystemExit(f"contracts: {' '.join(cmd)} failed with {proc.returncode}")
    return proc.stdout


def component_contract(component: str) -> dict[str, Any]:
    """Ask the component to describe itself."""
    # --no-print-directory because make writes "Entering directory" to stdout, which
    # would corrupt the document; the component Makefiles keep their own build
    # chatter on stderr for the same reason.
    out = run(["make", "--no-print-directory", "-C", component, "contract"])
    try:
        return json.loads(out)
    except json.JSONDecodeError as err:
        raise SystemExit(
            f"contracts: {component} printed something that is not JSON on stdout "
            f"({err}). `make -C {component} contract` must print the document and "
            f"nothing else."
        ) from err


def chart_objects(component: str) -> list[dict[str, Any]]:
    """Render the component's chart and return every object in it."""
    out = run(["helm", "template", RELEASE[component], f"{component}/helm"])
    return [doc for doc in yaml.safe_load_all(out) if doc]


def chart_facts(component: str) -> dict[str, Any]:
    """Extract the deployment-shaped facts a process cannot know about itself."""
    objects = chart_objects(component)

    namespaces = sorted(
        {o["metadata"]["name"] for o in objects if o.get("kind") == "Namespace"}
    ) or sorted({o["metadata"].get("namespace") for o in objects if o["metadata"].get("namespace")})

    services = []
    for o in objects:
        if o.get("kind") != "Service":
            continue
        spec = o.get("spec", {})
        for port in spec.get("ports", []):
            services.append(
                {
                    "name": o["metadata"]["name"],
                    "namespace": o["metadata"].get("namespace"),
                    "type": spec.get("type", "ClusterIP"),
                    "port": port.get("port"),
                    "target_port": port.get("targetPort"),
                    "node_port": port.get("nodePort"),
                    "protocol": port.get("protocol", "TCP"),
                }
            )

    workloads = []
    for o in objects:
        kind = o.get("kind")
        if kind not in ("Deployment", "CronJob", "StatefulSet", "DaemonSet", "Job"):
            continue
        entry: dict[str, Any] = {"name": o["metadata"]["name"], "kind": kind}
        if kind == "CronJob":
            entry["schedule"] = o["spec"].get("schedule")
            entry["suspend"] = o["spec"].get("suspend")
            containers = o["spec"]["jobTemplate"]["spec"]["template"]["spec"]["containers"]
        else:
            containers = o["spec"]["template"]["spec"]["containers"]
        entry["images"] = [c.get("image") for c in containers]
        entry["container_ports"] = [
            p.get("containerPort") for c in containers for p in c.get("ports", []) or []
        ]
        workloads.append(entry)

    rbac = []
    for o in objects:
        if o.get("kind") not in ("Role", "ClusterRole"):
            continue
        rbac.append(
            {
                "kind": o["kind"],
                "name": o["metadata"]["name"],
                "rules": o.get("rules", []),
            }
        )

    return {
        "namespaces": namespaces,
        "services": sorted(services, key=lambda s: (s["name"], s["port"] or 0)),
        "workloads": sorted(workloads, key=lambda w: w["name"]),
        "rbac": sorted(rbac, key=lambda r: (r["kind"], r["name"])),
    }


def chart_version(component: str) -> str:
    """The appVersion the chart ships, which is the version that actually deploys.

    The writer owns the version in every generated file, and deliberately overrides
    whatever the component put there. Two of the four stamp themselves with
    `git describe --always --dirty` at build time, which changes on every commit --
    publishing that would rewrite the contract on every commit and turn the diff,
    the only thing this exists to make readable, into noise. appVersion moves once
    per `make release`, which is when the interface can actually have changed.
    """
    chart = yaml.safe_load((REPO / component / "helm" / "Chart.yaml").read_text())
    return str(chart.get("appVersion") or chart.get("version"))


def declared_port(contract: dict[str, Any]) -> int | None:
    """The port the process listens on by its own account.

    Every component publishes a listen address in `defaults`, spelled the same way
    across the repo -- ":8080" or "127.0.0.1:8080" -- so one reader does for all four.
    """
    defaults = contract.get("defaults", {})
    for key in ("addr", "serve_addr"):
        addr = defaults.get(key)
        if isinstance(addr, str) and ":" in addr:
            try:
                return int(addr.rsplit(":", 1)[1])
            except ValueError:
                return None
    return None


def cross_check(component: str, contract: dict[str, Any], facts: dict[str, Any]) -> None:
    """Fail if the code and the chart disagree about how this service is reached.

    This is the check the whole exercise turns on. The two live in different files
    with no compiler between them, and the failure mode is silent: a pod that starts,
    passes its own probes and is unreachable through its Service.
    """
    service_name = contract.get("service")
    port = declared_port(contract)
    if port is None:
        return

    fronting = [s for s in facts["services"] if s["name"] == service_name]
    if not fronting:
        # A component with no Service of its own name is legitimate -- eval's two
        # CronJobs have none -- but the one that declares a listen address is not.
        raise Drift(
            f"{component}: the contract names {service_name!r} and declares port "
            f"{port}, but the chart renders no Service called {service_name!r}. "
            f"Services in the chart: {[s['name'] for s in facts['services']] or 'none'}"
        )

    for svc in fronting:
        if svc["port"] != port:
            raise Drift(
                f"{component}: {service_name} listens on {port} by its own default "
                f"(FATHOM_{component.upper()}_ADDR) but the chart publishes "
                f"service.port {svc['port']}. These are set in two files and nothing "
                f"else checks they agree: fix one of them."
            )

    declared_container_ports = {
        p for w in facts["workloads"] for p in w["container_ports"] if p is not None
    }
    if declared_container_ports and port not in declared_container_ports:
        raise Drift(
            f"{component}: listens on {port} but the chart's containerPort(s) are "
            f"{sorted(declared_container_ports)}."
        )


def service_document(component: str, contract: dict[str, Any], facts: dict[str, Any]) -> dict:
    """The identity file: what this is called, where it runs and how it is reached."""
    return {
        "layout_version": LAYOUT_VERSION,
        "component": component,
        "service": contract.get("service"),
        "version": chart_version(component),
        "namespaces": facts["namespaces"],
        "listen_port": declared_port(contract),
        "services": facts["services"],
        "workloads": facts["workloads"],
        "rbac": facts["rbac"],
        "probes": contract.get("probes", {}),
        "defaults": contract.get("defaults", {}),
        "depends_on": contract.get("depends_on", []),
    }


def dump(path: Path, payload: Any) -> None:
    """Write JSON deterministically: sorted keys, one trailing newline.

    Byte stability is not cosmetic. A generator whose output moves on its own makes
    every commit a contract commit, and people stop reading the diff -- which is the
    only thing the diff was for.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")


def write_component(component: str, root: Path) -> list[Path]:
    contract = component_contract(component)
    facts = chart_facts(component)
    cross_check(component, contract, facts)

    out = root / component
    written = [out / "service.json"]
    dump(out / "service.json", service_document(component, contract, facts))

    if "config" in contract:
        dump(
            out / "config.json",
            {
                "layout_version": LAYOUT_VERSION,
                "note": "Recorded by the config loader as it ran, not parsed out of it: "
                "every variable goes through one of a handful of env helpers, and those "
                "helpers record what they were asked for.",
                "variables": contract["config"],
            },
        )
        written.append(out / "config.json")

    if "telemetry" in contract:
        dump(out / "telemetry.json", {"layout_version": LAYOUT_VERSION, **contract["telemetry"]})
        written.append(out / "telemetry.json")

    if "openapi" in contract:
        spec = contract["openapi"]
        spec.setdefault("info", {})["version"] = chart_version(component)
        dump(out / "openapi.json", spec)
        written.append(out / "openapi.json")

    if "tools" in contract:
        dump(
            out / "mcp-tools.json",
            {
                "layout_version": LAYOUT_VERSION,
                "protocol": contract.get("protocol", {}),
                "tools": contract["tools"],
            },
        )
        written.append(out / "mcp-tools.json")

    if "routes" in contract:
        dump(
            out / "routes.json",
            {"layout_version": LAYOUT_VERSION, "routes": contract["routes"]},
        )
        written.append(out / "routes.json")

    storage = contract.get("storage")
    if storage:
        ddl = storage.pop("ddl", None)
        dump(out / "storage.json", {"layout_version": LAYOUT_VERSION, **storage})
        written.append(out / "storage.json")
        if ddl:
            # The DDL goes out as .sql rather than a JSON string so it can be read,
            # diffed and piped into sqlite3 without unescaping it first.
            (out / "schema.sql").write_text(ddl.rstrip() + "\n")
            written.append(out / "schema.sql")

    if "fixture_namespaces" in contract:
        dump(
            out / "fixture-namespaces.json",
            {
                "layout_version": LAYOUT_VERSION,
                "namespaces": contract["fixture_namespaces"],
                "note": "Two-file agreement: these must appear in "
                "eval/helm -> rbac.fixtureNamespaces or the harness cannot stage them.",
            },
        )
        written.append(out / "fixture-namespaces.json")

    # Created once, then never touched. It holds the retry, idempotency and ordering
    # semantics a spec cannot express -- the part that causes incidents when it is
    # missing -- and overwriting it would silently destroy the most valuable content
    # in the directory.
    notes = out / "integration-notes.md"
    if not notes.exists():
        notes.write_text(INTEGRATION_STUB.format(service=contract.get("service", component)))
        written.append(notes)

    return written


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "components",
        nargs="*",
        default=None,
        help="components to regenerate (default: all)",
    )
    parser.add_argument(
        "--out",
        default=None,
        help="write here instead of contracts/ (used by contracts-check)",
    )
    args = parser.parse_args()

    wanted = args.components or COMPONENTS
    unknown = [c for c in wanted if c not in COMPONENTS]
    if unknown:
        raise SystemExit(f"contracts: unknown component(s): {', '.join(unknown)}")

    root = Path(args.out).resolve() if args.out else CONTRACTS
    root.mkdir(parents=True, exist_ok=True)

    for component in [c for c in COMPONENTS if c in wanted]:
        try:
            written = write_component(component, root)
        except Drift as err:
            print(f"contracts: {err}", file=sys.stderr)
            return 1
        rel = [str(p.relative_to(root.parent)) for p in written]
        print(f"contracts: {component} -> {', '.join(sorted(rel))}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    os.chdir(REPO)
    sys.exit(main())
