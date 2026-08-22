# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

Phase 1 is built. All four deliverables exist and are deployable: `ollama/`, `agent/` (Python),
`mcp/` (Go) and `front/` (Go). All three in-cluster services carry OpenTelemetry
instrumentation.

`eval/` (Python) is the evaluation harness, added after Phase 1: it stages known-broken
workloads in disposable namespaces, asks the agent about them, and grades the answers
deterministically into SQLite. It runs as a suspended-by-default CronJob, not as a service.

## What this is

Fathom is an agentic AI system: three services running as workloads inside a local Kubernetes
cluster, driving a host-side Ollama model to answer questions about that cluster using read-only
tools.

- **`agent/`** — Python 3.12, FastAPI + LangGraph. Owns the agent loop and the `POST /chat` API.
  Holds no Kubernetes credentials at all.
- **`mcp/`** — Go, an MCP server over streamable HTTP. Owns every read-only tool and the only
  service account that can read the cluster.
- **`front/`** — Go, a chat UI served from the binary with `/api/chat` reverse-proxied to
  the agent. Holds no Kubernetes credentials and no conversation state.
- **`eval/`** — Python, a batch harness in namespace `fathom-eval`. **The only component
  in this repo whose ServiceAccount can write to the cluster.** That is deliberate and
  isolated: it needs to create and destroy fixture namespaces, and it is kept out of the
  `fathom` namespace for that reason. It does not go through `fathom-mcp` and cannot
  widen what the agent can see.

The split matters: the agent pod runs with `automountServiceAccountToken: false` and no RBAC, so
every cluster read goes through `fathom-mcp`. Widening what the agent can see is an RBAC change in
`mcp/helm`, never a change in the agent.

## Topology (the important part)

Five moving pieces:

| Piece | Runs where |
|---|---|
| Ollama model server | **Host machine**, outside the cluster |
| `fathom-agent` (Python) | **Inside** the cluster, namespace `fathom` |
| `fathom-mcp` (Go) | **Inside** the cluster, namespace `fathom` |
| `fathom-front` (Go) | **Inside** the cluster, namespace `fathom` |
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

Six tools, all read-only, in two groups:

- **Kubernetes**, always registered: `k8s_get`, `k8s_describe`, `k8s_log`. Backed by client-go
  against the API server, scoped by the ClusterRole in `mcp/helm`.
- **Prometheus**, registered only when `prometheus.url` is set: `list_metrics`,
  `describe_metric`, `run_promql`. Backed by the Prometheus HTTP API — a different backend
  entirely, with no Kubernetes credentials involved and no RBAC attached.

The gate is the point of the second group: with no URL there is no Prometheus client and those
three tools are absent from `tools/list`, because a tool that fails on every call is worse than
one that is not there — the model spends turns discovering it. Adding the URL is a values change
and a rollout, never a code change.

Do not add write/mutate tools, and do not widen the RBAC beyond read verbs, without an explicit
decision from the user — the read-only boundary is a safety property, not an accident of
incomplete work. That boundary holds on the Prometheus side by construction: the four endpoints
used (`/api/v1/query`, `/api/v1/series`, `/api/v1/metadata`, `/api/v1/label/__name__/values`)
only read the TSDB, and `run_promql` is instant-query only.

## Layout

Each component is its own top-level directory, with its own chart and its own release lifecycle:

```
ollama/        host-side Ollama setup — config.env (all knobs) + setup.sh (applies them)
agent/         Python agent — src/fathom_agent/, tests/, Dockerfile, helm/
mcp/           Go MCP server — cmd/, internal/, Dockerfile, helm/
front/         Go chat UI — cmd/, internal/ (assets embedded), Dockerfile, helm/
eval/          Python eval harness — src/fathom_eval/ (fixtures + rubrics packaged), helm/
```

There is no top-level `helm/`. The charts are `agent/helm`, `mcp/helm`, `front/helm` and
`eval/helm`; `mcp/helm` creates the `fathom` namespace and `agent`/`front` do not, because two
releases owning one Namespace object collide on install. `eval/helm` creates its own
`fathom-eval` namespace, which is a different object and so does not collide.

The top-level `Makefile` cuts one version across all four at once (`make release BUMP=patch`).
A component joins it by appending to `COMPONENTS` and `CHARTS` and adding an ordered
`deploy-<name>` target — ordered because each service's readiness depends on the one before it:
mcp, then agent, then front, then eval. `deploy-eval` is the one target that cannot gate on a
rollout: a CronJob has none, and its PVC stays `Pending` by design under
`WaitForFirstConsumer`, so waiting on `Bound` would hang on a healthy install. It asserts the
CronJob's image tag instead.

Build and test: `make check` in any component (`fmt`, `lint`, tests, chart lint).

The four component Makefiles are deliberately uniform: `help build fmt lint test check
image push lint-chart render-chart run-local version clean all` mean the same thing in
each, so `make -C <component> <target>` works without knowing the language behind it, and
`make` with no target prints `help`. Anything outside that list is component-specific and
named so — `run-stdio` in `mcp`, `venv`/`lock`/`lock-check` in `agent` and `eval`, `test-ui` in
`front`, `run-judge`/`run-now`/`db-pull` in `eval`; the Go components keep `vet` as an alias for `lint`.
A new target that belongs to all four goes into all four, or it is not part of the shared set.

## Telemetry

All three in-cluster services export OTLP traces and metrics over **gRPC** to
`otel-collector.observability.svc.cluster.local:4317`, plaintext. Logs are not exported — they stay
on stdout and carry `trace_id`/`span_id` for correlation.

Configuration is the standard `OTEL_*` environment variables, read by the SDKs directly. The one
exception is `FATHOM_AGENT_TELEMETRY` / `FATHOM_MCP_TELEMETRY` / `FATHOM_FRONT_TELEMETRY`
(`otlp` | `console` | `off`), which picks the exporter rather than tuning one. `console` writes to **stderr** and needs no collector —
use it for local runs, and note that stdout is unusable for this in `mcp` because the stdio
transport carries JSON-RPC there.

Two rules when touching this:

- **Adding a boundary means instrumenting it.** Prefer the auto-instrumentation libraries over
  hand-rolled spans; reach for a manual span only where no instrumentor can see the seam. There
  are three such boundaries in `fathom-mcp`: inbound MCP, the Kubernetes API, and — when
  configured — Prometheus. `fathom-front` has two: inbound HTTP from the browser and outbound
  HTTP to the agent. Each is instrumented by wrapping its transport once at construction,
  never per call.
- **Probes are excluded, not sampled.** `/healthz` and `/readyz` produce no spans anywhere.
  The agent suppresses instrumentation around its dependency checks and opens their MCP session
  without a span; `fathom-mcp` in turn drops inbound requests that carry no trace context, since
  that is exactly what the suppressed probe looks like from the far side. `tools/call` is exempt,
  so a propagation failure surfaces as root spans rather than silence. Verified end to end: a
  probe cycle emits zero spans, a real `/chat` emits one trace across every service it touches.

  **`fathom-front` is the exception to the trace-context rule, and it is not a bug.** A browser
  sends no `traceparent`, so the front is where a request first acquires a trace id — it is the
  root, not a middle hop. Its filter excludes the two probe paths and nothing else; copying
  `fathom-mcp`'s "drop what carries no trace context" rule here would silence every real request
  and leave the probes as the only traced traffic. See `front/internal/telemetry/httpfilter.go`,
  which says so at the point where someone would be tempted to change it.

Metric labels must stay low-cardinality: resource kinds, tool names, error classes, and Prometheus
endpoint kinds (`query`, `series`, `metadata`, `label_values`) are safe; object names, selectors,
session ids, question text, **metric names and PromQL queries** are span attributes only. The
Prometheus failure path classifies onto the same seven error classes as the Kubernetes one rather
than inventing a parallel vocabulary — both feed `mcp.tool.errors`, and two vocabularies would
split one stream into two that cannot be compared.

**Python dependency changes are a two-step, in `agent/` and `eval/` alike.** `agent/Dockerfile` installs `requirements.lock` and
then `pip install --no-deps .`, so the lock is the only source of what lands in the image. After
editing `pyproject.toml`, run `make lock` (needs Docker) or the image will build green and die on
`ImportError` at pod start. `make check` guards this via `lock-check`.

`ollama/config.env` is the single source of truth for the model name and endpoint.
`sudo ollama/setup.sh` applies it to the host (systemd drop-in, model pull, derived
`fathom-agent` alias with pinned `num_ctx`/temperature) and prints the values to copy into
`agent/helm/values.yaml`; `ollama/setup.sh --verify` checks an existing install without changing
anything. The agent requests the **alias**, not the raw model tag, so swapping base models is a
config change plus a re-run rather than an application change.

`sudo ollama/setup.sh --judge` installs a **second, CPU-only** Ollama instance
(`ollama-judge.service`, `CUDA_VISIBLE_DEVICES=""`, bound `0.0.0.0:11435`) serving the
`fathom-judge` alias for `eval/`. It is a separate unit rather than a second model in the
existing one because the GPU has 8 GB, the agent's model already fills it, and
`OLLAMA_MAX_LOADED_MODELS=1` means a second model would evict it. The judge must bind the
node network, not loopback, because it is dialled from an in-cluster CronJob. Its knobs
live in the judge block of `ollama/config.env` and must match `eval/helm/values.yaml`
→ `judge.url` / `judge.model`, the same two-file agreement the agent's endpoint has.

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
