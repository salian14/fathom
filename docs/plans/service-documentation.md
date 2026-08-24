# Service documentation audit

Date: 2026-08-22
Branch: agent/feat/eval-results-view
HEAD: 0fbfc6e

## Method

Phase 0 of `cross-service-work`. No `docs/conventions.md`, no `contracts/`, no
per-service `CLAUDE.md`, no generator. What exists instead: a 540-line root
`README.md`, a 208-line root `CLAUDE.md`, and a README per component
(agent 397, mcp 379, front 331, eval 468 lines).

Each README was cross-checked mechanically against the code it describes:
`helm/values.yaml` keys, `FATHOM_*` env vars read in source, HTTP routes
registered, and `make` targets defined.

## Verdict per service

| Service | README | Verdict |
|---|---|---|
| `mcp/` | 379 lines | **Good.** Config table, tool set, RBAC coupling, Prometheus gate, telemetry, troubleshooting, upgrade/uninstall. Two small omissions. |
| `agent/` | 397 lines | **Good.** Most complete config table in the repo; `/chat` API documented with rationale. |
| `front/` | 331 lines | **Good.** Routes incl. `/api/features` documented, eval gate explained, timeout hierarchy justified. Stale counts. |
| `eval/` | 468 lines | **Longest but structurally incomplete.** Rich rationale, missing the operational reference sections the other three all carry. |

Nothing is undocumented in the sense of "nobody wrote it down". The gaps are
(a) `eval/` diverging from the README shape the other three share, and
(b) the root `README.md` never finishing the eval integration that root
`CLAUDE.md` already completed.

## Findings

### F1 — `eval/README.md` is missing three of the four shared sections (highest value)

`agent`, `mcp` and `front` READMEs all carry: a Helm value table, a Telemetry
section, Troubleshooting, and Upgrade/uninstall. `eval/README.md` carries none
of them, and has no `helm install` line anywhere — the only component you
cannot deploy from its own README.

Undocumented as a result: ~30 Helm values across `judge.*`, `serve.*`,
`persistence.*`, `cronjob.*`, `priorityClass.*`, `otel.*` and most of `eval.*`.
Some are load-bearing and appear nowhere else:
- `cronjob.activeDeadlineSeconds` (18000) / `softDeadlineSeconds` (14400) —
  the SIGKILL that the README's own `-wal`/`-shm` discussion depends on
- `cronjob.suspend` (true) — mentioned in prose, absent from any table
- `persistence.keep` (true), `persistence.size` (2Gi)
- `serve.maxRows` (500)
- `priorityClass.value` (-10)

Fix: add `## Configuration`, `## Deploying`, `## Telemetry`, `## Troubleshooting`
and `## Upgrade and uninstall` to `eval/README.md`, matching the other three.

### F2 — `FATHOM_EVAL_TELEMETRY` is documented nowhere

`eval/helm/templates/_helpers.tpl:124` sets it and
`eval/src/fathom_eval/__main__.py:15` reads it. Both root `README.md` (~line 475)
and root `CLAUDE.md` (Telemetry section) enumerate exactly three:
`FATHOM_AGENT_TELEMETRY` / `FATHOM_MCP_TELEMETRY` / `FATHOM_FRONT_TELEMETRY`.

Note `eval/helm/values.yaml`'s `otel` block has only `mode`, `endpoint`,
`environment` — no `sampler`, no `metricExportInterval`, unlike the other three.
Worth documenting as deliberate or fixing as drift; currently unstated either way.

### F3 — root `README.md` says "three" where it is now four

Root `CLAUDE.md` was updated for eval (commit 0fbfc6e). Root `README.md` was
updated only in its deep tables ("Where each thing is defined", "Where to go
next"); the framing at the top was not.

| Line | Text | Actual |
|---|---|---|
| 15–16 | "currently at **v0.5.1** across all three charts" | v0.5.4, four charts |
| 121 | "### The components" table lists three | `fathom-eval-serve` absent |
| 185 | "set `otel.mode=off` … in all three charts" | four |
| 265–282 | Layout block omits `eval/`; "The charts are `agent/helm`, `mcp/helm` and `front/helm`" | `eval/helm` absent |
| 272 | "Makefile cuts one version across all three components" | four (`Makefile:42` — `COMPONENTS := mcp agent front eval`) |
| 421 | "spans all three components" | four |
| 462 | "Every knob in all three components" | four |
| 475 | `FATHOM_AGENT_*`, `FATHOM_MCP_*`, `FATHOM_FRONT_*` | `FATHOM_EVAL_*` absent |
| 490 | "Image version — the three `Chart.yaml` `appVersion`s" | four |
| 511 | "All three services export OTLP" | eval exports too |

Line 516's "a single trace spanning all three services" is **correct** — that is
browser → front → agent → mcp, and eval is not in that path. Do not change it.

### F4 — smaller omissions

- `front/README.md:193` "the same pattern the other two components use" → three;
  same at :227 and :329.
- `front/README.md` config table documents `otel.mode` only; `agent/README.md`
  documents all five `otel.*` keys. Front's chart has all five.
- `mcp/README.md` config table has no `otel.*` rows at all (its prose Telemetry
  section covers the mode, not the other four keys), and omits `server.transport`.

### F5 — structural absences (no action unless wanted)

No `docs/conventions.md`, no `contracts/`, no per-service `CLAUDE.md`. For a
four-component repo where all four Makefiles already share one target vocabulary
(verified: `all build check clean fmt help image lint lint-chart push
render-chart run-local test version` in all four), a conventions doc plus a
generator would be the durable fix. Root `CLAUDE.md` currently does part of this
job.

## Fix order

Providers before consumers; here that means the authoritative file first.

1. `eval/README.md` — F1, the four missing sections. Largest gap, self-contained.
2. Root `README.md` — F3, the ten "three"→"four" sites plus the version bump.
3. Root `CLAUDE.md` — F2, add `FATHOM_EVAL_TELEMETRY` to the telemetry list.
4. `front/README.md` — F4 counts and the four missing `otel.*` rows.
5. `mcp/README.md` — F4 `otel.*` rows and `server.transport`.

Verification per step: `grep -c 'all three' README.md` → 0 for the ones listed;
every Helm key in `<svc>/helm/values.yaml` that is not boilerplate
(`resources`, `nodeSelector`, `tolerations`, `affinity`, `pod*`,
`securityContext`, `imagePullSecrets`, `nameOverride`, `fullnameOverride`)
appears in that service's config table.

## Status

- [x] 1. eval/README.md — added Deploying, Configuration, Telemetry, Troubleshooting,
      Upgrade and uninstall. 468 -> 725 lines. `make check` green (243 passed).
- [x] 2. README.md — status line v0.5.1->v0.5.4 and "three charts"->"four"; eval added
      to the components table and the Layout block; telemetry section corrected.
- [x] 3. CLAUDE.md — `FATHOM_EVAL_TELEMETRY` added; the traces-only fact recorded with
      a "do not fix this by adding a meter" note.
- [x] 4. front/README.md — three count fixes, four `otel.*` rows added. `make check` green.
- [x] 5. mcp/README.md — five `otel.*` rows and `server.transport` added. `make check` green.
- [x] 6. docs/conventions.md — 484 lines. Prohibitions confirmed by the owner
      2026-08-23; all four hold as stated.

### Further findings recorded in conventions.md rather than fixed

- **Go module paths disagree.** `mcp/go.mod` is `github.com/spellbound/fathom/mcp`,
  `front/go.mod` is `github.com/spellbound/fathom/fathom-front`. The directory-named
  form matches every other naming rule in the repo. Recorded as the rule for new Go
  components; renaming `front` was not asked for.
- **`eval` does not set `OTEL_PROPAGATORS`** where the other three do. Harmless — the
  SDK default is the same `tracecontext,baggage` — but implicit. Recorded, not changed.

### Offered, not done

- A `templates/` skeleton plus `scripts/new-service`. Everything under "Template
  material" in conventions.md is copyable, so the doc is a workaround for the generator
  not existing.
- Per-component `CLAUDE.md` (declined for this pass).

### Done later — 2026-08-23, `agent/feat/service-contracts`

> **Superseded in part by the entry below.** The `contract` make targets and the
> `config.json` / `telemetry.json` surfaces described here were removed the same day;
> what replaced them is at [Reworked](#reworked--2026-08-23-contracts-without-service-code).

Two of the three items above were taken up in a follow-up pass:

- **`contracts/` now exists**, one directory per component, holding `service.json`
  (name, namespace, listen port, Service type, nodePort, workloads, RBAC, probes),
  `config.json`, `telemetry.json`, and the interface itself — `openapi.json` for the
  three HTTP services, `mcp-tools.json` for `fathom-mcp`, `storage.json` + `schema.sql`
  for the results database.
- **A pre-commit hook regenerates it**, scoped to the components a commit touches, and
  includes the result in that commit. `.githooks/pre-commit`, opted into with
  `make hooks`. `make contracts-check` is the same gate for anyone who has not installed
  it, and is wired into the root `make check`.

Everything is generated by *running* the services rather than parsing them, which is why
F1-style drift cannot recur in the mechanical facts: each component gained a `contract`
make target (the fifteenth shared one) that prints its own interface, and
`scripts/contracts.py` cross-checks it against `helm template` — a default listen port
that stops matching `service.port` now fails the commit naming both files.

Two things the pass found on its own, neither of which was in the original audit:

- `mcp.server.operation.duration` records an `outcome` attribute that no document
  mentioned. Found by the test that compares declared metric attributes against what the
  middleware actually emits.
- `eval/helm/values.yaml:93` declares `serve.service.type`, but
  `eval/helm/templates/serve-service.yaml:20` hardcodes the type and nothing reads the
  value. The hardcode is deliberate and commented; the key is dead and misleading.
  Recorded, not fixed.

`FATHOM_EVAL_TELEMETRY` — F2, the variable that was documented nowhere — now appears in
`contracts/eval/config.json` without anyone having to remember it, because the config
loader records each variable as it reads it.

Still open: the `templates/` skeleton and `scripts/new-service`.

### Amendments made during the work

- **F2 was half wrong.** `otel.sampler` was genuine drift and is now in `eval/helm`
  (`OTEL_TRACES_SAMPLER`, verified in all three rendered workloads).
  `otel.metricExportInterval` was **not** added: `eval/src/fathom_eval/telemetry.py`
  builds a TracerProvider and no MeterProvider on purpose, so the key would have had
  nothing reading it. `values.yaml` now says so at the point where someone would add it.
- **New finding, fixed:** `eval/helm/values.yaml:63` pointed at `ollama/judge.env`,
  which does not exist. The judge block is in `ollama/config.env`. Corrected.

### Reworked — 2026-08-23, contracts without service code

The pass above was rejected on review, and the objection was the right one: it put
~1,650 lines into service source, of which ~840 were `contract.py` / `contract.go`
entrypoints that existed only to print a document. FastAPI already publishes
`/openapi.json` and MCP already answers `tools/list`; a contract writer that runs the
container and asks needs none of that code.

**The rule now is: no component contains a line of code that exists to produce a
contract.** `scripts/contracts.py` builds each image, starts it in a throwaway container
with the chart's own environment, and scrapes it over the wire. The `contract` make
target is gone from all four components, and the shared set is fourteen targets again.

What was removed from the services:

| Removed | Lines |
|---|---|
| `agent/src/fathom_agent/contract.py`, `mcp/cmd/fathom-mcp/contract.go`, `front/cmd/fathom-front/contract.go`, `eval/src/fathom_eval/contract.py` | ~840 |
| `front`'s declarative `route` table in `main.go` (back to a plain mux) | ~90 |
| `serve.RESPONSE_SCHEMAS` in `eval` | 137 |
| the config-recording helpers in all four config loaders | ~200 |
| `mcp`'s `metricdoc.go` in `internal/telemetry` and `internal/tools`, plus their tests | ~250 |

`api.py`'s `ChatResponse` model was **kept**: `POST /chat` documented no response body
before it, and that is an API fix rather than contract scaffolding. The scraped
`contracts/agent/openapi.json` came back byte-identical to the one the in-process module
produced, which is the evidence that nothing was lost in the move.

What got better, not just smaller:

- **The port check is stronger.** It used to compare a binary's default listen port
  against `service.port`. But every Deployment *sets* `FATHOM_<C>_ADDR` from
  `service.port`, so the default never applies in the cluster and a misspelled variable
  name passed the comparison. The sandbox now runs the component with the chart's
  environment and dials the chart's `containerPort`, which checks the deployed
  configuration end to end.
- **`contracts/eval/schema.sql` is now the applied schema**, read out of `sqlite_master`
  in a database the image builds for itself — so it carries `SCHEMA` plus every
  migration in `_ADDED_COLUMNS`, rather than the DDL constant. The migrations are the
  half that drifts.
- **`mcp`'s server `instructions` are scraped** from the `initialize` result in both
  gate states, rather than declared.

What was accepted as lost:

- **`config.json` and `telemetry.json` are gone.** Neither is observable from outside a
  process: environment variables a program reads and metric attribute keys chosen at
  record time leave no external trace. F2's `FATHOM_EVAL_TELEMETRY` no longer has an
  automatic guarantee — it is documented in `CLAUDE.md`, `README.md` and
  `eval/README.md`, by hand, like everything else.
- **The metric-attribute cardinality guard is gone with `metricdoc`.** Prohibition 4 in
  conventions.md is back to being a rule people follow rather than one a test enforces.
  The `outcome` attribute that pass found on `mcp.server.operation.duration` stays
  found; nothing re-finds the next one.
- **`front` has no generated spec at all.** Go's `ServeMux` does not report what was
  registered with it. Its routes and `fathom-eval-serve`'s are hand-written in
  `integration-notes.md` and pinned by `TestDocumentedRoutesAreServed` /
  `test_documented_routes_are_served`, which dial every documented row through the real
  mux or router.

The cost: `make contracts` now needs Docker and builds four images. Layer-cached that is
seconds per component; cold it is a minute or more. `.githooks/pre-commit` skips rather
than blocking when Docker is not running, which does let a stale contract through — the
root `make check` still catches it via `contracts-check`.

### Amended — 2026-08-23, `fathom-eval-serve` publishes a spec

The rework above left `front` and `eval` both undescribed, on the grounds that neither
could be scraped. Challenged on review, and the challenge was right about one of them:

- **`front` genuinely cannot be scraped.** Go's `ServeMux` does not report what was
  registered with it, and the binary serves an embedded UI plus two reverse proxies. It
  owns no response shape of its own, so there is nothing a spec would describe that the
  agent's and eval's specs do not already carry.
- **`eval-serve` could be, and the only thing stopping it was a framework choice.** It
  is the only pure JSON API in the repo, and `front/internal/web/static/evals.js:23`
  consumes its response shapes across a namespace boundary — a contract that existed
  only as prose.

`eval/pyproject.toml` had recorded that choice explicitly: aiohttp because the
kubernetes client already pulls it, and "FastAPI was the alternative and would have
added starlette, uvicorn and pydantic to a batch image for six read-only routes." That
reasoning was right about image weight and wrong about what `serve` is.

**Reversed.** `serve.py` is FastAPI, so `contracts/eval/openapi.json` is now scraped from
the container's own `/openapi.json` exactly as `fathom-agent`'s is — and this component
still contains no describe-myself code, which is the constraint that matters. This
establishes the rule now in conventions.md: *the one legitimate way to change a service
for a contract is to change how it serves, never to add something whose only job is to
describe it.*

What the move surfaced, none of which was visible before:

- **`ResultDetail` was modelled wrong** — `get_result` returns an envelope
  (`{result, run, checks, category_order, judgements}`), not the bare row. FastAPI's
  response validation caught it on the first request. Nothing in the aiohttp version
  could have.
- **The database dependency has to be `async def`.** A sync dependency runs in a
  threadpool, so the connection is opened in one thread and used by the route handler in
  another, which SQLite refuses outright. Async keeps open/query/close on the event loop
  thread — the same single-threaded model aiohttp had.
- **Parameter validation moved from 400 to 422.** Hand-parsing kept the 400, but FastAPI
  then advertised a 422 in the spec that could never fire. Letting FastAPI validate makes
  the declared status a real one and types `limit`/`offset` as `integer, minimum: 0`; a
  `RequestValidationError` handler keeps the `{"error": ...}` envelope `evals.js`
  depends on. `evals.js` does not branch on the status code, so nothing downstream
  changed.

Cost: three wheels in the eval image (`fastapi`, `uvicorn`, `starlette`/`pydantic`
transitively). `aiohttp` remains in `requirements.lock` as the kubernetes client's
transitive dependency, which is what it was before it was promoted.

`front` remains the one component with no interface file, documented by hand in
`contracts/front/integration-notes.md` and pinned by `TestDocumentedRoutesAreServed`.
