BINARY      := fathom-front
REGISTRY    ?= registry.lab:5000
IMAGE       ?= $(REGISTRY)/$(BINARY)
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
CHART       := helm

# Where `make run-local` listens. Not 8080, which is the port-forward to
# fathom-mcp, and not 8081, which is the agent's own run-local.
LOCAL_PORT  ?= 8082
AGENT_URL   ?= http://localhost:8081

.PHONY: all
all: check build

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: test
test:
	go test ./...

.PHONY: check
check: fmt vet test test-ui lint-chart

# The UI has no build step and no package.json, so this needs nothing but a
# node binary -- and skips cleanly when there isn't one, the same way the chart
# lint skips without helm. What it covers is the client-side logic that has no
# Go counterpart: history replay caps, error-envelope parsing, chat lifecycle.
.PHONY: test-ui
test-ui:
	@command -v node >/dev/null 2>&1 || { echo "node not installed, skipping UI checks"; exit 0; }; \
	node internal/web/app_test.mjs

.PHONY: fmt
fmt:
	@out="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet:
	go vet ./...

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
	helm template fathom-front $(CHART) >/dev/null

.PHONY: render-chart
render-chart:
	helm template fathom-front $(CHART)

# Serves the UI against an agent reached however you like -- usually
# `kubectl -n fathom port-forward svc/fathom-agent 8081:8080`. Console
# telemetry so it needs no collector.
.PHONY: run-local
run-local: build
	FATHOM_FRONT_ADDR=:$(LOCAL_PORT) \
	FATHOM_FRONT_AGENT_URL=$(AGENT_URL) \
	FATHOM_FRONT_TELEMETRY=console \
	./bin/$(BINARY)

.PHONY: clean
clean:
	rm -rf bin
