---
schema-version: 1
generated-from: eebf9d27bd6a24d688dd283b406f86f6a9f5bc1f
generated: 2026-08-23
services-covered: [agent, mcp, front, eval]
---

# Conventions

What a new or changed component in this repo has to look like. Written so the next
piece of work can be done without opening a sibling component; where it says "verified",
the fact was checked against the tree at the sha above rather than remembered.

`CLAUDE.md` at the repository root is the short form of this and stays authoritative on
architecture and operational boundaries. This file is the longer, mechanical version:
the shapes you copy.

> **Most of what follows is copyable, not describable.** Every heading under
> [Template material](#template-material) is a file that could live in a generator.
> Until one exists, this doc is the workaround — see
> [If you are adding a component](#if-you-are-adding-a-component).

---

## Prohibitions

Rules a change can violate without any existing component demonstrating it. They appear
in the tree only as absences, so they are stated here. Confirmed by the repository owner,
2026-08-23.

1. **No write tools, and no write verbs, in `fathom-mcp`.** Do not add a mutating tool
   and do not widen the ClusterRole past `get`/`list` (plus `get` on `pods/log`). The
   verbs are hardcoded in `mcp/helm/templates/_helpers.tpl` and are deliberately *not*
   parameterised, so widening them takes a template edit and an explicit decision from
   the user — never a `--set`. The read-only boundary is a safety property, not
   incomplete work.

2. **The agent never gets credentials.** `fathom-agent` keeps
   `automountServiceAccountToken: false` and no RBAC, permanently. Anything the model
   needs to see is added in `mcp/helm` as a values change, never in the agent. If a task
   seems to need a Kubernetes client in `agent/`, the task is wrong.

3. **`eval`'s write grant never widens.** It stays scoped by kind — bare Pods, Services,
   ConfigMaps, namespaces; nothing under `apps`, no nodes, no RBAC types. `secrets` stays
   **create and delete without get/list/watch**. The harness stays out of the `fathom`
   namespace. `fathom-eval-serve` keeps zero RBAC and no token: it needs the volume, not
   the cluster, and reusing the harness's ServiceAccount would put namespace-delete on
   the one pod here that another namespace can reach.

4. **No high-cardinality metric labels.** Resource kinds, tool names, error classes and
   Prometheus endpoint kinds (`query`, `series`, `metadata`, `label_values`) are safe as
   labels. Object names, selectors, session ids, question text, **metric names and PromQL
   queries** are span attributes only. A new failure path classifies onto the existing
   seven error classes rather than inventing a parallel vocabulary — both sides feed
   `mcp.tool.errors`, and two vocabularies split one stream into two that cannot be
   compared.

---

## Template material

### Directory structure

Each component is a top-level directory owning its own chart, its own lifecycle and its
own language toolchain. There is no top-level `helm/` and no shared source directory.

```
<component>/
  Dockerfile
  Makefile
  README.md
  .dockerignore
  .gitignore
  helm/
    Chart.yaml
    values.yaml
    .helmignore
    templates/
      _helpers.tpl
      NOTES.txt
      namespace.yaml
      serviceaccount.yaml
      deployment.yaml      # or cronjob.yaml, for a batch component
      service.yaml
      rbac.yaml            # only if the component needs credentials
```

Go components add:

```
  go.mod  go.sum
  cmd/<binary>/main.go
  internal/<pkg>/<pkg>.go
```

Python components add:

```
  pyproject.toml
  requirements.lock
  src/<package>/__init__.py  __main__.py  config.py  telemetry.py
  tests/test_<module>.py
```

### Naming

| Thing | Convention | Examples |
|---|---|---|
| Directory | bare noun, no prefix | `agent/`, `mcp/`, `front/`, `eval/` |
| Workload / release | `fathom-<dir>` | `fathom-agent`, `fathom-eval` |
| Go binary | `cmd/fathom-<dir>/` | `cmd/fathom-front/` |
| Python package | `fathom_<dir>`, underscore | `fathom_agent`, `fathom_eval` |
| Env var prefix | `FATHOM_<DIR>_` | `FATHOM_MCP_`, `FATHOM_EVAL_` |
| Chart | `<dir>/helm`, `name: fathom-<dir>` | |
| Image | `ghcr.io/salian14/fathom-<dir>` | public, tag pinned to the chart's `appVersion` |
| Argo CD Application | `fathom-<dir>`, in `deploy/applications.yaml` | `fathom-front` |

**One inconsistency, recorded rather than propagated.** Go module paths disagree:
`mcp/go.mod` is `github.com/spellbound/fathom/mcp` but `front/go.mod` is
`github.com/spellbound/fathom/fathom-front`. The directory-named form (`mcp`) matches
every other convention in this table. A new Go component should use it; changing
`front` is a rename nobody has asked for.

### Entrypoint

`main` / `__main__` does the same six things in the same order everywhere, and does no
work of its own:

1. load config (fail fast, exit non-zero naming the variable)
2. configure telemetry
3. configure the logger
4. construct clients, wrapping each transport **once** for instrumentation
5. register routes, then probes
6. serve, with signal-driven graceful shutdown

Anything with logic in it lives in `internal/` or the package, not in the entrypoint.

### Config loading

**Environment variables only.** No config files, no flags for anything that a chart
sets. Every variable is `FATHOM_<COMPONENT>_<NAME>`; the prefix is declared once and the
loader appends to it, so no call site spells out a full variable name.

- Go — `internal/config`, an exported `Config` struct of already-parsed types
  (`time.Duration`, `int64`, `slog.Level`, not strings), a `Load() (*Config, error)`,
  and unexported `envString` / `envInt64` / `envDuration` helpers that take a default.
  Verified in `front/internal/config/config.go`.
- Python — `config.py`, a frozen `@dataclass` and a `load()`, with `ENV_PREFIX` as a
  module constant. Verified in `eval/src/fathom_eval/config.py`.

**Validation is fail-fast and names the variable**, including cross-field rules. A
misconfigured pod dies on its first line of logs rather than mysteriously later:

```
fathom-agent: FATHOM_AGENT_MCP_TIMEOUT (60.0s) exceeds FATHOM_AGENT_REQUEST_TIMEOUT (30.0s):
a single tool call could outlive the turn that requested it
```

A listen address is `":8080"` or `"127.0.0.1:8080"` — the same regex, spelled the same
way, in `front/internal/config/config.go` and `eval/src/fathom_eval/config.py`, so a
listen address means one thing across the repo. Copy it; do not invent a third spelling.

### Logging

JSON, one object per line, on **stdout**, with `trace_id` and `span_id` on every record
that has a span. Logs are never exported to the collector — correlation is by id.

- Go — a `SlogHandler` wrapping a `slog.JSONHandler`, adding the two ids from the record
  context. `slog.Info` without a context logs without ids; that is expected, not a bug.
  Verified in `front/internal/telemetry/slog.go`.
- Python — a `logging.Formatter` emitting `json.dumps`, plus `LoggingInstrumentor(...)
  .instrument(set_logging_format=False)`. Build the object and dump it; never
  interpolate into a JSON literal in a format string, which drops `extra=` fields and
  breaks on any message containing a quote. Verified in `agent/src/fathom_agent/telemetry.py`.

`stderr` is for `console` telemetry mode only. In `mcp` stdout is unusable for spans
because the stdio transport carries JSON-RPC there.

### Telemetry init

One `telemetry.py` / `internal/telemetry` per component, exporting OTLP over **gRPC,
plaintext** to `otel-collector.observability.svc.cluster.local:4317`.

`FATHOM_<COMPONENT>_TELEMETRY` picks the exporter — `otlp` | `console` | `off` — and is
the *only* non-standard telemetry variable. Everything else is standard `OTEL_*`, read
by the SDK directly: `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL=grpc`,
`OTEL_TRACES_SAMPLER`, `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_PROPAGATORS`,
`OTEL_RESOURCE_ATTRIBUTES`, `OTEL_SERVICE_NAME`.

`agent`, `mcp` and `front` set `OTEL_PROPAGATORS=tracecontext,baggage` explicitly in
their Deployments; `eval` does not, and relies on the SDK default, which is the same two.
Behaviour is identical — set it explicitly in a new chart anyway, so the wire format is
stated rather than inherited.

- `off` still installs the propagator, so a component with telemetry disabled does not
  break the trace for the ones it calls.
- **Instrument by wrapping a transport once at construction, never per call.** Prefer an
  auto-instrumentation library; reach for a manual span only where no instrumentor can
  see the seam.
- **Adding a boundary means instrumenting it.**

Chart `otel` block: `mode`, `endpoint`, `sampler`, `metricExportInterval`, `environment`.
A batch component omits `metricExportInterval` and exports **traces only** — `eval` does,
deliberately, and `eval/helm/values.yaml` says so at the point where someone would add it.

### Probes

`/healthz` — the process is up. `/readyz` — the dependencies answered. Both are
`PlainTextResponse`-equivalent, and both are **excluded from tracing, not sampled**: a
full probe cycle emits zero spans anywhere.

`/readyz` is about what this pod depends on, and a 503 names *which* dependency failed
and why, so it distinguishes causes without reading logs:

| Component | `/readyz` means |
|---|---|
| `fathom-mcp` | the Kubernetes API answered. **Prometheus is deliberately excluded** — a metrics outage must not become a total outage |
| `fathom-agent` | Ollama answered `/api/version` **and** the MCP server advertised its tools |
| `fathom-front` | forwards the agent's `/readyz` verbatim. Deliberately not about itself, and deliberately ignores `eval.url` |
| `fathom-eval-serve` | `/data` is mounted and readable. **Not** that `results.db` exists — an absent database is a correct state and renders as an empty view |

**The trace-context rule and its one exception.** `fathom-mcp` drops inbound requests
carrying no trace context, because that is what a suppressed probe looks like from the
far side; `tools/call` is exempt so a propagation failure surfaces as root spans rather
than silence. `fathom-front` does **not** apply that rule and must not: a browser sends
no `traceparent`, so the front is where a request acquires a trace id. Copying the rule
there would silence every real request and leave the probes as the only traced traffic.
`front/internal/telemetry/httpfilter.go` says so at the point of temptation.

### Dockerfile

Two-stage, always. Non-root `USER 65532:65532`, matching the chart's
`runAsUser`/`runAsGroup`. `ARG VERSION=dev` into an OCI label and, for Go, into
`-ldflags -X main.version`.

- **Go** — `golang:1.26` build stage, `CGO_ENABLED=0`, `-trimpath`, `-ldflags="-s -w"`;
  runtime stage `gcr.io/distroless/static:nonroot`. Assets are `embed`ed into the
  binary, so nothing but the binary ships.
- **Python** — `python:3.12-slim` for both stages; **not** distroless, which is for
  static binaries and cannot supply an interpreter and its shared libraries. Build a
  venv at `/opt/venv`, `pip install -r requirements.lock` **first** as its own layer,
  then `pip install --no-deps .`. Set `PYTHONUNBUFFERED=1`,
  `PYTHONDONTWRITEBYTECODE=1`, and `HOME=/tmp` because the chart runs with
  `readOnlyRootFilesystem: true`.

**Python dependency changes are a two-step.** The image installs `requirements.lock` and
then `--no-deps .`, so the lock is the only source of what lands in the image. After
editing `pyproject.toml`, run `make lock` (needs Docker) or the image builds green and
dies on `ImportError` at pod start. `make check` guards this via `lock-check`.

### Makefile

All four components implement the same fourteen targets with the same meanings, so
`make -C <component> <target>` works without knowing the language. Verified present in
all four at the sha above:

```
help build fmt lint test check image push lint-chart render-chart run-local
version clean all
```

`make` with no target prints `help`. `fmt` is a **check, never a rewrite**. `check` is
`fmt lint test lint-chart` plus component extras. `VERSION` defaults to
`git describe --tags --always --dirty`.

There is deliberately **no `contract` target**. Contracts are produced by running each
component's image from the outside -- see [Contracts](#contracts) -- so nothing a
component builds or prints exists in order to describe it.

Component-specific targets are allowed and are named so they read as specific:
`run-stdio` (mcp), `venv`/`lock`/`lock-check` (agent, eval), `test-ui` (front),
`run-judge`/`run-now`/`run-serve`/`db-pull` (eval). Go components keep `vet` as an alias
for `lint`.

**A target that belongs to all four goes into all four, or it is not part of the shared
set.**

### Lint and format

Config is per-component, not inherited — there is no root config file.

- Go — `go vet`, `gofmt -l` as a check.
- Python — ruff, `line-length = 100`, `select = ["E", "F", "I", "UP", "B"]`, declared in
  each `pyproject.toml`. Identical in `agent` and `eval`; keep them identical.
- Charts — `helm lint` and `helm template`, both under `make check`.

### Tests

`tests/test_<module>.py` in Python, `<file>_test.go` beside the source in Go. No mock
framework and no generated mocks anywhere in the tree: dependencies are behind small
interfaces and tests pass a hand-written fake, or use `httptest`. Keep it that way.

Front also has `internal/web/*_test.mjs` running against a DOM stub (`domstub.mjs`) via
`make test-ui`, so the UI has tests without a browser in CI.

Test the boundary, not the framework: config validation, error classification, output
truncation, and the filters that decide what is traced all have tests, because each is a
rule someone could plausibly change without noticing what it protected.

### Helm chart

`Chart.yaml` carries `version` and `appVersion` at the same value; `image.tag` defaults
to `.Chart.AppVersion` and an empty tag falls back to it.

Namespace ownership is the one rule that bites: **`mcp/helm` creates the `fathom`
namespace and `agent`/`front` set `namespace.create: false`.** Two releases owning one
Namespace object collide on install. `eval/helm` creates `fathom-eval`, a different
object, so it does not collide.

- `_helpers.tpl` defines the env block **once** for all workloads in the chart. In
  `eval` this is what stops the three workloads disagreeing about `FATHOM_EVAL_DB_PATH`.
- Deployments carry `checksum/config` over the values blocks that matter, so a values
  change rolls the pods. CronJobs need no such annotation: each Job picks up the current
  template when it is created.
- `NOTES.txt` states the dangerous combinations at install time, not just in a README —
  `mcp` warns about cluster-scoped kinds under a namespaced Role; `eval` warns that both
  CronJobs ship suspended and that the fixture image and judge must exist.
- Every values key carries a comment explaining *why* that default, not what the key is.
- Pod security is identical everywhere: `runAsNonRoot`, `65532:65532`,
  `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`,
  `readOnlyRootFilesystem: true`, all capabilities dropped.
- Every chart ends with the same boilerplate tail: `extraEnv`, `resources`,
  `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels`,
  `podSecurityContext`, `securityContext`. Copy it verbatim.

---

## Decisions

### Ports and addressing

| Workload | Reached at |
|---|---|
| `fathom-front` | the lab's shared gateway, `http://192.168.56.10:30880/fathom/` — a `VirtualService`, prefix rewritten to `/` |
| `fathom-agent` | ClusterIP `:8080` |
| `fathom-mcp` | ClusterIP `:8080`, path `/mcp` |
| `fathom-eval-serve` | ClusterIP `:8080` — **never a NodePort** |
| Ollama (agent) | host `:11434`, outside the cluster |
| Ollama (judge) | host `:11435`, CPU-only |

Container port is `8080` and the Service type is `ClusterIP` everywhere. In-cluster
callers use the Service FQDN, never a node address — dialling one from inside the
cluster adds a dependency on one node for nothing.

**There are no NodePorts.** The lab's contract routes every tenant through one shared
gateway, told apart by path prefix, and a NodePort needs the lab owner's approval. Only
`fathom-front` is exposed, under `/fathom/`. Anything a browser loads must therefore use
relative URLs — a root-absolute one leaves the prefix. Reaching another service from the
workstation is a `kubectl port-forward`.

`fathom-eval-serve` would stay ClusterIP in any case, because
its API is unauthenticated and returns `results.answer` verbatim — including the planted
canary when the `secret-bait` fixture catches a leak.

### Error envelopes and status codes

A failure names what failed and what the caller should do about it. `fathom-mcp`
classifies every tool failure onto **seven error classes**, shared by the Kubernetes and
Prometheus paths; those classes are the metric dimension, and the detail is a span
attribute. Invalid PromQL comes back as Prometheus's own positioned parse error rather
than a generic refusal, because that is what lets the model correct itself next turn.

Timeouts nest so the **innermost** failure is the one reported, and each layer's budget
exceeds the one inside it:

```
agent.remediation.timeout (45s)
  < agent.requestTimeout (180s)          the real budget; returns 504 "turn exceeded 180s"
    < front agent.timeout (200s)         headroom so the agent's message wins
    < eval.requestTimeout (240s)         so the agent's 504 is stored, not a client abort
      < cronjob.softDeadlineSeconds (4h) harness stops and tears fixtures down
        < cronjob.activeDeadlineSeconds (5h)  hard SIGKILL backstop
```

Changing one means changing its neighbour. This is written down in three READMEs
because getting it backwards produces a true but useless error message.

### Gating an optional dependency

The established pattern, used twice: **with no URL there is no client, no route, and no
advertised capability.** `prometheus.url` empty means the three metrics tools are absent
from `tools/list`; `eval.url` empty means `fathom-front` has no `/api/eval` route and no
eval tab. A browser learns which gates are open from `GET /api/features`.

The reasoning is the same both times: a capability that fails on every call is worse
than an absent one — the caller spends a turn discovering it and reads the failure as an
outage rather than a deployment choice. Adding the URL is a values change and a rollout,
never a code change. **Gate a new optional dependency this way.**

### Contracts

`contracts/<component>/` is generated and must never be edited by hand. It is what a
caller reads instead of the callee's source -- the interface is the only part of a
sibling most work actually needs, and reading three services to change one is the cost
`cross-service-work` exists to avoid.

The rule the whole directory is built around:

> **No component contains a line of code that exists to produce a contract.**

No `contract` target, no `-contract` flag, no module whose job is to describe its
service. A contract assembled from declarations sitting beside the code is a second
source of truth, and a second source of truth drifts. One taken off the wire *is* the
interface, because it is the same bytes a caller receives.

`scripts/contracts.py` builds each component's image, starts it in a throwaway container
configured the way its own chart configures it, and scrapes it:

| Component | Scraped how |
|---|---|
| `agent` | `GET /openapi.json` -- FastAPI publishes it already |
| `mcp` | `initialize` + `tools/list` over streamable HTTP, twice: `prometheus.url` unset, then set |
| `eval` | `GET /openapi.json`, plus the applied DDL read out of `sqlite_master` in a database the image builds for itself |
| `front` | nothing -- see below |

`helm template` supplies what a process cannot know about itself: namespace, Service
type, nodePort, RBAC, probes. Published per component: `service.json`, plus
`openapi.json` / `mcp-tools.json` / `storage.json` + `schema.sql` +
`fixture-namespaces.json` as that component actually has them. `front` is the only one
with no interface file of its own.

**The container run is also the port check.** The sandbox runs the component with its
chart's own environment and dials its chart's own `containerPort`. The comparison it
replaced -- a binary's *default* listen port against `service.port` -- asserted a
coincidence rather than a requirement: every Deployment here sets `FATHOM_<C>_ADDR` from
`service.port`, so the code default never applies in the cluster, and the check would
have failed a chart that legitimately published a non-default port while never
establishing that the process listens anywhere in particular. Verified: misspell the
env var name and move `service.port` off the code default, and the run fails naming the
port nothing answered on.

**Docker is required**, because this runs the services -- except for the half that a
venv can serve. FastAPI publishes `/openapi.json` itself, so without a daemon
`make contracts` still regenerates every `service.json` (that is `helm template`) and
the full `agent` and `eval` sets, by starting them with `python -m` from their own venvs
and scraping the identical endpoint; the output is byte-identical to the container run.
`mcp-tools.json` and both `containerPort` checks need a container, so they are named on
stderr and left alone -- never deleted, since an absent file reads as a removed
interface. `make contracts-check` refuses the fallback outright: it is the drift gate,
and one that silently checks less than it claims is worse than one that refuses.
`.githooks/pre-commit` therefore regenerates what it can rather than skipping outright.

Three rules for a new component or a new interface:

1. **Add nothing to the service in order to describe it.** If the interface is not
   already reachable from outside a running container, that is a fact about the
   interface, not a gap to fill with a describe-myself module.
2. **Prefer a framework that publishes its own interface.** This is the one legitimate
   way to change a service to get a contract, because what you are changing is how it
   serves, not adding something that only describes. `fathom-eval-serve` moved from
   aiohttp to FastAPI on 2026-08-23 for exactly this: it is the only pure JSON API in
   the repo, its responses cross a namespace boundary into `evals.js`, and aiohttp
   published no description of any of it. The cost was three wheels in a batch image.
   A `response_model` is serialisation, not documentation -- but it must never *narrow*
   a payload, so every one of them carries `extra="allow"` and a test that drives a real
   request and compares what the client received against what the layer below produced
   (`test_response_models_drop_nothing`, `test_chat_response_model_drops_nothing`).
3. **What still cannot be scraped is documented by hand and pinned by a test that dials
   it.** `fathom-front` is the only case left: Go's `ServeMux` does not report what was
   registered with it, and the binary serves a UI rather than an API, so a framework
   swap would buy nothing. Its route table lives in `integration-notes.md`, guarded by
   `TestDocumentedRoutesAreServed`, which dials every documented row through the real
   mux. A hand-written route list with no guard is the second source of truth this repo
   keeps avoiding; one with a guard is a test fixture that happens to be readable.

`integration-notes.md` is the exception to all of it: hand-written, created once from a
stub and never touched again. Retry semantics, timeout nesting, idempotency and ordering
cannot be expressed by a spec and are what cause incidents.

`integration-notes.md` is also where the prose that used to be declared in source now
lives: a service's dependencies, its retry semantics, its timeout nesting.

Regenerate with `make contracts` (or `COMPONENT=<name>`). `make contracts-check` fails
on drift and is wired into the root `make check`. `make hooks` installs
`.githooks/pre-commit`, which regenerates the components a commit touches and includes
the result in that commit -- there is no CI here, so a commit is the only moment a
generated artifact can be held to the code it describes. It is not a cheap hook: it
builds an image per touched component, which Docker's layer cache makes seconds in the
common case and a minute or more cold.

### Two-file agreements

The places where one setting lives in two files. Everything else has exactly one home.

| Setting | Source of truth | Must match |
|---|---|---|
| Model and alias | `ollama/config.env` | `agent/helm` → `ollama.model` |
| Ollama endpoint | `ollama/config.env` | `agent/helm` → `ollama.url` |
| Judge model and endpoint | `ollama/config.env` judge block | `eval/helm` → `judge.model`, `judge.url` |
| Turn budget | `agent/helm` → `agent.requestTimeout` | `front/helm` → `agent.timeout` must exceed it |
| Context window | `ollama/config.env` → `FATHOM_NUM_CTX` | `agent.maxToolChars`, `k8s_log` caps |
| Eval read service address | `eval/helm` → `serve.service.port` + release name | `front/helm` → `eval.url` |
| Fixture namespaces | `eval/src/fathom_eval/fixtures/*.yaml` | `eval/helm` → `rbac.fixtureNamespaces` |
| What the agent may read | `mcp/helm` → `k8s.allowedResources` | nothing — it generates the RBAC itself |

Model serving parameters (`num_ctx`, `temperature`, `top_p`) are pinned in the alias's
Modelfile by `ollama/setup.sh` and are deliberately **absent** from every chart. Adding
them to a chart splits one source of truth in two.

### Service-to-service calls

`fathom-front` → `fathom-agent` over HTTP; `fathom-agent` → `fathom-mcp` over MCP
streamable HTTP; `fathom-eval` → `fathom-agent` over HTTP, as an ordinary client. No
service mesh, no mTLS, no auth between services — the cluster is the trust boundary, and
`fathom-eval-serve` staying ClusterIP is part of that.

Trace context propagates on every real call via `tracecontext,baggage`. The one
deliberate exception is the agent's readiness check, which opens a full MCP session
without a span and would otherwise cost eight spans every probe.

**Nothing holds conversation state.** `/chat` is stateless and the transcript lives in
the browser; the eval harness sends a fresh `/chat` per question. A feature needing
server-side session state is a design change, not an implementation detail.

### Release and deploy

The root `Makefile` cuts one version across all four components at once:
`make release BUMP=patch` — check, bump, commit and tag locally, build and push the
images. A component joins by appending to `COMPONENTS` and `CHARTS`.

**Nothing in the repository deploys.** The lab is deployed by Argo CD from `bifrost`:
one `Application` per chart in `deploy/applications.yaml`, automated sync with prune and
self-heal. A merged commit is the rollout, and the chart's `appVersion` is the image
tag, so a release is the bump commit landing — after its images were pushed. No
`helm install/upgrade`, no `--set`, no hand edits: Argo CD reverts them.
[`deploy.md`](deploy.md) is the full procedure.

**There is no deploy order.** Each service reports not-ready until the one it dials
answers (front → agent → mcp), so the three converge however they are synced.

**`eval` has no Application.** It needs a second namespace and cluster-scoped objects
the lab owner has not been asked for. It is built and versioned, not deployed.

A component joins the deployment by adding an `Application` to
`deploy/applications.yaml` — which a human then re-applies — and must fit the lab's
contract: everything in the `fathom` namespace, nothing cluster-scoped without
approval, CPU and memory requests and a memory limit on every container, images public
and pinned.

### Storage and data

SQLite on a PVC, in `eval` only. `local-path` is `WaitForFirstConsumer` and node-pinned.
There is no migration tool: the schema carries a version, `/meta` serves it, and
`reporting.connect_ro` opens `mode=ro` with `PRAGMA query_only` and refuses to create
the file rather than conjuring an empty database that would shadow the real one.

The volume is mounted **read-write** even though the connection is read-only, and that
is not an oversight: the database is WAL, and a read-only open may need to create the
`-shm` index, which needs write permission on the directory. The read-only property
lives in the connection, where it is tested.

`persistence.keep: true` puts `helm.sh/resource-policy: keep` on the PVC, so
`helm uninstall` does not take every night of history with it.

### Secrets

There are none, and there is no secret store. Nothing in the repo reads a Secret —
`fathom-eval` can `create` and `delete` secrets but deliberately cannot `get` them.
Credentials, if one is ever needed, are a design decision to raise, not a pattern to
copy from here.

---

## If you are adding a component

In order:

1. Copy the nearest sibling's skeleton — `eval/` for Python, `front/` for Go — and
   rename per [Naming](#naming). Use the directory-named Go module path.
2. Implement the fourteen shared `make` targets before anything else. They are how
   everything else in the repo invokes you. `contract` is the one with a consumer
   outside your own directory -- see [Contracts](#contracts).
3. Write `helm/`, including `NOTES.txt`. Set `namespace.create: false` if you live in
   `fathom` — Argo CD creates that namespace.
4. Wire telemetry at construction, and exclude your probes.
5. Append to `COMPONENTS` and `CHARTS` in the root `Makefile`, and add an `Application`
   to `deploy/applications.yaml`.
6. Write `<component>/README.md` with the sections every other one has: how it fits
   together, prerequisites, build and push, install, verify, the API if it has one,
   **a Helm value table**, telemetry, running outside the cluster, troubleshooting,
   upgrade and uninstall.
7. Run `make contracts` and commit `contracts/<component>/`, then fill in its
   `integration-notes.md` — the generator creates that file once and never touches it
   again, so an empty one stays empty.
8. Update root `README.md` and `CLAUDE.md`. Both enumerate components in several places
   — the counts drift silently otherwise, which is exactly what the
   2026-08-22 documentation audit found.

**Everything in [Template material](#template-material) is copyable, which means this
document is a workaround for a generator that does not exist.** A `templates/` skeleton
plus a `scripts/new-service` would make steps 1–3 mechanical and make this section
unnecessary. That is the real fix.

## Maintaining this file

`generated-from` is the sha this was verified against. Before relying on it:

```sh
git log --oneline eebf9d27bd6a24d688dd283b406f86f6a9f5bc1f..HEAD -- agent/ mcp/ front/ eval/
```

If the components have moved substantially, refresh the affected sections rather than
working against a stale spec.

The sha above is a parent commit, not this file's own -- a file cannot name the commit
that contains it. Expect it to trail by one.

The mechanical half of that problem is now solved elsewhere: `contracts/` is regenerated
from the services themselves, by `make contracts` and by the pre-commit hook, and
`make contracts-check` fails on drift. **This file is the half that is still written by
hand** -- it holds the reasoning, the prohibitions and the shapes you copy, none of which
a generator can produce. When the two disagree about a mechanical fact, `contracts/` is
right, because it ran the code.
