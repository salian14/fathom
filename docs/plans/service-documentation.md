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
