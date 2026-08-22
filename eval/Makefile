# fathom-eval -- component Makefile.
#
# The four component Makefiles (mcp, agent, front, eval) expose the same
# target names with the same meanings, so `make -C <component> <target>` works
# without knowing which language is behind it:
#
#   help build fmt lint test check image push lint-chart render-chart
#   run-local version clean all
#
# Anything beyond that list is component-specific and named so: `venv`, `lock`,
# `lock-check`, `run-judge`, `run-now` and `db-pull` here; `run-stdio` in mcp;
# `test-ui` in front.
#
# `build` is the venv rather than a compile step, matching agent/: this component
# ships as a source install, so the local artifact `all` produces is the environment
# the other targets run in.

SHELL    := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

BINARY   := fathom-eval
REGISTRY ?= registry.lab:5000
IMAGE    ?= $(REGISTRY)/$(BINARY)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART    := helm
VENV     := .venv
PY       := $(VENV)/bin/python

# `make run-local` talks to the deployed agent over its NodePort and writes to a
# scratch database, so a hand-run never touches the nightly results on the PVC.
AGENT_URL  ?= http://192.168.56.10:30080
JUDGE_URL  ?= http://192.168.56.1:11435
LOCAL_DB   ?= /tmp/fathom-eval.db
FIXTURES   ?=
ITERATIONS ?= 2

# Where the CronJob's database lives, for `db-pull`, and which CronJob `run-now`
# fires: `run` is the evaluation, `judge` the scoring pass.
FATHOM_EVAL_NS ?= fathom-eval
JOB            ?= run

.PHONY: help
help:
	@echo "fathom-eval targets"
	@echo
	@echo "  make build          create $(VENV) and install the package (alias: make venv)"
	@echo "  make fmt            ruff format --check (no rewrite)"
	@echo "  make lint           ruff check"
	@echo "  make test           pytest"
	@echo "  make check          lock-check, fmt, lint, test, lint-chart"
	@echo "  make image          docker build \$$IMAGE:\$$VERSION"
	@echo "  make push           image, then docker push"
	@echo "  make lint-chart     helm lint + template (skips without helm)"
	@echo "  make render-chart   helm template to stdout"
	@echo "  make run-local      run an eval against $(AGENT_URL), db $(LOCAL_DB)"
	@echo "  make run-judge      run the judge pass against $(JUDGE_URL) (component-specific)"
	@echo "  make run-now        fire the in-cluster CronJob once, now (component-specific)"
	@echo "  make db-pull        copy the CronJob's results.db out of the cluster (component-specific)"
	@echo "  make lock           regenerate requirements.lock (component-specific, needs Docker)"
	@echo "  make lock-check     fail if the lock is older than pyproject.toml (component-specific)"
	@echo "  make version        show the version this build would carry"
	@echo "  make clean          remove the venv and caches"
	@echo "  make all            check, then build"
	@echo
	@echo "Knobs:  VERSION=x.y.z  REGISTRY=<host:port>  IMAGE=<ref>"
	@echo "        AGENT_URL=<url>  JUDGE_URL=<url>  LOCAL_DB=<path>"
	@echo "        FIXTURES=a,b  ITERATIONS=$(ITERATIONS)  JOB=run|judge"
	@echo
	@echo "Version: $(VERSION)"

.PHONY: all
all: check build

.PHONY: version
version:
	@echo "$(VERSION)"

.PHONY: build
build: venv

.PHONY: venv
venv: $(VENV)/.stamp

$(VENV)/.stamp: pyproject.toml requirements.lock
	@python3 -m venv $(VENV)
	@$(VENV)/bin/pip install --quiet --upgrade pip
	@$(VENV)/bin/pip install --quiet -r requirements.lock
	@$(VENV)/bin/pip install --quiet -e '.[dev]'
	@touch $@

.PHONY: fmt
fmt: venv
	@$(VENV)/bin/ruff format --check src tests

.PHONY: lint
lint: venv
	@$(VENV)/bin/ruff check src tests

.PHONY: test
test: venv
	@$(VENV)/bin/pytest -q

.PHONY: check
check: lock-check fmt lint test lint-chart

# Mirrors agent/: the Dockerfile installs requirements.lock and then `pip install
# --no-deps .`, so the lock is the only source of what lands in the image. A
# pyproject edit without a re-lock builds green and dies on ImportError at pod start.
.PHONY: lock-check
lock-check:
	@if [ ! -f requirements.lock ]; then \
	  echo "error: requirements.lock is missing -- run 'make lock'"; exit 1; fi
	@if [ pyproject.toml -nt requirements.lock ]; then \
	  echo "error: pyproject.toml is newer than requirements.lock -- run 'make lock'"; exit 1; fi

# Resolved inside the same base image the Dockerfile uses, so the lock reflects what
# will actually be installed rather than whatever the workstation happens to resolve.
.PHONY: lock
lock:
	@docker run --rm -v "$(CURDIR):/w" -w /w python:3.12-slim sh -c '\
	  pip install --quiet --upgrade pip && \
	  pip install --quiet pip-tools && \
	  pip-compile --quiet --strip-extras --output-file requirements.lock pyproject.toml && \
	  chown $(shell id -u):$(shell id -g) requirements.lock'
	@echo "==> requirements.lock regenerated"

.PHONY: image
image:
	@docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

.PHONY: push
push: image
	@docker push $(IMAGE):$(VERSION)

.PHONY: lint-chart
lint-chart:
	@if command -v helm >/dev/null 2>&1; then \
	  helm lint $(CHART) && helm template $(BINARY) $(CHART) >/dev/null; \
	else \
	  echo "==> helm not installed, skipping chart lint"; \
	fi

.PHONY: render-chart
render-chart:
	@helm template $(BINARY) $(CHART)

.PHONY: run-local
run-local: venv
	@$(PY) -m fathom_eval run \
	  --agent-url $(AGENT_URL) \
	  --db $(LOCAL_DB) \
	  --iterations $(ITERATIONS) \
	  $(if $(FIXTURES),--fixtures $(FIXTURES),)

.PHONY: run-judge
run-judge: venv
	@$(PY) -m fathom_eval judge --judge-url $(JUDGE_URL) --db $(LOCAL_DB)

# Runs the real thing in the cluster, now, from the CronJob's own template -- so the
# Job it creates is the one the schedule would have created, deadlines and all, rather
# than a second definition of the same pod that drifts from the chart.
#
# It does not unsuspend anything: `suspend` stops the scheduler, not manual creation,
# so the CronJob stays shipped-suspended afterwards and no nightly run is implied.
#
# The guard is not decoration. `concurrencyPolicy: Forbid` only governs Jobs the
# CronJob controller creates, so nothing in the cluster stops a hand-made Job from
# landing on top of a live one -- two runs would fight over the same fixture
# namespaces and the same SQLite file. A judge counts as busy too: it saturates the
# host CPU, and running it beside an evaluation inflates the elapsed_seconds that
# evaluation exists to measure.
.PHONY: run-now
run-now:
	@case "$(JOB)" in \
	  run)   cron=fathom-eval;; \
	  judge) cron=fathom-eval-judge;; \
	  *)     echo "error: JOB must be 'run' or 'judge', got '$(JOB)'"; exit 1;; \
	 esac; \
	 busy=$$(kubectl -n $(FATHOM_EVAL_NS) get pods -l app.kubernetes.io/name=fathom-eval \
	   -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name} {end}{range .items[?(@.status.phase=="Pending")]}{.metadata.name} {end}'); \
	 if [ -n "$$busy" ]; then \
	   echo "error: fathom-eval is already busy: $$busy"; \
	   echo "       wait for it, or delete its Job -- a second run would collide on the"; \
	   echo "       fixture namespaces and the results database."; exit 1; fi; \
	 name="$$cron-now-$$(date +%Y%m%d-%H%M%S)"; \
	 echo "==> creating job/$$name from cronjob/$$cron"; \
	 kubectl -n $(FATHOM_EVAL_NS) create job "$$name" --from=cronjob/"$$cron"; \
	 echo "==> follow:  kubectl -n $(FATHOM_EVAL_NS) logs -f job/$$name"; \
	 echo "==> stop:    kubectl -n $(FATHOM_EVAL_NS) delete job/$$name"

# The results PVC is node-pinned by local-path, so the database is only directly
# readable from one node. This copies it out to wherever you are instead.
.PHONY: db-pull
db-pull:
	@pod=$$(kubectl -n $(FATHOM_EVAL_NS) get pods -l app.kubernetes.io/name=fathom-eval \
	         --sort-by=.metadata.creationTimestamp -o name | tail -1); \
	 if [ -z "$$pod" ]; then echo "error: no fathom-eval pod found -- has a run completed?"; exit 1; fi; \
	 echo "==> copying results.db from $$pod"; \
	 kubectl -n $(FATHOM_EVAL_NS) cp "$${pod#pod/}:/data/results.db" ./results.db

.PHONY: clean
clean:
	@rm -rf $(VENV) .pytest_cache .ruff_cache src/*.egg-info
	@find . -name __pycache__ -type d -prune -exec rm -rf {} + 2>/dev/null || true
