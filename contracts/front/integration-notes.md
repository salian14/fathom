# fathom-front -- integration notes

**Hand-written. `make contracts` never touches this file.**

`service.json` beside it is generated from the chart. This file is generated from
nothing, and for `fathom-front` it is the *whole* interface description, not just the
part a spec cannot express.

## Why there is no openapi.json here

`fathom-front` publishes no machine-readable spec and cannot be made to publish one
without putting code in it that exists only to be read. Go's `http.ServeMux` does not
report what was registered with it, so a route list can only come from a second
declaration beside `newMux` -- which is the hand-maintained copy this whole directory
exists to avoid -- or from probing paths a scraper already knows, which derives
nothing.

So the route table below is hand-written, and
`TestDocumentedRoutesAreServed` in `front/cmd/fathom-front/main_test.go` dials every
row of it through the real mux. **A route added to `newMux` without a row here is an
undocumented surface, and a row here that no longer resolves fails that test.** Change
both together.

The contract writer still starts the container and dials the chart's `containerPort`,
so the reachability check that matters is not lost -- see `scripts/contracts.py`.

## Routes

| Route | Methods | Goes to | Notes |
|---|---|---|---|
| `/api/chat` | POST | `fathom-agent` `POST /chat` | Request and response bodies pass through unchanged. Non-POST is 405 from the proxy, not the mux. |
| `/api/eval/` | GET | `fathom-eval-serve` | Subtree. `/api/eval` is stripped before forwarding, so `/api/eval/runs/7` becomes `/runs/7`. Query string is preserved. |
| `/api/features` | GET | handled here | `{"eval": bool}` -- which server-side gates are open. `Cache-Control: no-store`. |
| `/healthz` | GET | handled here | Liveness: this process is up. Never traced. |
| `/readyz` | GET | `fathom-agent` `GET /readyz` | The agent's body is forwarded verbatim, status included. |
| `/` | GET | handled here | The chat and eval UI, embedded in the binary. Anything unmatched lands here. |

## The eval gate

`/api/eval/` is registered **only** when `FATHOM_FRONT_EVAL_URL` is set. With no URL
there is no proxy, no route, and no tab -- `/api/eval/runs` falls through to the file
server and 404s like any other path that is not there. This is the same shape as
`fathom-mcp`'s `prometheus.url` gate, and for the same reason: a surface that fails on
every call is worse than one that is absent, because a caller spends time discovering
it.

`/api/features` is how the browser finds out. The page is embedded in the binary and
cannot read an environment variable, so it asks before rendering the view switcher.
**A client must read `/api/features` before assuming `/api/eval/*` exists.**

## Retries

`/api/chat` is not safe to retry blind -- see `contracts/agent/integration-notes.md`;
a turn that timed out at the proxy may still be running in the agent.

`/api/eval/*` is read-only all the way down and safe to retry freely.

`/readyz` is a probe: retrying is the point, and kubelet already does it.

## Timeouts

Three separate budgets, and they nest:

| Variable | Default | Bounds |
|---|---|---|
| `FATHOM_FRONT_AGENT_TIMEOUT` | 200s | one proxied `/api/chat` turn |
| `FATHOM_FRONT_READY_TIMEOUT` | 5s | the `/readyz` call into the agent |
| `FATHOM_FRONT_EVAL_TIMEOUT` | 10s | one proxied eval read |

`AGENT_TIMEOUT` must exceed the agent's own request timeout, or the front gives up
first and reports a failure the agent was about to answer. `READY_TIMEOUT` must not
exceed `AGENT_TIMEOUT`; `Load()` rejects a config where it does.

`EVAL_TIMEOUT` is deliberately not `AGENT_TIMEOUT`: a database read that takes 200
seconds is broken, and holding a browser connection open that long hides it.

## Idempotency

Nothing here holds state. Every route either serves an embedded asset or forwards to
someone who does, so idempotency is entirely the upstream's -- read theirs.

## Ordering and concurrency

No ordering constraints. The process holds no state between requests, so any number
of browsers may be talking to it at once; the ceiling is whatever the agent's own
concurrency is, not this service's.

## Auth

There is no auth between services in this repo -- the cluster is the trust boundary.
`fathom-front` holds no Kubernetes credentials and mounts no service account token; it
can reach exactly two URLs, both from configuration. Anything that changes that
changes the trust boundary, not just this service.
