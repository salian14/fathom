# Contracts

What each service publishes, generated from the services themselves.

Read these instead of a sibling's source. That is the whole point: cross-service work
that starts by reading three other components pays for those files for the rest of the
session, and the interface is the only part of them the caller actually needs.

**Everything here except this file and the `integration-notes.md` files is generated.
Do not edit it. Run `make contracts`.**

## Layout

```
contracts/<component>/
  service.json              identity: name, namespace, listen port, Service type,
                            nodePort, workloads, RBAC, probes, dependencies
  openapi.json              agent, front, eval -- a standalone OpenAPI 3.1 document
  mcp-tools.json            mcp -- the tools/list surface, in both gate states
  routes.json               front -- the mux table, with what each route proxies to
  storage.json + schema.sql eval -- the results database version and its DDL
  fixture-namespaces.json   eval -- one half of the rbac.fixtureNamespaces agreement
  integration-notes.md      HAND-WRITTEN. Never touched by the generator.
```

## Where the content comes from

Two sources, and neither is a parser pointed at source code:

1. **The component describes itself.** `make -C <component> contract` runs the code and
   prints its own interface. `fathom-agent` returns FastAPI's `app.openapi()`;
   `fathom-mcp` performs a real `tools/list` against a real server over an in-memory
   transport; `fathom-front` reads the same `routes()` slice its mux is built from;
   `fathom-eval` reads its live aiohttp router. Nothing restates a route or a schema,
   so nothing can disagree with the code.
2. **The chart supplies what a process cannot know about itself** — which namespace it
   lands in, what Service type fronts it, what nodePort, what RBAC. That comes from
   `helm template`.

Where the two overlap they are **cross-checked, not merged**. If the port a binary
listens on by default stops matching the `service.port` its chart publishes,
`make contracts` fails and names both. Nothing else in the repo checks that, and the
failure it prevents is quiet: a pod that starts, passes its own probes, and is
unreachable through its Service.

## `integration-notes.md` is yours

The generator creates it once from a stub and then never touches it again.

It holds what a spec cannot express and what actually causes incidents: which failures
are worth retrying, whose timeout budget yours sits inside, whether repeating a call is
safe, what may run concurrently. A generated file can tell you `POST /chat` takes a
`question`. Only this one can tell you that a 504 from it means the turn budget expired
with work in flight, and that resending is a fresh turn rather than a resumption.

## Regenerating

```sh
make contracts                  # everything
make contracts COMPONENT=agent  # one component
make contracts-check            # fail if contracts/ has drifted; wired into `make check`
```

`make hooks` installs the checked-in pre-commit hook, which regenerates the components a
commit touches and includes the result in that commit. Bypass once with
`git commit --no-verify`, or always with `FATHOM_SKIP_CONTRACTS=1`.

## Two things to know before trusting a diff

**The generator reads the working tree, not the index.** It runs the code rather than
parsing it, and there is no way to run a file that exists only in the index. So if you
stage part of a component and leave the rest unstaged, the generated contract describes
the working tree — not the commit. The hook warns when it detects this. It does not
stash around it: `git stash --keep-index` inside a hook can lose work if anything after
it fails, and a wrong contract is cheaper to fix than a lost change.

**The version is the chart's `appVersion`, not the build's.** Two components stamp
themselves with `git describe --always --dirty`, which changes on every commit.
Publishing that would rewrite these files on every commit and bury real interface
changes in noise, so `scripts/contracts.py` overrides it. A version change here means a
release happened, not that someone rebuilt.
