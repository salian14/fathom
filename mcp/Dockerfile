FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/fathom-mcp ./cmd/fathom-mcp


FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fathom-mcp /usr/local/bin/fathom-mcp
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fathom-mcp"]
