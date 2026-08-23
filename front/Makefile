# fathom-front -- component Makefile.
#
# The three component Makefiles (mcp, agent, front) expose the same
# target names with the same meanings, so `make -C <component> <target>` works
# without knowing which language is behind it:
#
#   help build fmt lint test check image push lint-chart render-chart
#   contract run-local version clean all
#
# Anything beyond that list is component-specific and named so: `test-ui` here,
# `run-stdio` in mcp, `venv`/`lock`/`lock-check` in agent. `vet` is kept as an
# alias for `lint` because it is the Go-native name and the READMEs use it.

SHELL       := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

BINARY      := fathom-front
REGISTRY    ?= registry.lab:5000
IMAGE       ?= $(REGISTRY)/$(BINARY)
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
CHART       := helm

# Where `make run-local` listens. Not 8080, which is fathom-mcp's local port and
# the port-forward to it, and not 8081, which is the agent's own run-local.
LOCAL_PORT  ?= 8082
AGENT_URL   ?= http://localhost:8081
# Empty by default, which is the disabled state -- `run-local` then serves the chat
# view alone, exactly as a deployment with no eval.url does. Point it at
# `make -C ../eval run-serve` (:8083) to bring up the eval view.
EVAL_URL    ?=

.PHONY: help
help:
	@echo "fathom-front targets"
	@echo
	@echo "  make build          compile bin/$(BINARY)"
	@echo "  make fmt            gofmt check (no rewrite)"
	@echo "  make lint           go vet  (alias: make vet)"
	@echo "  make test           go test ./..."
	@echo "  make check          fmt, lint, test, test-ui, lint-chart"
	@echo "  make image          docker build \$$IMAGE:\$$VERSION"
	@echo "  make push           image, then docker push"
	@echo "  make lint-chart     helm lint + template (skips without helm)"
	@echo "  make render-chart   helm template to stdout"
	@echo "  make contract       print this component's contract as JSON"
	@echo "  make run-local      serve the UI on :$(LOCAL_PORT)"
	@echo "  make test-ui        browser-side JS checks (component-specific, skips without node)"
	@echo "  make version        show the version this build would carry"
	@echo "  make clean          remove build output"
	@echo "  make all            check, then build"
	@echo
	@echo "        EVAL_URL=<url>  (empty disables the eval view, as in a deployment)"
	@echo "Knobs:  VERSION=x.y.z  REGISTRY=<host:port>  IMAGE=<ref>  LOCAL_PORT=$(LOCAL_PORT)"
	@echo "        AGENT_URL=<url>"
	@echo
	@echo "Version: $(VERSION)"

.PHONY: all
all: check build

.PHONY: version
version:
	@echo "$(VERSION)"

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: fmt
fmt:
	@out="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: vet

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

.PHONY: check
check: fmt lint test test-ui lint-chart

# Component-specific. The UI has no build step and no package.json, so this
# needs nothing but a node binary -- and skips cleanly when there isn't one, the
# same way the chart lint skips without helm. What it covers is the client-side
# logic that has no Go counterpart: history replay caps, error-envelope parsing,
# chat lifecycle.
.PHONY: test-ui
test-ui:
	@command -v node >/dev/null 2>&1 || { echo "node not installed, skipping UI checks"; exit 0; }; \
	node internal/web/app_test.mjs && node internal/web/evals_test.mjs

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
# build below is sent to stderr rather than allowed to corrupt the document.
# Needs no agent and no listener: see contract.go.
.PHONY: contract
contract:
	@$(MAKE) --no-print-directory build 1>&2
	@./bin/$(BINARY) -contract

# Serves the UI against an agent reached however you like -- usually
# `kubectl -n fathom port-forward svc/fathom-agent 8081:8080`, or
# `make -C ../agent run-local`. Console telemetry so it needs no collector.
.PHONY: run-local
run-local: build
	FATHOM_FRONT_ADDR=:$(LOCAL_PORT) \
	FATHOM_FRONT_AGENT_URL=$(AGENT_URL) \
	FATHOM_FRONT_EVAL_URL=$(EVAL_URL) \
	FATHOM_FRONT_TELEMETRY=console \
	./bin/$(BINARY)

.PHONY: clean
clean:
	rm -rf bin
