# Deploying fathom-mcp

`fathom-mcp` is the MCP tool server: it exposes three read-only Kubernetes tools
(`k8s_get`, `k8s_describe`, `k8s_log`) over streamable HTTP, and it ships the
ServiceAccount and RBAC that decide what the agent is allowed to see.

This document covers deploying **this component only**. The agent and the
host-side Ollama server are separate deliverables; see `../CLAUDE.md`.

## Prerequisites

- `helm` 3.x and a kubeconfig whose current context points at the target cluster
- Docker (or another OCI builder) to build the image
- Push access to `registry.lab:5000`, and nodes that can pull from it — see
  [Push the image](#2-push-the-image)

Confirm you are pointed where you think you are before anything else:

```sh
kubectl config current-context
kubectl get nodes
```

## 1. Build the image

From `mcp/`:

```sh
make image VERSION=0.1.0
```

This produces `registry.lab:5000/fathom-mcp:0.1.0`. The registry host comes from
`REGISTRY` (default `registry.lab:5000`); override it, or `IMAGE` outright, to
build for somewhere else. The tag matters: the chart defaults
`image.tag` to `.Chart.AppVersion`, which is `0.1.0`. `make image` without
`VERSION` derives the tag from `git describe` (e.g. `8957f22-dirty`), which will
not match the chart default — in that case pass `--set image.tag=<tag>` at
install time.

Run the gates first if you are about to ship this anywhere:

```sh
make check   # gofmt, go vet, go test, helm lint + template
```

## 2. Push the image

The chart's `image.repository` is `registry.lab:5000/fathom-mcp`, so the nodes
pull from `registry.lab:5000`. Build and push in one step:

```sh
make push VERSION=0.1.0
```

`push` depends on `image`, so it builds first; it pushes exactly the tag it
built. `image.pullPolicy` is `IfNotPresent`, which is fine for immutable version
tags — if you overwrite a tag in place, either bump the version or set
`--set image.pullPolicy=Always` so the node actually re-pulls.

If `registry.lab:5000` is served over plain HTTP, both ends need to be told to
allow it: the daemon doing the push (`insecure-registries` in
`/etc/docker/daemon.json`) and containerd on each node (a
`hosts.toml` under `/etc/containerd/certs.d/registry.lab:5000/` with
`skip_verify = true`). That is node configuration, so it belongs in Ansible
rather than here.

Add `imagePullSecrets` in values if the registry requires authentication.

### Fallback — side-load into containerd

Still workable if the registry is down or unreachable from a node. Build with a
bare name so nothing tries to pull, and match the chart to it:

```sh
make image VERSION=0.1.0 IMAGE=fathom-mcp
docker save fathom-mcp:0.1.0 -o /tmp/fathom-mcp-0.1.0.tar
```

Then, for each node that could schedule the pod (control plane plus both
workers, unless you constrain scheduling with `nodeSelector`):

```sh
# copy the tar to the node, then on the node:
sudo ctr -n k8s.io images import /tmp/fathom-mcp-0.1.0.tar
sudo crictl images | grep fathom-mcp   # verify
```

The `-n k8s.io` is not optional — an image imported into containerd's `default`
namespace is invisible to kubelet and you get `ErrImageNeverPull` /
`ImagePullBackOff` with the image apparently present.

Install with `--set image.repository=fathom-mcp` so the pod uses the side-loaded
image instead of the registry one.

Only one replica runs, so you can skip the copy to every node by pinning the
pod instead:

```sh
--set nodeSelector."kubernetes\.io/hostname"=k8s-worker1
```

## 3. Install the chart

The chart pins every resource to `namespace.name` (default `fathom`) rather
than the release namespace, and creates that namespace itself
(`namespace.create=true`). So install from a namespace that already exists and
let the chart place its own resources:

```sh
helm install fathom-mcp ./helm
```

Release metadata lands in your current namespace (`default` unless you say
otherwise); the ServiceAccount, RBAC, Service, and Deployment land in `fathom`.

Do **not** combine the chart's namespace creation with `helm --create-namespace`
— Helm would create the namespace outside the release and the chart's own
`Namespace` object then collides with it. If you prefer Helm to own the
namespace:

```sh
helm install fathom-mcp ./helm -n fathom --create-namespace \
  --set namespace.create=false
```

Preview the manifests without touching the cluster:

```sh
helm template fathom-mcp ./helm            # render only
helm install fathom-mcp ./helm --dry-run   # render + server-side validation
```

## 4. Verify

```sh
kubectl -n fathom rollout status deploy/fathom-mcp
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
| `image.repository` / `image.tag` | `registry.lab:5000/fathom-mcp` / chart appVersion | Image coordinates; tag falls back to `0.1.0` |
| `service.port` | `8080` | Container port, Service port, and the listen address |
| `server.path` | `/mcp` | HTTP path the MCP endpoint is served on |
| `server.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `k8s.allowedResources` | 18 types | Both the tool allowlist **and** the RBAC rules |
| `k8s.allowedNamespaces` | `[]` (any) | Server-side namespace restriction, applied before the API call |
| `k8s.defaultNamespace` | `default` | Used when the model omits a namespace |
| `limits.*` | see `values.yaml` | Result size caps: `maxBytes`, `listLimit`, log tail lines/bytes, request timeout |
| `rbac.namespaces` | `[]` | Non-empty swaps the ClusterRole for per-namespace Roles |

### `k8s.allowedResources` drives two things at once

That one list becomes `FATHOM_MCP_ALLOWED_RESOURCES` (what the tools will
accept) *and* the ClusterRole rules (what the API server will allow). That
coupling is deliberate — it is what keeps the tool surface and the RBAC in
lockstep. Adding a resource kind is a values change plus `helm upgrade`, never a
code change:

```sh
helm upgrade fathom-mcp ./helm \
  --set-json 'k8s.allowedResources=["pods","services","nodes","deployments.apps"]'
```

Entries are `resource` or `resource.group`, lowercase, no slashes.
`pods/log` is granted automatically by the chart whenever `pods` is listed.

Verbs are hardcoded to `get` and `list` in `helm/templates/_helpers.tpl` and are
not parameterised. Widening past read verbs ends the Phase 1 safety property, so
it takes a deliberate template edit and an explicit decision — not a `--set`.

### Restricting scope

Two independent limits, worth understanding as separate things:

- `k8s.allowedNamespaces` — enforced by the server, before it makes a request.
- `rbac.*` — enforced by the API server, regardless of what the server does.

Setting `rbac.namespaces` replaces the ClusterRole with a Role and RoleBinding
in each listed namespace. A Role cannot grant cluster-scoped types, so if you do
this, drop `nodes`, `namespaces`, and `persistentvolumes` from
`k8s.allowedResources` — otherwise those tools are advertised to the model and
then fail with a permission error at call time. The chart's `NOTES.txt` warns
about exactly this combination on install.

## Upgrade and uninstall

```sh
helm upgrade fathom-mcp ./helm --set image.tag=0.2.0
helm uninstall fathom-mcp
```

Uninstall removes the ClusterRole and ClusterRoleBinding too. It removes the
`fathom` namespace only if the chart created it — and that takes everything else
in the namespace with it, so check before you run it.

Note that changes to `k8s.allowedResources` roll the pods automatically: the
Deployment carries a `checksum/config` annotation over the `k8s` values block.

## Troubleshooting

**`ImagePullBackOff` / `ErrImageNeverPull`** — the node cannot get the image.
`kubectl -n fathom describe pod` carries the real reason. If it is a pull
failure, check the node can reach the registry (`curl -s
http://registry.lab:5000/v2/fathom-mcp/tags/list` from the node) and that
containerd is configured to accept it over HTTP. If you side-loaded instead,
check `sudo crictl images | grep fathom-mcp` on the node the pod landed on
(`kubectl -n fathom get pod -o wide` tells you which one) and confirm you
imported with `-n k8s.io`.

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

**Pod will not start at all** — config validation is fail-fast, and the error
names the offending variable (e.g. `FATHOM_MCP_LOG_TAIL_LINES (500) exceeds
FATHOM_MCP_LOG_MAX_TAIL_LINES (200)`). Read the first line of the logs.

## Running outside the cluster

For local development against your kubeconfig, no cluster deploy involved:

```sh
make run-stdio   # stdio transport, uses $KUBECONFIG or ~/.kube/config
```

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
OTLP/gRPC. `FATHOM_MCP_TELEMETRY` selects the exporter — `otlp` (default),
`console`, or `off` — and everything else is configured through the standard
`OTEL_*` variables, which the SDK reads directly.

For a local run, `FATHOM_MCP_TELEMETRY=console make run-stdio` prints spans to
**stderr**. That is not incidental: under the stdio transport stdout carries the
JSON-RPC stream, so a span written there would corrupt the protocol.

The probes are deliberately untraced, and so is Kubernetes discovery traffic —
`/readyz` alone would otherwise emit a root span every ten seconds for the life
of the pod.
