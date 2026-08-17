"""The agent loop: a LangGraph ReAct graph over the MCP tools.

Kept deliberately small. The graph is rebuilt per request because its tools are
bound to a per-request MCP session (see mcp_client), and building it is cheap next
to a single model turn.
"""

from __future__ import annotations

from typing import Any

from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_core.tools import BaseTool
from langgraph.prebuilt import create_react_agent

from .config import Config


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
