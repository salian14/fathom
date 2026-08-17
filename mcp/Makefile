BINARY      := fathom-mcp
IMAGE       ?= fathom-mcp
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
CHART       := helm

.PHONY: all
all: check build

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: test
test:
	go test ./...

.PHONY: check
check: fmt vet test lint-chart

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

.PHONY: lint-chart
lint-chart:
	@command -v helm >/dev/null 2>&1 || { echo "helm not installed, skipping chart lint"; exit 0; }
	helm lint $(CHART)
	helm template fathom-mcp $(CHART) >/dev/null

.PHONY: render-chart
render-chart:
	helm template fathom-mcp $(CHART)

.PHONY: run-stdio
run-stdio: build
	FATHOM_MCP_TRANSPORT=stdio \
	FATHOM_MCP_KUBECONFIG_PATH=$${KUBECONFIG:-$$HOME/.kube/config} \
	./bin/$(BINARY)

.PHONY: clean
clean:
	rm -rf bin
