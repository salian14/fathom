# fathom-eval -- integration notes

**Hand-written. `make contracts` never touches this file.**

`openapi.json`, `schema.sql`, `storage.json`, `fixture-namespaces.json` and
`service.json` beside it are all generated. This file is not: it is what a spec cannot
express.

## The spec is the route reference

`fathom-eval-serve` was aiohttp until 2026-08-23, which published no machine-readable
description of anything. It is FastAPI now, so `contracts/eval/openapi.json` is scraped
from a running container's own `/openapi.json` exactly as `fathom-agent`'s is -- **read
that for paths, parameters, status codes and response schemas.** The table below is
orientation, not the contract.

The response models exist to publish the envelope, never to filter it: every one carries
`extra="allow"`, and `test_response_models_drop_nothing` drives a real request and
asserts that what `reporting` produced is what the client receives. The row objects
inside each envelope are deliberately open -- they are SQL projections, and `schema.sql`
is where their columns are published.

The DDL is generated too, and by running the code: `scripts/contracts.py` starts the
image, lets the store create a database, and reads `schema.sql` back out of
`sqlite_master`. What is published is therefore `SCHEMA` **plus every migration in
`_ADDED_COLUMNS`** -- the applied schema, not the declared one. The migrations are the
half that drifts, so they are the half worth publishing.

## Routes

All GET. All read-only.

| Route | Returns | Notes |
|---|---|---|
| `/healthz` | `ok` | Liveness. Never traced. |
| `/readyz` | `ok (database present)` / `ok (no database yet)` | Reports on the **mount**, not the data. 503 only when `/data` is absent or unreadable. |
| `/meta` | `{database, max_rows, schema_version?}` | Present even with no database. `schema_version` is absent until there is one. |
| `/runs` | `{runs, limit, offset, total}` | Newest first. `?limit` (default 50), `?offset`. |
| `/runs/{id}` | `{run, fixtures, checks, category_order, rubric_versions, rubric, results, ...}` | `?rubric`, `?fixture`, `?outcome`, `?order`, `?limit`, `?offset`. 404 if no such run. |
| `/results/{id}` | one result, in full | The only endpoint carrying free text. 404 if no such result. |

Every response carries `Cache-Control: no-store`.

## Two rules the routes follow

**List endpoints carry no free text.** A run is ~200 results and an answer is
kilobytes, so `answer`, `question_text` and `rationale` appear only in
`/results/{id}`, one at a time. Do not add them to a list endpoint to save a round
trip.

**A missing database is not an error.** A cluster whose CronJob has never run is a
correct state of the system. The list endpoints answer **200** with
`{"empty": true, "reason": ..., "runs": [], "results": []}`, not 404 and not 503. A
client that treats an absent database as an outage will show a red banner on every
fresh install. `/readyz` is consistent with this: it reports the mount, because a
missing `/data` *is* a deploy fault and should keep the pod out of its Service.

## Errors

One envelope, `{"error": "..."}`, the same one `fathom-agent` and `fathom-front`'s
proxy use -- the browser has one shape to parse rather than one per origin.

| Status | Means |
|---|---|
| 422 | A query or path parameter did not validate. `limit`/`offset` must be non-negative integers; a path id must be an integer. |
| 404 | No run or result with that id. |
| 503 | The database is unreadable, **or** its schema is newer than this reader's. |

**422, not 400, since 2026-08-23.** The parameters are validated by FastAPI rather than
by hand, which is what lets the published spec declare a status that can actually occur
and type `limit`/`offset` as integers with `minimum: 0`. The envelope is unchanged --
a `RequestValidationError` handler translates FastAPI's `{"detail": [...]}` into this
service's `{"error": ...}` -- which is the half `evals.js` depends on.

The schema-too-new 503 is deliberate and is not a bug to route around: a reader that
best-effort parses a database written by a newer harness reports wrong numbers, which
is worse than reporting nothing. Roll the reader forward.

## Storage

`/data/results.db`, SQLite, schema version in `storage.json` and served at `/meta`.

There is no migration tool. `store.opened` applies `SCHEMA` and then `_ADDED_COLUMNS`
on every open, which is idempotent and metadata-only. `reporting.connect_ro` opens
`mode=ro` with `PRAGMA query_only` and **refuses to create the file**, rather than
conjuring an empty database that would shadow the real one on a misconfigured path.

The results PVC carries `helm.sh/resource-policy: keep` precisely so history survives
an uninstall -- which is also why a schema change has to be a migration rather than a
new file.

## Retries

Every route is a read and safe to retry freely. There is no write surface here at all:
the read service's ServiceAccount has no token and no RBAC, because it needs the
volume, not the cluster.

Retrying a 503 is worth it if the cause was an unreadable mount. Retrying a
schema-too-new 503 is not -- it will fail identically until the reader is upgraded.

## Timeouts

`FATHOM_EVAL_SERVE_ADDR` and `FATHOM_EVAL_SERVE_MAX_ROWS` are the only knobs on this
process; it sets no request deadline of its own. The budget that actually applies is
the caller's: `fathom-front` bounds a proxied read at `FATHOM_FRONT_EVAL_TIMEOUT`
(10s), deliberately not its 200s agent budget. A query that cannot answer in ten
seconds is a bug in the query, not a slow database.

`serve_max_rows` (500) caps every `limit`, silently. A client asking for more gets the
cap, not an error, so do not infer "there are only 500" from a full page -- read
`total`.

## Idempotency

Reads, so trivially idempotent.

The **harness** is a different matter and does not share this ServiceAccount's
constraints. `run` and `judge` are the two CronJobs, suspended by default, and they
create and destroy fixture namespaces -- see `fixture-namespaces.json`. Two harness
runs must never overlap: they stage the same namespaces. `store.sweep_abandoned` marks
runs whose process died, because a killed process cannot record its own death.

## Ordering and concurrency

`run` writes; `judge` reads what `run` wrote and writes judgements; `serve` only ever
reads. `serve` opens a connection per request and closes it, so it never holds a lock
against a writing CronJob, and it starts serving the moment the first run lands with
no pod restart.

**`serve` is single-threaded on purpose.** Its database dependency is an `async def`
generator, so the open, the queries and the close all happen on the event loop thread.
That is not a style choice: a sync dependency runs in a threadpool, and SQLite refuses a
connection used from a thread other than the one that created it. The blocking reads
stay on the loop because they are local file reads bounded by `serve_max_rows`, which is
the same model the aiohttp version had.

## Auth

There is no auth between services in this repo -- the cluster is the trust boundary.

Two ServiceAccounts here, and the split is deliberate: **the harness's is the only
ServiceAccount in this repo that can write to the cluster**, because it must create
and destroy fixture namespaces. The read service has its own, with no token and no
RBAC at all. Reusing the harness's would put namespace-delete on the one pod here that
another namespace can reach. Do not merge them.
