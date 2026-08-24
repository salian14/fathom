#!/usr/bin/env python3
"""Write contracts/<component>/ by running each component and asking it.

    make contracts          regenerate everything
    make contracts COMPONENT=agent
    make contracts-check    regenerate to a temp dir and diff; non-zero on drift

**No component contains a line of code that exists to produce a contract.** That is
the constraint this file is built around, and it is why the work happens here rather
than in the services. Each component is built into its image, started in a throwaway
container configured exactly the way its own chart configures it, and scraped over
the wire like any other client would:

  agent   GET /openapi.json          FastAPI publishes this already. Nothing was
                                     added to the service to obtain it.
  mcp     initialize + tools/list    A real MCP session over streamable HTTP, run
                                     twice -- see prometheus_variants() -- because
                                     three of the six tools are gated on a value.
  eval    GET /openapi.json, and     Also FastAPI, since 2026-08-23. The DDL is read
          the applied SQLite schema  back out of a database the image builds for
                                     itself, so what is published is CREATE TABLE plus
                                     every migration actually applied -- not what the
                                     source says it will be.
  front   GET /healthz               There is nothing to scrape: Go's ServeMux does
                                     not report what was registered with it and the
                                     binary publishes no spec. The container is still
                                     started, because the port check below is worth
                                     having on its own.

`helm template` supplies the rest: the facts a process cannot know about itself --
which namespace it lands in, what Service fronts it, what RBAC it is granted, what
its probes are.

## What the container run actually checks

The old writer compared the port a binary *defaults* to against the chart's
`service.port`. That asserts a coincidence rather than a requirement: every Deployment
here sets `FATHOM_<C>_ADDR` from `service.port`, so the code's default never applies in
the cluster at all. The comparison was wrong in both directions -- it would have failed
a chart that legitimately published a non-default port, and it could not see whether the
process actually ended up listening anywhere in particular.

Running the container with the chart's own environment and then dialling the chart's own
`containerPort` checks the thing that matters instead: the variable is spelled the way
the binary reads it, the value parses, and the process ends up where the Service will
look for it. A failure here is the pod that starts, passes its own probes and is
unreachable -- caught before it ships.

Two environment overrides are applied to the chart's env, and only two:

  * `FATHOM_<C>_TELEMETRY=off`, because the collector is a cluster DNS name that does
    not resolve on a workstation, and an exporter retrying in the background adds
    nothing to a spec.
  * `FATHOM_MCP_KUBECONFIG_PATH`, pointing at a kubeconfig for a server that does not
    exist. `fathom-mcp` builds a client-go clientset at startup and fails without
    credentials, but client-go does not dial when it is constructed, so a file naming
    an unreachable API server is enough to get the process to its listener. Nothing
    reachable is ever configured: the sandbox must not be able to touch a real
    cluster, and this is what guarantees it.

## Without Docker

`make contracts` passes `--allow-offline` and then degrades rather than refusing.
`service.json` is rendered by `helm template` and never needed a daemon. `agent` and
`eval` are started from their own venvs instead of their images and scraped through the
same `/openapi.json` -- FastAPI serves it either way, and the spec is built from the
routes and models in the source, which is what both the image and the venv install. The
two are therefore byte-identical between the modes, and that is load-bearing: the
pre-commit hook uses this path, so a document that differed by mode would poison the
next `make check` on every commit made offline.

What offline cannot do is run a Go binary, so `mcp-tools.json` is left as it is, and
neither Go component gets its containerPort dialled. Those files are never deleted --
a stale one that says so on stderr is recoverable, an absent one reads as a removed
interface. `contracts-check` does not accept the flag at all: it is the drift gate
inside `make check`, and a gate that silently checks less than it claims is worse than
one that refuses.

Runs on the agent venv's interpreter, which already carries the three things needed
here -- PyYAML, httpx and the MCP SDK -- so this adds no dependency to anything.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx
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

# The module each Python component runs as, for the offline path below. Only the
# two FastAPI services have one: `mcp` and `front` are Go binaries, and building
# them is not meaningfully cheaper than building their images.
LOCAL_MODULE = {
    "agent": "fathom_agent",
    "eval": "fathom_eval",
}

# The variable each component reads its listen address from. `eval` is the odd one
# out because its image is a CLI with several subcommands and only one of them
# listens, so the address belongs to the subcommand rather than the process.
ADDR_ENV = {
    "agent": "FATHOM_AGENT_ADDR",
    "eval": "FATHOM_EVAL_SERVE_ADDR",
}

# The version of the layout this writer produces.
LAYOUT_VERSION = 2

# Images are built under a tag of this writer's own, rather than the component
# Makefile's registry.lab:5000 default, so a scrape never collides with -- or is
# mistaken for -- something built to be pushed.
SCRAPE_TAG = "scrape"
SCRAPE_REGISTRY = "fathom-contracts"

# How long a container gets to start listening before the run is called a failure.
# Generous: the first start of a Python image on a cold page cache is not fast, and
# a flaky generator is worse than a slow one.
STARTUP_TIMEOUT = 90.0

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

# A kubeconfig for a server that is not there. Mounted into the fathom-mcp sandbox so
# client-go can build a clientset; it never connects, and there is deliberately
# nothing here that could reach a real cluster.
DEAD_KUBECONFIG = """\
apiVersion: v1
kind: Config
clusters:
  - name: contract-sandbox
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: contract-sandbox
    context:
      cluster: contract-sandbox
      user: contract-sandbox
current-context: contract-sandbox
users:
  - name: contract-sandbox
    user:
      token: ""
"""


class Drift(Exception):
    """A fact the component and its chart disagree about."""


def run(cmd: list[str], cwd: Path | None = None, capture: bool = True) -> str:
    """Run a command, returning stdout. stderr is left attached to the terminal so
    a build failure is legible instead of swallowed."""
    proc = subprocess.run(
        cmd,
        cwd=cwd or REPO,
        stdout=subprocess.PIPE if capture else None,
        text=True,
    )
    if proc.returncode != 0:
        raise SystemExit(f"contracts: {' '.join(cmd)} failed with {proc.returncode}")
    return proc.stdout or ""


# --- what the chart knows --------------------------------------------------------


def chart_objects(component: str) -> list[dict[str, Any]]:
    """Render the component's chart and return every object in it."""
    out = run(["helm", "template", RELEASE[component], f"{component}/helm"])
    return [doc for doc in yaml.safe_load_all(out) if doc]


def chart_facts(component: str) -> dict[str, Any]:
    """Extract the deployment-shaped facts a process cannot know about itself."""
    objects = chart_objects(component)

    namespaces = sorted(
        {o["metadata"]["name"] for o in objects if o.get("kind") == "Namespace"}
    ) or sorted(
        {o["metadata"].get("namespace") for o in objects if o["metadata"].get("namespace")}
    )

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
        "probes": chart_probes(objects),
        "serving_container": serving_container(objects),
    }


def serving_container(objects: list[dict[str, Any]]) -> dict[str, Any] | None:
    """The container spec of the long-running workload, if there is one.

    This is what the sandbox is configured from. `eval` renders two CronJobs and one
    Deployment; only the Deployment listens, and only it is scraped.
    """
    for o in objects:
        if o.get("kind") != "Deployment":
            continue
        containers = o["spec"]["template"]["spec"]["containers"]
        for c in containers:
            if c.get("ports"):
                return c
    return None


def chart_probes(objects: list[dict[str, Any]]) -> dict[str, Any]:
    """The probe paths the chart configures, which is where they are actually set."""
    container = serving_container(objects)
    if not container:
        return {}
    out: dict[str, Any] = {}
    for key, name in (("livenessProbe", "liveness"), ("readinessProbe", "readiness")):
        probe = container.get(key) or {}
        http = probe.get("httpGet")
        if http:
            out[name] = http.get("path")
    return out


def chart_version(component: str) -> str:
    """The appVersion the chart ships, which is the version that actually deploys.

    The writer owns the version in every generated file, and deliberately overrides
    whatever the image reports. Two of the four stamp themselves with
    `git describe --always --dirty` at build time, which changes on every commit --
    publishing that would rewrite the contract on every commit and turn the diff,
    the only thing this exists to make readable, into noise. appVersion moves once
    per `make release`, which is when the interface can actually have changed.
    """
    chart = yaml.safe_load((REPO / component / "helm" / "Chart.yaml").read_text())
    return str(chart.get("appVersion") or chart.get("version"))


def listen_port(facts: dict[str, Any]) -> int | None:
    """The port the chart tells the process to listen on."""
    container = facts["serving_container"]
    if not container:
        return None
    for port in container.get("ports") or []:
        if port.get("containerPort"):
            return int(port["containerPort"])
    return None


def chart_env(facts: dict[str, Any]) -> dict[str, str]:
    """The literal environment the chart gives the container.

    Entries sourced from a fieldRef are skipped: they are pod identity, they have no
    value outside a cluster, and the `$(POD_NAME)` references in
    OTEL_RESOURCE_ATTRIBUTES would not expand anyway. Nothing in the published
    contract depends on them.
    """
    container = facts["serving_container"] or {}
    return {
        e["name"]: e["value"]
        for e in container.get("env") or []
        if e.get("value") is not None
    }


def cross_check(component: str, facts: dict[str, Any]) -> None:
    """Fail if the chart contradicts itself about how this service is reached.

    The stronger half of the port check is the sandbox itself, which dials the port
    below and fails if nothing answers. This catches the case that never gets that
    far: a Service publishing a port the container does not open.
    """
    port = listen_port(facts)
    if port is None:
        return
    service_name = RELEASE[component]
    fronting = [s for s in facts["services"] if s["name"] == service_name]
    if not fronting:
        # Any Service will do for a component whose Service is not named after its
        # release -- eval's read service is `fathom-eval-serve`.
        fronting = facts["services"]
    for svc in fronting:
        target = svc.get("target_port")
        if target in (None, port, "http"):
            continue
        raise Drift(
            f"{component}: the container opens {port} but Service {svc['name']!r} "
            f"targets {target!r}. These are set in two places in {component}/helm "
            f"and nothing else checks they agree."
        )


# --- the sandbox -----------------------------------------------------------------


def free_port() -> int:
    """An unused loopback port for the container to publish onto."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def build_image(component: str) -> str:
    """Build the component's image through its own Makefile.

    Overriding IMAGE and VERSION rather than reimplementing `docker build` keeps one
    definition of how each component is built -- the component's, which is the one
    that also ships.
    """
    image = f"{SCRAPE_REGISTRY}/{RELEASE[component]}"
    print(f"contracts: building {image}:{SCRAPE_TAG}", file=sys.stderr)
    run(
        [
            "make",
            "--no-print-directory",
            "-C",
            component,
            "image",
            f"IMAGE={image}",
            f"VERSION={SCRAPE_TAG}",
        ],
        capture=False,
    )
    return f"{image}:{SCRAPE_TAG}"


def sandbox_env(component: str, facts: dict[str, Any]) -> dict[str, str]:
    """The chart's environment, with the two documented overrides applied."""
    env = {k: v for k, v in chart_env(facts).items() if not k.startswith("OTEL_")}
    env[f"FATHOM_{component.upper()}_TELEMETRY"] = "off"
    if component == "mcp":
        env["FATHOM_MCP_KUBECONFIG_PATH"] = "/sandbox/kubeconfig"
    return env


@contextlib.contextmanager
def sandbox(component: str, image: str, facts: dict[str, Any], extra_env: dict[str, str]):
    """Start the component in a throwaway container and yield its base URL.

    Published on loopback only, with every capability dropped: this is a build step
    on someone's workstation, and it has no business being reachable or privileged.
    """
    port = listen_port(facts)
    if port is None:
        raise Drift(f"{component}: the chart opens no containerPort, so there is nothing to scrape")

    host_port = free_port()
    name = f"fathom-contract-{component}-{uuid.uuid4().hex[:8]}"
    env = {**sandbox_env(component, facts), **extra_env}

    container = facts["serving_container"] or {}

    # Deliberately not --rm: a container that exits before it listens is the most
    # informative failure this writer has, and --rm destroys its logs before they
    # can be read. It is removed in the finally below instead.
    cmd = [
        "docker", "run", "--detach",
        "--name", name,
        "--cap-drop", "ALL",
        "--publish", f"127.0.0.1:{host_port}:{port}",
    ]
    for key, value in sorted(env.items()):
        cmd += ["--env", f"{key}={value}"]

    # Every volume the chart mounts gets an empty tmpfs. What is in them is never
    # part of an interface -- eval's /data holds a results database that does not
    # exist yet on a fresh install, which is a state the service already handles --
    # but a mount point that is missing entirely is not a state it has to handle.
    for mount in container.get("volumeMounts") or []:
        cmd += ["--tmpfs", mount["mountPath"]]

    kubeconfig: Path | None = None
    if component == "mcp":
        kubeconfig = Path(run(["mktemp", "-d"]).strip()) / "kubeconfig"
        kubeconfig.write_text(DEAD_KUBECONFIG)
        kubeconfig.chmod(0o644)
        cmd += ["--volume", f"{kubeconfig}:/sandbox/kubeconfig:ro"]

    # The chart's command and args, so the sandbox starts the same process the
    # cluster does. `eval` is the one that needs this -- its image serves the read
    # API only when handed `serve` -- but taking it from the chart rather than
    # special-casing that means the next component to need it is already handled.
    if container.get("command"):
        cmd += ["--entrypoint", container["command"][0]]

    cmd.append(image)
    if container.get("command"):
        cmd += list(container["command"][1:])
    cmd += list(container.get("args") or [])
    run(cmd)

    base = f"http://127.0.0.1:{host_port}"

    def tail() -> str:
        logs = subprocess.run(
            ["docker", "logs", "--tail", "40", name],
            capture_output=True, text=True,
        )
        return logs.stdout + logs.stderr

    try:
        await_listener(
            component, base, facts,
            alive=lambda: container_running(name),
            tail=tail,
            hint=(
                f"The chart sets FATHOM_{component.upper()}_ADDR and containerPort {port} "
                f"from the same value in {component}/helm; if the process is listening "
                f"somewhere else, one of them is wrong and the pod would start, pass its "
                f"probes and be unreachable through its Service."
            ),
        )
        yield base
    finally:
        subprocess.run(
            ["docker", "rm", "-f", name],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if kubeconfig:
            with contextlib.suppress(OSError):
                kubeconfig.unlink()
                kubeconfig.parent.rmdir()


def venv_python(component: str) -> Path | None:
    """The component's own interpreter, or None if it has not been built yet."""
    python = REPO / component / ".venv" / "bin" / "python"
    return python if python.exists() else None


@contextlib.contextmanager
def local_sandbox(component: str, facts: dict[str, Any], python: Path):
    """Start the component from its own venv and yield its base URL.

    The offline counterpart of sandbox(), and it exists for one reason: FastAPI serves
    /openapi.json itself, so the two Python services can be scraped without a daemon.
    A pre-commit hook that regenerates nothing when Docker is down lets a stale
    contract through on every commit made offline, and the spec is the same document
    either way -- it is built from the routes and models in the source, which is what
    both the image and the venv install.

    What this gives up is the half of the container run that is not the spec: the
    chart's containerPort is not dialled. Nothing may be listening on it on a
    workstation and binding it would collide, so a free loopback port is used instead
    and the pod-starts-but-is-unreachable check stays a Docker-only assertion.
    """
    port = free_port()
    addr_env = ADDR_ENV[component]
    env = sandbox_env(component, facts)
    env[addr_env] = f"127.0.0.1:{port}"

    tmp = Path(tempfile.mkdtemp(prefix=f"fathom-contract-{component}-"))
    if component == "eval":
        # The chart points at /data, which the container run satisfies with a tmpfs
        # and nothing satisfies here.
        env["FATHOM_EVAL_DB_PATH"] = str(tmp / "results.db")

    # A deliberately minimal inherited environment. A FATHOM_* override or an OTEL_*
    # endpoint that happens to be exported in someone's shell would change what the
    # process serves and therefore what gets published, and a contract that depends
    # on whose terminal produced it is not a contract.
    inherited = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "LANG", "LC_ALL")}
    inherited["TMPDIR"] = str(tmp)

    # The chart's args, for the same reason the container run takes them: `eval`
    # serves the read API only when handed `serve`, and reading that from the chart
    # rather than special-casing it means the next component to need one is handled.
    args = list((facts["serving_container"] or {}).get("args") or [])

    log = tmp / "output.log"
    handle = log.open("wb")
    proc = subprocess.Popen(
        [str(python), "-m", LOCAL_MODULE[component], *args],
        cwd=REPO / component,
        env={**inherited, **env},
        stdout=handle,
        stderr=subprocess.STDOUT,
    )

    base = f"http://127.0.0.1:{port}"
    try:
        await_listener(
            component, base, facts,
            alive=lambda: proc.poll() is None,
            tail=lambda: log.read_text(errors="replace")[-4000:],
            hint=(
                f"`python -m {LOCAL_MODULE[component]}` was started from "
                f"{component}/.venv with {addr_env}={env[addr_env]}. If the venv is "
                f"out of step with requirements.lock, `make -C {component} venv` "
                f"rebuilds it."
            ),
        )
        yield base
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            with contextlib.suppress(subprocess.TimeoutExpired):
                proc.wait(timeout=10)
        handle.close()
        shutil.rmtree(tmp, ignore_errors=True)


def await_listener(
    component: str,
    base: str,
    facts: dict[str, Any],
    alive: Callable[[], bool],
    tail: Callable[[], str],
    hint: str,
) -> None:
    """Block until the process under test answers its own liveness path.

    Liveness, never readiness: `/readyz` means "my dependencies answered", and in the
    sandbox they deliberately cannot. `/healthz` means the process is up and listening,
    which is the whole assertion.

    Both sandboxes share this loop, and differ only in how "still alive", "last output"
    and "what it means when nothing answered" are spelled -- which is exactly the three
    arguments.
    """
    path = facts["probes"].get("liveness") or "/healthz"
    deadline = time.monotonic() + STARTUP_TIMEOUT
    last: Exception | None = None
    while time.monotonic() < deadline:
        if not alive():
            raise Drift(
                f"{component}: it exited before it listened. Its last output:\n{tail()}"
            )
        try:
            response = httpx.get(base + path, timeout=2.0)
            if response.status_code == 200:
                return
        except httpx.HTTPError as err:  # not up yet
            last = err
        time.sleep(0.4)

    raise Drift(
        f"{component}: nothing answered {path} within {STARTUP_TIMEOUT:.0f}s. {hint} "
        f"Last error: {last}"
    )


def container_running(name: str) -> bool:
    probe = subprocess.run(
        ["docker", "inspect", "-f", "{{.State.Running}}", name],
        capture_output=True, text=True,
    )
    return probe.returncode == 0 and probe.stdout.strip() == "true"


# --- the scrapes -----------------------------------------------------------------


def scrape_openapi(base: str) -> dict[str, Any]:
    """FastAPI publishes this itself. That is the entire integration."""
    response = httpx.get(base + "/openapi.json", timeout=30.0)
    response.raise_for_status()
    return response.json()


def prometheus_variants(component: str, facts: dict[str, Any]) -> list[tuple[str, dict[str, str]]]:
    """The two configurations fathom-mcp has to be scraped in.

    Three of the six tools are registered only when `prometheus.url` is set, so one
    scrape describes one deployment and misdescribes the other. Both are published,
    and "these three exist only when configured" is exactly the fact a consumer needs.
    """
    return [
        ("always", {"FATHOM_MCP_PROMETHEUS_URL": ""}),
        ("with_prometheus", {}),
    ]


async def _mcp_session(url: str) -> tuple[str | None, list[dict[str, Any]]]:
    from mcp import ClientSession
    from mcp.client.streamable_http import streamablehttp_client

    async with streamablehttp_client(url, timeout=30) as (read, write, _):
        async with ClientSession(read, write) as session:
            # The instructions come back in the initialize result, so they are
            # scraped rather than declared -- they are the prompt the model is
            # actually handed, and they change with the same gate the tools do.
            initialized = await session.initialize()
            listing = await session.list_tools()

    tools = [
        json.loads(tool.model_dump_json(exclude_none=True))
        for tool in sorted(listing.tools, key=lambda t: t.name)
    ]
    return initialized.instructions, tools


def scrape_mcp(base: str, path: str) -> tuple[str | None, list[dict[str, Any]]]:
    """A real MCP session: initialize, then tools/list, exactly as the agent does."""
    return asyncio.run(_mcp_session(base + path))


def scrape_eval_storage(run_python: Callable[[str], str]) -> tuple[dict[str, Any], str, list[str]]:
    """Read the results schema back out of a database `fathom_eval` builds for itself.

    Running the store rather than reading `store.SCHEMA` is the difference between
    publishing what the DDL says and publishing what the DDL plus every migration
    leaves behind -- and the migrations are the half that drifts.

    How the script is executed is the caller's business -- `python` inside the image,
    or the eval venv's interpreter when there is no daemon. The script itself is one
    copy, because two would be two answers.
    """
    script = """
import json, sqlite3, tempfile, pathlib
from fathom_eval import fixtures, store

path = str(pathlib.Path(tempfile.mkdtemp()) / "results.db")
with store.opened(path):
    pass

conn = sqlite3.connect(path)
ddl = [
    row[0]
    for row in conn.execute(
        "SELECT sql FROM sqlite_master WHERE sql IS NOT NULL "
        "ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, name"
    )
]
print(json.dumps({
    "schema_version": store.SCHEMA_VERSION,
    "ddl": ";\\n\\n".join(ddl) + ";",
    "fixture_namespaces": sorted({f.namespace for f in fixtures.load_all()}),
}))
"""
    payload = json.loads(run_python(script))
    return (
        {"engine": "sqlite", "schema_version": payload["schema_version"]},
        payload["ddl"],
        payload["fixture_namespaces"],
    )


# --- writing ---------------------------------------------------------------------


def dump(path: Path, payload: Any) -> None:
    """Write JSON deterministically: sorted keys, one trailing newline.

    Byte stability is not cosmetic. A generator whose output moves on its own makes
    every commit a contract commit, and people stop reading the diff -- which is the
    only thing the diff was for.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")


def service_document(component: str, facts: dict[str, Any]) -> dict:
    """The identity file: what this is called, where it runs and how it is reached."""
    return {
        "layout_version": LAYOUT_VERSION,
        "component": component,
        "service": RELEASE[component],
        "version": chart_version(component),
        "namespaces": facts["namespaces"],
        "listen_port": listen_port(facts),
        "services": facts["services"],
        "workloads": facts["workloads"],
        "rbac": facts["rbac"],
        "probes": facts["probes"],
    }


@contextlib.contextmanager
def serving(component: str, facts: dict[str, Any], image: str | None, python: Path | None):
    """Whichever sandbox this run has, yielding the base URL to scrape."""
    if image is not None:
        with sandbox(component, image, facts, {}) as base:
            yield base
    else:
        assert python is not None
        with local_sandbox(component, facts, python) as base:
            yield base


def python_runner(image: str | None, python: Path | None, component: str) -> Callable[[str], str]:
    """How to run a throwaway Python script against this component's dependencies."""
    if image is not None:
        return lambda script: run(
            ["docker", "run", "--rm", "--cap-drop", "ALL", "--entrypoint", "python", image, "-c", script]
        )
    assert python is not None
    return lambda script: run([str(python), "-c", script], cwd=REPO / component)


def scrape_interface(
    component: str,
    facts: dict[str, Any],
    out: Path,
    image: str | None,
    python: Path | None,
) -> list[Path]:
    """Write everything that comes from the component rather than from its chart."""
    # The two Go components are only ever reached with an image; write_component does
    # not offer them the offline path, because there is nothing to serve their
    # interface from without building the binary.
    assert component in LOCAL_MODULE or image is not None
    written: list[Path] = []

    if component == "agent":
        with serving(component, facts, image, python) as base:
            spec = scrape_openapi(base)
        spec.setdefault("info", {})["version"] = chart_version(component)
        dump(out / "openapi.json", spec)
        written.append(out / "openapi.json")

    elif component == "mcp":
        path = chart_env(facts).get("FATHOM_MCP_MCP_PATH", "/mcp")
        surfaces: dict[str, list[dict[str, Any]]] = {}
        instructions: dict[str, str | None] = {}
        for label, overrides in prometheus_variants(component, facts):
            with sandbox(component, image, facts, overrides) as base:
                instructions[label], surfaces[label] = scrape_mcp(base, path)

        always = {t["name"] for t in surfaces["always"]}
        dump(
            out / "mcp-tools.json",
            {
                "layout_version": LAYOUT_VERSION,
                "protocol": {
                    "kind": "mcp",
                    "transport": "streamable-http",
                    "path": path,
                    "instructions": {
                        "without_prometheus": instructions["always"],
                        "with_prometheus": instructions["with_prometheus"],
                    },
                },
                "note": "Scraped from a real tools/list against a running container, "
                "once with prometheus.url unset and once with the chart's value. Tools "
                "marked gated are absent from tools/list on an install without a URL.",
                "tools": [
                    {**tool, "gated_on": None if tool["name"] in always else "prometheus.url"}
                    for tool in surfaces["with_prometheus"]
                ],
            },
        )
        written.append(out / "mcp-tools.json")

    elif component == "front":
        # Nothing to scrape -- see the module docstring. The container is still
        # started, because the port check is the reason this run exists.
        with sandbox(component, image, facts, {}):
            pass

    elif component == "eval":
        with serving(component, facts, image, python) as base:
            spec = scrape_openapi(base)
        spec.setdefault("info", {})["version"] = chart_version(component)
        dump(out / "openapi.json", spec)
        written.append(out / "openapi.json")

        storage, ddl, namespaces = scrape_eval_storage(python_runner(image, python, component))
        dump(
            out / "storage.json",
            {
                "layout_version": LAYOUT_VERSION,
                # Where the chart puts the file, which is the half of this the
                # process does not choose. The schema below is the half it does.
                "path": chart_env(facts).get("FATHOM_EVAL_DB_PATH"),
                **storage,
            },
        )
        written.append(out / "storage.json")
        # The DDL goes out as .sql rather than a JSON string so it can be read,
        # diffed and piped into sqlite3 without unescaping it first.
        (out / "schema.sql").write_text(ddl.rstrip() + "\n")
        written.append(out / "schema.sql")
        dump(
            out / "fixture-namespaces.json",
            {
                "layout_version": LAYOUT_VERSION,
                "namespaces": namespaces,
                "note": "Read out of the packaged fixtures by running the image. "
                "Two-file agreement: these must appear in eval/helm -> "
                "rbac.fixtureNamespaces or the harness cannot stage them.",
            },
        )
        written.append(out / "fixture-namespaces.json")

    return written


def write_component(component: str, root: Path, offline: bool = False) -> list[Path]:
    facts = chart_facts(component)
    cross_check(component, facts)

    out = root / component
    written = [out / "service.json"]
    dump(out / "service.json", service_document(component, facts))

    # service.json is rendered by `helm template` and needs nothing running, so it is
    # current in either mode. The interface itself is scraped from the component, and
    # without a daemon that is only possible for the two FastAPI services. What cannot
    # be regenerated is left alone rather than deleted: a stale file that says so on
    # stderr is recoverable, and an absent one looks like a removed interface.
    if not offline:
        written += scrape_interface(component, facts, out, build_image(component), None)
    elif component not in LOCAL_MODULE:
        print(
            f"contracts: {component}: interface left as it was -- it is scraped from a "
            f"running binary and that needs Docker.",
            file=sys.stderr,
        )
    elif (python := venv_python(component)) is None:
        print(
            f"contracts: {component}: interface left as it was -- no venv to serve it "
            f"from. Build one with `make -C {component} venv`.",
            file=sys.stderr,
        )
    else:
        written += scrape_interface(component, facts, out, None, python)

    # Created once, then never touched. It holds the retry, idempotency and ordering
    # semantics a spec cannot express -- the part that causes incidents when it is
    # missing -- and overwriting it would silently destroy the most valuable content
    # in the directory. For front and eval it also carries the route list, which is
    # the only place those two are described at all.
    notes = out / "integration-notes.md"
    if not notes.exists():
        notes.write_text(INTEGRATION_STUB.format(service=RELEASE[component]))
        written.append(notes)

    return written


def main() -> int:
    parser = argparse.ArgumentParser(description="Regenerate contracts/ by running the components.")
    parser.add_argument("components", nargs="*", default=None, help="components to regenerate (default: all)")
    parser.add_argument("--out", default=None, help="write here instead of contracts/ (used by contracts-check)")
    parser.add_argument(
        "--allow-offline",
        action="store_true",
        help="degrade instead of failing when Docker is unavailable (used by `make contracts`)",
    )
    args = parser.parse_args()

    wanted = args.components or COMPONENTS
    unknown = [c for c in wanted if c not in COMPONENTS]
    if unknown:
        raise SystemExit(f"contracts: unknown component(s): {', '.join(unknown)}")

    offline = subprocess.run(
        ["docker", "info"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    ).returncode != 0
    if offline and not args.allow_offline:
        raise SystemExit(
            "contracts: Docker is not available, and this writer runs the components "
            "rather than parsing them. `make contracts` degrades to the two FastAPI "
            "services and regenerates what it can; a check cannot, because a gate that "
            "silently checks less than it claims is worse than one that refuses. Start "
            "Docker and try again."
        )
    if offline:
        print(
            "contracts: Docker is not available. FastAPI publishes /openapi.json itself, "
            "so agent and eval are served from their own venvs and scraped the same way; "
            "anything that needs a running binary is left as it is and named below.",
            file=sys.stderr,
        )

    root = Path(args.out).resolve() if args.out else CONTRACTS
    root.mkdir(parents=True, exist_ok=True)

    for component in [c for c in COMPONENTS if c in wanted]:
        try:
            written = write_component(component, root, offline)
        except Drift as err:
            print(f"contracts: {err}", file=sys.stderr)
            return 1
        rel = [str(p.relative_to(root.parent)) for p in written]
        print(f"contracts: {component} -> {', '.join(sorted(rel))}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    os.chdir(REPO)
    sys.exit(main())
