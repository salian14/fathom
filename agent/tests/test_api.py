"""Smoke tests for the HTTP surface and the tool-result bounding.

The graph is stubbed: these check the wiring around the model, not the model. There
is no fake Ollama and no fake MCP server -- exercising those is what
`make run-local` against the real cluster is for.
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage

from fathom_agent import api, mcp_client
from fathom_agent.agent import build_messages, summarise
from fathom_agent.config import load


@pytest.fixture
def client(monkeypatch):
    app = api.create_app(load())
    with TestClient(app) as c:
        yield c


def test_healthz_is_process_only(client):
    response = client.get("/healthz")

    assert response.status_code == 200
    assert response.text.strip() == "ok"


def test_readyz_reports_which_dependency_failed(client):
    # Nothing is running on the default endpoints in a test environment, so this
    # exercises the real failure path rather than a mock of it.
    response = client.get("/readyz")

    assert response.status_code == 503
    assert "ollama unreachable" in response.text


def test_chat_rejects_an_empty_question(client):
    assert client.post("/chat", json={"question": ""}).status_code == 422


def test_chat_rejects_an_unknown_history_role(client):
    response = client.post(
        "/chat",
        json={"question": "hi", "history": [{"role": "system", "content": "x"}]},
    )

    assert response.status_code == 400
    assert "history role" in response.json()["error"]


def test_chat_returns_the_answer_and_the_tool_trace(client, monkeypatch):
    class FakeGraph:
        async def ainvoke(self, state, config):
            assert config["recursion_limit"] == 2 * 8 + 1
            return {
                "messages": [
                    AIMessage(
                        "",
                        tool_calls=[{"name": "k8s_get", "args": {"resource": "pods"}, "id": "1"}],
                    ),
                    ToolMessage("NAME  READY\nfathom-mcp  1/1", tool_call_id="1"),
                    AIMessage("One pod is running: fathom-mcp."),
                ]
            }

    monkeypatch.setattr(api, "build_graph", lambda cfg, model, tools: FakeGraph())
    monkeypatch.setattr(api, "tool_session", _fake_session)

    body = client.post("/chat", json={"question": "what is running?"}).json()

    assert body["answer"] == "One pod is running: fathom-mcp."
    assert body["steps"][0]["tool"] == "k8s_get"
    assert body["steps"][0]["args"] == {"resource": "pods"}
    assert body["steps"][0]["result_chars"] == len("NAME  READY\nfathom-mcp  1/1")
    assert body["model"] == "fathom-agent"


def _fake_session(cfg, mcp):
    import contextlib

    @contextlib.asynccontextmanager
    async def session():
        yield []

    return session()


# --- message and trace helpers -------------------------------------------------


def test_history_becomes_typed_messages():
    messages = build_messages(
        "and now?",
        [{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"}],
    )

    assert [type(m) for m in messages] == [HumanMessage, AIMessage, HumanMessage]
    assert messages[-1].content == "and now?"


def test_summarise_picks_the_last_non_tool_answer():
    answer, steps = summarise(
        {
            "messages": [
                AIMessage("", tool_calls=[{"name": "k8s_log", "args": {}, "id": "1"}]),
                ToolMessage("boom", tool_call_id="1"),
                AIMessage("The container ran out of memory."),
            ]
        }
    )

    assert answer == "The container ran out of memory."
    assert steps == [{"tool": "k8s_log", "args": {}, "result_chars": 4, "status": "success"}]


def test_summarise_explains_an_empty_closing_message():
    # A reasoning model that spends its final turn thinking returns content="" with
    # no tool calls. Returning "" to the caller would be a 200 that says nothing;
    # llm.py disables thinking to prevent this, and this is the backstop.
    answer, _ = summarise(
        {
            "messages": [
                AIMessage("", tool_calls=[{"name": "k8s_get", "args": {}, "id": "1"}]),
                ToolMessage("some output", tool_call_id="1"),
                AIMessage(""),
            ]
        }
    )

    assert "without producing an answer" in answer


def test_summarise_reports_hitting_the_iteration_ceiling():
    answer, _ = summarise(
        {"messages": [AIMessage("", tool_calls=[{"name": "k8s_get", "args": {}, "id": "1"}])]}
    )

    assert "iteration ceiling" in answer


def test_summarise_skips_an_empty_message_for_an_earlier_answer():
    answer, _ = summarise({"messages": [AIMessage("the real answer"), AIMessage("   ")]})

    assert answer == "the real answer"


# --- tool-result bounding ------------------------------------------------------


def test_truncate_keeps_the_tail_and_says_so():
    text = "\n".join(f"line {i}" for i in range(1000))

    out = mcp_client.truncate(text, 200)

    assert len(out) <= 200
    assert out.startswith("[truncated:")
    assert out.rstrip().endswith("line 999"), "the most recent lines must survive"


def test_truncate_leaves_small_results_alone():
    assert mcp_client.truncate("short", 8000) == "short"


def test_truncate_drops_the_partial_first_line():
    out = mcp_client.truncate("aaaaaaaaaa\nbbbb\ncccc", 60)

    assert "aaaa" not in out.split("\n", 1)[1]
