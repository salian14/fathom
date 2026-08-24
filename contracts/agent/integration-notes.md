# fathom-agent -- integration notes

**Hand-written. `make contracts` never touches this file.**

`openapi.json` beside it is scraped from a running container's own `/openapi.json` --
FastAPI publishes it, and nothing was added to the service to obtain it. That file
describes the shape of the interface. This one describes how to use it without causing
an incident.

## Depends on

| Service | Protocol | Required | Notes |
|---|---|---|---|
| `fathom-mcp` | MCP, streamable HTTP | yes | See `contracts/mcp/`. **Every cluster read goes through it.** The agent pod runs with `automountServiceAccountToken: false` and has no RBAC of its own. |
| Ollama | HTTP | yes | On the **host**, outside the cluster. Reached at the host's address on the libvirt network -- never `localhost`, never a Service. |

Widening what the agent can see is an RBAC change in `mcp/helm`. It is never a change
here.

The agent requests the **alias** (`fathom-agent`), not a raw model tag, so swapping
base models is `ollama/config.env` plus a re-run rather than an application change.

## Retries

**`POST /chat` is not safe to retry blind.** A turn that timed out at the caller may
still be running: the agent has no request-id and no dedup, so a retry is a second
independent turn against the same cluster. Reads are the only thing it can do, so a
duplicate is wasteful rather than dangerous -- but it doubles the model time and the
answers may differ.

| Status | Retry? |
|---|---|
| 400 | No. Malformed history -- an unknown role. Fix the request. |
| 422 | No. The body failed validation; `question` must be non-empty. |
| 502 | Sometimes. The turn failed and the underlying type and message are forwarded -- read them. An Ollama connection failure is worth one retry; a model that does not exist is not. |
| 504 | No, not immediately. The whole turn budget expired. Retrying spends it again. |

`/healthz` and `/readyz` are probes: retrying is the point.

## Timeouts

| Variable | Default | Bounds |
|---|---|---|
| `FATHOM_AGENT_REQUEST_TIMEOUT` | 180s | one whole `/chat` turn, model time included |
| `FATHOM_AGENT_REMEDIATION_TIMEOUT` | 45s | the second, structured-output call only |
| `FATHOM_AGENT_MAX_ITERATIONS` | 8 | tool-call rounds before the turn is cut short |

A caller must budget **more** than `REQUEST_TIMEOUT`, or it gives up on a turn the
agent is about to answer. `fathom-front` sets 200s against this 180s for exactly that
reason.

Hitting `MAX_ITERATIONS` is not an error. The turn returns 200 with whatever was
learned, `iterations` at the ceiling, and a `summary` that says so. Check `iterations`
before trusting an answer that reads as incomplete.

## Response shape

`remediation` and `remediation_error` are never both present. Absent `remediation`
with no error means shaping was skipped because there was nothing to remediate -- a
healthy cluster -- not that shaping failed.

`steps` is the tool trace: which tools ran, with what arguments, and how much came
back. It is the first thing to read when an answer looks wrong, and it is in the
response precisely so nobody has to reconstruct it from pod logs. A step with a null
`result_chars` never produced a `ToolMessage`, which is how a turn cut short by the
iteration ceiling ends.

The `remediation` schema is `REMEDIATION_SCHEMA` itself -- the same object Ollama is
constrained by -- pointed at rather than restated, so the published schema and the one
the model actually obeys cannot disagree.

## Idempotency

None. There is no request id, no dedup and no cache. Two identical questions are two
independent turns and may return different answers; the model is not pinned to a seed.

Nothing the agent can do mutates the cluster, so a repeat is safe in the sense that
matters -- it just is not free.

## Ordering and concurrency

`history` is the only ordering that exists, and it is supplied by the caller: the agent
holds no session state between requests. Roles must be ones it recognises, or the turn
is a 400 rather than a silently dropped message.

Any number of turns may run concurrently. The real ceiling is Ollama's --
`OLLAMA_MAX_LOADED_MODELS=1` on a host with 8 GB of VRAM means concurrent turns queue
behind one another at the model, not here.

## Auth

There is no auth between services in this repo -- the cluster is the trust boundary.

The property worth protecting is that **this service holds no Kubernetes credentials
at all.** `automountServiceAccountToken: false`, no RBAC, no kubeconfig. Anything that
would give it cluster access directly -- rather than through `fathom-mcp` -- removes
the boundary that makes the tool set the complete list of what the agent can see.
