"""The run loop.

Strictly serial, and that is a correctness decision rather than a simplification:

- The host Ollama pins `OLLAMA_NUM_PARALLEL=1` and `OLLAMA_MAX_LOADED_MODELS=1`, so
  concurrent /chat calls do not overlap -- the second one queues. It would still burn
  its own 180s budget waiting, so parallelism converts harmless queuing into spurious
  504s that look like agent failures. We would be measuring our own scheduler.
- With one fixture live at a time, a question about "the cluster" has one true answer.
  The system prompt actively encourages listing across namespaces, so ten simultaneous
  broken workloads would make several fixtures' questions legitimately ambiguous.
- It also makes cross-fixture contamination almost impossible by construction, which is
  worth more than the detector that looks for it.

The `healthy` control runs first and again at the end. If the two disagree, the run
drifted and every number between them is suspect -- ten extra turns for the cheapest
trust instrument available.
"""

from __future__ import annotations

import logging
import secrets
import signal
import socket
import subprocess
import time

from . import __version__, grading, store
from .agentclient import AgentClient
from .cluster import Cluster, NotReady, WrongState, connect
from .config import Config
from .fixtures import Fixture

log = logging.getLogger("fathom_eval.runner")

# Settle window after an exclusive fixture, so the next one is not measured on a node
# that is still recovering from it.
EXCLUSIVE_SETTLE = 60.0


class Deadline:
    """A soft deadline the harness enforces on itself.

    Set below the Job's activeDeadlineSeconds so we stop and tear down cleanly rather
    than being SIGKILLed mid-fixture, which would leak a namespace full of broken pods
    into a cluster the next run has to clean up.
    """

    def __init__(self, seconds: float) -> None:
        self._end = time.monotonic() + seconds if seconds else 0.0
        self._stop = False
        signal.signal(signal.SIGTERM, self._signal)
        signal.signal(signal.SIGINT, self._signal)

    def _signal(self, *_):
        log.warning("received termination signal -- finishing current turn, then tearing down")
        self._stop = True

    @property
    def expired(self) -> bool:
        return self._stop or (bool(self._end) and time.monotonic() >= self._end)


def git_sha() -> str:
    try:
        return subprocess.run(
            ["git", "rev-parse", "--short", "HEAD"],
            capture_output=True,
            text=True,
            timeout=5,
            check=True,
        ).stdout.strip()
    except (subprocess.SubprocessError, OSError):
        return ""


def run(cfg: Config, fixtures: list[Fixture], deadline_seconds: float = 0.0) -> int:
    """Execute a full evaluation. Returns a process exit code."""
    deadline = Deadline(deadline_seconds)
    cluster = Cluster(api=connect(cfg.kubeconfig), prometheus_url=cfg.prometheus_url)

    # Every other fixture's object names and namespaces, so a question can flag names
    # and namespaces that cannot legitimately have been observed.
    names_by_fixture = {f.name: list(f.object_names) for f in fixtures}
    namespaces_by_fixture = {f.name: f.namespace for f in fixtures}

    with store.opened(cfg.db_path) as conn, AgentClient(cfg.chat_url, cfg.request_timeout) as agent:
        if swept := store.sweep_abandoned(conn):
            log.warning("marked %d unfinished run(s) as abandoned", swept)

        if leftovers := cluster.sweep():
            log.warning("swept %d leftover fixture namespace(s): %s", len(leftovers), leftovers)

        if problem := agent.ready():
            log.error("agent is not ready: %s", problem)
            return 1

        run_id = store.start_run(
            conn,
            git_sha=git_sha(),
            harness_version=__version__,
            agent_url=cfg.agent_url,
            agent_model=agent.version(),
            iterations=cfg.iterations,
            host=socket.gethostname(),
        )
        log.info(
            "run %d started: %d fixtures, %d iterations each", run_id, len(fixtures), cfg.iterations
        )

        # One unrecorded turn so the cost of loading the model into VRAM is not charged
        # to the first fixture's elapsed_seconds.
        log.info("warming the model")
        agent.ask("Reply with the single word: ready.")

        status = "ok"
        try:
            for fixture in fixtures:
                if deadline.expired:
                    status = "deadline"
                    log.warning("deadline reached -- stopping before %s", fixture.name)
                    break
                _run_fixture(
                    cfg,
                    cluster,
                    agent,
                    conn,
                    run_id,
                    fixture,
                    names_by_fixture,
                    namespaces_by_fixture,
                    deadline,
                )
                if fixture.exclusive:
                    log.info(
                        "settling %gs after exclusive fixture %s", EXCLUSIVE_SETTLE, fixture.name
                    )
                    time.sleep(EXCLUSIVE_SETTLE)
        except Exception as err:  # noqa: BLE001 - recorded, then re-raised context
            log.exception("run failed")
            store.finish_run(conn, run_id, "failed", f"{type(err).__name__}: {err}")
            return 1

        store.finish_run(conn, run_id, status)
        log.info("run %d finished: %s", run_id, status)
        return 0 if status == "ok" else 2


def _run_fixture(
    cfg,
    cluster,
    agent,
    conn,
    run_id,
    fixture,
    names_by_fixture,
    namespaces_by_fixture,
    deadline,
) -> None:
    namespace = fixture.namespace
    log.info("fixture %s -> %s (%s)", fixture.name, namespace, fixture.isolation)

    # An exclusive fixture must genuinely be alone. Serial execution should already
    # guarantee it; asserting turns a silent assumption into a loud failure.
    if fixture.exclusive:
        others = [ns for ns in cluster.fixture_namespaces() if ns != namespace]
        if others:
            raise RuntimeError(
                f"{fixture.name} is exclusive but these fixture namespaces exist: {others}"
            )

    manifests, canaries = _plant_canaries(fixture)

    cluster.create_namespace(namespace)
    try:
        cluster.apply(namespace, manifests)
        try:
            cluster.wait_ready(fixture)
        except (WrongState, NotReady) as err:
            # Recorded as a fixture failure with zero iterations. Asking questions about
            # a fixture that never reached its intended state is how a database fills up
            # with confident answers to the wrong question.
            log.error("fixture %s unusable: %s", fixture.name, err)
            store.record_result(
                conn,
                run_id,
                {
                    "fixture": fixture.name,
                    "isolation": fixture.isolation,
                    "namespace": namespace,
                    "question_id": "-",
                    "iteration": 0,
                    "outcome": "fixture_error",
                    "error": str(err),
                    "steps": [],
                },
                [grading.Check("termination", "fixture_ready", False, str(err))],
            )
            return

        forbidden_names = [
            n for other, names in names_by_fixture.items() if other != fixture.name for n in names
        ]
        other_namespaces = [
            ns for other, ns in namespaces_by_fixture.items() if other != fixture.name
        ]

        for question in fixture.questions:
            text = question.text.replace("{{ namespace }}", namespace)
            expect = dict(question.expect)
            expect["forbidden_names"] = forbidden_names
            expect["canaries"] = canaries

            for i in range(1, cfg.iterations + 1):
                if deadline.expired:
                    log.warning(
                        "deadline reached mid-fixture -- stopping after %d iterations", i - 1
                    )
                    return
                _one_turn(
                    agent,
                    conn,
                    run_id,
                    fixture,
                    question,
                    text,
                    expect,
                    namespace,
                    other_namespaces,
                    i,
                    cfg,
                )
    finally:
        if cfg.keep_namespaces:
            log.warning("keeping namespace %s (FATHOM_EVAL_KEEP_NAMESPACES)", namespace)
        else:
            cluster.delete_namespace(namespace, timeout=cfg.teardown_timeout)


def _one_turn(
    agent,
    conn,
    run_id,
    fixture,
    question,
    text,
    expect,
    namespace,
    other_namespaces,
    iteration,
    cfg,
) -> None:
    started = time.monotonic()
    status, body, error = agent.ask(text)
    outcome = grading.classify_outcome(status, body, error)

    checks = grading.grade(
        outcome=outcome,
        body=body,
        expect=expect,
        namespace=namespace,
        resolved_names=list(expect.get("names") or []),
        owner_names=[],
        other_namespaces=other_namespaces,
        max_iterations=cfg.max_iterations,
    )

    store.record_result(
        conn,
        run_id,
        {
            "fixture": fixture.name,
            "isolation": fixture.isolation,
            "namespace": namespace,
            "question_id": question.id,
            "question_text": text,
            "iteration": iteration,
            "http_status": status,
            "outcome": outcome,
            "answer": (body or {}).get("answer"),
            "steps": (body or {}).get("steps") or [],
            "tool_calls": len((body or {}).get("steps") or []),
            "elapsed_seconds": (body or {}).get(
                "elapsed_seconds", round(time.monotonic() - started, 2)
            ),
            "error": error or None,
        },
        checks,
    )

    failed = [c.name for c in checks if not c.passed]
    log.info(
        "  %s/%s #%d %s%s",
        fixture.name,
        question.id,
        iteration,
        outcome,
        f" failed={failed}" if failed else " pass",
    )


def _plant_canaries(fixture: Fixture) -> tuple[list[dict], list[str]]:
    """Substitute a freshly generated secret value into the fixture's manifests.

    Generated per run rather than checked in, so the leak check cannot be satisfied by a
    value the model could have memorised from anywhere else.
    """
    token = "FATHOM-CANARY-" + secrets.token_hex(16)
    manifests = [_substitute(m, token) for m in fixture.manifests]
    planted = manifests != fixture.manifests
    return manifests, [token] if planted else []


def _substitute(node, token: str):
    if isinstance(node, dict):
        return {k: _substitute(v, token) for k, v in node.items()}
    if isinstance(node, list):
        return [_substitute(v, token) for v in node]
    if isinstance(node, str) and "FATHOM-CANARY-PLACEHOLDER" in node:
        return node.replace("FATHOM-CANARY-PLACEHOLDER", token)
    return node
