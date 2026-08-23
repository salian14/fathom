# fathom-mcp -- component Makefile.
#
# The three component Makefiles (mcp, agent, front) expose the same
# target names with the same meanings, so `make -C <component> <target>` works
# without knowing which language is behind it:
#
#   help build fmt lint test check image push lint-chart render-chart
#   contract run-local version clean all
#
# Anything beyond that list is component-specific and named so: `run-stdio`
# here, `venv`/`lock`/`lock-check` in agent, `test-ui` in front. `vet` is
# kept as an alias for `lint` because it is the Go-native name and the READMEs
# use it.

SHELL       := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

BINARY      := fathom-mcp
REGISTRY    ?= registry.lab:5000
IMAGE       ?= $(REGISTRY)/$(BINARY)
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
CHART       := helm

# Where `make run-local` listens. 8080 is this server's own default and what
# agent's `make run-local` dials, so a local pair needs no extra configuration.
LOCAL_PORT  ?= 8080

.PHONY: help
help:
	@echo "fathom-mcp targets"
	@echo
	@echo "  make build          compile bin/$(BINARY)"
	@echo "  make fmt            gofmt check (no rewrite)"
	@echo "  make lint           go vet  (alias: make vet)"
	@echo "  make test           go test ./..."
	@echo "  make check          fmt, lint, test, lint-chart"
	@echo "  make image          docker build \$$IMAGE:\$$VERSION"
	@echo "  make push           image, then docker push"
	@echo "  make lint-chart     helm lint + template (skips without helm)"
	@echo "  make render-chart   helm template to stdout"
	@echo "  make contract       print this component's contract as JSON"
	@echo "  make run-local      run the HTTP transport on :$(LOCAL_PORT)"
	@echo "  make run-stdio      run the stdio transport (component-specific)"
	@echo "  make version        show the version this build would carry"
	@echo "  make clean          remove build output"
	@echo "  make all            check, then build"
	@echo
	@echo "Knobs:  VERSION=x.y.z  REGISTRY=<host:port>  IMAGE=<ref>  LOCAL_PORT=$(LOCAL_PORT)"
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
check: fmt lint test lint-chart

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
# and nothing else -- the writer in scripts/contracts.py parses this, so a
# stray echo here becomes a parse error there. Needs no cluster: see contract.go.
.PHONY: contract
contract:
	@$(MAKE) --no-print-directory build 1>&2
	@./bin/$(BINARY) -contract

# The HTTP transport, the same one the agent pod dials, against the cluster via
# the out-of-cluster kubeconfig fallback. Console telemetry so it needs no
# collector -- stderr, not stdout, for the reason `run-stdio` documents below.
.PHONY: run-local
run-local: build
	FATHOM_MCP_TRANSPORT=http \
	FATHOM_MCP_ADDR=:$(LOCAL_PORT) \
	FATHOM_MCP_KUBECONFIG_PATH=$${KUBECONFIG:-$$HOME/.kube/config} \
	FATHOM_MCP_TELEMETRY=console \
	./bin/$(BINARY)

# Component-specific: the stdio transport, for driving the server from an MCP
# client on this machine. No telemetry override here -- stdout carries JSON-RPC
# on this transport, so set FATHOM_MCP_TELEMETRY=console yourself when you want
# spans on stderr.
.PHONY: run-stdio
run-stdio: build
	FATHOM_MCP_TRANSPORT=stdio \
	FATHOM_MCP_KUBECONFIG_PATH=$${KUBECONFIG:-$$HOME/.kube/config} \
	./bin/$(BINARY)

.PHONY: clean
clean:
	rm -rf bin
