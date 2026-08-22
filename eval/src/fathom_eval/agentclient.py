"""The /chat client.

Small on purpose, with two rules that matter more than anything else here.

**No retries, ever.** A retried turn double-samples the distribution: the second attempt
is conditioned on nothing, but it replaces a real observation with a luckier one, and the
pass rate stops meaning what it says. A failed turn is data.

**The client timeout must exceed the agent's own.** fathom-agent enforces
`request_timeout` (180s default) itself and answers 504 when it expires. If our client
gave up first we would record a transport error and lose the agent's own verdict about
what happened -- the same reasoning that makes fathom-front's timeout exceed the agent's.
"""

from __future__ import annotations

import logging

import httpx

log = logging.getLogger("fathom_eval.agent")


class AgentClient:
    def __init__(self, chat_url: str, timeout: float) -> None:
        self._chat_url = chat_url
        self._client = httpx.Client(timeout=timeout)

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> AgentClient:
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def ask(self, question: str) -> tuple[int | None, dict | None, str]:
        """One turn. Returns (status, body, error) -- never raises for a bad answer.

        `history` is deliberately never sent: each iteration must be an independent
        sample, and conditioning iteration N on N-1 would turn ten samples into one.
        """
        try:
            response = self._client.post(self._chat_url, json={"question": question})
        except httpx.HTTPError as err:
            return None, None, f"{type(err).__name__}: {err}"

        try:
            body = response.json()
        except ValueError:
            return response.status_code, None, f"non-JSON response: {response.text[:200]}"
        return response.status_code, body, ""

    def ready(self) -> str:
        """Empty when the agent reports ready, else why it did not.

        Checked once before a run rather than per iteration: a run against an agent
        whose MCP server is down produces a hundred identical failures that say nothing
        about answer quality.
        """
        url = self._chat_url.removesuffix("/chat") + "/readyz"
        try:
            response = self._client.get(url, timeout=30.0)
        except httpx.HTTPError as err:
            return f"{url} unreachable: {err}"
        return "" if response.status_code == 200 else response.text.strip()

    def version(self) -> str:
        """The agent's package version, for run provenance.

        Read from the OpenAPI document because /chat does not carry it and there is no
        dedicated endpoint. Best-effort: provenance is worth having, not worth failing a
        run over.
        """
        url = self._chat_url.removesuffix("/chat") + "/openapi.json"
        try:
            response = self._client.get(url, timeout=15.0)
            return str(response.json().get("info", {}).get("version", "")) or "unknown"
        except (httpx.HTTPError, ValueError):
            return "unknown"
