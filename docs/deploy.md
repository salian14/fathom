# Deploying Fathom to the lab

Fathom is deployed to the spellcore k8s lab by **Argo CD**, from this repository.
There is no deploy command: no `make deploy`, no `helm install`, no `kubectl apply`
of a workload. A commit on `bifrost` is the deployment.

This document is Fathom's side of the lab's
[application developer contract](https://github.com/salian14/spellcore_k8s_lab/blob/bifrost/docs/application-developer-contract.md).
The contract is the authority; clause numbers below (§) refer to it. If the two
disagree, the contract wins and this file is wrong.

## How it works

`deploy/applications.yaml` declares three Argo CD `Application`s, one per chart:

| Application | Chart | What it puts in namespace `fathom` |
|---|---|---|
| `fathom-mcp` | `mcp/helm` | the MCP server, its ServiceAccount, and its read-only ClusterRole + binding |
| `fathom-agent` | `agent/helm` | the agent |
| `fathom-front` | `front/helm` | the UI, and the `VirtualService` that exposes it |

Each tracks the `bifrost` branch with automated sync, `prune` and `selfHeal`. Argo
CD polls every 60 seconds, renders the chart with `helm template`, and makes the
namespace match. A hand edit to anything it manages is reverted within a minute.

There is no ordering between the three and none is needed: each service dials the
one before it (front → agent → mcp) and reports not-ready until that answers, so
they converge on their own.

**`fathom-eval` is not deployed to the lab.** It needs a second namespace, a
ClusterRole that creates and deletes namespaces, and a PriorityClass, each of which
is a request to the lab owner under §4.1 and §4.3. Its chart is still in the
repository and still built and versioned with the rest; it has no Application. The
front's eval view is gated on `eval.url`, which is empty, so there is no tab.

## What has to be true first

1. **The images exist.** The charts pull `ghcr.io/salian14/fathom-<component>` at
   the tag in each chart's `appVersion` (§5.3: a public registry, a pinned tag).
   Argo CD deploys whatever the charts name, so a version whose images were never
   pushed sits in `ImagePullBackOff`. `make release` pushes them before the bump
   commit can be merged; see [Releasing](#releasing). The packages must be public
   on GHCR, or the nodes cannot pull them.
2. **Ollama is up on the host** and reachable from the nodes at the `ollama.url`
   in `agent/helm/values.yaml`. `ollama/setup.sh --verify` checks it. This is
   outside the cluster and outside Argo CD.
3. **The lab passes its preflight** (§5.2). If any of these fails, stop and tell
   the lab owner; do not repair it.

   ```sh
   kubectl --context spellcore-k8s-lab get nodes        # three nodes, all Ready
   kubectl --context spellcore-k8s-lab get sc           # local-path (default)
   kubectl --context spellcore-k8s-lab -n argocd get applications   # the lab's own: Synced / Healthy
   curl -s -o /dev/null -w '%{http_code}\n' http://192.168.56.10:30880/   # 404, or a route
   ```

## Registering, once per lab build

This is the one `kubectl apply` anybody makes by hand, and a human makes it:

```sh
kubectl --context spellcore-k8s-lab -n argocd get applications    # no fathom-* owned by someone else
kubectl --context spellcore-k8s-lab -n argocd apply -f deploy/applications.yaml
kubectl --context spellcore-k8s-lab -n argocd get applications fathom-mcp fathom-agent fathom-front
```

All three should reach `Synced` / `Healthy`. Argo CD creates the `fathom` namespace
itself (`CreateNamespace=true`); no chart renders it.

**After a lab rebuild the Applications are gone**, and everything they deployed with
them (§2.1). That is expected. Run the preflight, then the same `apply` again. If
the Applications are missing and nobody deleted them, assume a rebuild.

## Verifying

```sh
open http://192.168.56.10:30880/fathom/                 # the UI. The trailing slash matters
curl -s http://192.168.56.10:30880/fathom/readyz        # the agent's readiness, forwarded by the front
```

`/readyz` through the front is the one check worth trusting from outside: it is
200 only when the agent answered, and the agent is ready only when Ollama answered
and the MCP server advertised its tools. A 503 names which one failed.

The agent and the MCP server are `ClusterIP` only. To reach them from the
workstation, port-forward — looking is what `kubectl` is for here:

```sh
kubectl --context spellcore-k8s-lab -n fathom port-forward svc/fathom-agent 8081:8080
kubectl --context spellcore-k8s-lab -n fathom port-forward svc/fathom-mcp   8080:8080
```

## Changing what is deployed

Everything is a commit, through the usual PR to `bifrost`:

| To change | Edit |
|---|---|
| a setting | the component's `helm/values.yaml` |
| what the agent can read | `mcp/helm/values.yaml` (`k8s.allowedResources`) — RBAC follows it |
| the running version | nothing by hand — see [Releasing](#releasing) |
| what is deployed at all | `deploy/applications.yaml`, then re-apply it |

Do not use `helm upgrade`, `kubectl edit`, `kubectl set image` or `--set` against
the lab. With `selfHeal` the change is reverted; without it, it is drift that does
not survive a rebuild.

Because Argo CD tracks `bifrost`, **a PR branch is not deployed**. The lab runs what
has been merged, and nothing else.

## Releasing

```sh
make release BUMP=patch       # preflight, check, bump, push images
```

That bumps every chart's `version` and `appVersion`, commits and tags the bump
locally, and builds and pushes the four images to `ghcr.io/salian14` at the new
tag (`docker login ghcr.io` first). It contacts no cluster.

The rollout is the bump commit reaching `bifrost` through its PR: within a minute
of the merge, Argo CD renders the charts at the new `appVersion` and rolls the
Deployments. The order is what makes this safe: the images are pushed before the
commit can be merged, so the tag always exists by the time a node pulls it.

**A workflow that does the push is written but not installed.**
`deploy/workflows/images.yml` builds and pushes any image whose chart version is not
yet on GHCR, on every push to `bifrost`. It sits outside `.github/workflows/` because
agents may not write there; its header says how a human installs it. Once it is
active, `make bump` plus the PR is a whole release and `make push` becomes optional.

Never re-push a tag. Argo CD sees no change in git and deploys nothing, and the
nodes keep the image they already cached (§5.3).

## Exposure

The UI is exposed through the lab's shared Istio gateway, `istio-ingress/lab-gateway`,
by a `VirtualService` in `front/helm` that matches the prefix `/fathom/` and
rewrites it to `/` (§6). Fathom creates no `Gateway` and no `NodePort`.

Being served under a sub-path is why the page requests relative URLs only
(`api/chat`, not `/api/chat`); `TestScriptsRequestRelativeURLs` in `front/` pins
that. The pods are not in the mesh: the namespace carries no `istio-injection`
label, so there are no sidecars to count against the resource budget.

## Where Fathom needs more than the contract gives by default

| What | Clause | Status |
|---|---|---|
| `fathom-mcp`'s ClusterRole and ClusterRoleBinding (`get`/`list` only) | §4.3 | approved by the lab owner, 2026-11-15 |
| `fathom-mcp` queries the platform Prometheus at `prometheus-server.observability.svc.cluster.local:80` | §10.1 | in-cluster Service of a §3 platform service; raise with the lab owner if in doubt |
| `fathom-eval`: second namespace, namespace-writing ClusterRole, PriorityClass | §4.1, §4.3 | not requested; not deployed |

The contract's table of reserved NodePorts still lists `30080` for Fathom. Fathom
no longer uses it and it can be reclaimed (§11.4).

## Teardown

In the contract's order (§11). Fathom ships no Grafana alert rules, so step 1 is
empty.

```sh
kubectl --context spellcore-k8s-lab -n argocd delete application fathom-front fathom-agent fathom-mcp
kubectl --context spellcore-k8s-lab delete namespace fathom
```

Delete the Applications first: deleting the namespace first just makes Argo CD
recreate it. The finalizer on each Application removes what it deployed, the
ClusterRole and its binding included.
