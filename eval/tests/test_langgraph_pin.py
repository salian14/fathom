"""Tripwire for the iteration-ceiling sentinel.

`grading.CEILING_SENTINEL` is a literal copied out of LangGraph's prebuilt ReAct agent,
where it is an internal implementation detail rather than a public contract. The agent
turns it into an ordinary HTTP 200, so if LangGraph ever rewords it, every run that
exhausted its turn budget would silently be classified as a clean pass -- a regression
that makes the numbers *better* and is therefore unlikely to be noticed.

Two layers guard it: this test pins the langgraph version the string was verified
against, and a second test asserts the string still appears in the installed library
when it happens to be importable (it is a dependency of the agent, not of the harness,
so in this component's venv it usually is not).
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

from fathom_eval.grading import CEILING_SENTINEL

# The version whose source was read when CEILING_SENTINEL was written. Bumping langgraph
# in the agent must come with re-reading chat_agent_executor.py and updating this.
LANGGRAPH_VERIFIED = "1.2.11"

AGENT_LOCK = Path(__file__).resolve().parents[2] / "agent" / "requirements.lock"


def test_langgraph_version_the_sentinel_was_verified_against_has_not_moved():
    if not AGENT_LOCK.exists():
        pytest.skip("agent/requirements.lock not present")
    match = re.search(r"^langgraph==(\S+)", AGENT_LOCK.read_text(), re.MULTILINE)
    assert match, "langgraph is not pinned in agent/requirements.lock"
    assert match.group(1) == LANGGRAPH_VERIFIED, (
        f"langgraph moved to {match.group(1)}; re-read "
        "langgraph/prebuilt/chat_agent_executor.py and confirm the ceiling message is "
        f"still {CEILING_SENTINEL!r}, then update LANGGRAPH_VERIFIED"
    )


def test_sentinel_matches_the_installed_langgraph_when_available():
    chat_agent_executor = pytest.importorskip(
        "langgraph.prebuilt.chat_agent_executor",
        reason="langgraph is the agent's dependency, not the harness's",
    )
    source = Path(chat_agent_executor.__file__).read_text()
    assert CEILING_SENTINEL in source
