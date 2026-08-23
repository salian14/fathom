# fathom-agent -- component Makefile.
#
# The three component Makefiles (mcp, agent, front) expose the same
# target names with the same meanings, so `make -C <component> <target>` works
# without knowing which language is behind it:
#
#   help build fmt lint test check image push lint-chart render-chart
#   contract run-local version clean all
#
# Anything beyond that list is component-specific and named so: `venv`, `lock`
# and `lock-check` here, `run-stdio` in mcp, `test-ui` in fathom-front.
#
# `build` is the venv rather than a compile step: this component ships as a
# source install, so the local artifact `all` produces is the environment the
# other targets run in.

SHELL    := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

BINARY   := fathom-agent
REGISTRY ?= registry.lab:5000
IMAGE    ?= $(REGISTRY)/$(BINARY)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART    := helm
VENV     := .venv
PY       := $(VENV)/bin/python

# Run against the real dependencies without deploying: port-forward the MCP
# server first (see README) or run `make -C ../mcp run-local`, and dial the
# host's Ollama directly rather than through the in-cluster ExternalName Service.
LOCAL_PORT ?= 8081
OLLAMA_URL ?= http://192.168.56.1:11434
MCP_URL    ?= http://localhost:8080/mcp

.PHONY: help
help:
	@echo "fathom-agent targets"
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
	@echo "  make contract       print this component's contract as JSON"
	@echo "  make run-local      run the agent on :$(LOCAL_PORT)"
	@echo "  make lock           regenerate requirements.lock (component-specific, needs Docker)"
	@echo "  make lock-check     fail if the lock is older than pyproject.toml (component-specific)"
	@echo "  make version        show the version this build would carry"
	@echo "  make clean          remove the venv and caches"
	@echo "  make all            check, then build"
	@echo
	@echo "Knobs:  VERSION=x.y.z  REGISTRY=<host:port>  IMAGE=<ref>  LOCAL_PORT=$(LOCAL_PORT)"
	@echo "        OLLAMA_URL=<url>  MCP_URL=<url>"
	@echo
	@echo "Version: $(VERSION)"

.PHONY: all
all: check build

.PHONY: version
version:
	@echo "$(VERSION)"

# There is nothing to compile; the local artifact is the environment.
.PHONY: build
build: venv

.PHONY: venv
venv: $(VENV)/.stamp

$(VENV)/.stamp: pyproject.toml requirements.lock
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install --upgrade pip
	$(VENV)/bin/pip install -r requirements.lock
	$(VENV)/bin/pip install -e ".[dev]"
	@touch $@

.PHONY: fmt
fmt: venv
	$(VENV)/bin/ruff format --check src tests

.PHONY: lint
lint: venv
	$(VENV)/bin/ruff check src tests

.PHONY: test
test: venv
	$(VENV)/bin/pytest -q

.PHONY: check
check: lock-check fmt lint test lint-chart

# Component-specific. The Dockerfile installs requirements.lock and then
# `pip install --no-deps .`, so the lock is the only source of what ends up in
# the image -- a dependency added to pyproject.toml without re-running
# `make lock` builds a green image that dies on ImportError at pod start. There
# is no CI to catch that, so catch it here.
.PHONY: lock-check
lock-check:
	@[ requirements.lock -nt pyproject.toml ] || \
	  { echo "requirements.lock is older than pyproject.toml: run 'make lock'"; exit 1; }

.PHONY: image
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

.PHONY: push
push: image
	docker push $(IMAGE):$(VERSION)

.PHONY: lint-chart
lint-chart:
	@command -v helm >/dev/null 2>&1 || { echo "helm not installed, skipping chart lint"; exit 0; }; \
	set -e; \
	helm lint $(CHART); \
	helm template $(BINARY) $(CHART) >/dev/null

.PHONY: render-chart
render-chart:
	helm template $(BINARY) $(CHART)

# Print this component's published interface as one JSON document on stdout,
# and nothing else -- the writer in scripts/contracts.py parses this, so the
# venv build below is sent to stderr rather than allowed to corrupt it.
# Needs no Ollama and no MCP server: the app is built, not served.
.PHONY: contract
contract:
	@$(MAKE) --no-print-directory venv 1>&2
	@$(PY) -m fathom_agent.contract

.PHONY: run-local
run-local: venv
	FATHOM_AGENT_ADDR=:$(LOCAL_PORT) \
	FATHOM_AGENT_MCP_URL=$(MCP_URL) \
	FATHOM_AGENT_OLLAMA_URL=$(OLLAMA_URL) \
	$(PY) -m fathom_agent

# Component-specific.
.PHONY: lock
lock:
	@command -v docker >/dev/null 2>&1 || { echo "docker required to regenerate the lock"; exit 1; }
	docker run --rm -v "$(PWD)":/w -w /w python:3.12-slim sh -c \
	  'pip install -q --upgrade pip && pip install -q . && pip freeze --exclude-editable | grep -v "^fathom-agent" > requirements.lock; \
	   rm -rf build; chown -R $(shell id -u):$(shell id -g) requirements.lock src'
	@touch requirements.lock

.PHONY: clean
clean:
	rm -rf $(VENV) .pytest_cache .ruff_cache src/*.egg-info
	find . -name __pycache__ -type d -prune -exec rm -rf {} +
