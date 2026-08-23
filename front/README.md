# Deploying fathom-front

`fathom-front` is Fathom's UI: a static page served from a small Go binary, with
`/api/chat` reverse-proxied to `fathom-agent`. It exists because the agent answers over
`POST /chat` and nothing in the repo held a conversation with it — the endpoint is
stateless, so multi-turn needs a client that keeps the transcript and replays it.

It has a second view, for inspecting what the evaluation harness measured, with
`/api/eval/*` reverse-proxied to `fathom-eval`'s read service. That view is **off unless
`eval.url` is set** — see *The eval view*.

This document covers deploying **this component only**. The agent, the MCP server and the
host-side Ollama server are separate deliverables; see `../agent/README.md`,
`../mcp/README.md` and `../ollama/`.

## How it fits together

```
    browser
       |  NodePort 30081
       v
  +---------------+   /api/chat -> /chat   +---------------+   MCP over HTTP   +--------------+
  | fathom-front  |----------------------->| fathom-agent  |------------------>|  fathom-mcp  |
  | (this chart)  |                        |               |                   |              |
  +---------------+                        +---------------+                   +--------------+
       |                                           |
       |                                           | HTTP, across the host boundary
       |                                           v
       |                                +----------------------------+
       |                                | Ollama on the HOST machine |
       |                                +----------------------------+
       |
       | /api/eval/* -> /*      optional: only when eval.url is set
       v
  +-----------------------------------------+
  | fathom-eval-serve                       |
  | namespace fathom-eval, ClusterIP        |
  | reads results.db -- no RBAC, no token   |
  +-----------------------------------------+
```

Four things follow from that picture, and they are most of what this component is.

- **The page and the API share an origin.** The browser never dials the agent. That is why
  `fathom-agent` needs no CORS middleware and could drop its own NodePort entirely: the only
  thing that talks to it is a pod in the same namespace.
- **This service holds nothing.** No Kubernetes credentials (`automountServiceAccountToken:
  false`, no Role, no binding) and no conversation state. Every cluster read still goes
  through `fathom-mcp`, and the transcript lives in the browser.
- **A turn blocks.** The agent does not stream — it runs the whole tool-calling loop and then
  returns one JSON body, typically 30–180 s later. Every timeout in this component is sized
  around that, and the page counts the seconds out loud so the wait does not look like a hang.
- **The second hop is optional and independent.** `fathom-eval-serve` lives in another
  namespace and is reached only when `eval.url` is set. It is not a dependency: `/readyz`
  ignores it entirely, so the eval view being down never takes the chat with it.

## Prerequisites

- `fathom-agent` deployed and its `/readyz` returning 200 (`../agent/README.md`)
- `helm` 3.x and a kubeconfig pointing at the target cluster
- Docker, and push access to `registry.lab:5000`

## 1. Build and push the image

From `front/`:

```sh
make check                    # gofmt, go vet, go test, UI checks, helm lint + template
make contract                 # this component's published interface, as JSON
make help                     # the full target list
make push VERSION=0.5.1
```

The tag matters: the chart defaults `image.tag` to `.Chart.AppVersion`. `make image` without
`VERSION` derives a tag from `git describe`, which will not match — pass
`--set image.tag=<tag>` at install time in that case.

The registry notes in `../mcp/README.md` apply unchanged here: the insecure-registry
configuration, and the `ctr -n k8s.io images import` side-load fallback if the registry is
unreachable from a node.

The UI is compiled into the binary with `embed`, so the image is the binary and nothing else —
there is no asset directory to mount and no ConfigMap to keep in step with the code.

## 2. Install the chart

Like the agent chart, this one defaults `namespace.create=false` — `fathom` already exists and
two releases both declaring it collide.

```sh
helm install fathom-front ./helm
```

Preview without touching the cluster:

```sh
helm template fathom-front ./helm
helm install fathom-front ./helm --dry-run
```

## 3. Verify

```sh
kubectl -n fathom rollout status deploy/fathom-front

# NodePort 30081, on the same 192.168.56.0/24 libvirt network as this workstation.
NODE=192.168.56.10
open http://$NODE:30081
```

Any node's IP works — `kube-proxy` forwards the port from every node regardless of where the
pod landed. If you would rather not expose it, set `service.type=ClusterIP` and tunnel:

```sh
kubectl -n fathom port-forward svc/fathom-front 8082:8080 &
open http://localhost:8082
```

The probes:

```sh
curl -s  http://$NODE:30081/healthz   # this process is up
curl -si http://$NODE:30081/readyz    # the AGENT is ready, in the agent's own words
```

`/readyz` is the one worth trusting, and it is deliberately not about this service. It calls
the agent's `/readyz` and forwards the result verbatim, so a 503 here still says
`ollama unreachable at ...` or `mcp server unreachable` — the failure that actually matters
is two hops away, and inventing a vaguer sentence for it would lose that. The consequence is
that this pod stays out of its Service until the agent can answer, rather than serving a chat
page that fails on the first question.

Confirm it holds no cluster permissions:

```sh
kubectl auth can-i --list --as=system:serviceaccount:fathom:fathom-front
```

## Using it

- **New chat** starts a fresh transcript. Nothing happens server-side — the agent has no
  session to open, so this is purely a local act.
- **Follow-up questions** replay the prior turns as `history` on the next request. That is the
  only way the agent sees context.
- Each answer carries a collapsed detail line: how long the turn took, how many tools the
  model called, and the trace of those calls with their result sizes and statuses. A step
  showing `status: error` is usually **not** a problem — the MCP server writes its errors as
  advice ("namespace not found"), the model reads them and retries with a better query. It is
  shown in the warning colour rather than the error one for that reason.
- **Stop** aborts the request. The agent keeps working on that turn regardless; there is no
  cancellation path through to the model.

### What the client sends, and what it leaves out

The model's context is 16k tokens (`ollama/config.env`, `FATHOM_NUM_CTX`) and a single tool
result can be 8k characters of it, so replaying an unbounded transcript stops fitting long
before the conversation feels long. The page caps `history` at the last 12 messages and about
24k characters, whichever bites first, and says so in the UI when it drops something — a
silently shortened history changes the answer, which is not a thing to hide.

Failed turns are never replayed: the agent did not produce them, and feeding it its own error
messages as assistant turns teaches it nothing.

## The eval view

A second view, at `#/evals`, over what `eval/`'s nightly harness measured: the run list,
one run's per-fixture and per-check aggregates, and one result with its answer, the fix
the agent proposed, its tool trace, every deterministic check and every judge verdict.

Three things about it.

**It is off by default.** With `eval.url` empty there is no `/api/eval` route on the mux
and no tab on the page — the same gate `fathom-mcp` puts on its Prometheus tools, for the
same reason: a feature that fails on every use is worse than one that is not there. The
page learns which gates are open from `GET /api/features`, because it is embedded in the
binary and cannot read an environment variable.

**`/readyz` deliberately ignores it.** That endpoint decides whether this pod is in its
Service, and an inspection page being unavailable must not take the chat offline with it.
Eval failures render inline, in their own pane.

**It can show a secret.** `results.answer` holds whatever the model read out of the
cluster, and when the harness's `secret-bait` fixture catches a leak, the planted canary
is stored in that column verbatim. Displaying it is the point — it is the evidence that
the leak happened. Bear it in mind before putting this UI anywhere less private than the
lab, and note that the eval service itself is ClusterIP for exactly this reason.

Routing is the URL hash (`#/evals`, `#/evals/runs/12`, `#/evals/results/941`), so deep
links and Back work while the assets stay a plain file server with no SPA fallback.

## Configuration

Every knob is a Helm value; the Deployment turns them into `FATHOM_FRONT_*` environment
variables, the same pattern the other three components use.

| Value | Default | What it does |
|---|---|---|
| `namespace.name` / `namespace.create` | `fathom` / `false` | Where the workload lives; creation is off because the MCP chart owns it |
| `image.repository` / `image.tag` | `registry.lab:5000/fathom-front` / chart appVersion | Image coordinates |
| `service.port` | `8080` | Container port, Service port, and the listen address |
| `service.type` | `NodePort` | `NodePort` so a browser on the host can open it; `ClusterIP` if you prefer a port-forward |
| `service.nodePort` | `30081` | Pinned so the URL survives a reinstall. 30080 is the agent's. Null lets the API server assign one |
| `agent.url` | `http://fathom-agent.fathom.svc.cluster.local:8080` | Base URL only — `/chat` and `/readyz` are the agent's API, not knobs |
| `agent.timeout` | `200s` | Whole-turn budget for the proxied call. **Must exceed the agent's `requestTimeout`** |
| `agent.readyTimeout` | `5s` | Bounds the `/readyz` call into the agent |
| `eval.url` | `""` | fathom-eval's read service. **Empty disables the eval view entirely** — no route, no tab |
| `eval.timeout` | `10s` | Bounds one proxied eval read. Not `agent.timeout`: this is a SQLite query, not a model turn |
| `front.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `front.maxBodyBytes` | `262144` | Cap on an inbound `/api/chat` body |
| `otel.mode` | `otlp` | `otlp`, `console` or `off` — picks the exporter, not its tuning |
| `otel.endpoint` | `http://otel-collector.observability.svc.cluster.local:4317` | Collector address. FQDN because it is another namespace; `http://` selects plaintext gRPC |
| `otel.sampler` | `parentbased_always_on` | This service is the trace **root**, so its decision is the one every downstream span inherits |
| `otel.metricExportInterval` | `60000` | Milliseconds between metric exports |
| `otel.environment` | `lab` | Becomes `deployment.environment.name` on every span and metric |

### Why `agent.timeout` is larger than the agent's own

The agent's `requestTimeout` (180 s) is the real budget, and when it expires the agent returns
a `504` saying `turn exceeded 180s`. If this service timed out first, that message would never
arrive — the browser would get "the agent did not answer in time", which is true but says
nothing about how long it waited or why. The 20 s of headroom exists so the more informative
failure is the one that wins. Raise `agent.requestTimeout` and this has to move with it.

### Why `eval.timeout` is not `agent.timeout`

`agent.timeout` is 200s because a model turn takes minutes. An eval read is a `SELECT`
against a local SQLite file and answers in milliseconds. Sharing one knob would mean a
wedged reader holding a browser connection for three minutes before failing.

## Telemetry

Traces and metrics go to the same collector as the other three components, configured with the
same `OTEL_*` variables and the same `otel:` values block.

`fathom-front` is the **trace root**. A browser sends no `traceparent`, so this is where a
request first acquires a trace id, and the proxy's instrumented transport is what carries it
into the agent — which is what makes one `/chat` a single trace spanning all three services.

That is also the one place this component's telemetry deliberately differs from
`fathom-mcp`'s. The MCP server drops inbound requests carrying no trace context, because its
only client is the agent and an unparented request there is the agent's suppressed readiness
probe. Applying that rule here would silence every real request and trace nothing but the
probes. `internal/telemetry/httpfilter.go` excludes `/healthz` and `/readyz` and nothing else,
and says why.

## Running outside the cluster

The fastest loop, and it needs no image:

```sh
kubectl -n fathom port-forward svc/fathom-agent 8081:8080 &
make run-local
#   -> http://localhost:8082
```

`run-local` listens on `:8082` (8080 is `fathom-mcp`'s own `run-local` and the port-forward
to it, 8081 is the agent's `run-local`) and uses `console` telemetry, so no collector is needed. Point it elsewhere
with `make run-local AGENT_URL=http://...`.

To bring up the eval view too, serve a results database beside it. This needs no agent, no
model and no cluster — pull a real night's results out and read them locally:

```sh
make -C ../eval db-pull
make -C ../eval run-serve LOCAL_DB=./results.db     # :8083
make run-local EVAL_URL=http://localhost:8083
#   -> http://localhost:8082/#/evals
```

`/readyz` will be red without an agent, but readiness is a probe rather than a gate on
serving, so the page loads and the eval view works.

The UI checks run under plain `node` with no package.json and no build step:

```sh
node internal/web/app_test.mjs
node internal/web/evals_test.mjs
```

`make check` runs them, and skips cleanly when node is not installed — the same way
`lint-chart` skips without helm.

## Troubleshooting

**The pod never goes ready** — `/readyz` here is the agent's readiness. Check the agent first
(`kubectl -n fathom get pod`, then its own `/readyz`); this pod is reporting its dependency
honestly, not failing on its own.

**The status dot is red and says `agent unreachable`** — `agent.url` is wrong or the agent's
Service is gone. The message carries the URL it tried.

**A turn returns 502 with `agent unreachable`** — the transport failed mid-call. The agent
crashed or was rolled during the turn; the answer is not recoverable, and Retry re-asks.

**A turn returns 504** — read which one. `turn exceeded 180s` is the agent's own message and
means the model ran long. `the agent did not answer in time` is this service's, and means
`agent.timeout` is set below the agent's `requestTimeout`.

**There is no Evals tab** — that is the gate, not a fault. `eval.url` is empty, so no route
is registered and the page does not offer the view. `curl -s <front>/api/features` returns
`{"eval":false}` and confirms it. Set `eval.url` to the read service's in-cluster address;
the eval chart's `NOTES.txt` prints it.

**The Evals tab says "No results yet"** — also not a fault. The harness has never completed
a run, so `/data/results.db` does not exist. Both CronJobs ship suspended; `eval/README.md`
says how to start one, and `make -C eval run-now` fires one immediately.

**Evals says `eval service unreachable`** — the hop from this pod to `fathom-eval-serve`
failed. Check `serve.enabled` is true in the eval chart and that the Deployment is ready
(`kubectl -n fathom-eval get deploy fathom-eval-serve`). Note the message names the eval
service specifically, so it cannot be confused with an agent failure.

**A turn returns 413** — the transcript outgrew `front.maxBodyBytes`. Start a new chat; the
client cap should normally prevent this, so seeing it means a single question was enormous.

**Answers arrive but the page looks stale after an upgrade** — it should not: assets are served
`no-store` precisely so a rolled binary is never paired with a cached `app.js`. A hard reload
rules it out.

**The trace stops at fathom-front** — check `otel.mode` is not `off` on this chart. Propagation
survives `off` (the propagator is installed unconditionally) but the outbound transport is only
instrumented when telemetry is on, so the agent's span becomes a new root.

## Upgrade and uninstall

```sh
helm upgrade fathom-front ./helm --set image.tag=0.5.2
helm uninstall fathom-front
```

Changes to `agent.*`, `front.*` or `otel.*` roll the pods automatically — the Deployment
carries a `checksum/config` annotation over those blocks.

Note that `make release` at the repository root does all of this for all four components at
one version; this chart is wired into it as `deploy-front`, rolled last because its readiness
depends on the agent's.

## The published contract

`contracts/front/` describes this service's interface, and is **generated** — never
edit it. `make contract` here prints what this component knows about itself; the writer
in `scripts/contracts.py` merges in the chart's view and cross-checks the two, so a
default listen port that stops matching `service.port` fails rather than shipping.

```sh
make contract              # this component's contract, as JSON on stdout
make -C .. contracts COMPONENT=front
make -C .. contracts-check # fail on drift; also part of the root `make check`
```

`make -C .. hooks` installs the pre-commit hook that regenerates it for you.
`contracts/front/integration-notes.md` is the one file in there written by hand.
