"""The Ollama chat model.

Deliberately thin. The interesting decision is what is *not* here: no temperature,
no top_p, no num_ctx. ollama/config.env pins those in the `fathom-agent` alias's
Modelfile so that serving parameters travel with the model rather than with every
request -- which is what makes swapping the base model a config change plus a re-run
of ollama/setup.sh, invisible to this application.

Passing them here would silently override that and split the source of truth in two.
langchain-ollama leaves them out of the request when they are None, so the alias's
values win.
"""

from __future__ import annotations

from langchain_ollama import ChatOllama

from .config import Config


def build_model(cfg: Config) -> ChatOllama:
    return ChatOllama(
        base_url=cfg.ollama_url,
        model=cfg.model,
        # think=false. Left on (the default), this model spends its final turn
        # reasoning and returns a message with empty content -- an answer that never
        # reaches the caller. It also costs context we cannot spare: the window is
        # 16k and tool results are the scarce resource here, not deliberation.
        #
        # This is the same stance ollama/config.env takes when it pins temperature
        # low: "this model picks tools and reads cluster state, it is not writing
        # prose."
        reasoning=False,
        # The pod must come up even when the host-side Ollama is down; /readyz is
        # what reports that, rather than a crash loop the operator has to decode.
        validate_model_on_init=False,
        client_kwargs={"timeout": cfg.request_timeout},
    )
