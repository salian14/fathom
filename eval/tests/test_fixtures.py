"""Every packaged fixture must be well-formed.

This is the cheapest guard in the component: a malformed fixture would otherwise fail at
01:00 inside a CronJob, after the namespace has already been created.
"""

from __future__ import annotations

import pytest

from fathom_eval.config import NAMESPACE_PREFIX
from fathom_eval.fixtures import Fixture, load_all, parse

FIXTURES = load_all()
EXPECTED = {
    "healthy",
    "crashloop",
    "imagepull",
    "pending",
    "missing-configmap",
    "service-no-endpoints",
    "secret-bait",
    "prom-restarts",
    "oomkill",
    "cpu-throttle",
}


def test_the_expected_suite_is_present():
    assert {f.name for f in FIXTURES} == EXPECTED


def test_fixtures_are_ordered_with_the_control_first_and_noisy_ones_last():
    order = [f.name for f in FIXTURES]
    assert order[0] == "healthy", "the negative control must run first"
    exclusive = [f.name for f in FIXTURES if f.exclusive]
    assert exclusive == ["oomkill", "cpu-throttle"]
    # The noisy neighbours run after everything that could be perturbed by them.
    assert order.index("oomkill") > order.index("prom-restarts")


@pytest.mark.parametrize("fixture", FIXTURES, ids=lambda f: f.name)
def test_fixture_is_well_formed(fixture: Fixture):
    assert fixture.namespace.startswith(NAMESPACE_PREFIX)
    assert fixture.description and len(fixture.description) > 40, "ground truth feeds the judge"
    assert fixture.manifests
    assert fixture.questions
    assert fixture.ready_timeout > 0
    for question in fixture.questions:
        assert question.expect, "a question with no expectations grades nothing"
        # The namespace is generated, so a question can only point at it by template.
        assert "{{ namespace }}" in question.text


@pytest.mark.parametrize("fixture", FIXTURES, ids=lambda f: f.name)
def test_manifests_declare_no_namespace_of_their_own(fixture: Fixture):
    # The runner injects it. A hardcoded namespace would escape the fathom-eval- guard.
    for manifest in fixture.manifests:
        assert "namespace" not in (manifest.get("metadata") or {})


@pytest.mark.parametrize("fixture", FIXTURES, ids=lambda f: f.name)
def test_object_names_match_what_the_manifests_create(fixture: Fixture):
    created = {
        m["metadata"]["name"] for m in fixture.manifests if m.get("metadata", {}).get("name")
    }
    assert set(fixture.object_names) <= created, (
        f"{fixture.name} declares objectNames that no manifest creates: "
        f"{set(fixture.object_names) - created}"
    )


@pytest.mark.parametrize("fixture", FIXTURES, ids=lambda f: f.name)
def test_expected_names_are_real_objects(fixture: Fixture):
    for question in fixture.questions:
        for name in question.expect.get("names") or []:
            assert name in fixture.object_names, (
                f"{fixture.name}/{question.id} expects the answer to name {name!r}, "
                "which this fixture does not create"
            )


def test_every_broken_fixture_guards_against_the_wrong_failure():
    # Without a `never` clause the harness will happily ask ten questions about a
    # fixture that broke in an unintended way -- e.g. an image that would not pull.
    for fixture in FIXTURES:
        if fixture.name in ("secret-bait", "pending"):
            continue  # no pod that can fail to start, or nothing to mis-fail into
        assert fixture.ready_never, f"{fixture.name} has no 'never' predicates"


def test_selecting_an_unknown_fixture_is_an_error():
    with pytest.raises(ValueError, match="no such fixture"):
        load_all(("does-not-exist",))


def test_selection_filters():
    assert [f.name for f in load_all(("crashloop",))] == ["crashloop"]


def test_parse_rejects_a_fixture_missing_required_keys():
    with pytest.raises(ValueError, match="missing required key"):
        parse("name: x\ndescription: y\n")


def test_parse_rejects_an_unknown_isolation_class():
    with pytest.raises(ValueError, match="isolation must be"):
        parse(
            "name: x\ndescription: y\nisolation: sometimes\n"
            "manifests: [{metadata: {name: a}}]\nquestions: [{id: q, text: t}]\n"
        )


def test_chart_rbac_lists_exactly_the_fixture_namespaces():
    """helm values rbac.fixtureNamespaces must match the packaged suite.

    `delete` on namespaces is the one half of the harness's grant that RBAC *can*
    name-scope, and it is scoped by this list. A fixture added here but not there fails
    at teardown with a Forbidden, half way through a nightly run; one left there after a
    fixture is removed is a grant nothing needs.
    """
    from pathlib import Path

    import yaml

    values = Path(__file__).resolve().parents[1] / "helm" / "values.yaml"
    declared = set(yaml.safe_load(values.read_text())["rbac"]["fixtureNamespaces"])
    assert declared == {f.namespace for f in FIXTURES}
