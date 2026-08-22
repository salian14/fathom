"""Readiness-predicate evaluation and the namespace guard.

These are pure functions over an API response, so they are testable without a cluster --
which matters, because the alternative is discovering a broken predicate at 01:00 when a
fixture waits out its whole timeout and the run reports it as unusable.
"""

from __future__ import annotations

import pytest

from fathom_eval.cluster import _compare, _jsonpath, _tokenise, guard
from fathom_eval.fixtures import load_all

# Shaped exactly as the API server returns it, covering every field the fixtures assert.
POD = {
    "status": {
        "phase": "Pending",
        "conditions": [
            {"type": "Initialized", "status": "True"},
            {"type": "PodScheduled", "status": "False", "reason": "Unschedulable"},
        ],
        "containerStatuses": [
            {
                "ready": False,
                "restartCount": 4,
                "state": {"waiting": {"reason": "CrashLoopBackOff"}},
                "lastState": {"terminated": {"reason": "OOMKilled", "exitCode": 137}},
            }
        ],
    }
}


def test_tokenise_does_not_split_inside_brackets():
    # The bug this guards: a filter expression contains a dot of its own, so a naive
    # split tears it apart and every lookup silently returns None -- which readiness
    # code reads as "not ready yet" and waits out the entire timeout.
    assert _tokenise(".status.conditions[?(@.type=='PodScheduled')].reason") == [
        "status",
        "conditions[?(@.type=='PodScheduled')]",
        "reason",
    ]


@pytest.mark.parametrize(
    ("path", "expected"),
    [
        (".status.phase", "Pending"),
        (".status.containerStatuses[0].restartCount", 4),
        (".status.containerStatuses[0].ready", False),
        (".status.containerStatuses[0].state.waiting.reason", "CrashLoopBackOff"),
        (".status.containerStatuses[0].lastState.terminated.reason", "OOMKilled"),
        (".status.containerStatuses[0].lastState.terminated.exitCode", 137),
        (".status.conditions[?(@.type=='PodScheduled')].reason", "Unschedulable"),
        # Absent rather than an exception: a predicate on a field that does not exist yet
        # simply is not satisfied.
        (".status.conditions[?(@.type=='Missing')].reason", None),
        (".status.containerStatuses[9].restartCount", None),
        (".status.nothing.deeper", None),
        (".status.phase.deeper", None),
    ],
)
def test_jsonpath(path, expected):
    assert _jsonpath(POD, path) == expected


@pytest.mark.parametrize(
    ("actual", "op", "expected", "want"),
    [
        (4, "gte", 2, True),
        (1, "gte", 2, False),
        (None, "gte", 2, False),
        ("CrashLoopBackOff", "in", ["CrashLoopBackOff", "ErrImagePull"], True),
        ("Running", "in", ["CrashLoopBackOff"], False),
        (True, "eq", True, True),
        ("Running", "eq", "Pending", False),
        ([1], "nonempty", None, True),
        ([], "nonempty", None, False),
    ],
)
def test_compare(actual, op, expected, want):
    assert _compare(actual, op, expected) is want


def test_unknown_operator_is_loud():
    with pytest.raises(ValueError, match="unknown readiness operator"):
        _compare(1, "roughly", 1)


@pytest.mark.parametrize("namespace", ["kube-system", "fathom", "default", "", "fathom-eval"])
def test_guard_refuses_namespaces_outside_the_prefix(namespace):
    # RBAC cannot name-scope `create` on namespaces, so this is the second limit and it
    # has to hold for everything that is not a fixture namespace -- including the
    # harness's own namespace, which it must never delete.
    with pytest.raises(ValueError, match="refusing to touch namespace"):
        guard(namespace)


def test_guard_allows_fixture_namespaces():
    for fixture in load_all():
        assert guard(fixture.namespace) == fixture.namespace


def test_every_fixture_predicate_uses_a_supported_operator_and_path():
    """A typo in a fixture's readiness predicate should fail here, not at 01:00."""
    for fixture in load_all():
        for predicate in [*fixture.ready_all, *fixture.ready_never]:
            if predicate.get("kind") == "promql":
                assert predicate.get("query"), f"{fixture.name}: promql predicate with no query"
                assert predicate.get("op") == "nonempty"
                continue
            assert predicate.get("jsonpath"), f"{fixture.name}: predicate with no jsonpath"
            assert predicate.get("op") in ("eq", "in", "gte"), fixture.name
            # It must at least resolve against a real pod shape without raising.
            _jsonpath(POD, predicate["jsonpath"])
