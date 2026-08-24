# fathom-mcp -- integration notes

**Hand-written. `make contracts` never touches this file.**

`mcp-tools.json` beside it is scraped from a real `initialize` + `tools/list` against a
running container -- twice, once with `prometheus.url` unset and once with it set --
so the schemas there are whatever the SDK actually infers from the handler signatures.
That file describes the shape of the interface. This one describes how to use it
without causing an incident.

## Reaching it

Streamable HTTP at `FATHOM_MCP_MCP_PATH` (`/mcp`). The handler is registered at **both
`/mcp` and `/mcp/`** -- the subtree form works, and a client that appends a slash is
not broken.

`FATHOM_MCP_TRANSPORT=stdio` is the other mode, for `make -C mcp run-stdio` and local
tool exercise. Nothing in the cluster uses it.

## The Prometheus gate

Three of the six tools -- `list_metrics`, `describe_metric`, `run_promql` -- are
registered **only** when `prometheus.url` is set. They are marked `gated_on` in
`mcp-tools.json`. With no URL there is no Prometheus client and those tools are absent
from `tools/list` entirely.

That is deliberate: a tool that fails on every call is worse than one that is not
there, because the model spends turns discovering it. The server's `instructions`
change with the gate too, and both variants are published.

**A client must read `tools/list`. Do not hardcode six tools.**

## Retries

Every tool is read-only, so every call is safe to retry. Whether it is *worth*
retrying depends on the error class the failure was counted under -- the same seven for
Kubernetes and Prometheus failures alike, because `mcp.tool.errors` is one stream and
two vocabularies would split it into two that cannot be compared:

| Class | Retry? |
|---|---|
| `none` | -- |
| `not_found` | No. The object is not there. |
| `forbidden` | No. The ClusterRole does not cover it; widening RBAC is a `mcp/helm` change. |
| `unauthorized` | No. The projected token is bad -- a deploy fault, not a transient. |
| `timeout` | Yes, once. Covers API-server timeouts and 429s. |
| `invalid` | No. The arguments were rejected; the model supplied them and should fix them. |
| `other` | Once, then stop. |

## Timeouts

| Variable | Default | Bounds |
|---|---|---|
| `FATHOM_MCP_REQUEST_TIMEOUT` | 30s | one Kubernetes API call |
| `FATHOM_MCP_PROMETHEUS_TIMEOUT` | 15s | one Prometheus HTTP call |

Both sit inside the agent's 180s turn budget, many times over -- a turn makes several
tool calls, and the ceiling that actually binds is
`FATHOM_AGENT_MAX_ITERATIONS` (8), not these.

## Result bounding

Tool output is bounded **before** it reaches the model, and the caps are not advisory:

| Variable | Default | Caps |
|---|---|---|
| `FATHOM_MCP_MAX_BYTES` | 65536 | any single tool result |
| `FATHOM_MCP_LIST_LIMIT` | 200 | objects returned by `k8s_get` |
| `FATHOM_MCP_LOG_TAIL_LINES` | 200 | default `k8s_log` tail |
| `FATHOM_MCP_LOG_MAX_TAIL_LINES` | 2000 | the most a caller may ask for |
| `FATHOM_MCP_LOG_MAX_BYTES` | 32768 | bytes of log, whatever the line count |

A truncated result says so in its text. Do not treat a bounded list as a complete one.

## Idempotency

Every tool is a read; repeating one is free and returns the cluster as it is now, which
may differ from a moment ago. There is no caching anywhere in this service.

## Ordering and concurrency

No ordering constraints between tools, and no session state -- an MCP session carries
no server-side context beyond the connection. Concurrency into the API server is
bounded by client-go at QPS 20 / burst 40, shared across all in-flight calls.

## Tracing

`tools/call` is exempt from the probe filter on purpose, so a propagation failure
surfaces as root spans rather than as silence. Everything else inbound that carries no
trace context is dropped, because that is exactly what the agent's suppressed
dependency probe looks like from this side. `/healthz` and `/readyz` produce no spans
anywhere.

## Auth

There is no auth between services in this repo -- the cluster is the trust boundary.

**This service holds the only ServiceAccount in the `fathom` namespace that can read
the cluster**, and the ClusterRole in `mcp/helm` is the complete list of what the whole
system can see. Two properties depend on it and neither is an accident:

- **Read verbs only.** No tool mutates anything. The Prometheus side holds this by
  construction -- the four endpoints used only read the TSDB and `run_promql` is
  instant-query only.
- **The RBAC and the tool set move together.** A tool that reads a new resource kind
  needs the matching `get`/`list` rule, or it fails as `forbidden` at runtime.

Adding a write tool or a write verb is an explicit decision by the repo owner, not a
step in implementing something else.
