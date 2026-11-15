# fathom-mcp

`fathom-mcp` is the MCP tool server. It exposes three read-only Kubernetes tools
(`k8s_get`, `k8s_describe`, `k8s_log`) over streamable HTTP, and it ships the
ServiceAccount and RBAC that decide what the agent is allowed to see.

Three further read-only tools (`list_metrics`, `describe_metric`, `run_promql`)
are backed by Prometheus rather than the Kubernetes API. They are registered
only when `prometheus.url` is set, which the chart does by default — see
[Metrics tools](#metrics-tools).

This document covers **this component only**. How Fathom as a whole gets onto the
lab — Argo CD, the image registry, releasing — is in
[`../docs/deploy.md`](../docs/deploy.md).

## Prerequisites

- Docker (or another OCI builder) to build the image
- `helm` 3.x, for `make lint-chart` and `make render-chart` only — nothing here
  installs with it
- `kubectl` with the `spellcore-k8s-lab` context, for looking

## 1. Build the image

From `mcp/`:

```sh
make check                 # gofmt, go vet, go test, helm lint + template
make image VERSION=0.5.4
```

This produces `ghcr.io/salian14/fathom-mcp:0.5.4`. The registry comes from
`REGISTRY` (default `ghcr.io/salian14`); override it, or `IMAGE` outright, to
build for somewhere else. `make image` without `VERSION` derives the tag from
`git describe` (e.g. `8957f22-dirty`), which no chart names — useful locally,
never deployed.

## 2. Push the image

```sh
docker login ghcr.io
make push VERSION=0.5.4
```

`push` depends on `image`, so it builds first and pushes exactly the tag it
built. The package must be public on GHCR for the lab's nodes to pull it.

**Never overwrite a tag.** The chart pins `image.tag` to its `appVersion` and
`image.pullPolicy` is `IfNotPresent`: a re-pushed tag changes nothing in git, so
Argo CD deploys nothing, and nodes keep the image they already have. A new image
is a new version — `make release` at the repo root.

## 3. Deploy

There is no install step in this directory. Argo CD renders `mcp/helm` from the
`bifrost` branch as the `fathom-mcp` Application (`../deploy/applications.yaml`)
and keeps the `fathom` namespace matching it; a merged commit is the deployment.

The chart pins every resource to `namespace.name` (default `fathom`) and does
not create that namespace — Argo CD does (`CreateNamespace=true`).

Preview what Argo CD will apply, without touching the cluster:

```sh
make render-chart          # helm template fathom-mcp ./helm
```

## 4. Verify

```sh
kubectl --context spellcore-k8s-lab -n argocd get application fathom-mcp   # Synced / Healthy
kubectl --context spellcore-k8s-lab -n fathom rollout status deploy/fathom-mcp
```

Health and MCP endpoint, via port-forward:

```sh
kubectl -n fathom port-forward svc/fathom-mcp 8080:8080 &
curl -s localhost:8080/healthz   # process is up
curl -s localhost:8080/readyz    # process is up AND the API server answers
```

`/readyz` calls the Kubernetes API with the pod's own credentials, so a 200
there means the ServiceAccount token and CA are mounted and working — that is
the check worth trusting.

Confirm the grant is what you intended, not what you assumed:

```sh
kubectl auth can-i --list --as=system:serviceaccount:fathom:fathom-mcp
```

Every verb in that list should be `get` or `list` (plus `get` on `pods/log`).
Anything else is a bug in the chart, not a local misconfiguration.

The in-cluster address for the agent to dial:

```
http://fathom-mcp.fathom.svc.cluster.local:8080/mcp
```

## Configuration

Every knob is a Helm value; the Deployment turns them into `FATHOM_MCP_*`
environment variables. The ones that matter at deploy time:

| Value | Default | What it does |
|---|---|---|
| `namespace.name` / `namespace.create` | `fathom` / `true` | Where the workload lives, and whether the chart creates it |
| `image.repository` / `image.tag` | `ghcr.io/salian14/fathom-mcp` / chart appVersion | Image coordinates; an empty tag falls back to the chart's `appVersion` |
| `service.port` | `8080` | Container port, Service port, and the listen address |
| `server.transport` | `http` | `http` (streamable HTTP, what the chart deploys) or `stdio`, which is for `make run-stdio` and has no meaning in a pod |
| `server.path` | `/mcp` | HTTP path the MCP endpoint is served on |
| `server.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `k8s.allowedResources` | 18 types | Both the tool allowlist **and** the RBAC rules |
| `k8s.allowedNamespaces` | `[]` (any) | Server-side namespace restriction, applied before the API call |
| `k8s.defaultNamespace` | `default` | Used when the model omits a namespace |
| `limits.*` | see `values.yaml` | Result size caps: `maxBytes`, `listLimit`, log tail lines/bytes, request timeout, and the metrics/PromQL bounds |
| `rbac.namespaces` | `[]` | Non-empty swaps the ClusterRole for per-namespace Roles |
| `prometheus.url` | `http://prometheus-server.observability.svc.cluster.local:80` | Prometheus HTTP API root. Empty means the three metrics tools are not registered |
| `prometheus.timeout` | `15s` | Bounds one Prometheus call, and is sent to Prometheus as its own `timeout` |
| `otel.mode` | `otlp` | `otlp`, `console` or `off` — picks the exporter, not its tuning. See [Telemetry](#telemetry) |
| `otel.endpoint` | `http://otel-collector.observability.svc.cluster.local:4317` | Collector address. FQDN because it is another namespace; `http://` selects plaintext gRPC |
| `otel.sampler` | `parentbased_always_on` | Parentbased matters most here: this service is always a child of the agent's span, and disagreeing with the parent would produce half-traces |
| `otel.metricExportInterval` | `60000` | Milliseconds between metric exports |
| `otel.environment` | `lab` | Becomes `deployment.environment.name` on every span and metric |

### `k8s.allowedResources` drives two things at once

That one list becomes `FATHOM_MCP_ALLOWED_RESOURCES` (what the tools will
accept) *and* the ClusterRole rules (what the API server will allow). That
coupling is deliberate — it is what keeps the tool surface and the RBAC in
lockstep. Adding a resource kind is an edit to `k8s.allowedResources` in
`helm/values.yaml`, committed — never a code change, and never a `--set` against
the cluster, which Argo CD would revert:

```yaml
k8s:
  allowedResources: [pods, services, nodes, deployments.apps]
```

Entries are `resource` or `resource.group`, lowercase, no slashes.
`pods/log` is granted automatically by the chart whenever `pods` is listed.

Verbs are hardcoded to `get` and `list` in `helm/templates/_helpers.tpl` and are
not parameterised. Widening past read verbs ends the Phase 1 safety property, so
it takes a deliberate template edit and an explicit decision — not a values change.

### Metrics tools

`list_metrics`, `describe_metric` and `run_promql` query Prometheus over its HTTP
API. They are **not registered at all** unless `prometheus.url` is set. The chart
ships it pointed at the lab's Prometheus, whose Service listens on port 80 rather
than 9090:

```yaml
# what values.yaml already sets
prometheus:
  url: http://prometheus-server.observability.svc.cluster.local:80

# elsewhere, or to turn the three tools off again
prometheus:
  url: ""
```

The gate is deliberate. A tool that fails on every call is worse than an absent
one — the model has to spend a turn discovering it, and the failure reads to it
like a cluster problem rather than a deployment choice. With no URL the model
simply sees three tools instead of six.

Some consequences worth knowing:

- **No RBAC change goes with this.** Prometheus is not the Kubernetes API, so
  `k8s.allowedResources` and the ClusterRole are untouched and the read-only
  boundary is unaffected. What the tools can see is whatever that Prometheus
  scraped, which is a separate question from what the ServiceAccount can read.
- **Prometheus is not in the readiness probe.** `/readyz` covers the Kubernetes
  API only, so a Prometheus outage degrades the metrics tools and leaves the
  rest of the server serving. Turning a metrics outage into a total outage would
  be the wrong trade.
- **`run_promql` is instant-query only** (`/api/v1/query`). Rates and rollups
  still work inside a single query — `rate(x[5m])` — but there is no
  `query_range`, so nothing returns a time series to plot.
- **Results are bounded twice**: `limits.promqlMaxSeries` caps series before
  `limits.maxBytes` caps bytes, and both announce themselves in the output so
  the model does not read a truncated answer as a complete one.

Invalid PromQL comes back as Prometheus's own positioned parse error
(`parse error at char 8: unexpected ")"`) rather than a generic refusal — that
is what lets the model correct its own query on the next turn.

### Restricting scope

Two independent limits, worth understanding as separate things:

- `k8s.allowedNamespaces` — enforced by the server, before it makes a request.
- `rbac.*` — enforced by the API server, regardless of what the server does.

Setting `rbac.namespaces` replaces the ClusterRole with a Role and RoleBinding
in each listed namespace. A Role cannot grant cluster-scoped types, so if you do
this, drop `nodes`, `namespaces`, and `persistentvolumes` from
`k8s.allowedResources` — otherwise those tools are advertised to the model and
then fail with a permission error at call time. The chart's `NOTES.txt` warns
about exactly this combination, for anyone who installs the chart by hand elsewhere.

## Upgrade and removal

Upgrading is a version bump merged to `bifrost` (`make release` at the repo
root); Argo CD rolls the Deployment. Removal is deleting the `fathom-mcp`
Application, whose finalizer takes the ClusterRole and ClusterRoleBinding with
it. Both are in [`../docs/deploy.md`](../docs/deploy.md).

Note that changes to `k8s.allowedResources` roll the pods automatically: the
Deployment carries a `checksum/config` annotation over the `k8s`, `server`,
`limits`, `otel` and `prometheus` value blocks.

## Troubleshooting

**`ImagePullBackOff`** — the node cannot get the image.
`kubectl -n fathom describe pod` carries the real reason. The usual two: the
tag the chart names was never pushed (`docker manifest inspect
ghcr.io/salian14/fathom-mcp:<appVersion>` from the workstation), or the GHCR
package is still private.

**Pod runs but `/readyz` returns 503** — the process is up and the API server
is not answering it. `kubectl -n fathom logs deploy/fathom-mcp` will carry a
`readiness check failed` warning with the underlying error. Usually the
ServiceAccount was not created or the binding did not land; `kubectl auth can-i
--list --as=system:serviceaccount:fathom:fathom-mcp` distinguishes an RBAC
problem from a connectivity one.

**A tool refuses a resource type** — the server rejected it against
`FATHOM_MCP_ALLOWED_RESOURCES` before ever calling the API. Add the type to
`k8s.allowedResources` and upgrade; that fixes the allowlist and the RBAC
together.

**The metrics tools are missing from `tools/list`** — `prometheus.url` is empty.
The startup log line carries `prometheus_url` and
`metrics_tools` for exactly this: `kubectl -n fathom logs deploy/fathom-mcp | head -1`.
Set the value and upgrade; the Deployment's `checksum/config` covers the
`prometheus` block, so the pod rolls on its own.

**Metrics tools all fail with a connection error** — the URL is set but wrong or
unreachable from the `fathom` namespace. The pod is still `Ready`, because
Prometheus is deliberately not part of the readiness probe. Check it from inside
the cluster rather than from your laptop:

```sh
kubectl -n fathom exec deploy/fathom-mcp -- \
  wget -qO- "$FATHOM_MCP_PROMETHEUS_URL/-/healthy"
```

**Pod will not start at all** — config validation is fail-fast, and the error
names the offending variable (e.g. `FATHOM_MCP_LOG_TAIL_LINES (500) exceeds
FATHOM_MCP_LOG_MAX_TAIL_LINES (200)`). Read the first line of the logs.

## Running outside the cluster

For local development against your kubeconfig, no cluster deploy involved:

```sh
make run-local   # HTTP transport on :8080, uses $KUBECONFIG or ~/.kube/config
make run-stdio   # stdio transport, same credentials
```

`run-local` is the target every component has, and here it serves the same HTTP
transport the agent pod dials — so `make -C agent run-local` reaches it with no
port-forward. `run-stdio` is the component-specific one, for driving the server
from an MCP client on this machine.

In-cluster credentials are always tried first; the kubeconfig path
(`FATHOM_MCP_KUBECONFIG_PATH`) is a fallback for exactly this case.

## Not covered here

The agent and the host-side Ollama server deploy separately; see `agent/README.md`
and `ollama/config.env`. The host is reached at `192.168.56.1` on the node network
(`192.168.56.0/24`) — not libvirt's default `virbr0` at `192.168.122.1`, which the
nodes are not on. `ollama/config.env` is the source of truth for that address;
confirm it with `ip -brief addr show` if the network is ever rebuilt.

## Telemetry

Traces and metrics go to the otel-collector in the `observability` namespace over
OTLP/gRPC. Three outbound and inbound boundaries are instrumented — inbound MCP,
the Kubernetes API, and Prometheus when configured — each by wrapping its
transport once at construction rather than per call. Metric names and PromQL
queries are span attributes only; the error metric's dimensions are the
endpoint kind (`query`, `series`, `metadata`, `label_values`), which is a closed
set. `FATHOM_MCP_TELEMETRY` selects the exporter — `otlp` (default),
`console`, or `off` — and everything else is configured through the standard
`OTEL_*` variables, which the SDK reads directly.

For a local run, `FATHOM_MCP_TELEMETRY=console make run-stdio` prints spans to
**stderr**. That is not incidental: under the stdio transport stdout carries the
JSON-RPC stream, so a span written there would corrupt the protocol.

Three kinds of traffic are deliberately untraced, all of them repeating for the
life of the pod and none of them saying anything a span could add:

- the probes themselves;
- Kubernetes discovery, which client-go issues without a context and which
  could therefore only ever produce root spans;
- inbound MCP requests carrying no trace context. The agent propagates on every
  real call, and the one path where it deliberately does not is its readiness
  check — which opens a full MCP session and would otherwise cost eight spans
  every time it ran.

`tools/call` is exempt from that last rule. It is the actual work and it is low
volume, so keeping it means a propagation failure shows up as unparented root
spans rather than as silence.

## The published contract

`contracts/mcp/` describes this service's interface, and is **generated** — never
edit it.

`scripts/contracts.py` starts this component's image in a throwaway container and
runs a real `initialize` + `tools/list` against it — twice, with `prometheus.url` unset
and then set, because three of the six tools are gated on it. **Nothing in this service
exists to produce that** — no `contract` target, no `-contract` flag.

The chart supplies what a process cannot know about itself — namespace, Service type,
nodePort, RBAC, probes. The sandbox runs the container with the chart's own environment
and dials its `containerPort`, so a pod that would start, pass its probes and be
unreachable through its Service fails here instead.

```sh
make -C .. contracts COMPONENT=mcp   # needs Docker: this runs the service
make -C .. contracts-check              # fail on drift; also part of the root `make check`
```

`make -C .. hooks` installs the pre-commit hook that regenerates it for you.
`contracts/mcp/integration-notes.md` is the one file in there written by hand.
