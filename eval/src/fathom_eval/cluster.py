"""Staging and tearing down fixtures against the real cluster.

This is the only module in the harness that writes to Kubernetes, and it is the reason
the eval ServiceAccount exists at all -- separate from fathom-mcp's, which is read-only
and must stay that way.

Three behaviours here are load-bearing for trustworthy results:

**The namespace guard.** RBAC cannot restrict `create` on namespaces by name
(`resourceNames` has nothing to match before the object exists), so the second limit
lives here in code: nothing is created or deleted unless its name starts with
`fathom-eval-`. That is the same belt-and-braces shape fathom-mcp already uses, where
the server checks allowedNamespaces itself rather than trusting RBAC alone.

**Teardown waits for the namespace to actually be gone.** `delete namespace` returns
immediately and leaves the object in Terminating. Returning at that point means the next
run's create races a namespace that still exists, which fails in a way that looks like a
harness bug rather than a timing one.

**Readiness has three outcomes, not two.** `ready`, `not_yet`, and `wrong_state` -- the
last when a `never` predicate matches, meaning the fixture broke differently than its
expectations describe. Grading ten answers about a fixture that failed the wrong way is
the easiest route to a database full of confident nonsense, so that aborts the fixture.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass

import httpx
from kubernetes import client, config
from kubernetes import utils as k8s_utils

from .config import NAMESPACE_PREFIX

log = logging.getLogger("fathom_eval.cluster")

# Namespaces the harness owns carry this, so the startup sweep can find leftovers from a
# run that was killed before it could tidy up.
OWNER_LABEL = "fathom.eval/fixture"


class WrongState(RuntimeError):
    """A `never` predicate matched: the fixture is broken in an unintended way."""


class NotReady(RuntimeError):
    """The fixture never reached its declared state within its timeout."""


def guard(namespace: str) -> str:
    if not namespace.startswith(NAMESPACE_PREFIX):
        raise ValueError(
            f"refusing to touch namespace {namespace!r}: the harness only manages "
            f"{NAMESPACE_PREFIX}* namespaces"
        )
    return namespace


def connect(kubeconfig: str = "") -> client.ApiClient:
    """In-cluster credentials first, kubeconfig as the local-development fallback.

    Same precedence the rest of the project uses: the deployed path is the primary one,
    and the kubeconfig branch exists so `make run-local` works from a workstation.
    """
    if kubeconfig:
        config.load_kube_config(config_file=kubeconfig)
    else:
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
    return client.ApiClient()


@dataclass
class Cluster:
    api: client.ApiClient
    prometheus_url: str = ""

    @property
    def core(self) -> client.CoreV1Api:
        return client.CoreV1Api(self.api)

    # -- namespace lifecycle --------------------------------------------------

    def create_namespace(self, namespace: str) -> None:
        guard(namespace)
        body = client.V1Namespace(
            metadata=client.V1ObjectMeta(
                name=namespace,
                labels={
                    OWNER_LABEL: namespace.removeprefix(NAMESPACE_PREFIX),
                    "app.kubernetes.io/managed-by": "fathom-eval",
                },
            )
        )
        self.core.create_namespace(body)

    def delete_namespace(self, namespace: str, timeout: float = 180.0) -> None:
        """Delete and wait for it to be gone. A timeout here is fatal to the run."""
        guard(namespace)
        try:
            self.core.delete_namespace(namespace)
        except client.ApiException as err:
            if err.status != 404:
                raise

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                self.core.read_namespace(namespace)
            except client.ApiException as err:
                if err.status == 404:
                    return
                raise
            time.sleep(2.0)
        raise TimeoutError(
            f"namespace {namespace} was still Terminating after {timeout:g}s; "
            "the next run cannot safely recreate it"
        )

    def sweep(self, timeout: float = 300.0) -> list[str]:
        """Delete every fixture namespace left over from an earlier run.

        A leaked namespace is not merely untidy: it is a second broken workload in the
        cluster, so it contaminates any question the agent answers with
        allNamespaces=true and inflates what lands in the model's context.
        """
        found = self.core.list_namespace(label_selector=OWNER_LABEL).items
        names = [ns.metadata.name for ns in found]
        for name in names:
            log.warning("sweeping leftover namespace %s", name)
            self.delete_namespace(name, timeout=timeout)
        return names

    def fixture_namespaces(self) -> list[str]:
        return [
            ns.metadata.name for ns in self.core.list_namespace(label_selector=OWNER_LABEL).items
        ]

    # -- manifests ------------------------------------------------------------

    def apply(self, namespace: str, manifests: list[dict]) -> None:
        """Create the fixture's objects, in declared order.

        `create`, not apply: a leftover object should be a loud conflict, not a silent
        merge into whatever was already there.
        """
        guard(namespace)
        for manifest in manifests:
            body = dict(manifest)
            body.setdefault("metadata", {})
            body["metadata"] = dict(body["metadata"], namespace=namespace)
            k8s_utils.create_from_dict(self.api, body, namespace=namespace)

    # -- readiness ------------------------------------------------------------

    def wait_ready(self, fixture, poll: float = 5.0) -> None:
        """Block until every `all` predicate holds, or fail.

        Raises WrongState the moment a `never` predicate matches -- that is a fixture
        bug and must not be graded -- and NotReady if the deadline passes.
        """
        deadline = time.monotonic() + fixture.ready_timeout
        last = ""
        while time.monotonic() < deadline:
            pod = self._first_pod(fixture.namespace)

            for predicate in fixture.ready_never:
                if self._holds(predicate, pod, fixture.namespace):
                    raise WrongState(
                        f"{fixture.name}: 'never' predicate matched ({predicate}); the "
                        f"fixture is broken in a way its expectations do not describe"
                    )

            unmet = [p for p in fixture.ready_all if not self._holds(p, pod, fixture.namespace)]
            if not unmet:
                return
            last = str(unmet[0])
            time.sleep(poll)

        raise NotReady(
            f"{fixture.name}: not ready after {fixture.ready_timeout:g}s; "
            f"first unmet predicate: {last}"
        )

    def _first_pod(self, namespace: str):
        pods = self.core.list_namespaced_pod(namespace).items
        return pods[0] if pods else None

    def _holds(self, predicate: dict, pod, namespace: str) -> bool:
        if predicate.get("kind") == "promql":
            return self._promql_nonempty(predicate["query"].replace("{{ namespace }}", namespace))

        if pod is None:
            return False
        actual = _jsonpath(self.api.sanitize_for_serialization(pod), predicate["jsonpath"])
        return _compare(actual, predicate.get("op", "eq"), predicate.get("value"))

    def _promql_nonempty(self, query: str) -> bool:
        """True when an instant query returns at least one sample.

        Fixtures whose symptom is a metric need this rather than a Kubernetes assertion:
        kube-state-metrics is scraped every 30s, so a pod can be in the right state
        while Prometheus still knows nothing about it -- and the agent would then
        correctly answer "no data" and be graded wrong for it.
        """
        if not self.prometheus_url:
            return False
        try:
            response = httpx.get(
                self.prometheus_url.rstrip("/") + "/api/v1/query",
                params={"query": query},
                timeout=15.0,
            )
            response.raise_for_status()
            return bool(response.json().get("data", {}).get("result"))
        except (httpx.HTTPError, ValueError) as err:
            log.warning("prometheus readiness query failed: %s", err)
            return False


def _tokenise(path: str) -> list[str]:
    """Split a path on dots that are NOT inside brackets.

    The naive `path.split(".")` is wrong and was: a filter such as
    `conditions[?(@.type=='PodScheduled')]` contains a dot of its own, so splitting
    blindly tears the expression into fragments and the lookup silently returns None --
    which readiness code reads as "not ready yet" and waits out the whole timeout.
    """
    tokens, current, depth = [], [], 0
    for char in path.lstrip("."):
        if char == "[":
            depth += 1
        elif char == "]":
            depth -= 1
        if char == "." and depth == 0:
            tokens.append("".join(current))
            current = []
            continue
        current.append(char)
    if current:
        tokens.append("".join(current))
    return [t for t in tokens if t]


def _jsonpath(obj, path: str):
    """A deliberately small subset of JSONPath: dotted keys, [n], and [?(@.k=='v')].

    Enough for pod status, and small enough to read in full. A general implementation
    would be another dependency and another thing that can surprise you at 01:00.
    """
    current = obj
    for part in _tokenise(path):
        if current is None:
            return None
        # A filter such as conditions[?(@.type=='PodScheduled')]
        if "[?(@." in part:
            name, _, expr = part.partition("[?(@.")
            key, _, wanted = expr.rstrip(")]").partition("==")
            wanted = wanted.strip("'\"")
            items = current.get(name) if name else current
            if not isinstance(items, list):
                return None
            current = next(
                (i for i in items if isinstance(i, dict) and str(i.get(key)) == wanted), None
            )
            continue
        if "[" in part and part.endswith("]"):
            name, _, index = part.partition("[")
            items = current.get(name) if name else current
            if not isinstance(items, list):
                return None
            idx = int(index.rstrip("]"))
            current = items[idx] if idx < len(items) else None
            continue
        current = current.get(part) if isinstance(current, dict) else None
    return current


def _compare(actual, op: str, expected) -> bool:
    if op == "eq":
        return actual == expected
    if op == "in":
        return actual in (expected or [])
    if op == "gte":
        return actual is not None and float(actual) >= float(expected)
    if op == "nonempty":
        return bool(actual)
    raise ValueError(f"unknown readiness operator {op!r}")
