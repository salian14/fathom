BINARY   := fathom-agent
REGISTRY ?= registry.lab:5000
IMAGE    ?= $(REGISTRY)/$(BINARY)
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART    := helm
VENV     := .venv
PY       := $(VENV)/bin/python

.PHONY: all
all: check

.PHONY: venv
venv: $(VENV)/.stamp

$(VENV)/.stamp: pyproject.toml requirements.lock
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install --upgrade pip
	$(VENV)/bin/pip install -r requirements.lock
	$(VENV)/bin/pip install -e ".[dev]"
	@touch $@

.PHONY: check
check: fmt lint test lint-chart

.PHONY: fmt
fmt: venv
	$(VENV)/bin/ruff format --check src tests

.PHONY: lint
lint: venv
	$(VENV)/bin/ruff check src tests

.PHONY: test
test: venv
	$(VENV)/bin/pytest -q

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

# Run against the real dependencies without deploying: port-forward the MCP server
# first (see README), and dial the host's Ollama directly rather than through the
# in-cluster ExternalName Service.
OLLAMA_URL ?= http://192.168.56.1:11434
MCP_URL    ?= http://localhost:8080/mcp

.PHONY: run-local
run-local: venv
	FATHOM_AGENT_ADDR=:8081 \
	FATHOM_AGENT_MCP_URL=$(MCP_URL) \
	FATHOM_AGENT_OLLAMA_URL=$(OLLAMA_URL) \
	$(PY) -m fathom_agent

.PHONY: lock
lock:
	@command -v docker >/dev/null 2>&1 || { echo "docker required to regenerate the lock"; exit 1; }
	docker run --rm -v "$(PWD)":/w -w /w python:3.12-slim sh -c \
	  'pip install -q --upgrade pip && pip install -q . && pip freeze --exclude-editable | grep -v "^fathom-agent" > requirements.lock'

.PHONY: clean
clean:
	rm -rf $(VENV) .pytest_cache .ruff_cache src/*.egg-info
	find . -name __pycache__ -type d -prune -exec rm -rf {} +
