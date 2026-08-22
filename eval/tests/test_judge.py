"""The judge's pure parts: rubric versioning, prompt assembly, verdict parsing.

Nothing here talks to the judge model. What is worth testing is the machinery around
it -- a rubric edit must invalidate exactly the work it invalidated, a prompt must be
reproducible, and a reply that cannot be parsed must never be coerced into a score.
"""

from __future__ import annotations

import pytest

from fathom_eval.judge import build_prompt, parse_verdict, rubric

A_FIX = {
    "action_required": True,
    "summary": "Raise the memory limit on oom-app.",
    "target_name": "oom-app",
    "confidence": "high",
    "commands": ["kubectl -n ns set resources pod/oom-app --limits=memory=256Mi"],
}


def test_the_rubric_grades_remediation_first():
    text, version = rubric()

    assert version.startswith("answer_v2-")
    assert "remediation_score" in text
    # The pass bar is both dimensions: a good diagnosis with no usable fix is the
    # exact failure this whole change exists to catch.
    assert "remediation_score >= 4" in text


def test_the_prompt_carries_the_proposed_fix():
    prompt = build_prompt("RUBRIC", "ground truth", "why?", "an answer", [], A_FIX)

    assert "--- THE ASSISTANT'S PROPOSED REMEDIATION ---" in prompt
    assert "Raise the memory limit on oom-app." in prompt
    assert "kubectl -n ns set resources" in prompt


def test_a_missing_fix_is_stated_rather_than_omitted():
    # An absent section would read to the judge as a prompt-assembly bug. "Proposed
    # nothing" is the thing being graded, so it has to be said out loud.
    prompt = build_prompt("RUBRIC", "gt", "why?", "an answer", [], None)

    assert "proposed no remediation" in prompt


def test_the_prompt_is_deterministic():
    # Re-judging the same result must be the same experiment, not a new one.
    args = ("RUBRIC", "gt", "why?", "an answer", [{"tool": "k8s_get", "args": {"b": 1, "a": 2}}])

    assert build_prompt(*args, A_FIX) == build_prompt(*args, dict(reversed(list(A_FIX.items()))))


def test_a_verdict_carries_both_scores():
    parsed = parse_verdict('{"verdict":"pass","score":5,"remediation_score":4,"rationale":"ok"}')

    assert parsed == {
        "verdict": "pass",
        "score": 5,
        "remediation_score": 4,
        "rationale": "ok",
    }


def test_a_verdict_is_found_inside_surrounding_prose():
    parsed = parse_verdict(
        "Here you go:\n"
        '{"verdict":"fail","score":2,"remediation_score":1,"rationale":"no"}\n'
        "Hope that helps."
    )

    assert parsed["remediation_score"] == 1


@pytest.mark.parametrize(
    "raw",
    [
        # A judge that defaulted a missing remediation_score to anything would invent
        # a number nobody produced -- and under the v2 rubric that number decides the
        # headline metric.
        '{"verdict":"pass","score":5,"rationale":"ok"}',
        '{"verdict":"pass","score":5,"remediation_score":0,"rationale":"ok"}',
        '{"verdict":"pass","score":5,"remediation_score":9,"rationale":"ok"}',
    ],
)
def test_a_missing_or_out_of_range_remediation_score_is_rejected(raw):
    with pytest.raises(ValueError, match="remediation_score"):
        parse_verdict(raw)
