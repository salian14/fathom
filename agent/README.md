# Deploying fathom-agent

`fathom-agent` is the piece that ties the other two together: a FastAPI service that
drives the host-side Ollama model against the read-only Kubernetes tools
`fathom-mcp` exposes, and answers questions over HTTP.

This document covers deploying **this component only**. The MCP server and the
host-side Ollama server are separate deliverables; see `../mcp/README.md` and
`../ollama/`.

## How it fits together

```
       POST /chat
           |
           v
   +---------------+   MCP over HTTP    +--------------+   client-go   +-----------+
   | fathom-agent  |------------------->|  fathom-mcp  |-------------->| k8s API   |
   | (this chart)  |                    |              |               |           |
   +---------------+                    +--------------+               +-----------+
           |
           | /api/chat, via the fathom-ollama ExternalName Service
           v
   +----------------------------+
   | Ollama on the HOST machine |   outside the cluster
   +----------------------------+
```

Two things follow from that picture, and they are most of what makes this component
awkward:

- **The model is outside the cluster.** A pod cannot reach it as a Service, and
  `localhost` means the pod. The chart ships an `ExternalName` Service so the host
  is reachable under a normal in-cluster DNS name. See
  [The Ollama endpoint](#the-ollama-endpoint).
- **The agent holds no Kubernetes permissions.** Every cluster read goes through
  `fathom-mcp`, whose ServiceAccount carries the RBAC. This chart creates no Role,
  no ClusterRole, and no binding, and the pod runs with
  `automountServiceAccountToken: false`. That is deliberate: it means the read-only
  boundary lives in exactly one place.

## Prerequisites

- `fathom-mcp` deployed and its `/readyz` returning 200 (`../mcp/README.md`)
- Ollama running on the host with the `fathom-agent` alias created
  (`sudo ../ollama/setup.sh`, or `--verify` to check an existing install)
- `helm` 3.x and a kubeconfig pointing at the target cluster
- Docker, and push access to `registry.lab:5000`

## The Ollama endpoint

This is the part worth reading before you install.

`ollama/config.env` is the source of truth for the host's address, and
`setup.sh` prints the block to paste into this chart's values. **Confirm which
interface the cluster's nodes are actually on** before trusting it:

```sh
kubectl get nodes -o wide          # the nodes' INTERNAL-IP
ip -brief addr show                # the host's address on that same network
```

This lab's nodes are on `192.168.56.0/24` and the host is `192.168.56.1`, which is
what `ollama/config.env` now says. Note that `192.168.122.1` — libvirt's *default*
bridge, `virbr0` — is a different network that the nodes are not on, and it is an
easy wrong answer because Ollama answers there too.

**The agent dials that IP directly.** `ollama.url` is `http://192.168.56.1:11434`,
copied from what `setup.sh` prints, and no DNS is involved. The `ExternalName`
Service still exists as a template but ships turned off
(`ollama.service.create=false`).

### Why not the ExternalName Service

Worth knowing, because the Service is the obvious design and it is already written.

An `ExternalName` Service is only a CNAME. `kubectl describe` showing no ClusterIP
and no Endpoints is correct and is not a bug — a CNAME is all it ever produces.
The real problem is that **CoreDNS cannot resolve `.lab`**, so the CNAME lands on
nothing:

```sh
# from the host, libvirt's dnsmasq answers fine:
dig +short @192.168.56.1 localllm.lab       # -> registry.lab. -> 192.168.56.1

# but CoreDNS does not (port-forward avoids needing a throwaway pod):
kubectl -n kube-system port-forward deploy/coredns 15353:53 &
dig +tcp @127.0.0.1 -p 15353 localllm.lab   # -> NXDOMAIN
dig +tcp @127.0.0.1 -p 15353 github.com     # -> NOERROR, so upstream itself is fine
```

CoreDNS forwards to the node's `/run/systemd/resolve/resolv.conf`, and on these VMs
that points at a public resolver, not at libvirt's dnsmasq. `registry.lab` looks like
a counter-example, but it resolves only because it is in each VM's own `/etc/hosts`
and **kubelet** resolves image references — CoreDNS never reads a node's hosts file,
so no pod can resolve it. A pod that asks for the Service gets a CNAME with no A
record, which surfaces in the agent log as:

```
readiness check failed: ollama unreachable at http://fathom-ollama...: [Errno -2] Name or service not known
```

So adding `localllm.lab` to the host's `/etc/hosts` is necessary but not sufficient,
and `--set ollama.service.externalName=registry.lab` does not help either — it fails
at the same step. The only fix is a `.lab` stub zone in the `coredns` ConfigMap
forwarding to the dnsmasq on the node network.

**That was considered and rejected.** It is a hand-edit to shared `kube-system`
state that a cluster rebuild silently drops, and it buys indirection this component
does not need — one agent, one model, one address. Half-owning the cluster's DNS is
a bigger commitment than the problem justifies. If the cluster ever grows a
provisioned `.lab` zone of its own, flip `ollama.service.create` back to `true` and
point `ollama.url` at `fathom-ollama.fathom.svc.cluster.local`; the template is
still there.

The trade accepted instead: the address appears in `ollama/config.env` and in
`values.yaml`. `setup.sh` prints the exact `ollama.url` line to copy, so the second
one is transcription rather than a second decision.

### Verifying

`kubectl describe svc fathom-ollama` is not a useful check even when the Service is
enabled — an `ExternalName` Service has no ClusterIP and no Endpoints by design,
whether or not DNS works. Check that the agent can actually reach the host:

```sh
kubectl -n fathom exec deploy/fathom-agent -- \
  python3 -c "import urllib.request;print(urllib.request.urlopen('http://192.168.56.1:11434/api/version',timeout=5).read())"
kubectl -n fathom get pods -l app.kubernetes.io/name=fathom-agent   # want 1/1
```

## 1. Build and push the image

From `agent/`:

```sh
make check                    # lock-check, ruff format + lint, pytest, helm lint + template
make help                     # the full target list
make push VERSION=0.5.1
```

The tag matters: the chart defaults `image.tag` to `.Chart.AppVersion`, the
version declared in `helm/Chart.yaml`. `make image` without `VERSION` derives a
tag from `git describe`, which will not match — pass `--set image.tag=<tag>` at
install time in that case.

The registry notes in `../mcp/README.md` apply unchanged here: the insecure-registry
configuration, and the `ctr -n k8s.io images import` side-load fallback if the
registry is unreachable from a node.

## 2. Install the chart

Unlike the MCP chart, this one defaults `namespace.create=false` — `fathom` already
exists, and two releases both declaring the same Namespace collide.

```sh
helm install fathom-agent ./helm
```

Preview without touching the cluster:

```sh
helm template fathom-agent ./helm
helm install fathom-agent ./helm --dry-run
```

## 3. Verify

```sh
kubectl -n fathom rollout status deploy/fathom-agent

# The Service is a NodePort on 30080, and the nodes share the 192.168.56.0/24
# libvirt network with this workstation, so no tunnel is needed.
export FATHOM=http://192.168.56.10:30080

curl -s $FATHOM/healthz   # process is up
curl -s $FATHOM/readyz    # process is up AND both dependencies answered
```

Any node's IP works — `kube-proxy` forwards the port from every node regardless of
where the pod landed, so `192.168.56.11` and `.12` are equally valid. If you would
rather not expose it, set `service.type=ClusterIP` and tunnel instead:

```sh
kubectl -n fathom port-forward svc/fathom-agent 8081:8080 &
export FATHOM=http://localhost:8081
```

`/readyz` is the one worth trusting: it returns 200 only when Ollama answered
`/api/version` *and* the MCP server advertised its tools. A 503 names which one
failed and why, so it distinguishes "the model server is down" from "the MCP server
is down" without reading logs.

Then ask it something:

```sh
curl -s $FATHOM/chat -H 'content-type: application/json' \
  -d '{"question":"Is the fathom-mcp deployment healthy? Check its events and recent logs."}'
```

Confirm the agent really has no cluster permissions of its own:

```sh
kubectl auth can-i --list --as=system:serviceaccount:fathom:fathom-agent
```

## The API

### `POST /chat`

```json
{
  "question": "which pods are not running?",
  "history": [{"role": "user", "content": "..."},
              {"role": "assistant", "content": "..."}]
}
```

`history` is optional. The endpoint is **stateless** — there is no server-side
conversation store, so whatever context you want carried, you send. That keeps the
pod restartable and replaceable at the cost of the client holding the transcript.

The response carries the tool trace alongside the answer, because "which tools did
it actually call" is the first question worth asking when an answer looks wrong:

```json
{
  "answer": "The oom-app pod is being OOMKilled against its 32Mi limit...",
  "remediation": {
    "action_required": true,
    "summary": "Raise oom-app's memory limit above what the container allocates.",
    "target_kind": "Pod",
    "target_name": "oom-app",
    "namespace": "fathom-eval-oomkill",
    "commands": ["kubectl -n fathom-eval-oomkill edit pod oom-app"],
    "confidence": "high"
  },
  "remediation_error": null,
  "steps": [
    {"tool": "k8s_describe",
     "args": {"resource": "deployments.apps", "name": "fathom-mcp", "namespace": "fathom"},
     "result_chars": 3095,
     "status": "success"}
  ],
  "iterations": 5,
  "model": "fathom-agent",
  "elapsed_seconds": 37.93
}
```

Status codes: `400` a malformed history role, `422` an empty question, `502` Ollama
or the MCP server failed, `504` the turn exceeded `agent.requestTimeout`.

Expect tens of seconds per question on lab hardware — a five-tool answer takes
roughly 40 s against a 9B model on CPU. `agent.requestTimeout` defaults to 180 s
for that reason.

### The remediation

`remediation` is the proposed fix, and the reason the prompt tells the model to lead
with one: an answer that diagnoses correctly and stops there leaves the reader exactly
where they started. `fathom-eval` grades it as its own category.

It is produced by a **second, tool-free model call** after the ReAct loop, using
Ollama's structured-output `format` to fill a fixed schema. It is a separate call
rather than a step in the loop because `format` and tool calling contend for the same
channel — constraining the loop's output would break the tool calls it exists to make.
Only the question and the finished answer go into it, never the transcript: the window
is 16k and the tool results have already done their work by then.

`action_required: false` is the case where the correct fix is no fix. Without it a
model learns that every answer ends in a kubectl command, which is what
`fathom-eval`'s `healthy` control exists to catch.

**`remediation` and `remediation_error` are never both set, and both can be null.**
The distinction matters and is why the second field exists:

| `remediation` | `remediation_error` | Meaning |
|---|---|---|
| object | `null` | a fix was proposed |
| `null` | `null` | shaping was skipped — turned off, or the answer was a sentinel worth nothing to shape |
| `null` | string | the shaping call itself failed |

A shaping failure never fails the turn. The diagnosis is already in hand by then and
is worth returning without a fix attached, so `/chat` still answers 200 — which is
also why the harness grades an empty `remediation` and a `remediation_error` as
different things: one is a poor answer, the other is an infrastructure fault.

Set `agent.remediation.enabled: false` to skip the second call entirely; the prose
answer is unchanged either way. `agent.remediation.timeout` (45 s) bounds it inside
`agent.requestTimeout`.

## Configuration

Every knob is a Helm value; the Deployment turns them into `FATHOM_AGENT_*`
environment variables, the same pattern the MCP server uses.

| Value | Default | What it does |
|---|---|---|
| `namespace.name` / `namespace.create` | `fathom` / `false` | Where the workload lives; creation is off because the MCP chart owns it |
| `image.repository` / `image.tag` | `registry.lab:5000/fathom-agent` / chart appVersion | Image coordinates |
| `service.port` | `8080` | Container port, Service port, and the listen address |
| `service.type` | `NodePort` | `NodePort` so the host can query the agent directly; `ClusterIP` if you prefer a port-forward |
| `service.nodePort` | `30080` | Pinned so the URL survives a reinstall. Ignored unless `service.type` is `NodePort`; null lets the API server assign one |
| `mcp.url` | `http://fathom-mcp.fathom.svc.cluster.local:8080/mcp` | The MCP endpoint, path included |
| `ollama.url` | `http://192.168.56.1:11434` | The host, dialled directly. Copy from `setup.sh` output |
| `ollama.model` | `fathom-agent` | The **alias**, not the base model tag |
| `ollama.service.create` / `.name` / `.externalName` | `false` / `fathom-ollama` / `localllm.lab` | The ExternalName Service, off — see [Why not the ExternalName Service](#why-not-the-externalname-service) |
| `agent.maxIterations` | `8` | Tool-calling rounds per turn |
| `agent.maxToolChars` | `8000` | Per-tool-result cap before it reaches the model |
| `agent.requestTimeout` | `180s` | Whole-turn budget |
| `agent.mcpTimeout` | `30s` | Single MCP call |
| `agent.readyCache` | `30s` | How long a `/readyz` result is reused |
| `agent.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `agent.systemPrompt` | `""` | Empty uses the prompt built into the image. An override replaces it wholesale, including the instruction to lead with a fix |
| `agent.remediation.enabled` | `true` | The second, tool-free call that produces `remediation` |
| `agent.remediation.timeout` | `45s` | Budget for that call, inside `requestTimeout` |
| `otel.mode` | `otlp` | `otlp`, `console` or `off` — picks the exporter, not its tuning |
| `otel.endpoint` | `http://otel-collector.observability.svc.cluster.local:4317` | Collector address. FQDN because it is another namespace; `http://` selects plaintext gRPC |
| `otel.sampler` | `parentbased_always_on` | Probes are excluded before sampling, so this only samples real work |
| `otel.metricExportInterval` | `60000` | Milliseconds between metric exports |
| `otel.environment` | `lab` | Becomes `deployment.environment.name` on every span and metric |

### Why the model parameters are not here

`num_ctx`, `temperature` and `top_p` are missing on purpose. They are pinned in the
`fathom-agent` alias's Modelfile by `ollama/setup.sh`, so serving parameters travel
with the model rather than with every request — which is what makes swapping the
base model a config change plus a re-run of `setup.sh`, invisible to this
application. Setting them here would split that source of truth in two.

For the same reason the agent requests `ollama.model` (the alias) and never a base
tag like `qwen3.5:9b-q4_K_M`.

### Two size limits, doing different jobs

`agent.maxToolChars` (8000) sits on top of the MCP server's own
`limits.maxBytes` (65536). They are not redundant:

- the server's cap is about what it is willing to *send*;
- this one is about what the model can *afford* — the context is 16k tokens, so one
  maximal server response is roughly the entire window, leaving nothing to reason
  with.

Raise this only alongside `FATHOM_NUM_CTX` in `ollama/config.env`, and remember the
KV cache grows linearly with that.

## Running outside the cluster

The fastest loop, and the only way to exercise the model without a deploy:

```sh
kubectl -n fathom port-forward svc/fathom-mcp 8080:8080 &
make run-local OLLAMA_URL=http://192.168.56.1:11434
```

That listens on `:8081` and dials the host's Ollama directly, bypassing the
ExternalName Service.

## Troubleshooting

**`/readyz` says `ollama unreachable`** — the message carries the URL it tried and
the underlying error. If it is a DNS failure, the ExternalName target does not
resolve from inside the cluster; run the `dnstest` pod above. If it is a connection
refused or timeout, check `OLLAMA_BIND` in `ollama/config.env` is not loopback-only
and that the host firewall allows the node network.

**`/readyz` says `mcp server unreachable`** — check `fathom-mcp` itself first
(`kubectl -n fathom get pod`, then its own `/readyz`). If the MCP server is healthy,
confirm `mcp.url` includes the `/mcp` path; a bare host:port will connect and then
fail to initialize.

**An answer is correct but a tool call in `steps` shows `status: error`** — that is
usually fine and often a good sign. The MCP server writes its errors as advice, so
the model reads "namespace not found" and retries with a broader query. You will see
this most often on the first call, when the model guesses the default namespace.

**Answers are empty or truncated mid-thought** — check `agent.maxIterations` in the
response's `iterations` field. If it is at the ceiling, the model ran out of rounds;
the `answer` field says so explicitly rather than returning nothing.

**Pod will not start** — config validation is fail-fast and the error names the
variable, e.g. `fathom-agent: FATHOM_AGENT_MCP_TIMEOUT (60.0s) exceeds
FATHOM_AGENT_REQUEST_TIMEOUT (30.0s)`. Read the first line of the logs.

**`OOMKilled`** — the memory limit is 1536Mi and the LangChain import tree is most of
the baseline. Raise `resources.limits.memory` rather than trimming dependencies.

## Upgrade and uninstall

```sh
helm upgrade fathom-agent ./helm --set image.tag=0.5.2
helm uninstall fathom-agent
```

Changes to `agent.*`, `ollama.*`, `mcp.*` or `otel.*` roll the pods automatically —
the Deployment carries a `checksum/config` annotation over those blocks.

## The published contract

`contracts/agent/` describes this service's interface, and is **generated** — never
edit it.

`scripts/contracts.py` starts this component's image in a throwaway container
and fetches FastAPI's own `/openapi.json` from it. **Nothing in this service exists to
produce that** — no `contract` target, no describe-myself module.

The chart supplies what a process cannot know about itself — namespace, Service type,
nodePort, RBAC, probes. The sandbox runs the container with the chart's own environment
and dials its `containerPort`, so a pod that would start, pass its probes and be
unreachable through its Service fails here instead.

```sh
make -C .. contracts COMPONENT=agent   # needs Docker: this runs the service
make -C .. contracts-check              # fail on drift; also part of the root `make check`
```

`make -C .. hooks` installs the pre-commit hook that regenerates it for you.
`contracts/agent/integration-notes.md` is the one file in there written by hand.
