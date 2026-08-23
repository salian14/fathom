# fathom-agent -- integration notes

**Hand-written. `make contracts` never touches this file.**

The generated files beside it describe the shape of the interface. This one describes
how to use it without causing an incident -- the part a spec cannot express.

## Retries

_Which failures are worth retrying, and which are not._

## Timeouts

_What this service's budget is, and whose budget it sits inside._

## Idempotency

_Whether repeating a call is safe, and what makes two calls the same call._

## Ordering and concurrency

_What may run at the same time, and what must not._

## Auth

_There is no auth between services in this repo -- the cluster is the trust boundary.
Record here anything that depends on that staying true._
