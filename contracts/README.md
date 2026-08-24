# Contracts

What each service publishes, obtained by **running** the services -- never by parsing
them, and never from code the services carry in order to be described.

Read these instead of a sibling's source. That is the whole point: cross-service work
that starts by reading three other components pays for those files for the rest of the
session, and the interface is the only part of them the caller actually needs.

**Everything here except this file and the `integration-notes.md` files is generated.
Do not edit it. Run `make contracts`.**

## The rule this directory is built around

> **No component contains a line of code that exists to produce a contract.**

There is no `contract` make target, no `-contract` flag, and no module in any service
whose job is to describe it. `scripts/contracts.py` builds each component's image,
starts it in a throwaway container configured the way its own chart configures it, and
scrapes it over the wire like any other client.

That constraint is what makes these files trustworthy. A contract assembled from
declarations sitting beside the code is a second source of truth, and a second source
of truth drifts; a contract taken off the wire is the interface, because it is the same
bytes a caller receives.

## Layout

```
contracts/<component>/
  service.json              identity: name, namespace, listen port, Service type,
                            nodePort, workloads, RBAC, probes
  openapi.json              agent, eval -- FastAPI's own /openapi.json, over HTTP
  mcp-tools.json            mcp -- a real tools/list, in both gate states
  storage.json + schema.sql eval -- the results database version and its applied DDL
  fixture-namespaces.json   eval -- one half of the rbac.fixtureNamespaces agreement
  integration-notes.md      HAND-WRITTEN. Never touched by the generator.
```

## Where the content comes from

**The running container, scraped over the wire:**

| Component | How |
|---|---|
| `agent` | `GET /openapi.json`. FastAPI publishes this already; the service was not modified to provide it. |
| `mcp` | `initialize` + `tools/list` over streamable HTTP, run **twice** -- once with `prometheus.url` unset, once with the chart's value -- because three of the six tools are gated on it. The server `instructions` come from the initialize result. |
| `eval` | `GET /openapi.json` -- also FastAPI, since 2026-08-23. Plus the DDL, read out of `sqlite_master` in a database the image builds for itself, so what is published is `SCHEMA` **plus every migration**, not the declared schema. |
| `front` | Nothing. See below. |

**The chart, for what a process cannot know about itself** -- which namespace it lands
in, what Service fronts it, what nodePort, what RBAC, what its probes are. That comes
from `helm template`.

## `front` has no generated spec, and that is the honest answer

Go's `http.ServeMux` does not report what was registered with it, and `fathom-front`
publishes no OpenAPI. There are only three ways to produce a route list, and two are
worse than none:

- a declaration beside `newMux` -- the second source of truth this directory exists to
  avoid;
- probing a candidate path list -- which derives nothing, since the list is the answer;
- writing it down and testing it.

So `contracts/front/integration-notes.md` carries the route table by hand, and
`TestDocumentedRoutesAreServed` dials every row of it through the real mux. The
container is still started, because the port check below is worth having on its own.

`fathom-eval-serve` used to be in the same position and no longer is. It was aiohttp,
which publishes nothing; it is FastAPI as of 2026-08-23, so it now serves its own spec
like the agent does. That is the one legitimate way to change a service for a contract
-- changing *how it serves*, rather than adding something whose only job is to describe
it. `front` is not a candidate for the same move: it serves a UI and proxies two
upstreams, so there is no API of its own for a framework to publish.

## What the container run checks that nothing else does

The sandbox runs each component **with its chart's own environment** and then dials
**its chart's own `containerPort`**. That checks the thing that actually breaks: the
env var is spelled the way the binary reads it, the value parses, and the process ends
up listening where the Service will look for it.

Comparing a binary's *default* port against `service.port` -- which is what this used to
do -- asserts a coincidence rather than a requirement. Every Deployment here sets
`FATHOM_<C>_ADDR` from `service.port`, so the code's default never applies in the cluster
at all: the old comparison would have failed a chart that legitimately published a
non-default port, while never establishing that the process listens anywhere in
particular. The failure it let through is quiet: a pod that starts, passes its own
probes, and is unreachable through its Service.

Two environment overrides are applied, and only two: telemetry is forced `off` (the
collector is a cluster DNS name), and `fathom-mcp` is handed a kubeconfig naming a
server that does not exist -- client-go needs a config to build a clientset but does not
dial when it is constructed. **Nothing reachable is ever configured.** The sandbox
cannot touch a real cluster.

## `integration-notes.md` is yours

The generator creates it once from a stub and then never touches it again.

It holds what a spec cannot express and what actually causes incidents: which failures
are worth retrying, whose timeout budget yours sits inside, whether repeating a call is
safe, what may run concurrently. A generated file can tell you `POST /chat` takes a
`question`. Only this one can tell you that a 504 from it means the turn budget expired
with work in flight, and that resending is a fresh turn rather than a resumption.

For `front` and `eval` it also carries the route list, which is the only place those
two are described at all.

## Regenerating

```sh
make contracts                  # everything
make contracts COMPONENT=agent  # one component
make contracts-check            # fail if contracts/ has drifted; wired into `make check`
```

**This needs Docker**, because it runs the services. There is no fallback that would
produce the same document, so the writer fails with that message rather than emitting
something weaker under the same filenames.

`make hooks` installs the checked-in pre-commit hook, which regenerates the components a
commit touches and includes the result in that commit. It **skips** when Docker is not
running rather than blocking a commit on a daemon that is not there. Bypass once with
`git commit --no-verify`, or always with `FATHOM_SKIP_CONTRACTS=1`.

## Two things to know before trusting a diff

**The generator reads the working tree, not the index.** It builds an image from the
working tree, and there is no way to build a file that exists only in the index. So if
you stage part of a component and leave the rest unstaged, the generated contract
describes the working tree -- not the commit. The hook warns when it detects this. It
does not stash around it: `git stash --keep-index` inside a hook can lose work if
anything after it fails, and a wrong contract is cheaper to fix than a lost change.

**The version is the chart's `appVersion`, not the build's.** Two components stamp
themselves with `git describe --always --dirty`, which changes on every commit.
Publishing that would rewrite these files on every commit and bury real interface
changes in noise, so `scripts/contracts.py` overrides it. A version change here means a
release happened, not that someone rebuilt.
