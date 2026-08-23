"""Smoke tests for the HTTP surface and the tool-result bounding.

The graph is stubbed: these check the wiring around the model, not the model. There
is no fake Ollama and no fake MCP server -- exercising those is what
`make run-local` against the real cluster is for.
"""

from __future__ import annotations

import dataclasses
import json

import pytest
from fastapi.testclient import TestClient
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage

from fathom_agent import api, mcp_client
from fathom_agent.agent import build_messages, shape_remediation, summarise
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
    _stub_shaper(client, {"action_required": False, "summary": "nothing to do"})

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


# --- remediation ---------------------------------------------------------------


class _Shaper:
    """Stands in for the second, tool-free model call."""

    def __init__(self, reply):
        self._reply = reply
        self.calls = 0

    async def ainvoke(self, messages):
        self.calls += 1
        if isinstance(self._reply, Exception):
            raise self._reply
        return AIMessage(self._reply)


def _stub_shaper(client, reply):
    payload = reply if isinstance(reply, (str, Exception)) else json.dumps(reply)
    shaper = _Shaper(payload)
    client.app.state.shaper = shaper
    return shaper


def _answering_graph(monkeypatch, answer="The pod crashloop-app is in CrashLoopBackOff."):
    class FakeGraph:
        async def ainvoke(self, state, config):
            return {"messages": [AIMessage(answer)]}

    monkeypatch.setattr(api, "build_graph", lambda cfg, model, tools: FakeGraph())
    monkeypatch.setattr(api, "tool_session", _fake_session)


def test_chat_returns_a_structured_remediation(client, monkeypatch):
    _answering_graph(monkeypatch)
    _stub_shaper(
        client,
        {
            "action_required": True,
            "summary": "Correct the container command on crashloop-app.",
            "target_kind": "Pod",
            "target_name": "crashloop-app",
            "namespace": "fathom-eval-crashloop",
            "commands": ["kubectl -n fathom-eval-crashloop edit pod crashloop-app"],
            "confidence": "high",
        },
    )

    body = client.post("/chat", json={"question": "why is it down?"}).json()

    assert body["remediation"]["action_required"] is True
    assert body["remediation"]["target_name"] == "crashloop-app"
    assert body["remediation"]["commands"] == [
        "kubectl -n fathom-eval-crashloop edit pod crashloop-app"
    ]
    assert body["remediation_error"] is None
    # The prose answer is untouched by shaping.
    assert body["answer"] == "The pod crashloop-app is in CrashLoopBackOff."


def test_a_failed_shaping_call_does_not_fail_the_turn(client, monkeypatch):
    # The whole point of remediation_error: the diagnosis is already in hand, so an
    # infrastructure fault in the second call must not cost the caller the answer.
    _answering_graph(monkeypatch)
    _stub_shaper(client, RuntimeError("connection refused"))

    response = client.post("/chat", json={"question": "why is it down?"})
    body = response.json()

    assert response.status_code == 200
    assert body["answer"] == "The pod crashloop-app is in CrashLoopBackOff."
    assert body["remediation"] is None
    assert "connection refused" in body["remediation_error"]


def test_an_unparseable_shaper_reply_is_reported_not_coerced(client, monkeypatch):
    # A shaper that silently defaulted to "no action required" would report a
    # healthy cluster every time the model returned prose.
    _answering_graph(monkeypatch)
    _stub_shaper(client, "I think you should probably restart it?")

    body = client.post("/chat", json={"question": "why is it down?"}).json()

    assert body["remediation"] is None
    assert "unparseable" in body["remediation_error"]


@pytest.mark.parametrize(
    "answer",
    [
        "Sorry, need more steps to process this request.",
        "The model stopped while still calling tools, which usually means it hit ...",
        "The model finished without producing an answer. The tool calls it made ...",
    ],
)
def test_shaping_is_skipped_when_there_is_no_answer_to_shape(client, monkeypatch, answer):
    # There is nothing to extract from a sentinel, and calling anyway would spend the
    # shaping budget to learn that.
    _answering_graph(monkeypatch, answer=answer)
    shaper = _stub_shaper(client, {"action_required": True, "summary": "x"})

    body = client.post("/chat", json={"question": "why is it down?"}).json()

    assert shaper.calls == 0
    assert body["remediation"] is None
    assert body["remediation_error"] is None


def test_remediation_off_skips_the_call_entirely(client, monkeypatch):
    _answering_graph(monkeypatch)
    shaper = _stub_shaper(client, {"action_required": True, "summary": "x"})
    client.app.state.cfg = dataclasses.replace(client.app.state.cfg, remediation="off")

    body = client.post("/chat", json={"question": "why is it down?"}).json()

    assert shaper.calls == 0
    assert body["remediation"] is None


async def test_empty_shaper_fields_are_dropped_not_stored_blank():
    # Ollama honours the schema but returns "" for a field it has nothing to say
    # about. Keeping those would make "no target" indistinguishable from "blank".
    cfg = load()
    shaper = _Shaper(
        json.dumps(
            {
                "action_required": True,
                "summary": "Raise the memory limit.",
                "target_kind": "",
                "target_name": "oom-app",
                "commands": ["", "  "],
                "confidence": "medium",
            }
        )
    )

    remediation, error = await shape_remediation(cfg, shaper, "q", "an answer")

    assert error == ""
    assert remediation == {
        "action_required": True,
        "summary": "Raise the memory limit.",
        "target_name": "oom-app",
        "confidence": "medium",
    }


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


# --- the published contract -----------------------------------------------------


def test_chat_response_model_drops_nothing(client, monkeypatch):
    """A response_model filters the body it serialises, so it can silently delete a
    field the UI or the eval harness reads. That failure is invisible in the spec --
    the spec would simply stop mentioning it -- so it is pinned here instead."""

    class FakeGraph:
        async def ainvoke(self, state, config):
            return {
                "messages": [
                    AIMessage(
                        "",
                        tool_calls=[{"name": "k8s_log", "args": {"name": "p"}, "id": "1"}],
                    ),
                    ToolMessage("boom", tool_call_id="1"),
                    AIMessage("The pod is crash-looping."),
                ]
            }

    monkeypatch.setattr(api, "build_graph", lambda cfg, model, tools: FakeGraph())
    monkeypatch.setattr(api, "tool_session", _fake_session)
    _stub_shaper(client, {"action_required": True, "summary": "restart it", "confidence": "low"})

    body = client.post("/chat", json={"question": "why?"}).json()

    assert set(body) == {
        "answer",
        "remediation",
        "remediation_error",
        "steps",
        "iterations",
        "model",
        "elapsed_seconds",
    }
    # The remediation passes through as the shaper produced it -- the schema is
    # REMEDIATION_SCHEMA, not a Pydantic restatement that could narrow it.
    assert body["remediation"]["summary"] == "restart it"
    assert set(body["steps"][0]) == {"tool", "args", "result_chars", "status"}


def test_the_published_spec_describes_every_route():
    """The contract is generated from app.openapi(), so an undocumented route is an
    absence in the spec rather than a visible bug. Assert the shape the writer in
    scripts/contracts.py depends on."""
    from fathom_agent.contract import build

    doc = build()
    spec = doc["openapi"]

    assert doc["service"] == "fathom-agent"
    assert sorted(spec["paths"]) == ["/chat", "/healthz", "/readyz"]
    # A body schema, not a bare 200. This is the gap response_model closed.
    ok = spec["paths"]["/chat"]["post"]["responses"]["200"]
    assert ok["content"]["application/json"]["schema"]["$ref"].endswith("/ChatResponse")
    # Every failure a caller has to handle is declared.
    assert {"400", "422", "502", "504"} <= set(spec["paths"]["/chat"]["post"]["responses"])


def test_the_contract_is_byte_stable():
    """Regenerating without changing anything must produce identical bytes, or every
    commit becomes a contract commit and people stop reading the diff."""
    import json

    from fathom_agent.contract import build

    first = json.dumps(build(), indent=2, sort_keys=True)
    second = json.dumps(build(), indent=2, sort_keys=True)
    assert first == second


def test_declared_metric_attributes_are_what_is_recorded():
    """The telemetry contract's attribute keys are the one thing in contracts/ that
    cannot be read off a live provider -- keys are chosen at record time, not at
    instrument creation -- so the declaration is hand-written and worth exactly as
    much as this test.

    It is also the rule conventions.md prohibition 4 exists for: anything from the
    question or the answer added to this stream multiplies the time series, and that
    change looks harmless in review. It fails here instead."""
    from opentelemetry import metrics
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import InMemoryMetricReader

    from fathom_agent.contract import build

    reader = InMemoryMetricReader()
    metrics.set_meter_provider(MeterProvider(metric_readers=[reader]))

    class _Span:
        def set_attribute(self, *args):
            pass

        def record_exception(self, *args):
            pass

    api._record_turn(_Span(), "ok", 0.0)

    observed: set[str] = set()
    found = False
    for rm in reader.get_metrics_data().resource_metrics:
        for sm in rm.scope_metrics:
            for metric in sm.metrics:
                if metric.name != "fathom.chat.turn.duration":
                    continue
                found = True
                for point in metric.data.data_points:
                    observed |= set(point.attributes or {})
    assert found, "the turn histogram was never recorded"

    declared = {
        key
        for instrument in build()["telemetry"]["instruments"]
        if instrument["name"] == "fathom.chat.turn.duration"
        for key in instrument["attributes"]
    }
    assert observed == declared, (
        f"recorded {sorted(observed)} but declared {sorted(declared)}. If a new key is "
        f"not a closed set, it belongs on the span rather than on this histogram."
    )
