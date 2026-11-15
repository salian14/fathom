# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

Phase 1 is built. All four deliverables exist and are deployable: `ollama/`, `agent/` (Python),
`mcp/` (Go) and `front/` (Go). The three in-cluster services all carry OpenTelemetry
instrumentation, as does `eval/` — see [Telemetry](#telemetry) for the one way `eval/`
differs.

`eval/` (Python) is the evaluation harness, added after Phase 1: it stages known-broken
workloads in disposable namespaces, asks the agent about them, and grades the answers
deterministically into SQLite. The grading runs as two suspended-by-default CronJobs. It
also carries one long-running workload — `serve`, a read-only HTTP view of the results
database — because SQLite is a library rather than a server, and `fathom-front`'s eval
view is in another namespace and cannot mount the volume.

## What this is

Fathom is an agentic AI system: three services running as workloads inside a local Kubernetes
cluster, driving a host-side Ollama model to answer questions about that cluster using read-only
tools.

- **`agent/`** — Python 3.12, FastAPI + LangChain's `create_agent`. Owns the agent loop and the
  `POST /chat` API. Holds no Kubernetes credentials at all.
- **`mcp/`** — Go, an MCP server over streamable HTTP. Owns every read-only tool and the only
  service account that can read the cluster.
- **`front/`** — Go, a two-view UI served from the binary: a chat view with `/api/chat`
  reverse-proxied to the agent, and an eval view with `/api/eval/*` reverse-proxied to
  `fathom-eval`'s read service. Holds no Kubernetes credentials and no state. The eval
  view is gated on `eval.url` being set, exactly as the Prometheus tools are gated on
  `prometheus.url` — with no URL there is no route and no tab.
- **`eval/`** — Python, in namespace `fathom-eval`: two batch CronJobs plus a read-only
  service over their results. **The harness's ServiceAccount is the only one in this repo
  that can write to the cluster.** That is deliberate and isolated: it needs to create and
  destroy fixture namespaces, and it is kept out of the `fathom` namespace for that
  reason. It does not go through `fathom-mcp` and cannot widen what the agent can see.
  The read service has a ServiceAccount of its own, with no token and no RBAC at all — it
  needs the volume, not the cluster, and reusing the harness's would put namespace-delete
  on the one pod here that another namespace can reach.

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
| `fathom-eval-serve` (Python) | **Inside** the cluster, namespace `fathom-eval` |
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
deploy/        applications.yaml — the Argo CD Applications that deploy the lab
contracts/     GENERATED — each service's published interface. Never edit; `make contracts`
scripts/       contracts.py, the writer behind `make contracts`
.githooks/     the opt-in pre-commit hook; install with `make hooks`
```

There is no top-level `helm/`. The charts are `agent/helm`, `mcp/helm`, `front/helm` and
`eval/helm`. None of the three charts in the `fathom` namespace creates it — Argo CD does
(`CreateNamespace=true`). `eval/helm` creates its own `fathom-eval` namespace.

The top-level `Makefile` cuts one version across all four at once (`make release BUMP=patch`):
check, bump, commit and tag locally, build and push the images to `ghcr.io/salian14`. A
component joins it by appending to `COMPONENTS` and `CHARTS`. **It does not deploy, and
nothing in this repo does** — see [Deploying](#deploying).

Build and test: `make check` in any component (`fmt`, `lint`, tests, chart lint).

**`contracts/` is generated by running the services, and must never be hand-edited.**
The rule it is built around: **no component contains a line of code that exists to
produce a contract.** There is no `contract` make target and no `-contract` flag.
`scripts/contracts.py` builds each component's image, starts it in a throwaway
container configured the way its own chart configures it, and scrapes it over the wire
— FastAPI's own `/openapi.json` for `agent` and `eval`, a real `initialize` +
`tools/list` for `mcp` (twice, because three tools are gated on `prometheus.url`), plus
the applied SQLite schema read out of `sqlite_master` for `eval`. `helm template` supplies what a process
cannot know about itself: namespace, Service type, nodePort, RBAC, probes. The sandbox
dials the chart's own `containerPort`, so a pod that would start, pass its probes and
be unreachable through its Service fails here instead. **This needs Docker** — with one
carve-out. FastAPI publishes `/openapi.json` itself, so `make contracts` falls back to
serving `agent` and `eval` from their own venvs when no daemon is there, scraped through
the identical path and byte-identical as a result; `mcp`'s tool surface and both port
checks need a container and are left as they were, named on stderr. `contracts-check`
does not take that path — it is the drift gate, and one that silently checks less than
it claims is worse than one that refuses.

`front` is the only component with no generated spec, and that is the honest answer:
Go's `ServeMux` does not report what was registered with it, and it serves a UI plus two
proxies rather than an API of its own. Its routes are documented by hand in
`contracts/front/integration-notes.md` and pinned by `TestDocumentedRoutesAreServed`,
which dials every documented row through the real mux. Change the routes and the notes
together.

`fathom-eval-serve` moved from aiohttp to FastAPI on 2026-08-23 so that it publishes its
own spec — **the one legitimate way to change a service for a contract is to change how
it serves, never to add something whose only job is to describe it.** Its
`response_model`s must never narrow a payload: they carry `extra="allow"`, and
`test_response_models_drop_nothing` proves it against a real request.

Regenerate with `make contracts`; `make contracts-check` fails on drift and is part of
the root `make check`. `integration-notes.md` is the hand-written file per service —
the generator creates it once and never touches it again. **When adding an interface,
add nothing to the service to describe it.** If a shape cannot be scraped from a
running container, document it in `integration-notes.md` and write the test that dials
it, rather than declaring it in the source.

The four component Makefiles are deliberately uniform: `help build fmt lint test check
image push lint-chart render-chart run-local version clean all` mean the same
thing in each, so `make -C <component> <target>` works without knowing the language behind it, and
`make` with no target prints `help`. Anything outside that list is component-specific and
named so — `run-stdio` in `mcp`, `venv`/`lock`/`lock-check` in `agent` and `eval`, `test-ui` in
`front`, `run-judge`/`run-now`/`run-serve`/`db-pull` in `eval`; the Go components keep `vet` as an
alias for `lint`.
A new target that belongs to all four goes into all four, or it is not part of the shared set.

## Deploying

The lab is deployed by **Argo CD**, under the lab's application developer contract (linked
from `~/.claude/CLAUDE.md`; read it before changing anything deploy-shaped).
`docs/deploy.md` is this repo's side of it. The shape:

- `deploy/applications.yaml` holds three `Application`s — `fathom-mcp`, `fathom-agent`,
  `fathom-front` — each rendering its chart from `bifrost` into namespace `fathom` with
  automated sync, prune and self-heal. **A commit merged to `bifrost` is the deployment.**
  A PR branch is not deployed.
- There are no deploy targets, no `helm install/upgrade` and no `--set`. A change to what
  runs is a change to `values.yaml`; a new version is the bump commit from `make release`,
  whose images are pushed before it can merge. Never overwrite an image tag.
- Registering the Applications is the one `kubectl apply`, once per lab build, and it is a
  human's to run.
- `fathom-front` is the only thing exposed: a `VirtualService` on the lab's shared gateway
  under `/fathom/`, rewritten to `/`. No NodePorts. The page must request relative URLs
  only; `TestScriptsRequestRelativeURLs` pins it.
- `fathom-mcp`'s ClusterRole is cluster-scoped and was approved by the lab owner
  (2026-11-15). Anything else cluster-scoped, any second namespace, any NodePort is a
  request to the lab owner first.
- **`eval` is not deployed to the lab**: it needs exactly those things and they have not
  been requested. It has no Application; do not add one without that decision.
- `deploy/workflows/images.yml` is a build-and-push workflow parked outside
  `.github/workflows/` for a human to install. Agents still do not write to
  `.github/workflows/`.

## Telemetry

The three services on the request path export OTLP traces and metrics over **gRPC** to
`otel-collector.observability.svc.cluster.local:4317`, plaintext. `fathom-eval` exports **traces
only** — it builds a TracerProvider and no MeterProvider, because a batch job has no steady state
for an export interval to describe and the numbers it produces belong in `results.db`. Do not
"fix" that by adding a meter. Logs are not exported anywhere — they stay on stdout and carry
`trace_id`/`span_id` for correlation.

Configuration is the standard `OTEL_*` environment variables, read by the SDKs directly. The one
exception is `FATHOM_AGENT_TELEMETRY` / `FATHOM_MCP_TELEMETRY` / `FATHOM_FRONT_TELEMETRY` /
`FATHOM_EVAL_TELEMETRY` (`otlp` | `console` | `off`), which picks the exporter rather than tuning one. `console` writes to **stderr** and needs no collector —
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
`helm install/upgrade`, `kubectl apply`, or `vagrant up/destroy` against the real cluster — commit
the manifests and let Argo CD sync them; registering `deploy/applications.yaml` is the user's to do.
