"""The agent loop: a LangGraph ReAct graph over the MCP tools.

Kept deliberately small. The graph is rebuilt per request because its tools are
bound to a per-request MCP session (see mcp_client), and building it is cheap next
to a single model turn.
"""

from __future__ import annotations

import json
import logging
from typing import Any

from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_core.tools import BaseTool
from langgraph.prebuilt import create_react_agent

from .config import Config

log = logging.getLogger("fathom_agent.agent")

# The shape of a proposed fix. Deliberately flat: this model fills a handful of
# top-level fields reliably and nested objects much less so, and every field here is
# something the eval harness grades or a human reads.
#
# `action_required` is the escape hatch and it is the reason this is a boolean rather
# than an implied "there is always a fix". A healthy cluster has no remediation, and a
# schema that cannot say so teaches the model to invent one.
REMEDIATION_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "action_required": {"type": "boolean"},
        "summary": {"type": "string"},
        "target_kind": {"type": "string"},
        "target_name": {"type": "string"},
        "namespace": {"type": "string"},
        "commands": {"type": "array", "items": {"type": "string"}},
        "confidence": {"type": "string", "enum": ["low", "medium", "high"]},
    },
    "required": ["action_required", "summary", "confidence"],
}

SHAPER_PROMPT = """\
You are extracting the recommended fix from a Kubernetes troubleshooting answer that
has already been written. You are not diagnosing anything yourself and you have no
tools: use only what the answer says.

Fill the schema from the answer. `summary` is one sentence saying what to change.
`commands` are the kubectl commands the answer gives or clearly implies, if any.
`target_name` is the object the fix applies to.

If the answer says nothing is wrong, or recommends no action, set action_required to
false and say so in `summary`. Do not invent a fix the answer does not support.\
"""


def build_graph(cfg: Config, model, tools: list[BaseTool]):
    return create_react_agent(model, tools, prompt=SystemMessage(cfg.system_prompt))


def build_messages(question: str, history: list[dict[str, str]] | None) -> list[Any]:
    """Turn the request body into a message list.

    /chat is stateless: whatever context the caller wants carried, the caller sends.
    That keeps the pod restartable and horizontally scalable without a session store,
    at the cost of the client holding the transcript.
    """
    messages: list[Any] = []
    for turn in history or []:
        role = turn.get("role")
        content = turn.get("content", "")
        if role == "assistant":
            messages.append(AIMessage(content))
        elif role == "user":
            messages.append(HumanMessage(content))
        else:
            raise ValueError(f'history role must be "user" or "assistant", got {role!r}')
    messages.append(HumanMessage(question))
    return messages


def summarise(result: dict[str, Any]) -> tuple[str, list[dict[str, Any]]]:
    """Pull the final answer and the tool trace out of the finished graph state.

    The trace is returned to the caller because "which tools did it actually call"
    is the first question worth asking when an answer looks wrong -- and reading it
    from the response beats reconstructing it from pod logs.
    """
    messages = result.get("messages", [])

    steps: list[dict[str, Any]] = []
    for message in messages:
        if isinstance(message, AIMessage):
            for call in message.tool_calls or []:
                steps.append({"tool": call.get("name"), "args": call.get("args", {})})
        elif isinstance(message, ToolMessage):
            # Pair the result back onto the call it answers, in order.
            for step in reversed(steps):
                if "result_chars" not in step:
                    step["result_chars"] = len(str(message.content))
                    step["status"] = message.status or "success"
                    break

    return _answer(messages, steps), steps


def _answer(messages: list[Any], steps: list[dict[str, Any]]) -> str:
    """The model's closing message, or an explanation of why there isn't one.

    Returning "" here would be a silent failure -- the caller gets a 200 and an empty
    answer, with nothing to say whether the model refused, ran out of rounds, or
    spent its last turn thinking. Say which.
    """
    for message in reversed(messages):
        if isinstance(message, AIMessage) and not message.tool_calls:
            text = _text(message.content).strip()
            if text:
                return text

    if steps and any("result_chars" not in step for step in steps):
        return (
            "The model stopped while still calling tools, which usually means it hit "
            "the iteration ceiling (FATHOM_AGENT_MAX_ITERATIONS). The tool calls it "
            "did make are in `steps`."
        )
    return (
        "The model finished without producing an answer. The tool calls it made are "
        "in `steps`; the transcript had no closing message with any content."
    )


def _text(content: Any) -> str:
    """Flatten a message's content, which may be a string or a list of blocks."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [
            block if isinstance(block, str) else str(block.get("text", ""))
            for block in content
            if isinstance(block, str) or isinstance(block, dict)
        ]
        return "".join(parts)
    return str(content)


async def shape_remediation(
    cfg: Config, shaper, question: str, answer: str
) -> tuple[dict | None, str]:
    """Turn a finished answer into a structured remediation. Never raises.

    Returns (remediation, error). Both being falsy means the call was deliberately
    skipped. The distinction the caller needs -- and the harness grades -- is between
    "the model proposed no fix", which is an answer worth recording as poor, and "the
    shaping call failed", which is an infrastructure fault and says nothing about the
    model's judgement. That is what `error` carries.

    Only the question and the finished answer go in, never the transcript: the context
    window is 16k, prefill dominates the cost of a call this short, and the tool
    results have already done their work by this point.
    """
    if cfg.remediation != "on":
        return None, ""

    # Nothing to shape. These are the sentinels the caller would otherwise pay a full
    # shaping timeout to learn nothing about.
    if not answer.strip() or answer.startswith(_NO_ANSWER_PREFIXES):
        return None, ""

    messages = [
        SystemMessage(SHAPER_PROMPT),
        HumanMessage(f"QUESTION\n{question}\n\nANSWER\n{answer}"),
    ]
    try:
        reply = await shaper.ainvoke(messages)
    except Exception as err:  # noqa: BLE001 - reported, never fatal to the turn
        log.warning("remediation shaping failed: %s", err)
        return None, f"{type(err).__name__}: {err}"

    text = _text(reply.content).strip()
    if not text:
        return None, "shaper returned an empty response"

    try:
        parsed = json.loads(text)
    except ValueError as err:
        # Recorded rather than coerced. A shaper that silently defaults to
        # "no action required" would report a healthy cluster on every parse failure.
        return None, f"unparseable shaper response: {err}"

    if not isinstance(parsed, dict) or "action_required" not in parsed:
        return None, "shaper response did not match the schema"

    return _clean_remediation(parsed), ""


def _clean_remediation(parsed: dict) -> dict:
    """Normalise the shaper's object to the schema's types, dropping empty fields.

    Ollama honours the schema, but a field it has nothing to say about comes back as
    an empty string rather than absent. Dropping those keeps "the model gave no
    target" distinguishable from "the model gave an empty target".
    """
    out: dict[str, Any] = {
        "action_required": bool(parsed.get("action_required")),
        "summary": str(parsed.get("summary") or "").strip(),
    }
    for key in ("target_kind", "target_name", "namespace", "confidence"):
        if value := str(parsed.get(key) or "").strip():
            out[key] = value
    commands = [str(c).strip() for c in (parsed.get("commands") or []) if str(c).strip()]
    if commands:
        out["commands"] = commands
    return out


# The openings of the two fallbacks _answer() produces, plus LangGraph's own
# short-circuit string. Matched by prefix because only the opening clause is stable.
_NO_ANSWER_PREFIXES = (
    "The model stopped while still calling tools",
    "The model finished without producing an answer",
    "Sorry, need more steps to process this request.",
)
