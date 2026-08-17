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

At the time of writing this lab's nodes are on `192.168.56.0/24` and the host is
`192.168.56.1` — note that `ollama/config.env` still says `192.168.122.1`, which is
a different (and currently down) libvirt bridge. Fixing that file is tracked
separately; until then, take the address from `kubectl get nodes -o wide`.

The chart's default `ollama.service.externalName` is `localllm.lab`. **That name
does not exist yet.** Adding it is a one-time host change, deliberately left out of
this component:

```sh
# on the host, as root:
echo "192.168.56.1 localllm.lab" >> /etc/hosts
pkill -HUP dnsmasq     # libvirt's dnsmasq re-reads /etc/hosts only on SIGHUP
```

That works because libvirt's dnsmasq serves the host's `/etc/hosts` to the VMs, and
CoreDNS forwards to the node's resolver — so an entry on the host reaches pods.
Verify it does, from inside the cluster rather than from your workstation:

```sh
kubectl run dnstest --rm -it --restart=Never --image=busybox \
  -- nslookup localllm.lab
```

Two escapes if you would rather not touch `/etc/hosts`:

```sh
# 1. reuse a name that already resolves to the host on the node network
helm install fathom-agent ./helm --set ollama.service.externalName=registry.lab

# 2. skip the Service entirely and dial the host's address
helm install fathom-agent ./helm \
  --set ollama.service.create=false \
  --set ollama.url=http://192.168.56.1:11434
```

## 1. Build and push the image

From `agent/`:

```sh
make check                    # ruff format + lint, pytest, helm lint + template
make push VERSION=0.1.0
```

The tag matters: the chart defaults `image.tag` to `.Chart.AppVersion` (`0.1.0`).
`make image` without `VERSION` derives a tag from `git describe`, which will not
match — pass `--set image.tag=<tag>` at install time in that case.

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
kubectl -n fathom port-forward svc/fathom-agent 8081:8080 &

curl -s localhost:8081/healthz   # process is up
curl -s localhost:8081/readyz    # process is up AND both dependencies answered
```

`/readyz` is the one worth trusting: it returns 200 only when Ollama answered
`/api/version` *and* the MCP server advertised its tools. A 503 names which one
failed and why, so it distinguishes "the model server is down" from "the MCP server
is down" without reading logs.

Then ask it something:

```sh
curl -s localhost:8081/chat -H 'content-type: application/json' \
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
  "answer": "Yes, the fathom-mcp deployment is healthy...",
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

## Configuration

Every knob is a Helm value; the Deployment turns them into `FATHOM_AGENT_*`
environment variables, the same pattern the MCP server uses.

| Value | Default | What it does |
|---|---|---|
| `namespace.name` / `namespace.create` | `fathom` / `false` | Where the workload lives; creation is off because the MCP chart owns it |
| `image.repository` / `image.tag` | `registry.lab:5000/fathom-agent` / chart appVersion | Image coordinates |
| `service.port` | `8080` | Container port, Service port, and the listen address |
| `mcp.url` | `http://fathom-mcp.fathom.svc.cluster.local:8080/mcp` | The MCP endpoint, path included |
| `ollama.url` | `http://fathom-ollama.fathom.svc.cluster.local:11434` | Dialled through the ExternalName Service |
| `ollama.model` | `fathom-agent` | The **alias**, not the base model tag |
| `ollama.service.create` / `.name` / `.externalName` | `true` / `fathom-ollama` / `localllm.lab` | The ExternalName Service |
| `agent.maxIterations` | `8` | Tool-calling rounds per turn |
| `agent.maxToolChars` | `8000` | Per-tool-result cap before it reaches the model |
| `agent.requestTimeout` | `180s` | Whole-turn budget |
| `agent.mcpTimeout` | `30s` | Single MCP call |
| `agent.readyCache` | `30s` | How long a `/readyz` result is reused |
| `agent.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `agent.systemPrompt` | `""` | Empty uses the prompt built into the image |

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

**`OOMKilled`** — the memory limit is 1Gi and the LangChain import tree is most of
the baseline. Raise `resources.limits.memory` rather than trimming dependencies.

## Upgrade and uninstall

```sh
helm upgrade fathom-agent ./helm --set image.tag=0.2.0
helm uninstall fathom-agent
```

Changes to `agent.*`, `ollama.*` or `mcp.*` roll the pods automatically — the
Deployment carries a `checksum/config` annotation over those blocks.
