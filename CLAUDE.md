# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

Phase 1 is built. All three deliverables exist and are deployable: `ollama/`, `agent/` (Python),
`mcp/` (Go). Both components carry OpenTelemetry instrumentation.

## What this is

Fathom is an agentic AI system: two services running as workloads inside a local Kubernetes cluster,
driving a host-side Ollama model to answer questions about that cluster using read-only tools.

- **`agent/`** — Python 3.12, FastAPI + LangGraph. Owns the agent loop and the `POST /chat` API.
  Holds no Kubernetes credentials at all.
- **`mcp/`** — Go, an MCP server over streamable HTTP. Owns the three read-only tools and the only
  service account that can read the cluster.

The split matters: the agent pod runs with `automountServiceAccountToken: false` and no RBAC, so
every cluster read goes through `fathom-mcp`. Widening what the agent can see is an RBAC change in
`mcp/helm`, never a change in the agent.

## Topology (the important part)

Four moving pieces:

| Piece | Runs where |
|---|---|
| Ollama model server | **Host machine**, outside the cluster |
| `fathom-agent` (Python) | **Inside** the cluster, namespace `fathom` |
| `fathom-mcp` (Go) | **Inside** the cluster, namespace `fathom` |
| otel-collector | **Inside** the cluster, namespace `observability` |

The consequence that shapes most of the code: **the agent and the model are on opposite sides of a
network boundary.** The agent is a pod on a VM-hosted cluster; Ollama listens on the host. Ollama
must bind beyond loopback (`OLLAMA_HOST`) and the agent must reach it via the host's address on the
libvirt network — not `localhost`, not a Service. Any change to the Ollama endpoint touches both the
startup scripts and the Helm values. Treat the endpoint as configuration, never a hardcoded default
that happens to work on one machine.

Cluster access uses a **service account registered with k8s**, so the agent authenticates from
in-cluster credentials (projected token + CA at the standard mount path), not a kubeconfig. Code
should use in-cluster config as the primary path; if a kubeconfig fallback exists for local dev, it
is a fallback only. The RBAC that backs the service account is part of the Helm chart and must stay
in lockstep with the tool set: adding a tool that reads a new resource kind means adding the
corresponding `get`/`list` rule.

## Scope

Tools are limited to exactly three, all read-only: `k8s_get`, `k8s_describe`, `k8s_log`. Do not add
write/mutate tools, and do not widen the RBAC beyond read verbs, without an explicit decision from
the user — the read-only boundary is a safety property, not an accident of incomplete work.

## Layout

Each component is its own top-level directory, with its own chart and its own release lifecycle:

```
ollama/   host-side Ollama setup — config.env (all knobs) + setup.sh (applies them)
agent/    Python agent — src/fathom_agent/, tests/, Dockerfile, helm/
mcp/      Go MCP server — cmd/, internal/, Dockerfile, helm/
```

There is no top-level `helm/`. The charts are `agent/helm` and `mcp/helm`; `mcp/helm` creates the
`fathom` namespace and `agent/helm` does not, because two releases owning one Namespace object
collide on install.

Build and test: `make check` in either component (`fmt`, `lint`/`vet`, tests, chart lint).

## Telemetry

Both services export OTLP traces and metrics over **gRPC** to
`otel-collector.observability.svc.cluster.local:4317`, plaintext. Logs are not exported — they stay
on stdout and carry `trace_id`/`span_id` for correlation.

Configuration is the standard `OTEL_*` environment variables, read by the SDKs directly. The one
exception is `FATHOM_AGENT_TELEMETRY` / `FATHOM_MCP_TELEMETRY` (`otlp` | `console` | `off`), which
picks the exporter rather than tuning one. `console` writes to **stderr** and needs no collector —
use it for local runs, and note that stdout is unusable for this in `mcp` because the stdio
transport carries JSON-RPC there.

Two rules when touching this:

- **Adding a boundary means instrumenting it.** Prefer the auto-instrumentation libraries over
  hand-rolled spans; reach for a manual span only where no instrumentor can see the seam.
- **Probes are excluded, not sampled.** `/healthz` and `/readyz` produce no spans on either side.
  The agent suppresses instrumentation around its dependency checks and opens their MCP session
  without a span; `fathom-mcp` in turn drops inbound requests that carry no trace context, since
  that is exactly what the suppressed probe looks like from the far side. `tools/call` is exempt,
  so a propagation failure surfaces as root spans rather than silence. Verified end to end: a
  probe cycle emits zero spans, a real `/chat` emits one trace across both services.

Metric labels must stay low-cardinality: resource kinds, tool names, and error classes are safe;
object names, selectors, session ids, and question text are span attributes only.

**Python dependency changes are a two-step.** `agent/Dockerfile` installs `requirements.lock` and
then `pip install --no-deps .`, so the lock is the only source of what lands in the image. After
editing `pyproject.toml`, run `make lock` (needs Docker) or the image will build green and die on
`ImportError` at pod start. `make check` guards this via `lock-check`.

`ollama/config.env` is the single source of truth for the model name and endpoint.
`sudo ollama/setup.sh` applies it to the host (systemd drop-in, model pull, derived
`fathom-agent` alias with pinned `num_ctx`/temperature) and prints the values to copy into
`agent/helm/values.yaml`; `ollama/setup.sh --verify` checks an existing install without changing
anything. The agent requests the **alias**, not the raw model tag, so swapping base models is a
config change plus a re-run rather than an application change.

The components have independent lifecycles: the Go module root is `mcp/`, the Python project root is
`agent/`, each chart versions separately, and the host scripts never run in CI against real
infrastructure.

## Conventions
- Tool implementations should be thin wrappers over client-go with strict input validation
  (namespace, name, resource kind). The model supplies these arguments; treat them as untrusted.
- Log output from `k8s_log` can be large — bound it (tail lines / byte cap) before it reaches the
  model context.

## Operational boundaries

`~/.claude/CLAUDE.md` defines the branching and CI/CD pipeline for this repo (`bifrost` = dev,
`asgard` = prod, `agent/<type>/<desc>` branches, no direct pushes, no manual `terraform apply` or
`ansible-playbook`). Those rules apply here in full. Additionally, in this repo: do not run
`helm install/upgrade`, `kubectl apply`, or `vagrant up/destroy` against the real cluster — propose
the manifests and let the pipeline or the user apply them.
